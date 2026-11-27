import type { LegionTake, LegionTask } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import type { RelayTaskValue } from "@legion/envoy-client/relay-frame";
import { costCapDrop } from "./cost-cap";
import { type ClaimCaller, type LegionDaemonClient, sendUntilAnswered } from "./daemon-client";
import { takeTaskSignals, taskTurnMark } from "./task-values";

/**
 * A worker's take of its claim's task (dispatch://INFRA-2811 R2). The daemon writes the number of
 * the task its record owes a claim into the claim's key of the Legion task bucket, and the Envoy
 * listener relays each write to the session holding the claim's role. The worker holds the newest
 * value it was handed — by the registration's answer, by a read of `claims/task`, or by a relayed
 * task frame — reports each one to the daemon as seen, whether or not its host is idle, and asks
 * the daemon to take it once its host is idle. The daemon answers from its record: the text to
 * run, a hold that refuses it, or a drop. A new revision is the only signal to ask again: the
 * worker asks once per `(epoch, revision)`, and the pairs it asked at are kept in this process's
 * memory alone, so a relaunched process asks again at the value it is handed, which is the
 * daemon's to answer.
 *
 * The host is idle when no run of its session is going, from a run's `agent_start` to its
 * `agent_end`: a compaction between runs does not make it busy, so a take is made during one, and
 * its text, sent while the compaction runs, waits for it (`run`).
 *
 * From the moment it holds a value it will ask about until the task's turn starts, or the take is
 * refused or dropped, the take holds the task-turn mark (`setPending`), which keeps a card that
 * finds the session idle from starting the turn the take goes into
 * (`packages/pi-envoy/src/card-wake.ts`). It holds the mark as well while its host awaits a read
 * that can bring it a value it does not hold yet (`awaitRead`): the read after the claim's ready,
 * whose decision can write one, so no card starts a turn ahead of that task. The first run that
 * starts after an accepted take's text was sent is that task's turn, as the daemon counts it, and
 * its end is reported naming the task's number (`runEnded`); for a worker on the current contract
 * that report alone finishes the task, so from the end of a turn whose report carries no provider
 * error the take holds the number finished, and a newer revision of it is owed nothing. The
 * report's answer settles that run-end mark: a 2xx finished the number in the record, which
 * deletes its value, so the mark may stay; a 4xx refusal applied nothing and the record still
 * owes the number, so the mark is cleared (`reportRefused`) and a newer revision of it is taken
 * and runs once, as R2's "a number the record owes it". The mark is kept apart from the
 * `finished` a reading says, so its clearing never lowers what a reading said. A value naming the
 * marked number is still reported seen, as every value is; only its take waits.
 *
 * Nothing sends a task into a busy worker (R5). A take the daemon accepts while a run is going —
 * the run started after the daemon decided the take and before its answer came back, or while a
 * take sent again after a lost answer waited — has its text held until that run's `agent_end`,
 * and sent then, so it starts a run of its own. So has a take whose accepted answer finds the host
 * not idle by its own reading though no run is going: the host is setting up a prompt it already
 * took, a card's or a person's, before that prompt's `agent_start`, so `run` answers that it is
 * not idle and hands nothing, and the text waits for the end of the run that setup starts. A run's
 * end that finds the host not idle, a continuation it scheduled following, holds the text on in
 * the same way, to the end of that continuation. The daemon gave the run that was going, or the
 * one the setup started, to the take, as the first to start after its accept; that run's end is
 * reported naming no task and naming the take's number as held (`held`), which puts the take back
 * to awaiting its turn on the daemon (the plan's decisions 8 and 9), also where the daemon's count
 * of the session's turns is not known; and the run the held text starts is then the task's turn on
 * both sides, its end the report that names the number. The text is never sent as a follow-up:
 * Oh My Pi folds a follow-up sent during a run into that run (its agent loop reads the follow-up
 * queue once the run's last step has stopped, `getFollowUpMessages`, and goes on with no
 * `agent_end` or `agent_start` between), so the task would run inside a run whose end names no
 * task, and a follow-up's answer settles once the host has queued it, so a drop of it after that
 * would not be seen (plan review round 71). So every handing of the text settles at the text's own
 * turn or at its drop. A setup the host gives up with no run (an interrupt while it sets the prompt
 * up) ends no run, so the text stays held until the daemon writes the number's value again, which
 * lets the held text go (`hold`): R5's second wait, which charges and takes back an accepted take
 * whose turn has not started within its bound, makes that write.
 *
 * An accepted number's text reaches the host when the take hands it over (`run`), and from then
 * on a newer revision of that number changes nothing: the daemon answers its take with the same
 * text until the task finishes, and the host runs the text once, also when the newer revision
 * comes while the host sets the text up before its `agent_start` (the first prompt's recall can
 * take 30 seconds). Until the text has reached the host, a newer revision of its number is a new
 * offer (the plan's decision 9). A text held through a run the worker did not start was never
 * handed over, so it is let go at that revision, which the take asks about once the host is idle.
 * A text the host dropped before its turn started — `run`'s answer rejects, as Oh My Pi's
 * `pi.sendUserMessage` does for a message it drops before its turn begins, an abort of a prompt
 * the host is setting up among them — leaves the take asking at the number's next revision. The
 * next accepted answer's text goes to the host once, at idle: one that came while the host still
 * held the text it then dropped goes in that text's place at the drop. The daemon answers such a
 * take afresh where it took the task back (a provider error's ladder, R5's second wait) and with
 * the same text where it only wrote the value again. A compaction the daemon asks for takes no
 * task back: it is Oh My Pi's handoff, which aborts no prompt and parks a text it receives while
 * the handoff runs until the handoff is over.
 *
 * A cost-cap drop is different. The take reports it as soon as the host rejects the text.
 * It asks for no next take until that report is answered. The daemon then offers the task at
 * a new revision after the provider-error wait.
 *
 * A seen report or a take is sent through `sendUntilAnswered` (`./daemon-client`): one the daemon
 * gave no answer to — any failure that carries no HTTP status, whatever the transport names it (a
 * refused, reset or closed connection, an unreachable host or network, a timeout, a DNS failure
 * among them), no answer within `DAEMON_ANSWER_TIMEOUT_MS`, or an outage (a 408, a 429 or any
 * 5xx: `outage` of `@legion/contracts/http-status`) whose body is not the daemon's refusal shape,
 * such as a proxy's page — is sent again, the same request, after a doubling wait with no attempt
 * limit, until the daemon answers it or a newer value replaces it: a take the daemon never
 * received would otherwise be lost, since the worker asks once per revision. Such a re-send is the
 * same ask, not a second one, and the daemon answers a take repeated for the number it accepted
 * with the same text until that task finishes, whether or not its turn has started.
 * Every refusal of these routes is in the daemon's refusal shape, so it is an answer and is not
 * sent again.
 *
 * A take its host replaced (`close`) does nothing more: what it was sending stops, an answer that
 * still arrives for it is dropped, and its task-turn mark is cleared.
 */

