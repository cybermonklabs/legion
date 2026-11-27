import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage, isDeploymentReadRefused } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import { isLiveQuery } from "../../api/query-refresh";
import type { DeliverySettings, DeliverySettingsInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { compareTimestamps } from "../../lib/timestamps";
import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
} from "../settings/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** The characters that separate the entries of a list field, as a character class's body: line
 *  breaks, and commas or spaces too, since neither a GitHub login nor an `owner/repo` can hold
 *  one. */
const SEPARATOR_CHARACTERS = String.raw`\s,`;

/** A run of separators between two entries. */
const LIST_SEPARATOR = new RegExp(`[${SEPARATOR_CHARACTERS}]+`);

/** Any one character of a list entry: anything but a separator. */
const LIST_ENTRY_CHARACTER = new RegExp(`[^${SEPARATOR_CHARACTERS}]`);

/** The entries a list field's text holds. */
function listEntries(text: string): string[] {
  return text.split(LIST_SEPARATOR).filter(Boolean);
}

/** A job-name list's entries: one per line, each trimmed. A job name holds spaces and slashes
 *  (`production / production-apply`), so only a line break separates two. */
function lineEntries(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
}

/** A required field's text as the draft keeps it: text holding nothing but whitespace is empty,
 *  so the browser's `required` check refuses it before the server's 400 would. */
function textOrEmpty(text: string): string {
  return text.trim() === "" ? "" : text;
}

/** A required list field's text as the draft keeps it: text holding no entry, only separators, is
 *  empty, as `textOrEmpty` keeps a field of only whitespace. */
function listOrEmpty(text: string): string {
  return LIST_ENTRY_CHARACTER.test(text) ? text : "";
}

/** An optional field's text as the request sends it: trimmed, or `null` when it holds nothing. */
function textOrNull(text: string): string | null {
  const trimmed = text.trim();
  return trimmed === "" ? null : trimmed;
}

/** The fields as typed: the lists are the textareas' text, split only when saved. */
interface Draft {
  deploy_repo: string;
  deploy_workflow_path: string;
  production_job_names: string;
  production_apply_step_name: string;
  staging_e2e_job_name: string;
  staging_apply_job_name: string;
  pr_checks_rollup_job_name: string;
  pr_checks_workflow_path: string;
  population_authors: string;
  excluded_repos: string;
}

/** The fields as the stored record fills them, and empty where there is none. */
function recordDraft(record: DeliverySettings | null): Draft {
  return {
    deploy_repo: record?.deploy_repo ?? "",
    deploy_workflow_path: record?.deploy_workflow_path ?? "",
    excluded_repos: record?.excluded_repos?.join("\n") ?? "",
    population_authors: record?.population_authors?.join("\n") ?? "",
    pr_checks_workflow_path: record?.pr_checks_workflow_path ?? "",
    production_job_names: record?.production_job_names?.join("\n") ?? "",
    production_apply_step_name: record?.production_apply_step_name ?? "",
    pr_checks_rollup_job_name: record?.pr_checks_rollup_job_name ?? "",
    staging_apply_job_name: record?.staging_apply_job_name ?? "",
    staging_e2e_job_name: record?.staging_e2e_job_name ?? "",
  };
}

/** The later of two versions of the record, by when Dispatch wrote each (`updated_at`): `held`,
 *  as a reader of it has it, and `other`, which may be newer - a save's own answer, or the
 *  cache's record beside the one the form was handed - and wins a tie. No record, or none read
 *  yet, gives way to any record. */
function laterRecord(
  held: DeliverySettings | null | undefined,
  other: DeliverySettings | null | undefined
): DeliverySettings | null {
  if (other == null) return held ?? null;
  if (held == null) return other;
  return compareTimestamps(other.updated_at, held.updated_at) >= 0 ? other : held;
}

