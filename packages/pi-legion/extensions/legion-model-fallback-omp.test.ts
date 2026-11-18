import { afterEach, expect, test } from "bun:test";
import {
  type FallbackCooldownState,
  LEGION_FALLBACK_ENTRY,
} from "@legion/pi-shared/fallback-cooldown";
import {
  type Block,
  type Cleanup,
  call,
  type LegionPane,
  messageStream,
  ompNatsGuard,
  type Refusal,
  type Request,
  refusalStream,
  runLegionPane,
  type StandinProvider,
} from "@legion/pi-shared/test/omp-harness";

// The model fallback on the real Oh My Pi (src/model-fallback.ts, on the cooldown machine and the
// chain resolver of @legion/pi-shared): only the real binary shows which model each request went
// to across a provider's refusal — the refusals the host walks its own chain for, the ones it
// leaves where they are, when the `context` hook puts the session back inside a turn already
// running, and that the transcript keeps the arming a relaunched worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which
// CI's pi-legion job installs; on the devbox, `mise where <pin>`/bin/omp. LEGION_TEST_NATS_URL
// names the NATS server the pane's Envoy extension connects to: a session is given no consumer
// without one, and its claim boot then ends the session (INFRA-771). A run without both skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (`ompNatsGuard`).
const { skip, setup } = ompNatsGuard();

/** The pane's own models: the one every model role names, the one its chain steps to, and the
 * one in between, for the case whose chain has two steps. */
const STRONG = "standin/strong-model";
const MIDDLE = "standin/middle-model";
const WEAK = "standin/weak-model";

/** One stand-in serves them all, so a case tells them apart by the `model` of the request it
 * answers. The first model of the first provider is what every model role runs on
 * (`writeStandinProfile`), so the stronger model stays first. */
const PROVIDERS: readonly StandinProvider[] = [
  { provider: "standin", models: ["strong-model", "weak-model", "middle-model"] },
];

/**
 * The pane's overlay, in the shape INFRA's own operator overlay carries
 * (deploy/kubernetes/operator-route/overlay.yml, docs/kubernetes.md "Operator configuration"):
 * the model fallback on, the chain the case runs under, and no revert of the host's own — the
 * return is the plugin's.
 */
function overlay(chains: readonly string[]): readonly string[] {
  return [
    "retry:",
    "  modelFallback: true",
    "  fallbackRevertPolicy: never",
    "  fallbackChains:",
    ...chains,
  ];
}

/** One step: the chain every case but the two-refusal walk runs under. */
const ONE_STEP: readonly string[] = [`    ${STRONG}: [${WEAK}]`];

/** Two steps, each model keyed on its own: the host's own chain resolution keys a walk by the
 * model the session runs now (`resolveRetryFallbackChainKey`, `session/retry-fallback-chains.ts`
 * at the pinned tag), so the middle model needs a key of its own to be walked from. */
const TWO_STEPS: readonly string[] = [
  `    ${STRONG}: [${MIDDLE}, ${WEAK}]`,
  `    ${MIDDLE}: [${WEAK}]`,
];

/**
 * The first wait a refusal of one selector buys, through `LEGION_REFUSAL_COOLDOWN_MS`
 * (`fallbackCooldownLimits`): seconds here, an hour in a deployment, so every case below sees its
 * waits run out while it watches.
 */
const COOLDOWN_MS = 4_000;

/** The cooldown the killed-and-resumed case arms: long enough that a restored arming is still
 * running when the replacement pane is relaunched, short enough for the return to come while the
 * case watches. The case reads the arming and the return off the pane's own records, so the
 * relaunch's own boot, however long a loaded box makes it, decides nothing. */
const RESUME_COOLDOWN_MS = 12_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A shell sleep the model asks for, which keeps one turn running while a cooldown runs out: the
 * requests of that same turn are the ones the `context` hook restores on. */
function sleeps(seconds: number): Block {
  return call("bash", { command: `sleep ${seconds}` });
}

/** The gateway's answer to one turn: the blocks, as an Anthropic event stream. */
function replies(blocks: readonly Block[], id: string): Response {
  return new Response(messageStream(blocks, id), {
    headers: { "content-type": "text/event-stream" },
  });
}

/** The gateway's answer to one turn: a content filter's refusal, after `refusal.text` when it
 * names any. */
function refuses(id: string, refusal: Refusal = {}): Response {
  return new Response(refusalStream(id, refusal), {
    headers: { "content-type": "text/event-stream" },
  });
}

