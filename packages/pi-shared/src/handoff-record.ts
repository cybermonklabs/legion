import { execFile } from "node:child_process";
import * as path from "node:path";
import { promisify } from "node:util";
import { DISPATCH_KEY_PATTERN, type LegionRole, type LegionStepRole } from "@legion/contracts";
import { LEGION_PHASES, type LegionIssue, type LegionPhase } from "@legion/contracts/legion-api";
import { DispatchClient, DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { messageFor } from "@legion/envoy-client/errors";
import {
  type SessionConfigFile,
  sessionDispatchSettings,
} from "@legion/envoy-client/session-settings";
import { staleGitLockNote } from "./git-lock";

/**
 * What a `handoff_complete` reports beside its summary, found in the pane before anything is
 * posted: the Dispatch handoff record the phase ended with, and the commit the completion is of.
 * The daemon reads the record back from Dispatch and holds it to the issue, the phase, the version
 * and the claim's session; this module compares no session itself.
 *
 * The phase a completion is of is its step's (`StepCopy`, from the issue's record) when the
 * session's role works it, and otherwise the phase the role left (`completedPhase`), where the
 * daemon records a completion that arrives once the step moved on: the reviewer's, whose review
 * moves the issue on before it completes, is one. A file-backed phase of the role's own
 * (`FILE_BACKED`) ends with a handoff record on the issue, which the worker writes with
 * `dispatch handoff-write`; its
 * completion reports the newest record of the phase's word, `handoffId` and `handoffVersion`,
 * read from Dispatch here. No record of the phase the role works is refused naming the command
 * that writes one. A Dispatch that does not answer, or refuses the read, is refused with its
 * answer in either phase. The completion's `commit` is the head that record names: every
 * file-backed role's handoff body carries `head`, the 40-character lower-case commit the work
 * that handoff reports was done against, and the daemon takes a completion of an earlier phase
 * entry only at the head the record holds. It is read from the record and never from the
 * workspace, because a sibling role's push moves the step's branch at origin under a worker whose
 * evidence is older. A record whose body does not parse, holds no `head`, or holds one of another
 * shape reports no commit for the planner, the tester and the reviewer, whose completion still
 * goes; the daemon refuses it `HANDOFF_STALE_ENTRY` when it is of an earlier phase entry, and
 * `HANDOFF_HEAD_MERGED_FORWARD` when it is of testing or reviewing once a merge forward of the
 * issue's pull request has landed, since a completion naming no commit says nothing about a head.
 * A role of the deployment writes no record and always reports `@-` (`completedPhase`), which the
 * daemon refuses `HANDOFF_HEAD_MERGED_FORWARD` when it is a head a merge forward replaced.
 *
 * While the step stands in implementing, the phase the implementer works, its round is the
 * commit its working copy stands on, `@-`, so its record and its branch are held to that commit:
 * `@-` must be on the step's branch at origin in the pane's jj, at the branch's tip or behind it,
 * and the head the record names must be `@-` itself, which puts that head on the branch too. A record
 * naming no readable head is refused naming what to write. A workspace whose `@-` the branch does
 * not carry is refused naming the push, and saying that the record's head must name that pushed
 * commit, since a round that forgot its push wrote its record against the branch's tip at origin,
 * a head the branch carries while the round's own work is under `@-` alone. A pushed `@-` the
 * record does not name is refused naming the rewrite of the record at `@-`: a head other than the
 * commit the round's work stands on is not the head that work was done against, whether the
 * branch carries it or not. These checks hold in the phase the implementer works alone.
 *
 * The left-phase rule, which every other comment of this module and its tests points at: a role
 * whose phase the issue has left never runs a workspace check, and where it holds a record of that
 * phase never reads `@-` as its own. The issue's workspace is one checkout every role of the issue
 * shares, so once the issue has moved on, `@-` is often a sibling role's commit (the tester
 * commits there); such a completion reports its record as it stands, with the head that record
 * names, and the daemon decides it. Two shapes send the workspace's `@-` by design, and no record:
 * a left phase with no record at all, which a role on a held or an admitted issue can meet, and an
 * implementer's left record that names no readable head (one written before records carried
 * `head`). Each goes as a completion that names no record goes, with the commit the workspace
 * stands on (`standingReport`), because the daemon's completion route requires a commit on every
 * implementer and merger completion (`packages/daemon/internal/api/handoff.go`, its
 * `requireFailureFields` call for `commit`). The daemon refuses each, so neither is recorded: the
 * workflow answers a completion of a left phase that names no record
 * `409 HANDOFF_NOT_CURRENT_PHASE` where its `leftItsPhase` would not record it (the role's
 * completion of that phase already stands, or no role works the issue's phase) and
 * `409 HANDOFF_RECORD_REQUIRED` where it would. So no worker is told here to write a record the
 * daemon would not read, and the daemon never records a sibling's `@-` as the role's handoff
 * commit.
 *
 * Every other completion — retro, the merger's, the architect's, which leaves no phase — reports
 * the commit the workspace stands on, `@-`, and no record. The implementer's report on its closed
 * issue's production check resolves neither: `legion-phase-ops.ts` never asks this module for
 * one.
 *
 * The module also reads the head of the step's branch at origin that a `ready_for_update` signal
 * reports (`findPushedBranchHead`), with the same jj and the same revset the completion's push
 * check uses.
 *
 * Every one of these reads is of the step this session's agent runs (`stepCopy`): its working
 * copy, its branch and its phase. Of an issue whose record serves its tasks, that is the task the
 * agent's step is of, whose working copy sits beside the issue's own (LEGION_WORKSPACE, where the
 * agent's process started) and whose branch is its own; of an issue served no tasks, and for an
 * agent at no step of them, it is LEGION_WORKSPACE, `legion/<issue>` and the issue's phase, as
 * before tasks.
 */

/** Runs the pane's jj with `args` against the workspace at `cwd` and answers its trimmed stdout;
 * throws on a non-zero exit. */
export type JjRunner = (args: readonly string[], cwd: string) => Promise<string>;

/** The newest handoff record of a phase as `LatestHandoff` answers it (envoy-client's `Handoff`
 * carries these fields among others). `body` is the record's exact text, the JSON object the
 * phase's role wrote, which names the head its work was done against. */
export interface LatestHandoffRecord {
  readonly id: string;
  readonly version: number;
  readonly body: string;
}

/** The newest handoff record of `issue`'s phase `word` (envoy-client's
 * `DispatchClient.latestHandoff`): throws Dispatch's answer, a `404 HANDOFF_NOT_FOUND` when the
 * phase has none. */
export type LatestHandoff = (issue: string, word: string) => Promise<LatestHandoffRecord>;

/** The fields a completion adds to its request (`LegionHandoffCompleteRequest`), each present only
 * when the completion carries it. */
export interface HandoffReport {
  readonly commit?: string;
  readonly handoffId?: string;
  readonly handoffVersion?: number;
}

export interface HandoffReportInput {
  /** The step the completion is of (`stepCopy`): its working copy, its branch and its phase. */
  readonly step: StepCopy;
  /** The session's claim role: one of Legion's six or a role of the deployment. */
  readonly role: LegionStepRole;
  /** The pane's environment: `LEGION_ISSUE`, whose handoff records are read, and
   * `LEGION_WORKSPACE`, which a refusal names where the step is the issue's own. */
  readonly env: NodeJS.ProcessEnv;
  /** The pane's Dispatch (`paneLatestHandoff`). */
  readonly latestHandoff: LatestHandoff;
  /** PATH's jj by default, the one the pane's own shell runs. */
  readonly jj?: JjRunner;
}

/** The handoff a file-backed phase ends with, by the phase's word, and the role that works it:
 * the daemon's `phase.Handoff` table (each phase's word and writer), mirrored. */
const FILE_BACKED: Readonly<Partial<Record<LegionPhase, { word: string; role: LegionRole }>>> = {
  planning: { word: "plan", role: "planner" },
  implementing: { word: "implement", role: "implementer" },
  testing: { word: "test", role: "tester" },
  reviewing: { word: "review", role: "reviewer" },
};

const execFileAsync = promisify(execFile);

/** PATH's jj, run as `jj -R <workspace> …` from the workspace, so a revset names the same commits
 * from any directory. The stderr of a failure is the error, followed by a note on each stale git
 * lock it names (`staleGitLockNote`). */
export const pathJj: JjRunner = async (args, cwd) => {
  try {
    const { stdout } = await execFileAsync("jj", ["-R", cwd, ...args], {
      cwd,
      maxBuffer: 16 * 1024 * 1024,
    });
    return stdout.trim();
  } catch (error) {
    const detail =
      error !== null && typeof error === "object" && "stderr" in error
        ? String(error.stderr).trim()
        : error instanceof Error
          ? error.message
          : String(error);
    const note = await staleGitLockNote(detail);
    throw new Error(`jj ${args.join(" ")} failed: ${detail}${note === "" ? "" : `\n${note}`}`);
  }
};

/**
 * The pane's Dispatch as its `dispatch` command reaches it: `file` is the session's envoy.json (the
 * one its plugin hands its shell as `ENVOY_CONFIG_FILE`), read again on each call with `env`, so a
 * file that moved its server takes effect without a restart. A pane whose Dispatch is off throws
 * why. `session` is the session this pane acts as, which a broker trade writes under.
 */
export function paneLatestHandoff(
  file: SessionConfigFile,
  env: NodeJS.ProcessEnv,
  cwd: string,
  session: string,
  fetchImpl: typeof fetch = fetch
): LatestHandoff {
  return async (issue, word) => {
    const { dispatch } = sessionDispatchSettings(file, env, { cwd, session });
    if (!dispatch.enabled) {
      throw new Error(`Dispatch is off for this pane: ${dispatch.error ?? dispatch.notice}`);
    }
    return new DispatchClient(dispatch.url, dispatch.credential, fetchImpl).latestHandoff(
      issue,
      word
    );
  };
}

/** What a completion by `role` of `step` reports in the pane (`findHandoffReport` with the
 * pane's environment `env` and its Dispatch, `paneLatestHandoff(file, env, cwd, session)`), as each
 * harness's `legion` tool asks it. */
export function paneHandoffReport(
  file: SessionConfigFile,
  env: NodeJS.ProcessEnv,
  cwd: string,
  session: string
): (step: StepCopy, role: LegionStepRole) => Promise<HandoffReport> {
  return (step, role) =>
    findHandoffReport({
      step,
      role,
      env,
      latestHandoff: paneLatestHandoff(file, env, cwd, session),
    });
}

/** A variable the pane's jj is run with, or an error naming what the caller cannot do without it
 * (`purpose`, a clause after `so`). */
function requiredVariable(env: NodeJS.ProcessEnv, name: string, purpose: string): string {
  const value = env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} is not set, so ${purpose}`);
  }
  return value;
}

const FOR_HANDOFF = "handoff_complete cannot find what to report";

/** The issue the pane works and the branch it names, `legion/<issue>`: the key is the worker's own
 * environment, so a value carrying `#` or `/` would otherwise name another reference or path.
 * `purpose` is what the caller cannot do without the key. */
function issueBranch(env: NodeJS.ProcessEnv, purpose: string): { issue: string; bookmark: string } {
  const issue = requiredVariable(env, "LEGION_ISSUE", purpose);
  if (!DISPATCH_KEY_PATTERN.test(issue)) {
    throw new Error(
      `LEGION_ISSUE ${JSON.stringify(issue)} is not a Dispatch issue key like LEGION-1`
    );
  }
  return { issue, bookmark: `legion/${issue}` };
}

/** The working copy, the branch and the phase of the step that this session's agent runs. */
export interface StepCopy {
  /** The directory of the step's working copy. `""` only for the issue's own step where
   * LEGION_WORKSPACE is not set: each read of that working copy refuses it as it always has
   * (`requiredWorkspace`), and a completion that reads none, a record's, still goes. */
  readonly workspace: string;
  /** The step's branch, its working copy's jj bookmark: `legion/<issue>`, or a task's own. */
  readonly bookmark: string;
  /** The phase the step stands in, which a completion of it is of. */
  readonly phase: LegionPhase;
}

const FOR_STEP = "the working copy of this session's step cannot be found";

/**
 * The step that this session's agent, `agent`, runs in the issue `record` serves, as a completion
 * and an update signal read it. Of a record that serves the issue's tasks (`tasks`, contract 264)
 * it is the task whose `agent` is `agent` and whose step is at work: a running task whose step
 * waits for nothing, or a held one. Its branch is the task's, its working copy the directory the
 * daemon's `workspace.TaskDir` derives from LEGION_WORKSPACE and that branch (`taskDir`), and its
 * phase the task's, `held` for a held task, as the daemon's completion route reads a completion of
 * one (`handoffPosition`). Otherwise the step is the issue's own: LEGION_WORKSPACE as it stands,
 * `legion/<issue>` and the issue's phase. That is the answer for a record that serves no tasks — a
 * daemon below contract 264, or an issue with no plan rows — and for an agent at no step of the
 * record's tasks, whose completion the daemon then decides: of a plan of one task as before tasks,
 * and of a plan of several as a completion of no phase of it (`HANDOFF_NOT_CURRENT_PHASE`).
 *
 * A step that waits for the join of its task's needs or for its agent is no running step: nothing
 * of it was delivered (the daemon's `Issue.Running` holds delivered steps alone). So an agent with
 * a step at work and a step that waits has one step here, the one at work, whichever task comes
 * first in task id order. A stopped task is served with the agent of the act it stopped at and no
 * wait, and that agent runs no step of it (the daemon's `Issue.StepOfAgent` reads running and held
 * tasks alone). The daemon keeps at most one step at work for an agent, and the first in task id
 * order is taken.
 *
 * A step of a task with no LEGION_WORKSPACE is refused, since no rule gives its working copy; the
 * issue's own step keeps LEGION_WORKSPACE as it stands, and each read of that working copy refuses
 * a missing one as it always has (`requiredWorkspace`).
 */
export function stepCopy(
  env: NodeJS.ProcessEnv,
  record: Pick<LegionIssue, "phase" | "tasks">,
  agent: string
): StepCopy {
  const task = record.tasks?.find(
    (task) =>
      task.agent === agent &&
      (task.state === "held" || (task.state === "running" && task.wait === undefined))
  );
  // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const issueWorkspace = env["LEGION_WORKSPACE"] ?? "";
  if (task === undefined) {
    const { bookmark } = issueBranch(env, FOR_STEP);
    return { workspace: issueWorkspace, bookmark, phase: record.phase };
  }
  if (issueWorkspace === "") throw new Error(`LEGION_WORKSPACE is not set, so ${FOR_STEP}`);
  const branch = task.branch ?? "";
  return {
    workspace: taskDir(issueWorkspace, branch),
    bookmark: branch === "" ? issueBranch(env, FOR_STEP).bookmark : branch,
    phase: task.state === "held" ? "held" : stepPhase(branch, task.phase),
  };
}

/** The phase a served step stands in: a running task's `phase`, one of the phases this plugin
 * knows. A phase the daemon serves that this plugin does not know is refused by name. */
function stepPhase(branch: string, phase: string | undefined): LegionPhase {
  const known = LEGION_PHASES.find((each) => each === phase);
  if (known === undefined) {
    throw new Error(
      `the daemon serves the step on ${branch} at phase ${JSON.stringify(phase ?? "")}, which this plugin does not know, so ${FOR_STEP}`
    );
  }
  return known;
}

/** The words of a task's slug's shape that no slug may be, each for the reason the daemon's
 * `claim.notSlugs` gives: `lock` ends no git ref, `removing` is where a removed working copy is
 * renamed to, and `join` names a join branch. */
const NOT_SLUGS: readonly string[] = ["lock", "removing", "join"];

/** Whether `part`, a task's branch after `legion/<issue>.`, is a slug or a later give of one, as
 * the daemon's `claim.isTaskPart` reads it: a slug is a lower-case letter or digit, then at most
 * 39 lower-case letters, digits and hyphens, and none of `NOT_SLUGS`; a later give is a slug, a
 * dot and the give's number, 2 or more with no leading zero. */
function isTaskPart(part: string): boolean {
  const isSlug = (slug: string) =>
    /^[a-z0-9][a-z0-9-]{0,39}$/.test(slug) && !NOT_SLUGS.includes(slug);
  const later = /^([^.]+)\.([2-9]|[1-9][0-9]+)$/.exec(part);
  return isSlug(part) || (later !== null && isSlug(later[1] ?? ""));
}

/** An owner and a repository name as GitHub serves them, as the daemon's `ghrepo.Parse` reads
 * them. */
const OWNER_NAME = /^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$/;
const REPOSITORY_NAME = /^[A-Za-z0-9._-]{1,100}$/;

/**
 * The directory of the working copy of the task whose branch is `branch`, by the rule of the
 * daemon's `workspace.TaskDir`, which `packages/contracts/fixtures/daemon-api/task-dirs.json` holds
 * this function to case by case: `issueWorkspace` itself for `""` and for the issue's own branch,
 * and otherwise the directory beside it named for the branch without `legion/`, in lower case.
 * `issueWorkspace` must be an issue's working copy as the daemon lays one out,
 * `<state>/workspaces/<owner>/<repository>/<issue key in lower case>`, and `branch` a branch of
 * that issue or of a task of it, so neither names a directory outside the issue's working copies.
 */
function taskDir(issueWorkspace: string, branch: string): string {
  const repositoryDir = path.posix.dirname(issueWorkspace);
  const ownerDir = path.posix.dirname(repositoryDir);
  const workspacesDir = path.posix.dirname(ownerDir);
  const owner = path.posix.basename(ownerDir);
  const repository = path.posix.basename(repositoryDir);
  const name = path.posix.basename(issueWorkspace);
  const laidOut =
    OWNER_NAME.test(owner) &&
    REPOSITORY_NAME.test(repository) &&
    repository !== "." &&
    repository !== ".." &&
    !repository.endsWith(".git") &&
    name !== "." &&
    name !== ".." &&
    path.posix.join(
      path.posix.dirname(workspacesDir),
      "workspaces",
      owner,
      repository,
      name.toLowerCase()
    ) === issueWorkspace;
  if (!laidOut) {
    throw new Error(
      `LEGION_WORKSPACE ${JSON.stringify(issueWorkspace)} is no issue's working copy as the daemon lays one out, <state>/workspaces/<owner>/<repository>/<issue key in lower case>, so ${FOR_STEP}`
    );
  }
  if (branch === "") return issueWorkspace;
  const [key = "", ...rest] = branch.startsWith("legion/")
    ? branch.slice("legion/".length).split(".")
    : [];
  const part = rest.join(".");
  if (!DISPATCH_KEY_PATTERN.test(key) || (rest.length > 0 && !isTaskPart(part))) {
    throw new Error(
      `the daemon serves this session's step on ${JSON.stringify(branch)}, which is no branch of an issue or of a task of one, so ${FOR_STEP}`
    );
  }
  if (key.toLowerCase() !== name) {
    throw new Error(
      `the daemon serves this session's step on ${branch}, a branch of ${key}, and LEGION_WORKSPACE ${issueWorkspace} is the working copy of another issue, so ${FOR_STEP}`
    );
  }
  return path.posix.join(repositoryDir, (rest.length > 0 ? `${key}.${part}` : key).toLowerCase());
}

/** The directory of `step`'s working copy, or an error naming what the caller cannot do without
 * it (`purpose`, a clause after `so`): only the issue's own step has none, where LEGION_WORKSPACE
 * is not set (`StepCopy.workspace`). */
function requiredWorkspace(step: StepCopy, purpose: string): string {
  if (step.workspace === "") throw new Error(`LEGION_WORKSPACE is not set, so ${purpose}`);
  return step.workspace;
}

/** The revset of `<bookmark>@origin`: the branch head as the remote has it, which the
 * implementer's completion in implementing holds the commit its workspace stands on to, and which
 * `ready_for_update` reports. */
function pushedBranch(bookmark: string): string {
  return `remote_bookmarks(exact:${JSON.stringify(bookmark)}, exact:"origin")`;
}

/** The jj command that answers the commit the workspace stands on, `@-`. */
const STANDING = ["log", "-r", "@-", "--no-graph", "-T", "commit_id"] as const;

/** A commit as a handoff record names it: 40 lower-case hexadecimal characters, the shape every
 * role's prompt asks for and the one the daemon compares a completion's head against. */
export const RECORD_HEAD = /^[0-9a-f]{40}$/;

/** The head a handoff record's `body` names, the commit the work that record reports was done
 * against, or `undefined` when the body is no JSON object, carries no `head`, or carries one of
 * another shape. A body this cannot read throws nothing: what a completion whose record names no
 * readable head sends is the module doc's to say, for the role's own phase and for one it left. */
function recordHead(body: string): string | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (parsed === null || typeof parsed !== "object" || !("head" in parsed)) return undefined;
  const head = parsed.head;
  return typeof head === "string" && RECORD_HEAD.test(head) ? head : undefined;
}

