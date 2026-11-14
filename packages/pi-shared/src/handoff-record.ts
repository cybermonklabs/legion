import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { DISPATCH_KEY_PATTERN, type LegionRole } from "@legion/contracts";
import type { LegionPhase } from "@legion/contracts/legion-api";
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
 * The phase a completion is of is the issue's (from its record) when the session's role works
 * it, and otherwise the phase the role left (`completedPhase`), where the daemon records a
 * completion that arrives once the issue moved on: the reviewer's, whose review moves the issue
 * on before it completes, is one. A file-backed phase of the role's own (`FILE_BACKED`) ends with
 * a handoff record on the issue, which the worker writes with `dispatch handoff-write`; its
 * completion reports the newest record of the phase's word, `handoffId` and `handoffVersion`,
 * read from Dispatch here. No record is refused naming the command that writes one; a Dispatch
 * that does not answer, or refuses the read, is refused with its answer. The completion's
 * `commit` is the head that record names: every file-backed role's handoff body carries `head`,
 * the 40-character lower-case commit the work that handoff reports was done against, and the
 * daemon takes a completion of an earlier phase entry only at the head the record holds. It is
 * read from the record and never from the workspace, because a sibling role's push moves
 * `legion/<issue>@origin` under a worker whose evidence is older. A record
 * whose body does not parse, holds no `head`, or holds one of another shape reports no commit for
 * the planner, the tester and the reviewer, whose completion still goes; the daemon refuses it
 * `HANDOFF_STALE_ENTRY` when it is of an earlier phase entry, since a completion naming no commit
 * says nothing about a head. The implementer's implementing round is the commit its workspace
 * stands on, `@-`, so its record and its branch are held to that commit: `@-` must be on
 * `legion/<issue>@origin` in the pane's jj, at the branch's tip or behind it, and the head the
 * record names must be `@-` itself, which puts that head on the branch too. A record naming no
 * readable head is refused naming what to write. A workspace whose `@-` the branch does not carry
 * is refused naming the push, and saying that the record's head must name that pushed commit,
 * since a round that forgot its push wrote its record against the branch's tip at origin, a head
 * the branch carries while the round's own work is under `@-` alone. A pushed `@-` the record does
 * not name is refused naming the rewrite of the record at `@-`: a head other than the commit the
 * round's work stands on is not the head that work was done against, whether the branch carries
 * it or not. Every other completion — retro, the merger's, the architect's, which leaves no
 * phase — reports the commit the workspace stands on, `@-`, and no record. The implementer's
 * report on its closed issue's production check resolves neither: `legion-phase-ops.ts` never
 * asks this module for one.
 *
 * The module also reads the head of `legion/<issue>@origin` that a `ready_for_update` signal
 * reports (`findPushedBranchHead`), with the same jj and the same revset the completion's push
 * check uses.
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
  /** The issue's phase, from its record (`read_record`'s `phase`). */
  readonly phase: LegionPhase;
  /** The session's claim role. */
  readonly role: LegionRole;
  /** The pane's environment: `LEGION_WORKSPACE` and `LEGION_ISSUE`. */
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

/** What a completion by `role` reports in the pane while the issue stands at `phase`
 * (`findHandoffReport` with the pane's environment `env` and its Dispatch,
 * `paneLatestHandoff(file, env, cwd, session)`), as each harness's `legion` tool asks it. */
