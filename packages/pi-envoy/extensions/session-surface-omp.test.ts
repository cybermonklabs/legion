import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { realpathSync } from "node:fs";
import { readFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  deleteRelayCapturingStreams,
  isSummarization,
  messageStream,
  ompRoot,
  type Request,
  sendRelayFrame,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { connect, ErrorCode, headers, type NatsConnection, NatsError, StringCodec } from "nats";
import { z } from "zod";
import { SHELL_OUTPUT_MAX } from "../src/typed-shell";
import { NO_WINDOW_MODEL } from "../test/no-window-provider";

// What the live view's three session-side surfaces do on the real Oh My Pi (dispatch://INFRA-812,
// R3 and R4, and dispatch://INFRA-5271, R1): the `context` frame the session publishes about how
// full its context window is (extensions/envoy.ts's `refreshAgentStreamContext` over
// `@legion/pi-shared/agent-stream`'s `setContext`), with the `context-cleared` frame that takes it
// away once the session reports no window (dispatch://INFRA-4325); a person's `!command`, which
// the session runs itself before the host is handed the line (src/typed-shell.ts, read in
// `runTypedInput`); and the Envoy card the session displays, which the stream carries as the
// terminal shows it. All are read off the wire: the frames over a real NATS server, as the
// Dispatch relay reads them, and the reply over the stand-in Dispatch. What reached the model is
// read off the requests the binary sends the stand-in gateway, each held until the test answers it.
// LEGION_TEST_OMP names the binary, as in card-delivery-omp.test.ts, and LEGION_TEST_NATS_URL the
// NATS server. A run without both skips, except on GitHub Actions, where the pi-envoy job
// supplies both.
// This file runs in a Bun process of its own (the package's `test` script): envoy.test.ts's
// `mock.module("nats", …)` replaces `nats` for the whole process, and a file loaded after it gets
// the mock's `connect`, which the check below turns into one clear failure.
const omp = process.env.LEGION_TEST_OMP;
const natsUrl = process.env.LEGION_TEST_NATS_URL;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = (omp === undefined || natsUrl === undefined) && !onActions;
/** The Dispatch token the session is handed. */
const DISPATCH_TOKEN = fixtureSecret("dsp");

/** The binary and NATS server a case runs against; throws on GitHub Actions without them. */
function setup(): { readonly binary: string; readonly nats: string } {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  if (natsUrl === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
  return { binary: omp, nats: natsUrl };
}

/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 30_000;

// The real `connect`'s connection reports the server it reached (`getServer`); the mock's has no
// such method. Checked once, before any case spends a binary's startup on a connection that
// reaches no server. Bounded as a case's wait is, not by the runner's 5 s hook default: on a
// loaded machine the connection alone has taken longer.
beforeAll(async () => {
  if (skip) return;
  const probe = await connect({ servers: setup().nats });
  try {
    if (typeof probe.getServer !== "function") {
      throw new Error(
        "`nats` here is envoy.test.ts's mock: run this file in its own process with `bun run test`"
      );
    }
  } finally {
    await probe.close();
  }
}, DEADLINE_MS);

/** How often one session may put its context usage on the wire in these cases, the interval the
 *  check names (`ENVOY_CONTEXT_FRAME_INTERVAL_MS`). */
const CONTEXT_INTERVAL_MS = 2_000;

/** How long a `!command` runs in the shell session before it is stopped
 *  (`ENVOY_SHELL_COMMAND_LIMIT_MS`), the limit the check names. */
const SHELL_LIMIT_MS = 2_000;

/** The sessions these cases share are torn down once, at the end: each boot costs the real
 *  binary's startup, and every case of a session leaves it settled for the next one. Each
 *  teardown step is a real binary's stop, so the whole is bounded as a case's wait is. */
const cleanup: Cleanup = [];
afterAll(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
}, DEADLINE_MS);

function text(reply: string): Block {
  return { type: "text", text: reply };
}

/** Every message of a Messages request as JSON, in conversation order. */
function messagesOf(request: Request): string[] {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages.map((message) => JSON.stringify(message));
}

/** A Messages request's conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** One Messages request the stand-in holds until the test answers it. */
interface Turn {
  readonly request: Request;
  /** Its position among every Messages request the session sent, from 0. */
  readonly index: number;
  /** Answers it with `blocks`, reporting `inputTokens` of input usage. */
  reply(blocks: readonly Block[], inputTokens?: number): void;
}

/** The fields of `get_state`'s answer (the pin's `RpcSessionState`) the cases read. */
const RpcStateSchema = z.object({
  isSettled: z.boolean(),
  sessionFile: z.string().optional(),
  model: z.object({ provider: z.string(), id: z.string(), contextWindow: z.number() }).optional(),
});

/** One frame the session published on its stream, and when this test read it. */
interface SeenFrame {
  /** `Date.now()` as this test read it: the wall clock the session's publisher times its context
   *  interval on. */
  readonly clock: number;
  readonly frame: Record<string, unknown>;
}

/** What a person sent the session from Dispatch's Agents page: the message Dispatch stored, and
 *  the frame the listener hands the session. */
interface Sent {
  readonly messageID: string;
  readonly eventID: string;
}

/** A running session with pi-envoy, a real NATS connection to its subjects, and what it sent the
 *  stand-in. */
interface SurfaceSession {
  /** The directory the session runs in, which its `!command` runs in too. */
  readonly workspace: string;
  /** The HOME the binary runs with, which its `!command` runs with too. */
  readonly home: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** Every frame the session published on its agent stream, in arrival order. */
  readonly frames: readonly SeenFrame[];
  /** Every request the stand-in served, in arrival order. */
  readonly requests: readonly Request[];
  /** When the stand-in first served a request whose path starts with `prefix`, on the wall clock;
   *  undefined while none has arrived. */
  arrivedAt(prefix: string): number | undefined;
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** Relays `envelope` to the session as the listener relays its consumer, the frame naming the
   *  session's own inbox as the subject the stream stored it on, and resolves once the session
   *  has taken the frame. */
  relay(envelope: string, what: string): Promise<void>;
  /** Sends a person's direct message and resolves once the session has taken the frame: a Send or
   *  an Aside once Dispatch was asked to accept it, any other mode once the session answered or
   *  acted on it. */
  send(
    body: string,
    options?: {
      readonly mode?: "aside" | "btw" | "steer";
      readonly broadcastID?: string;
      readonly accepted?: boolean;
    }
  ): Promise<Sent>;
  /** The reply the session posted on Dispatch for `message`, once it is there. */
  reply(message: Sent, what: string): Promise<string>;
  /** Whether the session has posted a reply for `message` yet. */
  replied(message: Sent): boolean;
  /** Arms this session's publisher as the Dispatch relay does, and keeps it armed. */
  arm(): Promise<void>;
  /** What the session answers a replay request with, as the relay asks for it. */
  replay(): Promise<{ readonly frames: readonly Record<string, unknown>[] }>;
  /** Resolves once every frame the session published before this call is in `frames`. */
  flushed(): Promise<void>;
  /** Sends one RPC command and resolves with its response frame. */
  command(command: Record<string, unknown>): Promise<Record<string, unknown>>;
  /** Resolves once `get_state` reports the session settled. */
  settled(): Promise<void>;
  /** The session file's lines, as raw JSON. */
  entries(): Promise<string[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/** Polls `probe` until it returns a value, failing on `what` at the deadline or when omp exits. */
async function until<T>(
  what: string,
  probe: () => T | undefined | Promise<T | undefined>,
  closed: { readonly error?: unknown }
): Promise<T> {
  const deadline = Date.now() + DEADLINE_MS;
  for (;;) {
    if (closed.error !== undefined) throw closed.error;
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // What the cases wait on (a request reaching the stand-in, a frame, the host's state) has no
    // event the test can await, so it is polled, and a missing one fails at the deadline.
    await Bun.sleep(20);
  }
}

/**
 * Starts the real Oh My Pi in RPC mode with only envoy.ts loaded, against one stand-in that is the
 * model gateway, the Envoy listener and Dispatch, and the NATS server `setup` names. `config` lines
 * go into the profile's config.yml and `env` beside the plugin's own variables, and `prepare` is
 * handed the two directories before the binary starts, for a case whose point is a file the
 * binary must read at its launch. `noWindowModel` loads test/no-window-provider.ts beside
 * envoy.ts and enables its provider, so `set_model` can switch the session to `NO_WINDOW_MODEL`,
 * whose context window is zero. Resolves once the session has registered its id with the
 * listener, the test holds a NATS connection, and this test's subscription to the session's
 * stream is live.
 */
async function startSession(
  options: {
    readonly config?: readonly string[];
    readonly env?: Record<string, string>;
    readonly noWindowModel?: boolean;
    readonly prepare?: (paths: {
      readonly home: string;
      readonly workspace: string;
    }) => Promise<void>;
  } = {}
): Promise<SurfaceSession> {
  const { config = [], env = {}, noWindowModel = false, prepare } = options;
  const { binary, nats } = setup();
  const { home, workspace, sessions } = await ompRoot(binary, "session-surface-omp-", cleanup);
  await prepare?.({ home, workspace });

  const turns: Turn[] = [];
  /** The body and mode each published message id carries, which the accept answers with. */
  const stored = new Map<string, { readonly body: string; readonly mode: "aside" | "steer" }>();
  /** When the stand-in first served each path, as `arrivedAt` reads it. */
  const arrivals = new Map<string, number>();
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (!arrivals.has(url.pathname)) arrivals.set(url.pathname, Date.now());
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        index,
        reply: (blocks, inputTokens) =>
          answer.resolve(messageStream(blocks, `msg_${index}`, inputTokens)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation, a side turn or a compaction's summary call.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    // The answers route, as a Dispatch answers it with nothing for the session.
    const answers = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
    if (answers !== null) {
      return Response.json({
        session_id: decodeURIComponent(answers[1] ?? ""),
        since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
        total: 0,
        items: [],
      });
    }
    const accept = url.pathname.match(/^\/api\/v1\/messages\/([^/]+)\/deliveries\/(\d+)\/accept$/);
    if (accept !== null) {
      const messageID = accept[1] as string;
      const message = stored.get(messageID);
      if (message === undefined) return Response.json({ code: "NOT_FOUND" }, { status: 404 });
      return Response.json({
        message_id: messageID,
        body: message.body,
        delivery: message.mode,
        accepted_as: "user_turn",
        attempt: Number(accept[2]),
      });
    }
    if (/^\/api\/v1\/messages\/[^/]+\/reply$/.test(url.pathname)) {
      return Response.json({}, { status: 201 });
    }
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "session-surface-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end (an aborted one, a command's own turn) is answered, so the
  // server stops at once.
  cleanup.push(async () => {
    for (const turn of turns) turn.reply([text("Case over.")]);
  });
  await writeStandinProfile(home, base, config, {
    registered: noWindowModel ? [NO_WINDOW_MODEL.provider] : [],
  });

  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  const closed: { error?: unknown } = {};
  const rpc = spawnRpc(
    binary,
    {
      extensions: [
        path.join(import.meta.dir, "envoy.ts"),
        ...(noWindowModel
          ? [path.join(import.meta.dir, "..", "test", "no-window-provider.ts")]
          : []),
      ],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
        DISPATCH_URL: base,
        DISPATCH_TOKEN,
        ...(noWindowModel && { NO_WINDOW_PROVIDER_BASE_URL: `${base}/anthropic` }),
        ...env,
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "response" && "id" in frame) {
          responses.get(String(frame.id))?.resolve(frame as Record<string, unknown>);
        }
      },
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });

  const sid = await until(
    "the session's registration with the listener",
    () => {
      const subscribe = requests.find((request) => request.path === "/v1/interests/subscribe");
      const id = subscribe?.body.session_id;
      return typeof id === "string" && id !== "" ? id : undefined;
    },
    closed
  );
  const connection: NatsConnection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  // No stream another suite left on this server may answer the frames
  // (`deleteRelayCapturingStreams`).
  await deleteRelayCapturingStreams(connection);
  const codec = StringCodec();
  const subject = `notifications.agent.${sid}`;
  const control = `agentstream.${sid}.control`;

  const framesSubject = `agentstream.${sid}.frames`;
  const frames: SeenFrame[] = [];
  /** The markers `flushed` put on the frames subject, which are this test's and no frame of the
   *  session's. */
  const markers = new Set<string>();
  const stream = connection.subscribe(framesSubject);
  void (async () => {
    for await (const message of stream) {
      const frame = JSON.parse(codec.decode(message.data));
      if (typeof frame.flushMarker === "string") {
        markers.add(frame.flushMarker);
        continue;
      }
      frames.push({ clock: Date.now(), frame });
    }
  })();

  /** The replay request the relay sends, which arms the publisher and answers what it holds. */
  const askReplay = async (timeout: number): Promise<Record<string, unknown> | undefined> => {
    const asked = JSON.stringify({ type: "replay", v: 1 });
    try {
      const answer = await connection.request(control, codec.encode(asked), { timeout });
      return JSON.parse(codec.decode(answer.data)) as Record<string, unknown>;
    } catch (error) {
      if (error instanceof NatsError && error.code === ErrorCode.NoResponders) return undefined;
      if (error instanceof NatsError && error.code === ErrorCode.Timeout) return undefined;
      throw error;
    }
  };

  // One stream for the whole case: every frame of it carries the sequence after the last.
  let streamSeq = 0;
  let taken = 0;
  const session: SurfaceSession = {
    home,
    workspace,
    turns,
    frames,
    requests,
    arrivedAt(prefix) {
      for (const [served, at] of arrivals) if (served.startsWith(prefix)) return at;
      return undefined;
    },
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken], closed);
      taken += 1;
      return turn;
    },
    async relay(envelope, what) {
      // The one path a session receives on: a frame of its own consumer's relay, carrying the
      // subject the stream stored the message on (INFRA-771). The session subscribes that
      // subject after it registers, so until it does the server reports no responder and the
      // frame goes again, as the one frame it is; the word it answers is what says it took it.
      streamSeq += 1;
      try {
        await sendRelayFrame({
          connection,
          newHeaders: headers,
          sessionID: sid,
          subject,
          payload: codec.encode(envelope),
          seq: streamSeq,
          withinMs: DEADLINE_MS,
          stopped: closed,
        });
      } catch (error) {
        throw new Error(`${what} was taken by no session: ${String(error)}`);
      }
    },
    async send(body, sendOptions = {}) {
      const { mode = "steer", broadcastID, accepted = mode !== "btw" } = sendOptions;
      const messageID = randomUUID();
      const label = `send-${messageID.slice(0, 8)}`;
      const eventID = `dispatch-${label}`;
      if (mode !== "btw") stored.set(messageID, { body, mode });
      const actor = { id: "alice", kind: "user" };
      const envelope = JSON.stringify({
        dedupe_key: label,
        event_id: eventID,
        issued_at: Date.now(),
        payload: JSON.stringify({
          event: {
            actor,
            issue_key: null,
            payload: {
              author: actor,
              body,
              ...(broadcastID === undefined ? {} : { broadcast_id: broadcastID }),
              created_at: "2026-11-07T00:00:00Z",
              deliveries: [],
              id: messageID,
              in_reply_to: null,
              issue_key: null,
              target: `session:${sid}`,
            },
            type: "message.created",
          },
          delivery: { attempt: 1, mode },
        }),
        payload_summary: body,
        source: "dispatch",
        source_event_id: "1",
        topic: subject,
        trace_id: label,
      });
      await session.relay(envelope, label);
      const sent = { eventID, messageID };
      if (accepted) {
        await until(
          `Dispatch's accept of ${label}`,
          () =>
            requests.some((request) =>
              request.path.startsWith(`/api/v1/messages/${messageID}/deliveries/`)
            )
              ? true
              : undefined,
          closed
        );
      }
      return sent;
    },
    async reply(message, what) {
      const body = await until(
        what,
        () =>
          requests.find((request) => request.path === `/api/v1/messages/${message.messageID}/reply`)
            ?.body,
        closed
      );
      return typeof body.body === "string" ? body.body : JSON.stringify(body);
    },
    replied(message) {
      return requests.some(
        (request) => request.path === `/api/v1/messages/${message.messageID}/reply`
      );
    },
    async arm() {
      // A replay arms the publisher for `AGENT_STREAM_WATCH_TTL_MS`, as the relay's first pair of
      // control messages does, and the first one answers before it arms, so a case reads its
      // frames from a later replay. The keep-alive is the relay's watch ping, well inside the
      // window, so no case loses the stream by running long.
      await until(
        "the session's agent stream to answer a replay",
        async () => ((await askReplay(1_000)) === undefined ? undefined : true),
        closed
      );
      const ping = setInterval(() => {
        connection.publish(control, codec.encode(JSON.stringify({ type: "watch", v: 1 })));
      }, 5_000);
      cleanup.push(async () => clearInterval(ping));
    },
    async replay() {
      const answer = await until(
        "the session's replay",
        async () => await askReplay(5_000),
        closed
      );
      const replayed = answer as { readonly frames?: readonly Record<string, unknown>[] };
      return { frames: replayed.frames ?? [] };
    },
    async flushed() {
      // The session publishes its frames and its replay answer on one connection, which the
      // server reads in order, so by the time the answer is here every earlier frame is queued
      // to this connection ahead of it. A marker this test publishes next is queued behind them
      // on the frames subscription, so once it is read, so is every one of them.
      await session.replay();
      const marker = randomUUID();
      connection.publish(framesSubject, codec.encode(JSON.stringify({ flushMarker: marker })));
      await until(
        "this test's marker behind the session's frames",
        () => (markers.has(marker) ? true : undefined),
        closed
      );
    },
    async command(command) {
      const id = randomUUID();
      const waiting = Promise.withResolvers<Record<string, unknown>>();
      responses.set(id, waiting);
      rpc.send({ ...command, id });
      const response = await Promise.race([waiting.promise, rpc.closed]);
      if (response.success !== true) {
        throw new Error(`omp refused ${String(command.type)}: ${JSON.stringify(response)}`);
      }
      return response;
    },
    async settled() {
      await until(
        "the session to settle",
        async () => {
          const response = await session.command({ type: "get_state" });
          return RpcStateSchema.parse(response.data).isSettled ? true : undefined;
        },
        closed
      );
    },
    async entries() {
      const response = await session.command({ type: "get_state" });
      const file = RpcStateSchema.parse(response.data).sessionFile;
      if (file === undefined) throw new Error("get_state named no session file");
      return (await readFile(file, "utf8")).split("\n");
    },
    until: (what, probe) => until(what, probe, closed),
  };
  return session;
}