/** The head of `step`'s branch at origin, as `ready_for_update` reports it: the commit the pane's
 * jj reads for that branch at the remote, and `""` when the branch is not on the remote at all — a
 * planner step that runs before the branch's first push has none, which is no error. */
export async function findPushedBranchHead(input: {
  readonly step: StepCopy;
  readonly jj?: JjRunner;
}): Promise<string> {
  const { step } = input;
  const jj = input.jj ?? pathJj;
  const workspace = requiredWorkspace(
    step,
    "ready_for_update cannot find the branch head to report"
  );
  return jj(["log", "-r", pushedBranch(step.bookmark), "--no-graph", "-T", "commit_id"], workspace);
}

/**
 * The phase a completion by `role` is of while the issue stands at `at`: `at` when `role` works
 * it, and otherwise the phase `role` left, the one the daemon records a late completion on (one
 * that arrives after the issue moved on) and reads that completion's handoff record against. It
 * mirrors the daemon's `phase.Of`, as the completion route reads it
 * (`packages/daemon/internal/phase/phase.go`,
 * `packages/daemon/internal/api/handoff.go`): the planner, the tester, the reviewer and the
 * merger each work one phase, which is the answer wherever the issue stands; the implementer
 * works implementing and retro, and has left retro once the issue stands past it in the
 * workflow's order (`phase.MovesBack(at, retro)`, which merging alone answers) and implementing
 * anywhere else; the architect leaves no phase, so its answer is the issue's; and a role of the
 * deployment works `step` (`phase.LeftBy`), which ends with no handoff record, so its completion
 * reports the commit the workspace stands on.
 */
