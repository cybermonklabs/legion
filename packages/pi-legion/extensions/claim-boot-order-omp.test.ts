import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { mkdir, readdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { connect, StringCodec } from "nats";
import { z } from "zod";

// INFRA-1573 on the real binary: a phase worker takes its Envoy name once its first turn has
// started, so a message the listener kept for a role no session held is handed over inside a turn
// that is already running rather than ahead of the daemon's task.
//
// The worker is the pinned Oh My Pi in RPC mode, as the daemon launches a phase worker, with both
// plugin entries from this checkout. One stand-in is the model gateway — each Messages request
// held until the case answers it — the daemon's claim routes and the Envoy listener; the Envoy
// role claim and the delivery that follows it run over the real NATS server. The stand-in
// listener does what the real one does with a message published to a role no session holds: it
// keeps it, records the session that takes the role, and hands the kept message over the moment
// the claim is granted, as a card forwarded on the role's lane to the session's agent subject.
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server, as in this package's
// other real-binary tests. A run without both skips, except on GitHub Actions, where the
// pi-legion job supplies both (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export
// CI=true).
const omp = process.env.LEGION_TEST_OMP;
const natsUrl = process.env.LEGION_TEST_NATS_URL;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = (omp === undefined || natsUrl === undefined) && !onActions;

/** The binary and NATS server a case runs against; throws on GitHub Actions without them. */
function setup(): { readonly binary: string; readonly nats: string } {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  if (natsUrl === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
  return { binary: omp, nats: natsUrl };
}

/**
 * The extensions every Legion pane loads. The Envoy entry is the sibling plugin's: a pane loads
 * both, and the Legion entry refuses to run without it.
 */
const WORKER_EXTENSIONS = [
  path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
  path.resolve(import.meta.dir, "legion.ts"),
];

/** The claim the stand-in daemon registers: a phase worker of one issue, whose Envoy role is its
 * claim token. */
const ROLE = "implementer";
const TREE = "INFRA-1573";
const ISSUE = "INFRA-1573";
const CLAIM_TOKEN = "legion-claim-boot-order-infra-1573-implementer";

/** The daemon's assignment, written to the worker as one RPC `prompt`, as the shim writes it
 * (packages/daemon/internal/shimwire `Prompt`). */
const TASK = "Legion task: implement INFRA-1573 and report.";

/**
 * How long after the spawn a wait may last: Oh My Pi's startup, which loads both extensions
 * before it reads any RPC command, took about 27 s of wall time in a slow but normal pod, so the
 * waits that span it get this much from the spawn.
 */
const STARTUP_DEADLINE_MS = 150_000;

/** How long one wait for something the started session owes a case may last once startup is over. */
const STEP_DEADLINE_MS = 30_000;

/** A case's own timeout: its startup, each of its waits and the cleanup of its processes. */
const CASE_TIMEOUT_MS = 600_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** One Messages request the stand-in holds until the case answers it. */
interface Turn {
  readonly request: Request;
  /** Answers it with one text block. */
  reply(text: string): void;
  /** Answers it with `blocks` (a tool call among them), so the run takes another step. */
  replyBlocks(blocks: readonly Block[]): void;
}

/** The fields of a transcript card entry the cases read; the rest is passed through. */
const CardEntrySchema = z.object({
  type: z.literal("custom_message"),
  customType: z.literal("envoy-message"),
  details: z.object({ handOver: z.string() }),
});

/** A running worker session, the stand-in it runs against, and the message kept for its role. */
interface Worker {
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /**
   * Writes the daemon's delivery of `message` as the shim writes it to Oh My Pi's stdin: one RPC
   * `prompt`. Its response is not awaited here, as the daemon's delivery reads only the turn it
   * starts.
   */
  deliver(message: string): void;
  /**
   * Oh My Pi's own refusal of that prompt, once it has answered with one, else undefined: a
   * prompt the host takes is answered only when its turn ends, a prompt it refuses at once.
   */
  refusal(): string | undefined;
  /** The summary of the message the stand-in listener kept for this worker's role. */
  readonly kept: string;
  /** Resolves once the session has taken that message: the lane receipt it answers the forwarded
   * envelope with before it delivers it (`expectsLaneReceipt`, packages/envoy-client). */
  handedOver(what: string): Promise<void>;
  /** How long after the stand-in answered `claims/ready` the role claim reached the listener. */
  claimedAfterReadyMs(): number;
  /** How many runs of this session have ended: a message read inside a turn that is already
   * running arrives with none of them over. */
  runsEnded(): number;
  /**
   * How the session's own host handed the card carrying `summary` over, read off the transcript
   * Oh My Pi wrote rather than any record of the extension's: `run-active` is a card the session
   * took while a run of its own was running, `startup` one it took while it was idle and still
   * starting (`CardHandOver`, packages/pi-envoy/src/card-wake.ts).
   */
  handOverOf(summary: string): Promise<string>;
}

/**
 * Starts one phase worker against its stand-in: the daemon's claim routes, the Envoy listener
 * that keeps one message for the worker's role, and the model gateway, with `boundMs` as the
 * claim boot's first-turn bound (LEGION_CLAIM_FIRST_TURN_BOUND_MS), so a case drives that bound
 * rather than waiting out the 30 seconds a deployment runs on.
 */
async function startWorker(options: { readonly boundMs: number }): Promise<Worker> {
  const { binary, nats } = setup();
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    "claim-boot-order-omp-",
    cleanup
  );
  const state = path.join(root, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });
  // The daemon's golden registration answer (packages/daemon/internal/api), so a field the daemon
  // adds to it reaches this stand-in's `claims/register` too.
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );

  // The message the listener kept while no session held the role, and the connection it is handed
  // over on. The forwarded envelope keeps the role topic, so the session acknowledges it on the
  // request's reply inbox before it delivers it, which is the receipt a case waits for.
  const kept = `KEPT-${randomUUID().slice(0, 8)}`;
  const connection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  let receipt: Promise<unknown> | undefined;
  const handOver = (sid: string): void => {
    const key = `kept-${randomUUID()}`;
    const minted = createHash("sha256").update(key).digest("hex").slice(0, 32);
    receipt = connection.request(
      `notifications.agent.${sid}`,
      codec.encode(
        JSON.stringify({
          event_id: `evt-${key}`,
          source: "envoy",
          source_event_id: `source-${key}`,
          topic: `${ROLE_TOPIC_PREFIX}${CLAIM_TOKEN}`,
          dedupe_key: `envoy.role.forward.publish.${minted}`,
          issued_at: Date.now(),
          payload_summary: kept,
          trace_id: `trace-${key}`,
        })
      ),
      { timeout: 10_000 }
    );
    // A case that never waits for the receipt (the hand-over is the proposition, not the ack)
    // leaves no rejection unhandled.
    receipt.catch(() => undefined);
  };

  const turns: Turn[] = [];
  let readyAt = 0;
  let claimAt = 0;
  let roleHolder = "";
  let grants = 0;
  const { base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        reply: (text) => answer.resolve(messageStream([{ type: "text", text }], `msg_${index}`)),
        replyBlocks: (blocks) => answer.resolve(messageStream(blocks, `msg_${index}`)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: CLAIM_TOKEN,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: "claim-boot-order-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") {
      // The boot's own, which the deferred claim's bound is armed after; a capability report
      // later in the session's life is a ready of its own and moves nothing.
      if (readyAt === 0) readyAt = Date.now();
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `claim-boot-order-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // An unavailable Dispatch arms no run-end self-check, so every Messages request is a turn.
    if (url.pathname.startsWith("/api/")) return new Response("unavailable", { status: 503 });
    // The Envoy listener, which serves `/v1/` and nothing else: the role claim records its
    // holder and hands the kept message over, the role read names the holder, and registration
    // and any other `/v1/` read answer with an interest. A path outside it is a route this
    // stand-in does not serve, and it is refused.
    if (!url.pathname.startsWith("/v1/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    const sid = typeof body.session_id === "string" ? body.session_id : "";
    if (url.pathname === "/v1/roles/set") {
      if (body.role === CLAIM_TOKEN && roleHolder === "") {
        claimAt = Date.now();
        // The next turn of the stand-in's event loop, so the grant goes out before the lane it
        // hands over, as the listener grants a claim and then forwards what it kept.
        setImmediate(() => handOver(sid));
      }
      roleHolder = sid;
    }
    if (url.pathname === `/v1/roles/${CLAIM_TOKEN}`) {
      return Response.json({ role: CLAIM_TOKEN, holder: roleHolder, last_seen: 1 });
    }
    return Response.json({
      session_id: sid === "" ? roleHolder : sid,
      machine_id: "claim-boot-order-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end is answered, so the server stops at once.
  cleanup.push(async () => {
    for (const turn of turns) turn.reply("Case over.");
  });
  await writeStandinProfile(home, base);

  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  const closed: { error?: unknown } = {};
  let runsEnded = 0;
  const spawnedAt = Date.now();
  const rpc = spawnRpc(
    binary,
    {
      extensions: WORKER_EXTENSIONS,
      home,
      workspace,
      sessions,
      env: {
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: ROLE,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_BOOT_TOKEN: "claim-boot-order-boot",
        LEGION_STATE_DIR: state,
        LEGION_WORKSPACE: workspace,
        LEGION_CLAIM_FIRST_TURN_BOUND_MS: String(options.boundMs),
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "response" && "id" in frame) {
          responses.get(String(frame.id))?.resolve(frame as Record<string, unknown>);
        }
        if (
          "type" in frame &&
          frame.type === "agent_end" &&
          !("isTerminal" in frame && frame.isTerminal === false)
        ) {
          runsEnded += 1;
        }
      },
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });

  const until = async <T>(
    what: string,
    probe: () => T | undefined | Promise<T | undefined>
  ): Promise<T> => {
    // The first wait spans Oh My Pi's startup, which no case can shorten.
    const deadline = Math.max(Date.now() + STEP_DEADLINE_MS, spawnedAt + STARTUP_DEADLINE_MS);
    for (;;) {
      if (closed.error !== undefined) throw closed.error;
      const value = await probe();
      if (value !== undefined) return value;
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
      // What the cases wait on — a request reaching the stand-in, a frame omp prints — has no
      // event of its own to await.
      await Bun.sleep(25);
    }
  };

  let taken = 0;
  let prompt: Record<string, unknown> | undefined;
  return {
    turns,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken]);
      taken += 1;
      return turn;
    },
    deliver(message) {
      const id = randomUUID();
      const waiting = Promise.withResolvers<Record<string, unknown>>();
      responses.set(id, waiting);
      void waiting.promise.then((response) => {
        prompt = response;
      });
      rpc.send({ type: "prompt", id, message });
    },
    refusal: () =>
      prompt === undefined || prompt.success === true ? undefined : JSON.stringify(prompt),
    kept,
    async handedOver(what) {
      const sent = await until(what, () => receipt);
      await sent;
    },
    claimedAfterReadyMs: () => claimAt - readyAt,
    runsEnded: () => runsEnded,
    async handOverOf(summary) {
      const files = (await readdir(sessions, { recursive: true })).filter((file) =>
        file.endsWith(".jsonl")
      );
      if (files.length !== 1) throw new Error(`want one transcript under ${sessions}: ${files}`);
      const written = (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
        .split("\n")
        .filter((line) => line.includes('"envoy-message"') && line.includes(summary));
      if (written.length !== 1) throw new Error(`want one card carrying ${summary}: ${written}`);
      return CardEntrySchema.parse(JSON.parse(written[0] ?? "")).details.handOver;
    },
  };
}

/** A Messages request's conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/**
 * The request of the running turn that carries `text`, the run kept going meanwhile: each
 * request before it is answered with a tool call the host runs to its end, so a message the
 * session took while a request was in flight reaches the model at one of the steps that follow.
 * An aside the host never put into this run reaches none of them, and the search fails.
 */
async function stepUntilRead(worker: Worker, text: string, steps: number): Promise<Turn> {
  for (let step = 0; step < steps; step += 1) {
    const turn = await worker.nextTurn(`the run's model request carrying ${text}`);
    if (conversation(turn.request).includes(text)) return turn;
    turn.replyBlocks([call("read", { path: "." })]);
  }
  throw new Error(`${text} reached no model request within ${steps} steps of the task's run`);
}

