import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { renameSync, writeFileSync } from "node:fs";
import { readFile, writeFile } from "node:fs/promises";
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
  deleteRelayCapturingStreams,
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
import { useRowGuard } from "@legion/test-fixtures/deadline";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
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

/** How many requests the host sends the model a classifier refused before any output, before it
 * walks its own chain: the refused one and the `CLASSIFIER_REFUSAL_SAME_MODEL_RETRIES` it sends
 * again at once (two, `session/refusal-retries.ts` at the pinned tag). A refusal after streamed
 * text is not sent again. */
const REFUSED_SENDS = 3;

/** The models of the first `REFUSED_SENDS` requests a pre-output refusal of `model` takes. */
const refusedSends = (model: string): string[] => Array<string>(REFUSED_SENDS).fill(model);

/**
 * The first wait a refusal of one selector buys, through `LEGION_REFUSAL_COOLDOWN_MS`
 * (`fallbackCooldownLimits`): seconds here, an hour in a deployment, so every case below runs
 * well inside the pi-legion job's limit (timeout-minutes: 28,
 * .github/workflows/ci-job-pi-legion.yaml), and every case on the pane's own clock sees its waits
 * run out while it watches. A case that holds the pane's clock (`heldClock`) ends the wait itself.
 */
const COOLDOWN_MS = 4_000;

/**
 * A wait no case reaches: longer than the longest case's own timeout (300 s), and under the cap
 * the pane sets (`LEGION_REFUSAL_COOLDOWN_MAX_MS`, 600 s), so its refusal raises the doubling level
 * as a deployment's does. A life armed with it never comes back on its own, so no step of the
 * case, however slow, can let the return come before the turn the case reads.
 */
const UNREACHED_COOLDOWN_MS = 400_000;

// Each case starts a cold Oh My Pi pane (a pinned binary spawning and registering over NATS, which
// reads 5-6 s) and watches its cooldowns run out, so it needs more than Bun's 5000 ms default in
// every mode: `bun run test`'s workers declare 300000 ms (`test-timeout-preload.ts`), but a run of
// this file alone takes no worker and keeps 5000 ms. Each case therefore takes a guard of its own,
// as the cases of `legion-stall-watchdog-omp.test.ts` do: the case calls `useRowGuard` for the
// shared wait's deadline, which every wait of the pane's and of the case's own ends at, and passes
// the same value as `test`'s third argument for Bun's row kill, and the afterEach, whose cleanup
// kills a real Oh My Pi, passes it as its own hook timeout.
const COLD_PANE_GUARD_MS = 300_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
}, COLD_PANE_GUARD_MS);

/** A directory of one case's own: the file of the pane's checks, and the file of a clock the case
 * holds. */
const scratchDir = temporaryDirectories("fallback-omp");

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
  /** Sets the pane's time to `at`, epoch ms, as a new file renamed over the old one, so the pane
   *  never reads half a time. */
  readonly moveTo: (at: number) => void;
}

/** A held clock for one case, in a directory of its own, which starts at the case's own time. */
function heldClock(): HeldClock {
  const file = path.join(scratchDir("clock"), "clock");
  const moveTo = (at: number): void => {
    writeFileSync(`${file}.next`, String(at));
    renameSync(`${file}.next`, file);
  };
  moveTo(Date.now());
  return { file, moveTo };
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
  /** The same states, each with when omp recorded it (`LegionPane.transcriptRecords`). */
  readonly cooldownsAt: () => Promise<Recorded<FallbackCooldownState>[]>;
  /** Each check of a refusal's arming, by the pane's own record
   *  (`LEGION_FALLBACK_CHECK_RECORD_FILE`, src/model-fallback.ts), over both lives, in order. */
  readonly checks: () => Promise<RestoreCheck[]>;
  /** Each change of the session's model the transcript records, in order: the model it set
   * (`standin/...`), and when omp recorded the change. */
  readonly modelTimeline: () => Promise<Recorded<string>[]>;
  /**
   * Each change of the session's model and each run's end that omp printed, in order: a change as
   * `model`, an end as `end <n>`, or `parked <n>` when the host marked the run as waiting on a
   * background job's result (`awaitingAsyncWork`). `n` is how many turns the gateway had served
   * when the end arrived, which is exact: the gateway answers a run's last turn before omp can end
   * the run. A change carries no count, since omp's frames and the gateway's requests reach the
   * case on two streams, and a change printed before a request can be read after it.
   */
  readonly frames: readonly string[];
}