export function paneHandoffReport(
  file: SessionConfigFile,
  env: NodeJS.ProcessEnv,
  cwd: string,
  session: string
): (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport> {
  return (phase, role) =>
    findHandoffReport({
      phase,
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

/** The revset of `<bookmark>@origin`: the branch head as the remote has it, which the
 * implementer's completion holds the commit its workspace stands on to, and which
 * `ready_for_update` reports. */
function pushedBranch(bookmark: string): string {
  return `remote_bookmarks(exact:${JSON.stringify(bookmark)}, exact:"origin")`;
}

/** The jj command that answers the commit the workspace stands on, `@-`. */
const STANDING = ["log", "-r", "@-", "--no-graph", "-T", "commit_id"] as const;

/** A commit as a handoff record names it: 40 lower-case hexadecimal characters, the shape every
 * role's prompt asks for and the one the daemon compares a completion's head against. */
const RECORD_HEAD = /^[0-9a-f]{40}$/;

/** The head a handoff record's `body` names, the commit the work that record reports was done
 * against, or `undefined` when the body is no JSON object, carries no `head`, or carries one of
 * another shape. A body this cannot read throws nothing: the planner, the tester and the reviewer
 * complete without a commit, and the daemon refuses such a completion `HANDOFF_STALE_ENTRY` when
 * it is of an earlier phase entry, since it says nothing about a head. */
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

/** The head of `legion/<issue>@origin`, as `ready_for_update` reports it: the commit the pane's
 * jj reads for the issue branch at the remote, and `""` when the branch is not on the remote at
 * all — a planner before its first push has none, which is no error. */
export async function findPushedBranchHead(input: {
  readonly env: NodeJS.ProcessEnv;
  readonly jj?: JjRunner;
}): Promise<string> {
  const { env } = input;
  const jj = input.jj ?? pathJj;
  const purpose = "ready_for_update cannot find the branch head to report";
  const workspace = requiredVariable(env, "LEGION_WORKSPACE", purpose);
  const { bookmark } = issueBranch(env, purpose);
  return jj(["log", "-r", pushedBranch(bookmark), "--no-graph", "-T", "commit_id"], workspace);
}

/**
 * The phase a completion by `role` is of while the issue stands at `at`: `at` when `role` works
 * it, and otherwise the phase `role` left, the one the daemon records a late completion on (one
 * that arrives after the issue moved on) and reads that completion's handoff record against. It
 * mirrors the daemon's `phase.LeftBy` behind its `phase.RoleFor` check, as the completion route
 * reads the two (`packages/daemon/internal/phase/phase.go`,
 * `packages/daemon/internal/api/handoff.go`): the planner, the tester, the reviewer and the
 * merger each work one phase, which is the answer wherever the issue stands; the implementer
 * works implementing and retro, and has left retro once the issue stands past it in the
 * workflow's order (`phase.MovesBack(at, retro)`, which merging alone answers) and implementing
 * anywhere else; the architect leaves no phase, so its answer is the issue's.
 */
function completedPhase(role: LegionRole, at: LegionPhase): LegionPhase {
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
  }
}

/** What a completion by `role` reports while the issue stands at `phase`: the report of the phase
 * the completion is of (`completedPhase`). */
export async function findHandoffReport(input: HandoffReportInput): Promise<HandoffReport> {
  const { role, env } = input;
  const jj = input.jj ?? pathJj;
  const handoff = FILE_BACKED[completedPhase(role, input.phase)];
  if (handoff === undefined || handoff.role !== role) {
    return { commit: await jj(STANDING, requiredVariable(env, "LEGION_WORKSPACE", FOR_HANDOFF)) };
  }
  const { issue, bookmark } = issueBranch(env, FOR_HANDOFF);
  let record: LatestHandoffRecord;
  try {
    record = await input.latestHandoff(issue, handoff.word);
  } catch (error) {
    if (
      error instanceof DispatchServiceError &&
      error.fromDispatch &&
      error.code === "HANDOFF_NOT_FOUND"
    ) {
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
  if (role !== "implementer") return head === undefined ? report : { commit: head, ...report };

  // The implementer's round is the commit its workspace stands on, `@-`: `legion/<issue>@origin`
  // must carry that commit, at the branch's tip or behind it, and the record must name it. A
  // record that names no readable head is refused before the workspace is read, since the record
  // is what has to change.
  if (head === undefined) {
    throw new Error(
      `the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names no head: write the handoff again with \`head\` set to the 40-character lower-case commit you pushed as \`@-\`, as \`jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T commit_id\` prints it once the push succeeded (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`), then complete again`
    );
  }
  const workspace = requiredVariable(env, "LEGION_WORKSPACE", FOR_HANDOFF);
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
      `the commit this workspace stands on, ${stands}, is not on ${bookmark}@origin: push it with \`jj bookmark set ${bookmark} -r @- && jj git push --bookmark ${bookmark}\`; the handoff's \`head\` must name that pushed commit, and the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names ${head}, so when that is another commit, write the handoff again with \`head\` set to ${stands} (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`); then complete again`
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
