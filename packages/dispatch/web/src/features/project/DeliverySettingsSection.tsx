import { arrayMove } from "@dnd-kit/sortable";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useRef, useState } from "react";
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
import { compareTimestamps } from "../../lib/timestamps";
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

/** The fields as typed. A target left as it was read, or left empty, keeps the eval's target: the
 *  save leaves it out (`editedTargets`). */
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

/** The later of two versions of the record, by when Dispatch wrote each (`updated_at`): `held`,
 *  as a reader of it has it, and `other`, which may be newer - a save's own answer, or the
 *  cache's record beside the one the fields were handed - and wins a tie. No record, or none read
 *  yet, gives way to any record. */
function laterRecord(
  held: ProjectDeliverySettings | null | undefined,
  other: ProjectDeliverySettings | null | undefined
): ProjectDeliverySettings | null {
  if (other == null) return held ?? null;
  if (held == null) return other;
  return compareTimestamps(other.updated_at, held.updated_at) >= 0 ? other : held;
}

/** A deploy source's fields as typed, each a leaf of the draft of its own (`leavesOf`). */
const SOURCE_FIELDS = [
  "kind",
  "repo",
  "deploy_workflow_path",
  "production_job_names",
  "production_apply_step_name",
  "staging_e2e_job_name",
  "staging_apply_job_name",
  "pr_checks_workflow_path",
  "pr_checks_rollup_job_name",
] as const satisfies readonly (keyof SourceDraft)[];

/** A code group's boxes as typed and its stored lists, each a leaf of the draft of its own
 *  (`leavesOf`), keyed by the leaf's name after the group's key. */
const GROUP_FIELDS = {
  name: "name",
  branch_prefixes: "branch_prefixes",
  author_logins: "author_logins",
  storedPrefixes: "stored_prefixes",
  storedLogins: "stored_logins",
} as const satisfies Record<Exclude<keyof GroupDraft, "key">, string>;

/** One thing the reader changes as a whole, which a record that changes under the form either
 *  keeps as they typed it or takes from the record: the switch, a text box, the failure sources
 *  (sorted, as the boxes show no order), a target box (`target:<slug>`), the deploy sources' keys
 *  in their order (`sources`), one field of one source (`source:<key>:<field>`), the code groups'
 *  keys in their order (`code_groups`), or one box or stored list of one group
 *  (`group:<key>:<field>`), a stored list `null` or `undefined` where the record held none or its
 *  box was edited. */
type Leaf = boolean | string | readonly string[] | null | undefined;

/** A draft's leaves by name (`leavesOf`). */
type Leaves = Readonly<Record<string, Leaf>>;

function sameLeaf(left: Leaf | undefined, right: Leaf | undefined): boolean {
  if (Array.isArray(left) && Array.isArray(right)) {
    return left.length === right.length && left.every((entry, at) => entry === right[at]);
  }
  return left === right;
}

/** `draft` as its leaves. */
function leavesOf(draft: Draft): Leaves {
  const leaves: Record<string, Leaf> = {
    delivery: draft.delivery,
    excluded_repos: draft.excluded_repos,
    failure_sources: [...draft.failure_sources].sort(),
    failures_measured_since: draft.failures_measured_since,
    population_authors: draft.population_authors,
    sources: draft.sources.map((source) => source.key),
    code_groups: draft.code_groups.map((group) => group.key),
  };
  for (const slug of TARGET_SLUGS) leaves[`target:${slug}`] = draft.targets[slug];
  for (const source of draft.sources) {
    for (const field of SOURCE_FIELDS) leaves[`source:${source.key}:${field}`] = source[field];
  }
  for (const group of draft.code_groups) {
    for (const [field, leaf] of Object.entries(GROUP_FIELDS)) {
      leaves[`group:${group.key}:${leaf}`] = group[field as keyof typeof GROUP_FIELDS];
    }
  }
  return leaves;
}

/** The draft `leaves` holds: each of its sources and code groups, in their order, with every
 *  field of it. */
