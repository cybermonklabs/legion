import { roleToken } from "@legion/contracts";
import type {
  LegionActivityRequestBody,
  LegionSessionActivityBody,
} from "@legion/contracts/legion-api";
import { withBound } from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";

/**
 * What a live role's session reports of what it is doing (INFRA-945), so the daemon can decide
 * whether the session's pod may be released: work of the session's own still running in the
 * background, and the phase reminder's armed wake.
 *
 * The module builds a report, compares two of them, renders the request that carries one and
 * sends that request under one of its two bounds: `reportActivityWithin` for the report a turn-end
 * handler awaits, `reportActivityBetweenTurns` for one no handler awaits. The hosts decide when to
 * send it: `packages/pi-legion/extensions/legion.ts` at `session_stop` and on a change, and
 * `packages/claude-envoy/src/legion-phase-hook.ts` at the `Stop` hook and when a timer clears the
 * reminder wake.
 * Neither host decides what the report says, or how long it waits for the daemon to take it, so
 * the two say the same thing about the same session.
 *
 * A claim with no report after its last turn is never released, so a host that reports nothing
 * holds its pod rather than losing it: every failure here is a report not sent.
 */

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

/**
 * How long a host waits for the daemon to take the activity report its turn-end handler sends:
 * 3 s, the same bound as the owed read (`OWED_READ_BOUND_MS` in `src/phase-reminder.ts`). Each
 * host's turn-end handler awaits the report, so this bound is part of the handler's own bound
 * (dispatch://INFRA-2746/spec#b-r7).
 *
 * These are every wait of the two handlers. Oh My Pi's `session_stop` handler for a phase worker
 * (`packages/pi-legion/extensions/legion.ts`) waits on `phaseReminder.settle`, whose one daemon
 * read is the owed check, bounded at `OWED_READ_BOUND_MS` (3 s); on `pi.appendEntry` from the
 * reminder's `writeEntry`, which runs in the process; and on the activity report, bounded here.
 * For an architect it waits on the activity report alone. Claude Code's `Stop` hook
 * (`packages/claude-envoy/src/legion-phase-hook.ts`) waits on `reminder.settle`, the same owed
 * check, and on `sendActivity`, bounded here. So when the daemon does not answer, each handler
 * returns within about 6 s. That is well inside the 30 s Oh My Pi gives a handler, and the 30 s
 * Claude Code gives the `Stop` hook (`packages/claude-envoy/scripts/build.ts`). A report with no
 * bound waits for as long as the daemon does: INFRA's daemon log of 2026-11-13 holds 249
 * `session_stop` handler timeouts (`handler timed out after 30000ms`), and a 250th line that
 * quotes one of them. pi-envoy's own `session_stop` handlers are another extension, with a limit
 * of their own.
 *
 * A report the bound cuts off is a report not sent, and it costs each host this. On Oh My Pi, a
 * failed report clears `lastActivity`, so the next 5 s reading (`SESSION_ACTIVITY_PERIOD_MS`)
 * sends it again, as a report between turns (`reportActivityBetweenTurns`). On Claude Code, which
 * reports only at `Stop` and when a timer clears the reminder wake, the claim stays unreported
 * until its next report, and for that time the idle release holds its pod (`IdleHoldUnreported`
 * in `packages/daemon/internal/workflow/idle.go`).
 */
export const ACTIVITY_REPORT_BOUND_MS = 3_000;

/**
 * Sends `request` through `send` and waits at most `ACTIVITY_REPORT_BOUND_MS` for the daemon to
 * take it. At the bound the signal `send` was handed is aborted, so the request does not stay
 * open, and the promise rejects with `no answer within 3000ms` (`withBound` in
 * `src/daemon-client.ts`, which also bounds the owed read). `send` is the host's
 * `LegionDaemonClient.activity`, given the signal. Both hosts send their turn-end report through
 * this, and each keeps its own handling of a rejection.
 */
export function reportActivityWithin(
  send: (request: LegionActivityRequestBody, signal: AbortSignal) => Promise<void>,
  request: LegionActivityRequestBody
): Promise<void> {
  return withBound((signal) => send(request, signal), ACTIVITY_REPORT_BOUND_MS);
}

/**
 * How long a host waits for the daemon to take an activity report it sends between turns: 60 s.
 * No handler waits for such a report, so the 3 s turn-end bound does not apply to it, and a daemon
 * slower than 3 s still takes it. A report cut off at 3 s between turns would be cut off again at
 * each later report while the daemon stays slow, so none would land.
 *
 * This bound ends only a request the daemon never answers. Oh My Pi keeps one report between turns
 * open at a time (`reportActivity` in `packages/pi-legion/extensions/legion.ts`), and a request
 * that never ends would hold that rule shut for ever. Sixty seconds is about six times the
 * slowest daemon answer measured on INFRA, its whole-state read (8.8 to 9.4 s,
 * dispatch://INFRA-2746/artifact/decisions-md).
 */
export const ACTIVITY_BETWEEN_TURNS_BOUND_MS = 60_000;

/**
 * Sends `request` through `send` and waits at most `ACTIVITY_BETWEEN_TURNS_BOUND_MS` for the
 * daemon to take it, the same way `reportActivityWithin` waits for a turn-end report: at the bound
 * the signal `send` was handed is aborted, and the promise rejects with `no answer within
 * 60000ms` (`withBound` in `src/daemon-client.ts`). Both hosts send a report no handler awaits
 * through this: Oh My Pi's 5 s reading and its report after a timer clears the reminder wake, and
 * Claude Code's report after a timer clears the reminder wake.
 */
export function reportActivityBetweenTurns(
  send: (request: LegionActivityRequestBody, signal: AbortSignal) => Promise<void>,
  request: LegionActivityRequestBody
): Promise<void> {
  return withBound((signal) => send(request, signal), ACTIVITY_BETWEEN_TURNS_BOUND_MS);
}
