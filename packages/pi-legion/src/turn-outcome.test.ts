import { expect, test } from "bun:test";
import {
  createLegionDaemonClient,
  DAEMON_ANSWER_TIMEOUT_MS,
  DAEMON_RESTART_FIRST_DELAY_MS,
} from "@legion/pi-shared/daemon-client";
import type { AgentEndEvent } from "@legion/pi-shared/pi-types";
import { taskStandIn } from "@legion/pi-shared/test/task-stand-in";
import {
  createTurnOutcomeReporter,
  type ReportedClaim,
  type TurnOutcomeLog,
  type TurnOutcomeReporter,
} from "./turn-outcome";

const SESSION = "ses_implementer_208";

/** The contract this build states on every request (`Legion-Plugin-Contract`). */
const CONTRACT = 60;

const implementer: ReportedClaim = {
  caller: {
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: SESSION,
    secret: "U2VjcmV0",
    generation: 3,
  },
  role: "implementer",
  issue: "LEGION-208",
};

interface Rig {
  readonly report: TurnOutcomeReporter["report"];
  readonly sessionEnded: (sessionID: string) => void;
  readonly posted: Record<string, unknown>[];
  readonly debugged: string[];
  readonly warned: string[];
  /** Every wait between two attempts of a report, in the order they were made. */
  readonly waits: number[];
  /** Every message the host's cyber-policy classifier was asked about, in order. */
  readonly classified: unknown[];
}

/**
 * The reporter over a daemon that answers `answer` to every report (a throw is a request that got
 * no answer), or answers each one as the daemon applies it. The clock is the test's own: a wait
 * between attempts passes at once, and an attempt's bound never passes, so an attempt waits for
 * its answer. The host's cyber-policy classifier is `cyberPolicyRefusal`, one that marks nothing
 * unless the case gives its own, and every message it is asked about is kept in `classified`.
 */
function rig(
  options: {
    readonly claim?: (sessionID: string) => ReportedClaim | undefined;
    readonly answer?: (attempt: number, body: Record<string, unknown>) => Response;
    readonly cyberPolicyRefusal?: (message: unknown) => Promise<boolean>;
  } = {}
): Rig {
  const classified: unknown[] = [];
  const posted: Record<string, unknown>[] = [];
  const debugged: string[] = [];
  const warned: string[] = [];
  const waits: number[] = [];
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (path !== "/legion/v1/claims/turn-outcome") throw new Error(`unexpected route ${path}`);
    const body = JSON.parse(String(init?.body));
    posted.push(body);
    return options.answer === undefined
      ? Response.json({ refusedRun: 0, failed: false })
      : options.answer(posted.length, body);
  }) as typeof fetch;
  const log: TurnOutcomeLog = {
    debug: (message) => debugged.push(message),
    warn: (message) => warned.push(message),
  };
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stub,
  });
  const reporter = createTurnOutcomeReporter({
    daemon: () => client,
    claim: options.claim ?? ((sessionID) => (sessionID === SESSION ? implementer : undefined)),
    log,
    wait: (ms, signal) => {
      if (ms === DAEMON_ANSWER_TIMEOUT_MS) {
        const bound = Promise.withResolvers<boolean>();
        signal.addEventListener("abort", () => bound.resolve(false), { once: true });
        return bound.promise;
      }
      waits.push(ms);
      return Promise.resolve(!signal.aborted);
    },
    cyberPolicyRefusal: async (message) => {
      classified.push(message);
      return options.cyberPolicyRefusal === undefined ? false : options.cyberPolicyRefusal(message);
    },
  });
  return {
    report: reporter.report,
    sessionEnded: reporter.sessionEnded,
    posted,
    debugged,
    warned,
    waits,
    classified,
  };
}

/** A run whose last assistant message settled normally. */
const clean: AgentEndEvent = {
  messages: [{ role: "user" }, { role: "assistant", stopReason: "stop" }, { role: "toolResult" }],
};

test("a turn the model answered is reported clean, under the claim's own credentials", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean);

  expect(posted).toEqual([
    {
      claimToken: "legion-legion-legion-208-implementer",
      sessionId: SESSION,
      secret: "U2VjcmV0",
      generation: 3,
      sequence: 1,
      outcome: "clean",
    },
  ]);
});

