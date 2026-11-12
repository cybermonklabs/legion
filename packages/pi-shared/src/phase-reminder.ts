import { randomUUID } from "node:crypto";
import { ROLE_TOPIC_PREFIX, roleToken } from "@legion/contracts";
import type { LegionOwed } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import type { OwedInput } from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";
import {
  LAST_MESSAGE_LIMIT,
  type PhaseStallEntry,
  type PhaseStallInput,
  type PhaseStallState,
  recordedPhaseStall,
  reminderText,
  stepPhaseStall,
} from "./phase-stall";
import type { SessionActivityPhase } from "./session-activity";
import type { StallLimits } from "./stall-limits";

/**
 * The endless phase reminder (INFRA-220): it owns the phase-stall machine (`src/phase-stall.ts`)
 * for one session, decides how each reminder reaches the worker, and tells the architect when a
 * run of reminders changes nothing. It imports nothing of Oh My Pi — every collaborator is a
 * field, so a unit test drives it without a host, and `extensions/legion.ts` is the only place
 * that knows what `pi` and the daemon client are.
 *
 * Whether a reminder goes out at all is the daemon's answer (`POST /legion/v1/claims/owed`),
 * asked as each one is due: the reminder keeps no model of the issue's phase, so a process the
 * daemon restarts or a pod it replaces asks again and reads the same answer (INFRA-3136).
 *
 * The first reminder of a run is the `session_stop` continuation, which the host turns into one
 * more turn of the same session, and it is the only one of that run: every one after it is a
 * timer, so the run settles and the daemon sees the turn end, can deliver a queued task and can
 * land a held suspension. A host that cannot continue at all says so on `settle`, and its first
 * reminder is a timer too. The host's timer cannot be cancelled (`SessionContext.setTimeout`
 * returns void and there is no clearTimeout), so a monotonic generation, bumped by everything
 * that makes an armed timer stale, is the whole defence against two timers sending two reminders.
 */

/**
 * Publishes one Envoy message (`EnvoyClient.publish`), typed as the part of the listener's answer
 * this reminder reads: `kept` alone. This sender publishes with `keep: false`, so every listener
 * this code knows answers an unheld or lapsed role 404 and keeps nothing, and a kept answer is
 * one none of them gives. It is read all the same (`tellArchitect`), because a listener that
 * ignored the flag would otherwise have the run record an architect as told that no architect was
 * ever handed the notice; reading it costs one field.
 */
export type EnvoyPublish = (input: PublishInput) => Promise<Pick<PublishResult, "kept">>;

export interface PhaseReminderLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface PhaseReminderDeps {
  /** Puts one reminder in front of the worker and starts a turn on it (`pi.sendMessage` with the
   * `legion-phase-reminder` custom type, steered, triggering a turn). A host whose delivery can
   * fail — the Claude Code host writes the reminder over the bridge's inbox — returns a promise
   * that rejects when the reminder did not reach the worker: no turn starts on it, so no settle
   * will arm the next one, and the runner uncounts it and arms the next itself. A host that cannot
   * fail that way (Oh My Pi's `pi.sendMessage` is synchronous) returns nothing. */
  readonly sendReminder: (reminder: string) => void | Promise<void>;
  /** The host's managed one-shot timer (`SessionContext.setTimeout`): it cannot be cancelled, so
   * every callback guards itself on the generation it was armed at. */
  readonly armTimer: (callback: () => void | Promise<void>, delayMs: number) => void;
  /** Persists the record where this host's relaunch reads it again — the Oh My Pi transcript
   * entry, the Claude Code state file — on every change of it. */
  readonly saveEntry: (entry: PhaseStallEntry) => void;
  /** This session's own claim, or undefined before it has registered one. */
  readonly claim: () => ClaimCapability | undefined;
  /** What this session's claim owes now (`LegionDaemonClient.owed`): the one answer that decides
   * whether a due reminder goes out, and the name of the work the reminder and the architect
   * notice give. `signal` is aborted once `OWED_READ_BOUND_MS` passes without an answer, and the
   * request is to abort with it, so a daemon that hangs is left no open request. An answer that
   * does not arrive is `NUDGE_WHEN_OWED_UNREAD`'s to decide, and no host decides it again. */
  readonly readOwed: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>;
  /** The Envoy listener the architect notice goes to, or undefined when Envoy is off for this
   * session. Built lazily and once by the caller. */
  readonly publisher: () => EnvoyPublish | undefined;
  /** `LEGION_PROJECT`: the project token every role topic carries. */
  readonly project: string | undefined;
  readonly limits: StallLimits;
  readonly log: PhaseReminderLog;
  /** The bound on the owed read; `OWED_READ_BOUND_MS` when it is left out. Only a test, whose
   * daemon hangs on purpose, passes one. */
  readonly owedBoundMs?: number;
}

