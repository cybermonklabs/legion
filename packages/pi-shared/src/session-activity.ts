import { roleToken } from "@legion/contracts";
import type {
  LegionActivityRequestBody,
  LegionSessionActivityBody,
} from "@legion/contracts/legion-api";
import type { ClaimCapability } from "./legion-phase-ops";

/**
 * What a live role's session reports of what it is doing (INFRA-945), so the daemon can decide
 * whether the session's pod may be released: work of the session's own still running in the
 * background, the phase reminder's armed wake, and a session store that refuses its transcript
 * (INFRA-5660), which the daemon shows and tells the architect of.
 *
 * The module is pure. It builds a report, compares two of them and renders the request that
 * carries one; the hosts — `packages/pi-legion/extensions/legion.ts` at `session_stop` and on a
 * change, `packages/claude-envoy/src/legion-phase-hook.ts` at the `Stop` hook — are what send it.
 * Neither host decides what the report says, so the two say the same thing about the same session.
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

/** The store half of a report: the session store refusing the session's transcript, when the
 * session first read it and the store's error (`storeFailure` on `LegionSessionActivity`,
 * INFRA-5660). Absent from a report while the store takes the transcript. */
export type SessionStoreFailure = NonNullable<SessionActivityReport["storeFailure"]>;

/**
 * What `sessionStoreFailure` reads of an Oh My Pi session manager: the two members it carries
 * on the object it hands an extension, though its declared `ReadonlySessionManager` names
 * neither (`SessionManager.hasPersistenceFailure` and `getPersistenceFailure`). Declared
 * structurally here, as `RunningAsyncJobs` is, so a host without them passes as one.
 */
export interface PersistenceFailureReader {
  readonly hasPersistenceFailure?: () => boolean;
  readonly getPersistenceFailure?: () => Error | undefined;
}

/**
 * The failure the session store holds against this session now, or undefined while it takes the
 * transcript. `previous` is the last reading: a failure that still stands keeps the start it was
 * first read at, and one read after a reading of none starts at `now`. A session manager without
 * the two members, or whose member throws, reads as none, so a host that cannot say reports
 * nothing and throws nothing. Each member is called on the manager, as the methods they are.
 */
export function sessionStoreFailure(
  manager: PersistenceFailureReader,
  previous: SessionStoreFailure | undefined,
  now: Date = new Date()
): SessionStoreFailure | undefined {
  if (
    typeof manager.hasPersistenceFailure !== "function" ||
    typeof manager.getPersistenceFailure !== "function"
  ) {
    return undefined;
  }
  let error: Error | undefined;
  try {
    if (!manager.hasPersistenceFailure()) return undefined;
    error = manager.getPersistenceFailure();
  } catch {
    return undefined;
  }
  const message = error?.message.trim() ?? "";
  return {
    since: previous?.since ?? now.toISOString(),
    error: message === "" ? "the session store refused a write and named no error" : message,
  };
}

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
 * One report: the kinds of background work found, the phase reminder's wake, the moment the
 * session read them, and the store's failure while it refuses the transcript. The kinds are
 * sorted and de-duplicated here, so two readings of the same work compare equal and the reason
 * the daemon records for a hold reads the same every time.
 */
export function sessionActivity(
  kinds: readonly string[],
  phase: SessionActivityPhase,
  at: Date = new Date(),
  storeFailure?: SessionStoreFailure
): SessionActivityReport {
  const found = [...new Set(kinds)].sort();
  return {
    at: at.toISOString(),
    backgroundWork: { running: found.length > 0, kinds: found },
    phase,
    ...(storeFailure === undefined ? {} : { storeFailure }),
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
    one.phase.nextReminderAt === other.phase.nextReminderAt &&
    sameStoreFailure(one.storeFailure, other.storeFailure)
  );
}

/** Whether two readings of the store say the same: both none, or one failure with one start
 * and one error. */
function sameStoreFailure(
  one: SessionStoreFailure | undefined,
  other: SessionStoreFailure | undefined
): boolean {
  if (one === undefined || other === undefined) return one === other;
  return one.since === other.since && one.error === other.error;
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