// What dispatch://INFRA-264's implementer did on every turn: Oh My Pi ended the run with the last
// assistant message stopped at `error`, and the provider's words beside it.
test("a turn the provider refused is reported refused, with the provider's own message", async () => {
  const { report, posted } = rig();

  await report(SESSION, {
    messages: [
      { role: "assistant", stopReason: "stop" },
      { role: "assistant", stopReason: "error", errorMessage: "Overloaded: the model is busy" },
    ],
  });

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    message: "Overloaded: the model is busy",
  });
});

// The first run after an accepted take is the task's turn, and its report is what finishes the
// task on the daemon: it names the task's number.
test("the report of a take's turn names the task's number, and a turn of no task names none", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean, 4);
  await report(SESSION, clean);

  expect(posted[0]).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: SESSION,
    secret: "U2VjcmV0",
    generation: 3,
    sequence: 1,
    outcome: "clean",
    task: 4,
  });
  expect(posted[1]).not.toHaveProperty("task");
});

// A run that ended while the claim's take held an accepted take's text unrun names that take as
// held, which tells the daemon for certain the run was not the task's turn; a report that names
// none holds nothing, as every report before the field did.
test("the report of a run that held a take's text names it held, and a report of none names nothing held", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean, undefined, 4);
  await report(SESSION, clean, 4);

  expect(posted[0]).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: SESSION,
    secret: "U2VjcmV0",
    generation: 3,
    sequence: 1,
    outcome: "clean",
    held: 4,
  });
  expect(posted[1]).not.toHaveProperty("held");
});

// A provider error keeps the task under its number on the daemon, which decides by the status
// alone; the provider and the model are what its lines and the controller's notice name.
test("a provider error's report names its status, provider and model beside the task", async () => {
  const { report, posted } = rig();

  await report(
    SESSION,
    {
      messages: [
        {
          role: "assistant",
          stopReason: "error",
          errorMessage: "529 overloaded_error",
          errorStatus: 529,
          provider: "anthropic",
          model: "claude-opus-4-5",
        },
      ],
    },
    4
  );
  // A content filter's refusal comes with no status at all.
  await report(SESSION, {
    messages: [{ role: "assistant", stopReason: "error", errorMessage: "Refusal (cyber)" }],
  });

  expect(posted[0]).toMatchObject({
    outcome: "refused",
    message: "529 overloaded_error",
    task: 4,
    providerError: {
      status: 529,
      provider: "anthropic",
      model: "claude-opus-4-5",
      message: "529 overloaded_error",
      retryable: true,
    },
  });
  expect(posted[1]).toMatchObject({
    outcome: "refused",
    providerError: {
      status: 0,
      provider: "",
      model: "",
      message: "Refusal (cyber)",
      retryable: false,
    },
  });
  expect(posted[1]).not.toHaveProperty("task");
});

/** A refusal as Oh My Pi records it, with the status the provider answered where it gave one. */
function refusedWith(errorStatus?: number): AgentEndEvent {
  return {
    messages: [
      {
        role: "assistant",
        stopReason: "error",
        errorMessage: "the provider refused the request",
        ...(errorStatus === undefined ? {} : { errorStatus }),
      },
    ],
  };
}

// The status is the host's own field (`errorStatus` on the refused assistant message), carried in
// the report's provider error, and never read out of the words: a refusal whose message names a
// status but whose message carries no `errorStatus` goes with none, as does a number that is no
// HTTP status. The status rides in `providerError` alone: this plugin sends no `httpStatus`, the
// field of a plugin from before the task's number, below the contract of the task-take entry
// (`contract_20261125171504_task-take.go`)
// (dispatch://INFRA-4508).
test("a refused turn's provider error carries the status the host recorded, and only that", async () => {
  const { report, posted } = rig();

  await report(SESSION, refusedWith(529));
  await report(SESSION, refusedWith(400));
  await report(SESSION, {
    messages: [{ role: "assistant", stopReason: "error", errorMessage: "429 rate limit" }],
  });
  await report(SESSION, refusedWith(0));
  await report(SESSION, refusedWith(1000));
  await report(SESSION, {
    messages: [{ role: "assistant", stopReason: "stop", errorStatus: 500 }],
  });

  const statuses = posted.map((body) => [
    body.outcome,
    (body.providerError as { status?: number } | undefined)?.status,
  ]);
  expect(statuses).toEqual([
    ["refused", 529],
    ["refused", 400],
    ["refused", 0],
    ["refused", 0],
    ["refused", 0],
    ["clean", undefined],
  ]);
  for (const body of posted) expect(body).not.toHaveProperty("httpStatus");
});

