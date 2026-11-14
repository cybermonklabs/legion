import type { LegionTake, LegionTask } from "@legion/contracts/legion-api";
import type { RelayTaskValue } from "@legion/envoy-client/consumer-delivery";
import { messageFor } from "@legion/envoy-client/errors";
import { LegionDaemonApiError, type LegionDaemonClient } from "./daemon-client";

/**
 * A worker's take of its claim's task (dispatch://INFRA-2811 R2). The daemon pushes no task: it
 * writes the number of the task its record owes a claim into the claim's key of the Legion task
 * bucket, and the Envoy listener relays each write to the session holding the claim's role. The
 * worker holds the newest value it was handed — by the registration's answer, by a read of
 * `claims/task`, or by a relayed task frame — reports each one to the daemon as seen, whether or
 * not its host is idle, and asks the daemon to take it once its host is idle. The daemon answers
 * from its record: the text to run, a hold that refuses it, or a drop. A new revision is the only
 * signal to ask again: the worker asks once per `(epoch, revision)`, and the pairs it asked at are
 * kept in this process's memory alone, so a relaunched process asks again at the value it is
 * handed, which is the daemon's to answer.
 *
 * The host is idle when no run of its session is going, from a run's `agent_start` to its
 * `agent_end`: a compaction between runs does not make it busy, so a take is made during one, and
 * its text, sent while the compaction runs, waits for it (`run`).
 *
 * From the moment it holds a value it will ask about until the task's turn starts, or the take is
 * refused or dropped, the take holds the task-turn mark (`setPending`), which keeps a card that
 * finds the session idle from starting the turn the take goes into
 * (`packages/pi-envoy/src/card-wake.ts`). The first run that starts after an accepted take is that
 * task's turn, as the daemon counts it, and its end is reported naming the task's number
 * (`runEnded`).
 *
 * A take the daemon accepts while a run is going — the run started before the daemon heard of it
 * — has its text held until that run's `agent_end`, and sent then, so it starts a run of its own.
 * Oh My Pi folds a follow-up sent during a run into that run: its agent loop reads the follow-up
 * queue once the run's last step has stopped (`getFollowUpMessages`) and goes on in the same run,
 * with no `agent_end` or `agent_start` between, so a text sent then would run inside a run that
 * started before the take and is labelled as no task's.
 *
 * A seen report or a take that got no answer — a refused, reset or closed connection, a DNS
 * failure, no answer within `TASK_ANSWER_TIMEOUT_MS`, or a 502, 503 or 504 that names no Legion
 * error code — is sent again, the same request, after a wait that doubles from
 * `TASK_RESEND_FIRST_DELAY_MS` to `TASK_RESEND_MAX_DELAY_MS`, with no attempt limit, until the
 * daemon answers it or a newer value replaces it (departure 7 of INFRA-2811's plan): a take the
 * daemon never received would otherwise be lost, since the worker asks once per revision. Such a
 * re-send is the same ask, not a second one, and the daemon answers a take repeated for a number
 * it accepted with the same text. Every refusal of these routes names a code, so a coded refusal
 * is an answer and is not sent again.
 */

/** How long one attempt of a seen report or a take waits for the daemon's answer before the same
 * request is sent again. */
export const TASK_ANSWER_TIMEOUT_MS = 5 * 60_000;

/** The first wait before a seen report or a take with no answer is sent again; each wait after it
 * doubles, up to `TASK_RESEND_MAX_DELAY_MS`, and nothing limits the attempts. */
export const TASK_RESEND_FIRST_DELAY_MS = 500;
export const TASK_RESEND_MAX_DELAY_MS = 30_000;

/** A claim's task value: the task's number, and the bucket's epoch and revision of the write that
 * put it there. */
export type TaskValue = NonNullable<LegionTask["task"]>;

/** The claim a take speaks for, in the agent form of the task routes. */
export interface TaskTakeCaller {
  readonly claimToken: string;
  readonly sessionId: string;
  readonly secret: string;
  readonly generation: number;
}

