import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { writeFile } from "node:fs/promises";
import * as path from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
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
  type Rpc,
  refusalStream,
  runLegionPane,
  type StandinProvider,
  sendRelayFrame,
} from "@legion/pi-shared/test/omp-harness";
import { connect, headers, type NatsConnection, StringCodec } from "nats";

// The model fallback on the real Oh My Pi (src/model-fallback.ts, on the cooldown machine and the
// chain resolver of @legion/pi-shared): only the real binary shows which model each request went
// to across a provider's refusal — the refusals the host walks its own chain for, the ones it
// leaves where they are, the return when the wait ends, inside a turn already running and before
// a run a card or a job's result starts, and that the transcript keeps the arming a relaunched
// worker restores.
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
 * (`fallbackCooldownLimits`): seconds here, an hour in a deployment, so every case below runs
 * inside the pi-legion job's ten minutes.
 */
const COOLDOWN_MS = 4_000;

/**
 * A wait no case reaches: longer than the longest case's own timeout (300 s), and under the cap
 * the pane sets (`LEGION_REFUSAL_COOLDOWN_MAX_MS`, 600 s), so its refusal raises the doubling level
 * as a deployment's does. A life armed with it never comes back on its own, so no step of the
 * case, however slow, can let the return come before the turn the case reads.
 */
const UNREACHED_COOLDOWN_MS = 400_000;

/** The wait the killed-and-resumed case writes into the transcript's arming once the first life
 * is gone, counted from that moment: long enough that the restored arming is still running when
 * the replacement pane is relaunched, short enough for the return to come while the case watches.
 * The relaunch's own boot is not covered by it and is not meant to be - the case measures the
 * return from the resumed life's first served turn, and reads the restored arming from where that
 * turn went (below). */
const RELAUNCH_WAIT_MS = 12_000;

/** How long a case waits for its return past the due time before it gives up on it: an idle
 * pane's wait (`IdleLife.modelChanges`), or a turn that asks the gateway again until the return.
 * It bounds only a session that does not come back, which the case's own assertion then fails
 * on, and a session that does come back ends the wait at the frame or the request that says so. */
const RETURN_GIVE_UP_MS = 60_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A shell sleep the model asks for, which keeps one turn running while a cooldown runs out: the
 * requests of that same turn are the ones that show where the return came. */
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

/** One turn the gateway served: the model it was sent to, when it arrived, and its request's
 * messages as JSON. */
interface Served {
  readonly model: string;
  readonly at: number;
  readonly messages: string;
}

/** What a settled fallback pane left behind, beside every pane's own record. */
interface FallbackPane extends LegionPane {
  /** Every turn the gateway served: its model and its time, in order. */
  readonly served: readonly Served[];
  /** The model of every turn the gateway served, in order. */
  readonly models: () => string[];
  /** When the first turn after `since` went to the stronger model, or undefined when none did:
   * the moment the session came back. */
  readonly returnedAt: (since: number) => number | undefined;
  /** The cooldown states the pane recorded in its transcript, in order. */
  readonly cooldowns: () => Promise<FallbackCooldownState[]>;
  /**
   * Each change of the session's model and each run's end that omp printed, in order: a change as
   * `model`, an end as `end <n>`, or `parked <n>` when the host marked the run as waiting on a
   * background job's result (`awaitingAsyncWork`). `n` is how many turns the gateway had served
   * when the end arrived, which is exact: the gateway answers a run's last turn before omp can end
   * the run. A change carries no count, since omp's frames and the gateway's requests reach the
   * case on two streams, and a change printed before a request can be read after it.
   */
  readonly frames: readonly string[];
  /** When each `model_changed` frame reached the case (epoch ms), in order: never before omp
   * printed it, so a change made at or after a due time reads at or after it here. */
  readonly changedAt: readonly number[];
}

/**
 * What a case can do with its pane's first life once every prompt's run has settled and before
 * that life ends (`fallbackPane`'s `whileUp`): the pane is up and idle, so a run can be started
 * by something other than a prompt — an Envoy card, a background job's result.
 */