// A turn that was not the take's — a phase reminder the plugin starts itself, an Envoy message, a
// background job's result — names no task, which is how this plugin says what the `unprompted`
// mark of a plugin from before the task's number says: the daemon judges that turn from the
// report alone (dispatch://INFRA-4508's R4). The take's own turn names its task. Neither report is
// ever marked unprompted.
test("a turn that was not the take's names no task, a take's turn names its task, and neither is marked unprompted", async () => {
  const { report, posted } = rig();

  await report(SESSION, refusedWith(529), 4);
  await report(SESSION, refusedWith(529));

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    task: 4,
    providerError: { status: 529 },
  });
  expect(posted[1]).toMatchObject({
    sequence: 2,
    outcome: "refused",
    providerError: { status: 529 },
  });
  expect(posted[1]).not.toHaveProperty("task");
  for (const body of posted) expect(body).not.toHaveProperty("unprompted");
});

// The sequence is this producer's own: it is what keys the outcome the daemon stores, so two
// turns must never share one, and the order it gives them out is the order the turns ended in.
test("the sequence counts this plugin's own turns, from one and in order, within the generation", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean);
  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  await report(SESSION, clean);

  expect(posted.map((body) => body.sequence)).toEqual([1, 2, 3]);
  expect(posted.map((body) => body.outcome)).toEqual(["clean", "refused", "clean"]);
  expect(posted.every((body) => body.generation === 3)).toBe(true);
});

// Reports that overlap still take their numbers in the order their turns ended: the number is
// taken before anything is awaited, so the daemon can order them however late they arrive. A
// refused turn waits on the host's classifier before it is sent, so it can arrive after a later
// clean one; what this holds is the number each turn took, whatever order the bodies came in.
test("overlapping reports take their numbers in the order the turns ended", async () => {
  const { report, posted } = rig();

  const refused = report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  const answered = report(SESSION, clean);
  await Promise.all([refused, answered]);

  const numbered = posted.map((body) => [body.sequence, body.outcome]);
  expect(numbered.toSorted(([a], [b]) => Number(a) - Number(b))).toEqual([
    [1, "refused"],
    [2, "clean"],
  ]);
});

// A `task` subagent loads an instance of this module of its own and registers no claim, and the
// pane's own instance is asked before its registration has answered.
test("a session holding no claim reports nothing and spends no sequence", async () => {
  const { report, posted } = rig();

  await report("ses_subagent", clean);
  await report(SESSION, clean);

  expect(posted).toHaveLength(1);
  expect(posted[0]).toMatchObject({ sequence: 1 });
});

// A refusal the daemon itself gives will not pass on a retry, so the turn goes uncounted and the
// pane says so once: a report dropped in silence is a run of refusals that comes up short.
test("a report the daemon refuses is warned about, and the next turn still reports", async () => {
  const { report, posted, warned } = rig({
    answer: (attempt) =>
      attempt === 1
        ? Response.json({ error: "sequence is required" }, { status: 400 })
        : Response.json({ refusedRun: 1, failed: false }),
  });

  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });

  expect(warned).toEqual(["legion: a turn's outcome did not reach the daemon"]);
  expect(posted.map((body) => body.sequence)).toEqual([1, 2]);
});

/** A request that reached no daemon, as Bun's fetch raises it, carrying `code`. */
const transport = (code: string): Error =>
  Object.assign(new TypeError(`${code}: the request reached no daemon`), { code });