/** A claim's task value: the task's number, and the bucket's epoch and revision of the write that
 * put it there. */
export type TaskValue = NonNullable<LegionTask["task"]>;

export interface TaskTakeLog {
  readonly info: (message: string, fields?: Record<string, unknown>) => void;
  readonly warn: (message: string, fields?: Record<string, unknown>) => void;
}

export interface TaskTakeDeps {
  readonly daemon: Pick<LegionDaemonClient, "taskSeen" | "take">;
  readonly caller: ClaimCaller;
  /**
   * Runs an accepted take's text as the turn's user message, in the form that starts a turn on
   * an idle host and waits behind a compaction that runs, and never as a follow-up. Its answer,
   * where the host gives one, rejects when the host dropped the text before its turn started, as
   * Oh My Pi's `pi.sendUserMessage` does (`PiApi.sendUserMessage`). A host that is not idle by its
   * own reading — one setting up a prompt it already took, before that prompt's `agent_start` —
   * is handed nothing: `run` answers `"not idle"`, and the take holds the text until the end of
   * the run that setup starts.
   */
  readonly run: (text: string) => "not idle" | void | Promise<void>;
  /** Reports a cost-cap drop before any turn starts. Resolves when sending ends. */
  readonly reportDrop?:
    | ((task: number, code: "cost_cap", message: string) => Promise<void>)
    | undefined;
  /** Sets the task-turn mark for the claim's session, or clears it. */
  readonly setPending: (pending: boolean) => void;
  readonly log: TaskTakeLog;
  /**
   * Resolves `true` once `ms` have passed, or `false` as soon as `signal` aborts: the wait between
   * a request's attempts, and each attempt's bound. A timer by default; a test passes its own
   * clock.
   */
  readonly wait?: (ms: number, signal: AbortSignal) => Promise<boolean>;
}