interface IdleLife {
  /** Every turn the gateway served so far, in order. */
  readonly served: readonly Served[];
  /** The session's working directory, where its shell commands run. */
  readonly workspace: string;
  /** Writes `message` as one RPC `prompt` and resolves once the run it started has ended, with an
   * `agent_end` the host marks terminal or not: a run that leaves a background job going ends
   * with one marked not terminal, since the job's result continues it. */
  readonly prompt: (message: string) => Promise<void>;
  /** Hands the session one Envoy card carrying `summary` on its own inbox, as the listener's
   * relay does, and resolves once the run the card started has ended. */
  readonly card: (summary: string) => Promise<void>;
  /** How many runs of the session have ended so far. */
  readonly runs: () => number;
  /** Resolves once more than `runs` runs of the session have ended. */
  readonly runAfter: (runs: number) => Promise<void>;
  /** Resolves true once the host has reported `count` changes of the session's model since
   * `since` (epoch ms), or false at `giveUpAt` when it has not. */
  readonly modelChanges: (since: number, count: number, giveUpAt: number) => Promise<boolean>;
}

/**
 * Runs one root architect's pane on the real Oh My Pi (`runLegionPane`) under the chain overlay,
 * its gateway answering each turn from `answer`, until every assignment's run has settled. A root
 * architect holds a Legion claim, which is all the model fallback gates on, and is the one role
 * the phase reminder leaves alone, so nothing but the case's own assignments starts a turn.
 * `whileUp` runs in the first life once those runs have settled, before it ends.
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
  readonly whileUp?: (life: IdleLife) => Promise<void>;
}): Promise<FallbackPane> {
  const { binary, nats } = setup();
  const served: Served[] = [];
  /** The session the listener registered, whose deliver subject a card is relayed on. */
  let sessionID = "";
  let workspace = "";
  /** When each `model_changed` frame arrived, and how many runs have ended. */
  const modelChanged: number[] = [];
  let runsEnded = 0;
  const frames: string[] = [];
  /** The checks of every wait still open, run after each frame omp prints. */
  const waits = new Set<() => void>();
  const { whileUp } = options;

  /** The case's view of the life `rpc` belongs to, over the real NATS server `nc`. */
  const idleLife = (rpc: Rpc, nc: NatsConnection): IdleLife => {
    /** Resolves true once `holds` does, read after each frame omp prints, or false at
     * `giveUpAt`; rejects, with omp's stderr, once omp has closed its RPC stream. */
    const until = (holds: () => boolean, giveUpAt = Number.POSITIVE_INFINITY): Promise<boolean> => {
      if (holds()) return Promise.resolve(true);
      const { promise, resolve } = Promise.withResolvers<boolean>();
      // The one clock of the wait, on purpose: it bounds only a pane that never does what is
      // waited for, and the cooldown it is set past runs on the omp process's own clock, which no
      // test can move. A pane that does it ends the wait at the frame that says so.
      const giveUp = Number.isFinite(giveUpAt)
        ? setTimeout(() => settle(false), Math.max(0, giveUpAt - Date.now()))
        : undefined;
      const check = (): void => {
        if (holds()) settle(true);
      };
      const settle = (value: boolean): void => {
        waits.delete(check);
        clearTimeout(giveUp);
        resolve(value);
      };
      waits.add(check);
      return Promise.race([promise, rpc.closed]);
    };
    const codec = StringCodec();
    let seq = 0;
    return {
      served,
      workspace,
      prompt: async (message) => {
        const ended = runsEnded;
        rpc.send({ type: "prompt", message });
        await until(() => runsEnded > ended);
      },
      card: async (summary) => {
        if (sessionID === "") throw new Error("the pane registered no Envoy session to send to");
        const ended = runsEnded;
        const key = `card-${randomUUID()}`;
        seq += 1;
        await sendRelayFrame({
          connection: nc,
          newHeaders: headers,
          sessionID,
          subject: `notifications.agent.${sessionID}`,
          seq,
          payload: codec.encode(
            JSON.stringify({
              event_id: `evt-${key}`,
              source: "envoy",
              source_event_id: `source-${key}`,
              // The role lane of the claim `runLegionPane` registers.
              topic: `${ROLE_TOPIC_PREFIX}legion-fallback-fall-1-architect`,
              dedupe_key: `envoy.role.forward.publish.${createHash("sha256").update(key).digest("hex").slice(0, 32)}`,
              issued_at: Date.now(),
              payload_summary: summary,
              trace_id: `trace-${key}`,
            })
          ),
        });
        await until(() => runsEnded > ended);
      },
      runs: () => runsEnded,
      runAfter: async (runs) => {
        await until(() => runsEnded > runs);
      },
      modelChanges: (since, count, giveUpAt) =>
        until(() => modelChanged.filter((at) => at >= since).length >= count, giveUpAt),
    };
  };

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
      prepare: async (paths) => {
        workspace = paths.workspace;
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "model_changed") {
          modelChanged.push(Date.now());
          frames.push("model");
        }
        if ("type" in frame && frame.type === "agent_end") {
          runsEnded += 1;
          const parked = "awaitingAsyncWork" in frame && frame.awaitingAsyncWork === true;
          frames.push(`${parked ? "parked" : "end"} ${served.length}`);
        }
        for (const check of [...waits]) check();
      },
      ...(whileUp === undefined
        ? {}
        : {
            whileUp: async (rpc: Rpc) => {
              const nc = await connect({ servers: nats });
              cleanup.push(() => nc.close());
              await whileUp(idleLife(rpc, nc));
            },
          }),
      answer: (url, body) => {
        if (url.pathname === "/v1/interests/subscribe" && typeof body.session_id === "string") {
          sessionID = body.session_id;
        }
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        const model = String(body.model ?? "");
        served.push({ model, at: Date.now(), messages: JSON.stringify(body.messages ?? null) });
        return options.answer(model, served.length, { path: url.pathname, body });
      },
    },
    cleanup
  );
  return {
    ...pane,
    served,
    models: () => served.map((turn) => turn.model),
    returnedAt: (since) =>
      served.find((turn) => turn.at >= since && turn.model === "strong-model")?.at,
    cooldowns: async () =>
      (await pane.transcriptEntries(LEGION_FALLBACK_ENTRY)) as FallbackCooldownState[],
    frames,
    changedAt: modelChanged,
  };
}

