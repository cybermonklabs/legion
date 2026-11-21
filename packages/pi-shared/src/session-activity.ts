import { roleToken } from "@legion/contracts";
import type {
  LegionActivityRequestBody,
  LegionSessionActivityBody,
} from "@legion/contracts/legion-api";
import type { ClaimCapability } from "./legion-phase-ops";
import { integerSetting, type StallLimitsLog } from "./stall-limits";

/**
 * What a live role's session reports of what it is doing (INFRA-945), so the daemon can decide
 * whether the session's pod may be released: work of the session's own still running in the
 * background, and the phase reminder's armed wake.
 *
 * The module is pure. It builds a report, compares two of them and renders the request that
 * carries one; the hosts — `packages/pi-legion/extensions/legion.ts` at `session_stop`, on a
 * change, and every {@link TURN_ACTIVITY_HEARTBEAT_MS} while a turn is open,
 * `packages/claude-envoy/src/legion-phase-hook.ts` at the `Stop` hook — are what send it.
 * Neither host decides what the report says, so the two say the same thing about the same session.
 *
 * A claim with no report after its last turn is never released, so a host that reports nothing
 * holds its pod rather than losing it: every failure here is a report not sent.
 */

/**
 * How often an Oh My Pi session sends its report again while a turn is open, whatever it says:
 * 5 minutes. The daemon's relaunch guard (R5, `RelaunchGuard` in
 * `packages/daemon/internal/supervise/reach.go`) reads a report made in the turn in the last 20
 * minutes as the process still running that turn, since a `wait` on a named `bash` service blocks
 * with no step on the pinned Oh My Pi and no report names the service. A quarter of those 20
 * minutes leaves three reports that can be lost before the guard reads a live turn as a quiet one.
 * The report goes from a timer of the process's own, so a process that is frozen or whose event
 * loop is held sends none, and the guard lets it through 20 minutes after its last.
 */
export const TURN_ACTIVITY_HEARTBEAT_MS = 300_000;

/** The one variable that moves {@link TURN_ACTIVITY_HEARTBEAT_MS}, which a test on the real binary
 * shortens so a turn's reports come in seconds. No production path sets it, and the daemon copies
 * it to no session. */
const TURN_ACTIVITY_HEARTBEAT_VARIABLE = "LEGION_TURN_ACTIVITY_HEARTBEAT_MS";

/** The period this session's turn heartbeat runs on, read from its own environment: the default
 * when the variable is unset, and the default with one `warn` naming it when it is not an
 * integer of 1 or more (`integerSetting`). */
export function turnActivityHeartbeatMs(
  env: Record<string, string | undefined>,
  log: StallLimitsLog
): number {
  return integerSetting(env, log, TURN_ACTIVITY_HEARTBEAT_VARIABLE, TURN_ACTIVITY_HEARTBEAT_MS, 1);
}

/** A backgrounded shell command, or anything else the host's async-job manager runs that is not a
 * subagent (`AsyncJob.type` on Oh My Pi: `bash`, `eval`). */
export const ASYNC_JOB_KIND = "async-job";

/** A `task` subagent of the session, which the host runs as an async job of type `task`. */
export const SUBAGENT_KIND = "subagent";

/** A Claude Code background task, the one kind that host reports (`${background_tasks}` in its
 * `Stop` hook). */
export const CLAUDE_BACKGROUND_TASK_KIND = "claude-background-task";

/**
 * One reading of what a session is doing, which is the body of the `claims/activity` request and
 * is declared once, there (`LegionSessionActivity` in `@legion/contracts/legion-api`): `at`, when
 * the session read it, which is why it is no part of what the report says
 * (`sameSessionActivity`); `backgroundWork`, whether work of the session's own was running and
 * of what kinds; and `phase`.
 */
export type SessionActivityReport = LegionSessionActivityBody;