// Only the report finishes a current worker's task, so it is sent
// until the daemon answers, by the take's own policy. An uncoded 504 (a proxy's timeout) and a DNS
// failure are two of the failures the earlier retry gave up on at once.
test("a report whose first two attempts meet an uncoded 504 and a DNS failure is sent again, the same report, and delivered on the third", async () => {
  const { report, posted, debugged, warned, waits } = rig({
    answer: (attempt) => {
      if (attempt === 1) return new Response("upstream request timeout", { status: 504 });
      if (attempt === 2) throw transport("ENOTFOUND");
      return Response.json({ refusedRun: 0, failed: false });
    },
  });

  await report(SESSION, clean, 4);

  expect(posted).toHaveLength(3);
  expect(posted[0]).toMatchObject({ sequence: 1, outcome: "clean", task: 4 });
  expect(posted[1]).toEqual(posted[0]);
  expect(posted[2]).toEqual(posted[0]);
  expect(waits).toEqual([DAEMON_RESTART_FIRST_DELAY_MS, DAEMON_RESTART_FIRST_DELAY_MS * 2]);
  expect(debugged).toEqual(["legion: reported a turn's outcome"]);
  expect(warned).toEqual([
    "legion: claims/turn-outcome failed; it is sent again until the daemon answers it",
  ]);
});

// A failure that carries no answer of the daemon is sent again whatever the transport names it,
// not only where a list names it: an attempt that never reached the daemon applied nothing, and
// one that did is applied once however often it comes. Here a code Bun gives a socket it could
// not open, a code no list holds, and an error with no code.
test("a report whose attempts meet transport failures no list names is sent again, the same report, and delivered", async () => {
  const tasks = taskStandIn();
  const value = tasks.give("Implement LEGION-208 on its branch.");
  const { report, posted, debugged, waits } = rig({
    answer: (attempt, body) => {
      if (attempt === 1) throw transport("FailedToOpenSocket");
      if (attempt === 2) throw transport("ECONNABORTED");
      if (attempt === 3) throw new Error("the socket hung up");
      tasks.answer("/legion/v1/claims/turn-outcome", body);
      return Response.json({ refusedRun: 0, failed: false });
    },
  });

  expect(await report(SESSION, clean, value.number)).toBe("recorded");

  expect(posted).toHaveLength(4);
  expect(posted.slice(1)).toEqual([posted[0], posted[0], posted[0]]);
  expect(waits).toEqual([
    DAEMON_RESTART_FIRST_DELAY_MS,
    DAEMON_RESTART_FIRST_DELAY_MS * 2,
    DAEMON_RESTART_FIRST_DELAY_MS * 4,
  ]);
  expect(tasks.registered()).toEqual({ task: null, finished: value.number });
  expect(debugged).toEqual(["legion: reported a turn's outcome"]);
});

// The daemon answers a report whose store write it could not make with a 500 that names no code,
// and applies a repeated report once. Here the first attempt is that 500, the second is applied
// but its answer is lost to a reset connection, and the third is answered: the same report is
// posted three times, and the task it names is finished.
test("an end report answered a 500, then applied with its answer lost to a reset, is posted a third time and its task is finished", async () => {
  const tasks = taskStandIn();
  const value = tasks.give("Implement LEGION-208 on its branch.");
  const { report, posted, debugged } = rig({
    answer: (attempt, body) => {
      if (attempt === 1) {
        return Response.json(
          { error: "turn outcome failed: the daemon could not record it" },
          { status: 500 }
        );
      }
      tasks.answer("/legion/v1/claims/turn-outcome", body);
      if (attempt === 2) throw transport("ECONNRESET");
      return Response.json({ refusedRun: 0, failed: false });
    },
  });

  await report(SESSION, clean, value.number);

  expect(posted).toHaveLength(3);
  expect(posted[0]).toMatchObject({ sequence: 1, task: value.number });
  expect(posted[1]).toEqual(posted[0]);
  expect(posted[2]).toEqual(posted[0]);
  expect(tasks.registered()).toEqual({ task: null, finished: value.number });
  expect(debugged).toEqual(["legion: reported a turn's outcome"]);
});

test("a report still being sent when its session ends sends nothing more, and says it did not reach the daemon", async () => {
  const { report, sessionEnded, posted, warned } = rig({
    answer: () => {
      throw transport("ECONNREFUSED");
    },
  });

  const sending = report(SESSION, clean, 4);
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  sessionEnded(SESSION);
  // Counted at the end, before anything else settles: an attempt posted after it is one the end
  // did not stop.
  const attempts = posted.length;
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();

  expect(attempts).toBeGreaterThan(0);
  expect(posted).toHaveLength(attempts);
  await sending;
  expect(warned.at(-1)).toBe("legion: a turn's outcome did not reach the daemon");
});

