import type { LegionIssue } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { EnvoyApiError, type PublishResult } from "@legion/envoy-client/transport";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import {
  architectLive,
  type EnvoyPublish,
  RECORD_READ_BOUND_MS,
  withBound,
} from "@legion/pi-shared/phase-reminder";
import { DEFAULT_STALL_LIMITS } from "@legion/pi-shared/stall-limits";
import type {
  StallCall,
  StallEvidence,
  StallTold,
  StallWatchdogLog,
  StallWatchdogTeller,
} from "@legion/pi-shared/stall-watchdog";
import type { LegionSessionKind } from "./classify";
import {
  STALL_WATCH_ENTRY,
  type StallNotice,
  type StallWatchRecord,
  stallEvidenceRecord,
  stallNoticeMessage,
  stallNoticeTopic,
} from "./stall-evidence";

/**
 * The notice delivery of the stall watch: the detector's teller (`createStallTeller`), which writes
 * each stuck call's evidence to the session's transcript and sends its owner one notice, and sends
 * that notice again while a publish no rule decides fails. What the evidence and the notice say,
 * and who is told, is `./stall-evidence.ts`'s: this module is the half that talks to the listener
 * and the daemon and keeps the re-sends' state.
 */

/** The teller's log: the detector's two levels, and `info` for the news that needs no one to act:
 * a notice that told its owner only on a re-send after a failure that was not a refusal for this
 * session's send rate, and a notice not sent again because what it says is no longer true. A
 * notice told only on a re-send after a refusal for the send rate is a `warn`, as the refusals
 * before it are. */
export interface StallTellerLog extends StallWatchdogLog {
  readonly info: (message: string, context?: Record<string, unknown>) => void;
}

/** The wait before a notice whose publish failed is first sent again, and the longest any wait
 * grows to: each failed attempt doubles the wait before the next, so a listener back within a
 * second is asked again a second later, and one that stays down is asked once a minute until
 * `RESEND_CAP_MS` ends the re-sends. A refusal for this session's send rate waits the seconds it
 * names instead (`rateLimitedSeconds`). */
const RESEND_FIRST_WAIT_MS = 1_000;
const RESEND_LONGEST_WAIT_MS = 60_000;

/** The wait after failed attempt `attempt` (1 is the first publish) before the next one. */
function resendWaitMs(attempt: number): number {
  return Math.min(RESEND_FIRST_WAIT_MS * 2 ** (attempt - 1), RESEND_LONGEST_WAIT_MS);
}

/** How many of the listener's refusals for this session's send rate one notice takes: the fourth
 * ends its re-sends, so a notice the listener refuses for nothing but the send rate is sent four
 * times at most, the first publish and three re-sends, each after the wait its refusal named. */
const STALL_NOTICE_SENDS = 4;

/**
 * The longest a notice is sent again for, counted from its first attempt: two of the ladder's step
 * gaps at their default (`DEFAULT_STALL_LIMITS.stepMs`), 4 minutes, which is how long the ladder
 * takes from the overdue notice to its last step. By then the ladder has decided the call: it
 * returned, which ends the overdue notice's re-sends at once (`createStallTeller`); it ended this
 * process, which ends them with it; or it stopped short, and the stop's own notice supersedes the
 * overdue one. An overdue notice still not stored by then would reach its owner behind the news
 * that overtook it. A stop's notice is bounded by this alone: what it says, that the ladder
 * stopped short for this call, stays true once the call ends — which the ladder's own abort
 * usually brings about — so its call ending does not end its re-sends, and this bound does. Both
 * end long before a notice is stale for whoever reads it: the listener itself holds a role
 * message for its next live holder for an hour, and a listener that stays down is asked at most
 * nine times in these four minutes (the waits of `resendWaitMs`). It is the default's number
 * rather than this worker's own `LEGION_STALL_STEP_MS`, which a test sets to 0, and a bound of 0
 * would leave no re-send at all.
 */
