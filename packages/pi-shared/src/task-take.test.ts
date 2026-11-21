import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  type LegionTake,
  LegionTakeResponse,
  LegionTaskResponse,
} from "@legion/contracts/legion-api";
import type { RelayTaskValue } from "@legion/envoy-client/relay-frame";
import {
  type ClaimCaller,
  DAEMON_ANSWER_TIMEOUT_MS,
  DAEMON_RESTART_FIRST_DELAY_MS,
  DAEMON_RESTART_MAX_DELAY_MS,
  LegionDaemonApiError,
} from "./daemon-client";
import { createTaskTake, IN_TURN_REWRITE_BOUND_MS } from "./task-take";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): unknown {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

// The daemon's answers, as its golden tests wrote them and the client reads them.
const ACCEPTED = LegionTakeResponse.parse(daemonFixture("take-accepted.json"));
const REFUSED = LegionTakeResponse.parse(daemonFixture("take-refused.json"));
const DROPPED = LegionTakeResponse.parse(daemonFixture("take-dropped.json"));
const NOTHING_HELD = LegionTaskResponse.parse(daemonFixture("task.json"));
const TEXT = ACCEPTED.outcome === "accepted" ? ACCEPTED.text : "";
// The daemon's answer to a take it decided afresh after it took the task back: its text can differ
// from the first accept's, since what that take told goes back to untold with it.
const AFRESH_TEXT = `${TEXT} (taken again)`;
const ACCEPTED_AFRESH = LegionTakeResponse.parse({ outcome: "accepted", text: AFRESH_TEXT });
/** How Oh My Pi's `pi.sendUserMessage` rejects a message it drops before its turn begins. */
const DROP = new Error("Prompt dropped before provider dispatch.");

const caller: ClaimCaller = {
  claimToken: "legion-legion-legion-209-implementer",
  sessionId: "ses_implementer_209",
  secret: "U2VjcmV0",
  generation: 3,
};
const EPOCH = "2026-11-13T16:21:44.123456789Z";

/** A value the daemon read or relayed: task `number` at `revision` of the bucket's epoch. */
const value = (number: number, revision: number, epoch = EPOCH) => ({ number, epoch, revision });

/** A task frame the listener relayed for this claim. */
const put = (number: number, revision: number, claim = caller.claimToken): RelayTaskValue => ({
  claim,
  epoch: EPOCH,
  revision,
  op: "put",
  task: number,
});

/** What one attempt of a request meets: the daemon's answer, a failure, no answer at all, in
 * which case the attempt waits until its bound aborts it, or an answer that arrives when `late`
 * settles, whether or not the attempt was aborted before. */
type Attempt<T> =
  | { readonly answer: T }
  | { readonly fail: Error }
  | { readonly silent: true }
  | { readonly late: Promise<T> };

interface Call {
  readonly route: "task-seen" | "take";
  readonly body: Record<string, unknown>;
}

/** Lets every promise queued so far settle. */
async function flush(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/**
 * The take over a fake daemon client and a fake clock. The daemon answers each take with the next
 * attempt `takes` holds (a refusal, by default, as a hold would answer it), and each seen report
 * with the next `seens` holds (204, by default). `advance` moves the clock, firing every wait it
 * passes; a wait that fired stops listening for its abort, as `timerWait` does, so a later abort
 * of its signal takes no other wait off the clock. `waits` is every wait the take made other than
 * an attempt's own bound, in the order they were made: the wait between two attempts of one
 * request, and the bound of a take refused `in-turn`. `handed` is the host's answer to each text
 * the take handed it, in the order of `ran`, which a case rejects where the host dropped that
 * text. `host` is the host's own idle reading at each hand-over: a case sets `idle` false while
 * the host sets up a prompt it already took (a card's or a person's, before its `agent_start`),
 * for which `run` answers that the host is not idle and hands nothing, as pi-legion's `runTask`
 * does.
 */
function rig() {
  const calls: Call[] = [];
  const takes: Attempt<LegionTake>[] = [];
  const seens: Attempt<undefined>[] = [];
  const ran: string[] = [];
  const handed: { readonly reject: (reason: unknown) => void }[] = [];
  const marks: boolean[] = [];
  const warned: string[] = [];
  const waits: number[] = [];
  let now = 0;
  const timers: { readonly due: number; readonly fire: () => void }[] = [];
  const host = { idle: true };

  const attempt = <T>(next: Attempt<T> | undefined, fallback: T, signal?: AbortSignal) => {
    if (next === undefined) return Promise.resolve(fallback);
    if ("answer" in next) return Promise.resolve(next.answer);
    if ("fail" in next) return Promise.reject(next.fail);
    if ("late" in next) return next.late;
    const { promise, reject } = Promise.withResolvers<T>();
    signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
    return promise;
  };

  const take = createTaskTake({
    caller,
    daemon: {
      taskSeen: (input, options) => {
        calls.push({ route: "task-seen", body: { ...input } });
        return attempt(seens.shift(), undefined, options?.signal);
      },
      take: (input, options) => {
        calls.push({ route: "take", body: { ...input } });
        return attempt(takes.shift(), REFUSED, options?.signal);
      },
    },
    run: (text) => {
      if (!host.idle) return "not idle";
      ran.push(text);
      const hand = Promise.withResolvers<void>();
      handed.push(hand);
      return hand.promise;
    },
    setPending: (pending) => marks.push(pending),
    log: { info: () => undefined, warn: (message) => warned.push(message) },
    wait: (ms, signal) => {
      const { promise, resolve } = Promise.withResolvers<boolean>();
      if (signal.aborted) {
        resolve(false);
        return promise;
      }
      if (ms !== DAEMON_ANSWER_TIMEOUT_MS) waits.push(ms);
      const stop = () => {
        timers.splice(timers.indexOf(timer), 1);
        resolve(false);
      };
      const timer = {
        due: now + ms,
        fire: () => {
          signal.removeEventListener("abort", stop);
          resolve(true);
        },
      };
      timers.push(timer);
      signal.addEventListener("abort", stop, { once: true });
      return promise;
    },
  });

  const advance = async (ms: number): Promise<void> => {
    const until = now + ms;
    for (;;) {
      await flush();
      timers.sort((a, b) => a.due - b.due);
      const next = timers[0];
      if (next === undefined || next.due > until) break;
      timers.shift();
      now = next.due;
      next.fire();
    }
    now = until;
    await flush();
  };

  const routed = (route: Call["route"]) =>
    calls.filter((call) => call.route === route).map((call) => call.body);

  return { take, calls, takes, seens, ran, handed, marks, warned, waits, advance, routed, host };
}

const body = (number: number, revision: number) => ({
  ...caller,
  number,
  epoch: EPOCH,
  revision,
});

test("reports a value seen while the host is busy, and asks only once the run ends", async () => {
  const { take, routed, advance, takes } = rig();
  take.ready();
  take.runStarted();
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);

  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([]);

  takes.push({ answer: ACCEPTED });
  expect(take.runEnded(false)).toBeUndefined();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks no take before the claim's ready has answered, and once after it", async () => {
  const { take, routed, advance } = rig();
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([]);

  take.ready();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks once per revision: the same value read and relayed again is neither reported nor asked again", async () => {
  const { take, routed, advance } = rig();
  take.ready();
  take.read({ task: value(4, 12), finished: 3 });
  take.relayed(put(4, 12));
  await advance(0);
  take.read({ task: value(4, 12), finished: 3 });
  take.relayed(put(4, 12));
  await advance(0);

  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks nothing again for the same value relayed after a refusal, and once at the hold-end's new revision, running the accepted text", async () => {
  const { take, routed, advance, takes, ran, marks } = rig();
  take.ready();
  take.relayed(put(4, 12));
  await advance(0);
  // The take was refused (a hold stands, `take-refused.json`), so the task's turn is not pending.
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);

  take.relayed(put(4, 12));
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  // The hold ended: the daemon wrote the value again, a new revision, which is the only signal.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
  // The mark stays until the task's turn starts.
  expect(marks).toEqual([true, false, true]);
  take.runStarted();
  expect(marks).toEqual([true, false, true, false]);
  expect(take.runEnded(false)).toBe(4);
});