/** One frame of the context slot as a case sees it: a reading's two figures, or the clearing
 *  frame, which carries none and so reads NaN for both (no figure a row expects of a reading
 *  holds of it); its kind; the publisher's sequence number on it; and when this case saw the
 *  frame. */
interface Reading {
  /** `SeenFrame.clock`: when this case saw the frame, on the wall clock. */
  readonly clock: number;
  readonly kind: "context" | "context-cleared";
  readonly seq: number;
  readonly tokens: number;
  readonly contextWindow: number;
}

/** The frames of the context slot among `seen`: each reading, and each clearing frame. */
function contextFrames(seen: readonly SeenFrame[]): Reading[] {
  return seen
    .filter((each) => each.frame.kind === "context" || each.frame.kind === "context-cleared")
    .map((each) => ({
      clock: each.clock,
      kind: each.frame.kind === "context" ? "context" : "context-cleared",
      contextWindow: Number(each.frame.contextWindow),
      seq: Number(each.frame.seq),
      tokens: Number(each.frame.tokens),
    }));
}

/** One `message` frame the session published. */
interface SeenMessage {
  readonly message: Record<string, unknown>;
}

/** The `message` frames among `seen`: what the session published about its conversation. */
function messageFrames(seen: readonly SeenFrame[]): SeenMessage[] {
  return seen
    .filter((each) => each.frame.kind === "message")
    .map((each) => ({ message: each.frame.message as Record<string, unknown> }));
}