const RESEND_CAP_MS = 2 * DEFAULT_STALL_LIMITS.stepMs;

/** The re-send's wait in production: a timer that holds no process open. The re-sends go on for
 * `RESEND_CAP_MS` at most, and a process with nothing else left to do is ending, which takes this
 * process's copy of the notice with it whatever it waited for. */
function unheldSleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms).unref();
  return promise;
}

/** The two reasons an overdue notice still being sent again stops saying what is true, each logged
 * once at `info` as the reason it is not sent again. A stop's notice has neither. */
const CALL_ENDED = "legion: the stall notice is not sent again, because its tool call has ended";
const STOP_SUPERSEDES =
  "legion: the stall notice is not sent again, because the ladder's stop notice supersedes it";

/**
 * The whole seconds the listener's refusal for this session's send rate names, or undefined for
 * any other failure. That refusal is the 429 `sender_rate_limited` (`refuseSenderRateLimited` in
 * `packages/envoy/cmd/listener/request_record.go`): this session sent past its send limit, the
 * listener stored nothing, and its body's `retry_after_seconds` names when it takes the next
 * message. The listener's other 429, for too many access tokens checked from one address
 * (`refuseBrokerToken` in `packages/envoy/cmd/listener/apiauth.go`), carries no such reason.
 */
function rateLimitedSeconds(error: unknown): number | undefined {
  if (!(error instanceof EnvoyApiError)) return undefined;
  if (error.details.status !== 429 || error.reason !== "sender_rate_limited") return undefined;
  return error.retryAfterSeconds;
}

/**
 * Whether a publish's failure is decided, so that sending the same notice again gets the same
 * answer: a 4xx is the listener's own refusal of it — 400 or 413 for the request, 403 for the
 * capability or the grant, 404 for a role with no live holder whose keep kept nothing under a rule
 * (`roleSend.noHolder` in `packages/envoy/cmd/listener/confirmed.go`; a keep that failed on NATS —
 * a full stream, a publish not answered, a connection down — is answered the bus-unavailable 503
 * instead, and is sent again), 409 for a message the stream refuses however often it is sent
 * (`streamRefusedPrefix` in `packages/envoy/cmd/listener/api.go`), and every 429 but the refusal
 * for this session's send rate that names its wait (`rateLimitedSeconds`): a 429 with another
 * reason, or with none, as the broker token check's is, is not sent again. 408 is the one 4xx
 * answer that says "later" rather than "no", so it is not decided, and nor is the refusal for the
 * send rate. Nor is anything else: a 5xx, 504 `delivery_failed` and 503 among them, a timeout, a
 * connection that failed, a 2xx whose body did not parse, a credential that could not be minted.
 * The status is read from the Envoy client's `EnvoyApiError`, which is what its `publish` throws
 * for every answer that is not a 2xx, never from an error's text: anything else thrown is no
 * answer of the listener's at all.
 */
function refusedForGood(error: unknown): boolean {
  if (!(error instanceof EnvoyApiError)) return false;
  const { status } = error.details;
  if (status === 429) return rateLimitedSeconds(error) === undefined;
  return status >= 400 && status < 500 && status !== 408;
}

/** An attempt that failed for a reason no rule decides, with its error: the listener's refusal
 * for this session's send rate, with the seconds it named, or any other. */
type NoticeFailure =
  | { readonly kind: "rate-limited"; readonly retryAfterSeconds: number; readonly error: string }
  | { readonly kind: "failed"; readonly error: string };

/** What one attempt of a notice came to: the listener `stored` it or `kept` it, `refused` it for
 * good (`refusedForGood`), or the attempt failed for a reason no rule decides. */
type NoticeAttempt =
  | { readonly kind: "stored" }
  | { readonly kind: "kept" }
  | { readonly kind: "refused" }
  | NoticeFailure;

/** One publish of a notice: the same topic, message, payload and idempotency key every attempt,
 * answered with the part of the listener's answer this teller reads. */