/** A value a pane's transcript records, and when omp wrote its entry (epoch ms), by the omp
 * process's own clock: the clock a cooldown's due time is on, so the two compare whatever the test
 * process was doing at the time. */
interface Recorded<T> {
  readonly value: T;
  readonly at: number;
}

/** The entries of `transcript` that `pick` reads a value from, each with its entry's time. */
function recorded<T>(
  transcript: readonly unknown[],
  pick: (entry: object) => T | undefined
): Recorded<T>[] {
  return transcript.flatMap((entry) => {
    if (typeof entry !== "object" || entry === null) return [];
    const value = pick(entry);
    if (value === undefined) return [];
    const at =
      "timestamp" in entry && typeof entry.timestamp === "string"
        ? Date.parse(entry.timestamp)
        : Number.NaN;
    return [{ value, at }];
  });
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
  /** Resolves once the host has reported `count` changes of the session's model since the
   * gateway had served `afterTurns` turns, through the shared wait: a pane that never makes them
   * fails at the row's deadline, naming the changes it waited for. A change counts by how many
   * turns the gateway had served when the case read its frame, which can be past a request omp
   * printed the change before (`frames` says why), so a case counts from a turn that a run's end,
   * or the refusal itself, puts after every change it must leave out. */
  readonly modelChanges: (afterTurns: number, count: number) => Promise<void>;
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
  const checkRecord = path.join(scratchDir("checks"), "checks.jsonl");
  /** The session the listener registered, whose deliver subject a card is relayed on. */
  let sessionID = "";
  let workspace = "";
  /** How many turns the gateway had served when each `model_changed` frame arrived, and how many
   * runs have ended. */
  const modelChanged: number[] = [];
  let runsEnded = 0;
  const frames: string[] = [];
  const { whileUp } = options;

  /** The case's view of the life `rpc` belongs to, over the real NATS server `nc`. */
  const idleLife = (rpc: Rpc, nc: NatsConnection): IdleLife => {
    /** Resolves once `holds` does, through the shared wait, which ends at the row's deadline
     * naming `what`; rejects, with omp's stderr, once omp has closed its RPC stream. */
    const whileLive = async (what: string, holds: () => boolean): Promise<void> => {
      await Promise.race([until(what, holds), rpc.closed]);
    };
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
        // The send is offered until the session takes it, through the shared wait: a pane that
        // never takes it fails at the row's deadline, and one whose omp has exited fails the send
        // at once with omp's stderr.
        const stopped: { error?: unknown } = {};
        rpc.closed.catch((error: unknown) => {
          stopped.error = error;
        });
        await sendRelayFrame({
          connection: nc,
          newHeaders: headers,
          sessionID,
          subject: `notifications.agent.${sessionID}`,
          seq,
          stopped,
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
      modelChanges: (afterTurns, count) =>
        whileLive(
          `the host to report ${count} changes of the session's model once the gateway had served ${afterTurns} turns`,
          () => modelChanged.filter((turns) => turns >= afterTurns).length >= count
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
        if ("type" in frame && frame.type === "model_changed") {
          modelChanged.push(served.length);
          frames.push("model");
        }
        if ("type" in frame && frame.type === "agent_end") {
          runsEnded += 1;
          const parked = "awaitingAsyncWork" in frame && frame.awaitingAsyncWork === true;
          frames.push(`${parked ? "parked" : "end"} ${served.length}`);
        }
      },
      ...(whileUp === undefined
        ? {}
        : {
            whileUp: async (rpc: Rpc) => {
              const nc = await connect({ servers: nats });
              cleanup.push(() => nc.close());
              // No stream another suite left on this server may answer the frames
              // (`deleteRelayCapturingStreams`).
              await deleteRelayCapturingStreams(nc);
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
    cooldowns: async () =>
      (await pane.transcriptEntries(LEGION_FALLBACK_ENTRY)) as FallbackCooldownState[],
    cooldownsAt: async () =>
      (await pane.transcriptRecords(LEGION_FALLBACK_ENTRY)).map(({ at, data }) => ({
        value: data as FallbackCooldownState,
        at,
      })),
    checks: async () =>
      (await readFile(checkRecord, "utf8").catch(() => ""))
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as RestoreCheck),
    modelTimeline: async () =>
      recorded(await pane.transcript(), (entry) =>
        "type" in entry &&
        entry.type === "model_change" &&
        "model" in entry &&
        typeof entry.model === "string"
          ? entry.model
          : undefined
      ),
    frames,
  };
}

/**
 * Writes `dueAt` into the arming the pane's transcript recorded last, once its first life is gone:
 * on the omp process's own clock, which no test can move, a case that relaunches a pane sets the
 * boundary of the wait the resumed life restores itself, after every step it takes before the
 * relaunch.
 */
async function moveDueTime(pane: FallbackPane, dueAt: number): Promise<void> {
  await pane.editTranscriptEntry(LEGION_FALLBACK_ENTRY, (data) => {
    const state = data as FallbackCooldownState;
    if (state.arming === undefined) throw new Error("the last fallback state holds no arming");
    return { ...state, arming: { ...state.arming, dueAt } };
  });
}

/** The wait `armed`'s arming bought, by the pane's own record: its due time less the moment it was
 * armed. */
function waitOf(armed: Recorded<FallbackCooldownState> | undefined): number {
  const arming = armed?.value.arming;
  if (arming?.armedAt === undefined) throw new Error("the pane recorded no arming with its moment");
  return arming.dueAt - arming.armedAt;
}

/** Holds a return to the first check at or after its arming's due time, by the pane's own record
 *  of each check, whichever trigger made it (a request's `context` hook, a run's
 *  `before_agent_start`, the timer on an idle pane): every check before the due time read the
 *  arming not due, and the one check at or after it read it due and put the session back, so no
 *  request went to the weaker model once a check had found the wait over. */
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
    useRowGuard(COLD_PANE_GUARD_MS);
    const pane = await fallbackPane({
      // One assignment, one run: the host's sends of the refused turn, its replay on the weaker
      // model and every turn after it ask the gateway again, a second apart, while the session is
      // on the weaker model, so no step of the case falls between the refusal and the return. The
      // run ends at its first request to any other model: it ends on the return, whenever that
      // comes, and a session that never comes back fails the run's wait at the row's deadline.
      prompt: ["Start FALL-1."],
      answer: (model, nth) => {
        if (nth <= REFUSED_SENDS) return refuses(`refusal_${nth}`);
        return model === "weak-model"
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    expect(pane.models().slice(0, REFUSED_SENDS + 1)).toEqual([
      ...refusedSends("strong-model"),
      "weak-model",
    ]);
    // One assignment, and every request of it the same turn: the host sent the refused turn to
    // the stronger model again and then replayed it on the weaker one itself, which it may do
    // because nothing of it had been committed, rather than the plugin moving a later turn.
    expect(userText(pane.turns()[REFUSED_SENDS] as Request)).toContain("Start FALL-1.");

    // The host walked its own chain, and the host's replay of the turn succeeded, so the settled
    // turn reads clean and only the fallback event itself says a filter refused the session. The
    // arming it leaves is a refusal's all the same: the same cooldown and the same doubling the
    // plugin's own walk takes.
    const states = await pane.cooldownsAt();
    const armed = states[0];
    expect(armed?.value).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The wait the refusal bought is the base cooldown, counted from the moment the pane armed it.
    expect(waitOf(armed)).toBe(COOLDOWN_MS);
    const dueAt = armed?.value.arming?.dueAt ?? 0;
    // The defect this case is the proof of: an arming the plugin read as anything but a refusal
    // waits on a suppression the host never applies for a classifier refusal — it pins the
    // fallback instead — so the session went straight back to the model that had just refused
    // it. Nothing goes back before the wait is over: the session's model changes, which omp
    // records on the clock the due time is on, are its start on the stronger model, the host's
    // walk, and the return, no earlier than the due time, so no request served in the wait went
    // to the stronger model.
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, WEAK, STRONG]);
    expect(timeline[2]?.at).toBeGreaterThanOrEqual(dueAt);
    // And the session does come back, once the wait is over: a turn after the host's replay went
    // to the stronger model.
    expect(pane.models().slice(REFUSED_SENDS)).toContain("strong-model");
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a refusal of the fallback model after the host's own walk steps down again and comes back to the model the session started on",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    const pane = await fallbackPane({
      chains: TWO_STEPS,
      // One assignment. Its first turn starts a background job, so the turn the second refusal
      // ends waits on the job's result. The case reads the walks and the return from the
      // transcript, on omp's own clock, so neither the job's length nor a step of the case decides
      // it: the job's result starts a run whenever it comes, before the return or after it.
      prompt: ["Start FALL-1."],
      answer: (model, nth) => {
        if (nth === 1) {
          return replies(
            [call("bash", { command: "sleep 3; echo finished", async: true })],
            `msg_${nth}`
          );
        }
        // Refused before any output: the host sends the turn to the stronger model again, then
        // walks its own chain to the middle model and replays the turn there.
        if (nth <= 1 + REFUSED_SENDS) return refuses(`refusal_${nth}`);
        // The replay is refused after text, which the host leaves where it is, and the turn ends
        // waiting on the job's result: the plugin's own walk takes it from the middle model to
        // the weaker one.
        if (nth === 2 + REFUSED_SENDS) return refuses("refusal_middle", { text: "Looking at it." });
        // The job's result starts a run that asks the gateway every second while the session is
        // on the weaker model, and ends at its first request to any other: on the return, or at
        // once when the result comes after it. A session that never comes back fails the run's
        // wait at the row's deadline.
        return model === "weak-model"
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The job's start, the refused turn's sends, and the host's replay of it on the middle model.
    expect(pane.models().slice(0, 2 + REFUSED_SENDS)).toEqual([
      "strong-model",
      ...refusedSends("strong-model"),
      "middle-model",
    ]);
    const states = await pane.cooldownsAt();
    const armed = states.findLast((entry) => entry.value.arming !== undefined);
    // The defect this case is the proof of: the plugin's own walk armed the return on the model
    // it was leaving, so the session came back to the middle of the chain and never to the model
    // it started on. The second step continues the host's own walk, so the return still points
    // at the stronger model, and the wait it bought is the doubled one of that model, counted
    // from the moment the pane armed it.
    expect(armed?.value.arming).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(armed?.value.levels).toEqual({ [STRONG]: 3 });
    expect(waitOf(armed)).toBe(2 * COOLDOWN_MS);
    const dueAt = armed?.value.arming?.dueAt ?? 0;
    // Nothing went back before that wait, and what the session went back to is the stronger
    // model: the session's model changes, which omp records on the clock the due time is on, are
    // its start on the stronger model, the host's walk to the middle one, the plugin's walk to the
    // weaker one, and the return to the stronger one, no earlier than the due time, so no request
    // served in the wait went to the stronger model. A turn after the replay went to the stronger
    // model, and none of this pane went to the middle model again.
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, MIDDLE, WEAK, STRONG]);
    expect(timeline[3]?.at).toBeGreaterThanOrEqual(dueAt);
    expect(pane.models().slice(2 + REFUSED_SENDS)).toContain("strong-model");
    expect(pane.models().slice(2 + REFUSED_SENDS)).not.toContain("middle-model");
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a refusal after streamed text, which the host leaves where it is, sends the next turn to the chain's next model",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
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
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "the session is put back on the model it fell back from inside a turn already running, once the cooldown is over",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    const pane = await fallbackPane({
      // One assignment, one run, so no step of the case falls inside the wait.
      prompt: ["Start FALL-1."],
      answer: (_model, nth) => {
        // Refused before any output: the host sends the turn to the stronger model again, then
        // walks its own chain and replays the turn on the weaker model inside the same run.
        if (nth <= REFUSED_SENDS) return refuses(`refusal_${nth}`);
        // One long turn on the weaker model: the sleep outlasts the cooldown, so the wait ends
        // while the turn's tool runs. It starts after the refusal, so it outlasts what is left of
        // the wait however late it starts.
        if (nth === REFUSED_SENDS + 1) return replies([sleeps(6)], `msg_${nth}`);
        if (nth === REFUSED_SENDS + 2) {
          return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        }
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The wait ends while the turn runs, so the runner's timer switches no model: a switch there
    // could land in the middle of a stream. The turn's own `context` hook makes the return, and the
    // host chooses a request's model before it runs that hook, so the hook's switch is the model of
    // the *next* request of that turn: the request sent after the sleep is still the weaker
    // model, and the one after it is the model the session fell back from — inside the same
    // turn, which no new prompt and no host setting would have reached.
    expect(pane.models()).toEqual([
      ...refusedSends("strong-model"),
      "weak-model",
      "weak-model",
      "strong-model",
    ]);
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The return clears the arming and leaves the doubling where it stands.
    expect(states.at(-1)).toEqual({ levels: { [STRONG]: 1 } });
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a second refusal of the same model doubles its wait, so the return does not come at the first one",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    /** Whether the stronger model has refused a second time: the refusal whose wait has doubled. */
    let secondRefused = false;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        // The first assignment is refused on the stronger model, which arms the base wait.
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // The second runs long enough for the return, and is refused again the moment it lands
        // on the stronger model: that refusal is the one whose wait has doubled.
        if (model === "strong-model" && !secondRefused) {
          secondRefused = true;
          return refuses("refusal_2", { text: "Looking at it again." });
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (!secondRefused) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The third assignment's turn asks the gateway every two seconds while the session is on
        // the weaker model, and ends at its first request to the stronger one: on the return,
        // whenever that comes. A session that never comes back fails the run's wait at the row's
        // deadline.
        return model === "weak-model"
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const states = await pane.cooldownsAt();
    // The first refusal of the stronger model took the base wait and raised its level; the second
    // took twice the base, counted from the moment the pane armed it, and raised it again.
    expect(states[0]?.value).toMatchObject({ levels: { [STRONG]: 2 } });
    const armings = states.filter((entry) => entry.value.arming !== undefined);
    const doubled = armings.at(-1);
    expect(doubled?.value.levels).toEqual({ [STRONG]: 3 });
    expect(waitOf(doubled)).toBe(2 * COOLDOWN_MS);
    const dueAt = doubled?.value.arming?.dueAt ?? 0;
    // Each return came no earlier than the due time of the arming before it: the session's model
    // changes, which omp records on the clock the due times are on, are its start, the first
    // walk, the first return, the second walk and the second return.
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, WEAK, STRONG, WEAK, STRONG]);
    expect(timeline[2]?.at).toBeGreaterThanOrEqual(armings[0]?.value.arming?.dueAt ?? Infinity);
    expect(timeline[4]?.at).toBeGreaterThanOrEqual(dueAt);
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a clean turn on the model the session came back to puts its wait back to the base",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    /** How many turns the stronger model has been sent: the first is refused, the second is the
     * clean turn that puts the wait back, the third is refused again, before any output, and so are
     * the host's sends of it again. */
    let onStrong = 0;
    const pane = await fallbackPane({
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        if (model === "strong-model") {
          onStrong += 1;
          if (onStrong === 1) return refuses("refusal_1", { text: "Looking at it." });
          // The clean turn: it ends the second assignment's run on the stronger model.
          if (onStrong === 2) return replies([{ type: "text", text: "Back on it." }], `msg_${nth}`);
          // The second refusal comes before any output: the host sends the turn again, then walks
          // its own chain and replays the turn on the weaker model, and that replay ends the last
          // run, so the pane is idle while the wait runs and the runner's own timer makes the
          // return, at the due time.
          if (onStrong >= 3 && onStrong < 3 + REFUSED_SENDS) return refuses(`refusal_${onStrong}`);
          return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (onStrong < 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
      whileUp: async (life) => {
        // The pane stays up until the return: the host's walk is the first model change once the
        // gateway had served the second refusal, and the return is the second. The return is the
        // timer's, on omp's own clock, so how long the case takes to get here or to read the frame
        // decides nothing; a pane that never comes back fails this wait at the row's deadline.
        const strongTurns = life.served.flatMap((turn, index) =>
          turn.model === "strong-model" ? [index] : []
        );
        const secondRefusal = strongTurns[2];
        if (secondRefusal === undefined)
          throw new Error("the stronger model was not refused again");
        await life.modelChanges(secondRefusal + 1, 2);
      },
    });

    const states = await pane.cooldownsAt();
    const armings = states.filter((entry) => entry.value.arming !== undefined);
    // Both refusals of the stronger model armed from level 1 and raised it to 2: the clean turn
    // between them put back what the first one raised, so the second bought the base wait again
    // rather than twice it, by the pane's own record of the moment it armed and the due time.
    expect(armings.map((entry) => entry.value.levels[STRONG])).toEqual([2, 2]);
    const secondArming = states.findLastIndex((entry) => entry.value.arming !== undefined);
    expect(states[secondArming - 1]?.value).toEqual({ levels: { [STRONG]: 1 } });
    expect(waitOf(states[secondArming])).toBe(COOLDOWN_MS);
    const dueAt = states[secondArming]?.value.arming?.dueAt ?? 0;
    // Each return came no earlier than the due time of the arming before it: the session's model
    // changes, which omp records on the clock the due times are on, are its start, the first
    // walk, the first return, the host's second walk and the timer's return.
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, WEAK, STRONG, WEAK, STRONG]);
    expect(timeline[2]?.at).toBeGreaterThanOrEqual(armings[0]?.value.arming?.dueAt ?? Infinity);
    expect(timeline[4]?.at).toBeGreaterThanOrEqual(dueAt);
    // And no later than it says: the first check of the second arming at or after its due time,
    // the timer's on the idle pane, put the session back.
    expectBackAtFirstDueCheck(await pane.checks(), dueAt);
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a pane whose process is gone resumes its own transcript's arming, and comes back no earlier than it says",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    // The case holds the pane's clock: the wait the refusal buys is still running when the first
    // life ends, however long that life and the relaunch take, and it ends where the case moves
    // the clock. The resumed life's long turn asks the gateway every two seconds while the
    // session is on the weaker model and ends at its first request to the stronger one: on the
    // return. A life that never comes back fails the run's wait at the row's deadline. The first
    // life's turns end at once.
    const clock = heldClock();
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
        // The resumed life's second request moves the pane's clock to the restored due time, so
        // the wait ends while that life's turn runs, after a request of it went out inside it.
        if (dueAt !== undefined && ++resumed === 2) clock.moveTo(dueAt);
        return model === "weak-model"
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The first life ends on the arming: its last record holds it, so it never put the session
    // back, and the wait is the transcript's to carry over. The wait is the base one, counted from
    // the moment the pane armed it.
    const armed = (await pane.cooldownsAt()).at(-1);
    expect(armed?.value.arming).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(waitOf(armed)).toBe(COOLDOWN_MS);
    dueAt = armed?.value.arming?.dueAt;
    if (dueAt === undefined) throw new Error("the first life recorded no due time");
    const firstLife = pane.served.length;

    // The daemon relaunches the worker: a fresh process whose only memory of the fallback is the
    // transcript it resumes.
    await pane.resume([], "Carry on after the relaunch.");

    // The arming was restored, not started again: the resumed life armed nothing of its own, so
    // the one arming on record is the first life's. A life that had started the wait again would
    // have recorded an arming of its own, whatever its boot cost.
    const states = await pane.cooldownsAt();
    expect(states.filter((entry) => entry.value.arming !== undefined)).toEqual([
      armed as Recorded<FallbackCooldownState>,
    ]);
    // And the return did come, which is what says the arming survived at all: a life that had
    // restored nothing would have stayed on the weaker model for good. It came on this life's own
    // turns, after the request that went out inside the restored wait, the one at which the clock
    // moved, and the one whose `context` hook put the session back (the host chose that request's
    // model before the hook ran).
    const timeline = await pane.modelTimeline();
    const walk = timeline.findIndex((change) => change.value === WEAK);
    expect(walk).toBeGreaterThan(0);
    expect(timeline.slice(walk + 1).map((change) => change.value)).toContain(STRONG);
    expect(pane.beforeReturn(firstLife - 1).length).toBeGreaterThan(2);
    // And no earlier and no later than it says, over the two lives, by the pane's own record of
    // each check: every check before the due time read the wait still running, and the first at
    // or after it put the session back.
    expectBackAtFirstDueCheck(await pane.checks(), dueAt);
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a pane relaunched once its restored wait is over sends its first turn to the model it fell back from",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
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
    // longer than the cooldown it fell back for. The wait the resumed life restores ended the
    // moment the case wrote it, so it is over by the relaunch.
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
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a card that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    const summary = `CARD-${randomUUID().slice(0, 8)}`;
    /** What the idle life saw: how many turns had been served when the return came, and the
     * first turn the card's run served. */
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
        // the wait is over, or this wait fails at the row's deadline, naming it. The walk is the
        // first model change the host reports once the gateway had served the refusal, the pane's
        // first turn, and the return is the second.
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
    // And it came no earlier than the wait the refusal armed: the walk and the return, which omp
    // records on the clock the due time is on.
    const armed = (await pane.cooldowns())[0];
    expect(armed).toMatchObject({ arming: { restoreTo: STRONG, cause: "refusal" } });
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, WEAK, STRONG]);
    expect(timeline[2]?.at).toBeGreaterThanOrEqual(armed?.arming?.dueAt ?? Infinity);
    const woken = pane.served.slice(first);
    expect(woken.length).toBeGreaterThan(0);
    // The run is the card's: its first request carries the card.
    expect(woken[0]?.messages).toContain(summary);
    expect(woken[0]?.model).toBe("strong-model");
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a background job's result that wakes an idle pane once its wait is over starts its run on the model it fell back from",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    /** The file whose existence ends the background job: the case makes it once the wait is
     * over, so the job's result comes when the case says rather than at a sleep's end. */
    const release = ".release-job";
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
        // The turn after the job has started is refused before any output: the host sends it to
        // the stronger model again, then walks its own chain and replays the turn on the weaker
        // model, and that replay ends the run.
        if (nth >= 3 && nth < 3 + REFUSED_SENDS) return refuses(`refusal_${nth}`);
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
      whileUp: async (life) => {
        // The job's run ends with an `agent_end` the host marks as not terminal, since the job's
        // result continues it; `runLegionPane` settles a prompt only at a terminal one, so this
        // prompt is sent here. The refusal comes inside this run, after the job has started, so
        // the wait begins once every step the case takes to set the pane up is behind it: between
        // the refusal and the pane going idle only the host acts, with its replay.
        await life.prompt("Start the job.");
        // The walk and the return are the two model changes once the gateway had served the
        // refusal, the pane's third turn; a pane that never comes back fails this wait at the
        // row's deadline, naming it.
        await life.modelChanges(3, 2);
        // The job's result is an agent-attributed wake, as a card is: no `before_agent_start`,
        // and the run's model read before the `context` hook of its first request runs.
        first = life.served.length;
        const runs = life.runs();
        await writeFile(path.join(life.workspace, release), "");
        await life.runAfter(runs);
      },
    });

    expect(pane.models()).toEqual([
      "strong-model",
      "strong-model",
      ...refusedSends("strong-model"),
      "weak-model",
      "strong-model",
    ]);
    // The order omp printed: the walk at the last refused send, the job's run parked on the job
    // once the replay has answered, and the return after that with no turn served in between,
    // before the job's result started a run. No hook ran while the pane waited, so the return is
    // the timer's; a return made inside the job's run would come before the parked end.
    const replay = 3 + REFUSED_SENDS;
    expect(pane.frames).toEqual([
      "end 1",
      "model",
      `parked ${replay}`,
      "model",
      `end ${replay + 1}`,
    ]);
    // And the return came no earlier than the wait the refusal armed, which the transcript records.
    const armed = (await pane.cooldowns())[0];
    expect(armed).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    const timeline = await pane.modelTimeline();
    expect(timeline.map((change) => change.value)).toEqual([STRONG, WEAK, STRONG]);
    expect(timeline[2]?.at).toBeGreaterThanOrEqual(armed?.arming?.dueAt ?? Infinity);
    // The run is the job's: its first request carries the job's output.
    expect(pane.served[first]?.messages).toContain("released");
  },
  COLD_PANE_GUARD_MS
);

