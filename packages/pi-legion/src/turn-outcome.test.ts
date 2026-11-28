import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent } from "@legion/pi-shared/pi-types";
import {
  createTurnOutcomeReporter,
  type TurnOutcomeLog,
  type TurnOutcomeReporter,
} from "./turn-outcome";

const SESSION = "ses_implementer_208";

/** The contract this build states on every request (`Legion-Plugin-Contract`). */
const CONTRACT = 60;

const implementer: ClaimCapability = {
  sessionID: SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 3,
};

interface Rig {
  readonly report: TurnOutcomeReporter["report"];
  readonly inputArrived: TurnOutcomeReporter["inputArrived"];
  readonly runStarted: TurnOutcomeReporter["runStarted"];
  /** Every body posted to `claims/turn-outcome`, in the order the daemon received it. */
  readonly posted: Record<string, unknown>[];
  readonly debugged: string[];
  readonly warned: string[];
  /** Every message the host's cyber-policy classifier was asked about, in order. */
  readonly classified: unknown[];
}

/** The reporter over a daemon that answers `answer` to every report, or refuses it. The host's
 * cyber-policy classifier is `cyberPolicyRefusal`, one that marks nothing unless the case gives
 * its own, and every message it is asked about is kept in `classified`. */
function rig(
  options: {
    readonly claim?: (sessionID: string) => ClaimCapability | undefined;
    readonly answer?: (attempt: number) => Response;
    readonly cyberPolicyRefusal?: (message: unknown) => Promise<boolean>;
  } = {}
): Rig {
  const classified: unknown[] = [];
  const posted: Record<string, unknown>[] = [];
  const debugged: string[] = [];
  const warned: string[] = [];
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (path !== "/legion/v1/claims/turn-outcome") throw new Error(`unexpected route ${path}`);
    posted.push(JSON.parse(String(init?.body)));
    return options.answer === undefined
      ? Response.json({ refusedRun: 0, failed: false })
      : options.answer(posted.length);
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
    cyberPolicyRefusal: async (message) => {
      classified.push(message);
      return options.cyberPolicyRefusal === undefined ? false : options.cyberPolicyRefusal(message);
    },
  });
  return {
    report: reporter.report,
    inputArrived: reporter.inputArrived,
    runStarted: reporter.runStarted,
    posted,
    debugged,
    warned,
    classified,
  };
}

/** A run whose last assistant message settled normally. */
const clean: AgentEndEvent = {
  messages: [{ role: "user" }, { role: "assistant", stopReason: "stop" }, { role: "toolResult" }],
};

// The daemon's prompt arrives as RPC input, and the run that starts next is that prompt's: its
// result judges the turn, so the report says nothing more than how it ended.
test("a turn the daemon's prompt ran is reported clean, under the claim's own credentials", async () => {
  const { report, inputArrived, runStarted, posted } = rig();

  inputArrived(SESSION, { text: "Continue the task.", source: "rpc" });
  runStarted(SESSION);
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

/** One step of Oh My Pi's own retry or model fallback: a refusal it goes on from. */
const step: AgentEndEvent = {
  willContinue: true,
  messages: [{ role: "assistant", stopReason: "error", errorMessage: "429 rate limit" }],
};

// What INFRA-764's workers did on 2026-11-23
// (dispatch://INFRA-4508/comment/fd89b8eb-d326-46d6-82b6-6ae84fe76f67): at each step of its
// fallback chain Oh My Pi ends the run with `willContinue`, the refused attempt's message last, and
// goes on to the next model. The steps and the end that settles them are one turn.
test("a refusal Oh My Pi goes on from reports nothing and spends no sequence", async () => {
  const { report, posted } = rig();

  await report(SESSION, step);
  await report(SESSION, step);
  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });

  expect(posted.map((body) => [body.sequence, body.outcome])).toEqual([[1, "refused"]]);
});

// A fallback walk whose next model answers is one turn, and an answered one: the refused step
// reports nothing, so the one report the turn makes is clean, and it ends the daemon's run of
// refusals rather than adding one to it.
test("a refusal Oh My Pi goes on from, then an answered end, reports one clean turn", async () => {
  const { report, posted } = rig();

  await report(SESSION, step);
  await report(SESSION, clean);

  expect(posted).toHaveLength(1);
  expect(posted[0]).toMatchObject({ sequence: 1, outcome: "clean" });
  expect(posted[0]).not.toHaveProperty("message");
});

// The host also goes on from an answered turn, as when the phase reminder continues it: that turn
// ended clean, and it ends the run of refusals before it.
test("an answered turn the host goes on from is reported clean", async () => {
  const { report, posted } = rig();

  await report(SESSION, { ...clean, willContinue: true });

  expect(posted.map((body) => [body.sequence, body.outcome])).toEqual([[1, "clean"]]);
});