export interface TaskTake {
  /** The daemon's reading of the claim's value: the registration's answer, or a `claims/task`
   * read right after the claim's ready, at a new epoch of the relay, or at a regain of the role. */
  readonly read: (reading: LegionTask) => void;
  /** A value a task frame relayed; one for another claim is not this take's. */
  readonly relayed: (value: RelayTaskValue) => void;
  /** The claim's ready has answered: from now on a take is made once the host is idle. Before
   * it, the daemon refuses a take as not ready. */
  readonly ready: () => void;
  /**
   * The host awaits a read of the claim's value that can bring one this take does not hold yet:
   * the read after the claim's ready, whose decision can write a value (the relaunch record's
   * task) the registration did not carry (the plan's decision 15). From this call until the
   * returned function runs, which the host calls once that read has answered — its reading handed
   * to `read`, or its failure logged —, the take holds the task-turn mark whatever value it holds,
   * so a card that finds the host idle in that window, the session's own direct message included,
   * starts no turn ahead of the task's. From then on the mark follows the take's own rule again:
   * it stays while the take holds a value it will ask about, and is cleared where it holds none.
   * The returned function does nothing a second time.
   */
  readonly awaitRead: () => () => void;
  /** A run of the claim's session started (its `agent_start`). */
  readonly runStarted: () => void;
  /**
   * The number of the accepted take whose text this take holds unrun, for the end of the run
   * going (or of the one the host was setting up when the answer came), and undefined for none:
   * read at a run's end before `runEnded`, which hands the text on, it says the run ending was not
   * that task's turn. The run's report names it as `held` (`packages/pi-legion/src/turn-outcome.ts`),
   * which tells the daemon so for certain, even where it has lost its count of the session's turns
   * (a daemon restarted under the process), and puts the take back to await its own turn there.
   * A text handed to the host, or let go at a newer revision of its number, is held no more.
   */
  readonly held: () => number | undefined;
  /**
   * That run ended (its `agent_end`; a refusal the host goes on from, a step of its own retry or
   * model-fallback walk, ends no run, and `packages/pi-legion/extensions/legion.ts` calls nothing
   * for it): the number of the task whose turn it was, which the turn's report names, or
   * undefined for a run of no task. `providerError` is whether that report carries the
   * provider's failure (`reportsProviderError` in `packages/pi-legion/src/turn-outcome.ts`).
   * The daemon finishes a current claim's task on a report naming its number with no provider
   * error, so from such a run's end the take holds that number finished, beside a reading's
   * `finished`, until the daemon refuses that report (`reportRefused`): a newer revision of the
   * same number, which the daemon may write before the report lands (a daemon start writes every
   * owed value again, and the daemon writes the value again from its record where the bucket lost
   * it or holds another write) and would answer with the same text while the task is unfinished,
   * is reported seen, owed nothing and runs nothing a second time. A report with a provider error
   * finishes nothing: the daemon takes the task back, and the take takes that number again at
   * the revision the daemon writes after its wait.
   */
  readonly runEnded: (providerError: boolean) => number | undefined;
  /**
   * The daemon refused with a 4xx the report of a turn of task `number` that `runEnded` held
   * finished, one whose report carried no provider error: that report applied nothing and the
   * record still owes the number, so the take holds it finished no longer, and asks about a newer
   * revision of it, one it holds already or one the daemon writes later, which then runs once. A
   * number a reading said finished stays finished, and a mark that has moved on to a later number
   * since stays. A report the daemon answered with a 2xx finished the number in the record, which
   * deletes its value, so nothing is told of it.
   */
  readonly reportRefused: (number: number) => void;
  /** The take is replaced by another of the same host: every request it is sending stops, an
   * answer that still arrives for one changes nothing, its task-turn mark is cleared, and every
   * call after this one does nothing. */
  readonly close: () => void;
}