/** The text every part of a stream message carries. */
function frameText(message: Record<string, unknown>): string {
  const parts = Array.isArray(message.parts) ? message.parts : [];
  return parts.map((part: { readonly text?: string }) => part.text ?? "").join("");
}

/**
 * The frames the session published for `message`, once at least one of them is here.
 *
 * The session publishes a `!command`'s frame and posts its Dispatch reply in the same run, but
 * the frame reaches this test over the NATS server and the reply over the stand-in, so the frame
 * can arrive after the reply: a row that reads the frames the moment the reply arrives is reading
 * a race. Measured in this pod, the `!exit 3` row read no frame at all once, and an earlier row's
 * frame once. The wait ends on this message's first frame, so a second frame the session should
 * not have published is still there for the row's own count to fail on.
 */
async function framesFor(
  session: SurfaceSession,
  message: Sent,
  what: string
): Promise<SeenMessage[]> {
  return await session.until(what, () => {
    const published = messageFrames(session.frames).filter(
      (each) => each.message.dispatchMessageId === message.messageID
    );
    return published.length > 0 ? published : undefined;
  });
}

/** The context session, booted once: the `ENVOY_CONTEXT_FRAME_INTERVAL_MS` the check names, and a
 *  compaction threshold its last case reaches with one reply's reported usage. */