function draftOfLeaves(leaves: Leaves): Draft {
  const text = (name: string) => leaves[name] as string;
  const targets = {} as Record<TargetSlug, string>;
  for (const slug of TARGET_SLUGS) targets[slug] = text(`target:${slug}`);
  return {
    delivery: leaves.delivery as boolean,
    excluded_repos: text("excluded_repos"),
    failure_sources: leaves.failure_sources as readonly string[],
    failures_measured_since: text("failures_measured_since"),
    population_authors: text("population_authors"),
    sources: (leaves.sources as readonly string[]).map(
      (key) =>
        ({
          key,
          ...Object.fromEntries(
            SOURCE_FIELDS.map((field) => [field, leaves[`source:${key}:${field}`]])
          ),
        }) as SourceDraft
    ),
    code_groups: (leaves.code_groups as readonly string[]).map(
      (key) =>
        ({
          key,
          ...Object.fromEntries(
            Object.entries(GROUP_FIELDS).map(([field, leaf]) => [
              field,
              leaves[`group:${key}:${leaf}`],
            ])
          ),
        }) as GroupDraft
    ),
    targets,
  };
}

/** The leaves of `next` that differ from `stored`'s: what the reader has changed. */
function changedLeaves(next: Leaves, stored: Leaves): Leaves {
  const changed: Record<string, Leaf> = {};
  for (const [name, leaf] of Object.entries(next)) {
    if (!sameLeaf(leaf, stored[name])) changed[name] = leaf;
  }
  return changed;
}

/** The leaves of `sent` that `edited` still holds as sent: the ones this tab's Save sent that the
 *  reader has not changed since. */
function stillSent(sent: Leaves, edited: Leaves): Leaves {
  const kept: Record<string, Leaf> = {};
  for (const [name, leaf] of Object.entries(sent)) {
    if (sameLeaf(edited[name], leaf)) kept[name] = leaf;
  }
  return kept;
}

/** What the reader has changed over the record, and what this tab's newest Save sent of it. */
interface Changes {
  /** The leaves the reader has changed, as typed. */
  readonly edited: Leaves;
  /** The newest Save's: the leaves it sent that the reader has not changed since (`stillSent`),
   *  and the Save itself, so only its own landing retires them. `null` before any Save, and once
   *  Dispatch has refused it or its landing has retired them. */
  readonly sent: { readonly fields: Leaves; readonly save: object } | null;
}

const NO_CHANGES: Changes = { edited: {}, sent: null };

/** `changes` once the Save `save` has landed and the record holds what it sent: the leaves it
 *  sent that the reader has not changed since follow the record again. Where a newer Save is out
 *  or this one was refused, nothing changes. */
function retired(changes: Changes, save: object): Changes {
  if (changes.sent?.save !== save) return changes;
  const edited = { ...changes.edited };
  for (const name of Object.keys(changes.sent.fields)) delete edited[name];
  return { edited, sent: null };
}

/** The fields of the record a save starts from: every field it read, one this build does not know
 *  included, so a field of a newer build survives a save of this one; but not its targets, each a
 *  version of its eval that another write may have moved since the read, which a save names only
 *  where the reader changed it (`editedTargets`). */