/** One request being sent until it is answered: the pair it names, and what replaces it. */
interface Sending {
  readonly key: string;
  readonly stop: AbortController;
}

/**
 * Where the take the daemon last accepted stands: none; its text held until the run going when
 * it was accepted, or the one the host was setting up then, has ended on an idle host (`during`
 * is the number of the task whose turn that run is, where it is one's); its text handed to the
 * host, its turn not started; or its turn running. A handed text carries the count of its
 * hand-over (`handing`), so a drop of an earlier one changes nothing, and `again`, the text of an
 * accepted answer to a take at a newer revision of its number that came while the host held the
 * text, which goes to the host in its place should the host drop it.
 */
type Accepted =
  | { readonly kind: "none" }
  | {
      readonly kind: "held";
      readonly number: number;
      readonly text: string;
      readonly during: number | undefined;
    }
  | {
      readonly kind: "handed";
      readonly number: number;
      readonly handing: number;
      readonly again: string | undefined;
    }
  | { readonly kind: "running"; readonly number: number };

const NONE: Accepted = { kind: "none" };

/** The number of the task whose turn is running. */
const turnRunning = (state: Accepted): number | undefined =>
  state.kind === "running" ? state.number : state.kind === "held" ? state.during : undefined;

const keyOf = (value: { readonly epoch: string; readonly revision: number }): string =>
  `${value.epoch} ${value.revision}`;