let contextSessionPromise: Promise<SurfaceSession> | undefined;
async function contextSession(): Promise<SurfaceSession> {
  contextSessionPromise ??= (async () => {
    const session = await startSession({
      config: [
        "compaction:",
        "  thresholdTokens: 100000",
        "  asyncEnabled: false",
        "  autoContinue: false",
        "  keepRecentTokens: 1",
        "  methodOrder: [soft]",
        "  midTurnEnabled: false",
      ],
      env: { ENVOY_CONTEXT_FRAME_INTERVAL_MS: String(CONTEXT_INTERVAL_MS) },
    });
    await session.arm();
    return session;
  })();
  return contextSessionPromise;
}

/**
 * The newest frame of the context slot (a reading or the clearing frame) once the publisher owes
 * nothing and its interval has run out, so the next reading the session takes goes out at once.
 * The caller has seen the session take its last reading: it is settled, or it waits on a model
 * request the caller holds.
 *
 * Each step waits on the publisher's own schedule rather than on how long this runner takes.
 * Readings come only from the session's events, so every one was taken before this call, and
 * the timer a held reading arms falls due an interval after it at the latest. A reading the
 * publisher held and then found unchanged sends no frame, so that timer's run, which starts the
 * next interval, cannot be seen on the wire; it is waited for through the binary's event loop.
 * That loop runs a timer that is due before it reads the next input, though it may read input
 * already waiting first (measured on Bun), so the second of two `get_state` round trips is
 * answered only after the timer ran. The interval it started is up an interval after that.
 * Real timers, as the subject is the real binary's own timer.
 */
async function settledReading(session: SurfaceSession): Promise<Reading | undefined> {
  await Bun.sleep(CONTEXT_INTERVAL_MS);
  await session.command({ type: "get_state" });
  await session.command({ type: "get_state" });
  await Bun.sleep(CONTEXT_INTERVAL_MS);
  await session.flushed();
  return contextFrames(session.frames).at(-1);
}

// The figures the frame carries are the host's own (`getContextUsage`), and the window is the
// model's: the harness's models.yml gives the stand-in model a 200000-token window. One turn of a
// real conversation ends several messages - the person's own and the assistant's - and the
// publisher reads the figures at the turn's start and after each of them. The turn's start puts
// its reading on the wire at once and opens the interval; a message that ends inside it has its
// reading held rather than dropped, and the newest held reading goes out when the interval is up,
// as it stood at that message. Without that frame the ring would hold the turn's first figure for
// as long as the session then stayed idle, however much the reply added to it.
//
// How fast this runner gets through the turn decides only which interval the reply ends in, so no
// check below measures the turn. A reply that ends inside the interval the turn's frame opened is
// held to its end; one that ends inside the next interval, opened by the person's message's held
// reading going out, is held to that one's end; one that ends later still goes out at once. In
// each case the last frame comes after the turn's last message and carries the figure the reply
// left, and no frame goes out within an interval of the one before it. That lower bound is the
// only time checked, counted from before the turn starts on the wall clock the publisher times
// its interval on, so a slower runner can only widen it. envoy.test.ts's "the session's context
// frame" rows hold the interval with a host stand-in.
test.skipIf(skip)(
  "a turn publishes its frame as it starts and the figure after its reply once the interval is up",
  async () => {
    const session = await contextSession();
    // The turn must open the interval it is measured in. A session writes a message of its own
    // while it starts - a card, or the line it writes when Envoy inbound is unavailable - and the
    // reading that message is owed is published as the state the arming replay carries, which is
    // inside the interval this turn starts in. Waiting for the publisher to owe nothing leaves
    // the turn's own reading the first of the window.
    await settledReading(session);
    const from = session.frames.length;
    const prompted = Date.now();
    await session.command({ type: "prompt", message: "What is the first case?" });
    const turn = await session.nextTurn("the first model request");
    // Four hundred distinct words, hundreds of tokens on the host's estimate, against the handful
    // the person's message adds: a figure read before the reply ended is within a few tokens of
    // the turn's frame, and one read after it is hundreds above it. No word repeats, as the
    // host's loop guard cuts a reply that repeats itself short and asks the model again.
    const words = Array.from({ length: 400 }, (_, index) => (index * 7_919 + 1_000).toString(36));
    turn.reply([text(words.join(" "))]);
    await session.settled();
    // The reply's last frame and the context frame after it. The frames reach this test over
    // NATS and the settle over RPC, so the reply's frame can still be on its way here.
    const seen = await session.until("the frame the reply's own reading was owed", () => {
      const frames = session.frames.slice(from);
      const replied = frames.findIndex(
        (each) =>
          each.frame.kind === "message" &&
          (each.frame.message as Record<string, unknown>).role === "assistant" &&
          (each.frame.message as Record<string, unknown>).streaming === false
      );
      const after = frames.findLastIndex((each) => each.frame.kind === "context");
      return replied !== -1 && after > replied ? frames : undefined;
    });

    const published = contextFrames(seen);
    const reading = published[0] as Reading;
    const trailing = published.at(-1) as Reading;
    expect(reading.contextWindow).toBe(200_000);
    expect(reading.tokens).toBeGreaterThan(0);
    // In the order the session published them: the turn's frame went out when the turn started,
    // ahead of the person's message the host ends after that, and the reply's figure after the
    // turn's last message.
    const kinds = seen.map((each) => each.frame.kind);
    expect(kinds.indexOf("context")).toBeLessThan(kinds.indexOf("message"));
    expect(kinds.lastIndexOf("context")).toBeGreaterThan(kinds.lastIndexOf("message"));
    // The interval held each frame back. The turn starts after the prompt above was sent, its
    // frame opens the interval as it starts, and no frame goes out within an interval of the one
    // before it, so the k-th frame after the turn's reached this test no sooner than k intervals
    // after the prompt was sent. A publisher that sent each reading as it was taken would put the
    // reply's figure on the wire as soon as the reply ended.
    for (const [index, each] of published.entries()) {
      expect(each.clock - prompted).toBeGreaterThanOrEqual(index * CONTEXT_INTERVAL_MS);
    }
    // The figure the reply left, read when the reply ended rather than when the frame went out:
    // the host answers for the turn that is running, and a reading taken later, with the session
    // idle, is a figure of its own; a reading taken before the reply ended has none of it.
    expect(trailing.tokens).toBeGreaterThan(reading.tokens + 100);
    expect(trailing.tokens).toBeLessThan(200_000);
  },
  120_000
);