type NoticeSend = () => Promise<Pick<PublishResult, "kept" | "keptUntil">>;

/** What the first attempt of a notice achieved. */
interface NoticeSent {
  readonly ownerTold: boolean;
  /**
   * Set when the first attempt failed for a reason no rule decides: starts the detached re-sends,
   * which run `onTold` when a re-send tells the owner. The caller starts them once it has recorded
   * the first answer, so a re-send that tells the owner finds that record to correct.
   */
  readonly resend: ((onTold?: () => void) => void) | undefined;
}

const NOTICE_UNTOLD: NoticeSent = { ownerTold: false, resend: undefined };

export interface StallTellerDeps {
  readonly now: () => number;
  /** This session's Legion identity (`classifySession`), which decides who is told. */
  readonly session: () => LegionSessionKind;
  /** This session's claim, or undefined before it has registered one. */
  readonly claim: () => ClaimCapability | undefined;
  /** `LEGION_PROJECT`. */
  readonly project: string | undefined;
  /** One issue's record, read as `read_record` reads it (`LegionDaemonClient.record`): the
   * record, or `undefined` for an issue the daemon records nothing of. `signal` is aborted once
   * the record check's bound passes without an answer. */
  readonly readRecord: (issue: string, signal: AbortSignal) => Promise<LegionIssue | undefined>;
  /** The Envoy listener the notice goes to, or undefined when Envoy is off for this session.
   * Built lazily and once by the caller, as the phase reminder's is. */
  readonly publisher: () => EnvoyPublish | undefined;
  /** Writes one record to the session's transcript (`pi.appendEntry`). */
  readonly appendEntry: (customType: string, data: unknown) => void;
  /** The calls the detector holds open now (`StallWatchdog.openCalls`), read before each re-send
   * of an overdue notice: a call the detector has closed — it returned, or its turn or its session
   * ended — is no longer stuck, so its overdue notice is not sent again. A stop's notice does not
   * read it (`RESEND_CAP_MS`). */
  readonly openCalls: () => readonly StallCall[];
  readonly log: StallTellerLog;
  /** The bound on one daemon read; `RECORD_READ_BOUND_MS` when it is left out. */
  readonly recordBoundMs?: number;
  /** Waits `ms` before a notice whose publish failed is sent again; `unheldSleep` when it is left
   * out. A test hands a wait it ends itself, or one that ends at once, so it runs every re-send
   * without a clock. */
  readonly sleep?: (ms: number) => Promise<void>;
}