/**
 * The gateway's answer to one request of a long turn that waits for the return: on the weaker
 * model it asks again, after a shell sleep of `seconds`, so the model of each request says where
 * the session stood at that moment; the first request on any other model is the return, wherever
 * it went, and ends the turn. The turn ends on that fact, not on a window of the wall clock, so a
 * pane a loaded box slows still asks until it is back, and the case's assertions judge when and to
 * which model it came back.
 */
function asksUntilBack(model: string, nth: number, seconds: number): Response {
  return model === "weak-model"
    ? replies([sleeps(seconds)], `ping_${nth}`)
    : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
}

/** The text of every user message in a Messages request, joined: which assignment the turn that
 * carried it was started by. */
function userText(request: Request): string {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages
    .flatMap((message) => {
      if (typeof message !== "object" || message === null) return [];
      if (!("role" in message) || message.role !== "user") return [];
      if (!("content" in message)) return [];
      return [JSON.stringify(message.content)];
    })
    .join("\n");
}

/** One turn the gateway served: the model it was sent to. Its place in `served` is the order the
 * turns arrived in, which is all a case reads of when they came. */
interface Served {
  readonly model: string;
}

/** One record the pane's runner wrote to its transcript: the cooldown state after a change, and
 * when the host wrote it (`at`, epoch ms), on the pane's own clock, the clock its arming reads. */
interface CooldownRecord {
  readonly at: number;
  readonly state: FallbackCooldownState;
}

/** What a settled fallback pane left behind, beside every pane's own record. */
interface FallbackPane extends LegionPane {
  /** Every turn the gateway served, in order. */
  readonly served: readonly Served[];
  /** The model of every turn the gateway served, in order. */
  readonly models: () => string[];
  /** The turns after the `from`th (from 0) up to the first after it that went to the stronger
   * model, which is the turn the session came back on; throws when none did. */
  readonly beforeReturn: (from: number) => readonly Served[];
  /** The cooldown states the pane recorded in its transcript, in order. */
  readonly cooldowns: () => Promise<FallbackCooldownState[]>;
  /** The same, each with the moment the pane wrote it. */
  readonly records: () => Promise<CooldownRecord[]>;
}

/**
 * Runs one root architect's pane on the real Oh My Pi (`runLegionPane`) under the chain overlay,
 * its gateway answering each turn from `answer`, until every assignment's run has settled. A root
 * architect holds a Legion claim, which is all the model fallback gates on, and is the one role
 * the phase reminder leaves alone, so nothing but the case's own assignments starts a turn.
 */
async function fallbackPane(options: {
  readonly prompt: readonly string[];
  /** Answers the turn the gateway was sent: `model` is the request's own model id, and `nth`
   * counts the turns of the whole pane, its second life included, from 1. */
  readonly answer: (model: string, nth: number, request: Request) => Response;
  /** `LEGION_REFUSAL_COOLDOWN_MS`; `COOLDOWN_MS` by default. */
  readonly cooldownMs?: number;
  /** The `fallbackChains` lines of the pane's overlay; `ONE_STEP` by default. */
  readonly chains?: readonly string[];
}): Promise<FallbackPane> {
  const { binary, nats } = setup();
  const served: Served[] = [];
  const pane = await runLegionPane(
    binary,
    [],
    {
      name: "fallback",
      role: "architect",
      tree: "FALL-1",
      issue: "FALL-1",
      nats,
      prompt: options.prompt,
      config: overlay(options.chains ?? ONE_STEP),
      providers: PROVIDERS,
      env: {
        LEGION_REFUSAL_COOLDOWN_MS: String(options.cooldownMs ?? COOLDOWN_MS),
        // Well above every wait a case asks for, so the doubling is what decides them.
        LEGION_REFUSAL_COOLDOWN_MAX_MS: "600000",
      },
      answer: (url, body) => {
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        const model = String(body.model ?? "");
        served.push({ model });
        return options.answer(model, served.length, { path: url.pathname, body });
      },
    },
    cleanup
  );
  const records = async (): Promise<CooldownRecord[]> =>
    (await pane.transcriptRecords(LEGION_FALLBACK_ENTRY)).map(({ at, data }) => ({
      at,
      state: data as FallbackCooldownState,
    }));
  return {
    ...pane,
    served,
    models: () => served.map((turn) => turn.model),
    beforeReturn: (from) => {
      const back = served.findIndex((turn, index) => index > from && turn.model === "strong-model");
      if (back === -1)
        throw new Error(`no turn after the ${from}th went back to the stronger model`);
      return served.slice(from + 1, back);
    },
    cooldowns: async () => (await records()).map((record) => record.state),
    records,
  };
}