test("holds the task-turn mark from the registration's value, before the ready and the role claim, until the task's turn starts", async () => {
  const { take, marks, takes, advance } = rig();
  take.read({ task: value(4, 12), finished: 3 });
  expect(marks).toEqual([true]);

  takes.push({ answer: ACCEPTED });
  take.ready();
  await advance(0);
  expect(marks).toEqual([true]);
  take.runStarted();
  expect(marks).toEqual([true, false]);
});

test("takes during a compaction, which leaves the host idle, and the first run after it is the task's turn", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  // A run ends, and the host compacts in its idle maintenance: no run is going, so the host is
  // idle for the take, and the text waits for the compaction (the host's own form, `run`).
  take.runStarted();
  take.runEnded(false);
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);

  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  // A run after the task's names no task.
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
});

test("a provider error's end, the wait's end, and exactly one new take at the value the daemon writes again", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  take.runStarted();
  // The turn ended in a provider error, which its report names beside the number: the daemon
  // keeps the task under its number and waits before it writes the value again.
  expect(take.runEnded(true)).toBe(4);

  // Nothing new while the wait runs: the same value read or relayed again asks nothing.
  take.relayed(put(4, 12));
  take.read({ task: value(4, 12), finished: 3 });
  await advance(30_000);
  expect(routed("take")).toEqual([body(4, 12)]);

  // The wait ended: the daemon wrote the value again, and the worker takes it once.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  take.read({ task: value(4, 13), finished: 3 });
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
});

// The daemon finishes a current claim's task on a report naming its number with no provider
// error, but it may write the same number's value again before that report lands (a daemon
// start writes every owed value again), and it answers a take of the accepted, unfinished number
// with the same text. The take holds the number finished from the end of the run whose report
// carries no provider error, so that newer revision is owed nothing: no take, no second run.
test("a task's turn whose report carries no provider error finishes it at the run's end: a newer revision of its number, written before the report lands, is neither taken nor run again", async () => {
  const { take, routed, takes, ran, advance, marks } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);

  // The daemon would answer a take of the newer revision with the same text.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  take.read({ task: value(4, 13), finished: 3 });
  await advance(0);

  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);
  expect(marks.at(-1)).toBe(false);
});

// A report with a provider error finishes nothing: the daemon takes the task back, and the worker
// takes the number again at the revision the daemon writes, and runs it once more.
test("a task's turn whose report carries a provider error leaves it owed: the newer revision of its number is taken and runs once", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.runEnded(true)).toBe(4);

  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);

  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  take.relayed(put(4, 14));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
});

// R2 and c2: each value is reported seen, idle or not. While the run-end mark stands, a value
// naming the marked number is reported seen like any other; only its take waits.
test("a value naming the marked number is reported seen while the mark stands, and only its take waits", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);

  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  take.read({ task: value(4, 14), finished: 3 });
  await advance(0);

  expect(routed("task-seen")).toEqual([body(4, 12), body(4, 13), body(4, 14)]);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);
});

// The mark lasts only until the turn's report is answered. Here the daemon refuses it with a 4xx,
// which applied nothing: the record still owes the number, so the newer revision it wrote while
// the report was on its way, reported seen and not taken while the mark stood, is taken once the
// refusal comes, as a number the record owes, and its text runs once. That turn's end marks the
// number again, so a revision after it runs nothing.
test("a report refused with a 4xx clears the mark, and the newer revision of n is then taken and runs once", async () => {
  const { take, routed, takes, ran, advance, marks } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);

  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);

  takes.push({ answer: ACCEPTED });
  take.reportRefused(4);
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
  // The task-turn mark is up again from the take until the task's turn starts.
  expect(marks).toEqual([true, false, true]);

  take.runStarted();
  expect(marks).toEqual([true, false, true, false]);
  expect(take.runEnded(false)).toBe(4);
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 14));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
});

// The run-end mark is kept apart from the number a reading says the record finished: a refusal
// of the report clears the mark alone, and never lowers what a reading said.
test("a refused report clears the run-end mark alone: a number a reading said finished stays finished", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);

  take.read({ task: null, finished: 4 });
  take.reportRefused(4);
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);

  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);
});