/** The fields of `next` that differ from `stored`, as typed: what the reader has changed. */
function changedFields(next: Draft, stored: Draft): Partial<Draft> {
  const changed: Partial<Draft> = {};
  for (const field of Object.keys(next) as (keyof Draft)[]) {
    if (next[field] !== stored[field]) changed[field] = next[field];
  }
  return changed;
}

/** The entries of `sent` that `edited` still holds as sent: the fields this tab's Save sent that
 *  the reader has not typed in since. */
function stillSent(sent: Partial<Draft>, edited: Partial<Draft>): Partial<Draft> {
  const kept: Partial<Draft> = {};
  for (const field of Object.keys(sent) as (keyof Draft)[]) {
    if (edited[field] === sent[field]) kept[field] = sent[field];
  }
  return kept;
}

/** What the reader has changed over the record, and what this tab's newest Save sent of it. */
interface Changes {
  /** The fields the reader has changed, as typed. */
  readonly edited: Partial<Draft>;
  /** The newest Save's: the fields it sent that the reader has not typed in since (`stillSent`),
   *  and the Save itself, so only its own landing retires them. `null` before any Save, and once
   *  Dispatch has refused it or its landing has retired them. */
  readonly sent: { readonly fields: Partial<Draft>; readonly save: object } | null;
}

const NO_CHANGES: Changes = { edited: {}, sent: null };

/** `changes` once the Save `save` has landed and the record holds what it sent: the fields it
 *  sent that the reader has not typed in since follow the record again. Where a newer Save is out
 *  or this one was refused, nothing changes. */
function retired(changes: Changes, save: object): Changes {
  if (changes.sent?.save !== save) return changes;
  const edited = { ...changes.edited };
  for (const field of Object.keys(changes.sent.fields) as (keyof Draft)[]) delete edited[field];
  return { edited, sent: null };
}

/** The three optional job names the Pipeline page follows, in the order the form lists them. */
const PIPELINE_JOB_FIELDS = [
  {
    key: "staging_e2e_job_name",
    label: "Staging end-to-end job",
    placeholder: "The staging end-to-end job, as a deploy run lists it (optional)",
  },
  {
    key: "staging_apply_job_name",
    label: "Staging deploy job",
    placeholder: "The staging deploy job (optional)",
  },
  {
    key: "pr_checks_rollup_job_name",
    label: "PR checks roll-up job",
    placeholder: "The PR-checks roll-up job the failing-jobs table leaves out (optional)",
  },
] as const;

interface DeliverySettingsFormProps {
  /** The record when the caller already holds the answer, and the form then reads nothing: the
   *  Delivery page passes `null`, since the timeline's `DELIVERY_NOT_CONFIGURED` already says no
   *  record is stored. Left out, the form reads `GET /api/v1/settings/delivery`. */
  readonly initial?: DeliverySettings | null;
}

/**
 * The delivery timeline's configuration record as a form: the deploy repository, its deploy and
 * pull-request-check workflow paths, the deploy workflow's production jobs (one per line) and
 * optional apply step, the three optional job names the Pipeline page follows (the staging
 * end-to-end gate, the staging deploy and the PR-checks roll-up), and the population's authors and
 * excluded repositories. The two population lists take one entry per line (a comma or a space also
 * separates entries); production job names split on newlines alone because they can contain spaces.
 * The fields start from the stored record (`initial`, or the record the form reads), and empty, with
 * placeholders saying what each holds, when there is none. A required field holding only whitespace
 * (a list, only separators) is empty, so the browser refuses it before any request; an optional job
 * name left empty saves `null`. Save replaces the whole record; the server first checks that the
 * GitHub App can read the deploy repository, and a refusal shows under the form as the server wrote
 * it. A save refreshes the record and every delivery read, so the Delivery page, which renders this
 * form in place of a timeline it cannot read yet, shows the timeline once the save succeeds. The
 * record is the deployment's, which Dispatch refuses a restricted member, and no page shows such a
 * member this form: a read refused with 403 `DEPLOYMENT_READ_REFUSED` (a credential that changed
 * while the page was open) draws nothing, rather than a load error.
 */
