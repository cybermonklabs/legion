import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { renameSync, writeFileSync } from "node:fs";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
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
import { until } from "@legion/test-fixtures/wait";
import { connect, headers, type NatsConnection, StringCodec } from "nats";
import {
  FALLBACK_CHECK_RECORD_FILE,
  FALLBACK_CLOCK_FILE,
  type RestoreCheck,
} from "../src/model-fallback";

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
 * (`fallbackCooldownLimits`): seconds here, an hour in a deployment, so every case on the pane's
 * own clock sees its waits run out while it watches. A case that holds the pane's clock
 * (`heldClock`) ends the wait itself.
 */
const COOLDOWN_MS = 4_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/**
 * The model fallback's clock in a pane a case holds it for (`FALLBACK_CLOCK_FILE`,
 * src/model-fallback.ts `fallbackClock`): the pane reads the time the case set last, which holds
 * until the case moves it, and each move runs every timer of the return the new time has reached.
 * So where a case's verdict needs a wait to be still running, or over, at a step of its own, the
 * case says when the wait ends, and no pane a loaded box slows reaches the due time by itself.
 */
interface HeldClock {
  /** The file the pane reads the time from. */
  readonly file: string;
  /** The time the pane starts on, epoch ms: the case's own when it made the clock. */
  readonly start: number;
  /** Sets the pane's time to `at`, epoch ms, as a new file renamed over the old one, so the pane
   *  never reads half a time. */
  readonly moveTo: (at: number) => void;
}

/** A held clock for one case, in a directory of its own that the case's cleanup removes. */
async function heldClock(): Promise<HeldClock> {
  const directory = await mkdtemp(path.join(tmpdir(), "fallback-clock-"));
  cleanup.push(() => rm(directory, { recursive: true, force: true }));
  const file = path.join(directory, "clock");
  const moveTo = (at: number): void => {
    writeFileSync(`${file}.next`, String(at));
    renameSync(`${file}.next`, file);
  };
  const start = Date.now();
  moveTo(start);
  return { file, start, moveTo };
}

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

/** One turn the gateway served: the model it was sent to, and its request's messages as JSON. Its
 * place in `served` is the order the turns arrived in, which is all a case reads of when they
 * came. */
interface Served {
  readonly model: string;
  readonly messages: string;
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
  /** Each check of a refusal's arming, by the pane's own record
   *  (`LEGION_FALLBACK_CHECK_RECORD_FILE`, src/model-fallback.ts), over both lives, in order. */
  readonly checks: () => Promise<RestoreCheck[]>;
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
  /** How many changes of the session's model the host has reported once the gateway had served
   * `afterTurns` turns: the refusal is the first turn, so `changes(1)` counts the walk and what
   * came after it. */
  readonly changes: (afterTurns: number) => number;
  /** Resolves once `changes(afterTurns)` is `count`, through the shared wait: a pane that never
   * gets there fails at the row's deadline, naming the changes it waited for. `each`, where given,
   * runs before each read. */
  readonly modelChanges: (afterTurns: number, count: number, each?: () => void) => Promise<void>;
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
  /** The pane's fallback clock, where the case holds it (`heldClock`); the pane's own clock,
   *  unset. */
  readonly clock?: HeldClock;
}): Promise<FallbackPane> {
  const { binary, nats } = setup();
  const served: Served[] = [];
  const scratch = await mkdtemp(path.join(tmpdir(), "fallback-checks-"));
  cleanup.push(() => rm(scratch, { recursive: true, force: true }));
  const checkRecord = path.join(scratch, "checks.jsonl");
  /** The session the listener registered, whose deliver subject a card is relayed on. */
  let sessionID = "";
  let workspace = "";
  /** How many turns the gateway had served when each `model_changed` frame arrived, and how many
   * runs have ended. */
  const modelChanged: number[] = [];
  let runsEnded = 0;
  const { whileUp } = options;

  /** The case's view of the life `rpc` belongs to, over the real NATS server `nc`. */
  const idleLife = (rpc: Rpc, nc: NatsConnection): IdleLife => {
    /** Resolves once `holds` does, through the shared wait, which ends at the row's deadline
     * naming `what`; rejects, with omp's stderr, once omp has closed its RPC stream. */
    const whileLive = async (what: string, holds: () => boolean): Promise<void> => {
      await Promise.race([until(what, holds), rpc.closed]);
    };
    const changes = (afterTurns: number): number =>
      modelChanged.filter((turns) => turns >= afterTurns).length;
    const codec = StringCodec();
    let seq = 0;
    return {
      served,
      workspace,
      prompt: async (message) => {
        const ended = runsEnded;
        rpc.send({ type: "prompt", message });
        await whileLive("the run the prompt started to end", () => runsEnded > ended);
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
        await whileLive("the run the card started to end", () => runsEnded > ended);
      },
      runs: () => runsEnded,
      runAfter: async (runs) => {
        await whileLive(`a run after the ${runs}th to end`, () => runsEnded > runs);
      },
      changes,
      modelChanges: (afterTurns, count, each) =>
        whileLive(
          `the host to report ${count} changes of the session's model once the gateway had served ${afterTurns} turns`,
          () => {
            each?.();
            return changes(afterTurns) >= count;
          }
        ),
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
        [FALLBACK_CHECK_RECORD_FILE]: checkRecord,
        ...(options.clock === undefined ? {} : { [FALLBACK_CLOCK_FILE]: options.clock.file }),
      },
      prepare: async (paths) => {
        workspace = paths.workspace;
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "model_changed") modelChanged.push(served.length);
        if ("type" in frame && frame.type === "agent_end") runsEnded += 1;
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
        served.push({ model, messages: JSON.stringify(body.messages ?? null) });
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
    checks: async () =>
      (await readFile(checkRecord, "utf8").catch(() => ""))
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as RestoreCheck),
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

/** Holds a return to the first check at or after its arming's due time, by the pane's own record
 *  of each check, whichever trigger made it (a request's `context` hook, a run's
 *  `before_agent_start`, the timer on an idle pane; R4: the bound the pane chose and what it did
 *  at it, read across the process boundary, not a clock): every check before the due time read the
 *  arming not due, and the one check at or after it read it due and put the session back, so no
 *  request went to the weaker model once the wait was over. */
function expectBackAtFirstDueCheck(checks: readonly RestoreCheck[], dueAt: number): void {
  const ofArming = checks.filter((check) => check.dueAt === dueAt);
  expect(ofArming.filter((check) => check.at < dueAt && check.due)).toEqual([]);
  expect(ofArming.filter((check) => check.at >= dueAt)).toEqual([
    { at: expect.any(Number), dueAt, due: true },
  ]);
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
        // One long turn on the weaker model: the sleep outlasts the cooldown, so the wait ends
        // while the turn's tool runs.
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
    // And no later than it says: the first request past the due time came back.
    expectBackAtFirstDueCheck(await pane.checks(), secondArming?.state.arming?.dueAt ?? Number.NaN);
  }
);