/** OpenAI's cyber-policy refusal as Oh My Pi records it: a gateway's 503, or none for a request
 * the host held after one. The words are the provider's, which this plugin never reads itself. */
function cyberRefused(errorStatus?: number): AgentEndEvent {
  return {
    messages: [
      {
        role: "assistant",
        stopReason: "error",
        errorMessage: "synthetic refusal: the request was flagged for cyber_policy",
        ...(errorStatus === undefined ? {} : { errorStatus }),
      },
    ],
  };
}

/** A host classifier that marks exactly the messages `cyberRefused` builds. */
const marksCyberRefusals = async (message: unknown) =>
  typeof message === "object" &&
  message !== null &&
  "errorMessage" in message &&
  typeof message.errorMessage === "string" &&
  message.errorMessage.includes("cyber_policy");

// The daemon reads every turn from its report, and the status of a cyber-policy refusal says
// nothing it can act on: a gateway relays it as a 503, and a request the host held after one has
// none. So the report carries the class the host's own classifier gives the refused message
// (`isCyberPolicyRefusal`, the reading that sets a prompt result's `code`), and the daemon fails
// the claim on it rather than giving the session another task.
test("a refused turn the host classes as OpenAI's cyber-policy refusal is reported with its code", async () => {
  const { report, posted, classified } = rig({ cyberPolicyRefusal: marksCyberRefusals });

  await report(SESSION, cyberRefused(503));
  await report(SESSION, cyberRefused());

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    providerError: { status: 503 },
    code: "cyber_policy",
  });
  expect(posted[0]).not.toHaveProperty("task");
  expect(posted[0]).not.toHaveProperty("unprompted");
  expect(posted[0]).not.toHaveProperty("httpStatus");
  expect(posted[1]).toMatchObject({
    sequence: 2,
    outcome: "refused",
    providerError: { status: 0 },
    code: "cyber_policy",
  });
  // The classifier is handed the assistant message that ended the turn, as the host hands it.
  expect(classified).toEqual([cyberRefused(503).messages?.[0], cyberRefused().messages?.[0]]);
});

// Every other refusal goes with no class, and an answered turn is asked nothing: the daemon reads
// one class alone, and a turn the provider took is no refusal of any kind.
test("a refusal the host does not class, and an answered turn, are reported with no code", async () => {
  const { report, posted, classified } = rig({ cyberPolicyRefusal: marksCyberRefusals });

  await report(SESSION, refusedWith(400));
  await report(SESSION, clean);

  expect(posted.map((body) => [body.outcome, body.code])).toEqual([
    ["refused", undefined],
    ["clean", undefined],
  ]);
  for (const body of posted) expect(body).not.toHaveProperty("code");
  expect(classified).toHaveLength(1);
});

// A take's turn carries the class beside its task number and providerError: the daemon takes the
// task back and fails the claim under cyber_policy rather than putting it on the ladder.
test("a take's turn the host classes as a cyber-policy refusal carries the code beside its task", async () => {
  const { report, posted } = rig({ cyberPolicyRefusal: marksCyberRefusals });

  await report(SESSION, cyberRefused(403), 4);

  expect(posted[0]).toMatchObject({
    outcome: "refused",
    task: 4,
    providerError: { status: 403 },
    code: "cyber_policy",
  });
  expect(posted[0]).not.toHaveProperty("unprompted");
  expect(posted[0]).not.toHaveProperty("httpStatus");
});

// The classifier is the host's module, read when the first refusal comes; a host that cannot
// answer still has the turn reported, with no class, and the pane says why once per failure.
test("a classifier that fails leaves the refused turn reported with no code, and warns", async () => {
  const { report, posted, warned } = rig({
    cyberPolicyRefusal: async () => {
      throw new Error("Cannot find module '@oh-my-pi/pi-ai/error'");
    },
  });

  await report(SESSION, cyberRefused(503));

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    providerError: { status: 503 },
  });
  expect(posted[0]).not.toHaveProperty("code");
  expect(warned).toEqual([
    "legion: the host could not class a refused turn; it is reported with no code",
  ]);
});