// The next message after the interval publishes the next frame, and the figures reach the viewer
// in the same breath as that message: the publisher reads them in the host's `message_end`, right
// after the message's own frame, and sends them there rather than holding them for a timer. The
// model request is held until the publisher owes nothing and its interval has run out, so the
// assistant's message is the next reading and goes out at once. "At once" is checked by order,
// not by time: the host settles the turn only after its message events, so the reading taken in
// the reply's `message_end` is the one the publisher keeps by the time the turn has settled,
// while a reading held for the interval would still be waiting on its timer then.
test.skipIf(skip)(
  "a message after the interval publishes its frame at once, before its turn settles",
  async () => {
    const session = await contextSession();
    await settledReading(session);

    const from = session.frames.length;
    await session.command({ type: "prompt", message: "What is the second case?" });
    const turn = await session.nextTurn("the second model request");
    // The turn has started and its user message is written: that is the interval this case opens.
    const atStart = await session.until("the turn's own context frame", () => {
      const published = contextFrames(session.frames.slice(from));
      return published.length > 0 ? published[0] : undefined;
    });
    await settledReading(session);
    const held = session.frames.length;
    turn.reply([text("The second case.")]);
    await session.settled();

    // The reading the publisher keeps once the turn has settled is the reply's own.
    const replayed = await session.replay();
    const kept = replayed.frames.findLast((frame) => frame.kind === "context");
    expect(Number(kept?.tokens)).toBeGreaterThan(atStart.tokens);
    // It went out on the wire, as the one frame since the reply and after the assistant's
    // message it was read at.
    await session.flushed();
    const seen = session.frames.slice(held);
    const published = contextFrames(seen);
    expect(published).toHaveLength(1);
    const frame = published[0] as Reading;
    expect(frame.seq).toBe(Number(kept?.seq));
    expect(frame.tokens).toBe(Number(kept?.tokens));
    const replied = seen.findIndex(
      (each) =>
        each.frame.kind === "message" &&
        (each.frame.message as Record<string, unknown>).role === "assistant" &&
        (each.frame.message as Record<string, unknown>).streaming === false
    );
    expect(replied).not.toBe(-1);
    expect(seen.findIndex((each) => each.frame.kind === "context")).toBeGreaterThan(replied);
  },
  120_000
);

// A view that opens or wakes draws its ring from the replay, so the newest figures ride in its
// tail beside the settings and the command list, in that order, with no new message since. The
// newest is the last frame on the wire once the publisher owes nothing (`settledReading`): the
// same frame, under the same sequence number.
test.skipIf(skip)(
  "a replay after the turn carries the context frame in its tail, beside the settings and the commands",
  async () => {
    const session = await contextSession();
    const live = await settledReading(session);
    if (live === undefined) throw new Error("no context frame was published before this case");
    const turnsBefore = session.turns.length;

    const replayed = await session.replay();
    expect(session.turns.length).toBe(turnsBefore);
    const tail = replayed.frames.slice(-3).map((frame) => frame.kind);
    expect(tail).toEqual(["settings", "context", "commands"]);
    const frame = replayed.frames.at(-2) as {
      readonly tokens: number;
      readonly contextWindow: number;
      readonly seq: number;
    };
    expect(frame.contextWindow).toBe(200_000);
    expect(frame.tokens).toBe(live.tokens);
    expect(frame.seq).toBe(live.seq);
  },
  120_000
);

// A compaction is the one moment the figures fall, and a ring left at the old reading would tell a
// person the session is still as full as it was. The publisher reads them after a compaction too,
// with no message of a turn to carry them: the reply below reports usage past the threshold, the
// host compacts, and the summary call is held past the publisher's interval so the frame that
// follows it is the compaction's own.
test.skipIf(skip)(
  "a compaction publishes a frame of its own, with no further message",
  async () => {
    const session = await contextSession();
    await settledReading(session);

    const opened = session.frames.length;
    await session.command({ type: "prompt", message: "What is the third case?" });
    const turn = await session.nextTurn("the third model request");
    turn.reply([text("The third case.")], 150_000);
    const summary = await session.nextTurn("the automatic compaction's summary request");
    expect(isSummarization(summary.request)).toBe(true);
    const atSummary = await session.until("the turn's own context frame", () =>
      contextFrames(session.frames.slice(opened)).at(-1)
    );
    // Past the trailing read the turn's own messages are owed too, so the next frame is the
    // compaction's own.
    const beforeCompaction = (await settledReading(session)) ?? atSummary;

    const from = session.frames.length;
    summary.reply([text("The conversation so far.")]);
    // A compaction can make more than one summary call (a split turn's prefix is summarised on
    // its own), and the frame comes once the compaction has ended, so each is answered as it
    // arrives.
    const after = await session.until("the compaction's context frame", async () => {
      for (const call of session.turns.slice(summary.index + 1)) {
        if (isSummarization(call.request)) call.reply([text("The conversation so far.")]);
      }
      const published = contextFrames(session.frames.slice(from));
      return published.length > 0 ? published[0] : undefined;
    });
    await session.settled();

    // Nothing of the conversation drove it: no message was written after the compaction's summary
    // call, no turn of the conversation ran, and the frame carries figures of its own.
    expect(messageFrames(session.frames.slice(from))).toEqual([]);
    expect(
      session.turns.slice(summary.index + 1).filter((call) => !isSummarization(call.request))
    ).toEqual([]);
    expect(after.contextWindow).toBe(200_000);
    expect(after.tokens).not.toBe(beforeCompaction.tokens);
  },
  120_000
);

// A switch to a model with no context window takes the ring away (dispatch://INFRA-4325): the
// next turn's start reads a window of zero (`getContextUsage`), and the publisher sends the
// clearing frame in the reading's place, so a viewer drawing the earlier reading drops it and a
// view that opens later draws none. The pin's models.yml refuses a window of zero, so the model is
// one test/no-window-provider.ts registers at runtime. The turn on it runs as any other: measured
// on the pinned binary, its request reaches the stand-in naming the model and the reply ends the
// turn. The row boots a session of its own, so the context session's rows keep their 200,000-token
// window.
test.skipIf(skip)(
  "a switch to a model with no context window publishes the clearing frame, and the replay keeps no reading",
  async () => {
    const session = await startSession({
      env: { ENVOY_CONTEXT_FRAME_INTERVAL_MS: String(CONTEXT_INTERVAL_MS) },
      noWindowModel: true,
    });
    await session.arm();

    const from = session.frames.length;
    await session.command({ type: "prompt", message: "What is the first case?" });
    (await session.nextTurn("the first model request")).reply([text("The first case.")]);
    await session.settled();
    await session.until("the turn's context reading", () =>
      contextFrames(session.frames.slice(from)).find((each) => each.kind === "context")
    );
    // Once the publisher owes nothing and its interval has run out, nothing the turn above was
    // owed follows the switch, and the next turn's start publishes at once.
    const reading = await settledReading(session);
    if (reading === undefined) throw new Error("no context frame was published by the first turn");
    expect(reading.kind).toBe("context");
    expect(reading.contextWindow).toBe(200_000);

    await session.command({ type: "set_model", ...NO_WINDOW_MODEL });
    const state = RpcStateSchema.parse((await session.command({ type: "get_state" })).data);
    expect(state.model).toEqual({
      provider: NO_WINDOW_MODEL.provider,
      id: NO_WINDOW_MODEL.modelId,
      contextWindow: 0,
    });

    const switched = session.frames.length;
    await session.command({ type: "prompt", message: "What is the second case?" });
    const turn = await session.nextTurn("the model request on the model with no window");
    expect(turn.request.body.model).toBe(NO_WINDOW_MODEL.modelId);
    turn.reply([text("The second case.")]);
    await session.settled();
    const cleared = await session.until("the clearing frame", () =>
      contextFrames(session.frames.slice(switched)).find((each) => each.kind === "context-cleared")
    );
    expect(cleared.seq).toBeGreaterThan(reading.seq);
    // Every later read of the turn answers the same window of zero, so the clearing frame is the
    // slot's one frame since the switch, and no reading follows it.
    await settledReading(session);
    expect(contextFrames(session.frames.slice(switched)).map((each) => each.kind)).toEqual([
      "context-cleared",
    ]);

    const replayed = await session.replay();
    const kinds = replayed.frames.map((frame) => frame.kind);
    expect(kinds.slice(-3)).toEqual(["settings", "context-cleared", "commands"]);
    expect(kinds).not.toContain("context");
    expect(replayed.frames.at(-2)?.seq).toBe(cleared.seq);
  },
  120_000
);