export function createTaskTake(deps: TaskTakeDeps): TaskTake {
  const { caller, log } = deps;
  // The newest value this process was handed; the number of the last task the daemon's readings
  // said the claim finished; and the run-end mark, the number of the last task whose turn ended
  // here with no provider error, which that turn's report finishes on the daemon, kept apart from
  // the readings' number so that a refused report clears the mark alone (`reportRefused`). A value
  // naming a number at or below either is owed nothing.
  let held: TaskValue | undefined;
  let finished = 0;
  let finishing = 0;
  // The pairs this process reported seen and asked at, in its memory alone.
  const seen = new Set<string>();
  const asked = new Set<string>();
  let armed = false;
  let busy = false;
  // Where the take the daemon last accepted stands (`Accepted`), and the count of its hand-overs
  // (`hand`).
  let accepted: Accepted = NONE;
  let handings = 0;
  let reportingDrop = false;
  let seeing: Sending | undefined;
  let taking: Sending | undefined;
  let marked = false;
  // The reads the host awaits (`awaitRead`) that have not answered yet.
  let reading = 0;
  let closed = false;

  const owed = (value: TaskValue | undefined): value is TaskValue =>
    value !== undefined && value.number > Math.max(finished, finishing);

  const request = (value: TaskValue) => ({
    ...caller,
    number: value.number,
    epoch: value.epoch,
    revision: value.revision,
  });

  const reportSeen = (value: TaskValue): void => {
    const key = keyOf(value);
    if (seen.has(key)) return;
    seen.add(key);
    const sending: Sending = { key, stop: new AbortController() };
    seeing = sending;
    sendUntilAnswered(
      "claims/task-seen",
      (signal) => deps.daemon.taskSeen(request(value), { signal }),
      { stop: sending.stop.signal, warn: log.warn, wait: deps.wait }
    ).then(
      () => {
        if (seeing === sending) seeing = undefined;
      },
      (error: unknown) => {
        if (seeing !== sending) return;
        seeing = undefined;
        log.warn("legion: the daemon refused a task value's seen report", {
          task: value.number,
          revision: value.revision,
          error: messageFor(error),
        });
      }
    );
  };

  /**
   * The host dropped the text of hand-over `handing` before that text's turn started: the text
   * never reached it, so a newer revision of its number is a new offer. The text of an accepted
   * answer at such a revision that came while the host held the dropped one goes to the host now,
   * once; with none, the take asks at the number's next revision, and the text its accepted answer
   * carries goes to the host.
   */
  const hostDropped = (handing: number, error: unknown): void => {
    if (closed || accepted.kind !== "handed" || accepted.handing !== handing) return;
    const { number, again } = accepted;
    log.warn("legion: the host dropped an accepted take's text before its turn started", {
      task: number,
      error: messageFor(error),
    });
    const cap = costCapDrop(error);
    if (cap !== undefined && deps.reportDrop !== undefined) {
      accepted = NONE;
      reportingDrop = true;
      // A newer revision can arrive before the report's answer. Do not ask for it until then.
      taking?.stop.abort();
      taking = undefined;
      const report = deps.reportDrop;
      const send = async (): Promise<void> => {
        try {
          await report(number, "cost_cap", cap);
        } catch (error) {
          log.warn("legion: could not report a take dropped at the cost cap", {
            task: number,
            error: messageFor(error),
          });
        } finally {
          reportingDrop = false;
          if (!closed) settle();
        }
      };
      void send();
      settle();
      return;
    }
    if (again === undefined) accepted = NONE;
    else hand(number, again);
    settle();
  };

  /** Hands an accepted take's text to the host (`run`), when no run is going: from here it has
   *  reached the host, unless the host's answer to it rejects before the text's turn starts
   *  (`hostDropped`). A host that answers it is not idle, one setting up a prompt it already took,
   *  took nothing: the text is held as one accepted during a run is, and goes to the host at the
   *  end of the run that setup starts (`runEnded`). */
  const hand = (number: number, text: string): void => {
    handings += 1;
    const handing = handings;
    accepted = { kind: "handed", number, handing, again: undefined };
    const answer = deps.run(text);
    if (answer === "not idle") {
      accepted = { kind: "held", number, text, during: undefined };
      return;
    }
    Promise.resolve(answer).then(undefined, (error: unknown) => hostDropped(handing, error));
  };

  const answered = (value: TaskValue, answer: LegionTake): void => {
    log.info("legion: the daemon answered a take", {
      task: value.number,
      revision: value.revision,
      outcome: answer.outcome,
      ...(answer.outcome === "refused" ? { hold: answer.hold } : {}),
    });
    switch (answer.outcome) {
      case "accepted":
        // The daemon answers a take of the number it accepted with the same text until that task
        // finishes, whether or not its turn has started: a take sent again at a newer revision
        // of a number whose text has reached the host, before the task's turn (while the host
        // sets the text up), during it, or after its end was reported with no provider error
        // (`runEnded`) and until that report is refused, runs nothing more. While the host holds
        // the text unstarted, the answer is kept for the host's drop of it (`hostDropped`).
        if (!owed(value) || turnRunning(accepted) === value.number) return;
        if (accepted.kind === "handed" && accepted.number === value.number) {
          accepted = { ...accepted, again: answer.text };
          return;
        }
        // Nothing goes into a busy worker: a text accepted while a run is going waits for that
        // run's end and starts a run of its own then (`runEnded`), and that run is the task's
        // turn. Sent now, it goes to a host no run of which is going (`hand`), which holds it all
        // the same where the host is setting a prompt up.
        if (busy) {
          accepted = {
            kind: "held",
            number: value.number,
            text: answer.text,
            during: turnRunning(accepted),
          };
        } else {
          hand(value.number, answer.text);
        }
        return;
      case "dropped":
        if (held?.number === value.number) held = undefined;
        return;
      case "refused":
        return;
    }
  };

  const ask = (value: TaskValue, key: string): void => {
    const sending: Sending = { key, stop: new AbortController() };
    taking = sending;
    sendUntilAnswered("claims/take", (signal) => deps.daemon.take(request(value), { signal }), {
      stop: sending.stop.signal,
      warn: log.warn,
      wait: deps.wait,
    }).then(
      (answer) => {
        if (taking !== sending) return;
        taking = undefined;
        answered(value, answer);
        settle();
      },
      (error: unknown) => {
        if (taking !== sending) return;
        taking = undefined;
        log.warn(
          "legion: the daemon refused a take; it is asked again at the value's next revision",
          {
            task: value.number,
            revision: value.revision,
            error: messageFor(error),
          }
        );
        settle();
      }
    );
  };

  /** Asks about the held value where it may, and keeps the task-turn mark to what is pending. */
  const settle = (): void => {
    const key = held === undefined ? undefined : keyOf(held);
    if (armed && !busy && !reportingDrop && owed(held) && key !== undefined && !asked.has(key)) {
      asked.add(key);
      ask(held, key);
    }
    const pending =
      reading > 0 ||
      (owed(held) && key !== undefined && !asked.has(key)) ||
      reportingDrop ||
      taking !== undefined ||
      accepted.kind === "held" ||
      accepted.kind === "handed";
    if (pending === marked) return;
    marked = pending;
    deps.setPending(pending);
  };

  /** A request for a pair other than `key`'s is replaced: it is sent no more. */
  const replace = (key: string | undefined): void => {
    if (seeing !== undefined && seeing.key !== key) {
      seeing.stop.abort();
      seeing = undefined;
    }
    if (taking !== undefined && taking.key !== key) {
      taking.stop.abort();
      taking = undefined;
    }
  };

  const hold = (value: TaskValue): void => {
    if (held !== undefined && held.epoch === value.epoch && value.revision <= held.revision) {
      return;
    }
    held = value;
    // A newer write of the number whose accepted text is held, a text never handed to the host, is
    // a new offer: the held text is let go, and the take at this value, made once the host is idle,
    // brings the text that runs. The run going keeps its own task, if it is one's.
    if (accepted.kind === "held" && accepted.number === value.number) {
      accepted =
        accepted.during === undefined ? NONE : { kind: "running", number: accepted.during };
    }
    replace(keyOf(value));
    reportSeen(value);
  };

  return {
    read: (reading) => {
      if (closed) return;
      finished = Math.max(finished, reading.finished);
      // The value the reading names, field by field, as `relayed` builds one from a relayed write.
      const task = reading.task;
      if (task !== null) hold({ number: task.number, epoch: task.epoch, revision: task.revision });
      settle();
    },
    relayed: (value) => {
      if (closed || value.claim !== caller.claimToken) return;
      const at = { epoch: value.epoch, revision: value.revision };
      if (value.op === "put") {
        hold({ number: value.task, ...at });
      } else if (
        held !== undefined &&
        (held.epoch !== value.epoch || held.revision < value.revision)
      ) {
        // The claim has no current task from this revision on: the record dropped or finished it.
        held = undefined;
        replace(undefined);
      }
      settle();
    },
    ready: () => {
      if (closed) return;
      armed = true;
      settle();
    },
    awaitRead: () => {
      if (closed) return () => undefined;
      reading += 1;
      settle();
      let answered = false;
      return () => {
        if (answered) return;
        answered = true;
        reading -= 1;
        if (!closed) settle();
      };
    },
    runStarted: () => {
      if (closed) return;
      busy = true;
      // The first run that starts after an accepted take's text was handed to an idle host is the
      // task's turn. A text still held waits for the end of a run that was going when it was
      // accepted, or that the host was setting up then (`hand`), so a run that starts before that
      // end is not its turn: the daemon counts that run as the take's, and its end, reported
      // naming no task and the text held (`held`), puts the take back.
      if (accepted.kind === "handed") accepted = { kind: "running", number: accepted.number };
      settle();
    },
    held: () => (closed || accepted.kind !== "held" ? undefined : accepted.number),
    runEnded: (providerError) => {
      if (closed) return undefined;
      busy = false;
      const ended = turnRunning(accepted);
      // That run's report names the task with no provider error, which finishes it on the
      // daemon: the number is owed nothing from here on, before the report has even landed, and
      // until the daemon refuses it (`reportRefused`).
      if (ended !== undefined && !providerError) finishing = Math.max(finishing, ended);
      // A text accepted during the run that just ended, or while the host set that run's prompt
      // up: that run was not the task's turn, so its end names no task (`ended` is that run's own
      // task, if any), and the text starts the next run, the task's, or waits for that next run's
      // end too where the host is still not idle here (`hand`).
      if (accepted.kind === "held") {
        hand(accepted.number, accepted.text);
      } else if (accepted.kind === "running") {
        accepted = NONE;
      }
      settle();
      return ended;
    },
    reportRefused: (number) => {
      if (closed || finishing !== number) return;
      finishing = 0;
      settle();
    },
    close: () => {
      if (closed) return;
      closed = true;
      replace(undefined);
      if (!marked) return;
      marked = false;
      deps.setPending(false);
    },
  };
}