function recordFields(record: ProjectDeliverySettings | null) {
  if (record === null) {
    return {};
  }
  const { targets: _eachEvalsOwn, ...fields } = record;
  return fields;
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

/** With delivery off, start from the stored record and discard edits to the hidden fields, the
 *  targets among them: the save names no target, so each eval keeps the target it holds. */
function storedInput(record: ProjectDeliverySettings | null): ProjectDeliverySettingsInput {
  return {
    ...recordFields(record),
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

/** Each target the reader changed over the record as the form now holds it (`stored`), as a save
 *  sends it. A box that holds the record's target, or nothing, keeps its eval's target, so the
 *  save leaves it out: a target another write moved while the form was open stands, and a box the
 *  reader did not touch shows it once the record is read again. */
function editedTargets(
  draft: Draft,
  stored: Draft
): NonNullable<ProjectDeliverySettingsInput["targets"]> {
  const targets: NonNullable<ProjectDeliverySettingsInput["targets"]> = {};
  for (const slug of TARGET_SLUGS) {
    const target = parsedTarget(draft.targets[slug]);
    if (target !== undefined && target !== parsedTarget(stored.targets[slug])) {
      targets[slug] = target;
    }
  }
  return targets;
}

/** The save of a project with delivery on: built from the record the fields follow (sending back
 *  every field the form does not edit, one it does not know included, but the targets:
 *  `recordFields`), overwriting the fields the form edits, and naming each target the reader
 *  changed (`editedTargets`). `refusalOf` passed the draft first. */
function draftInput(
  draft: Draft,
  stored: Draft,
  record: ProjectDeliverySettings | null
): ProjectDeliverySettingsInput {
  return {
    ...recordFields(record),
    delivery: true,
    sources: draft.sources.map(sourceInput),
    population_authors: listEntries(draft.population_authors),
    excluded_repos: listEntries(draft.excluded_repos),
    code_groups: draft.code_groups.map(groupInput),
    failure_sources: FAILURE_SOURCES.map(({ value }) => value).filter((value) =>
      draft.failure_sources.includes(value)
    ),
    failures_measured_since: textOrNull(draft.failures_measured_since),
    targets: editedTargets(draft, stored),
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
 * delivery off it shows the switch alone, and Save sends the stored fields back unchanged; a save
 * names only the targets the reader changed, so a target another write moved while the form was
 * open stands. Save updates the project's settings through
 * `PUT /api/v1/projects/{key}/settings/delivery`; a refusal shows under the form as the server
 * wrote it and the draft stays. A save writes Dispatch's answer as the record, and refreshes the
 * record and the evals the save moves.
 *
 * Its groups, in this order, each on a line of its own: Heading (the title and what the settings
 * are); Fields - the switch, the default repository and, with delivery on, Deploy sources (each
 * source a group of its own: Kind and Remove, then its fields; then the Add controls),
 * Population, Code groups (each group a group of its own: its name, Move up, Move down and
 * Remove, then its prefixes and logins; then Add), Failure sources and Targets, whose fields are
 * two columns from `sm` and one below it; Save; and under them the refusal of a save, or that it
 * saved. A one-line value longer than its field ends in an ellipsis with the whole value as its
 * title; a list wraps inside its own box. The fields the reader has changed stay as they typed
 * them when the record changes under them - anyone's save - and the rest follow the record, as
 * does a changed field once this tab's Save of it has landed: that Save's answer is the record
 * from then on.
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
      // The answer is the record from now on, written where the fields and every other reader of
      // it read it, unless a newer one is there already: another writer's, read meanwhile. The
      // fields this save sent follow it in the render that takes it in (`retired`).
      if (answer !== null) {
        queryClient.setQueryData(settingsKey, (current) => laterRecord(current, answer));
      }
      void queryClient.invalidateQueries({ queryKey: settingsKey });
      // Each target the save changed is a new version of the project's eval, which the Evals
      // tab's reads and the cross-project read hold. The tab's live views learn of the save from
      // Dispatch, as a delta, or as the snapshot a view that had none (DELIVERY_NOT_CONFIGURED) is
      // answered once a project has delivery on; an open run's drill-down is read again when the
      // timeline's delta changes its run.
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
        <DeliverySettingsFields
          defaultRepo={defaultRepo}
          onRefused={() => save.reset()}
          onSave={(input, press) => submitGuard.guard(() => save.mutate(input, press))}
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

/** What one Save's landing or refusal tells the fields that sent it. */
interface SavePress {
  readonly onError: () => void;
  readonly onSuccess: (answer: ProjectDeliverySettings | null) => void;
}

interface DeliverySettingsFieldsProps {
  readonly project: string;
  /** The record as the section read it, which the cache's own may be newer than. */
  readonly record: ProjectDeliverySettings | null;
  readonly defaultRepo: string | null;
  readonly pending: boolean;
  /** Sends `input`, telling `press` of its landing or refusal, unless a save is out: whether it
   *  sent. */
  readonly onSave: (input: ProjectDeliverySettingsInput, press: SavePress) => boolean;
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
  // The record the fields follow: the one the section was handed, or the cache's where that is
  // newer. A save writes its own answer there before the callback that retires the fields it sent
  // runs (the section's `onSuccess`), and this read of the cache takes it in that same render,
  // while TanStack Query has not yet told the section of it - in a browser, a timer later.
  const cached = useQuery({ ...projectDeliverySettingsQuery(project), enabled: false }).data;
  const written = laterRecord(record, cached);
  const storedDraft = draftOf(written);
  const stored = leavesOf(storedDraft);
  // What the reader has changed, over the record as the server now holds it: a save from anywhere
  // refreshes what the reader has not touched and keeps what they have as they typed it, so a
  // line break typed to start a list's next entry stays, and the caret after it; a target box the
  // reader left shows a target another write moved, and a save leaves it out (`editedTargets`).
  // `sent` holds the changes this tab's newest Save sent, as typed, until the reader changes one
  // again; a refusal of that Save drops them all. That Save's own landing retires them: they
  // follow the record its answer is, so a list typed with a trailing line break or with commas,
  // which Save rewrites, shows as saved, and a source Add made shows as the one Dispatch stored.
  // While the Save is out, a record - another writer's - is no answer to it and keeps every
  // change as typed.
  const [changes, setChanges] = useState<Changes>(NO_CHANGES);
  const { edited, sent } = changes;
  const kept = changedLeaves(edited, stored);
  if (Object.keys(kept).length !== Object.keys(edited).length) {
    setChanges({ edited: kept, sent: sent && { ...sent, fields: stillSent(sent.fields, kept) } });
  }
  // The leaves the fields drew last: a source the reader's changed list still holds once another
  // writer's save has removed it from the record keeps its fields as they were drawn.
  const drawn = useRef<Leaves>(stored);
  const draft = draftOfLeaves({ ...drawn.current, ...stored, ...edited });
  drawn.current = leavesOf(draft);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const update = (next: Draft) => {
    const changed = changedLeaves(leavesOf(next), stored);
    setChanges({
      edited: changed,
      sent: sent && { ...sent, fields: stillSent(sent.fields, changed) },
    });
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
    let input: ProjectDeliverySettingsInput;
    if (draft.delivery) {
      const refused = refusalOf(draft, defaultRepo);
      setRefusal(refused);
      if (refused !== null) {
        onRefused();
        return;
      }
      input = draftInput(draft, storedDraft, written);
    } else {
      input = storedInput(written);
    }
    // This Save, by identity: its own landing, and no record that arrives meanwhile, retires what
    // it sent, and the landing of an earlier Save leaves a newer one's changes as typed.
    const press = {};
    const sending = onSave(input, {
      onError: () =>
        setChanges((current) =>
          current.sent?.save === press ? { ...current, sent: null } : current
        ),
      onSuccess: (answer) => {
        // A `PUT` goes under no key (`goesUnderKey`), so Dispatch keeps nothing under one for it
        // and answers each with the record it stored: a landing with no answer (`null`) is a keyed
        // write's alone, and here the type's only.
        if (answer !== null) setChanges((current) => retired(current, press));
      },
    });
    if (sending) setChanges({ edited, sent: { fields: edited, save: press } });
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
                title={defaultRepo ?? undefined}
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
                  version of the project&apos;s eval; a box left empty, or as it was read, keeps the
                  eval&apos;s target.
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
            title={source.repo === "" ? undefined : source.repo}
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
          title={source.deploy_workflow_path === "" ? undefined : source.deploy_workflow_path}
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
          title={source.pr_checks_workflow_path === "" ? undefined : source.pr_checks_workflow_path}
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
            title={
              source.production_apply_step_name === ""
                ? undefined
                : source.production_apply_step_name
            }
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
            title={source[key] === "" ? undefined : source[key]}
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