// The first state the issue's third check names: the stand-in daemon answers `claims/ready` and
// then sends the task, as the daemon does. The session's first turn is the task's, the task is
// not refused, and the message kept for the role reaches the session inside that turn, because
// the name is taken as that turn starts and the lane is handed over then. The case holds the
// task's model request until the session has acknowledged the kept message, then keeps the run
// going with tool calls: the aside the host took reaches the model at one of the run's next
// steps, with no run of this session ended, and the host's own record of the hand-over says the
// session took it with a run of its own active.
//
// At the commit before this change the boot claims the role before it calls ready, so the
// stand-in listener hands the kept message over inside Oh My Pi's startup, where it is a startup
// card whose wake starts the session's first turn. **The first assertion, that the session's
// first model request carries the daemon's task, is the one that fails there**: that request is
// the wake's, "1 Envoy message above arrived with no turn to read it". The daemon's prompt,
// written once ready answered and read only when the startup drain is over, is refused with Oh
// My Pi's own words — "Agent is already processing. Use steer() or followUp() to queue messages,
// or wait for completion." — which the second assertion would name had the first one held (both
// measured at 0c803aaa9 on 2026-11-09).
test.skipIf(skip)(
  "the daemon's task is the session's first turn, and the message kept for its role arrives inside that turn",
  async () => {
    // Past every wait of this case, so only the first turn can take the name.
    const worker = await startWorker({ boundMs: 120_000 });
    worker.deliver(TASK);

    const task = await worker.nextTurn("the task's model request");
    expect(conversation(task.request)).toContain(TASK);
    expect(worker.refusal()).toBeUndefined();

    await worker.handedOver("the session to take the message kept for its role");
    task.replyBlocks([call("read", { path: "." })]);
    const read = await stepUntilRead(worker, worker.kept, 3);
    // Inside the task's own turn: no run of this session has ended since it started, and the
    // host wrote the card as one taken while a run was active.
    expect(worker.runsEnded()).toBe(0);
    expect(await worker.handOverOf(worker.kept)).toBe("run-active");
    read.reply("Read the task and the message.");
  },
  CASE_TIMEOUT_MS
);

// The second state: the stand-in daemon sends no task at all, which is a role brought back with
// none. No turn starts of its own accord, so the claim boot takes the name at its bound, the
// stand-in listener hands the kept message over then, and that message starts the session's turn
// — a worker that is only brought back still hears what waited for it.
//
// At the commit before this change the name is taken during the boot, before `claims/ready`, so
// the first assertion — the claim reached the listener no sooner than the bound after the ready
// — fails there with a negative measurement (−4 ms at 0c803aaa9 on 2026-11-09).
test.skipIf(skip)(
  "a role brought back with no task takes its name at the bound, and the message kept for it starts its turn",
  async () => {
    const boundMs = 5_000;
    const worker = await startWorker({ boundMs });

    const woken = await worker.nextTurn("the turn the kept message started");
    expect(worker.claimedAfterReadyMs()).toBeGreaterThanOrEqual(boundMs);
    expect(conversation(woken.request)).toContain(worker.kept);
    // This session was sent nothing, so the turn it ran is the kept message's own.
    expect(worker.turns).toHaveLength(1);
    woken.reply("Read the message.");
  },
  CASE_TIMEOUT_MS
);