export interface TaskTakeLog {
  readonly info: (message: string, fields?: Record<string, unknown>) => void;
  readonly warn: (message: string, fields?: Record<string, unknown>) => void;
}

export interface TaskTakeDeps {
  readonly daemon: Pick<LegionDaemonClient, "taskSeen" | "take">;
  readonly caller: TaskTakeCaller;
  /** Runs an accepted take's text as the turn's user message, in the form that starts a turn on
   * an idle host and waits behind a setup or a compaction that runs. */
  readonly run: (text: string) => void;
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
   * read at a new epoch of the relay or a regain of the role. */
  readonly read: (reading: Pick<LegionTask, "task" | "finished">) => void;
  /** A value a task frame relayed; one for another claim is not this take's. */
  readonly relayed: (value: RelayTaskValue) => void;
  /** The claim's ready has answered: from now on a take is made once the host is idle. Before
   * it, the daemon refuses a take as not ready. */
  readonly ready: () => void;
  /** A run of the claim's session started (its `agent_start`). */
  readonly runStarted: () => void;
  /** That run ended (its `agent_end`): the number of the task whose turn it was, which the turn's
   * report names, or undefined for a run of no task. */
  readonly runEnded: () => number | undefined;
}

/** An attempt the daemon did not answer within `TASK_ANSWER_TIMEOUT_MS`. */
class TaskAnswerTimeout extends Error {
  constructor(what: string) {
    super(`${what} had no answer within ${TASK_ANSWER_TIMEOUT_MS / 60_000} minutes`);
    this.name = "TaskAnswerTimeout";
  }
}

/** The error codes of a request that reached no daemon answer: a refused, reset or closed
 * connection (Bun's own names and Node's), and a DNS failure. */
const UNANSWERED_CODES: Readonly<Record<string, true>> = {
  ConnectionRefused: true,
  ECONNREFUSED: true,
  ConnectionClosed: true,
  ECONNRESET: true,
  EPIPE: true,
  ENOTFOUND: true,
  EAI_AGAIN: true,
};

/** Whether `error` is an attempt the daemon gave no answer to, which is sent again: every
 * failure departure 7 names, and nothing the daemon itself answered. */
function unanswered(error: unknown): boolean {
  if (error instanceof TaskAnswerTimeout) return true;
  if (error instanceof LegionDaemonApiError) {
    return [502, 503, 504].includes(error.status) && error.code === undefined;
  }
  if (!(error instanceof Error)) return false;
  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  if (code !== undefined && UNANSWERED_CODES[code] === true) return true;
  return (
    error instanceof TypeError &&
    (error.message === "fetch failed" || error.message === "Failed to fetch")
  );
}

function timerWait(ms: number, signal: AbortSignal): Promise<boolean> {
  const { promise, resolve } = Promise.withResolvers<boolean>();
  if (signal.aborted) {
    resolve(false);
    return promise;
  }
  const stop = () => {
    clearTimeout(timer);
    resolve(false);
  };
  const timer = setTimeout(() => {
    signal.removeEventListener("abort", stop);
    resolve(true);
  }, ms);
  timer.unref?.();
  signal.addEventListener("abort", stop, { once: true });
  return promise;
}

/** One request being sent until it is answered: the pair it names, and what replaces it. */
interface Sending {
  readonly key: string;
  readonly stop: AbortController;
}

const keyOf = (value: { readonly epoch: string; readonly revision: number }): string =>
  `${value.epoch} ${value.revision}`;