export interface PhaseReminder {
  /** `session_start`: the record this host persisted, or undefined when it holds none yet. */
  readonly restore: (entry: PhaseStallEntry | undefined) => void;
  /** `session_start`, once the claim bootstrap has returned: the resume reminder, for a worker
   * the daemon relaunched owing work that nothing else will wake it for. The daemon's answer
   * when it is due decides whether it goes out. */
  readonly armResumeReminder: () => void;
  /** `message_start`: a turn is running, and every armed timer is now stale. */
  readonly turnStarted: () => void;
  /** An input the session's hooks classified; `settle` and `reminded` are this module's own. */
  readonly apply: (input: Exclude<PhaseStallInput, { kind: "settle" | "reminded" }>) => void;
  /** `session_stop`: the continuation text to return to the host, or undefined. A host that
   * cannot turn this settle into one more turn of the same session — Claude Code ignores what a
   * `StopFailure` hook returns — says so with `canContinue: false`, and the reminder it earned
   * comes from the timer instead, under every guard an armed reminder has. `backgroundWork` is a
   * turn ending with the session's own background job still running, which buys the same quiet
   * wait a WAITING line does. */
  readonly settle: (
    lastAssistantText: string,
    options?: { readonly canContinue?: boolean; readonly backgroundWork?: boolean }
  ) => Promise<string | undefined>;
  /** `session_switch`, `session_branch`, `session_tree`. */
  readonly sessionChanged: () => void;
  /** The phase half of the session's activity report (`src/session-activity.ts`): when the
   * reminder this session armed last is due. This is the one statement of that moment — the
   * delay each arming picks comes from the limits here, so nothing outside recomputes it.
   * `nextReminderAt` is null when no armed timer is still live: none was armed, or something has
   * happened since that made it stale, which is the same generation guard the armed callback
   * applies to itself. */
  readonly phaseWake: () => SessionActivityPhase;
}

/**
 * How long a due reminder waits for the daemon's answer of what this claim owes
 * (`POST /legion/v1/claims/owed`) before it reads that answer as unread (`NUDGE_WHEN_OWED_UNREAD`).
 * `createLegionDaemonClient` puts no deadline on its fetch, and `session_stop` is a hook the host
 * awaits, so the check bounds the read itself and aborts it at the bound. It is a constant, with
 * no setting and no switch: a value nobody measures is worse than a line somebody edits, so the
 * measurement it was sized against is written here.
 *
 * Measured on 2026-11-12. The bound this replaces, 5 s on a read of the daemon's whole state, did
 * not fit INFRA: its `GET /legion/v1/state` answered in 9.2 s to 9.6 s (4,121,439 bytes, at
 * 18:30Z; 8.6 s to 8.8 s from 17:59Z to 18:08Z), so every reminder there timed out, and the
 * timeout reminded. That was most of the fleet's reminder noise: from 13:35Z to 17:25Z INFRA sent
 * 1,642 reminders, BET 71, OSSV 15 and LINUX none. The same INFRA daemon answered a cheap route
 * (`GET /healthz`) in 4.4 ms to 6.7 ms, with one first connection at 1.0 s. The owed route reads
 * the asking claim, its issue's record and its tree root's, and that issue's own phase, pull
 * request and check rows (`claimOwed` in `packages/daemon/internal/api/owed.go`, `handoffPosition`
 * in `handoff.go`), each by key, so its time does not grow with the deployment: through the real
 * handler over the test PostgreSQL, with 2,300 issues and 300 live claims seeded, 600 requests
 * answered at a median of 1.9 ms to 2.0 ms and a maximum of 9.7 ms to 25.8 ms over two runs.
 *
 * The worst answer those numbers predict is about 1.03 s: a first connection of 1.0 s, INFRA's
 * network of about 5 ms, and the route's own 26 ms. 3 s is about three times that, which leaves
 * room for a daemon whose database is busy with many turns ending at once, while a daemon that is
 * really down costs a worker's turn end 3 s, not 5 s, and stays well inside the 30 s Claude Code
 * gives the `Stop` hook that awaits it (`packages/claude-envoy/scripts/build.ts`).
 */