export function DeliverySettingsForm({ initial }: DeliverySettingsFormProps): ReactNode {
  const settings = useQuery({ ...deliverySettingsQuery(), enabled: initial === undefined });
  let record = initial;
  if (record === undefined) {
    if (settings.isPending) {
      return <p className={`mt-6 ${textMutedOnCanvas}`}>Loading delivery settings…</p>;
    }
    if (isDeploymentReadRefused(settings.error)) return null;
    if (settings.isError) {
      return (
        <div className="mt-6">
          <QueryError
            message="Couldn't load the delivery settings."
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        </div>
      );
    }
    record = settings.data;
  }
  return <DeliverySettingsFields record={record} />;
}

/**
 * The form itself, over a record `DeliverySettingsForm` has read. Its groups, in this order:
 * Fields - Deploy repository, Deploy workflow, Production jobs, Production apply step, the three
 * Pipeline job names, PR checks workflow, Population authors and Excluded repositories, each one
 * item - then Save, and under them the refusal of a save. Fields are two columns from `sm` and
 * one below it; Save stays on its own line under them. A one-line value longer than its field
 * ends in an ellipsis with the whole value as its title; a list wraps inside its own box.
 */
function DeliverySettingsFields({ record }: { record: DeliverySettings | null }): ReactNode {
  const queryClient = useQueryClient();
  const id = useId();
  const submitGuard = useSubmitGuard();
  // The record the fields start from: the one the form was handed, or the cache's where that is
  // newer. A save writes its own answer there in the callback that retires the fields it sent,
  // and this read of the cache takes it in that same render, while TanStack Query has not yet
  // told the form's host of it - in a browser, a timer later.
  const cached = useQuery({ ...deliverySettingsQuery(), enabled: false }).data;
  const written = laterRecord(record, cached);
  const stored = recordDraft(written);
  // What the reader has changed, over the record as the server now holds it: a save from anywhere
  // refreshes the fields the reader has not touched and keeps the ones they have as they typed
  // them, so a line break typed to start the next entry stays, and the caret after it. `sent`
  // holds the changed fields this tab's newest Save sent, as typed, until the reader types in one
  // again; a refusal of that Save drops them all. That Save's own landing retires them: they
  // follow the record its answer is, so a list typed with a trailing line break or with commas,
  // which Save rewrites, shows as saved, and a later writer's record after it shows too. While the
  // Save is out, a record - another writer's - is no answer to it and keeps every field as typed.
  const [changes, setChanges] = useState<Changes>(NO_CHANGES);
  const { edited, sent } = changes;
  const kept = changedFields({ ...stored, ...edited }, stored);
  if (Object.keys(kept).length !== Object.keys(edited).length) {
    setChanges({ edited: kept, sent: sent && { ...sent, fields: stillSent(sent.fields, kept) } });
  }
  const draft: Draft = { ...stored, ...edited };
  const setDraft = (next: Draft) => {
    const changed = changedFields(next, stored);
    setChanges({
      edited: changed,
      sent: sent && { ...sent, fields: stillSent(sent.fields, changed) },
    });
  };
  // The form shows its own refusal, under the fields it was refused over.
  const save = useKeyedWrite({
    method: "PUT",
    write: (input: DeliverySettingsInput, bounds) => api.putDeliverySettings(input, bounds),
    inline: true,
    label: () => "Delivery settings saved",
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: deliverySettingsQuery().queryKey });
      // A live view learns of the save from Dispatch, as a delta, or as the snapshot a page that
      // had none (DELIVERY_NOT_CONFIGURED) is answered once a configuration exists.
      void queryClient.invalidateQueries({
        queryKey: ["delivery"],
        predicate: (query) => !isLiveQuery(query),
      });
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => {
      // This Save, by identity: its own landing, and no record that arrives meanwhile, retires
      // what it sent, and the landing of an earlier Save leaves a newer one's fields as typed.
      const press = {};
      setChanges({ edited, sent: { fields: edited, save: press } });
      save.mutate(
        {
          deploy_repo: draft.deploy_repo,
          deploy_workflow_path: draft.deploy_workflow_path.trim(),
          excluded_repos: listEntries(draft.excluded_repos),
          population_authors: listEntries(draft.population_authors),
          pr_checks_workflow_path: draft.pr_checks_workflow_path.trim(),
          production_job_names: lineEntries(draft.production_job_names),
          production_apply_step_name: textOrNull(draft.production_apply_step_name),
          pr_checks_rollup_job_name: textOrNull(draft.pr_checks_rollup_job_name),
          staging_apply_job_name: textOrNull(draft.staging_apply_job_name),
          staging_e2e_job_name: textOrNull(draft.staging_e2e_job_name),
        },
        {
          onError: () =>
            setChanges((current) =>
              current.sent?.save === press ? { ...current, sent: null } : current
            ),
          onSuccess: (answer) => {
            // A `PUT` goes under no key (`goesUnderKey`), so Dispatch keeps nothing under one for
            // it and answers each with the record it stored: a landing with no answer (`null`) is
            // a keyed write's alone, and here the type's only.
            if (answer === null) return;
            // The answer is the record from now on, written where the form and every other
            // reader of it read it, unless a newer one is there already: another writer's, read
            // meanwhile. The fields it sent follow it in the render that takes it in.
            queryClient.setQueryData(deliverySettingsQuery().queryKey, (current) =>
              laterRecord(current, answer)
            );
            setChanges((current) => retired(current, press));
          },
        }
      );
    });
  };

  return (
    <>
      <form className={`mt-6 grid gap-4 rounded-xl border p-4 ${borderDefault}`} onSubmit={submit}>
        <fieldset className="grid min-w-0 gap-4 sm:grid-cols-2" disabled={save.isPending}>
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-repo`}>
            Deploy repository
            <input
              className={settingsMonoInput}
              id={`${id}-deploy-repo`}
              onChange={(event) => setDraft({ ...draft, deploy_repo: event.target.value })}
              pattern={ownerRepoPattern}
              placeholder="owner/repo"
              required
              title={draft.deploy_repo === "" ? undefined : draft.deploy_repo}
              value={draft.deploy_repo}
            />
          </label>
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-workflow`}>
            Deploy workflow
            <input
              className={settingsMonoInput}
              id={`${id}-deploy-workflow`}
              onChange={(event) =>
                setDraft({ ...draft, deploy_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<deploy>.yml"
              required
              title={draft.deploy_workflow_path === "" ? undefined : draft.deploy_workflow_path}
              value={draft.deploy_workflow_path}
            />
          </label>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-production-jobs`}>
              Production jobs
              {/* One setting per line rather than prose, so this box takes neither the
                  reference picker nor the renderer: `delivery-production-jobs` in
                  `DISPATCH_WRITING_BOXES` and `DISPATCH_READING_SURFACES` says why. */}
              <textarea
                aria-describedby={`${id}-production-jobs-hint`}
                className={settingsMonoInput}
                id={`${id}-production-jobs`}
                onChange={(event) =>
                  setDraft({ ...draft, production_job_names: textOrEmpty(event.target.value) })
                }
                placeholder="One job name per line, as a deploy run lists it"
                required
                rows={2}
                value={draft.production_job_names}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-production-jobs-hint`}>
              The jobs whose success counts as a production deploy, one per line, named as a run
              lists each: a reusable workflow&apos;s job reads &lt;caller&gt; / &lt;job&gt;. After a
              rename, keep the old name beside the new so history still counts.
            </p>
          </div>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-production-apply-step`}>
              Production apply step
              <input
                aria-describedby={`${id}-production-apply-step-hint`}
                className={settingsMonoInput}
                id={`${id}-production-apply-step`}
                onChange={(event) =>
                  setDraft({ ...draft, production_apply_step_name: event.target.value })
                }
                placeholder="The step whose success means the job applied (optional)"
                title={
                  draft.production_apply_step_name === ""
                    ? undefined
                    : draft.production_apply_step_name
                }
                value={draft.production_apply_step_name}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-production-apply-step-hint`}>
              Named as GitHub lists the job&apos;s step. Set, a production job that succeeds without
              this step succeeding is not a deploy.
            </p>
          </div>
          {PIPELINE_JOB_FIELDS.map(({ key, label, placeholder }) => (
            <label className={settingsFieldLabel} htmlFor={`${id}-${key}`} key={key}>
              {label}
              <input
                aria-describedby={`${id}-pipeline-jobs-hint`}
                className={settingsMonoInput}
                id={`${id}-${key}`}
                onChange={(event) => setDraft({ ...draft, [key]: event.target.value })}
                placeholder={placeholder}
                title={draft[key] === "" ? undefined : draft[key]}
                value={draft[key]}
              />
            </label>
          ))}
          <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-pipeline-jobs-hint`}>
            Leave empty if the pipeline has no such step; the Pipeline page omits that card. With no
            roll-up job the failing-jobs table leaves nothing out.
          </p>
          <label className={settingsFieldLabel} htmlFor={`${id}-pr-checks-workflow`}>
            PR checks workflow
            <input
              className={settingsMonoInput}
              id={`${id}-pr-checks-workflow`}
              onChange={(event) =>
                setDraft({ ...draft, pr_checks_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<checks>.yml"
              required
              title={
                draft.pr_checks_workflow_path === "" ? undefined : draft.pr_checks_workflow_path
              }
              value={draft.pr_checks_workflow_path}
            />
          </label>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-population-authors`}>
              Population authors
              {/* One setting per line rather than prose: `delivery-population-authors` in
                  `DISPATCH_WRITING_BOXES` and `DISPATCH_READING_SURFACES` says why. */}
              <textarea
                aria-describedby={`${id}-population-authors-hint`}
                className={settingsMonoInput}
                id={`${id}-population-authors`}
                onChange={(event) =>
                  setDraft({ ...draft, population_authors: listOrEmpty(event.target.value) })
                }
                placeholder="One GitHub login per line"
                required
                rows={4}
                value={draft.population_authors}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-population-authors-hint`}>
              Whose merged pull requests count, as GitHub spells each login: an App's ends in [bot].
            </p>
          </div>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-excluded-repos`}>
              Excluded repositories
              {/* One setting per line rather than prose: `delivery-excluded-repos` in
                  `DISPATCH_WRITING_BOXES` and `DISPATCH_READING_SURFACES` says why. */}
              <textarea
                aria-describedby={`${id}-excluded-repos-hint`}
                className={settingsMonoInput}
                id={`${id}-excluded-repos`}
                onChange={(event) => setDraft({ ...draft, excluded_repos: event.target.value })}
                placeholder="One owner/repo per line"
                rows={4}
                value={draft.excluded_repos}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-excluded-repos-hint`}>
              Repositories whose merged pull requests never count, as GitHub spells each owner/repo,
              capitals included.
            </p>
          </div>
        </fieldset>
        <button
          className={`justify-self-start ${settingsSubmitButton}`}
          disabled={save.isPending}
          type="submit"
        >
          Save delivery settings
        </button>
      </form>
      {save.isError ? (
        <div className="mt-4">
          {/* A save the reader stopped may have been made, and the form's button is a press of
              its own: it sends what the fields hold then. */}
          <QueryError
            message={
              save.error instanceof SendStoppedError
                ? stoppedWriteNote("the delivery settings", "saved", {
                    anew: "Save delivery settings sends what the form holds now.",
                  })
                : apiErrorMessage(save.error, "Couldn't save the delivery settings.")
            }
          />
        </div>
      ) : null}
    </>
  );
}