/** The phase half of a report: when the reminder armed for the session's phase is due (ISO
 * 8601), null when no armed reminder is still live, which a session whose phase has closed
 * always reports. The phase reminder states it (`PhaseReminder.phaseWake` in
 * `src/phase-reminder.ts`); nothing recomputes it from the limits, and whether the phase is open
 * is the daemon's own reading of its record, not the session's to state. */
export type SessionActivityPhase = SessionActivityReport["phase"];

/**
 * What `asyncJobKinds` reads of a host's async-job snapshot: the jobs running now, and what
 * registered each. Oh My Pi's `AsyncJobSnapshot` (`src/pi-types.ts`,
 * `SessionContext.getAsyncJobSnapshot`) is one. It is declared structurally here so this module
 * stays clear of the Oh My Pi host surface, which a Claude Code pane does not build against.
 */
export interface RunningAsyncJobs {
  readonly running: readonly { readonly type: string }[];
}

/**
 * The kinds of background work an Oh My Pi session is running now, from
 * `SessionContext.getAsyncJobSnapshot()`. A `task` job is a subagent and everything else the
 * manager runs — a backgrounded `bash` command, an eval — is an async job; a host that answers
 * null, or one that has no such member at all, is running none that this pane can see. Repeats
 * are left in: `sessionActivity` is where a report's kinds are made canonical.
 */
export function asyncJobKinds(snapshot: RunningAsyncJobs | null | undefined): string[] {
  if (snapshot === null || snapshot === undefined) return [];
  return snapshot.running.map((job) => (job.type === "task" ? SUBAGENT_KIND : ASYNC_JOB_KIND));
}

/**
 * One report: the kinds of background work found, the phase reminder's wake, and the moment the
 * session read them. The kinds are sorted and de-duplicated here, so two readings of the same
 * work compare equal and the reason the daemon records for a hold reads the same every time.
 */
export function sessionActivity(
  kinds: readonly string[],
  phase: SessionActivityPhase,
  at: Date = new Date()
): SessionActivityReport {
  const found = [...new Set(kinds)].sort();
  return {
    at: at.toISOString(),
    backgroundWork: { running: found.length > 0, kinds: found },
    phase,
  };
}

/**
 * Whether two reports say the same thing, which is what a host sending only on a change compares.
 * `at` is left out: it is when the session read the report, not part of what the report says, and
 * every reading would otherwise differ.
 */
export function sameSessionActivity(
  one: SessionActivityReport,
  other: SessionActivityReport
): boolean {
  return (
    one.backgroundWork.running === other.backgroundWork.running &&
    one.backgroundWork.kinds.length === other.backgroundWork.kinds.length &&
    one.backgroundWork.kinds.every((kind, index) => kind === other.backgroundWork.kinds[index]) &&
    one.phase.nextReminderAt === other.phase.nextReminderAt
  );
}

/**
 * The body of `POST /legion/v1/claims/activity` for this session's report, or undefined when the
 * claim token cannot be spelled — no project on the pane's environment, or an issue key the token
 * rule refuses — which is a report not sent rather than a throw inside a host's hook.
 *
 * The claim token is the role token of the claim's project, issue and role, which is how the
 * daemon minted it (`claim.NewToken`) and how the phase reminder already spells the architect's
 * (`tellArchitect` in `src/phase-reminder.ts`). An architect reports as a phase worker does
 * (pi-legion), since the daemon releases a pod only when each of its live roles has reported;
 * claude-envoy runs phase workers alone, so its reports are theirs.
 */
export function sessionActivityRequest(input: {
  readonly project: string | undefined;
  readonly claim: ClaimCapability;
  readonly report: SessionActivityReport;
}): LegionActivityRequestBody | undefined {
  const { claim, project } = input;
  if (project === undefined || project === "") return undefined;
  let claimToken: string;
  try {
    claimToken = roleToken(project, claim.issue, claim.role);
  } catch {
    return undefined;
  }
  return {
    claimToken,
    sessionId: claim.sessionID,
    secret: claim.secret,
    activity: input.report,
  };
}