test.skipIf(skip)(
  "a pane whose process is gone resumes its own transcript's arming, and comes back no earlier than it says",
  async () => {
    // The case holds the pane's clock: the wait the refusal buys is still running when the first
    // life ends, however long that life takes, and it ends where the case moves the clock.
    const clock = await heldClock();
    /** The restored arming's due time, once the first life has ended on it. */
    let dueAt: number | undefined;
    /** How many requests the resumed life has served. */
    let resumed = 0;
    const pane = await fallbackPane({
      clock,
      // The second assignment is a tool-call turn: its own entries carry the arming written at
      // the end of the refused turn to disk, which is where the resumed life reads it.
      prompt: ["Start FALL-1.", "Note that."],
      answer: (model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        if (nth === 2) return replies([call("bash", { command: "echo noted" })], `msg_${nth}`);
        if (nth === 3) return replies([{ type: "text", text: "Noted." }], `msg_${nth}`);
        // The resumed life's turn asks the gateway every two seconds until the session is back.
        // Its second request moves the pane's clock to the restored due time, so the wait ends
        // while that turn runs, after a request of this life went out inside it.
        if (dueAt !== undefined && ++resumed === 2) clock.moveTo(dueAt);
        return asksUntilBack(model, nth, 2);
      },
    });

    // The first life ends on the arming: its last record holds it, so it never put the session
    // back, and the wait is the transcript's to carry over. The wait is the one the case set,
    // counted from the moment the pane armed it.
    const armed = (await pane.records()).at(-1);
    expect(armed?.state.arming).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(waitOf(armed)).toBe(COOLDOWN_MS);
    dueAt = armed?.state.arming?.dueAt;
    if (dueAt === undefined) throw new Error("the first life recorded no due time");
    const firstLife = pane.served.length;

    // The daemon relaunches the worker: a fresh process whose only memory of the fallback is the
    // transcript it resumes.
    await pane.resume([], "Carry on after the relaunch.");

    const records = await pane.records();
    // The arming was restored, not started again: the resumed life armed nothing of its own, so
    // the one arming on record is the first life's, and a record after it is its return. A life
    // that had started the wait again would have recorded an arming of its own, whatever its boot
    // cost, and one that had dropped the arming would record no return.
    const armings = records.filter((record) => record.state.arming !== undefined);
    expect(armings).toEqual([armed as CooldownRecord]);
    returnAfter(records, armings[0]);
    // And the return came on this life's own turns, after the request that went out inside the
    // restored wait, the one at which the clock moved, and the one whose `context` hook put the
    // session back (the host chose that request's model before the hook ran): a life that had
    // restored nothing would have stayed on the weaker model for good.
    expect(pane.beforeReturn(firstLife - 1).length).toBeGreaterThan(2);
    // And no earlier and no later than it says, over the two lives, by the pane's own record of
    // each check: every check before the due time read the wait still running, and the first at
    // or after it put the session back.
    expectBackAtFirstDueCheck(await pane.checks(), dueAt);
  }
);