/**
 * Writes `dueAt` into the arming the pane's transcript recorded last, once its first life is gone:
 * the wait runs on the omp process's own clock, which no test can move, so a case that relaunches
 * a pane sets the boundary of the wait the resumed life restores itself, after every step it takes
 * before the relaunch.
 */
async function moveDueTime(pane: FallbackPane, dueAt: number): Promise<void> {
  await pane.editTranscriptEntry(LEGION_FALLBACK_ENTRY, (data) => {
    const state = data as FallbackCooldownState;
    if (state.arming === undefined) throw new Error("the last fallback state holds no arming");
    return { ...state, arming: { ...state.arming, dueAt } };
  });
}

test.skipIf(skip)(
  "a refusal before any output is retried on the chain's next model, which is the host's own walk",
  async () => {
    /** When the run stops asking again for a session that has not come back by then: it bounds
     * only a session that never comes back, which the last assertion then fails on. */
    let giveUpAt = 0;
    const pane = await fallbackPane({
      // One assignment, one run: the host's replay of the refused turn and every turn after it
      // ask the gateway again while the session is on the weaker model, so no step of the case
      // falls between the refusal and the requests that show where the session stood. The
      // replay's command returns at once, so a second request follows it inside the wait, and
      // each later turn sleeps a second first. The run ends at its first request to any other
      // model: it ends on the return, whenever that comes.
      prompt: ["Start FALL-1."],
      answer: (model, nth) => {
        if (nth === 1) {
          giveUpAt = Date.now() + COOLDOWN_MS + RETURN_GIVE_UP_MS;
          return refuses("refusal_1");
        }
        if (model !== "weak-model" || Date.now() >= giveUpAt) {
          return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
        }
        return nth === 2
          ? replies([call("bash", { command: "echo next" })], `msg_${nth}`)
          : replies([sleeps(1)], `ping_${nth}`);
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
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    const dueAt = states[0]?.arming?.dueAt ?? 0;
    const waiting = pane.served.slice(1).filter((turn) => turn.at < dueAt);
    // The defect this case is the proof of: an arming the plugin read as anything but a refusal
    // waits on a suppression the host never applies for a classifier refusal — it pins the
    // fallback instead — so the session went straight back to the model that had just refused
    // it. Nothing goes back before the wait is over, and several requests fall inside it.
    expect(waiting.length).toBeGreaterThan(1);
    expect(waiting.map((turn) => turn.model)).not.toContain("strong-model");
    // And the session does come back, once the wait is over.
    expect(pane.returnedAt(dueAt)).toBeDefined();
  },
  180_000
);

test.skipIf(skip)(
  "a refusal of the fallback model after the host's own walk steps down again and comes back to the model the session started on",
  async () => {
    /** When the turn that pings the gateway every second must stop and let the run settle: past
     * the doubled wait the second refusal bought. */
    let until = 0;
    const pane = await fallbackPane({
      chains: TWO_STEPS,
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (_model, nth) => {
        // Refused before any output: the host walks its own chain to the middle model and
        // replays the turn there.
        if (nth === 1) return refuses("refusal_1");
        // The replay is refused after text, which the host leaves where it is: the plugin's own
        // walk takes it from the middle model to the weaker one.
        if (nth === 2) {
          until = Date.now() + 4 * COOLDOWN_MS;
          return refuses("refusal_2", { text: "Looking at it." });
        }
        return Date.now() < until
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    expect(pane.models().slice(0, 3)).toEqual(["strong-model", "middle-model", "weak-model"]);
    const states = await pane.cooldowns();
    const armed = states.findLast((state) => state.arming !== undefined)?.arming;
    // The defect this case is the proof of: the plugin's own walk armed the return on the model
    // it was leaving, so the session came back to the middle of the chain and never to the model
    // it started on. The second step continues the host's own walk, so the return still points
    // at the stronger model, and the wait it bought is the doubled one of that model.
    expect(armed).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(states.findLast((state) => state.arming !== undefined)?.levels).toEqual({
      [STRONG]: 3,
    });
    const refusedAt = pane.served[1]?.at ?? 0;
    const dueAt = armed?.dueAt ?? 0;
    expect(dueAt - refusedAt).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
    // Nothing went back before that wait, and what the session went back to is the stronger
    // model: no request of this pane ever went to the middle model again.
    const waiting = pane.served.slice(2).filter((turn) => turn.at < dueAt);
    expect(waiting.length).toBeGreaterThan(1);
    expect(waiting.map((turn) => turn.model)).not.toContain("strong-model");
    expect(pane.returnedAt(dueAt)).toBeDefined();
    expect(pane.models().slice(2)).not.toContain("middle-model");
  },
  240_000
);

test.skipIf(skip)(
  "a refusal after streamed text, which the host leaves where it is, sends the next turn to the chain's next model",
  async () => {
    const pane = await fallbackPane({
      // The case reads only the turn after the refusal, never the return: a wait no case
      // reaches, so the next assignment's turn reads the walk's model however late it is sent.
      cooldownMs: UNREACHED_COOLDOWN_MS,
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
  },
  180_000
);

test.skipIf(skip)(
  "the session is put back on the model it fell back from inside a turn already running, once the cooldown is over",
  async () => {
    const pane = await fallbackPane({
      // One assignment, one run, so no step of the case falls inside the wait.
      prompt: ["Start FALL-1."],
      answer: (_model, nth) => {
        // Refused before any output: the host walks its own chain and replays the turn on the
        // weaker model inside the same run.
        if (nth === 1) return refuses("refusal_1");
        // One long turn on the weaker model: the sleep outlasts the cooldown, so the wait ends
        // while the turn's tool runs. It starts after the refusal, so it outlasts what is left of
        // the wait however late it starts.
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (nth === 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The wait ends while the turn runs, so the runner's timer switches no model: a switch there
    // could land in the middle of a stream. The turn's own `context` hook makes the return, and the
    // host chooses a request's model before it runs that hook, so the hook's switch is the model of
    // the *next* request of that turn: the third request, sent after the sleep, is still the
    // weaker model, and the fourth is the model the session fell back from — inside the same
    // turn, which no new prompt and no host setting would have reached.
    expect(pane.models()).toEqual(["strong-model", "weak-model", "weak-model", "strong-model"]);
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The return clears the arming and leaves the doubling where it stands.
    expect(states.at(-1)).toEqual({ levels: { [STRONG]: 1 } });
  },
  180_000
);

test.skipIf(skip)(
  "a second refusal of the same model doubles its wait, so the return does not come at the first one",
  async () => {
    /** When the turn that pings every two seconds must stop and let the run settle. */
    let until = 0;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        // The first assignment is refused on the stronger model, which arms the base wait.
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // The second runs long enough for the return, and is refused again the moment it lands
        // on the stronger model: that refusal is the one whose wait has doubled.
        if (model === "strong-model" && until === 0) {
          until = Date.now() + 4 * COOLDOWN_MS;
          return refuses("refusal_2", { text: "Looking at it again." });
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (until === 0) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The third assignment's turn keeps asking the gateway every two seconds, so the model
        // of each request says where the session stood at that moment.
        return Date.now() < until
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const secondRefusal = pane.served.filter((turn) => turn.model === "strong-model")[1]?.at;
    expect(secondRefusal).toBeDefined();
    const states = await pane.cooldowns();
    // The first refusal of the stronger model took the base wait and raised its level; the second
    // took twice the base and raised it again.
    expect(states[0]).toMatchObject({ levels: { [STRONG]: 2 } });
    const doubled = states.findLast((state) => state.arming !== undefined);
    expect(doubled?.levels).toEqual({ [STRONG]: 3 });
    const dueAt = doubled?.arming?.dueAt ?? 0;
    expect(dueAt - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
    expect(dueAt - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS + 2_000);
    // The return came after the doubled wait, and no request went to the stronger model at the
    // first one.
    const returned = pane.returnedAt((secondRefusal ?? 0) + 1);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
  },
  240_000
);

test.skipIf(skip)(
  "a clean turn on the model the session came back to puts its wait back to the base",
  async () => {
    /** How many turns the stronger model has been sent: the first is refused, the second is the
     * clean turn that puts the wait back, the third is refused again. */
    let onStrong = 0;
    /** When the last run stops asking again for a session that has not come back by then: it
     * bounds only a session that never comes back, which the last assertion then fails on. */
    let giveUpAt = 0;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        if (model === "strong-model") {
          onStrong += 1;
          if (onStrong === 1) return refuses("refusal_1", { text: "Looking at it." });
          // The clean turn: it ends the second assignment's run on the stronger model.
          if (onStrong === 2) return replies([{ type: "text", text: "Back on it." }], `msg_${nth}`);
          // The second refusal comes before any output: the host walks its own chain and replays
          // the turn on the weaker model inside the same run, so no step of the case falls inside
          // the wait it buys. The run ends at its next request to the stronger model, the return.
          if (onStrong === 3) {
            giveUpAt = Date.now() + COOLDOWN_MS + RETURN_GIVE_UP_MS;
            return refuses("refusal_2");
          }
          return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (onStrong < 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The last run asks the gateway every second while the session is on the weaker model,
        // so the model of each request says where the session stood at that moment.
        return Date.now() < giveUpAt
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const secondRefusal = pane.served.filter((turn) => turn.model === "strong-model")[2]?.at;
    expect(secondRefusal).toBeDefined();
    const states = await pane.cooldowns();
    const armings = states.filter((state) => state.arming !== undefined);
    // Both refusals of the stronger model armed from level 1 and raised it to 2: the clean turn
    // between them put back what the first one raised, so the second bought the base wait again
    // rather than twice it.
    expect(armings.map((state) => state.levels[STRONG])).toEqual([2, 2]);
    const secondArming = states.findLastIndex((state) => state.arming !== undefined);
    expect(states[secondArming - 1]).toEqual({ levels: { [STRONG]: 1 } });
    const dueAt = states[secondArming]?.arming?.dueAt ?? 0;
    expect(dueAt - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(COOLDOWN_MS);
    expect(dueAt - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS);
    // And the session came back at that wait, before the doubled one it would have waited had
    // the clean turn counted for nothing.
    const returned = pane.returnedAt((secondRefusal ?? 0) + 1);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS);
  },
  240_000
);

test.skipIf(skip)(
  "a pane whose process is gone resumes its own transcript's arming, and comes back no earlier than it says",
  async () => {
    // The resumed life's long turn keeps asking the gateway, every two seconds, until the
    // stronger model has answered once past the restored due time, and stops at the request after
    // that. So whether the return came is read from the turns the life made, and never raced
    // against a fixed stop: a fixed stop two seconds past the due time could fall before the
    // life's first request past it on a loaded box, ending the run on the weaker model with
    // nothing wrong behind it. `giveUpAt` bounds a life that never comes back, which the
    // assertions below then fail on. Before the relaunch both are unset, so the first life's
    // turns end at once.
    let dueBy = Number.POSITIVE_INFINITY;
    let giveUpAt = 0;
    let cameBack = false;
    const pane = await fallbackPane({
      // A wait the first life never reaches, so nothing the case does before the relaunch can let
      // it come back on its own; the case writes the wait the resumed life restores below.
      cooldownMs: UNREACHED_COOLDOWN_MS,
      // The second assignment is a tool-call turn: its own entries carry the arming written at
      // the end of the refused turn to disk, which is where the resumed life reads it.
      prompt: ["Start FALL-1.", "Note that."],
      answer: (model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        if (nth === 2) return replies([call("bash", { command: "echo noted" })], `msg_${nth}`);
        if (nth === 3) return replies([{ type: "text", text: "Noted." }], `msg_${nth}`);
        const done = cameBack || Date.now() >= giveUpAt;
        if (model === "strong-model" && Date.now() >= dueBy) cameBack = true;
        return done
          ? replies([{ type: "text", text: "Done." }], `msg_${nth}`)
          : replies([sleeps(2)], `ping_${nth}`);
      },
    });

    const armed = (await pane.cooldowns()).at(-1)?.arming;
    expect(armed).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    const firstLife = pane.served.length;
    // The wait the resumed life restores starts now, once the first life is gone and every step
    // the case takes before the relaunch is behind it.
    const dueAt = Date.now() + RELAUNCH_WAIT_MS;
    await moveDueTime(pane, dueAt);

    // The daemon relaunches the worker: a fresh process whose only memory of the fallback is the
    // transcript it resumes.
    dueBy = dueAt;
    giveUpAt = dueAt + 4 * RELAUNCH_WAIT_MS;
    await pane.resume([], "Carry on after the relaunch.");

    const resumed = pane.served.slice(firstLife);
    expect(resumed.length).toBeGreaterThan(1);
    // That the arming was restored, and not dropped, is read from where this life's own turns
    // went: every turn served while the restored wait still ran went to the weaker model. A boot
    // that outran the wait leaves no such turn, and then nothing here says where this life's first
    // request went: the run's model is read before its first request, and whether the return was
    // made ahead of it depends on which side of the due time the run started, not the request. The
    // case below relaunches past the due time on purpose and asserts that first request.
    const beforeDue = resumed.filter((turn) => turn.at < dueAt);
    expect(beforeDue.map((turn) => turn.model)).not.toContain("strong-model");
    // And the return did come, which is what says the arming survived at all: a life that had
    // restored nothing would have stayed on the weaker model for good.
    //
    // Measured from this life's own first served turn rather than from the restored due time: a
    // relaunched pane pays its whole boot before it serves anything - the process start, a NATS
    // connect, its subscribes, the claim's registration and the consumer answer that registration
    // waits on - and under parallel load that boot alone can outlast the cooldown, which would
    // red this row with nothing wrong behind it. What the bound is for is the return itself: once
    // this life is serving turns, the restored arming must give the stronger model back inside
    // one cooldown rather than holding the weaker one for good.
    const resumedFrom = Math.max(dueAt, resumed[0]?.at ?? 0);
    const returned = pane.returnedAt(dueAt);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - resumedFrom).toBeLessThan(RELAUNCH_WAIT_MS);
  },
  300_000
);

test.skipIf(skip)(
  "a pane relaunched once its restored wait is over sends its first turn to the model it fell back from",
  async () => {
    const pane = await fallbackPane({
      // As in the case above: a wait the first life never reaches, and the second assignment's
      // entries carry the arming to disk.
      cooldownMs: UNREACHED_COOLDOWN_MS,
      prompt: ["Start FALL-1.", "Note that."],
      answer: (model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        if (nth === 2) return replies([call("bash", { command: "echo noted" })], `msg_${nth}`);
        if (nth === 3) return replies([{ type: "text", text: "Noted." }], `msg_${nth}`);
        // The resumed life's turn asks again while the session is on the weaker model and ends at
        // its first request to any other: it ends on the return, whenever that comes.
        return model === "weak-model"
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const armed = (await pane.cooldowns()).at(-1)?.arming;
    expect(armed).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    const firstLife = pane.served.length;

    // The daemon relaunches the worker once the wait is over, as it does for a pane that was gone
    // longer than the cooldown it fell back for, and as a boot slower than the rest of the wait
    // does in the case above. The wait the resumed life restores ended the moment the case wrote
    // it, so it is over by the relaunch.
    await moveDueTime(pane, Date.now());
    await pane.resume([], "Carry on after the relaunch.");

    // The host reads a request's model before it runs the `context` hook, so that hook cannot
    // move the first request of a run: the runner's timer, set at the boot for a due time already
    // past, and `before_agent_start` each put the due arming back ahead of it. A life that had
    // dropped the arming would never leave the weaker model, and one that had started the wait
    // again would spend a whole cooldown on it.
    expect(pane.served[firstLife]?.model).toBe("strong-model");
    // The return is the restored arming's own: the state it writes clears the arming and keeps the
    // level the refusal raised, before the clean turn after it puts that level back to the base.
    const states = await pane.cooldowns();
    const armedLast = states.findLastIndex((state) => state.arming !== undefined);
    expect(states[armedLast + 1]).toEqual({ levels: { [STRONG]: 2 } });
  },
  180_000
);

test.skipIf(skip)(
  "a card that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    const summary = `CARD-${randomUUID().slice(0, 8)}`;
    /** What the idle life saw: whether the return came, how many turns had been served when it
     * did, and the first turn the card's run served. */
    let cameBack = false;
    let servedAtReturn = 0;
    let first = 0;
    const pane = await fallbackPane({
      cooldownMs: COOLDOWN_MS,
      prompt: ["Start FALL-1."],
      answer: (_model, nth) =>
        // Refused after text, which the host leaves where it is: the plugin's own walk moves the
        // session to the weaker model and arms the return. Every later turn ends at once.
        nth === 1
          ? refuses("refusal_1", { text: "Looking at it." })
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`),
      whileUp: async (life) => {
        const refusedAt = life.served[0]?.at ?? 0;
        // The pane is idle now and nothing starts a run of its own: the session comes back when
        // the wait is over or not at all. The walk is the first model change the host reports
        // after the refusal, and the return is the second.
        cameBack = await life.modelChanges(
          refusedAt,
          2,
          refusedAt + COOLDOWN_MS + RETURN_GIVE_UP_MS
        );
        servedAtReturn = life.served.length;
        // A card is an agent-attributed wake: the host starts its run with no
        // `before_agent_start`, and reads the run's model before the `context` hook of its first
        // request runs. So that request goes to the model the session was left on while idle.
        first = life.served.length;
        await life.card(summary);
      },
    });

    expect(cameBack).toBe(true);
    // No request ran a hook while the pane waited: the refused one was the last before the return,
    // so the return is the timer's.
    expect(servedAtReturn).toBe(1);
    const woken = pane.served.slice(first);
    expect(woken.length).toBeGreaterThan(0);
    // The run is the card's: its first request carries the card.
    expect(woken[0]?.messages).toContain(summary);
    expect(woken[0]?.model).toBe("strong-model");
  },
  180_000
);

test.skipIf(skip)(
  "a background job's result that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    /** The file whose existence ends the background job: the case makes it once the wait is
     * over, so the job's result comes when the case says rather than at a sleep's end. */
    const release = ".release-job";
    let cameBack = false;
    let first = 0;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1."],
      answer: (_model, nth) => {
        // The first assignment's one turn, which only brings the pane up.
        if (nth === 1) return replies([{ type: "text", text: "Ready." }], `msg_${nth}`);
        // The second assignment starts a background job, which the host runs on after the run
        // has ended, and wakes the session with its result.
        if (nth === 2) {
          return replies(
            [
              call("bash", {
                command: `until [ -e ${release} ]; do sleep 0.2; done; echo released`,
                async: true,
              }),
            ],
            `msg_${nth}`
          );
        }
        // The turn after the job has started is refused before any output: the host walks its
        // own chain and replays the turn on the weaker model, and that replay ends the run.
        if (nth === 3) return refuses("refusal_1");
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
      whileUp: async (life) => {
        // The job's run ends with an `agent_end` the host marks as not terminal, since the job's
        // result continues it; `runLegionPane` settles a prompt only at a terminal one, so this
        // prompt is sent here. The refusal comes inside this run, after the job has started, so
        // the wait begins once every step the case takes to set the pane up is behind it: between
        // the refusal and the pane going idle only the host acts, with its replay.
        await life.prompt("Start the job.");
        const refusedAt = life.served[2]?.at ?? 0;
        cameBack = await life.modelChanges(
          refusedAt,
          2,
          refusedAt + COOLDOWN_MS + RETURN_GIVE_UP_MS
        );
        // The job's result is an agent-attributed wake, as a card is: no `before_agent_start`,
        // and the run's model read before the `context` hook of its first request runs.
        first = life.served.length;
        const runs = life.runs();
        await writeFile(path.join(life.workspace, release), "");
        await life.runAfter(runs);
      },
    });

    expect(cameBack).toBe(true);
    expect(pane.models()).toEqual([
      "strong-model",
      "strong-model",
      "strong-model",
      "weak-model",
      "strong-model",
    ]);
    // The order omp printed: the walk at the refusal, the job's run parked on the job once the
    // replay (the fourth turn) has answered, and the return after that with no turn served in
    // between, before the job's result started a run. No hook ran while the pane waited, so the
    // return is the timer's; a return made inside the job's run would come before `parked 4`.
    expect(pane.frames).toEqual(["end 1", "model", "parked 4", "model", "end 5"]);
    // And the return came no earlier than the wait the refusal armed, which the transcript records.
    const armed = (await pane.cooldowns())[0];
    expect(armed).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    expect(pane.changedAt[1]).toBeGreaterThanOrEqual(armed?.arming?.dueAt ?? Infinity);
    // The run is the job's: its first request carries the job's output.
    expect(pane.served[first]?.messages).toContain("released");
  },
  180_000
);