/**
 * The detector's teller: the evidence into the session's transcript, and one notice to the owner
 * per stuck call.
 *
 * **One message per stuck call.** The detector tells a call's owner once, before the first step,
 * and a sweep that finds the same call still open carries it a step further without telling
 * anyone again; this teller holds that answer by call id as well, so a second telling of the same
 * call publishes nothing whatever reaches it, and starts no second run of the re-sends below.
 *
 * **A publish no rule decides is sent again while the notice is still true.** A 5xx, a timeout, a
 * connection that failed, or the listener's refusal for this session's send rate (`refusedForGood`
 * names what is decided) leaves the owner untold, and nothing that reads this teller's answer will
 * send the notice later: the ladder goes on whatever it is told. So the teller sends the same
 * notice again — the same topic, message, payload and idempotency key — until an attempt is stored
 * or kept or the listener refuses it for good. The wait before each re-send is the listener's own
 * where it names one: its 429 `sender_rate_limited` says that this session sent past its send
 * limit, that nothing was stored, and the whole seconds until it takes the next message, and the
 * teller waits exactly that long (`rateLimitedSeconds`). After any other failure it waits a
 * second, then twice the last wait, up to a minute (`resendWaitMs`). Each failed attempt leaves one
 * warning that names the wait before the next. The one key keeps that one message however many
 * attempts reach the listener. The re-sends run detached: the detector awaits the teller before
 * the step that frees the worker, and runs every overdue call in turn, so a wait here would hold
 * that step, and every other stuck call's, for as long as the waits run. The ladder's answer is the
 * first attempt's, `ownerTold` false. A re-send that tells the owner logs that it did — a warning
 * after a refusal for the send rate, as the refusals were, and `info` after any other failure — and
 * corrects the teller's record of the call, so a later telling of it answers `ownerTold` true: the
 * re-sends start only once that first answer is recorded. They live in this process, so the
 * ladder's last step, which ends the process, ends them too.
 *
 * **A notice that is no longer true is not sent again, and none is sent again for ever.** Before
 * each re-send of an overdue notice the teller reads the detector's open calls (`openCalls`): a
 * call that returned, or whose turn or session ended, is no longer stuck, so its overdue notice is
 * not sent again. Nor is it once the ladder has stopped short for its call: the stop's notice is
 * the newer news about it. Either is logged once, at `info`, with the notice's key. No attempt of
 * any notice starts past `RESEND_CAP_MS` from the first: the failure that would have waited past
 * it is the last, and so is a wait that ended past it, which a paused process or a clock that
 * moved can make; either is logged with one warning that names the notice and its attempts. And
 * the listener's fourth refusal of a notice for this session's send rate is its last
 * (`STALL_NOTICE_SENDS`), logged with one warning that the notice was given up. These bounds hold
 * whatever the failures before them were.
 *
 * **What a rule decides is not sent again.** A publish the listener refuses (a 4xx, but for 408
 * and the refusal for the send rate), a publish it keeps for the owner's role while no live
 * session holds it — kept, so not lost — and a session with Envoy off or with nobody to tell, each
 * leave a line in the log and `ownerTold` false: the ladder goes on, because an owner that cannot
 * be reached is no reason to leave a worker stuck.
 *
 * **The ladder stopping short is a second thing, and it is told too.** `tellStopped` is the exit
 * cap, an unreadable counter file and unwritten evidence — the cases where the worker will not
 * take the step that would free it — so the owner hears about each of them once, and the "one
 * message per stuck call" rule above is about the overdue notice that opens the ladder. A stop's
 * notice is sent again as the overdue one is but for how its re-sends end: it says that the ladder
 * stopped short for the call, which stays true once the call ends, so only an answer or
 * `RESEND_CAP_MS` ends them. A stop already told starts no second run of its re-sends.
 */