/** The wait `armed`'s arming bought, by the pane's own record: its due time less the moment it was
 * armed. */
function waitOf(armed: CooldownRecord | undefined): number {
  const arming = armed?.state.arming;
  if (arming?.armedAt === undefined) throw new Error("the pane recorded no arming with its moment");
  return arming.dueAt - arming.armedAt;
}

/** The record of the return `armed`'s arming waited for, by the pane's own record: the first after
 * it that holds no arming, which the runner writes once it has put the session back. */
function returnAfter(
  records: readonly CooldownRecord[],
  armed: CooldownRecord | undefined
): CooldownRecord {
  const from = armed === undefined ? -1 : records.indexOf(armed);
  if (from === -1) throw new Error("the arming is not one of the pane's records");
  const returned = records.slice(from + 1).find((record) => record.state.arming === undefined);
  if (returned === undefined) throw new Error("the pane recorded no return after its arming");
  return returned;
}

test.skipIf(skip)(
  "a refusal before any output is retried on the chain's next model, which is the host's own walk",
  async () => {
    const pane = await fallbackPane({
      // The second assignment is one long turn that asks the gateway every second until the
      // session is back, so the model of each of its requests says where the session stood at
      // that moment, and the case sees both sides of the wait the refusal bought.
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (model, nth) => {
        if (nth === 1) return refuses("refusal_1");
        if (nth === 2) return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
        return asksUntilBack(model, nth, 1);
      },
    });

    expect(pane.models().slice(0, 2)).toEqual(["strong-model", "weak-model"]);
    // One assignment, two requests: the host replayed the refused turn itself, which it may do
    // because nothing of it had been committed, rather than the plugin moving a later turn.
    expect(userText(pane.turns()[1] as Request)).toContain("Start FALL-1.");

    // The host walked its own chain, and the host's replay of the turn succeeded, so the settled
    // turn reads clean and only the fallback event itself says a filter refused the session. The
    // arming it leaves is a refusal's all the same: the same cooldown and the same doubling the
    // plugin's own walk takes.
    const records = await pane.records();
    const armed = records[0];
    expect(armed?.state).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The wait the refusal bought is the base cooldown, counted from the moment the pane armed it.
    expect(waitOf(armed)).toBe(COOLDOWN_MS);
    // The defect this case is the proof of: an arming the plugin read as anything but a refusal
    // waits on a suppression the host never applies for a classifier refusal — it pins the
    // fallback instead — so the session went straight back to the model that had just refused
    // it. Nothing goes back before the wait is over, by the pane's own record of its return.
    expect(returnAfter(records, armed).at).toBeGreaterThanOrEqual(
      armed?.state.arming?.dueAt ?? Number.POSITIVE_INFINITY
    );
    // Several requests fall inside the wait, and the session does come back once it is over:
    // every turn before the return went to the weaker model, and the one whose request put the
    // session back is still the weaker model's (the host chooses a request's model before it runs
    // the `context` hook), so two at least ran while it waited.
    expect(pane.beforeReturn(0).length).toBeGreaterThan(2);
  }
);

test.skipIf(skip)(
  "a refusal of the fallback model after the host's own walk steps down again and comes back to the model the session started on",
  async () => {
    const pane = await fallbackPane({
      chains: TWO_STEPS,
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (model, nth) => {
        // Refused before any output: the host walks its own chain to the middle model and
        // replays the turn there.
        if (nth === 1) return refuses("refusal_1");
        // The replay is refused after text, which the host leaves where it is: the plugin's own
        // walk takes it from the middle model to the weaker one.
        if (nth === 2) return refuses("refusal_2", { text: "Looking at it." });
        // The second assignment's turn asks the gateway every second until the session leaves the
        // weaker model: the return, whose moment and model the case judges below.
        return asksUntilBack(model, nth, 1);
      },
    });

    expect(pane.models().slice(0, 3)).toEqual(["strong-model", "middle-model", "weak-model"]);
    const records = await pane.records();
    const armed = records.findLast((record) => record.state.arming !== undefined);
    // The defect this case is the proof of: the plugin's own walk armed the return on the model
    // it was leaving, so the session came back to the middle of the chain and never to the model
    // it started on. The second step continues the host's own walk, so the return still points
    // at the stronger model, and the wait it bought is the doubled one of that model.
    expect(armed?.state.arming).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(armed?.state.levels).toEqual({ [STRONG]: 3 });
    expect(waitOf(armed)).toBe(2 * COOLDOWN_MS);
    // Nothing went back before that wait, by the pane's own record of its return, two requests at
    // least ran inside it (the one whose request put the session back being the weaker model's
    // too), and what the session went back to is the stronger model: no request of this pane ever
    // went to the middle model again.
    expect(returnAfter(records, armed).at).toBeGreaterThanOrEqual(
      armed?.state.arming?.dueAt ?? Number.POSITIVE_INFINITY
    );
    expect(pane.beforeReturn(1).length).toBeGreaterThan(2);
    expect(pane.models().slice(2)).not.toContain("middle-model");
  }
);

