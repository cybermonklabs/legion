import { randomUUID } from "node:crypto";
import { roleToken, roleTopic } from "@legion/contracts";
import type { LegionOwed, LegionWaitNews } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type KeyTakenResult,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import type { OwedInput, WaitNewsInput } from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";
import {
  type GateHold,
  LAST_MESSAGE_LIMIT,
  type PhaseStallEntry,
  type PhaseStallInput,
  type PhaseStallState,
  quietReminderText,
  recordedPhaseStall,
  reminderText,
  stepPhaseStall,
  type TurnEnd,
  type WaitNewsReport,
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
 * asked at each turn's end, at boot and as each timer is due: the reminder keeps no model of the
 * issue's phase, so a process the daemon restarts or a pod it replaces asks again and reads the
 * same answer. The same answer gives the wait the worker declared (`declare_wait`) while it
 * stands, and the closed design gate that holds a merger (dispatch://INFRA-2040). While either
 * stands, nothing is armed and nothing is sent but the reminder for a tool call written as text
 * (`quietReminderText`), and the activity report says no reminder is due, so the daemon can
 * release the pod. News after the declaration is reported to the daemon (`news`), whose answer is
 * the word on the wait; a report that fails ends the wait in the plugin and is kept, with its
 * delivery's key, until a turn's end, a timer or the next boot sends it again.
 *
 * The first reminder of a run is the `session_stop` continuation, which the host turns into one
 * more turn of the same session, and it is the only one of that run: every one after it is a
 * timer, so the run settles and the daemon sees the turn end, can deliver a queued task and can
 * land a held suspension. A turn the provider refused earns no continuation, since the worker
 * never read the last reminder and the next turn would be refused the same way; it earns one
 * timer instead, so a single transient refusal cannot leave the worker idle. The host's timer
 * cannot be cancelled (`SessionContext.setTimeout` returns void and there is no clearTimeout), so
 * a monotonic generation, bumped by everything that makes an armed timer stale, is the whole
 * defence against two timers sending two reminders.
 */

/**
 * Publishes one Envoy message (`EnvoyClient.publish`), typed as the part of the listener's answer
 * this reminder reads: `kept` and `keptUntil`. The notice sends no `keep` field, so a listener
 * keeps it for an architect role no live session holds and answers 202 kept, which tells no
 * architect (`tellArchitect`). `keptUntil` says whether the next attempt may send the same key
 * again: a listener that names it answers a repeat with the copy it still holds, or keeps a new
 * copy once that copy has been forwarded or its hour has ended. A listener of an earlier release
 * names none, and answers a repeat inside its stream's 72-hour duplicate window as the first copy,
 * which it may already have dropped. A key another sender's message already holds is answered as
 * taken ({@link KeyTakenResult}): nothing was delivered, and the next attempt mints a new key.
 */
export type EnvoyPublish = (
  input: PublishInput
) => Promise<Pick<PublishResult, "kept" | "keptUntil"> | KeyTakenResult>;

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
   * whether a due reminder goes out, the name of the work the reminder and the architect notice
   * give, the architect that owns the claim, which the architect notice goes to, and the wait and
   * the hold that keep every reminder back but one. `signal` is aborted once `OWED_READ_BOUND_MS`
   * passes without an answer, and the request is to abort with it, so a daemon that hangs is left
   * no open request. An answer that does not arrive is `UNREAD_CHECK`'s to decide, and no host
   * decides it again. */
  readonly readOwed: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>;
  /** Reports news after the declaration of a wait to the daemon (`LegionDaemonClient.waitNews`).
   * The answer is the daemon's word on the wait: `ended` true ends it, and false keeps it standing.
   * `signal` is aborted once `OWED_READ_BOUND_MS` passes without an answer, as the owed read's is,
   * and a report with no answer by then is one that failed. */
  readonly reportNews: (input: WaitNewsInput, signal: AbortSignal) => Promise<LegionWaitNews>;
  /** The Envoy listener the architect notice goes to, or undefined when Envoy is off for this
   * session. Built lazily and once by the caller. */
  readonly publisher: () => Promise<EnvoyPublish | undefined>;
  /** `LEGION_PROJECT`: the project token every role topic carries. */
  readonly project: string | undefined;
  readonly limits: StallLimits;
  readonly log: PhaseReminderLog;
  /** The bound on the owed read and on a report of news; `OWED_READ_BOUND_MS` when it is left
   * out. Only a test, whose daemon hangs on purpose, passes one. */
  readonly owedBoundMs?: number;
}

export interface PhaseReminder {
  /** `session_start`: the record this host persisted, or undefined when it holds none yet. */
  readonly restore: (entry: PhaseStallEntry | undefined) => void;
  /** `session_start`, once the claim bootstrap has returned: sends any report of news a previous
   * process left unsent, reads the owed answer, which also says which wait stands, and then arms
   * the resume reminder for a worker that owes work nothing else will wake it for, unless a wait
   * or the design gate's hold stands, or a turn has started meanwhile, when it arms nothing. The
   * daemon's answer when the reminder is due decides whether it goes out. */
  readonly armResumeReminder: () => Promise<void>;
  /** `message_start`: a turn is running, and every armed timer is now stale. */
  readonly turnStarted: () => void;
  /** An input the session's hooks classified: a daemon `prompt`, `update-answered` or
   * `tool-call`. `news` arrives through `news`, and `settle`, `refused` and `reminded` are this
   * module's own — the first two arrive through `settle`. */
  readonly apply: (
    input: Exclude<PhaseStallInput, { kind: "news" | "settle" | "refused" | "reminded" }>
  ) => void;
  /** News reached the session: an Envoy delivery, with `deliveryKey` its own dedupe key as the
   * host read it off the delivery, or a person's message, with none. It starts the count again,
   * and while a wait the plugin knows of stands it is reported to the daemon against that wait,
   * the key verbatim. It is not reported once the daemon has answered a report of that wait that
   * was not its update ask: a report then ends nothing. The promise settles once the daemon
   * answered, or the report failed and is kept to be sent again; it never rejects. */
  readonly news: (deliveryKey?: string | undefined) => Promise<void>;
  /** The `legion` tool's `declare_wait` returned the wait `waitId` (`onWaitDeclared` in
   * `src/legion-phase-ops.ts`): news after it is reported against it. A report kept unsent for
   * the wait it replaces is dropped, since the daemon ended that wait at this declaration. */
  readonly waitDeclared: (waitId: string) => void;
  /** `session_stop`: the turn's end as the session's hooks classified it (`turnEndOf`), and the
   * continuation text to return to the host, or undefined. A host that cannot turn this settle
   * into one more turn of the same session gets undefined whatever it asked: a refused turn earns
   * no continuation — its reminder comes from the timer this arms instead — and Claude Code's
   * `StopFailure`, the one settle it ignores the answer of, is exactly a refused turn. A settle
   * whose `backgroundWork` is set is a turn ending with the session's own background job still
   * running, which buys the quiet wait. Every settle but a refused one and one in the update wait
   * reads the owed answer, after sending any report of news kept unsent, before it arms anything:
   * a refused one arms its one timer, whose own read decides. */
  readonly settle: (turn: TurnEnd) => Promise<string | undefined>;
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
 * (`POST /legion/v1/claims/owed`) before it reads that answer as unread (`UNREAD_CHECK`).
 * `createLegionDaemonClient` puts no deadline on its fetch, and `session_stop` is a hook the host
 * awaits, so the check bounds the read itself and aborts it at the bound. It is a constant, with
 * no setting and no switch: a value nobody measures is worse than a line somebody edits, so the
 * measurement it was sized against is written here.
 *
 * Measured on 2026-11-12. The INFRA daemon answered a cheap route (`GET /healthz`) in 4.4 ms to
 * 6.7 ms, with one first connection at 1.0 s, while its `GET /legion/v1/state` answered in 9.2 s
 * to 9.6 s (4,121,439 bytes), which is why the check reads no whole state. The owed route reads
 * the asking claim, its issue's record and its tree root's, and that issue's own phase, pull
 * request and check rows (`claimOwed` in `packages/daemon/internal/api/owed.go`, `handoffPosition`
 * in `handoff.go`), each by key, so its time does not grow with the deployment: through the real
 * handler over the test PostgreSQL, with 2,300 issues and 300 live claims seeded, 600 requests
 * answered at a median of 1.9 ms to 2.0 ms and a maximum of 9.7 ms to 25.8 ms over two runs. The
 * route has since also come to read, for the architect that owns the claim, the issues of the
 * claim's tree and their architect claims (`owningArchitect` in `owed.go`), which grow with the
 * tree and not with the deployment; that read was not part of the measurement.
 *
 * The worst answer those numbers predict is about 1.03 s: a first connection of 1.0 s, INFRA's
 * network of about 5 ms, and the route's own 26 ms. 3 s is about three times that, which leaves
 * room for a daemon whose database is busy with many turns ending at once, while a daemon that is
 * really down costs a worker's turn end 3 s, not 5 s, and stays well inside the 30 s Claude Code
 * gives the `Stop` hook that awaits it (`packages/claude-envoy/scripts/build.ts`).
 *
 * A report of news (`PhaseReminderDeps.reportNews`) has the same bound, since it goes out at the
 * same moments as the read. Its route costs more than the owed read: `(*server).waitNews` in
 * `packages/daemon/internal/api/wait.go` applies a fact under the daemon's fact lock. The kept
 * reports go out one at a time, in passes, and a pass stops at the first report with no answer
 * within the bound (`sendUnsent`). A turn's end waits for the pass that runs, for the one pass
 * queued behind it, and for its own read (`sendNews`, `owedCheck`). So a daemon that answers
 * nothing costs a turn's end at most three bounds, 9 s, still well inside those 30 s. A daemon
 * that answers each report just inside the bound costs one bound more for each report those two
 * passes send. Of each wait, a pass sends the reports of the update ask and one report more: the
 * first answer that is not the update ask's drops every other report of its wait.
 */
export const OWED_READ_BOUND_MS = 3_000;

/**
 * The listener's "no live session holds that role, and nothing was kept": a 404 whose body names
 * `unclaimed` or `holder_lapsed` (`packages/envoy/cmd/listener/api.go`). Nobody was told, so
 * `reported` stays false and the next delivered reminder publishes the same notice again, under
 * the same idempotency key, until a session holding the role takes it.
 *
 * The notice sends no `keep` field, so a listener keeps it for a role no live session holds and
 * answers 202 kept rather than this (`tellArchitect`). This 404 comes when the listener's keep
 * failed, and from a listener of an earlier release for a holder that is not live, which such a
 * listener keeps nothing for.
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
 * `pi-legion/src/capability-report.ts` is the same shape, with a fallback answer in place of the
 * throw). */
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

/** What `claim` owes, asked through `readOwed` with the claim's own credential and bounded by
 * `ms` (`withBound`). The phase reminder's owed check and the stall watch's notice
 * (`pi-legion/src/stall-evidence.ts`) both ask it this way, so the two ask the same question.
 * Each catches its own failure and logs its own line, because a read that fails means something
 * different to each: the reminder reminds with no work named, and the notice goes to the tree's
 * architect. */
export function readOwedWithin(
  readOwed: PhaseReminderDeps["readOwed"],
  claim: ClaimCapability,
  ms: number
): Promise<LegionOwed> {
  return withBound(
    (signal) =>
      readOwed(
        { claimToken: claim.claimToken, sessionId: claim.sessionID, secret: claim.secret },
        signal
      ),
    ms
  );
}

/** What the owed check read, for the reminder and the architect notice that may follow it: the
 * same reminder never asks the daemon twice. `read` false is a read that failed (`UNREAD_CHECK`).
 * Otherwise `owes` and `work` are the answer's, `tree` is the claim's tree, which the closed
 * gate's sentence names, and `hold` is the closed design gate that holds the claim. `quiet` says
 * whether a wait or that hold stands, which keeps back every reminder but the one for a tool call
 * written as text. The answer's wait stands unless an unsent report of news names it: that report
 * failed, and a report that fails ends the wait in the plugin. */
type OwedCheck =
  | { readonly read: false }
  | {
      readonly read: true;
      readonly owes: boolean;
      readonly work: string;
      readonly tree: string;
      readonly quiet: boolean;
      readonly hold: GateHold | undefined;
    };

/**
 * The owed check's decision when the daemon's answer of what this claim owes could not be read:
 * the session holds no claim yet, or the read failed — the daemon unreachable, a refusal, an
 * answer the contract does not describe, or no answer inside `OWED_READ_BOUND_MS`. This is the
 * one place that choice is made. Both hosts build their reminder here (`createPhaseReminder`: Oh
 * My Pi in `packages/pi-legion/extensions/legion.ts`, Claude Code in
 * `packages/claude-envoy/src/legion-phase-hook.ts`), and neither decides it again.
 *
 * It reminds — the worker is nudged on a failed read — because on a daemon that does not itself
 * send a silent phase worker its task after `worker_silence_minutes` (dispatch://INFRA-3163),
 * nothing else nudges a stalled worker. The nudge says that it could not read the record
 * (`UNREAD_REMINDER`), and names no phase, no operation that ends one and no owed work, because
 * it read none of them. It holds nothing back for a wait or a hold, since it read neither.
 */
const UNREAD_CHECK: OwedCheck = { read: false };

/** The reminder one check and the worker's last words earn, as `text` puts it in front of the
 * worker: the `owed` reminder, naming the work the daemon gave or, where the read failed, none
 * (`reminderText`); or, while a wait or the design gate's hold stands, the reminder for a tool
 * call written as text (`quietReminderText`), the one reminder either lets through. `work` is what
 * the architect notice names. */
type Due = {
  readonly kind: "owed" | "text-call";
  readonly text: string;
  readonly work: string | undefined;
};

/** What `check` and `lastText` earn, or undefined when nothing is due: the daemon answered that
 * the claim owes nothing, or a wait or a hold stands and the worker's last words hold no tool
 * call written as text. */
function dueOf(check: OwedCheck, lastText: string): Due | undefined {
  if (!check.read) {
    return { kind: "owed", text: reminderText(lastText, undefined), work: undefined };
  }
  if (!check.owes) return undefined;
  if (!check.quiet) {
    return { kind: "owed", text: reminderText(lastText, check.work), work: check.work };
  }
  const text = quietReminderText(lastText, check.tree, check);
  return text === undefined ? undefined : { kind: "text-call", text, work: check.work };
}

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
   * so a `tool-call`, a `prompt` or news puts the count back to 0 mid-run. At most one
   * continuation per run: the run then settles, the daemon sees the turn end, and the next
   * reminder comes from the timer with its delay. */
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
  /** The architect claim the newest owed answer that named one says owns this claim
   * (`LegionOwed.architect`), which the architect notice goes to; undefined until an answer has
   * named one, when the notice goes to the tree's architect. */
  let owner: string | undefined;
  /** The id of the wait the worker declared that this plugin last knew to stand: the one news is
   * reported against. It is learned from `declare_wait`'s answer (`waitDeclared`) and from every
   * owed answer, and forgotten once a report of news ends it. */
  let standing: string | undefined;
  /** The reports of news the daemon has not answered, oldest first (`WaitNewsReport`). */
  let unsent: readonly WaitNewsReport[] = [];
  /** The waits the daemon has answered a report of with no `updateAsk`. The daemon applies the
   * news of a wait under one event id, `wait-news:<claim>:<wait>` (`(*server).waitNews` in
   * `packages/daemon/internal/api/wait.go`), and answers each later report of that wait
   * `ended: false`, as a duplicate. So `news` queues no report of these waits. It holds one id at
   * most for each wait the worker declares. */
  const answered = new Set<string>();
  /** Bumped by every declaration, so an owed answer asked before one does not overwrite the wait
   * that declaration recorded with the answer's older view. */
  let declarations = 0;
  /** The pass of `sendUnsent` that runs now, or the last one: one pass at a time, so no report
   * goes out twice. It never rejects. */
  let sendingNews: Promise<void> = Promise.resolve();
  /** The one pass queued behind `sendingNews` that has not begun, or undefined. */
  let queuedNews: Promise<void> | undefined;

  const entryOf = (): PhaseStallEntry => ({
    // The machine's own wait that the record does not hold (`recordedPhaseStall`): a session
    // restores a record only after a relaunch, which is the round that wait belonged to ending.
    state: recordedPhaseStall(stall.state),
    count: stall.count,
    reported,
    ...(reportKey === undefined ? {} : { reportKey }),
    ...(lastMessage === "" ? {} : { lastMessage }),
    ...(standing === undefined ? {} : { waitId: standing }),
    ...(unsent.length === 0 ? {} : { unsentNews: unsent }),
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

  /** Sends every report of news the daemon has not answered, oldest first, each bounded as the
   * owed read is. An answer takes its report off the list: `ended` true forgets the wait it named,
   * and false keeps it standing, which is the daemon's word on it (an update ask, or a wait that
   * had already ended or been replaced). An answer that is not the update ask's (`updateAsk`)
   * also takes every other report of its wait off the list, and puts the wait in `answered`: the
   * daemon would answer each of those reports as a duplicate. A failure, or no answer within the
   * bound, stops the pass and keeps that report and every later one, so the wait the report names
   * has ended in the plugin (`OwedCheck`) and the report goes again at the next turn's end, timer
   * or boot. A session with no claim yet sends nothing: the boot sends it once the claim is
   * registered. */
  const sendUnsent = async (): Promise<void> => {
    for (const report of unsent) {
      // A declaration, or an answer to another report of the same wait, made while an earlier
      // report was in flight dropped this one.
      if (!unsent.includes(report)) continue;
      const claim = deps.claim();
      if (claim === undefined) return;
      let answer: LegionWaitNews;
      try {
        answer = await withBound(
          (signal) =>
            deps.reportNews(
              {
                claimToken: claim.claimToken,
                sessionId: claim.sessionID,
                secret: claim.secret,
                waitId: report.waitId,
                ...(report.deliveryKey === undefined ? {} : { deliveryKey: report.deliveryKey }),
              },
              signal
            ),
          boundMs
        );
      } catch (error) {
        deps.log.warn(
          "legion: the daemon was not told of news after this worker's wait; the wait ends here, and the report goes again later",
          { issue: claim.issue, wait: report.waitId, error: messageFor(error) }
        );
        return;
      }
      if (answer.updateAsk === true) {
        unsent = unsent.filter((kept) => kept !== report);
      } else {
        answered.add(report.waitId);
        unsent = unsent.filter((kept) => kept.waitId !== report.waitId);
      }
      if (answer.ended && standing === report.waitId) standing = undefined;
      writeEntry();
    }
  };

  /** The one way a report goes out: a pass of `sendUnsent` queued behind the pass that runs, or
   * the pass queued there already. A pass reads the list when it begins, so the queued pass takes
   * every report kept until then, and a burst of news costs one pass. It never rejects, so a
   * turn's end, a timer and the boot go on to the owed read whatever it met: a report that failed
   * is kept by `sendUnsent` itself, and anything else is logged here. */
  const sendNews = (): Promise<void> => {
    if (queuedNews !== undefined) return queuedNews;
    const pass = sendingNews
      .then(() => {
        queuedNews = undefined;
        return sendUnsent();
      })
      .catch((error: unknown) => {
        deps.log.warn("legion: the reports of news after this worker's wait were not sent", {
          error: messageFor(error),
        });
      });
    queuedNews = pass;
    sendingNews = pass;
    return pass;
  };

  /**
   * What this session's claim owes, asked of the daemon and bounded, once every report of news the
   * daemon has not answered has been sent again. The reminder is dropped when
   * the daemon answers that the claim owes nothing — a worker whose phase has finished, whether
   * through the tool, from the shell, or by a move of the issue it did not make, and a claim the
   * daemon has failed, which is the daemon's own word that this process is finished: a claim the
   * daemon failed runs no further turn in this process — its retry relaunches this session in a
   * new process, or starts a new session once the daemon's `forgetSession` (its supervise package)
   * has dropped this one — and a reminder written now reaches nobody who can act on it. That is
<<<<<<< 7fa22de66d
   * what ends the reminding of a session whose provider refuses every turn, on either host. An
   * answer that could not be read is `UNREAD_CHECK`, logged at debug: it reminds, and the reminder
   * it sends names no work.
   *
   * The answer's wait and hold are read whether or not the claim owes, as its architect is. The
   * wait it gives is the one news is reported against from then on (`standing`), unless a
   * declaration came in while the read was out, since that answer was asked before it.
=======
   * what ends the reminding of a session whose provider refuses a turn with a status that cannot
   * clear (dispatch://INFRA-4508); a refusal that can clear fails no claim, so its reminders keep
   * their pace. An answer that could not be read is `UNREAD_CHECK`, logged at debug: it reminds,
   * and the reminder it sends names no work.
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
   */
  const owedCheck = async (): Promise<OwedCheck> => {
    await sendNews();
    const claim = deps.claim();
    if (claim === undefined) return UNREAD_CHECK;
    const declaredBefore = declarations;
    let answer: LegionOwed;
    try {
      answer = await readOwedWithin(deps.readOwed, claim, boundMs);
    } catch (error) {
      deps.log.debug("legion: the daemon did not say what this claim owes", {
        issue: claim.issue,
        error: messageFor(error),
      });
      return UNREAD_CHECK;
    }
    // The architect the daemon names as the claim's owner is kept from every answer that names
    // one, owed or not, so the notice a later reminder earns reaches it even when that
    // reminder's own read fails.
    if (answer.architect !== undefined) owner = answer.architect;
    const wait = answer.wait;
    if (declarations === declaredBefore && standing !== wait?.id) {
      standing = wait?.id;
      writeEntry();
    }
    const waits = wait !== undefined && !unsent.some((report) => report.waitId === wait.id);
    const quiet = waits || answer.hold !== undefined;
    if (!answer.owes) {
      deps.log.debug("legion: the daemon says this claim owes nothing; no reminder is sent", {
        issue: claim.issue,
        role: claim.role,
      });
    } else if (quiet) {
      deps.log.debug(
        "legion: a wait or the design gate holds this claim; only a tool call written as text is reminded",
        { issue: claim.issue, role: claim.role, wait: waits, hold: answer.hold !== undefined }
      );
    }
    return {
      read: true,
      owes: answer.owes,
      work: answer.work,
      tree: claim.tree,
      quiet,
      hold: answer.hold,
    };
  };

  /** Whether the run has reached the threshold and its architect has not been told yet. */
  const architectDue = (): boolean => !reported && stall.count >= deps.limits.architectAfter;

  /**
   * D6: one Envoy message to the architect that owns this worker, when a delivered reminder
   * brought the count to the threshold. It names the work the daemon answered was owed, or says
   * that the record could not be read, as the reminder did; where that reminder was the one for a
   * tool call written as text that a wait or a hold lets through, it says that the worker's tool
   * calls written as text did not run. `reportKey` is minted at the first
   * attempt of a run of reminders and kept until the count resets, so a publish that timed out
   * after the listener accepted it is not delivered twice, and a failure leaves `reported` false
   * for the next reminder to try again. A kept answer from a listener that names no `keptUntil`
   * clears it early, so the next attempt mints a new one (below). The key also names the run an
   * acceptance belongs to: one that lands after the run ended acknowledges nothing.
   */
<<<<<<< 7fa22de66d
  const tellArchitect = async (due: Due): Promise<void> => {
=======
  const tellArchitect = async (work: string | undefined): Promise<void> => {
    // A run whose claim has ended builds no publisher: its first build can run Envoy's token
    // command, which would trade a broker token for a notice that is not sent.
    if (deps.claim() === undefined) return;
    // The worker's own turn may end the run, or its claim may end, while the publisher is built.
    // What is read after it is the run as it stands, which may need no notice any more.
    const publish = await deps.publisher();
    if (!architectDue()) return;
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
    const claim = deps.claim();
    if (claim === undefined) return;
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
    const work = due.work;
    const owed =
      due.kind === "text-call"
        ? "its tool calls written as text did not run"
        : work === undefined
          ? "its turns keep ending while the daemon's record of what it owes cannot be read"
          : `its turns keep ending with ${work} still owed`;
    try {
      // The architect the daemon named as this claim's owner in its newest owed answer that named
      // one: the issue's own architect while its claim holds its role, else the nearest one above
      // it, else the tree root's. Before any answer has named one, the tree's architect, named
      // from the claim's tree and LEGION_PROJECT as the daemon names that role.
      const topic = roleTopic(owner ?? roleToken(deps.project ?? "", claim.tree, "architect"));
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
      });
      // The publish has no deadline of its own, so the worker's own turn may have reset the count
      // while it was in flight, and a later run may have begun. That run has a key of its own, or
      // none yet: this acceptance is not its notice.
      if (reportKey !== key) return;
      // Another sender's message already holds the key, so the listener stored nothing and told
      // no architect: the key is cleared, so the next attempt mints a new one.
      if ("keyTaken" in answer) {
        reportKey = undefined;
        writeEntry();
        deps.log.warn(
          "legion: another sender's message holds the architect notice's key; telling it again later",
          {
            issue: claim.issue,
            reminders,
            event_id: answer.keyTaken.eventId,
          }
        );
        return;
      }
      // A kept answer (202, `PublishResult.kept`) means that no live session holds the architect
      // role: the listener kept the notice on the role's kept lane for the next session that holds
      // the role live, and its wake report can bring a released architect back. That has told no
      // architect yet, and a copy no session takes in its hour is dropped, so the run's next
      // reminder publishes the notice again. A listener that names `keptUntil` answers a repeat of
      // the key with the copy it still holds, or keeps a new copy once the first one has been
      // forwarded or its hour has ended, so the key stays. A listener of an earlier release names
      // no time and answers a repeat inside its stream's duplicate window as the first copy, which
      // it may have dropped already: the key is cleared, so the next attempt mints a new one.
      if (answer.kept === true) {
        if (answer.keptUntil === undefined) {
          reportKey = undefined;
          writeEntry();
        }
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

  /** Puts `due` in front of the worker as a reminder that counts, and tells the architect once
   * the count reaches the threshold. `lastText` is what the worker said as the turn that earned
   * it settled, which the next reminder is armed with when this one does not reach the worker. */
  const deliver = async (lastText: string, due: Due): Promise<void> => {
    // Counted before it is sent, so a reminder the worker was given is never uncounted: a host
    // whose turn starts inside `sendReminder` saves the count before the turn can move it.
    const uncounted = stall;
    advance({ kind: "reminded" });
    const counted = stall;
    generation += 1;
    const sentAt = generation;
    try {
      await deps.sendReminder(due.text);
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
<<<<<<< 7fa22de66d
    if (reported || stall.count < deps.limits.architectAfter) return;
    await tellArchitect(due);
=======
    if (!architectDue()) return;
    await tellArchitect(work);
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
  };

  /**
   * D4: the one guard. A timer fires only while nothing has happened since it was armed, no turn
   * is running, and the daemon's answer earns a reminder (`dueOf`): not while it says that the
   * claim owes nothing, and, while a wait or a hold stands, only for a tool call written as text.
   * A timer that delivers nothing arms nothing. The check awaits the daemon, so the generation and
   * the turn are read again after it. `lastText` is what the worker said as the turn that armed it
   * settled, which decides whether the reminder leads with the tool call written as text.
   */
  const armReminder = (lastText: string, delayMs: number): void => {
    const armedAt = generation;
    armed = { at: Date.now() + delayMs, generation: armedAt };
    deps.armTimer(async () => {
      if (generation !== armedAt || turnRunning) return;
      const check = await owedCheck();
      if (generation !== armedAt || turnRunning) return;
      const due = dueOf(check, lastText);
      if (due === undefined) return;
      await deliver(lastText, due);
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
      // record holds `awaiting-update`: `recordedPhaseStall` records it as `waiting`, since the
      // relaunch this session resumed on is the update round ending. So a restored `waiting` is
      // that round's; on Claude Code it may also be the worker's own background work, and in a
      // record an earlier build wrote, a WAITING line's. The reminder times each out as it always
      // did. The wait news is reported against, and every report the daemon never answered, come
      // back too: the boot sends those reports again (`armResumeReminder`).
      reported = entry?.reported ?? false;
      reportKey = entry?.reportKey;
      lastMessage = entry?.lastMessage ?? "";
      standing = entry?.waitId;
      unsent = entry?.unsentNews ?? [];
      recorded = JSON.stringify(entryOf());
    },
    armResumeReminder: async () => {
      const startedAt = generation;
      const check = await owedCheck();
      // A turn that started while the daemon was asked arms its own reminder when it settles, as
      // it would make a timer armed before it stale. A wait or a hold that stands arms nothing:
      // the resume reminder has no text of the worker's, so no tool call written as text is due.
      if (generation !== startedAt || turnRunning || (check.read && check.quiet)) return;
      armReminder("", deps.limits.resumeMs);
    },
    turnStarted: () => {
      generation += 1;
      turnRunning = true;
    },
    apply: (input) => {
      advance(input);
    },
    news: async (deliveryKey) => {
      advance({ kind: "news" });
      const waitId = standing;
      if (waitId === undefined || answered.has(waitId)) return;
      // The key goes as the host read it. An empty one is no key, which the route would refuse.
      const key = deliveryKey === "" ? undefined : deliveryKey;
      if (!unsent.some((report) => report.waitId === waitId && report.deliveryKey === key)) {
        unsent = [...unsent, { waitId, ...(key === undefined ? {} : { deliveryKey: key }) }];
        writeEntry();
      }
      await sendNews();
    },
    waitDeclared: (waitId) => {
      declarations += 1;
      standing = waitId;
      // The daemon ended the wait this one replaces at the declaration, so a report kept for it
      // would end nothing now.
      unsent = unsent.filter((report) => report.waitId === waitId);
      writeEntry();
    },
    settle: async (turn) => {
      turnRunning = false;
      generation += 1;
      if (turn.kind === "refused") {
        // The provider refused this turn, so the worker read nothing and said nothing: the state
        // stands, and this settle returns no continuation, because the host would turn it into
        // one more turn at once and the provider would refuse that one too. One timer is armed
        // all the same: the task the refused turn carried ended with it, and nothing else would
        // wake this worker, so a single transient refusal would leave its issue idle. What ends
        // the reminders is the daemon failing the claim, after which the owed check the timer
        // runs reads that the claim owes nothing and drops the reminder: the daemon judges a
        // reminder's own turn, which no prompt of its ran, from its turn-outcome report, as it
        // judges a prompt's turn from that prompt's result (dispatch://INFRA-4508), so a refusal
        // whose status cannot clear (a 4xx other than 401, 403, 408 and 429), or whose report
        // carries `code: "cyber_policy"` (Oh My Pi's own class for OpenAI's cyber-policy refusal,
        // whatever the status), fails the claim at once and holds the issue for its architect,
        // and the next reminder is dropped. Any other refusal, or one whose report carries no
        // status and no code, fails nothing, so the reminders keep this pace for as long as the
        // provider refuses — the whole of dispatch://INFRA-264 — and the daemon sends the
        // `provider-errors` notice once when the worker's turns that ended in a provider error,
        // reminders' and prompts' alike, reach six in a row (`providerErrorRunReached` in the
        // daemon says to whom).
        advance(turn);
        continued = false;
        armReminder("", deps.limits.reminderMs);
        return undefined;
      }
      const text = turn.lastAssistantText;
      lastMessage = text.slice(0, LAST_MESSAGE_LIMIT);
      const before = stall.count;
      const remind = advance(turn);
      // Every outcome but the continuation ends the run here, so the next one may continue again.

      // The wait a `ready_for_update` answer created is the daemon's to end, so nothing is asked
      // and nothing is armed in it: the round replaces the pod, and the task it held behind the
      // round reaches the worker in the process that comes back on the new pod, while a reminder
      // steered in meanwhile starts a turn of work the replacement throws away, and before the
      // round's bound a running turn holds that replacement back (`awaiting-update` in
      // `./phase-stall`).
      if (stall.state === "awaiting-update") {
        continued = false;
        return undefined;
      }
      // Every other settle reads the daemon's answer before it arms anything: a wait the worker
      // declared or the design gate's hold that stands continues nothing and arms nothing, so the
      // activity report says that no reminder is due. The reminder for a tool call written as
      // text is the one either lets through, and it keeps the rhythm every reminder keeps.
      const check = await owedCheck();
      const due = dueOf(check, text);
      if (check.read && check.quiet && due === undefined) {
        continued = false;
        return undefined;
      }
      // A settle that earned no reminder now is one in the quiet wait the worker's own background
      // work buys: the machine sends no reminder into it, so the timer carries the one this
      // settling message earned, for when the wait has been long enough.
      if (!remind) {
        continued = false;
        armReminder(text, deps.limits.waitingMs);
        return undefined;
      }
      // The continuation is the first reminder of a run and the only one of it: the worker has
      // been reminded already (`before`) or has had this run's continuation (`continued`), and
      // the reminder comes from a timer instead. The run then settles, the daemon sees the turn
      // end, and the next reminder comes with its delay.
      if (before !== 0 || continued) {
        continued = false;
        armReminder(text, deps.limits.reminderMs);
        return undefined;
      }
      if (due === undefined) {
        continued = false;
        return undefined;
      }
      advance({ kind: "reminded" });
      generation += 1;
      continued = true;
      return due.text;
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