export function createStallTeller(deps: StallTellerDeps): StallWatchdogTeller {
  const sleep = deps.sleep ?? unheldSleep;
  /** What each call's telling achieved: the first attempt's answer, which the ladder gets, until a
   * re-send tells the owner. A second telling of the same call repeats nothing and starts no second
   * run of re-sends. */
  const told = new Map<string, StallTold>();
  /** The stops already told, by call and reason. */
  const stopsTold = new Set<string>();
  /** The calls the ladder stopped short for: the stop's notice supersedes the overdue one. */
  const stoppedCalls = new Set<string>();

  const write = (record: StallWatchRecord): boolean => {
    try {
      deps.appendEntry(STALL_WATCH_ENTRY, record);
      return true;
    } catch (error) {
      deps.log.warn("legion: the stall watch could not write its record to the transcript", {
        toolCallId: record.toolCallId,
        kind: record.kind,
        error: messageFor(error),
      });
      return false;
    }
  };

  /**
   * Whether the issue's own architect claim is live, as the phase reminder reads it. Everything
   * fails open to the tree's architect and is logged at debug: an unreachable daemon is exactly
   * when a stuck worker must still be reported.
   */
  const liveArchitect = async (issue: string): Promise<boolean> => {
    try {
      const record = await withBound(
        (signal) => deps.readRecord(issue, signal),
        deps.recordBoundMs ?? RECORD_READ_BOUND_MS
      );
      return architectLive(record);
    } catch (error) {
      deps.log.debug("legion: the daemon did not answer the stall notice's record check", {
        issue,
        error: messageFor(error),
      });
      return false;
    }
  };

  /** One attempt of a notice, and what it came to. Nothing here throws. A kept notice and a
   * refusal for good are logged here; a failure no rule decides, and an attempt that told the owner
   * only on a re-send, are the re-sends' to log. */
  const attemptNotice = async (
    send: NoticeSend,
    attempt: number,
    where: Record<string, unknown>
  ): Promise<NoticeAttempt> => {
    try {
      const answer = await send();
      // A kept answer (202, `PublishResult.kept`) means that no live session holds the owner's
      // role: the listener kept the notice on the role's kept lane for the next session that holds
      // it live within the hour. That has told no owner yet, so the worker records no owner as
      // told; and the notice is not lost, so it is not sent again. The phase reminder's notice
      // reads a kept answer as no owner told the same way.
      if (answer.kept === true) {
        deps.log.warn("legion: the listener kept the stall notice; no owner has taken it", {
          ...where,
          attempt,
        });
        return { kind: "kept" };
      }
      return { kind: "stored" };
    } catch (error) {
      const retryAfterSeconds = rateLimitedSeconds(error);
      if (retryAfterSeconds !== undefined) {
        return { kind: "rate-limited", retryAfterSeconds, error: messageFor(error) };
      }
      if (refusedForGood(error)) {
        deps.log.warn("legion: the owner was not told about this session's stuck tool call", {
          ...where,
          attempt,
          error: messageFor(error),
        });
        return { kind: "refused" };
      }
      return { kind: "failed", error: messageFor(error) };
    }
  };

  /**
   * The re-sends of a notice whose first attempt, at `since`, failed for a reason no rule decides.
   * Each waits — the seconds the last failure named where it was a refusal for this session's send
   * rate, the wait `resendWaitMs` names otherwise — and sends the notice again, until one attempt
   * is stored or kept or refused for good; the notice says what is no longer true (`stale`, read
   * just before each re-send, so nothing goes out after it; a stop's notice has none); the next
   * attempt would start past `RESEND_CAP_MS` from the first, judged both when the wait is set and
   * again when it ends, since a wait can end later than it was set for (a process paused, a clock
   * that moved); or the listener refuses the notice for the send rate for the
   * `STALL_NOTICE_SENDS`th time. Each failed attempt leaves one line, saying which of the two comes
   * next: another attempt, or none. A re-send that tells the owner runs `onTold` and says so.
   */
  const resendNotice = async (
    send: NoticeSend,
    where: Record<string, unknown>,
    notice: {
      readonly key: string;
      readonly since: number;
      readonly stale?: () => string | undefined;
    },
    first: NoticeFailure,
    onTold: (() => void) | undefined
  ): Promise<void> => {
    let failure = first;
    let rateRefusals = 0;
    /** Whether an attempt starting now would start past the cap, which ends the re-sends with one
     * warning: `againInMs` is the wait still to come before it, 0 once the wait has ended. */
    const pastCap = (attempts: number, againInMs: number): boolean => {
      const sentForMs = deps.now() - notice.since;
      if (sentForMs + againInMs <= RESEND_CAP_MS) return false;
      deps.log.warn(
        "legion: the stall notice is not sent again, because its re-sends reached their cap",
        {
          ...where,
          key: notice.key,
          attempts,
          sentForMs,
          capMs: RESEND_CAP_MS,
          error: failure.error,
        }
      );
      return true;
    };
    for (let failed = 1; ; failed += 1) {
      if (failure.kind === "rate-limited") {
        rateRefusals += 1;
        if (rateRefusals === STALL_NOTICE_SENDS) {
          deps.log.warn(
            `legion: the listener refused the stall notice for this session's send rate; it was given up after ${STALL_NOTICE_SENDS} sends`,
            { ...where, sends: failed, error: failure.error }
          );
          return;
        }
      }
      const againInMs =
        failure.kind === "rate-limited" ? failure.retryAfterSeconds * 1_000 : resendWaitMs(failed);
      if (pastCap(failed, againInMs)) return;
      if (failure.kind === "rate-limited") {
        deps.log.warn(
          "legion: the listener refused the stall notice for this session's send rate; it is sent again after the wait the listener named",
          {
            ...where,
            sends: failed,
            retryAfterSeconds: failure.retryAfterSeconds,
            error: failure.error,
          }
        );
      } else {
        deps.log.warn("legion: the stall notice was not stored, so it is sent again", {
          ...where,
          attempt: failed,
          againInMs,
          error: failure.error,
        });
      }
      await sleep(againInMs);
      if (pastCap(failed, 0)) return;
      const stale = notice.stale?.();
      if (stale !== undefined) {
        deps.log.info(stale, { ...where, key: notice.key, attempts: failed });
        return;
      }
      const next = await attemptNotice(send, failed + 1, where);
      if (next.kind === "stored") {
        onTold?.();
        if (failure.kind === "rate-limited") {
          deps.log.warn("legion: the stall notice was sent again and its owner was told", {
            ...where,
            sends: failed + 1,
          });
        } else {
          deps.log.info("legion: the owner was told late about this session's stuck tool call", {
            ...where,
            attempt: failed + 1,
          });
        }
        return;
      }
      if (next.kind === "kept" || next.kind === "refused") return;
      failure = next;
    }
  };

  /** One notice to its owner: what its first attempt achieved, which is the answer the ladder
   * gets, and the start of its re-sends when that attempt failed for a reason no rule decides.
   * Nothing here throws, and nothing here waits past that first attempt. */
  const publishNotice = async (
    notice: StallNotice,
    input: {
      readonly message: string;
      readonly payload: string;
      readonly key: string;
      /** Why the notice says what is no longer true, read before each re-send; undefined while
       * it is still true. An overdue notice's alone: a stop's notice stays true (`RESEND_CAP_MS`). */
      readonly stale?: () => string | undefined;
    },
    context: Record<string, unknown>
  ): Promise<NoticeSent> => {
    if (notice.kind === "none") {
      deps.log.warn("legion: nobody was told about this session's stuck tool call", {
        ...context,
        why: notice.why,
      });
      return NOTICE_UNTOLD;
    }
    const claim = deps.claim();
    const publish = deps.publisher();
    if (publish === undefined || claim === undefined) {
      deps.log.warn("legion: Envoy is off for this session, so its owner was not told", {
        ...context,
        topic: notice.topic,
      });
      return NOTICE_UNTOLD;
    }
    const where = { ...context, topic: notice.topic };
    // Every attempt is this one publish, under one idempotency key: an attempt the listener stored
    // but whose answer was lost is answered as a repeat on the next, never stored twice.
    const send = () =>
      publish({
        topic: notice.topic,
        message: input.message,
        // The listener refuses an object here: the facts travel as a JSON string.
        payload: input.payload,
        idempotencyKey: input.key,
        sourceSessionID: claim.sessionID,
      });
    const since = deps.now();
    const first = await attemptNotice(send, 1, where);
    if (first.kind === "stored") return { ownerTold: true, resend: undefined };
    if (first.kind === "kept" || first.kind === "refused") return NOTICE_UNTOLD;
    return {
      ownerTold: false,
      resend: (onTold) => {
        // Never awaited: the ladder has its answer. A wait or a log line that throws ends the
        // re-sends with a line of their own, never an unhandled rejection.
        resendNotice(
          send,
          where,
          { key: input.key, since, stale: input.stale },
          first,
          onTold
        ).catch((error: unknown) => {
          deps.log.warn("legion: the stall notice's re-sends stopped", {
            ...where,
            error: messageFor(error),
          });
        });
      },
    };
  };

  const noticeFor = async (): Promise<StallNotice> => {
    const session = deps.session();
    const claim = deps.claim();
    // Only a phase worker's notice depends on the daemon's record, so only it pays the read.
    const live =
      session.kind === "phase-worker" && claim !== undefined
        ? await liveArchitect(claim.issue)
        : false;
    try {
      return stallNoticeTopic({ session, claim, project: deps.project, liveArchitect: live });
    } catch (error) {
      // An issue key of a shape `roleToken` refuses is one owner not told, never a ladder that
      // stops: the evidence is written and the steps run either way.
      return {
        kind: "none",
        why: `this session's role topic could not be built: ${messageFor(error)}`,
      };
    }
  };

  return {
    tellOverdue: async (evidence: StallEvidence): Promise<StallTold> => {
      const { toolCallId } = evidence.call;
      const already = told.get(toolCallId);
      if (already !== undefined) {
        deps.log.debug("legion: this stuck call's owner has already been told", { toolCallId });
        return already;
      }
      const record = stallEvidenceRecord(evidence, deps.now(), {
        session: deps.session(),
        claim: deps.claim(),
      });
      const evidenceWritten = write(record);
      deps.log.warn("legion: a tool call of this session has not returned", {
        toolCallId,
        tool: record.tool,
        openMs: record.openMs,
        allowedMs: record.allowedMs,
        arguments: record.arguments,
        tree: record.tree.read ? record.tree.processes.length : record.tree.reason,
      });
      const sent = await publishNotice(
        await noticeFor(),
        {
          message: stallNoticeMessage(record),
          payload: JSON.stringify(record),
          key: `legion-stall:${toolCallId}`,
          stale: () => {
            if (!deps.openCalls().some((open) => open.toolCallId === toolCallId)) return CALL_ENDED;
            return stoppedCalls.has(toolCallId) ? STOP_SUPERSEDES : undefined;
          },
        },
        { toolCallId, tool: record.tool }
      );
      const answer = { evidenceWritten, ownerTold: sent.ownerTold };
      told.set(toolCallId, answer);
      // Started only now, so a re-send that tells the owner always finds this record to correct,
      // and a later telling of this call answers that the owner was told.
      sent.resend?.(() => told.set(toolCallId, { evidenceWritten, ownerTold: true }));
      return answer;
    },

    recordStep: ({ call, step, openMs, outcome }) => {
      write({
        kind: "step",
        at: new Date(deps.now()).toISOString(),
        toolCallId: call.toolCallId,
        tool: call.toolName,
        step,
        openMs,
        outcome,
      });
      deps.log.warn("legion: the stall watch carried a stuck tool call one step", {
        toolCallId: call.toolCallId,
        tool: call.toolName,
        step,
        openMs,
        outcome,
      });
    },

    tellStopped: async (call: StallCall, reason: string): Promise<void> => {
      write({
        kind: "stopped",
        at: new Date(deps.now()).toISOString(),
        toolCallId: call.toolCallId,
        tool: call.toolName,
        reason,
      });
      deps.log.warn("legion: the stall watch stopped short of a step", {
        toolCallId: call.toolCallId,
        tool: call.toolName,
        reason,
      });
      stoppedCalls.add(call.toolCallId);
      const key = `${call.toolCallId}:${reason}`;
      if (stopsTold.has(key)) return;
      stopsTold.add(key);
      const claim = deps.claim();
      const who = claim === undefined ? "A session" : `The ${claim.role} of ${claim.issue}`;
      const sent = await publishNotice(
        await noticeFor(),
        {
          message:
            `${who} has a \`${call.toolName}\` call it cannot recover from on its own: ${reason}. ` +
            "The evidence for the call is in that session's transcript.",
          payload: JSON.stringify({ toolCallId: call.toolCallId, tool: call.toolName, reason }),
          key: `legion-stall-stopped:${call.toolCallId}`,
          // No `stale`: that the ladder stopped short stays true once the call ends, which its
          // abort usually brings about, so only an answer or `RESEND_CAP_MS` ends the re-sends.
        },
        { toolCallId: call.toolCallId, tool: call.toolName, reason }
      );
      sent.resend?.();
    },
  };
}