export const OWED_READ_BOUND_MS = 3_000;

/**
 * Whether a due reminder still goes out when the daemon's answer of what this claim owes could
 * not be read: the session holds no claim yet, or the read failed — the daemon unreachable, a
 * refusal, an answer the contract does not describe, or no answer inside `OWED_READ_BOUND_MS`.
 * This is the one place that choice is made. Both hosts build their reminder here
 * (`createPhaseReminder`: Oh My Pi in `packages/pi-legion/extensions/legion.ts`, Claude Code in
 * `packages/claude-envoy/src/legion-phase-hook.ts`), and neither decides it again.
 *
 * It is true — the worker is nudged on a failed read — because nothing else nudges a stalled
 * worker today. dispatch://INFRA-3163 (cybermonklabs/legion#3829, in review) makes the daemon
 * itself send a silent phase worker its task after `worker_silence_minutes`; once that is
 * deployed everywhere, this can be false, and a read that fails reminds of nothing. R5 of
 * dispatch://INFRA-3136 ships the nudge because it must be right on every deployment the day it
 * lands, those that have not rolled #3829 included. The nudge says that it could not read the
 * record (`UNREAD_REMINDER`), and names no phase, no operation and no owed work, because it read
 * none of them.
 */
export const NUDGE_WHEN_OWED_UNREAD: boolean = true;

/**
 * The listener's "no live session holds that role": a 404 whose body names `unclaimed` or
 * `holder_lapsed` (`packages/envoy/cmd/listener/api.go`). Nobody was told, so `reported` stays
 * false and the next delivered reminder publishes the same notice again, under the same
 * idempotency key, until a session holding the role takes it.
 *
 * It is the only answer an unheld or lapsed architect role gets here, because this notice is
 * published with `keep: false` (INFRA-839). That is what the kept lane reserves the flag for: a
 * sender that holds a retry of its own, as the daemon's notice publisher does
 * (`packages/daemon/internal/notify/notify.go`). This sender's retry is the reminding itself —
 * reminders never stop while the work is owed, so every one after this one tries the notice
 * again — while a kept copy waits out the listener's window and is then dropped with nothing
 * left to send it again.
 *
 * It stays a debug line rather than a warning because an architect that is away is not this
 * worker's doing, and the retry is already in hand.
 */
function roleUnheld(error: unknown): boolean {
  if (!(error instanceof EnvoyApiError) || error.details.status !== 404) return false;
  let body: unknown;
  try {
    body = JSON.parse(error.details.responseBody);
  } catch {
    return false;
  }
  if (typeof body !== "object" || body === null || !("reason" in body)) return false;
  return body.reason === "unclaimed" || body.reason === "holder_lapsed";
}

/** `work`'s answer, or a rejection once `ms` have passed without one, when the signal `work` was
 * handed is aborted so what it started does not outlive the bound (`withinBudget` in
 * `src/capability-report.ts` is the same shape, with a fallback answer in place of the throw). */
