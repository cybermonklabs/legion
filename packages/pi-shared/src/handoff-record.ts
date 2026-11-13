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
 * A file-backed phase the session's role works (`FILE_BACKED`: the phase from the issue record,
 * the role from the session) ends with a handoff record on the issue, which the worker writes with
 * `dispatch handoff-write`; its completion reports the newest record of the phase's word,
 * `handoffId` and `handoffVersion`, read from Dispatch here. No record is refused naming the
 * command that writes one; a Dispatch that does not answer, or refuses the read, is refused with
 * its answer. The completion's `commit` is the head that record names: every file-backed role's
 * handoff body carries `head`, the 40-character lower-case commit the work that handoff reports
 * was done against, and the daemon takes a completion of an earlier phase entry only at the head
 * the record holds. It is read from the record and never from the workspace, because a sibling
 * role's push moves `legion/<issue>@origin` under a worker whose evidence is older. A record
 * whose body does not parse, holds no `head`, or holds one of another shape reports no commit for
 * the planner, the tester and the reviewer, whose completion still goes; the daemon refuses it
 * `HANDOFF_STALE_ENTRY` when it is of an earlier phase entry, since a completion naming no commit
 * says nothing about a head. The implementer's implementing round holds that head to its branch:
 * the head its record names must be on `legion/<issue>@origin` in the pane's jj, at the branch's
 * tip or behind it. A record naming no readable head is refused naming what to write, and one
 * naming a head the branch does not carry is refused naming both ways out: the push, when that
 * head is new work of the worker's own, and otherwise a record naming the head the branch
 * carries. Every other completion — retro, the merger's, a role reporting a phase it does not run
 * (which the daemon refuses naming whose phase it is) — reports the commit the workspace stands
 * on, `@-`, and no record. The implementer's report on its closed issue's production check
 * resolves neither: `legion-phase-ops.ts` never asks this module for one.
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

/** What a completion of `phase` by `role` reports in the pane (`findHandoffReport` with the pane's
 * environment `env` and its Dispatch, `paneLatestHandoff(file, env, cwd, session)`), as each
 * harness's `legion` tool asks it. */
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

/** The revset of `<bookmark>@origin`: the branch head as the remote has it, which is what the
 * implementer's completion holds its round to and what `ready_for_update` reports. */
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

/** What a completion of `phase` by `role` reports. */
export async function findHandoffReport(input: HandoffReportInput): Promise<HandoffReport> {
  const { phase, role, env } = input;
  const jj = input.jj ?? pathJj;
  const handoff = FILE_BACKED[phase];
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

  // The implementer's round is of the head its record names, and all that is held of that head
  // here is that `legion/<issue>@origin` carries it, at the branch's tip or behind it. A head the
  // branch does not carry is work not pushed, or a record naming a commit that is not the round's.
  if (head === undefined) {
    throw new Error(
      `the newest ${handoff.word} handoff of ${issue} (version ${record.version}) names no head: write the handoff again with \`head\` set to the 40-character lower-case commit you pushed as ${bookmark}@origin's tip (\`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`), then complete again`
    );
  }
  const workspace = requiredVariable(env, "LEGION_WORKSPACE", FOR_HANDOFF);
  // The branch's tip at origin when the record's head is on it, and nothing when it is not;
  // `present` so a head this repository does not know at all is that same refusal rather than a
  // jj error about a revision.
  const carried = await jj(
    [
      "log",
      "-r",
      `${pushedBranch(bookmark)} & present(${head})::`,
      "--no-graph",
      "-T",
      "commit_id",
    ],
    workspace
  );
  if (carried === "") {
    throw new Error(
      `the head the newest ${handoff.word} handoff of ${issue} names, ${head}, is not on ${bookmark}@origin: when it is new work of yours, push it with \`jj bookmark set ${bookmark} -r ${head} && jj git push --bookmark ${bookmark}\`; otherwise write the handoff again with \`head\` set to the commit ${bookmark}@origin carries (\`jj log -r ${bookmark}@origin --no-graph -T commit_id\` prints it) and \`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`; then complete again`
    );
  }
  return { commit: head, ...report };
}
