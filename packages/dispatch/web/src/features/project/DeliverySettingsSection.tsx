import { arrayMove } from "@dnd-kit/sortable";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import { agentSession } from "../../api/agent-session";
import { api, apiErrorMessage } from "../../api/client";
import {
  evalsAcrossProjectsQueryKey,
  evalsProjectKey,
  projectDeliverySettingsQuery,
  whoAmIQuery,
} from "../../api/queries";
import { isLiveQuery } from "../../api/query-refresh";
import type {
  AuthenticatedUser,
  CodeGroup,
  ProjectDeliverySettings,
  ProjectDeliverySettingsInput,
  ProjectDeploySource,
  ProjectDeploySourceInput,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import {
  ownerRepoPattern,
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  wholeValue,
} from "../settings/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** The characters that separate the entries of a list field, as a character class's body: line
 *  breaks, and commas or spaces too, since neither a GitHub login nor an `owner/repo` can hold
 *  one. */
const SEPARATOR_CHARACTERS = String.raw`\s,`;

/** A run of separators between two entries. */
const LIST_SEPARATOR = new RegExp(`[${SEPARATOR_CHARACTERS}]+`);

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

/** An optional field's text as the request sends it: trimmed, or `null` when it holds nothing. */
function textOrNull(text: string): string | null {
  const trimmed = text.trim();
  return trimmed === "" ? null : trimmed;
}

/** The check a source's repository box makes before Save sends: `owner/name`. The server holds
 *  the name to GitHub's own grammar, which is stricter, and its refusal shows under the form. */
const ownerRepo = wholeValue(ownerRepoPattern);

/** The most deploy sources one project holds (the settings route's own bound). */
const MAX_SOURCES = 50;

/** A switch's or a checkbox's label: the box beside its words, on one line. */
const checkboxLabel = `flex items-center gap-2 text-sm font-medium ${textSecondaryOnCanvas}`;

type SourceKind = ProjectDeploySource["kind"];

/** The seven evals whose targets the settings name (the spec's Design), each a number in its
 *  eval's own unit or `off`. `TARGET_LABELS` lists them in the order the route's own list
 *  (`deliverySettingsTargetSlugs`) gives, which is the order the editor shows them in. */
type TargetSlug = keyof NonNullable<ProjectDeliverySettings["targets"]>;

const TARGET_LABELS: Record<TargetSlug, string> = {
  "deploy-frequency": "Deploy frequency (deploys a day)",
  "change-fail-rate": "Change failure rate (a share from 0 to 1)",
  "merge-to-production": "Merge to production (minutes)",
  "pr-open-to-merge": "PR open to merge (minutes)",
  "deploy-run-success": "Deploy runs reaching production (a share from 0 to 1)",
  "failed-deploy-recovery": "Failed deployment recovery time (minutes)",
  "unowned-p0": "P0 issues with no owner",
};

const TARGET_SLUGS = Object.keys(TARGET_LABELS) as TargetSlug[];

/** A target that judges nothing. */
const TARGET_OFF = "off";

/** The failure sources a project may count, in the order the route lists them. */
const FAILURE_SOURCES = [
  { value: "check_results", label: "A check that fails after its merge" },
  { value: "reverts", label: "A merged revert" },
  { value: "incident_label", label: "An issue labelled incident" },
  { value: "events", label: "A failure posted to the project's production events" },
] as const;

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

/** What a code group's name may not hold: white space, a full stop or a control character. Each
 *  code eval names the group's series `<base>:<name>`, and Dispatch refuses a series name holding
 *  any of them; the server holds the name to that rule and to its length, and its refusal shows
 *  under the form. */
const GROUP_NAME_REFUSED = /[\s.\p{Cc}]/u;

/** One deploy source as typed: the lists are the textareas' text, split only when saved. */
interface SourceDraft {
  /** Tells the sources apart while they are edited: the stored source's id, or one Add made. */
  readonly key: string;
  kind: SourceKind;
  repo: string;
  deploy_workflow_path: string;
  production_job_names: string;
  production_apply_step_name: string;
  staging_e2e_job_name: string;
  staging_apply_job_name: string;
  pr_checks_workflow_path: string;
  pr_checks_rollup_job_name: string;
}

/** One code group as typed: the lists are the boxes' text, split only when saved. */
interface GroupDraft {
  /** Tells the groups apart while they are edited and moved: one the record or Add made. */
  readonly key: string;
  /** The stored group's lists, each held until its own box is edited: a save sends a list held
   *  here back as stored, so a prefix holding a comma stays one prefix whatever else changed.
   *  `undefined` for a group Add made, or a list whose box was edited. */
  readonly storedPrefixes: CodeGroup["branch_prefixes"] | undefined;
  readonly storedLogins: CodeGroup["author_logins"] | undefined;
  name: string;
  branch_prefixes: string;
  author_logins: string;
}

/** The boxes of a code group an edit changes. */
type GroupPatch = Partial<Pick<GroupDraft, "name" | "branch_prefixes" | "author_logins">>;

/** The fields as typed. A target left empty keeps the eval's target: the save leaves it out. */
interface Draft {
  delivery: boolean;
  sources: SourceDraft[];
  population_authors: string;
  excluded_repos: string;
  code_groups: GroupDraft[];
  failure_sources: readonly string[];
  failures_measured_since: string;
  targets: Record<TargetSlug, string>;
}

/** What Save refused before sending, and the fields it names: the repository boxes of `sources`
 *  (indexes into the draft's sources), the name boxes of `groupNames` and the prefix and login
 *  boxes of `groupLists` (indexes into the draft's code groups), or the target box of `target`. */
interface Refusal {
  readonly message: string;
  readonly sources?: readonly number[];
  readonly groupNames?: readonly number[];
  readonly groupLists?: readonly number[];
  readonly target?: TargetSlug;
}

/** Why `project`'s Save refuses every save of an agent viewer that names no session, `null`
 *  where the save names its writer: a person, whose cookie names them, or an agent whose
 *  credential proved a session or whose page's first address named one (`agentSession`). A
 *  bearer whose credential proves no session must name one as the save's writer, which Dispatch
 *  refuses `400 ACTOR_KIND` otherwise, so the editor sends nothing and says how to name it. */
function writerRefusal(project: string, viewer: AuthenticatedUser | undefined): Refusal | null {
  if (viewer?.kind !== "agent" || agentSession(viewer) !== undefined) return null;
  return {
    message: `An agent saves these settings as one of its sessions, and this page names none: open it at /projects/${project}/settings?session=<your session>.`,
  };
}

let addedSources = 0;

/** A new deploy source of `kind`, every field empty: a `github_workflow` one reads the project's
 *  default repository until it names one. */
function newSource(kind: SourceKind): SourceDraft {
  addedSources += 1;
  return {
    key: `added-${addedSources}`,
    kind,
    repo: "",
    deploy_workflow_path: "",
    production_job_names: "",
    production_apply_step_name: "",
    staging_e2e_job_name: "",
    staging_apply_job_name: "",
    pr_checks_workflow_path: "",
    pr_checks_rollup_job_name: "",
  };
}

let addedGroups = 0;

/** A new code group, every box empty. */
function newGroup(): GroupDraft {
  addedGroups += 1;
  return {
    key: `added-group-${addedGroups}`,
    storedPrefixes: undefined,
    storedLogins: undefined,
    name: "",
    branch_prefixes: "",
    author_logins: "",
  };
}

function groupDraft(group: CodeGroup, index: number): GroupDraft {
  return {
    key: `stored-group-${index}`,
    storedPrefixes: group.branch_prefixes,
    storedLogins: group.author_logins,
    name: group.name,
    branch_prefixes: group.branch_prefixes?.join(", ") ?? "",
    author_logins: group.author_logins?.join(", ") ?? "",
  };
}

function sourceDraft(source: ProjectDeploySource): SourceDraft {
  return {
    key: source.id,
    kind: source.kind,
    repo: source.repo ?? "",
    deploy_workflow_path: source.deploy_workflow_path ?? "",
    production_job_names: source.production_job_names?.join("\n") ?? "",
    production_apply_step_name: source.production_apply_step_name ?? "",
    staging_e2e_job_name: source.staging_e2e_job_name ?? "",
    staging_apply_job_name: source.staging_apply_job_name ?? "",
    pr_checks_workflow_path: source.pr_checks_workflow_path ?? "",
    pr_checks_rollup_job_name: source.pr_checks_rollup_job_name ?? "",
  };
}

/** The fields as the stored record holds them, and empty for a project with no record. */
function draftOf(record: ProjectDeliverySettings | null): Draft {
  const targets = {} as Record<TargetSlug, string>;
  for (const slug of TARGET_SLUGS) {
    const target = record?.targets?.[slug];
    targets[slug] = target === undefined ? "" : String(target);
  }
  return {
    delivery: record?.delivery ?? false,
    sources: record?.sources?.map(sourceDraft) ?? [],
    population_authors: record?.population_authors?.join("\n") ?? "",
    excluded_repos: record?.excluded_repos?.join("\n") ?? "",
    code_groups: record?.code_groups?.map(groupDraft) ?? [],
    failure_sources: record?.failure_sources ?? [],
    failures_measured_since: record?.failures_measured_since ?? "",
    targets,
  };
}

/** A stored source as a save sends it back unchanged. An `events` source names its kind alone. */
function storedSourceInput(source: ProjectDeploySource): ProjectDeploySourceInput {
  if (source.kind === "events") {
    return { kind: "events" };
  }
  return {
    kind: source.kind,
    repo: source.repo,
    deploy_workflow_path: source.deploy_workflow_path,
    production_job_names: source.production_job_names,
    production_apply_step_name: source.production_apply_step_name,
    staging_e2e_job_name: source.staging_e2e_job_name,
    staging_apply_job_name: source.staging_apply_job_name,
    pr_checks_workflow_path: source.pr_checks_workflow_path,
    pr_checks_rollup_job_name: source.pr_checks_rollup_job_name,
  };
}

/** The save of a project with delivery off: the switch, and every other field as stored, so
 *  turning delivery on again starts from them. It names no target, so each eval keeps its own. */
function storedInput(record: ProjectDeliverySettings | null): ProjectDeliverySettingsInput {
  return {
    delivery: false,
    sources: record?.sources?.map(storedSourceInput) ?? [],
    population_authors: record?.population_authors ?? [],
    excluded_repos: record?.excluded_repos ?? [],
    code_groups: record?.code_groups ?? [],
    failure_sources: record?.failure_sources ?? [],
    failures_measured_since: record?.failures_measured_since ?? null,
  };
}

function sourceInput(source: SourceDraft): ProjectDeploySourceInput {
  if (source.kind === "events") {
    return { kind: "events" };
  }
  return {
    kind: source.kind,
    repo: textOrNull(source.repo),
    deploy_workflow_path: source.deploy_workflow_path.trim(),
    production_job_names: lineEntries(source.production_job_names),
    production_apply_step_name: textOrNull(source.production_apply_step_name),
    staging_e2e_job_name: textOrNull(source.staging_e2e_job_name),
    staging_apply_job_name: textOrNull(source.staging_apply_job_name),
    pr_checks_workflow_path: source.pr_checks_workflow_path.trim(),
    pr_checks_rollup_job_name: textOrNull(source.pr_checks_rollup_job_name),
  };
}

/** A code group as a save sends it: the name trimmed, and each list as stored where its box was
 *  not edited, else split into its entries. */
function groupInput(group: GroupDraft): CodeGroup {
  return {
    name: group.name.trim(),
    branch_prefixes:
      group.storedPrefixes === undefined
        ? listEntries(group.branch_prefixes)
        : group.storedPrefixes,
    author_logins:
      group.storedLogins === undefined ? listEntries(group.author_logins) : group.storedLogins,
  };
}

/** A target box's text as a number or `off`, or `undefined` for one that is neither. */
function parsedTarget(text: string): number | typeof TARGET_OFF | undefined {
  const trimmed = text.trim();
  if (trimmed.toLowerCase() === TARGET_OFF) {
    return TARGET_OFF;
  }
  const value = Number(trimmed);
  return trimmed === "" || !Number.isFinite(value) ? undefined : value;
}

/** The save of a project with delivery on: every field as typed, and each target box that holds
 *  text (a box left empty keeps its eval's target). `refusalOf` passed the draft first. */
function draftInput(draft: Draft): ProjectDeliverySettingsInput {
  const targets: NonNullable<ProjectDeliverySettingsInput["targets"]> = {};
  for (const slug of TARGET_SLUGS) {
    const target = parsedTarget(draft.targets[slug]);
    if (target !== undefined) {
      targets[slug] = target;
    }
  }
  return {
    delivery: true,
    sources: draft.sources.map(sourceInput),
    population_authors: listEntries(draft.population_authors),
    excluded_repos: listEntries(draft.excluded_repos),
    code_groups: draft.code_groups.map(groupInput),
    failure_sources: FAILURE_SOURCES.map(({ value }) => value).filter((value) =>
      draft.failure_sources.includes(value)
    ),
    failures_measured_since: textOrNull(draft.failures_measured_since),
    targets,
  };
}

/** The line that names both settings a source naming no repository can read from. */
const NO_DEFAULT_REPOSITORY =
  "This project has no default repository: name this source's repository, or set the project's default repository in Settings → Projects.";

/** Why Save refuses a draft with delivery on before it sends anything, or `null` when it sends:
 *  no deploy source; a repository that is not `owner/name`; a source naming no repository of a
 *  project with no default repository; two sources that read one repository (a source naming
 *  none reads the default); a code group name holding a space, a full stop or a control
 *  character, or the name of an earlier group, capitals aside; two groups with no prefix and no
 *  login, or one that is not the last group; and a target that is neither a number nor `off`.
 *  The server holds the same rules, and the browser's own `required` checks hold every required
 *  field. */
function refusalOf(draft: Draft, defaultRepo: string | null): Refusal | null {
  if (draft.sources.length === 0) {
    return { message: "Delivery on needs a deploy source: add one, or turn delivery off." };
  }
  const readers = new Map<string, number>();
  for (const [index, source] of draft.sources.entries()) {
    if (source.kind !== "github_workflow") {
      continue;
    }
    const named = source.repo.trim();
    if (named !== "" && !ownerRepo.test(named)) {
      return {
        message: `Deploy source ${index + 1}: write its repository as owner/name.`,
        sources: [index],
      };
    }
    const repo = named === "" ? defaultRepo : named;
    if (repo === null) {
      return {
        message: `Deploy source ${index + 1} names no repository, and this project has no default repository: name the source's repository, or set the project's default repository in Settings → Projects.`,
        sources: [index],
      };
    }
    const first = readers.get(repo.toLowerCase());
    if (first !== undefined) {
      return {
        message: `Deploy sources ${first + 1} and ${index + 1} both read ${repo}: a repository is read by one deploy source at most.`,
        sources: [first, index],
      };
    }
    readers.set(repo.toLowerCase(), index);
  }
  const names = new Map<string, number>();
  let catchAll: number | null = null;
  for (const [index, draftGroup] of draft.code_groups.entries()) {
    const group = groupInput(draftGroup);
    if (GROUP_NAME_REFUSED.test(group.name)) {
      return {
        message: `Code group ${index + 1} (${group.name}): a name holds no space, full stop or control character, since each eval names the group's series after it.`,
        groupNames: [index],
      };
    }
    const first = names.get(group.name.toLowerCase());
    if (first !== undefined) {
      return {
        message: `Code groups ${first + 1} and ${index + 1} are both named ${group.name}, capitals aside: give each group a name of its own.`,
        groupNames: [first, index],
      };
    }
    names.set(group.name.toLowerCase(), index);
    if ((group.branch_prefixes?.length ?? 0) > 0 || (group.author_logins?.length ?? 0) > 0) {
      continue;
    }
    if (catchAll !== null) {
      return {
        message: `Code groups ${catchAll + 1} and ${index + 1} both have no prefix and no login: only one group can take every change the groups above it leave.`,
        groupLists: [catchAll, index],
      };
    }
    catchAll = index;
  }
  if (catchAll !== null && catchAll !== draft.code_groups.length - 1) {
    const group = groupInput(draft.code_groups[catchAll]);
    return {
      message: `Code group ${catchAll + 1} (${group.name}) has no prefix and no login, so it takes every change the groups above it leave: it can only be the last group.`,
      groupLists: [catchAll],
    };
  }
  for (const slug of TARGET_SLUGS) {
    const text = draft.targets[slug];
    if (text.trim() !== "" && parsedTarget(text) === undefined) {
      return {
        message: `${TARGET_LABELS[slug]}: a target is a number, or off.`,
        target: slug,
      };
    }
  }
  return null;
}

interface DeliverySettingsSectionProps {
  readonly project: string;
  /** The project's default repository (Settings → Projects), or `null` for none: what a source
   *  that names no repository reads. */
  readonly defaultRepo: string | null;
}

/**
 * One project's delivery settings, on its settings tab: the delivery switch; the deploy sources,
 * each a `github_workflow` (its repository, empty for the project's default repository, its
 * deploy workflow, production jobs, apply step, staging jobs, pull-request checks workflow and
 * roll-up job) or the project's posted deploys (`events`, one at most), with Add and Remove; the
 * population rule; the code groups, in order, each a name, head-branch prefixes and author
 * logins, with Add, Move up, Move down and Remove; the failure sources with the day they are
 * counted from; and the targets of the seven delivery evals, each a number or `off`. With
 * delivery off it shows the switch alone, and Save sends the stored fields back unchanged. Save
 * replaces the project's settings through `PUT /api/v1/projects/{key}/settings/delivery`; a
 * refusal shows under the form as the server wrote it and the draft stays. A save starts the
 * fields over from the record the server stored, and so does anyone else's save once the record
 * is read again.
 */
export function DeliverySettingsSection({
  project,
  defaultRepo,
}: DeliverySettingsSectionProps): ReactNode {
  const headingId = useId();
  const queryClient = useQueryClient();
  const settings = useQuery(projectDeliverySettingsQuery(project));
  const submitGuard = useSubmitGuard();
  const unnamedWriter = writerRefusal(project, useQuery(whoAmIQuery()).data);
  // The section shows its own refusal, under the fields it was refused over.
  const save = useKeyedWrite({
    method: "PUT",
    write: (input: ProjectDeliverySettingsInput, bounds) =>
      api.putProjectDeliverySettings(project, input, bounds),
    inline: true,
    label: () => `${project}'s delivery settings`,
    onSettled: () => submitGuard.release(),
    onSuccess: (answer) => {
      const settingsKey = projectDeliverySettingsQuery(project).queryKey;
      if (answer !== null) {
        queryClient.setQueryData(settingsKey, answer);
      }
      void queryClient.invalidateQueries({ queryKey: settingsKey });
      // A live view learns of the save from Dispatch, as a delta, or as the snapshot a page that
      // had none (DELIVERY_NOT_CONFIGURED) is answered once a project has delivery on.
      void queryClient.invalidateQueries({
        queryKey: ["delivery"],
        predicate: (query) => !isLiveQuery(query),
      });
      // Each target the save changed is a new version of the project's eval, which the Evals
      // tab's reads and the cross-project read hold; the tab's live views learn of it from
      // Dispatch, as a delta.
      void queryClient.invalidateQueries({
        queryKey: evalsProjectKey(project),
        predicate: (query) => !isLiveQuery(query),
      });
      void queryClient.invalidateQueries({ queryKey: evalsAcrossProjectsQueryKey });
    },
  });

  let body: ReactNode;
  // Only a first read that failed takes the fields away: a re-read that fails after a record
  // loaded keeps that record, and the draft drawn from it.
  if (settings.isPending || (settings.isLoadingError && settings.isFetching)) {
    body = <p className={`mt-4 ${textMutedOnCanvas}`}>Loading delivery settings…</p>;
  } else if (settings.isLoadingError) {
    body = (
      <div className="mt-4">
        <QueryError
          message="Couldn't load the delivery settings."
          onRetry={() => void settings.refetch()}
        />
      </div>
    );
  } else {
    const record = settings.data;
    const saved =
      save.isSuccess && (save.data === null || save.data.updated_at === record?.updated_at);
    body = (
      <>
        {/* Keyed by the record's write time, so a save (anyone's) starts the fields over from
            the record as the server stored it. */}
        <DeliverySettingsFields
          defaultRepo={defaultRepo}
          key={record?.updated_at ?? "unset"}
          onRefused={() => save.reset()}
          onSave={(input) => submitGuard.guard(() => save.mutate(input))}
          pending={save.isPending}
          project={project}
          record={record}
          unnamedWriter={unnamedWriter}
        />
        {saved ? (
          <p className={`mt-4 text-sm ${textSecondaryOnCanvas}`} role="status">
            Delivery settings saved
          </p>
        ) : null}
        {save.isError ? (
          <div className="mt-4">
            {/* A save the reader stopped may have been made, and the form's button is a press
                of its own: it sends what the fields hold then. */}
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

  return (
    <section aria-labelledby={headingId} className="max-w-4xl">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        Delivery
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Whether this project deploys, where its deploys come from, which failures count, and the
        targets its delivery evals are judged by. Every reader of the project reads them; a person,
        or an agent whose token has deployment scope, saves them, and the save checks that the
        Dispatch GitHub App can read each source&apos;s repository.
      </p>
      {body}
    </section>
  );
}

interface DeliverySettingsFieldsProps {
  readonly project: string;
  readonly record: ProjectDeliverySettings | null;
  readonly defaultRepo: string | null;
  readonly pending: boolean;
  readonly onSave: (input: ProjectDeliverySettingsInput) => void;
  /** Save refused the draft before sending: the last save's outcome no longer speaks for it. */
  readonly onRefused: () => void;
  /** Why every save is refused before sending, whatever the draft holds: an agent viewer that
   *  names no session to save as (`writerRefusal`); `null` where the save names its writer. */
  readonly unnamedWriter: Refusal | null;
}

function DeliverySettingsFields({
  project,
  record,
  defaultRepo,
  pending,
  onSave,
  onRefused,
  unnamedWriter,
}: DeliverySettingsFieldsProps): ReactNode {
  const id = useId();
  const [draft, setDraft] = useState<Draft>(() => draftOf(record));
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const update = (next: Draft) => {
    setDraft(next);
    setRefusal(null);
  };
  const updateSource = (index: number, patch: Partial<SourceDraft>) =>
    update({
      ...draft,
      sources: draft.sources.map((source, at) => (at === index ? { ...source, ...patch } : source)),
    });
  // An edit of a box lets go of the stored list it shows, and of no other: Save sends what that
  // box holds, and each other list as stored.
  const updateGroup = (index: number, patch: GroupPatch) =>
    update({
      ...draft,
      code_groups: draft.code_groups.map((group, at) =>
        at === index
          ? {
              ...group,
              ...patch,
              storedPrefixes:
                patch.branch_prefixes === undefined ? group.storedPrefixes : undefined,
              storedLogins: patch.author_logins === undefined ? group.storedLogins : undefined,
            }
          : group
      ),
    });
  const moveGroup = (from: number, to: number) =>
    update({ ...draft, code_groups: arrayMove(draft.code_groups, from, to) });
  const hasEvents = draft.sources.some((source) => source.kind === "events");

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (unnamedWriter !== null) {
      setRefusal(unnamedWriter);
      onRefused();
      return;
    }
    if (!draft.delivery) {
      onSave(storedInput(record));
      return;
    }
    const refused = refusalOf(draft, defaultRepo);
    setRefusal(refused);
    if (refused !== null) {
      onRefused();
      return;
    }
    onSave(draftInput(draft));
  };

  return (
    <>
      <form className={`mt-4 grid gap-4 rounded-xl border p-4 ${borderDefault}`} onSubmit={submit}>
        <fieldset className="grid min-w-0 gap-4" disabled={pending}>
          <div className="grid gap-1">
            <label className={checkboxLabel}>
              <input
                aria-checked={draft.delivery}
                aria-describedby={`${id}-delivery-hint`}
                checked={draft.delivery}
                onChange={(event) => {
                  const delivery = event.target.checked;
                  update({
                    ...draft,
                    delivery,
                    sources:
                      delivery && draft.sources.length === 0
                        ? [newSource("github_workflow")]
                        : draft.sources,
                  });
                }}
                role="switch"
                type="checkbox"
              />
              Delivery
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-delivery-hint`}>
              Off, the project deploys nothing: its delivery evals count none of its runs, and the
              settings stored below are kept for when it is turned on again.
            </p>
          </div>
          <div className="grid gap-1">
            <label className={settingsFieldLabel} htmlFor={`${id}-default-repo`}>
              Default repository
              <input
                aria-describedby={`${id}-default-repo-hint`}
                className={settingsMonoInput}
                id={`${id}-default-repo`}
                placeholder="None"
                readOnly
                value={defaultRepo ?? ""}
              />
            </label>
            <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-default-repo-hint`}>
              Set in{" "}
              <Link className={`font-medium ${linkText} ${linkHoverText}`} to="/settings">
                Settings → Projects
              </Link>
              . A deploy source that names no repository reads it, and moves with it.
            </p>
          </div>
          {draft.delivery ? (
            <>
              <fieldset className="grid min-w-0 gap-4">
                <legend className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
                  Deploy sources
                </legend>
                {draft.sources.map((source, index) => (
                  <SourceFields
                    canBeEvents={source.kind === "events" || !hasEvents}
                    defaultRepo={defaultRepo}
                    id={`${id}-${source.key}`}
                    index={index}
                    key={source.key}
                    onChange={(patch) => updateSource(index, patch)}
                    onRemove={() =>
                      update({ ...draft, sources: draft.sources.filter((_, at) => at !== index) })
                    }
                    project={project}
                    repoRefused={refusal?.sources?.includes(index) ?? false}
                    source={source}
                  />
                ))}
                {draft.sources.length < MAX_SOURCES ? (
                  <div className="flex flex-wrap gap-x-4 gap-y-2">
                    <button
                      className={`justify-self-start font-medium ${linkText} ${linkHoverText}`}
                      onClick={() =>
                        update({
                          ...draft,
                          sources: [...draft.sources, newSource("github_workflow")],
                        })
                      }
                      type="button"
                    >
                      Add a GitHub workflow source
                    </button>
                    {hasEvents ? null : (
                      <button
                        className={`justify-self-start font-medium ${linkText} ${linkHoverText}`}
                        onClick={() =>
                          update({ ...draft, sources: [...draft.sources, newSource("events")] })
                        }
                        type="button"
                      >
                        Add the posted deploys source
                      </button>
                    )}
                  </div>
                ) : null}
              </fieldset>
              <fieldset className="grid min-w-0 gap-4 sm:grid-cols-2">
                <legend className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
                  Population
                </legend>
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
                        update({ ...draft, population_authors: event.target.value })
                      }
                      placeholder="One GitHub login per line"
                      rows={4}
                      value={draft.population_authors}
                    />
                  </label>
                  <p
                    className={`text-xs ${textMutedOnCanvas}`}
                    id={`${id}-population-authors-hint`}
                  >
                    Whose merged pull requests count, as GitHub spells each login: an App&apos;s
                    ends in [bot].
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
                      onChange={(event) => update({ ...draft, excluded_repos: event.target.value })}
                      placeholder="One owner/repo per line"
                      rows={4}
                      value={draft.excluded_repos}
                    />
                  </label>
                  <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-excluded-repos-hint`}>
                    Repositories whose merged pull requests never count, as GitHub spells each
                    owner/repo, capitals included.
                  </p>
                </div>
              </fieldset>
              <fieldset className="grid min-w-0 gap-4">
                <legend className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
                  Code groups
                </legend>
                <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-code-groups-hint`}>
                  The code survival and post-merge rework evals count each merged change for the
                  first group, top to bottom, with a head-branch prefix that starts its branch or an
                  author login that opened it, as GitHub spells the login: an App&apos;s ends in
                  [bot]. A group with no prefix and no login takes every change the groups above it
                  leave, so it can only be the last group. Each eval names a group&apos;s series
                  after it, so a name holds no space, full stop or control character.
                </p>
                {draft.code_groups.map((group, index) => (
                  <GroupFields
                    count={draft.code_groups.length}
                    group={group}
                    hintId={`${id}-code-groups-hint`}
                    id={`${id}-${group.key}`}
                    index={index}
                    key={group.key}
                    listsRefused={refusal?.groupLists?.includes(index) ?? false}
                    nameRefused={refusal?.groupNames?.includes(index) ?? false}
                    onChange={(patch) => updateGroup(index, patch)}
                    onMove={(to) => moveGroup(index, to)}
                    onRemove={() =>
                      update({
                        ...draft,
                        code_groups: draft.code_groups.filter((_, at) => at !== index),
                      })
                    }
                  />
                ))}
                <button
                  className={`justify-self-start font-medium ${linkText} ${linkHoverText}`}
                  onClick={() =>
                    update({ ...draft, code_groups: [...draft.code_groups, newGroup()] })
                  }
                  type="button"
                >
                  Add a code group
                </button>
              </fieldset>
              <fieldset className="grid min-w-0 gap-2">
                <legend className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
                  Failure sources
                </legend>
                {FAILURE_SOURCES.map(({ value, label }) => (
                  <label className={checkboxLabel} key={value}>
                    <input
                      checked={draft.failure_sources.includes(value)}
                      onChange={(event) =>
                        update({
                          ...draft,
                          failure_sources: event.target.checked
                            ? [...draft.failure_sources, value]
                            : draft.failure_sources.filter((chosen) => chosen !== value),
                        })
                      }
                      type="checkbox"
                    />
                    {label}
                  </label>
                ))}
                <div className="mt-2 grid gap-1">
                  <label className={settingsFieldLabel} htmlFor={`${id}-failures-since`}>
                    Failures counted from
                    <input
                      aria-describedby={`${id}-failures-since-hint`}
                      className={`justify-self-start ${settingsMonoInput}`}
                      id={`${id}-failures-since`}
                      onChange={(event) =>
                        update({ ...draft, failures_measured_since: event.target.value })
                      }
                      required={draft.failure_sources.length > 0}
                      type="date"
                      value={draft.failures_measured_since}
                    />
                  </label>
                  <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-failures-since-hint`}>
                    The first UTC day the failure measures count: a window that starts before it
                    reads no data. Needed once a failure source is on.
                  </p>
                </div>
              </fieldset>
              <fieldset className="grid min-w-0 gap-4 sm:grid-cols-2">
                <legend className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
                  Targets
                </legend>
                <p
                  className={`text-xs sm:col-span-2 ${textMutedOnCanvas}`}
                  id={`${id}-targets-hint`}
                >
                  Each a number in its eval&apos;s unit, or off for no target. Saved as a new
                  version of the project&apos;s eval; a box left empty keeps the eval&apos;s target.
                </p>
                {TARGET_SLUGS.map((slug) => (
                  <label className={settingsFieldLabel} htmlFor={`${id}-target-${slug}`} key={slug}>
                    {TARGET_LABELS[slug]}
                    <input
                      aria-describedby={`${id}-targets-hint`}
                      aria-invalid={refusal?.target === slug || undefined}
                      className={settingsMonoInput}
                      id={`${id}-target-${slug}`}
                      inputMode="decimal"
                      onChange={(event) =>
                        update({
                          ...draft,
                          targets: { ...draft.targets, [slug]: event.target.value },
                        })
                      }
                      placeholder="A number, or off"
                      value={draft.targets[slug]}
                    />
                  </label>
                ))}
              </fieldset>
            </>
          ) : null}
        </fieldset>
        <button
          className={`justify-self-start ${settingsSubmitButton}`}
          disabled={pending}
          type="submit"
        >
          Save delivery settings
        </button>
      </form>
      {refusal === null ? null : (
        <p className={`mt-4 text-sm ${dangerText}`} role="alert">
          {refusal.message}
        </p>
      )}
    </>
  );
}

interface SourceFieldsProps {
  readonly project: string;
  readonly source: SourceDraft;
  readonly index: number;
  readonly id: string;
  readonly defaultRepo: string | null;
  /** Whether the kind box offers `events`: not where another source of the project is one. */
  readonly canBeEvents: boolean;
  /** Whether Save refused this source's repository box. */
  readonly repoRefused: boolean;
  readonly onChange: (patch: Partial<SourceDraft>) => void;
  readonly onRemove: () => void;
}

/** One deploy source's fields, under the legend `Deploy source <n>`. */
function SourceFields({
  project,
  source,
  index,
  id,
  defaultRepo,
  canBeEvents,
  repoRefused,
  onChange,
  onRemove,
}: SourceFieldsProps): ReactNode {
  const name = `Deploy source ${index + 1}`;
  return (
    <fieldset
      className={`grid min-w-0 gap-4 rounded-lg border p-3 sm:grid-cols-2 ${borderDefault}`}
    >
      <legend className={`px-1 text-sm font-semibold ${textPrimaryOnCanvas}`}>{name}</legend>
      <label className={settingsFieldLabel} htmlFor={`${id}-kind`}>
        Kind
        <select
          className={settingsMonoInput}
          id={`${id}-kind`}
          onChange={(event) => onChange({ kind: event.target.value as SourceKind })}
          value={source.kind}
        >
          <option value="github_workflow">GitHub workflow</option>
          {canBeEvents ? <option value="events">Posted deploys</option> : null}
        </select>
      </label>
      <button
        aria-label={`Remove ${name.toLowerCase()}`}
        className={`self-end justify-self-start font-medium ${dangerText}`}
        onClick={onRemove}
        type="button"
      >
        Remove
      </button>
      {source.kind === "events" ? (
        <p className={`text-sm sm:col-span-2 ${textSecondaryOnCanvas}`}>
          Counts the deploys posted to POST /api/v1/projects/{project}/production-events, each on
          the day it finished.
        </p>
      ) : (
        <GitHubWorkflowFields
          defaultRepo={defaultRepo}
          id={id}
          onChange={onChange}
          repoRefused={repoRefused}
          source={source}
        />
      )}
    </fieldset>
  );
}

interface GroupFieldsProps {
  readonly group: GroupDraft;
  readonly index: number;
  /** How many groups the draft holds: the last one does not move down. */
  readonly count: number;
  readonly id: string;
  /** The code groups' help text, which describes each group's boxes. */
  readonly hintId: string;
  /** Whether Save refused this group's name box. */
  readonly nameRefused: boolean;
  /** Whether Save refused this group's prefix and login boxes. */
  readonly listsRefused: boolean;
  readonly onChange: (patch: GroupPatch) => void;
  /** Moves the group to place `to`, from 0. */
  readonly onMove: (to: number) => void;
  readonly onRemove: () => void;
}

/**
 * One code group's fields, under the legend `Code group <n>`. Its groups, in order: the name box;
 * the order buttons (Move up, Move down, Remove), which wrap inside their group; the head-branch
 * prefixes box; and the author logins box. Two to a row from `sm` up, one below it.
 */
function GroupFields({
  group,
  index,
  count,
  id,
  hintId,
  nameRefused,
  listsRefused,
  onChange,
  onMove,
  onRemove,
}: GroupFieldsProps): ReactNode {
  const legend = `Code group ${index + 1}`;
  const orderButton = `font-medium disabled:cursor-not-allowed disabled:opacity-50 ${linkText} ${linkHoverText}`;
  return (
    <fieldset
      className={`grid min-w-0 gap-4 rounded-lg border p-3 sm:grid-cols-2 ${borderDefault}`}
    >
      <legend className={`px-1 text-sm font-semibold ${textPrimaryOnCanvas}`}>{legend}</legend>
      <label className={settingsFieldLabel} htmlFor={`${id}-name`}>
        Name
        <input
          aria-describedby={hintId}
          aria-invalid={nameRefused || undefined}
          autoComplete="off"
          className={settingsMonoInput}
          id={`${id}-name`}
          onChange={(event) => onChange({ name: textOrEmpty(event.target.value) })}
          placeholder="A name, such as agents"
          required
          spellCheck={false}
          value={group.name}
        />
      </label>
      <div className="flex flex-wrap gap-x-4 gap-y-2 self-end">
        <button
          aria-label={`Move ${legend.toLowerCase()} up`}
          className={orderButton}
          disabled={index === 0}
          onClick={() => onMove(index - 1)}
          type="button"
        >
          Move up
        </button>
        <button
          aria-label={`Move ${legend.toLowerCase()} down`}
          className={orderButton}
          disabled={index === count - 1}
          onClick={() => onMove(index + 1)}
          type="button"
        >
          Move down
        </button>
        <button
          aria-label={`Remove ${legend.toLowerCase()}`}
          className={`font-medium ${dangerText}`}
          onClick={onRemove}
          type="button"
        >
          Remove
        </button>
      </div>
      <label className={settingsFieldLabel} htmlFor={`${id}-prefixes`}>
        Head-branch prefixes
        <input
          aria-describedby={hintId}
          aria-invalid={listsRefused || undefined}
          autoComplete="off"
          className={settingsMonoInput}
          id={`${id}-prefixes`}
          onChange={(event) => onChange({ branch_prefixes: event.target.value })}
          placeholder="Separated by spaces or commas"
          spellCheck={false}
          value={group.branch_prefixes}
        />
      </label>
      <label className={settingsFieldLabel} htmlFor={`${id}-logins`}>
        Author logins
        <input
          aria-describedby={hintId}
          aria-invalid={listsRefused || undefined}
          autoComplete="off"
          className={settingsMonoInput}
          id={`${id}-logins`}
          onChange={(event) => onChange({ author_logins: event.target.value })}
          placeholder="GitHub logins, separated by spaces or commas"
          spellCheck={false}
          value={group.author_logins}
        />
      </label>
    </fieldset>
  );
}

/** A `github_workflow` source's repository and rule fields. */
function GitHubWorkflowFields({
  source,
  id,
  defaultRepo,
  repoRefused,
  onChange,
}: Pick<
  SourceFieldsProps,
  "source" | "id" | "defaultRepo" | "repoRefused" | "onChange"
>): ReactNode {
  return (
    <>
      <div className="grid gap-1 sm:col-span-2">
        <label className={settingsFieldLabel} htmlFor={`${id}-repo`}>
          Repository
          <input
            aria-describedby={`${id}-repo-hint`}
            aria-invalid={repoRefused || undefined}
            autoComplete="off"
            className={settingsMonoInput}
            id={`${id}-repo`}
            onChange={(event) => onChange({ repo: event.target.value })}
            placeholder={defaultRepo ?? "owner/name"}
            spellCheck={false}
            value={source.repo}
          />
        </label>
        <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-repo-hint`}>
          {defaultRepo === null
            ? NO_DEFAULT_REPOSITORY
            : "Empty, the source reads the project's default repository, and moves with it."}
        </p>
      </div>
      <label className={settingsFieldLabel} htmlFor={`${id}-deploy-workflow`}>
        Deploy workflow
        <input
          className={settingsMonoInput}
          id={`${id}-deploy-workflow`}
          onChange={(event) => onChange({ deploy_workflow_path: textOrEmpty(event.target.value) })}
          placeholder=".github/workflows/<deploy>.yml"
          required
          value={source.deploy_workflow_path}
        />
      </label>
      <label className={settingsFieldLabel} htmlFor={`${id}-pr-checks-workflow`}>
        PR checks workflow
        <input
          className={settingsMonoInput}
          id={`${id}-pr-checks-workflow`}
          onChange={(event) =>
            onChange({ pr_checks_workflow_path: textOrEmpty(event.target.value) })
          }
          placeholder=".github/workflows/<checks>.yml"
          required
          value={source.pr_checks_workflow_path}
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
              onChange({ production_job_names: textOrEmpty(event.target.value) })
            }
            placeholder="One job name per line, as a deploy run lists it"
            required
            rows={2}
            value={source.production_job_names}
          />
        </label>
        <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-production-jobs-hint`}>
          The jobs whose success counts as a production deploy, one per line, named as a run lists
          each: a reusable workflow&apos;s job reads &lt;caller&gt; / &lt;job&gt;. After a rename,
          keep the old name beside the new so history still counts.
        </p>
      </div>
      <div className="grid gap-1">
        <label className={settingsFieldLabel} htmlFor={`${id}-production-apply-step`}>
          Production apply step
          <input
            aria-describedby={`${id}-production-apply-step-hint`}
            className={settingsMonoInput}
            id={`${id}-production-apply-step`}
            onChange={(event) => onChange({ production_apply_step_name: event.target.value })}
            placeholder="The step whose success means the job applied (optional)"
            value={source.production_apply_step_name}
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
            onChange={(event) => onChange({ [key]: event.target.value })}
            placeholder={placeholder}
            value={source[key]}
          />
        </label>
      ))}
      <p className={`text-xs ${textMutedOnCanvas}`} id={`${id}-pipeline-jobs-hint`}>
        Leave empty if the pipeline has no such step; the Pipeline page omits that card. With no
        roll-up job the failing-jobs table leaves nothing out.
      </p>
    </>
  );
}