// A turn that ends waiting only on a background job's result has settled: the pane is idle until
// the result starts the next run.
test("a refusal that waits only on a background job's result is reported", async () => {
  const { report, posted } = rig();

  await report(SESSION, { ...step, awaitingAsyncWork: true });

  expect(posted.map((body) => [body.sequence, body.outcome])).toEqual([[1, "refused"]]);
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

// The status is the host's own field, the one its `prompt_result` copies into `httpStatus`
// (`errorStatus` on the refused assistant message), and never read out of the words: a refusal
// whose message names a status but whose message carries no `errorStatus` goes with none, as does
// a number that is no HTTP status, which the contract does not admit and the daemon would take as
// no status with a warning.
test("a refused turn's report carries the status the host recorded, and only that", async () => {
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

  expect(posted.map((body) => [body.outcome, body.httpStatus])).toEqual([
    ["refused", 529],
    ["refused", 400],
    ["refused", undefined],
    ["refused", undefined],
    ["refused", undefined],
    ["clean", undefined],
  ]);
  for (const body of posted.slice(2)) expect(body).not.toHaveProperty("httpStatus");
});

// A phase reminder is a run the plugin starts itself (`pi.sendMessage`): no prompt the daemon sent
// came before it, so no prompt result will come for it, and its report says so. The daemon judges
// that turn from the report; the turn the daemon's prompt ran is judged by the prompt's result.
test("a turn no prompt of the daemon's ran is reported unprompted, and a prompt's turn is not", async () => {
  const { report, inputArrived, runStarted, posted } = rig();

  inputArrived(SESSION, { text: "Continue the task.", source: "rpc" });
  runStarted(SESSION);
  await report(SESSION, refusedWith(529));
  runStarted(SESSION);
  await report(SESSION, refusedWith(529));

  expect(posted[0]).toMatchObject({ sequence: 1, outcome: "refused", httpStatus: 529 });
  expect(posted[0]).not.toHaveProperty("unprompted");
  expect(posted[1]).toMatchObject({
    sequence: 2,
    outcome: "refused",
    httpStatus: 529,
    unprompted: true,
  });
});

// A person's Send from Dispatch runs as typed input (`sendUserInput`, `source: "extension"`), and
// no prompt result reports it: only the daemon's own prompt, which arrives over RPC, does.
test("input that is not the daemon's prompt leaves the run it starts unprompted", async () => {
  const { report, inputArrived, runStarted, posted } = rig();

  inputArrived(SESSION, { text: "What is the CI saying?", source: "extension" });
  runStarted(SESSION);
  await report(SESSION, clean);

  expect(posted[0]).toMatchObject({ outcome: "clean", unprompted: true });
});

// The host reports a prompt's result at the first end it does not go on from itself, whatever
// waits to run next (`rpc-prompt-results.ts:164-169` at the pinned tag): a retry and a hook's
// continuation carry the prompt's run on, so every report up to that end is the prompt's, and the
// run after it is not.
test("a prompt's turn runs through the ends the host goes on from, up to the end its result reports", async () => {
  const { report, inputArrived, runStarted, posted } = rig();

  inputArrived(SESSION, { text: "Continue the task.", source: "rpc" });
  runStarted(SESSION);
  await report(SESSION, { ...clean, willContinue: true });
  runStarted(SESSION);
  await report(SESSION, step);
  await report(SESSION, refusedWith(503));
  runStarted(SESSION);
  await report(SESSION, refusedWith(503));

  expect(posted.map((body) => [body.sequence, body.unprompted ?? false])).toEqual([
    [1, false],
    [2, false],
    [3, true],
  ]);
});

// A prompt the daemon sends while a turn of the plugin's own runs waits for that turn to end, and
// the host's result reports the run that starts after the prompt, never the one already going.
test("a prompt that arrives during an unprompted turn is the next turn's, not that one's", async () => {
  const { report, inputArrived, runStarted, posted } = rig();

  runStarted(SESSION);
  inputArrived(SESSION, { text: "Continue the task.", source: "rpc" });
  await report(SESSION, refusedWith(529));
  runStarted(SESSION);
  await report(SESSION, clean);

  expect(posted.map((body) => [body.sequence, body.unprompted ?? false])).toEqual([
    [1, true],
    [2, false],
  ]);
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

// The daemon reads a turn with no prompt result from its report alone, and the status of a
// cyber-policy refusal says nothing it can act on: a gateway relays it as a 503, and a request
// the host held after one has none. So the report carries the class the host's own classifier
// gives the refused message (`isCyberPolicyRefusal`, the reading that sets a prompt result's
// `code`), and the daemon fails the claim on it rather than sending the session another prompt.
test("a refused turn the host classes as OpenAI's cyber-policy refusal is reported with its code", async () => {
  const { report, posted, classified } = rig({ cyberPolicyRefusal: marksCyberRefusals });

  await report(SESSION, cyberRefused(503));
  await report(SESSION, cyberRefused());

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    httpStatus: 503,
    unprompted: true,
    code: "cyber_policy",
  });
  expect(posted[1]).toMatchObject({ sequence: 2, outcome: "refused", code: "cyber_policy" });
  expect(posted[1]).not.toHaveProperty("httpStatus");
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

// A prompt's turn carries the class too: the daemon judges that turn by its prompt's result and
// reads nothing of the report's class there, so the report says what the turn was and no more.
test("a prompt's turn the host classes as a cyber-policy refusal carries the code, unmarked unprompted", async () => {
  const { report, inputArrived, runStarted, posted } = rig({
    cyberPolicyRefusal: marksCyberRefusals,
  });

  inputArrived(SESSION, { text: "Continue the task.", source: "rpc" });
  runStarted(SESSION);
  await report(SESSION, cyberRefused(403));

  expect(posted[0]).toMatchObject({ outcome: "refused", httpStatus: 403, code: "cyber_policy" });
  expect(posted[0]).not.toHaveProperty("unprompted");
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
    httpStatus: 503,
    unprompted: true,
  });
  expect(posted[0]).not.toHaveProperty("code");
  expect(warned).toEqual([
    "legion: the host could not class a refused turn; it is reported with no code",
  ]);
});