function completedPhase(role: LegionStepRole, at: LegionPhase): LegionPhase {
  switch (role) {
    case "planner":
      return "planning";
    case "tester":
      return "testing";
    case "reviewer":
      return "reviewing";
    case "merger":
      return "merging";
    case "implementer":
      if (at === "implementing" || at === "retro") return at;
      return at === "merging" ? "retro" : "implementing";
    case "architect":
      return at;
    default:
      return "step";
  }
}

/** The report of a completion that names no record: the commit `step`'s working copy stands on,
 * `@-`. */
async function standingReport(step: StepCopy, jj: JjRunner): Promise<HandoffReport> {
  return { commit: await jj(STANDING, requiredWorkspace(step, FOR_HANDOFF)) };
}

/** What a completion by `role` of its step reports: the report of the phase the completion is of
 * (`completedPhase`), read in the step's working copy and on its branch. */
export async function findHandoffReport(input: HandoffReportInput): Promise<HandoffReport> {
  const { step, role, env } = input;
  const jj = input.jj ?? pathJj;
  const of = completedPhase(role, step.phase);
  const handoff = FILE_BACKED[of];
  if (handoff === undefined || handoff.role !== role) return standingReport(step, jj);
  // Whether the role works its step's phase; a phase it left is the module doc's left-phase rule.
  const works = of === step.phase;
  const { issue } = issueBranch(env, FOR_HANDOFF);
  const { bookmark } = step;
  let record: LatestHandoffRecord;
  try {
    record = await input.latestHandoff(issue, handoff.word);
  } catch (error) {
    if (
      error instanceof DispatchServiceError &&
      error.fromDispatch &&
      error.code === "HANDOFF_NOT_FOUND"
    ) {
      // A left phase with no record sends `@-` alone (the module doc's left-phase rule).
      if (!works) return standingReport(step, jj);
      throw new Error(
        `${issue} has no ${handoff.word} handoff in Dispatch: write it with \`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`, then complete again`
      );
    }
    const answer =
      error instanceof DispatchServiceError && error.fromDispatch
        ? `${error.status} ${error.code}: ${error.message}`
        : messageFor(error);
    throw new Error(
      `handoff_complete could not read ${issue}'s newest ${handoff.word} handoff from Dispatch: ${answer}; complete again once Dispatch answers`
    );
  }
  const report = { handoffId: record.id, handoffVersion: record.version };
  const head = recordHead(record.body);
  // A left phase's record goes as it stands, and an implementer's left record that names no
  // readable head goes as `@-` alone (the module doc's left-phase rule). Only the implementer's
  // round, while its step stands in implementing, runs the workspace checks.
  if (role !== "implementer" || !works) {
    if (head !== undefined) return { commit: head, ...report };
    if (role !== "implementer") return report;
    return standingReport(step, jj);
  }

  // In implementing, the implementer's round is the commit its working copy stands on, `@-`: the
  // step's branch at origin must carry that commit, at the branch's tip or behind it, and the
  // record must name it. A record that names no readable head is refused before the working copy
  // is read, since the record is what has to change. A refusal names the jj of the step's working
  // copy: the issue's own by the variable the pane's shell holds, where a command run in it needs
  // none, and a task's by its directory, which no variable names.
  // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const own = step.workspace === (env["LEGION_WORKSPACE"] ?? "");
  const at = own ? "" : ` -R ${JSON.stringify(step.workspace)}`;
  if (head === undefined) {
    throw new Error(
      `the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names no head: write the handoff again with \`head\` set to the 40-character lower-case commit you pushed as \`@-\`, as \`jj${own ? ' -R "$LEGION_WORKSPACE"' : at} log -r @- --no-graph -T commit_id\` prints it once the push succeeded (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`), then complete again`
    );
  }
  const workspace = requiredWorkspace(step, FOR_HANDOFF);
  const stands = await jj(STANDING, workspace);
  // The branch's tip at origin when `@-` is on it, and nothing when it is not. A round that forgot
  // its push and wrote its record against the branch's tip at origin is refused here, and told of
  // its record too: once `@-` is pushed, a record naming that older tip names a commit the round's
  // work is not.
  const tip = await jj(
    ["log", "-r", `${pushedBranch(bookmark)} & ${stands}::`, "--no-graph", "-T", "commit_id"],
    workspace
  );
  if (tip === "") {
    throw new Error(
      `the commit this workspace stands on, ${stands}, is not on ${bookmark}@origin: push it with \`jj${at} bookmark set ${bookmark} -r @- && jj${at} git push --bookmark ${bookmark}\`; the handoff's \`head\` must name that pushed commit, and the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names ${head}, so when that is another commit, write the handoff again with \`head\` set to ${stands} (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`); then complete again`
    );
  }
  // `@-` is pushed, so the record must name it: any other head, on the branch or not, is not the
  // commit this round's work stands on. Equal to `@-`, the head is on the branch as well.
  if (head !== stands) {
    throw new Error(
      `the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names ${head} as its head, not the commit this workspace stands on, ${stands}, which ${bookmark}@origin carries: write the handoff again with \`head\` set to ${stands} (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`), then complete again`
    );
  }
  return { commit: head, ...report };
}