/** The shell session, booted once: the `ENVOY_SHELL_COMMAND_LIMIT_MS` the check names, and a
 *  viewer attached, since a `!command`'s own frame is the live view's copy of it. */
let shellSessionPromise: Promise<SurfaceSession> | undefined;
async function shellSession(): Promise<SurfaceSession> {
  shellSessionPromise ??= (async () => {
    const session = await startSession({
      env: { ENVOY_SHELL_COMMAND_LIMIT_MS: String(SHELL_LIMIT_MS) },
    });
    await session.arm();
    return session;
  })();
  return shellSessionPromise;
}

/** What the agent is told above a command and its output (`src/typed-shell.ts`). */
const SHELL_HEADING = "A person ran this shell command in this session from Dispatch.";

/**
 * Ends a `!command` case: the session has settled with no model request of its own since the
 * command, and a probe prompt is the next request, which carries what the model was told. Answers
 * the probe and waits for the session to settle again.
 */
async function probe(session: SurfaceSession): Promise<Request> {
  const expected = session.turns.length;
  await session.until("the session to settle with no turn of the command's own", async () => {
    const extra = session.turns[expected];
    if (extra !== undefined) {
      throw new Error(`a turn ran for the command, on ${conversation(extra.request)}`);
    }
    const response = await session.command({ type: "get_state" });
    return RpcStateSchema.parse(response.data).isSettled ? true : undefined;
  });
  const prompt = `PROBE-${randomUUID().slice(0, 8)}`;
  await session.command({ type: "prompt", message: prompt });
  const turn = await session.nextTurn("the probe's model request");
  expect(conversation(turn.request)).toContain(prompt);
  turn.reply([text("Probe answered.")]);
  await session.settled();
  return turn.request;
}

// The negative control: the same line without the mark is a prompt, and the session runs nothing.
// It is the shell session's first case, so no `!command` of an earlier case has put the heading
// below into this conversation and its absence is the line's own.
test.skipIf(skip)(
  "a Send of `printf hi` with no mark runs no command and reaches the model as a prompt",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("printf hi");
    const turn = await session.nextTurn("the turn the person's prompt started");
    expect(conversation(turn.request)).toContain("printf hi");
    turn.reply([text("Nothing was run.")]);
    await session.settled();

    expect(session.replied(sent)).toBe(false);
    const published = messageFrames(session.frames.slice(from)).map((each) =>
      frameText(each.message)
    );
    expect(published).toContain("printf hi");
    expect(published).not.toContain("printf hi\n\n```\nhi\n```");
    expect(conversation(turn.request)).not.toContain(SHELL_HEADING);
  },
  120_000
);

// A person's `!command` is the session's own to run: the host is never handed the line, the live
// view gets the person's message with what it printed, Dispatch gets the reply, and the model
// reads it at its next turn with no turn started for it.
test.skipIf(skip)(
  "a Send of `!printf hi` is run by the session, and all three answers carry it",
  async () => {
    const session = await shellSession();
    const turnsBefore = session.turns.length;

    const sent = await session.send("!printf hi");
    const reply = await session.reply(sent, "the reply to the person's command");
    expect(reply).toBe("!printf hi\n\n```\nhi\n```");

    // The live view: one user message, keyed on the Dispatch message it answers.
    const published = await framesFor(session, sent, "the frame of `!printf hi`");
    expect(published).toHaveLength(1);
    const message = published[0]?.message ?? {};
    expect(message.role).toBe("user");
    expect(frameText(message)).toBe("!printf hi\n\n```\nhi\n```");

    // No turn ran for it, and the model reads the command with the next turn.
    expect(session.turns.length).toBe(turnsBefore);
    const request = await probe(session);
    const messages = messagesOf(request);
    expect(messages.filter((each) => each.includes(SHELL_HEADING))).toHaveLength(1);
    expect(conversation(request)).toContain("!printf hi\\n\\n```\\nhi\\n```");
    // No prompt carries the person's line: the host was handed nothing for it, so the only copy
    // of the line in the conversation is the one inside the command's own report.
    expect(conversation(request).split("!printf hi").length - 1).toBe(1);

    // The ledger holds the message as taken, though the host wrote nothing for the line.
    const entries = await session.entries();
    expect(
      entries.filter(
        (entry) => entry.includes("envoy-replay-delivered") && entry.includes(sent.eventID)
      )
    ).toHaveLength(1);
    expect(
      entries.filter(
        (entry) =>
          entry.includes("envoy-dispatch-handled-attempt") && entry.includes(sent.messageID)
      )
    ).toHaveLength(1);
  },
  120_000
);

// The person's line is theirs as they sent it: the space after the mark is in every answer,
// though `printf hi` is what ran. The live view reads a report back against the message's
// stored body (`isShellCommandReport` in `@legion/contracts`), so a reply opening with the
// trimmed command is drawn as a second copy of the line (dispatch://INFRA-812, round 2).
test.skipIf(skip)(
  "a Send of `! printf hi` keeps the space after the mark in the reply and in the frame",
  async () => {
    const session = await shellSession();
    const sent = await session.send("! printf hi");
    const reply = await session.reply(sent, "the reply to `! printf hi`");
    expect(reply).toStartWith("! printf hi\n\n");
    expect(reply).toBe("! printf hi\n\n```\nhi\n```");
    const published = await framesFor(session, sent, "the frame of `! printf hi`");
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe("! printf hi\n\n```\nhi\n```");
  },
  120_000
);

// The command runs where the session runs: `SessionContext.cwd`, not the test's directory.
test.skipIf(skip)(
  "a Send of `!pwd` answers the session's own directory",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!pwd -P");
    expect(await session.reply(sent, "the reply to `!pwd -P`")).toBe(
      `!pwd -P\n\n\`\`\`\n${realpathSync(session.workspace)}\n\`\`\``
    );
  },
  120_000
);

// The command runs under the host's own shell configuration, which on the real session is read
// off the five variables the host adds over the environment it builds for a child shell.
test.skipIf(skip)(
  "a Send of a command reading the host's own variables answers with them set",
  async () => {
    const session = await shellSession();
    const command = `!printf '%s|%s|%s|%s' "$OMPCODE" "$CLAUDECODE" "$GIT_EDITOR" "$GPG_TTY"`;
    const sent = await session.send(command);
    expect(await session.reply(sent, "the reply to the command reading the host's variables")).toBe(
      `${command}\n\n\`\`\`\n1|1|true|not a tty\n\`\`\``
    );
  },
  120_000
);