test.skipIf(skip)(
  "a refusal after streamed text, which the host leaves where it is, sends the next turn to the chain's next model",
  async () => {
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Carry on."],
      answer: (_model, nth) =>
        nth === 1
          ? refuses("refusal_1", { text: "Looking at it." })
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`),
    });

    expect(pane.models()).toEqual(["strong-model", "weak-model"]);
    // The refused turn was replayed by nobody — committed text makes it unsafe to replay, so the
    // host ends the run there — and the next request is the next assignment's, on the model the
    // plugin's own walk moved the session to.
    expect(userText(pane.turns()[1] as Request)).toContain("Carry on.");
  }
);

test.skipIf(skip)(
  "the session is put back on the model it fell back from inside a turn already running, once the cooldown is over",
  async () => {
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (_model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // One long turn on the weaker model: the sleep outlasts the cooldown, so the requests
        // after it are the ones the `context` hook has something to do about.
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (nth === 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The host chooses a request's model before it runs the `context` hook, so the hook's switch
    // is the model of the *next* request of that turn: the third request, sent after the sleep,
    // is still the weaker model, and the fourth is the model the session fell back from — inside
    // the same turn, which no new prompt and no host setting would have reached.
    expect(pane.models()).toEqual(["strong-model", "weak-model", "weak-model", "strong-model"]);
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The return clears the arming and leaves the doubling where it stands.
    expect(states.at(-1)).toEqual({ levels: { [STRONG]: 1 } });
  }
);

test.skipIf(skip)(
  "a second refusal of the same model doubles its wait, so the return does not come at the first one",
  async () => {
    /** Whether the stronger model has been refused the second time: that refusal's wait is the
     * doubled one. */
    let refusedAgain = false;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        // The first assignment is refused on the stronger model, which arms the base wait.
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // The second runs long enough for the return, and is refused again the moment it lands
        // on the stronger model: that refusal is the one whose wait has doubled.
        if (model === "strong-model" && !refusedAgain) {
          refusedAgain = true;
          return refuses("refusal_2", { text: "Looking at it again." });
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (!refusedAgain) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The third assignment's turn keeps asking the gateway every two seconds until the
        // session is back, so the model of each request says where the session stood at that
        // moment.
        return asksUntilBack(model, nth, 2);
      },
    });

    // The second refusal is the second turn the stronger model was sent.
    const strongTurns = pane.served.flatMap((turn, index) =>
      turn.model === "strong-model" ? [index] : []
    );
    const secondRefusal = strongTurns[1];
    if (secondRefusal === undefined) throw new Error("the stronger model was sent one turn only");
    const records = await pane.records();
    // The first refusal of the stronger model took the base wait and raised its level; the second
    // took twice the base and raised it again.
    expect(records[0]?.state).toMatchObject({ levels: { [STRONG]: 2 } });
    const doubled = records.findLast((record) => record.state.arming !== undefined);
    expect(doubled?.state.levels).toEqual({ [STRONG]: 3 });
    expect(waitOf(doubled)).toBe(2 * COOLDOWN_MS);
    // The return came after the doubled wait, by the pane's own record of it, so no request went
    // to the stronger model at the first one; and it came, after turns on the weaker model.
    expect(returnAfter(records, doubled).at).toBeGreaterThanOrEqual(
      doubled?.state.arming?.dueAt ?? Number.POSITIVE_INFINITY
    );
    expect(pane.beforeReturn(secondRefusal).length).toBeGreaterThan(0);
  }
);

test.skipIf(skip)(
  "a clean turn on the model the session came back to puts its wait back to the base",
  async () => {
    /** How many turns the stronger model has been sent: the first is refused, the second is the
     * clean turn that puts the wait back, the third is refused again, and the fourth is the
     * return, which ends the last assignment's turn. */
    let onStrong = 0;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going.", "Carry on."],
      answer: (model, nth) => {
        if (model === "strong-model") {
          onStrong += 1;
          if (onStrong === 1) return refuses("refusal_1", { text: "Looking at it." });
          // The clean turn: it ends the second assignment's run on the stronger model.
          if (onStrong === 2) return replies([{ type: "text", text: "Back on it." }], `msg_${nth}`);
          if (onStrong === 3) return refuses("refusal_2", { text: "Looking at it again." });
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (onStrong < 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The last assignment's turn asks the gateway every second until the session is back, so
        // the model of each request says where the session stood at that moment.
        return asksUntilBack(model, nth, 1);
      },
    });

    // The second refusal is the third turn the stronger model was sent.
    const strongTurns = pane.served.flatMap((turn, index) =>
      turn.model === "strong-model" ? [index] : []
    );
    const secondRefusal = strongTurns[2];
    if (secondRefusal === undefined) throw new Error("the stronger model was not refused again");
    const records = await pane.records();
    const armings = records.filter((record) => record.state.arming !== undefined);
    // Both refusals of the stronger model armed from level 1 and raised it to 2: the clean turn
    // between them put back what the first one raised, so the second bought the base wait again
    // rather than twice it, by the pane's own record of the moment it armed and the due time.
    expect(armings.map((record) => record.state.levels[STRONG])).toEqual([2, 2]);
    const secondArming = armings.at(-1);
    expect(records[records.indexOf(secondArming as CooldownRecord) - 1]?.state).toEqual({
      levels: { [STRONG]: 1 },
    });
    expect(waitOf(secondArming)).toBe(COOLDOWN_MS);
    // And the session came back at that wait, no earlier than it says, after turns on the weaker
    // model.
    expect(returnAfter(records, secondArming).at).toBeGreaterThanOrEqual(
      secondArming?.state.arming?.dueAt ?? Number.POSITIVE_INFINITY
    );
    expect(pane.beforeReturn(secondRefusal).length).toBeGreaterThan(0);
  }
);

test.skipIf(skip)(
  "a pane whose process is gone resumes its own transcript's arming, and comes back no earlier than it says",
  async () => {
    const pane = await fallbackPane({
      cooldownMs: RESUME_COOLDOWN_MS,
      // The second assignment is a tool-call turn: its own entries carry the arming written at
      // the end of the refused turn to disk, which is where the resumed life reads it.
      prompt: ["Start FALL-1.", "Note that."],
      answer: (model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        if (nth === 2) return replies([call("bash", { command: "echo noted" })], `msg_${nth}`);
        if (nth === 3) return replies([{ type: "text", text: "Noted." }], `msg_${nth}`);
        return asksUntilBack(model, nth, 2);
      },
    });

    // The first life ends on the arming: its last record holds it, so it never put the session
    // back, and the wait is the transcript's to carry over. The wait is the one the case set,
    // counted from the moment the pane armed it.
    const armed = (await pane.records()).at(-1);
    expect(armed?.state.arming).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(waitOf(armed)).toBe(RESUME_COOLDOWN_MS);
    const dueAt = armed?.state.arming?.dueAt ?? 0;
    const firstLife = pane.served.length;

    // The daemon relaunches the worker: a fresh process whose only memory of the fallback is the
    // transcript it resumes. Its turn asks the gateway every two seconds until the session is
    // back, however long the relaunch took.
    await pane.resume([], "Carry on after the relaunch.");

    expect(pane.served.length - firstLife).toBeGreaterThan(1);
    const records = await pane.records();
    // The arming was restored, not started again: the resumed life armed nothing of its own, so
    // the one arming on record is the first life's, and the next record is its return. A life
    // that had started the wait again would have recorded an arming of its own, whatever its boot
    // cost, and one that had dropped the arming would record no return.
    const armings = records.filter((record) => record.state.arming !== undefined);
    expect(armings).toEqual([armed as CooldownRecord]);
    // It came back no earlier than the arming says, by the pane's own record of the return.
    expect(returnAfter(records, armings[0]).at).toBeGreaterThanOrEqual(dueAt);
    // And the return did come on this life's own turns: a life that had restored nothing would
    // have stayed on the weaker model for good.
    expect(() => pane.beforeReturn(firstLife - 1)).not.toThrow();
  }
);