// The race: the take is asked on an idle host and the daemon accepts
// it, but a run no take started (a background job's result, a reminder) begins before the answer
// comes back. The daemon gives that run to the take, as the first to start after its accept; the
// worker holds the text, since nothing is sent into a busy worker and a follow-up sent now would
// run inside that run, and reports the run's end with no number, which gives the take its turn
// back on the daemon. The held text then starts the next run, the task's turn on both sides, and
// that run's end names the number.
test("a take accepted while a run it did not start is going holds its text: that run's end names no task, and the text starts the next run, whose end names the task", async () => {
  const { take, takes, ran, advance, marks } = rig();
  take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  take.relayed(put(4, 12));
  await advance(0);
  // A run starts after the daemon decided the take and before its answer arrives.
  take.runStarted();
  answer.resolve(ACCEPTED);
  await advance(0);
  // The text is held: no follow-up goes into the running run, and the turn is still pending.
  expect(ran).toEqual([]);
  expect(marks).toEqual([true]);

  // That run's end names no task, and the text goes to the now idle host.
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  // The run it starts is the task's turn, and only it names the number.
  take.runStarted();
  expect(marks).toEqual([true, false]);
  expect(take.runEnded(false)).toBe(4);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
});

// The lost-answer variant: the daemon accepted the take, but its answer was lost (here a reset
// connection), and a run started while the take waited to be sent again. The daemon answers a
// take repeated for the number it accepted with the same text, also after it gave that run to the
// take, so the re-sent take is answered `accepted` during the run: the text is held, that run's
// end names no task, and the text runs as the next run, once.
test("a take whose accepted answer was lost is answered again during a run that started meanwhile: the text is held, and runs next, once", async () => {
  const { take, takes, ran, advance, routed } = rig();
  take.ready();
  takes.push({ fail: transport("ECONNRESET") }, { answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  // A run starts while the take waits to be sent again, and the re-sent take is answered then.
  take.runStarted();
  await advance(DAEMON_RESTART_FIRST_DELAY_MS);
  expect(routed("take")).toEqual([body(4, 12), body(4, 12)]);
  expect(ran).toEqual([]);

  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
});

// R5 and the plan's decision 9 for a host that is not idle though no run of its session is going:
// it sets up a prompt it already took, a card's here, before that prompt's `agent_start`. The take
// hands the text in no form into that setup — a follow-up's answer settles once the host queues
// it, so a drop of it after that would not be seen —: `run` answers that the host is not idle, and
// the take holds the text as it holds one accepted during a run. The card's run is the first to
// start after the accept, so the daemon counts it as the take's turn; its end names no task, which
// puts the take back, and the text goes to the idle host then, once, as the prompt that starts the
// task's own turn, whose end names the number.
test("a take accepted while the host sets up a card's prompt holds its text: the card's run names no task, and the text is handed at idle once, as the prompt of the next run, whose end names the task", async () => {
  const { take, takes, ran, advance, marks, host, routed } = rig();
  take.ready();
  host.idle = false;
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([]);
  expect(marks).toEqual([true]);

  // The card's run starts and ends; the host is idle again at its end.
  take.runStarted();
  expect(marks).toEqual([true]);
  host.idle = true;
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);

  // The run the text starts is the task's turn, and only it names the number.
  take.runStarted();
  expect(marks).toEqual([true, false]);
  expect(take.runEnded(false)).toBe(4);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  expect(routed("take")).toEqual([body(4, 12)]);
});

// A run's end can find the host not idle as well: the host goes on with a continuation it has
// already scheduled (`willContinue`). A text held for that end is held again, through the
// continuation's run, whose end names no task either, and goes to the host at the first end that
// finds it idle, once.
test("a held text whose run's end finds the host not idle, a continuation following, is held through that continuation too, and handed at idle once", async () => {
  const { take, takes, ran, advance, host } = rig();
  take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  answer.resolve(ACCEPTED);
  await advance(0);

  host.idle = false;
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([]);
  take.runStarted();
  host.idle = true;
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  expect(ran).toEqual([TEXT]);
});

// The run a held text waits through is reported as holding it (`held`), read at that run's end
// before `runEnded` hands the text on: that is the daemon's one certain word that the run was not
// the task's turn, also where the daemon has lost its count of the session's turns. A run that
// ends before the take's answer came holds nothing, nor does the run the text starts, nor a run
// after a newer revision of the number let the held text go; a continuation the held text waits
// through holds it too.
test("names the text it holds through a run at that run's end, and nothing once the text is handed or let go", async () => {
  const { take, takes, ran, advance, host } = rig();
  take.ready();
  expect(take.held()).toBeUndefined();
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  expect(take.held()).toBeUndefined();
  answer.resolve(ACCEPTED);
  await advance(0);

  expect(take.held()).toBe(4);
  host.idle = false;
  expect(take.runEnded(false)).toBeUndefined();
  expect(take.held()).toBe(4);
  take.runStarted();
  host.idle = true;
  expect(take.held()).toBe(4);
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  expect(take.held()).toBeUndefined();
  take.runStarted();
  expect(take.held()).toBeUndefined();
  expect(take.runEnded(false)).toBe(4);
  expect(take.held()).toBeUndefined();

  const letGo = rig();
  letGo.take.ready();
  const late = Promise.withResolvers<LegionTake>();
  letGo.takes.push({ late: late.promise });
  letGo.take.relayed(put(5, 20));
  await letGo.advance(0);
  letGo.take.runStarted();
  late.resolve(ACCEPTED);
  await letGo.advance(0);
  expect(letGo.take.held()).toBe(5);
  letGo.take.relayed(put(5, 21));
  expect(letGo.take.held()).toBeUndefined();
});

// The daemon answers a take of the number it accepted with the same text until that task
// finishes, also once the task's turn has started: a newer revision of the same number, asked
// before the turn and answered during it, is that same task, which is already running.
test("a take of a newer revision of the accepted number, answered during that task's own turn, runs nothing more", async () => {
  const { take, takes, ran, advance, routed } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(ran).toEqual([TEXT]);

  const again = Promise.withResolvers<LegionTake>();
  takes.push({ late: again.promise });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  // The task's turn starts, and the repeated take's answer arrives during it.
  take.runStarted();
  again.resolve(ACCEPTED);
  await advance(0);

  expect(take.runEnded(false)).toBe(4);
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
});

// The plan's decision 9: a number held as accepted is a new offer at a newer revision of it only
// while its text has not reached the host. A text held through a run the worker did not start was
// never handed to it: where the daemon writes the number's value again meanwhile — it took the task
// back, or only wrote the value again —, that run's end hands nothing, the take is made at the newer
// revision once the host is idle, and the text that answer carries runs, once.
test("a take's text held through a run the worker did not start is taken again at a newer revision of its number, and only that answer's text runs, once", async () => {
  const { take, takes, ran, advance, routed, marks } = rig();
  take.ready();
  const first = Promise.withResolvers<LegionTake>();
  takes.push({ late: first.promise });
  take.relayed(put(4, 12));
  await advance(0);
  take.runStarted();
  first.resolve(ACCEPTED);
  await advance(0);
  expect(ran).toEqual([]);

  // The newer revision is reported seen, and asked about only once the run has ended.
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  const again = Promise.withResolvers<LegionTake>();
  takes.push({ late: again.promise });
  expect(take.runEnded(false)).toBeUndefined();
  await advance(0);
  expect(ran).toEqual([]);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);

  again.resolve(ACCEPTED_AFRESH);
  await advance(0);
  expect(ran).toEqual([AFRESH_TEXT]);
  // The task's turn was pending throughout, until the run that text starts.
  expect(marks).toEqual([true]);
  take.runStarted();
  expect(marks).toEqual([true, false]);
  expect(take.runEnded(false)).toBe(4);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([AFRESH_TEXT]);
});