// That environment is the host's own for a child shell, not the binary's whole process
// environment. The host drops the git location variables from it, because forwarding `GIT_DIR`
// makes a `git` the command runs ignore its `cwd` and write to the worktree or index that names,
// and it drops the values the launch directory's `.env` files loaded. This session is launched
// under a `GIT_DIR` and over a workspace holding an `.env`, and neither value reaches the
// command. That the binary did load that `.env` is read off the other name in it: the command
// limit this session stops a command at, where the default is two minutes.
test.skipIf(skip)(
  "a Send reading `GIT_DIR` and a launch `.env` value answers with neither set",
  async () => {
    const session = await startSession({
      env: { GIT_DIR: "/elsewhere/.git" },
      prepare: async ({ workspace }) => {
        await Bun.write(
          path.join(workspace, ".env"),
          `LEGION_SURFACE_DOTENV=from the launch directory\nENVOY_SHELL_COMMAND_LIMIT_MS=${SHELL_LIMIT_MS}\n`
        );
      },
    });
    const command = `!printf '%s|%s' "\${GIT_DIR-unset}" "\${LEGION_SURFACE_DOTENV-unset}"`;
    const sent = await session.send(command);
    expect(await session.reply(sent, "the reply to the command reading the two")).toBe(
      `${command}\n\n\`\`\`\nunset|unset\n\`\`\``
    );
    const slept = await session.send("!sleep 600");
    expect(await session.reply(slept, "the reply to `!sleep 600` under the `.env` limit")).toBe(
      `!sleep 600\n\nThe command printed nothing.\n\nThe command was stopped after ${SHELL_LIMIT_MS} ms.`
    );
  },
  120_000
);

// No login shell: a login shell reads the profile of the HOME it runs with, and this session's
// HOME holds one that prints a banner and runs a `cd` of its own, under each of the three names
// the shells this machine resolves read. The command answers with the session's own directory
// and with nothing but its own output, which is what R4 asks of it.
test.skipIf(skip)(
  "a Send of `!pwd -P` answers the session's directory, though its HOME holds a profile",
  async () => {
    const session = await startSession({
      prepare: async ({ home }) => {
        const profile = `printf 'a banner\\n'\ncd ${home}\n`;
        for (const name of [".profile", ".bash_profile", ".zprofile"]) {
          await Bun.write(path.join(home, name), profile);
        }
      },
    });
    const sent = await session.send("!pwd -P");
    expect(await session.reply(sent, "the reply to `!pwd -P` under a profile")).toBe(
      `!pwd -P\n\n\`\`\`\n${realpathSync(session.workspace)}\n\`\`\``
    );
  },
  120_000
);

// An Aside is the person's turn too, so it takes the same path and is answered the same way.
test.skipIf(skip)(
  "an Aside of `!printf hi` behaves as the Send does",
  async () => {
    const session = await shellSession();
    const turnsBefore = session.turns.length;

    const sent = await session.send("!printf aside", { mode: "aside" });
    expect(await session.reply(sent, "the reply to the person's Aside")).toBe(
      "!printf aside\n\n```\naside\n```"
    );
    const published = await framesFor(session, sent, "the frame of the Aside's command");
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe("!printf aside\n\n```\naside\n```");
    expect(session.turns.length).toBe(turnsBefore);
  },
  120_000
);

// Every `!` message is answered, one whose command printed nothing included: silence is an answer
// a person must be given, or the message looks lost.
test.skipIf(skip)(
  "a Send of `!true` is answered, saying there was no output",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!true");
    expect(await session.reply(sent, "the reply to `!true`")).toBe(
      "!true\n\nThe command printed nothing."
    );
  },
  120_000
);

// Dispatch cannot store U+0000 and refuses a whole reply holding one (400 NUL_CHARACTER), so a
// command that prints one would leave the person with no answer at all: it is dropped from what
// the three texts carry, as every control character but the line break and the tab is.
test.skipIf(skip)(
  "a Send of a command printing a NUL is answered with the NUL dropped",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!printf 'a\\0b'");
    expect(await session.reply(sent, "the reply to the command printing a NUL")).toBe(
      "!printf 'a\\0b'\n\n```\nab\n```"
    );
    const published = await framesFor(session, sent, "the frame of the command printing a NUL");
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe("!printf 'a\\0b'\n\n```\nab\n```");
  },
  120_000
);

test.skipIf(skip)(
  "a Send of `!exit 3` names the status in the frame and in the reply",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!exit 3");
    const answer =
      "!exit 3\n\nThe command printed nothing.\n\nThe command ended with exit status 3.";
    expect(await session.reply(sent, "the reply to `!exit 3`")).toBe(answer);
    const published = await framesFor(session, sent, "the frame of `!exit 3`");
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe(answer);
  },
  120_000
);

// A command that would run for ten minutes is stopped at the session's limit, and the answer says
// so rather than leaving the person waiting. The session runs the command once Dispatch answered
// its accept, so the reply reaching the stand-in no sooner than the limit after the accept did is
// the command running to the limit rather than being cut short; that it was stopped long before
// its ten minutes is the reply arriving at all, inside the deadline every wait in this file
// shares. How soon after the limit this runner answers is no part of the rule.
test.skipIf(skip)(
  "a Send of `!sleep 600` is stopped at the limit and says when",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!sleep 600");
    const reply = await session.reply(sent, "the reply to `!sleep 600`");
    expect(reply).toBe(
      `!sleep 600\n\nThe command printed nothing.\n\nThe command was stopped after ${SHELL_LIMIT_MS} ms.`
    );
    const accepted = session.arrivedAt(`/api/v1/messages/${sent.messageID}/deliveries/`);
    const replied = session.arrivedAt(`/api/v1/messages/${sent.messageID}/reply`);
    if (accepted === undefined || replied === undefined) {
      throw new Error("the stand-in served no accept or no reply for `!sleep 600`");
    }
    expect(replied - accepted).toBeGreaterThanOrEqual(SHELL_LIMIT_MS);
  },
  120_000
);

// One command runs for a session at a time: a second is answered naming the first rather than
// queued behind it.
test.skipIf(skip)(
  "a second `!` sent while one runs is answered naming the command still running",
  async () => {
    const session = await shellSession();
    const first = await session.send("!sleep 600");
    const second = await session.send("!printf hi");
    expect(await session.reply(second, "the reply to the second command")).toBe(
      "!printf hi\n\nThe command was not run: this session is still running !sleep 600."
    );
    expect(await session.reply(first, "the reply to the command still running")).toBe(
      `!sleep 600\n\nThe command printed nothing.\n\nThe command was stopped after ${SHELL_LIMIT_MS} ms.`
    );
  },
  120_000
);