async function withBound<T>(work: (signal: AbortSignal) => Promise<T>, ms: number): Promise<T> {
  const controller = new AbortController();
  const deadline = Promise.withResolvers<T>();
  const timer = setTimeout(() => {
    const late = new Error(`no answer within ${ms}ms`);
    deadline.reject(late);
    controller.abort(late);
  }, ms);
  try {
    return await Promise.race([work(controller.signal), deadline.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** What the owed check decided, for the reminder and the architect notice that may follow it: the
 * same reminder never asks the daemon twice. `work` is the daemon's name for what is owed, and
 * undefined when its answer did not arrive (`NUDGE_WHEN_OWED_UNREAD`), which names no work. */
type OwedCheck =
  | { readonly reminds: false }
  | { readonly reminds: true; readonly work: string | undefined };

/** The owed check's decision when the daemon's answer could not be read. */
const UNREAD_CHECK: OwedCheck = NUDGE_WHEN_OWED_UNREAD
  ? { reminds: true, work: undefined }
  : { reminds: false };

export function createPhaseReminder(deps: PhaseReminderDeps): PhaseReminder {
  const boundMs = deps.owedBoundMs ?? OWED_READ_BOUND_MS;
  let stall: PhaseStallState = { state: "active", count: 0 };
  let reported = false;
  let reportKey: string | undefined;
  /** Bumped by everything that makes an armed timer stale. */
  let generation = 0;
  let turnRunning = false;
  /** Whether a continuation has already been delivered in the run that is now settling. The count
   * cannot say so: the continuation's own turn is one the worker is being told to call a tool in,
   * so a `tool-call` or an `inbound` puts the count back to 0 mid-run. At most one continuation
   * per run: the run then settles, the daemon sees the turn end, and the next reminder comes from
   * the timer with its delay. */
  let continued = false;
  /** The head of what the worker said as its last turn settled: the architect notice quotes it,
   * and it rides the record, so a notice the resume reminder of a relaunched worker earns still
   * names what the worker said before the relaunch. */
  let lastMessage = "";
  /** The entry last saved or restored, so only a change of the recorded value is saved. */
  let recorded = "";
  /** The wake the last `armReminder` set: the moment its timer is due, and the generation it was
   * armed at, so a stale one is reported as no wake at all. The host's timer cannot be cancelled,
   * so this is dropped by the same guard the callback is. */
  let armed: { readonly at: number; readonly generation: number } | undefined;

  const entryOf = (): PhaseStallEntry => ({
    // The machine's own wait that the record does not hold (`recordedPhaseStall`): a session
    // restores a record only after a relaunch, which is the round that wait belonged to ending.
    state: recordedPhaseStall(stall.state),
    count: stall.count,
    reported,
    ...(reportKey === undefined ? {} : { reportKey }),
    ...(lastMessage === "" ? {} : { lastMessage }),
  });

  const writeEntry = (): void => {
    const entry = entryOf();
    const written = JSON.stringify(entry);
    if (written === recorded) return;
    recorded = written;
    deps.saveEntry(entry);
  };
  recorded = JSON.stringify(entryOf());

  const advance = (input: PhaseStallInput): boolean => {
    const step = stepPhaseStall(stall, input);
    stall = step.state;
    // A count back at zero is a worker that has done something: the next run of reminders reports
    // afresh, under a key of its own.
    if (stall.count === 0) {
      reported = false;
      reportKey = undefined;
    }
    writeEntry();
    return step.remind === true;
  };

  /**
   * What this session's claim owes, asked of the daemon and bounded. The reminder is dropped when
   * the daemon answers that the claim owes nothing — a worker whose phase has finished, whether
   * through the tool, from the shell, or by a move of the issue it did not make. An answer that
   * could not be read is `UNREAD_CHECK`, logged at debug: whether it reminds is
   * `NUDGE_WHEN_OWED_UNREAD`'s choice, and a reminder it sends names no work.
   */
  const owedCheck = async (): Promise<OwedCheck> => {
    const claim = deps.claim();
    if (claim === undefined) return UNREAD_CHECK;
    let answer: LegionOwed;
    try {
      answer = await withBound(
        (signal) =>
          deps.readOwed(
            { claimToken: claim.claimToken, sessionId: claim.sessionID, secret: claim.secret },
            signal
          ),
        boundMs
      );
    } catch (error) {
      deps.log.debug("legion: the daemon did not say what this claim owes", {
        issue: claim.issue,
        error: messageFor(error),
      });
      return UNREAD_CHECK;
    }
    if (!answer.owes) {
      deps.log.debug("legion: the daemon says this claim owes nothing; no reminder is sent", {
        issue: claim.issue,
        role: claim.role,
      });
      return { reminds: false };
    }
    return { reminds: true, work: answer.work };
  };

  /**
   * D6: one Envoy message to the architect that owns this worker, when a delivered reminder
   * brought the count to the threshold. It names the work the daemon answered was owed, or says
   * that the record could not be read, as the reminder did. `reportKey` is minted at the first
   * attempt of a run of reminders and kept until the count resets, so a publish that timed out
   * after the listener accepted it is not delivered twice, and a failure leaves `reported` false
   * for the next reminder to try again. The key also names the run an acceptance belongs to: one
   * that lands after the run ended acknowledges nothing.
   */
  const tellArchitect = async (work: string | undefined): Promise<void> => {
    const claim = deps.claim();
    if (claim === undefined) return;
    const publish = deps.publisher();
    if (publish === undefined) {
      deps.log.debug("legion: Envoy is off for this session; the architect is not told", {
        issue: claim.issue,
        reminders: stall.count,
      });
      return;
    }
    reportKey ??= randomUUID();
    const key = reportKey;
    writeEntry();
    const reminders = stall.count;
    const owed =
      work === undefined
        ? "its turns keep ending while the daemon's record of what it owes cannot be read"
        : `its turns keep ending with ${work} still owed`;
    try {
      // The tree's architect, named from the claim's tree and LEGION_PROJECT exactly as the
      // daemon's `Legion addressing` names "the architect that owns your issue" to this pane
      // (`AddressingFragment`, packages/daemon/internal/daemon/specs.go). Nothing is read for it:
      // the whole state the issue's own architect claim was once read from took longer than any
      // bound on INFRA, exactly when a stalled worker's architect most needs telling.
      const topic = ROLE_TOPIC_PREFIX + roleToken(deps.project ?? "", claim.tree, "architect");
      const answer = await publish({
        topic,
        message:
          `The ${claim.role} of ${claim.issue} has let ${reminders} reminders go unanswered: ` +
          `${owed}, and nothing it does clears the reminder. ` +
          `What it last said was: ${lastMessage}`,
        // The listener refuses an object here: the facts travel as a JSON string. `work` is left
        // out when the daemon's answer did not arrive.
        payload: JSON.stringify({
          issue: claim.issue,
          role: claim.role,
          ...(work === undefined ? {} : { work }),
          reminders,
          lastMessage,
        }),
        idempotencyKey: key,
        sourceSessionID: claim.sessionID,
        // This sender keeps its own retry, so the listener is to keep nothing: an unheld or
        // lapsed role is answered 404 (`roleUnheld`) and the next reminder tries again, rather
        // than a kept copy that is dropped when its window passes and that this run would read
        // as the architect having been told.
        keep: false,
      });
      // The publish has no deadline of its own, so the worker's own turn may have reset the count
      // while it was in flight, and a later run may have begun. That run has a key of its own, or
      // none yet: this acceptance is not its notice.
      if (reportKey !== key) return;
      // A listener that kept the notice all the same (202, `PublishResult.kept`) has told no
      // architect, and its copy is dropped once the kept window passes. It is read as the 404 is:
      // nobody was told, so the run's next reminder publishes the same notice again under the
      // same key. No listener this code knows answers it, since the publish above sends
      // `keep: false`; it is read because one that ignored the flag would otherwise have this run
      // record an architect as told that no architect was ever handed the notice.
      if (answer.kept === true) {
        deps.log.debug("legion: the listener kept the architect notice; telling it again later", {
          issue: claim.issue,
          reminders,
        });
        return;
      }
      reported = true;
      writeEntry();
    } catch (error) {
      if (roleUnheld(error)) {
        deps.log.debug("legion: no session holds the architect role; telling it again later", {
          issue: claim.issue,
          reminders,
        });
        return;
      }
      // Any other failure — a 5xx, a 400 or 403, a timeout — leaves `reported` false too, and the
      // next delivered reminder of this run publishes again under the same key. This departs from
      // the spec's "logged and dropped" on purpose: the one key makes the retry safe against a
      // publish the listener did accept, and a listener that is failing is exactly when the
      // architect most needs telling.
      deps.log.warn("legion: the architect was not told its worker is stalled", {
        issue: claim.issue,
        reminders,
        error: messageFor(error),
      });
    }
  };

  const deliver = async (lastText: string, work: string | undefined): Promise<void> => {
    // Counted before it is sent, so a reminder the worker was given is never uncounted: a host
    // whose turn starts inside `sendReminder` saves the count before the turn can move it.
    const uncounted = stall;
    advance({ kind: "reminded" });
    const counted = stall;
    generation += 1;
    const sentAt = generation;
    try {
      await deps.sendReminder(reminderText(lastText, work));
    } catch (error) {
      deps.log.warn("legion: a phase reminder did not reach this worker; it comes again later", {
        error: messageFor(error),
      });
      // It reached no one, so it counts for nothing, unless the state has moved on since. No turn
      // starts on it, so no settle arms the next one: this does, under the usual guard, unless
      // something has happened since — a turn that started arms its own when it settles.
      if (stall === counted) {
        stall = uncounted;
        writeEntry();
      }
      if (generation === sentAt) armReminder(lastText, deps.limits.reminderMs);
      return;
    }
    if (reported || stall.count < deps.limits.architectAfter) return;
    await tellArchitect(work);
  };

  /**
   * D4: the one guard. A timer fires only while nothing has happened since it was armed, no turn
   * is running, and the daemon does not answer that the claim owes nothing. The check awaits the
   * daemon, so the generation and the turn are read again after it. `lastText` is what the worker
   * said as the turn that armed it settled, which decides whether the reminder leads with the
   * tool call written as text.
   */
  const armReminder = (lastText: string, delayMs: number): void => {
    const armedAt = generation;
    armed = { at: Date.now() + delayMs, generation: armedAt };
    deps.armTimer(async () => {
      if (generation !== armedAt || turnRunning) return;
      const check = await owedCheck();
      if (!check.reminds) return;
      if (generation !== armedAt || turnRunning) return;
      await deliver(lastText, check.work);
    }, delayMs);
  };

  return {
    restore: (entry) => {
      stall =
        entry === undefined
          ? { state: "active", count: 0 }
          : { state: entry.state, count: entry.count };
      // The run of reminders the restored count belongs to keeps its notice: a relaunch mid-run
      // neither repeats one the listener accepted nor mints a second key for one it refused. No
      // record holds `awaiting-update` (`recordedPhaseStall`): the relaunch this session resumed
      // on is the update round ending, so a restored `waiting` is a WAITING reply's, which the
      // reminder times out as it always did.
      reported = entry?.reported ?? false;
      reportKey = entry?.reportKey;
      lastMessage = entry?.lastMessage ?? "";
      recorded = JSON.stringify(entryOf());
    },
    armResumeReminder: () => {
      armReminder("", deps.limits.resumeMs);
    },
    turnStarted: () => {
      generation += 1;
      turnRunning = true;
    },
    apply: (input) => {
      advance(input);
    },
    settle: async (text, options) => {
      turnRunning = false;
      generation += 1;
      lastMessage = text.slice(0, LAST_MESSAGE_LIMIT);
      const before = stall.count;
      const remind = advance({
        kind: "settle",
        lastAssistantText: text,
        backgroundWork: options?.backgroundWork,
      });
      // Every outcome but the continuation ends the run here, so the next one may continue again.

      // Neither wait earns a reminder now. The wait a `ready_for_update` answer created is the
      // daemon's to end, so nothing is armed in it: the round replaces the pod, and the task it
      // held behind the round reaches the worker in the process that comes back on the new pod,
      // while a reminder steered in meanwhile is the turn the daemon reads as the answer
      // retracted (`awaiting-update` in `./phase-stall`). The machine sends no reminder into a
      // quiet wait either, so the timer carries the one this settling message earned, for when
      // the wait has been long enough.
      if (!remind) {
        continued = false;
        if (stall.state === "waiting") armReminder(text, deps.limits.waitingMs);
        return undefined;
      }
      // The continuation is the first reminder of a run and the only one of it: the worker has
      // been reminded already (`before`), has had this run's continuation (`continued`), or is on
      // a host that cannot continue at all, and the reminder comes from a timer instead. The run
      // then settles, the daemon sees the turn end, and the next reminder comes with its delay.
      if (before !== 0 || continued || options?.canContinue === false) {
        continued = false;
        armReminder(text, deps.limits.reminderMs);
        return undefined;
      }
      const check = await owedCheck();
      if (!check.reminds) {
        continued = false;
        return undefined;
      }
      advance({ kind: "reminded" });
      generation += 1;
      continued = true;
      return reminderText(text, check.work);
    },
    sessionChanged: () => {
      generation += 1;
    },
    phaseWake: () => {
      if (armed === undefined || armed.generation !== generation) {
        return { nextReminderAt: null };
      }
      return { nextReminderAt: new Date(armed.at).toISOString() };
    },
  };
}