// A text handed to the host and dropped by it before its turn started — a compaction aborted the
// prompt the host was setting up, and `sendUserMessage` rejected — never reached the host either:
// the take awaits the value's next revision, which the daemon writes once it has taken the task
// back, takes at it, and hands that answer's text once.
test("a take whose text the host dropped before its turn started is taken at the value's next revision, and its text runs once", async () => {
  const { take, takes, ran, handed, advance, routed, marks } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(handed).toHaveLength(1);
  handed[0]?.reject(DROP);
  await advance(0);

  takes.push({ answer: ACCEPTED_AFRESH });
  take.relayed(put(4, 13));
  await advance(0);
  expect(ran).toEqual([TEXT, AFRESH_TEXT]);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  takes.push({ answer: ACCEPTED_AFRESH });
  take.relayed(put(4, 14));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, AFRESH_TEXT]);
  // The mark comes down at the drop, since nothing is pending until the daemon writes the value
  // again, and goes up again with the take at the newer revision, until the task's turn starts.
  expect(marks).toEqual([true, false, true, false]);
});

// The host can drop the text after the daemon wrote the value again and answered the take at that
// newer revision: an abort ends a prompt the host is setting up only once that setup reaches its
// next step, which can be after a short compaction that took the task back has ended. The answer
// that came while the host still held the text is the next accepted answer, and its text goes to
// the host in the dropped one's place, once; a rejection of a text whose run has started is no drop.
test("a take answered at a newer revision while the host still held the text it then dropped hands that answer's text, once", async () => {
  const { take, takes, ran, handed, advance, routed } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  takes.push({ answer: ACCEPTED_AFRESH });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);

  expect(handed).toHaveLength(1);
  handed[0]?.reject(DROP);
  await advance(0);
  expect(ran).toEqual([TEXT, AFRESH_TEXT]);
  take.runStarted();
  handed[1]?.reject(new Error("the run failed"));
  await advance(0);
  expect(take.runEnded(false)).toBe(4);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, AFRESH_TEXT]);
});

// Once the text is with the host and the host has not dropped it, a newer revision of its number
// changes nothing, also while the host sets the prompt up before its `agent_start` (the first
// prompt's recall can take 30 seconds): the daemon answers the take of the accepted number with the
// same text, nothing more is handed, and the text runs once (plan review round 68: a guard keyed on
// a run having started would hand the text a second time here).
test("a take's text handed to the host and waiting on its setup hands nothing more at a newer revision of its number, and runs once", async () => {
  const { take, takes, ran, advance, routed } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(ran).toEqual([TEXT]);

  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(30_000);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);

  take.runStarted();
  expect(take.runEnded(false)).toBe(4);
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  expect(ran).toEqual([TEXT]);
});