export function createTaskTake(deps: TaskTakeDeps): TaskTake {
  const { caller, log } = deps;
  const wait = deps.wait ?? timerWait;
  // The newest value this process was handed, and the number of the last task the daemon said
  // the claim finished: a value naming that number or a lower one is owed nothing.
  let held: TaskValue | undefined;
  let finished = 0;
  // The pairs this process reported seen and asked at, in its memory alone.
  const seen = new Set<string>();
  const asked = new Set<string>();
  let armed = false;
  let busy = false;
  // The number of the take the daemon accepted whose turn has not started, and of the task whose
  // turn is running; and the text of an accepted take held until the run going when it was
  // accepted has ended.
  let accepted: number | undefined;
  let running: number | undefined;
  let heldText: string | undefined;
  let seeing: Sending | undefined;
  let taking: Sending | undefined;
  let marked = false;

  const owed = (value: TaskValue | undefined): value is TaskValue =>
    value !== undefined && value.number > finished;

  /**
   * Sends `call` until the daemon answers it, or `stop` aborts: each attempt bounded by
   * `TASK_ANSWER_TIMEOUT_MS`, and one the daemon did not answer sent again after a doubling wait.
   * A failure the daemon answered, and the stop, reject.
   */
  const untilAnswered = async <T>(
    what: string,
    stop: AbortSignal,
    call: (signal: AbortSignal) => Promise<T>
  ): Promise<T> => {
    let delay = TASK_RESEND_FIRST_DELAY_MS;
    let told = false;
    for (;;) {
      stop.throwIfAborted();
      const attempt = new AbortController();
      const forward = () => attempt.abort(stop.reason);
      stop.addEventListener("abort", forward, { once: true });
      void wait(TASK_ANSWER_TIMEOUT_MS, attempt.signal).then((passed) => {
        if (passed) attempt.abort(new TaskAnswerTimeout(what));
      });
      try {
        return await call(attempt.signal);
      } catch (error) {
        stop.throwIfAborted();
        if (!unanswered(error)) throw error;
        if (!told) {
          told = true;
          log.warn(
            `legion: ${what} got no answer from the daemon; it is sent again until it does`,
            {
              error: messageFor(error),
            }
          );
        }
      } finally {
        stop.removeEventListener("abort", forward);
        attempt.abort();
      }
      await wait(delay, stop);
      delay = Math.min(delay * 2, TASK_RESEND_MAX_DELAY_MS);
    }
  };

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
    untilAnswered("claims/task-seen", sending.stop.signal, (signal) =>
      deps.daemon.taskSeen(request(value), { signal })
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

  const answered = (value: TaskValue, answer: LegionTake): void => {
    log.info("legion: the daemon answered a take", {
      task: value.number,
      revision: value.revision,
      outcome: answer.outcome,
      ...(answer.outcome === "refused" ? { hold: answer.hold } : {}),
    });
    switch (answer.outcome) {
      case "accepted":
        // A take sent again for a number already accepted, whose turn has not started, is
        // answered the same text: the text that went for the first one runs once.
        if (accepted === value.number) return;
        accepted = value.number;
        // A follow-up sent during a run is folded into that run, so the text waits for its end
        // and starts a run of its own then (`runEnded`).
        if (busy) heldText = answer.text;
        else deps.run(answer.text);
        return;
      case "dropped":
        if (held?.number === value.number) held = undefined;
        return;
      case "refused":
        return;
    }
  };

  const ask = (value: TaskValue): void => {
    const sending: Sending = { key: keyOf(value), stop: new AbortController() };
    taking = sending;
    untilAnswered("claims/take", sending.stop.signal, (signal) =>
      deps.daemon.take(request(value), { signal })
    ).then(
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
    if (armed && !busy && owed(held) && !asked.has(keyOf(held))) {
      asked.add(keyOf(held));
      ask(held);
    }
    const pending =
      (owed(held) && !asked.has(keyOf(held))) || taking !== undefined || accepted !== undefined;
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
    replace(keyOf(value));
    reportSeen(value);
  };

  return {
    read: (reading) => {
      finished = Math.max(finished, reading.finished);
      if (reading.task !== null) hold(reading.task);
      settle();
    },
    relayed: (value) => {
      if (value.claim !== caller.claimToken) return;
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
      armed = true;
      settle();
    },
    runStarted: () => {
      busy = true;
      if (accepted !== undefined && heldText === undefined) {
        running = accepted;
        accepted = undefined;
      }
      settle();
    },
    runEnded: () => {
      busy = false;
      const ended = running;
      running = undefined;
      if (heldText !== undefined) {
        const text = heldText;
        heldText = undefined;
        deps.run(text);
      }
      settle();
      return ended;
    },
  };
}
