import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import { invalidateWaitingForPaused } from "../../api/query-client";
import { isLiveQuery } from "../../api/query-refresh";
import { useReadBound } from "../../api/read-bound";
import type { DeliverySettings, DeliverySettingsInput } from "../../api/types";
import { heldField } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { useReleaseWhenLate, useSubmitGuard } from "../../hooks/useSubmitGuard";
import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
} from "../settings/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite, useRefusalOfSubject } from "../writes/useKeyedWrite";
import { useHoldUntilDrawn } from "./useHoldUntilDrawn";

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

/** The fields as `record` fills them: each list one entry per line, and every field empty where
 *  no record is stored. */
function draftOf(record: DeliverySettings | null): Draft {
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

/** What the form says when its record's read failed, and before why a read of the record after
 *  a save has not settled. */
const DELIVERY_SETTINGS_NOT_LOADED = "Couldn't load the delivery settings.";

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
 * form in place of a timeline it cannot read yet, shows the timeline once the save succeeds.
 */
export function DeliverySettingsForm({ initial }: DeliverySettingsFormProps): ReactNode {
  const settings = useQuery({ ...deliverySettingsQuery(), enabled: initial === undefined });
  let record = initial;
  if (record === undefined) {
    if (settings.isPending) {
      return <p className={`mt-6 ${textMutedOnCanvas}`}>Loading delivery settings…</p>;
    }
    if (settings.isError) {
      return (
        <div className="mt-6">
          <QueryError
            message={DELIVERY_SETTINGS_NOT_LOADED}
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        </div>
      );
    }
    record = settings.data;
  }
  // A caller that holds the record reads it itself, so its own read is what moves once a save
  // lands.
  return <DeliverySettingsFields record={record} replacedByCaller={initial !== undefined} />;
}

/** The form's fields and its Save. Save and the fields are held while the save is out and until
 *  what it changes is drawn: through the record's own read, which fills these fields again from
 *  the stored record, or - where the caller holds the record (`replacedByCaller`), as the Delivery
 *  page's live view does, drawing the timeline in the form's place once its snapshot comes - until
 *  the caller replaces the form (`useHoldUntilDrawn`). Save is held as every held control is
 *  (`held-press.ts`): by `aria-disabled`, never `disabled`, so it keeps its Tab stop and the
 *  reader's focus through the whole hold. Each field is held by `aria-disabled` and read-only,
 *  never `disabled`, so the field the reader submitted from with Enter keeps the focus too. A
 *  press of Save still submits the form, by a click or by Enter, and so does Enter in a field: the
 *  form's submit sends nothing while Save is held. The `refused:` variant paints a held Save as a
 *  disabled one. The form, its fields and its Save stay the same elements through the read, so
 *  the focus stays where the reader pressed; a form the caller replaces goes while Save is held,
 *  so the control the reader pressed from keeps the focus until the form goes. A save past its
 *  deadline (`SEND_DEADLINE_MS`) lets them go then, still tried under its key, and a record read
 *  that has not settled by the request deadline (`REQUEST_DEADLINE_MS`) lets them go then, with
 *  the reason under the form (`useReadBound`). A refusal lets them go at once, with its reason
 *  under the form. */
function DeliverySettingsFields({
  record,
  replacedByCaller,
}: {
  record: DeliverySettings | null;
  replacedByCaller: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const id = useId();
  const submitGuard = useSubmitGuard();
  const version = record?.updated_at ?? null;
  const [draft, setDraft] = useState<Draft>(() => draftOf(record));
  // A new stored record - this tab's save, another tab's that the read brings, or another record
  // a caller hands over - fills the fields again. It is set during render (React's pattern for
  // state derived from props) and not by a key on the form, so the form and the Save the reader
  // pressed stay the same elements.
  const [filledFrom, setFilledFrom] = useState(version);
  if (version !== filledFrom) {
    setFilledFrom(version);
    setDraft(draftOf(record));
  }
  // The write time of the record the newest press was made over.
  const [pressedOver, setPressedOver] = useState(version);
  const [drawing, holdUntilDrawn] = useHoldUntilDrawn(version);
  const [unsettled, holdThrough] = useReadBound(deliverySettingsQuery().queryKey);
  // The form shows its own refusal, under the fields it was refused over.
  const save = useKeyedWrite({
    method: "PUT",
    write: (input: DeliverySettingsInput, bounds) => api.putDeliverySettings(input, bounds),
    inline: true,
    label: () => "Delivery settings saved",
    onSettled: () => submitGuard.release(),
    readsLate: true,
    onSuccess: () => {
      if (replacedByCaller) holdUntilDrawn();
      // Pending through the record's own read, which a caller holding the record has none of. A
      // read that has not settled by the request deadline (`REQUEST_DEADLINE_MS`) - one paused
      // while the browser is offline - lets Save go then, and the form says why the record is not
      // read yet (`useReadBound`). It waits on a fetch the network had already paused too, which
      // that deadline ends as well (`invalidateWaitingForPaused`).
      const read = invalidateWaitingForPaused(queryClient, deliverySettingsQuery().queryKey);
      // A live view learns of the save from Dispatch, as a delta, or as the snapshot a page that
      // had none (DELIVERY_NOT_CONFIGURED) is answered once a configuration exists.
      void queryClient.invalidateQueries({
        queryKey: ["delivery"],
        predicate: (query) => !isLiveQuery(query),
      });
      return holdThrough(read);
    },
  });
  // The fields and Save are held, not disabled, while a save is out within its deadline and until
  // what it changes is drawn: whichever holds the focus keeps it, an edit made then is dropped,
  // and a submit then sends nothing (`submitGuard` covers only the save's own round trip). A save
  // past its deadline (`KeyedWriteResult.late`) holds them no longer, and the guard lets go there
  // too, so a submit then sends the fields as a save of its own rather than nothing.
  const held = (save.isPending && !save.late) || drawing;
  useReleaseWhenLate(submitGuard, save.late);
  const edit = (next: Draft) => {
    if (!held) setDraft(next);
  };
  // A refusal speaks of the fields as the reader had them, so it goes once a new stored record
  // fills them again.
  const refused = useRefusalOfSubject(save, () => pressedOver === version);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // A held Save is only `aria-disabled`, so its press still submits the form: that submit sends
    // nothing.
    if (held) return;
    submitGuard.guard(() => {
      setPressedOver(version);
      save.mutate({
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
      });
    });
  };

  return (
    <>
      <form className={`mt-6 grid gap-4 rounded-xl border p-4 ${borderDefault}`} onSubmit={submit}>
        <fieldset
          aria-disabled={held || undefined}
          className="grid min-w-0 gap-4 aria-disabled:opacity-50 sm:grid-cols-2"
        >
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-repo`}>
            Deploy repository
            <input
              {...heldField(held)}
              className={settingsMonoInput}
              id={`${id}-deploy-repo`}
              onChange={(event) => edit({ ...draft, deploy_repo: event.target.value })}
              pattern={ownerRepoPattern}
              placeholder="owner/repo"
              required
              value={draft.deploy_repo}
            />
          </label>
          <label className={settingsFieldLabel} htmlFor={`${id}-deploy-workflow`}>
            Deploy workflow
            <input
              {...heldField(held)}
              className={settingsMonoInput}
              id={`${id}-deploy-workflow`}
              onChange={(event) =>
                edit({ ...draft, deploy_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<deploy>.yml"
              required
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
                {...heldField(held)}
                aria-describedby={`${id}-production-jobs-hint`}
                className={settingsMonoInput}
                id={`${id}-production-jobs`}
                onChange={(event) =>
                  edit({ ...draft, production_job_names: textOrEmpty(event.target.value) })
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
                {...heldField(held)}
                aria-describedby={`${id}-production-apply-step-hint`}
                className={settingsMonoInput}
                id={`${id}-production-apply-step`}
                onChange={(event) =>
                  edit({ ...draft, production_apply_step_name: event.target.value })
                }
                placeholder="The step whose success means the job applied (optional)"
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
                {...heldField(held)}
                aria-describedby={`${id}-pipeline-jobs-hint`}
                className={settingsMonoInput}
                id={`${id}-${key}`}
                onChange={(event) => edit({ ...draft, [key]: event.target.value })}
                placeholder={placeholder}
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
              {...heldField(held)}
              className={settingsMonoInput}
              id={`${id}-pr-checks-workflow`}
              onChange={(event) =>
                edit({ ...draft, pr_checks_workflow_path: textOrEmpty(event.target.value) })
              }
              placeholder=".github/workflows/<checks>.yml"
              required
              value={draft.pr_checks_workflow_path}
            />
          </label>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-population-authors`}>
              Population authors
              {/* One setting per line rather than prose: `delivery-population-authors` in
                  `DISPATCH_WRITING_BOXES` and `DISPATCH_READING_SURFACES` says why. */}
              <textarea
                {...heldField(held)}
                aria-describedby={`${id}-population-authors-hint`}
                className={settingsMonoInput}
                id={`${id}-population-authors`}
                onChange={(event) =>
                  edit({ ...draft, population_authors: listOrEmpty(event.target.value) })
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
                {...heldField(held)}
                aria-describedby={`${id}-excluded-repos-hint`}
                className={settingsMonoInput}
                id={`${id}-excluded-repos`}
                onChange={(event) => edit({ ...draft, excluded_repos: event.target.value })}
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
          aria-disabled={held || undefined}
          className={`justify-self-start ${settingsSubmitButton}`}
          type="submit"
        >
          Save delivery settings
        </button>
      </form>
      {refused ? (
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
      ) : unsettled === undefined ? null : (
        <div className="mt-4">
          {/* A read of the record that has not settled is said in the words the form says for a
              read of it that failed. */}
          <QueryError message={`${DELIVERY_SETTINGS_NOT_LOADED} ${unsettled}.`} />
        </div>
      )}
    </>
  );
}