test("a dropped take clears the mark, and neither the delete it made nor a stale relay of the dropped value asks again", async () => {
  const { take, routed, takes, marks, advance } = rig();
  take.ready();
  takes.push({ answer: DROPPED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(marks).toEqual([true, false]);

  // The delete the drop made, then a stale relay of the dropped value at its old revision.
  take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("a value naming a finished number, or another claim's, asks nothing", async () => {
  const { take, routed, advance, marks } = rig();
  take.ready();
  take.read({ task: value(3, 9), finished: 3 });
  take.relayed(put(5, 2, "legion-legion-legion-210-implementer"));
  take.read(NOTHING_HELD);
  await advance(0);

  expect(routed("take")).toEqual([]);
  expect(routed("task-seen")).toEqual([body(3, 9)]);
  expect(marks).toEqual([]);
});

test("a value of another epoch replaces the held one, whatever its revision", async () => {
  const { take, routed, advance } = rig();
  take.ready();
  take.relayed(put(4, 12));
  await advance(0);
  take.read({ task: value(4, 1, "2026-11-14T08:00:00.5Z"), finished: 3 });
  await advance(0);

  expect(routed("take")).toEqual([body(4, 12), { ...body(4, 1), epoch: "2026-11-14T08:00:00.5Z" }]);
});

/** A failure of the transport, as Bun's fetch raises it, carrying `code`. */
const transport = (code: string): Error =>
  Object.assign(new TypeError(`${code}: the request reached no daemon`), { code });

const uncoded = (status: number): Error =>
  new LegionDaemonApiError("POST", "/legion/v1/claims/take", status, undefined, "upstream failed");

for (const [name, attempt] of [
  ["a refused connection", { fail: transport("ECONNREFUSED") }],
  ["a reset connection", { fail: transport("ECONNRESET") }],
  ["a closed connection", { fail: transport("ConnectionClosed") }],
  ["an unreachable host", { fail: transport("EHOSTUNREACH") }],
  ["an unreachable network", { fail: transport("ENETUNREACH") }],
  ["a connect timeout", { fail: transport("ETIMEDOUT") }],
  ["a connect timeout Bun names ConnectionTimeout", { fail: transport("ConnectionTimeout") }],
  // Bun's fetch gives a read that timed out no code, only the name (Bun 1.4.4).
  ["a read timeout", { fail: new DOMException("The operation timed out.", "TimeoutError") }],
  ["a DNS failure", { fail: transport("ENOTFOUND") }],
  ["a transport failure no list names", { fail: transport("FailedToOpenSocket") }],
  ["a failure that names no code", { fail: new Error("the socket hung up") }],
  ["no answer within 5 minutes", { silent: true }],
  ["a 502 with no code", { fail: uncoded(502) }],
  ["a 503 with no code", { fail: uncoded(503) }],
  ["a 504 with no code", { fail: uncoded(504) }],
] as const) {
  test(`a take that meets ${name} is sent again, the same request, with growing waits and no limit, until it is answered`, async () => {
    const { take, routed, takes, ran, waits, advance, marks } = rig();
    take.ready();
    const failures = 12;
    for (let n = 0; n < failures; n += 1) takes.push(attempt);
    takes.push({ answer: ACCEPTED });
    take.relayed(put(4, 12));
    // Long enough for every attempt's bound and every wait between them.
    for (let n = 0; n <= failures; n += 1) await advance(DAEMON_ANSWER_TIMEOUT_MS);

    expect(routed("take")).toEqual(Array(failures + 1).fill(body(4, 12)));
    expect(waits).toEqual(
      Array.from({ length: failures }, (_, n) =>
        Math.min(DAEMON_RESTART_FIRST_DELAY_MS * 2 ** n, DAEMON_RESTART_MAX_DELAY_MS)
      )
    );
    expect(ran).toEqual([TEXT]);
    // Sending again is the same ask: the mark held throughout, until the turn starts.
    expect(marks).toEqual([true]);
  });

  test(`a seen report that meets ${name} is sent again, the same request, until it is answered`, async () => {
    const { take, routed, seens, advance } = rig();
    seens.push(attempt, attempt, attempt);
    take.relayed(put(4, 12));
    for (let n = 0; n <= 3; n += 1) await advance(DAEMON_ANSWER_TIMEOUT_MS);

    expect(routed("task-seen")).toEqual(Array(4).fill(body(4, 12)));
  });
}

for (const [name, refusal] of [
  ["a 409 with a code", new LegionDaemonApiError("POST", "/p", 409, "TASK_NOT_OWED", "refused")],
  ["a 503 with a code", new LegionDaemonApiError("POST", "/p", 503, "DAEMON_DRAINING", "draining")],
  ["a 400", new LegionDaemonApiError("POST", "/p", 400, undefined, "malformed")],
] as const) {
  test(`a take or a seen report the daemon answers with ${name} is not sent again`, async () => {
    const { take, routed, takes, seens, advance, marks } = rig();
    take.ready();
    takes.push({ fail: refusal });
    seens.push({ fail: refusal });
    take.relayed(put(4, 12));
    await advance(10 * DAEMON_ANSWER_TIMEOUT_MS);

    expect(routed("take")).toEqual([body(4, 12)]);
    expect(routed("task-seen")).toEqual([body(4, 12)]);
    expect(marks).toEqual([true, false]);
  });
}

test("a newer revision ends the re-sending of the older take and seen report, and is asked at once", async () => {
  const { take, routed, takes, seens, ran, advance } = rig();
  take.ready();
  takes.push({ fail: transport("ECONNREFUSED") }, { fail: transport("ECONNREFUSED") });
  seens.push({ fail: transport("ECONNREFUSED") }, { fail: transport("ECONNREFUSED") });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  takes.splice(0);
  takes.push({ answer: ACCEPTED });
  seens.splice(0);
  take.relayed(put(4, 13));
  await advance(10 * DAEMON_RESTART_MAX_DELAY_MS);

  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(routed("task-seen")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
});

test("a deleted value ends the re-sending of its take", async () => {
  const { take, routed, takes, advance, marks } = rig();
  take.ready();
  for (let n = 0; n < 5; n += 1) takes.push({ fail: transport("ECONNRESET") });
  take.relayed(put(4, 12));
  await advance(0);
  take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  await advance(10 * DAEMON_RESTART_MAX_DELAY_MS);

  expect(routed("take")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);
});

// The daemon-launched controller replaces its take at each claim (`takeFor` in
// `packages/pi-legion/src/controller-session.ts`): the replaced take's requests and mark are not
// the new take's to keep.
test("a closed take sends nothing more, drops an answer that still arrives, clears its mark, and does nothing after", async () => {
  const { take, routed, takes, seens, ran, marks, advance } = rig();
  take.ready();
  const late = Promise.withResolvers<LegionTake>();
  takes.push({ late: late.promise });
  seens.push({ fail: transport("ECONNREFUSED") }, { fail: transport("ECONNREFUSED") });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true]);

  take.close();
  expect(marks).toEqual([true, false]);
  // The seen report's re-sending stops, and the take's answer, arriving now, runs nothing.
  late.resolve(ACCEPTED);
  await advance(10 * DAEMON_RESTART_MAX_DELAY_MS);
  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(ran).toEqual([]);

  // Nothing it is handed after it reports, asks or marks anything.
  take.relayed(put(5, 13));
  take.read({ task: value(5, 13), finished: 4 });
  take.ready();
  take.runStarted();
  expect(take.runEnded(false)).toBeUndefined();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);
  expect(ran).toEqual([]);
});

// dispatch://INFRA-4130 R1: while a run is going and the claim holds a value the record owes and
// the worker did not take, the host adds one note to the run's next tool result, once for each
// task number (`noteFor`), and the take still goes only at the run's end. R6: while the take is
// pending (`pending`), no reminder turn starts ahead of it.

/** The daemon's refusal of a take, naming the hold that stands. */
const refused = (hold: string): LegionTake => ({ outcome: "refused", hold });

/** The take of task 4 at revision 12, asked while the host was idle, overtaken by a run that
 * started before the daemon's answer, and answered refused `in-turn` during that run: the daemon
 * writes the value again at that run's end. */
async function refusedInTurnDuringRun() {
  const taking = rig();
  taking.take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  taking.takes.push({ late: answer.promise });
  taking.take.relayed(put(4, 12));
  await taking.advance(0);
  taking.take.runStarted();
  answer.resolve(refused("in-turn"));
  await taking.advance(0);
  expect(taking.routed("take")).toEqual([body(4, 12)]);
  return taking;
}

test("a value held while the host is busy sends one note naming its number, asks no take until the run ends, and takes at the run's end", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  take.runStarted();
  // A tool result before any value is owed carries no note.
  expect(take.noteFor()).toBeUndefined();

  // S2: the value arrives during the run; the next tool result notes its number, and no take goes
  // into the run.
  take.relayed(put(4, 12));
  await advance(0);
  expect(take.noteFor()).toBe(4);
  expect(routed("take")).toEqual([]);
  // S3: the tool results after it note nothing more, nor does a newer revision of that number.
  expect(take.noteFor()).toBeUndefined();
  take.relayed(put(4, 13));
  await advance(0);
  expect(take.noteFor()).toBeUndefined();
  // S4: a newer value naming another number gets one note of its own.
  take.relayed(put(5, 14));
  await advance(0);
  expect(take.noteFor()).toBe(5);
  expect(take.noteFor()).toBeUndefined();
  expect(routed("take")).toEqual([]);

  // The run's end is where the take goes, at the newest value, and its text starts the next run.
  takes.push({ answer: ACCEPTED });
  expect(take.runEnded(false)).toBeUndefined();
  await advance(0);
  expect(routed("take")).toEqual([body(5, 14)]);
  expect(ran).toEqual([TEXT]);
  // That run is the task's own turn, which notes nothing.
  take.runStarted();
  expect(take.noteFor()).toBeUndefined();
  expect(take.runEnded(false)).toBe(5);
});

test("a value whose take is in flight when a run starts gets one note", async () => {
  const { take, routed, takes, advance } = rig();
  take.ready();
  takes.push({ silent: true });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  // The host is idle while the take is in flight: no tool result, no note.
  expect(take.noteFor()).toBeUndefined();

  // A run no take started (a background job's result, a reminder) begins before the answer.
  take.runStarted();
  expect(take.noteFor()).toBe(4);
  expect(take.noteFor()).toBeUndefined();
});

test("a value refused in-turn during a run gets one note", async () => {
  const { take } = await refusedInTurnDuringRun();
  expect(take.noteFor()).toBe(4);
  expect(take.noteFor()).toBeUndefined();
});

test("a value refused by a hold gets one note in the next run", async () => {
  for (const hold of ["issue-in-turn", "stale-plugin"]) {
    const { take, routed, takes, advance } = rig();
    take.ready();
    takes.push({ answer: refused(hold) });
    take.relayed(put(4, 12));
    await advance(0);
    expect(routed("take"), hold).toEqual([body(4, 12)]);
    expect(take.noteFor(), hold).toBeUndefined();

    // A run starts while the hold stands (a reminder's, a card's): the number the worker did not
    // take is noted at its first tool result, once.
    take.runStarted();
    expect(take.noteFor(), hold).toBe(4);
    expect(take.noteFor(), hold).toBeUndefined();
  }
});

test("a value that arrives after the run's last tool result gets no note, and is taken at the run's end", async () => {
  const { take, routed, takes, advance } = rig();
  take.ready();
  take.runStarted();
  // The run's last tool result comes before any value is owed.
  expect(take.noteFor()).toBeUndefined();

  // The value arrives after it, and the run ends with no tool result in between.
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([]);
  takes.push({ answer: refused("issue-in-turn") });
  take.runEnded(false);
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  // No note was handed out: the idle host notes nothing, and the first tool result of the next
  // run, while the hold stands, is the first to note the number.
  expect(take.noteFor()).toBeUndefined();
  take.runStarted();
  expect(take.noteFor()).toBe(4);
});

test("a take refused in-turn is ended by a delete, during the run or after its end, and one whose refusal meets an idle host stays pending until a run starts", async () => {
  // A delete ends it, during the run or after its end, and nothing is pending.
  const deletedInRun = await refusedInTurnDuringRun();
  expect(deletedInRun.take.pending()).toBe(true);
  deletedInRun.take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  await deletedInRun.advance(0);
  expect(deletedInRun.take.pending()).toBe(false);
  const deleted = await refusedInTurnDuringRun();
  deleted.take.runEnded(false);
  await deleted.advance(0);
  expect(deleted.take.pending()).toBe(true);
  deleted.take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  await deleted.advance(0);
  expect(deleted.take.pending()).toBe(false);
  expect(deleted.routed("take")).toEqual([body(4, 12)]);

  // An in-turn answer can meet an idle host: the daemon decided the take while it counted the run
  // that overtook it as going, and writes the value again only once it reads that run's agent_end
  // frame from the stream, so its write may still be to come. The take stays pending, a callback
  // held for it is not called, and a run that starts ends the refusal.
  const idle = rig();
  idle.take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  idle.takes.push({ late: answer.promise });
  idle.take.relayed(put(4, 12));
  await idle.advance(0);
  idle.take.runStarted();
  idle.take.runEnded(false);
  let calls = 0;
  expect(
    idle.take.deferUntilSettled(() => {
      calls += 1;
    })
  ).toBe(4);
  answer.resolve(refused("in-turn"));
  await idle.advance(0);
  expect(idle.take.pending()).toBe(true);
  expect(calls).toBe(0);
  idle.take.runStarted();
  expect(idle.take.pending()).toBe(false);
  expect(calls).toBe(0);
});

// A stall abort that ends a run whose take was refused `in-turn` holds its restart message for
// that take. The daemon writes the value again only once it reads that run's agent_end frame from
// the stream, so the take is still pending past the run's end, and asks nothing there: the restart
// waits for that write, at which the take asks, and a run the take's text starts drops it.
test("a stall restart held during a run whose take was refused in-turn waits for the daemon's write at that run's end", async () => {
  const { take, takes, routed, ran, advance } = await refusedInTurnDuringRun();
  let calls = 0;
  expect(
    take.deferUntilSettled(() => {
      calls += 1;
    })
  ).toBe(4);
  take.runEnded(false);
  await advance(0);
  expect(calls).toBe(0);
  expect(take.pending()).toBe(true);
  expect(routed("take")).toEqual([body(4, 12)]);

  // The daemon writes task 4 again at revision 13: the take asks it, and the host runs its text.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(calls).toBe(0);
});

// The take that a run's end sends races the agent_end frame the daemon reads from the stream: one
// that reaches the daemon first is refused `in-turn`, and that answer meets an idle host. The
// daemon writes the value again once it reads the frame, so the take stays pending, and a stall
// abort's held restart waits for that write; a run the take's text starts drops it.
test("a take sent at a run's end and refused in-turn on the idle host, having reached the daemon before that run's end did, stays pending, and a held restart waits for the daemon's write", async () => {
  const { take, takes, routed, ran, advance } = rig();
  take.ready();
  take.runStarted();
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([]);
  let calls = 0;
  expect(
    take.deferUntilSettled(() => {
      calls += 1;
    })
  ).toBe(4);

  // The run's end sends the take, which is in flight.
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  expect(take.runEnded(false)).toBeUndefined();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(take.pending()).toBe(true);

  // The daemon decided it before it read the run's agent_end frame.
  answer.resolve(refused("in-turn"));
  await advance(0);
  expect(take.pending()).toBe(true);
  expect(calls).toBe(0);

  // The daemon reads the frame and writes task 4 again at revision 13: the take asks it, and the
  // host runs its text.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(calls).toBe(0);
});

// The daemon writes nothing at the turn's end while another hold stands then, so a take refused
// `in-turn` waits for its write for at most the bound, and past it the reminding and a held
// restart go on. A run that starts is the worker working again, and a value written during it is
// asked at its end, so it ends the refusal too.
test("a take refused in-turn stays pending at most IN_TURN_REWRITE_BOUND_MS after its run's end, and a run that starts ends it", async () => {
  const bounded = await refusedInTurnDuringRun();
  let calls = 0;
  expect(
    bounded.take.deferUntilSettled(() => {
      calls += 1;
    })
  ).toBe(4);
  bounded.take.runEnded(false);
  await bounded.advance(IN_TURN_REWRITE_BOUND_MS - 1);
  expect(bounded.take.pending()).toBe(true);
  expect(calls).toBe(0);
  await bounded.advance(1);
  expect(bounded.take.pending()).toBe(false);
  expect(calls).toBe(1);

  // A run that starts after that end ends the refusal.
  const started = await refusedInTurnDuringRun();
  started.take.runEnded(false);
  started.take.runStarted();
  expect(started.take.pending()).toBe(false);
});

// A take asked while the host is idle can be overtaken by a short run that ends before the
// daemon's answer comes back: the refusal `in-turn` then meets an idle host with no race at the
// run's end. The bound starts at the later of the run's end and the refusal, so here at the
// refusal.
test("a take asked while the host is idle, overtaken by a short run and refused in-turn after that run's end, stays pending at most IN_TURN_REWRITE_BOUND_MS after the refusal", async () => {
  const { take, takes, routed, advance } = rig();
  take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  // A short run starts and ends before the answer comes back; a restart is held during it.
  take.runStarted();
  let calls = 0;
  expect(
    take.deferUntilSettled(() => {
      calls += 1;
    })
  ).toBe(4);
  expect(take.runEnded(false)).toBeUndefined();
  await advance(1_000);
  expect(calls).toBe(0);

  answer.resolve(refused("in-turn"));
  await advance(0);
  expect(take.pending()).toBe(true);
  expect(calls).toBe(0);
  await advance(IN_TURN_REWRITE_BOUND_MS - 1);
  expect(take.pending()).toBe(true);
  expect(calls).toBe(0);
  await advance(1);
  expect(take.pending()).toBe(false);
  expect(calls).toBe(1);
});

test("pending() holds a value the take has not asked about, idle before the ready or busy, and lets it go once refused", async () => {
  const { take, takes, advance } = rig();
  expect(take.pending()).toBe(false);
  // Before the ready, the take asks nothing.
  take.read({ task: value(4, 12), finished: 3 });
  expect(take.pending()).toBe(true);
  takes.push({ answer: refused("issue-in-turn") });
  take.ready();
  await advance(0);
  expect(take.pending()).toBe(false);

  // While a run goes, the take asks nothing either.
  take.runStarted();
  take.relayed(put(4, 13));
  await advance(0);
  expect(take.pending()).toBe(true);
  // A value naming a finished number is owed nothing.
  take.read({ task: value(4, 13), finished: 4 });
  expect(take.pending()).toBe(false);
});

test("pending() holds a take in flight until the daemon answers it", async () => {
  const { take, takes, advance } = rig();
  take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  takes.push({ late: answer.promise });
  take.relayed(put(4, 12));
  await advance(0);
  expect(take.pending()).toBe(true);
  take.runStarted();
  expect(take.pending()).toBe(true);

  answer.resolve(refused("stale-plugin"));
  await advance(0);
  expect(take.pending()).toBe(false);
});

test("pending() holds an accepted take until its turn starts, its text held through a run included", async () => {
  // Sent to an idle host: pending until the run it starts.
  const idle = rig();
  idle.take.ready();
  idle.takes.push({ answer: ACCEPTED });
  idle.take.relayed(put(4, 12));
  await idle.advance(0);
  expect(idle.ran).toEqual([TEXT]);
  expect(idle.take.pending()).toBe(true);
  idle.take.runStarted();
  expect(idle.take.pending()).toBe(false);
  expect(idle.take.runEnded(false)).toBe(4);
  expect(idle.take.pending()).toBe(false);

  // Accepted during a run it did not start: the text is held until that run's end, and the take
  // is pending until the run the text starts.
  const held = rig();
  held.take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  held.takes.push({ late: answer.promise });
  held.take.relayed(put(4, 12));
  await held.advance(0);
  held.take.runStarted();
  answer.resolve(ACCEPTED);
  await held.advance(0);
  expect(held.ran).toEqual([]);
  expect(held.take.pending()).toBe(true);
  expect(held.take.noteFor()).toBeUndefined();
  expect(held.take.runEnded(false)).toBeUndefined();
  expect(held.ran).toEqual([TEXT]);
  expect(held.take.pending()).toBe(true);
  held.take.runStarted();
  expect(held.take.pending()).toBe(false);
});

test("pending() holds a take refused in-turn during the run going now, and no other refusal", async () => {
  const inTurn = await refusedInTurnDuringRun();
  expect(inTurn.take.pending()).toBe(true);

  // Another hold answered during a run is the daemon's to end with a newer write: nothing waits.
  const other = rig();
  other.take.ready();
  const answer = Promise.withResolvers<LegionTake>();
  other.takes.push({ late: answer.promise });
  other.take.relayed(put(4, 12));
  await other.advance(0);
  other.take.runStarted();
  answer.resolve(refused("issue-in-turn"));
  await other.advance(0);
  expect(other.take.pending()).toBe(false);
});

// The running task's own number can reach the take at a newer revision during its own turn: the
// daemon writes a value again wherever the bucket holds anything but its own last write (a repair
// read or a boot read after the bucket lost the value, `valueRead` in
// packages/daemon/internal/supervise/taskvalue.go). Nothing is taken for it: the run's end holds
// the number finished. So no task's turn is still to come, which `noteFor` already says, and
// `pending` must say it too (R6), or the settle withholds the run's continuation and a stall abort
// withholds its restart for a take that never comes.
test("pending() is false for a newer revision of the task whose turn is running, as noteFor() is", async () => {
  const { take, takes, routed, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(take.pending()).toBe(false);

  take.relayed(put(4, 13));
  await advance(0);
  expect(take.noteFor()).toBeUndefined();
  expect(take.pending()).toBe(false);

  // The run's end asks nothing and runs nothing: the run's report finishes the number.
  expect(take.runEnded(false)).toBe(4);
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);
  expect(take.pending()).toBe(false);
});

// dispatch://INFRA-4130, risk (b): a stall abort that finds the take pending holds its restart
// message for the take (`deferUntilSettled`). A take the daemon accepts starts the next run with
// its own text, so the message is not sent; one the daemon refuses or drops starts nothing, so the
// message is all that starts the worker's work again, and it goes then, once.
test("deferUntilSettled calls back once when a pending take is refused or dropped, and not when the take's text starts a run", async () => {
  // Refused by a hold: the value came during the run the abort ends, the run's end asks the take,
  // and the callback runs once the daemon's refusal has answered it, not before.
  const held = rig();
  held.take.ready();
  held.take.runStarted();
  const refusal = Promise.withResolvers<LegionTake>();
  held.takes.push({ late: refusal.promise });
  held.take.relayed(put(4, 12));
  await held.advance(0);
  let heldCalls = 0;
  expect(
    held.take.deferUntilSettled(() => {
      heldCalls += 1;
    })
  ).toBe(4);
  expect(held.take.runEnded(false)).toBeUndefined();
  await held.advance(0);
  expect(held.routed("take")).toEqual([body(4, 12)]);
  expect(heldCalls).toBe(0);
  refusal.resolve(refused("issue-in-turn"));
  await held.advance(0);
  expect(heldCalls).toBe(1);
  // Once: a later take refused again calls nothing more.
  held.take.relayed(put(4, 13));
  await held.advance(0);
  expect(held.routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(heldCalls).toBe(1);

  // Dropped: called once, at the drop's answer.
  const dropped = rig();
  dropped.take.ready();
  const drop = Promise.withResolvers<LegionTake>();
  dropped.takes.push({ late: drop.promise });
  dropped.take.relayed(put(4, 12));
  await dropped.advance(0);
  let droppedCalls = 0;
  expect(
    dropped.take.deferUntilSettled(() => {
      droppedCalls += 1;
    })
  ).toBe(4);
  await dropped.advance(0);
  expect(droppedCalls).toBe(0);
  drop.resolve(DROPPED);
  await dropped.advance(0);
  expect(droppedCalls).toBe(1);

  // Accepted: the take's text starts the next run, which is the worker working again, so nothing
  // is called, then or at that run's end.
  const accepted = rig();
  accepted.take.ready();
  const accept = Promise.withResolvers<LegionTake>();
  accepted.takes.push({ late: accept.promise });
  accepted.take.relayed(put(4, 12));
  await accepted.advance(0);
  let acceptedCalls = 0;
  expect(
    accepted.take.deferUntilSettled(() => {
      acceptedCalls += 1;
    })
  ).toBe(4);
  accept.resolve(ACCEPTED);
  await accepted.advance(0);
  expect(accepted.ran).toEqual([TEXT]);
  expect(acceptedCalls).toBe(0);
  accepted.take.runStarted();
  expect(accepted.take.runEnded(false)).toBe(4);
  await accepted.advance(0);
  expect(acceptedCalls).toBe(0);

  // Not pending: it answers undefined and keeps nothing, so a take refused later calls nothing.
  const idle = rig();
  idle.take.ready();
  let idleCalls = 0;
  expect(
    idle.take.deferUntilSettled(() => {
      idleCalls += 1;
    })
  ).toBeUndefined();
  idle.take.relayed(put(4, 12));
  await idle.advance(0);
  expect(idle.routed("take")).toEqual([body(4, 12)]);
  expect(idleCalls).toBe(0);
});

// The running task's own number written again while the host sets that task's text up, before its
// agent_start: the idle host asks the newer revision at once, and the task's turn starts before
// the daemon answers. That take brings no turn (an accepted answer's text runs nothing for the
// number whose turn is running, and a refusal `in-turn` leaves that number out), so the take in
// flight is no task's turn to come, as a value not asked about is not (R6): `pending` must say so,
// or the run's own settle withholds its continuation and a stall abort holds its restart.
test("pending() is false for a take in flight at a newer revision of the task whose turn is running", async () => {
  const { take, takes, ran, routed, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(ran).toEqual([TEXT]);
  takes.push({ silent: true });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  take.runStarted();
  expect(take.noteFor()).toBeUndefined();
  expect(take.pending()).toBe(false);
});