/** A claim's take as a host runs it (`takeClaimTask`): the take, and its read of the claim's value
 * from the daemon. */
export interface ClaimTaskTake {
  readonly take: TaskTake;
  /** Reads the claim's value from the daemon (`claims/task`) and hands it to the take. A read that
   * fails is logged and asks nothing again, since the daemon's next write of the value reaches
   * the session as well. Never rejects. */
  readonly read: () => Promise<void>;
}

/**
 * The take of the claim `caller` names, as an Oh My Pi host runs it, the one way pi-legion's claim
 * session and its daemon-launched controller build it: the take (`createTaskTake`), whose
 * task-turn mark names the caller's session (`taskTurnMark`); its read of the claim's value; and
 * the one taker of the task signals the Envoy entry hands over (`takeTaskSignals`), which hands
 * the take every value relayed for that session and reads the value again at each new epoch of
 * the session's relay. A later call replaces this one's taker: a host that replaces its take
 * closes the one it replaced (`TaskTake.close`).
 */
export function takeClaimTask(deps: {
  readonly daemon: Pick<LegionDaemonClient, "task" | "taskSeen" | "take">;
  readonly caller: ClaimCaller;
  readonly run: TaskTakeDeps["run"];
  readonly reportDrop: TaskTakeDeps["reportDrop"];
  readonly log: TaskTakeLog;
}): ClaimTaskTake {
  const { daemon, caller, log } = deps;
  const take = createTaskTake({
    daemon,
    caller,
    run: deps.run,
    reportDrop: deps.reportDrop,
    setPending: taskTurnMark(caller.sessionId),
    log,
  });
  const read = async (): Promise<void> => {
    try {
      take.read(await daemon.task(caller));
    } catch (error) {
      log.warn(
        "legion: a claims/task read failed; the claim's value reaches this session with the daemon's next write of it",
        { error: messageFor(error) }
      );
    }
  };
  takeTaskSignals((signal) => {
    if (signal.session !== caller.sessionId) return;
    if (signal.kind === "value") take.relayed(signal.value);
    else void read();
  });
  return { take, read };
}