test.skipIf(skip)(
  "a refusal after streamed text that ends a turn waiting on a background job moves the session down before the job's result starts its run",
  async () => {
    useRowGuard(COLD_PANE_GUARD_MS);
    /** The file whose existence ends the background job, which the case makes once the walk has
     * set the model, so the job's result cannot start its run before that. */
    const release = ".release-job";
    let first = 0;
    const pane = await fallbackPane({
      // The case reads where the job's result starts its run, never the return: a wait no case
      // reaches, so no step of the case can let the return come first.
      cooldownMs: UNREACHED_COOLDOWN_MS,
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
        // The turn after the job has started is refused after text, which the host leaves where
        // it is: the turn ends waiting on the job's result, with `willContinue` and
        // `awaitingAsyncWork` both set.
        if (nth === 3) return refuses("refusal_1", { text: "Looking at it." });
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
      whileUp: async (life) => {
        // As in the case above: the job's run ends with an `agent_end` the host marks as not
        // terminal, so this prompt is sent here.
        await life.prompt("Start the job.");
        // The walk is the one model change the host reports once the gateway had served the
        // refusal, the pane's third turn; a plugin that reads that end as a retry makes none, and
        // this wait then fails at the row's deadline, naming it.
        await life.modelChanges(3, 1);
        first = life.served.length;
        const runs = life.runs();
        await writeFile(path.join(life.workspace, release), "");
        await life.runAfter(runs);
      },
    });

    // The job's result starts its run on the model the plugin's own walk moved the session to:
    // the refused turn settled when it ended waiting on the job, since the host retries nothing
    // then.
    expect(pane.models()).toEqual(["strong-model", "strong-model", "strong-model", "weak-model"]);
    expect(pane.served[first]?.messages).toContain("released");
    // And the walk wrote the arming a refusal at a terminal end writes.
    expect((await pane.cooldowns()).at(-1)).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
  },
  COLD_PANE_GUARD_MS
);