// A command that prints without end must not fill the bus, the person's page or the model's
// context: all three copies are cut at the same bound and say so.
test.skipIf(skip)(
  "a Send whose command prints 20000 characters is cut at 8000 in the frame and the model's copy",
  async () => {
    const session = await shellSession();
    const command = "!printf 'a%.0s' $(seq 1 20000)";
    const sent = await session.send(command);
    const cut = `${command}\n\n\`\`\`\n${"a".repeat(SHELL_OUTPUT_MAX)}\n\`\`\`\n\nThe output was cut at ${SHELL_OUTPUT_MAX} characters.`;
    expect(await session.reply(sent, "the reply to the long command")).toBe(cut);
    const published = await framesFor(session, sent, "the frame of the long command");
    expect(published).toHaveLength(1);
    expect(frameText(published[0]?.message ?? {})).toBe(cut);

    const request = await probe(session);
    expect(conversation(request)).toContain("a".repeat(SHELL_OUTPUT_MAX));
    expect(conversation(request)).not.toContain("a".repeat(SHELL_OUTPUT_MAX + 1));
  },
  120_000
);

// The mark is one `!`, and the rest is the command whatever it starts with.
test.skipIf(skip)(
  "a Send of `!!printf hi` runs `!printf hi` and answers with that shell's failure",
  async () => {
    const session = await shellSession();
    const sent = await session.send("!!printf hi");
    const reply = await session.reply(sent, "the reply to `!!printf hi`");
    expect(reply).toStartWith("!!printf hi\n\n```\n");
    expect(reply).toContain("!printf");
    expect(reply).toEndWith("The command ended with exit status 127.");

    // Nothing is hidden from the model: it reads the same report.
    const request = await probe(session);
    expect(conversation(request)).toContain("The command ended with exit status 127.");
  },
  120_000
);

// A BTW is a side question the model answers, never a line the session runs.
test.skipIf(skip)(
  "a BTW runs no command",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("!printf hi", { mode: "btw" });
    const side = await session.nextTurn("the BTW's side turn");
    expect(conversation(side.request)).toContain("!printf hi");
    side.reply([text("The model's answer.")]);
    expect(await session.reply(sent, "the BTW's reply")).toBe("The model's answer.");
    expect(
      messageFrames(session.frames.slice(from)).map((each) => frameText(each.message))
    ).not.toContain("!printf hi\n\n```\nhi\n```");
    await session.settled();
  },
  120_000
);

// A broadcast is no person's turn at all: it keeps its card, and the session runs nothing.
test.skipIf(skip)(
  "a broadcast runs no command",
  async () => {
    const session = await shellSession();
    const from = session.frames.length;
    const sent = await session.send("!printf hi", {
      accepted: false,
      broadcastID: "66666666-6666-4666-8666-666666666666",
    });
    // The card starts a turn of its own, which carries the text the broadcast sent.
    const turn = await session.nextTurn("the turn the broadcast's card started");
    expect(conversation(turn.request)).toContain("!printf hi");
    turn.reply([text("Read the broadcast.")]);
    await session.settled();

    expect(session.replied(sent)).toBe(false);
    expect(
      messageFrames(session.frames.slice(from)).map((each) => frameText(each.message))
    ).not.toContain("!printf hi\n\n```\nhi\n```");
    expect(
      session.requests.some((request) =>
        request.path.startsWith(`/api/v1/messages/${sent.messageID}/deliveries/`)
      )
    ).toBe(false);
  },
  120_000
);

// A `!command` runs on its own promise, never inside the queued delivery task (D11): a command
// that runs for minutes must not hold the session's other Dispatch messages behind it. This
// session's limit is the default two minutes, and the command ends when the case says so, so the
// second message is answered with the first command demonstrably still running.
test.skipIf(skip)(
  "a `!sleep` holds none of the session's other Dispatch messages",
  async () => {
    const session = await startSession();
    const release = path.join(session.workspace, "release");
    const waiting = await session.send(
      `!while [ ! -f ${release} ]; do sleep 0.05; done; printf released`
    );

    const prompt = await session.send("Are you still taking messages?");
    const turn = await session.nextTurn("the turn the second message started");
    expect(conversation(turn.request)).toContain("Are you still taking messages?");
    // The first command is still running: its own answer is not on Dispatch yet.
    expect(session.replied(waiting)).toBe(false);
    turn.reply([text("Still taking them.")]);
    await session.settled();
    expect(session.replied(waiting)).toBe(false);
    expect(session.replied(prompt)).toBe(false);

    await Bun.write(release, "");
    expect(await session.reply(waiting, "the reply once the command ended")).toEndWith(
      "```\nreleased\n```"
    );
  },
  120_000
);

// dispatch://INFRA-5271's red check (`accept-5195-liveview-brief-red.sh`) on the real Oh My Pi:
// an agent a session launcher made gets its brief as a message its claimer publishes to the
// agent's role, which the listener stores and the session's consumer relays, and the session
// shows it as an Envoy card. The check greps the live view's replay for the brief's opening, up to
// its `;`, and before this the stream carried no card, so the brief was on the terminal and not on
// the page.
// The envelope is the listener's role forward of that publish (card-delivery-omp.test.ts's
// `forwardedRoleEnvelope`): source `envoy`, no sending session, no reply wanted.
test.skipIf(skip)(
  "the brief a role message brings is in the live view's replay, saying who sent it, before the reply it started",
  async () => {
    const session = await startSession();
    await session.arm();
    const brief = [
      "alice started you with the session launcher; this is their request:",
      "",
      "Work on the Dispatch issue ACME-1: https://dispatch.example/issues/ACME-1.",
    ].join("\n");
    const label = `brief-${randomUUID()}`;
    await session.relay(
      JSON.stringify({
        event_id: `evt-${label}`,
        source: "envoy",
        source_event_id: `source-${label}`,
        topic: "notifications.role.alice-acme-1",
        dedupe_key: `envoy.role.forward.publish.${randomUUID().replaceAll("-", "")}`,
        issued_at: Date.now(),
        payload: brief,
        payload_summary: `${brief.slice(0, 60)}…`,
        expects_reply: false,
        trace_id: `trace-${label}`,
      }),
      "the brief"
    );
    const opening = brief.split(";")[0] ?? brief;
    // The card starts a turn, which the stand-in holds until it is answered.
    const turn = await session.nextTurn("the turn the brief started");
    expect(conversation(turn.request)).toContain(opening);
    turn.reply([text("ready")]);
    await session.settled();

    const replayed = await session.replay();
    // What the red check greps the live view's stream for.
    expect(JSON.stringify(replayed)).toContain(opening);
    const messages = replayed.frames
      .filter((frame) => frame.kind === "message")
      .map((frame) => frame.message as Record<string, unknown>);
    // The session may have shown a card of its own while it started, so the brief's is found by
    // what it says, and then held to all of it.
    const card = messages.findIndex((message) => frameText(message).includes(brief));
    expect(String(messages[card]?.id)).toStartWith("c");
    expect(messages[card]).toMatchObject({ role: "user", streaming: false });
    expect(frameText(messages[card] ?? {})).toBe(`Envoy message from envoy:\n\n${brief}`);
    const reply = messages.findIndex((message) => frameText(message) === "ready");
    expect(reply).toBeGreaterThan(card);
  },
  120_000
);