test.skipIf(skip)(
  "a pane relaunched once its restored wait is over sends its first turn to the model it fell back from",
  async () => {
    // The case holds the pane's clock, so the first life ends inside the wait however long it
    // takes.
    const clock = await heldClock();
    const pane = await fallbackPane({
      clock,
      // As in the case above: the second assignment's entries carry the arming to disk.
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
    const dueAt = armed?.dueAt ?? 0;
    const firstLife = pane.served.length;

    // The daemon relaunches the worker once the wait is over, as it does for a pane that was gone
    // longer than the cooldown it fell back for: the case moves the pane's clock to the due time
    // the transcript records before the relaunch, so the resumed life starts past it.
    clock.moveTo(dueAt);
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
  }
);

test.skipIf(skip)(
  "a card that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    const summary = `CARD-${randomUUID().slice(0, 8)}`;
    /** What the idle life saw: how many turns had been served when the return came, and the first
     * turn the card's run served. */
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
        // The pane is idle now and nothing starts a run of its own: the session comes back when
        // the wait is over or not at all, and a pane that never comes back fails this wait at the
        // row's deadline, naming it. The walk is the first model change the host reports after
        // the refusal, the pane's first turn, and the return is the second.
        await life.modelChanges(1, 2);
        servedAtReturn = life.served.length;
        // A card is an agent-attributed wake: the host starts its run with no
        // `before_agent_start`, and reads the run's model before the `context` hook of its first
        // request runs. So that request goes to the model the session was left on while idle.
        first = life.served.length;
        await life.card(summary);
      },
    });

    // No request ran a hook while the pane waited: the refused one was the last before the return,
    // so the return is the timer's.
    expect(servedAtReturn).toBe(1);
    const woken = pane.served.slice(first);
    expect(woken.length).toBeGreaterThan(0);
    // The run is the card's: its first request carries the card.
    expect(woken[0]?.messages).toContain(summary);
    expect(woken[0]?.model).toBe("strong-model");
  }
);

test.skipIf(skip)(
  "a background job's result that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    /** The file whose existence ends the background job: the case makes it once the wait is
     * over, so the job's result comes when the case says rather than at a sleep's end. */
    const release = ".release-job";
    let changesBeforeIdle = 0;
    let servedAtIdle = 0;
    let servedAtReturn = 0;
    let first = 0;
    // The case holds the pane's clock: the wait is still running however long the job's run
    // takes, and ends where the case moves the clock, once the pane is idle.
    const clock = await heldClock();
    const pane = await fallbackPane({
      clock,
      prompt: ["Start FALL-1."],
      answer: (_model, nth) => {
        // Refused after text: the plugin's own walk moves the session to the weaker model.
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // The second assignment starts a background job and ends its run: the host runs the job
        // on after the run has ended, and wakes the session with its result.
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
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
      whileUp: async (life) => {
        // The job's run ends with an `agent_end` the host marks as not terminal, since the job's
        // result continues it; `runLegionPane` settles a prompt only at a terminal one, so this
        // prompt is sent here.
        await life.prompt("Start the job.");
        // The pane is idle from here. Only the walk may have changed the model so far: a return
        // made inside the job's run would be its own hooks', not the idle return this case is for.
        changesBeforeIdle = life.changes(1);
        servedAtIdle = life.served.length;
        const runs = life.runs();
        // The wait ends now, on the idle pane: the refusal armed it at the clock's first reading.
        // A firing that finds the pane still busy, its job's run just ended, looks again 250 ms of
        // the pane's clock later (`BUSY_RECHECK_MS`, src/model-fallback.ts), so the wait moves the
        // clock that far at each of its reads until the return comes; a pane that never comes back
        // while it is idle fails it at the row's deadline.
        let at = clock.start + COOLDOWN_MS;
        clock.moveTo(at);
        await life.modelChanges(1, 2, () => {
          at += 250;
          clock.moveTo(at);
        });
        servedAtReturn = life.served.length;
        // The job's result is an agent-attributed wake, as a card is: no `before_agent_start`,
        // and the run's model read before the `context` hook of its first request runs.
        first = life.served.length;
        await writeFile(path.join(life.workspace, release), "");
        await life.runAfter(runs);
      },
    });

    expect(changesBeforeIdle).toBe(1);
    // No request ran a hook while the pane waited, so the return is the timer's.
    expect(servedAtReturn).toBe(servedAtIdle);
    const woken = pane.served.slice(first);
    expect(woken.length).toBeGreaterThan(0);
    // The run is the job's: its first request carries the job's output.
    expect(woken[0]?.messages).toContain("released");
    expect(woken[0]?.model).toBe("strong-model");
  }
);
