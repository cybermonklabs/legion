import { Database } from "bun:sqlite";
import { afterEach, expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import {
  type Block,
  type Cleanup,
  call,
  deliveryFor,
  isSummarization,
  messageStream,
  ompNatsGuard,
  ompRoot,
  type Request,
  type Rpc,
  STANDIN_STREAM_ID,
  sendRelayFrame,
  serveStandin,
  spawnInteractive,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { connect, createInbox, headers, type NatsConnection, StringCodec } from "nats";
import { z } from "zod";
import { cardLines, readCardLines } from "../test/card-entries";

// How an Envoy card and a person's Send reach the real Oh My Pi (extensions/envoy.ts's card send,
// and src/card-wake.ts's delivery choice and its wake for a card the host folds): both come over a
// real NATS server on the one path a session receives on, its own consumer's relay subject
// (`notifications.envoy.deliver.<session id>`, INFRA-771), each frame carrying the subject the
// stream stored the message on — a role lane the listener forwarded to the session's agent
// subject, and Dispatch's delivery frame, which the session accepts through the stand-in. What
// reached the model is read off the requests the binary sends the stand-in gateway, each held
// until the test answers it.
// LEGION_TEST_OMP names the binary, as in dispatch-first-omp.test.ts, and LEGION_TEST_NATS_URL the
// NATS server (the name the daemon's tests read, packages/daemon/internal/testnats/nats.go). A run
// without both skips, except on GitHub Actions, where the pi-envoy job supplies both
// (`ompNatsGuard`).
const { skip, setup } = ompNatsGuard();
/** The Dispatch token the session is handed. */
const DISPATCH_TOKEN = fixtureSecret("dsp");

/** The kind line of the wake (src/card-wake.ts, `CardWake`), whatever count it states. */
const WAKE_TEXT = /notice: unread messages/;

/** How long a case waits for one thing the session owes it before failing on it. */
const DEADLINE_MS = 30_000;

/** How long a case waits for Oh My Pi's startup, which loads the extensions from source. */
const STARTUP_DEADLINE_MS = 120_000;

/** How long a card read waits for the session file to end on a finished line. One line is one
 * write, so this is orders of magnitude more than it takes; it is a hang guard, not a sleep. */
const CARDS_WHOLE_MS = 5_000;

/** The one stream the stand-in listener answers a consumer registration with, and stamps every
 *  relay frame it publishes with: one stream for the run, so no frame reports a replaced one. */
const RELAY_STREAM = "card-delivery-stream";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A Messages request's conversation, as one string to find texts and their order in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** The text of a Messages request's last message: what the turn it answers was started on. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

function text(reply: string): Block {
  return { type: "text", text: reply };
}

/**
 * A role-lane card as the listener forwards it to the role holder's agent subject, which is the
 * subject its frame carries: the envelope keeps the role topic, and the session is not the one
 * the forward is answered to, since it counts as delivered once the stream stored it. Its key is
 * minted per label as envoy.test.ts's `forwardedRoleEnvelope` builds it.
 */
function forwardedRoleEnvelope(summary: string, label: string): string {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "envoy",
    source_event_id: `source-${label}`,
    topic: "notifications.role.card-delivery",
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: Date.now(),
    payload_summary: summary,
    trace_id: `trace-${label}`,
  });
}

/**
 * A notice or role message the daemon published, as the listener forwards it to the role holder's
 * agent subject: `agent` its source, as the listener writes for a publish that names none, no
 * sender session, and `urgency` when one is named (a daemon before INFRA-3686 names none).
 */
function daemonEnvelope(summary: string, label: string, urgency?: string): string {
  const minted = createHash("sha256").update(label).digest("hex").slice(0, 32);
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "agent",
    source_event_id: `source-${label}`,
    topic: "notifications.role.card-delivery",
    dedupe_key: `envoy.role.forward.publish.${minted}`,
    issued_at: Date.now(),
    payload_summary: summary,
    trace_id: `trace-${label}`,
    ...(urgency === undefined ? {} : { urgency }),
  });
}

/** Another session's message to session `sid` through Envoy, its envelope at `urgency`. */
function peerEnvelope(sid: string, summary: string, label: string, urgency: string): string {
  return JSON.stringify({
    event_id: `evt-${label}`,
    source: "agent",
    source_session: "ses_card_delivery_peer",
    source_event_id: `agent.ses_card_delivery_peer.${label}`,
    topic: `notifications.agent.${sid}`,
    dedupe_key: `agent.${sid}.${label}`,
    issued_at: Date.now(),
    payload_summary: summary,
    trace_id: `trace-${label}`,
    urgency,
  });
}

/** The session the Legion daemon writes to Dispatch as, the actor of every event it writes. */
const DAEMON_ACTOR = { kind: "session", id: "legion-daemon:CARD" };

/**
 * A Dispatch event on issue CARD-1 of `type`, by `actor`, carrying `payload`, as Dispatch's outbox
 * publishes it to a session that follows the issue, its envelope at `urgency`: no delivery, so it
 * is a card of the event and no message Dispatch delivers to the session.
 */
function dispatchEventEnvelope(
  type: string,
  actor: { readonly kind: string; readonly id: string },
  payload: Record<string, unknown>,
  summary: string,
  label: string,
  urgency: string
): string {
  return JSON.stringify({
    event_id: `dispatch-${label}`,
    source: "dispatch",
    source_event_id: "1",
    topic: `notifications.dispatch.issue.CARD-1.${type}`,
    dedupe_key: `dispatch-event-${label}`,
    issued_at: Date.now(),
    payload_summary: summary,
    payload: JSON.stringify({
      id: 1,
      issue_key: "CARD-1",
      seq: 1,
      type,
      actor,
      notify: false,
      quiet: false,
      created_at: new Date().toISOString(),
      payload,
    }),
    trace_id: `trace-${label}`,
    urgency,
  });
}

/** A comment the daemon wrote on CARD-1, whose body is `body`: the Dispatch event R5 sends `low`. */
function daemonCommentEnvelope(body: string, label: string, urgency: string): string {
  return dispatchEventEnvelope(
    "comment.created",
    DAEMON_ACTOR,
    {
      id: `comment-${label}`,
      issue_key: "CARD-1",
      author: DAEMON_ACTOR,
      body,
      reply_to: null,
      resolved: false,
      created_at: new Date().toISOString(),
    },
    body,
    label,
    urgency
  );
}

/** A person's answer to an ask on CARD-1 that the daemon opened, whose text is `answer`. */
function askAnsweredEnvelope(answer: string, label: string, urgency: string): string {
  const person = { kind: "user", id: "sami" };
  return dispatchEventEnvelope(
    "ask.answered",
    person,
    {
      id: `ask-${label}`,
      issue_key: "CARD-1",
      author: DAEMON_ACTOR,
      question: "Which way should the change go?",
      options: [{ label: "North" }, { label: "South" }],
      state: "answered",
      answer: { user: person, selected: ["North"], text: answer, at: new Date().toISOString() },
    },
    "Which way should the change go?",
    label,
    urgency
  );
}

/**
 * A person's direct Send to session `sid` from Dispatch's Agents page, as the listener hands the
 * session its delivery frame (envoy.test.ts's `directDispatchEnvelope`): no issue, the person as
 * the event's actor, attempt 1 in `mode`, its envelope at `urgency` when one is named.
 */
function directDispatchEnvelope(
  sid: string,
  messageID: string,
  body: string,
  label: string,
  mode: "btw" | "steer",
  urgency?: string
): string {
  const actor = { id: "alice", kind: "user" };
  return JSON.stringify({
    dedupe_key: `send-${label}`,
    event_id: `dispatch-send-${label}`,
    issued_at: Date.now(),
    payload: JSON.stringify({
      event: {
        actor,
        issue_key: null,
        payload: {
          author: actor,
          body,
          created_at: "2026-10-26T00:00:00Z",
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
    topic: `notifications.agent.${sid}`,
    trace_id: `send-${label}`,
    ...(urgency === undefined ? {} : { urgency }),
  });
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
  queuedMessages: z.object({ steering: z.array(z.string()) }),
});

/** A running session with pi-envoy, a real NATS connection to it, and what it sent the stand-in. */
interface CardSession {
  readonly root: string;
  /** The session's id, which its registration named. */
  readonly sid: string;
  /** Every Messages request so far, in arrival order. */
  readonly turns: readonly Turn[];
  /** Every RPC frame so far, in order. */
  readonly frames: readonly object[];
  /** The next Messages request after the ones already taken, once it arrives. */
  nextTurn(what: string): Promise<Turn>;
  /** Publishes a card and resolves once the session's subscription has taken its frame. */
  card(label: string): Promise<string>;
  /** Relays `envelope` as the listener relays a frame of the session's consumer, and resolves
   *  once the session's subscription has taken it. */
  relay(what: string, envelope: string): Promise<void>;
  /** Relays `envelope` as `relay` does, on a reply subject of its own, and resolves once the
   *  session has answered the frame `received`, with every word the session answers on it, the
   *  later ones added as they come (`done` once the card is in the session's store). */
  relayWatched(what: string, envelope: string): Promise<readonly string[]>;
  /** Publishes a person's Send, its envelope at `urgency` when named, and resolves once the
   *  session asked Dispatch to accept it. */
  send(label: string, urgency?: string): Promise<string>;
  /**
   * Relays one late BTW to the session as the listener relays a session's consumer, and resolves
   * with the body it carries once the session has answered the frame `done`: the card is written.
   */
  lateBtw(label: string): Promise<string>;
  /** Sends one RPC command and resolves with its response frame. */
  command(command: Record<string, unknown>): Promise<Record<string, unknown>>;
  state(): Promise<z.infer<typeof RpcStateSchema>>;
  /** Resolves once `get_state` reports the session settled. */
  settled(): Promise<void>;
  /** The `envoy-message` custom messages the session's store holds, as raw JSON lines: off the
   * session file, read once that file ends on a finished line, so an exact count of them is not
   * one short (`readCardLines`), or off the session's rows under SQL storage (`sqlSessionText`). */
  cardEntries(): Promise<string[]>;
  /** Polls `probe` until it returns a value, failing at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
  /** Answers the registration a session started with `holdStartup` holds; a no-op otherwise. */
  releaseStartup(): void;
  /** Resolves once the open-asks read a session started with `holdSetup` holds has reached the
   *  stand-in: the host is setting the prompt up, inside `before_agent_start`. */
  setupHeld(): Promise<void>;
  /** Answers the open-asks read a session started with `holdSetup` holds; a no-op otherwise. */
  releaseSetup(): void;
  /** Stops the process and starts it again on the same session (`--resume`), against the same
   *  stand-in and NATS connection, and resolves once it registered that session again. */
  restart(): Promise<void>;
  /** Types `text` at an interactive session's terminal and presses Enter once the composer holds
   *  it; an RPC session has no terminal. */
  type(text: string): Promise<void>;
}

/** Polls `probe` until it returns a value, failing on `what` at the deadline (`withinMs`,
 *  `DEADLINE_MS` unless named) or when omp exits. */
async function until<T>(
  what: string,
  probe: () => T | undefined | Promise<T | undefined>,
  closed: { readonly error?: unknown },
  withinMs = DEADLINE_MS
): Promise<T> {
  const deadline = Date.now() + withinMs;
  for (;;) {
    if (closed.error !== undefined) throw closed.error;
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    // What the cases wait on (a request reaching the stand-in, a file, the host's state) has no
    // event the test can await, so it is polled, and a missing one fails at the deadline.
    await Bun.sleep(20);
  }
}

/** Where Oh My Pi keeps a session: the session file (its default), or rows of a SQL database
 *  (`OMP_SESSION_STORAGE=sql`, which every Legion workflow session runs:
 *  packages/daemon/internal/podsafety/podsafety.go), where that file is never on disk. */
type SessionStorage = "file" | "sql";

/**
 * The text Oh My Pi's SQL store in the SQLite database `database` holds for the session it keys
 * by `key`, the path the host names as its session file; undefined while it holds none. It is read
 * as the store reads it (`sql-session-storage.ts` `readFull`, at the pinned build): the session
 * row's own content, then its parts in the order of their offsets. One statement reads both, so
 * the read sees one committed write, and a write is whole lines.
 */
function sqlSessionText(database: string, key: string): string | undefined {
  const store = new Database(database, { readonly: true });
  try {
    store.exec("PRAGMA busy_timeout = 5000");
    const rows = store
      .query<{ kind: number; start_offset: number; content: string }, [string]>(
        "SELECT 0 AS kind, 0 AS start_offset, content FROM omp_session_files WHERE path = ?1 " +
          "UNION ALL SELECT 1, start_offset, content FROM omp_session_files_parts WHERE path = ?1"
      )
      .all(key);
    const head = rows.find((row) => row.kind === 0);
    if (head === undefined) return undefined;
    const parts = rows
      .filter((row) => row.kind === 1)
      .sort((first, second) => first.start_offset - second.start_offset);
    return head.content + parts.map((part) => part.content).join("");
  } finally {
    store.close();
  }
}

/**
 * Starts the real Oh My Pi in RPC mode with only envoy.ts loaded, against one stand-in that is the
 * model gateway, the Envoy listener and Dispatch, and the NATS server `setup` names. `config` lines
 * go into the profile's config.yml. Resolves once the session has registered its id with the
 * listener and the test holds a NATS connection. With `holdStartup` the stand-in holds its answer
 * to that first registration until `releaseStartup`: envoy.ts's `session_start` awaits it
 * (`reconcileNow` registers once the relay subject is open), so the host is still inside its
 * startup, before it starts any extension send; on the pinned build it reads RPC input then and
 * answers a command that starts no turn, and holds one that starts a turn until startup is over
 * (`rpc-mode.ts` `waitsForStartup`). With `holdSetup` it holds
 * the first open-asks read until `releaseSetup`: envoy.ts's `before_agent_start` awaits it (for a
 * session its run-end nudge could reach, which this one is: a UI over RPC, a side turn, Dispatch
 * configured, neither a Legion pane nor a subagent), so the host is setting the first prompt up,
 * busy with no run, until the read is answered or `OPEN_ASKS_TIMEOUT_MS` (3 s) runs out.
 * `extensions` are loaded ahead of envoy.ts (a stand-in for another plugin's hooks), and `env` is
 * spread into the process's environment last. With `interactive` the session is the terminal
 * Oh My Pi under a pseudo-terminal (`spawnInteractive`), which reads no RPC command, rather than
 * RPC mode: it resolves once the session has registered and the terminal shows its editor.
 * `storage` is where Oh My Pi keeps the session: its file by default, or, with `sql`, rows of a
 * SQLite database in the case's root, which the host takes as it takes a Legion pod's database.
 */
async function startSession(
  options: {
    readonly config?: readonly string[];
    readonly holdStartup?: boolean;
    readonly holdSetup?: boolean;
    readonly extensions?: readonly string[];
    readonly env?: Readonly<Record<string, string>>;
    readonly interactive?: boolean;
    readonly storage?: SessionStorage;
  } = {}
): Promise<CardSession> {
  const {
    config = [],
    holdStartup = false,
    holdSetup = false,
    extensions = [],
    env = {},
    interactive = false,
    storage = "file",
  } = options;
  const { binary, nats } = setup();
  const { root, home, workspace, sessions } = await ompRoot(binary, "card-delivery-omp-", cleanup);
  const database = path.join(root, "sessions.db");
  const dsnFile = path.join(root, "sessions.dsn");
  if (storage === "sql") await writeFile(dsnFile, `sqlite://${database}`);

  let startupHold = holdStartup ? Promise.withResolvers<void>() : undefined;
  const releaseStartup = startupHold?.resolve ?? (() => undefined);
  let setupHold = holdSetup ? Promise.withResolvers<void>() : undefined;
  const releaseSetup = setupHold?.resolve ?? (() => undefined);
  const turns: Turn[] = [];
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
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
    // turn of the conversation or a compaction's summary call. A held setup holds the first read.
    if (url.pathname === "/api/v1/asks/open") {
      if (setupHold !== undefined) {
        const held = setupHold.promise;
        setupHold = undefined;
        await held;
      }
      return new Response("unavailable", { status: 503 });
    }
    // The answers route, as a Dispatch answers it with nothing for the session: every request
    // carries no list, so its last message is what started the turn.
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
      return Response.json({
        message_id: messageID,
        body: sends.get(messageID) ?? "",
        delivery: "steer",
        accepted_as: "user_turn",
        attempt: Number(accept[2]),
      });
    }
    if (/^\/api\/v1\/messages\/[^/]+\/deliveries\/\d+\/reply$/.test(url.pathname)) {
      return Response.json({}, { status: 201 });
    }
    // The Envoy listener: registration and any read answer with an interest. A held startup
    // holds the first registration.
    const id = typeof body.session_id === "string" ? body.session_id : "";
    const interest = { session_id: id, machine_id: "card-delivery-machine", dir: workspace };
    if (url.pathname === "/v1/interests/subscribe") {
      if (startupHold !== undefined) {
        const held = startupHold.promise;
        startupHold = undefined;
        await held;
      }
      // Every registration the listener accepts is answered the consumer it gave the session,
      // and one it can give none is refused (INFRA-771): a session receives only what its own
      // consumer holds, and the plugin fails a registration answered no delivery.
      return Response.json({
        ...interest,
        topics: [],
        ...deliveryFor(url.pathname, body, id),
      });
    }
    return Response.json({ ...interest, topics: [] });
  });
  // A request still held at the end (an aborted one, a held registration or open-asks read) is
  // answered, so the server stops at once.
  cleanup.push(async () => {
    releaseStartup();
    releaseSetup();
    for (const turn of turns) turn.reply([text("Case over.")]);
  });
  /** The body each published Send's message id carries, which the accept answers with. */
  const sends = new Map<string, string>();
  await writeStandinProfile(home, base, config);

  const frames: object[] = [];
  const responses = new Map<string, PromiseWithResolvers<Record<string, unknown>>>();
  // What ended the process the session runs in now, if anything did: a restart's own stop of the
  // process before it is no failure.
  const closed: { error?: unknown } = {};
  const launch = {
    extensions: [...extensions, path.join(import.meta.dir, "envoy.ts")],
    home,
    workspace,
    sessions,
    env: {
      ENVOY_URL: base,
      ENVOY_NATS_URL: nats,
      DISPATCH_URL: base,
      DISPATCH_TOKEN,
      ...(storage === "sql" && { OMP_SESSION_STORAGE: "sql", OMP_SESSION_SQL_DSN_FILE: dsnFile }),
      ...env,
    },
  };
  // Whether the pane may still be appending to its session file, which decides whether a read of
  // that file withholds a last line with no newline yet (`cardEntries`).
  let running = true;
  let rpc: Rpc | undefined;
  const spawn = (args: readonly string[]): void => {
    const started = spawnRpc(
      binary,
      {
        ...launch,
        args,
        onFrame: (frame) => {
          frames.push(frame);
          if ("type" in frame && frame.type === "response" && "id" in frame) {
            const waiting = responses.get(String(frame.id));
            waiting?.resolve(frame as Record<string, unknown>);
          }
        },
      },
      cleanup
    );
    rpc = started;
    running = true;
    started.closed.catch((error: unknown) => {
      if (rpc === started) closed.error = error;
    });
    void started.exited.then(() => {
      if (rpc === started) running = false;
    });
  };
  const terminal = interactive
    ? spawnInteractive(binary, { ...launch, args: [] }, cleanup)
    : undefined;
  if (terminal === undefined) spawn([]);
  else {
    void terminal.exited.then((code) => {
      closed.error = new Error(`omp exited (${code}); its screen:\n${terminal.screen()}`);
      running = false;
    });
  }

  /** The registrations the listener took that name a session, `id` alone when it is given. */
  const registrations = (id?: string): Request[] =>
    requests.filter(
      (request) =>
        request.path === "/v1/interests/subscribe" &&
        typeof request.body.session_id === "string" &&
        request.body.session_id !== "" &&
        (id === undefined || request.body.session_id === id)
    );
  // Oh My Pi's startup, its extensions' first load included, is the one wait that can outlast
  // `DEADLINE_MS` on a loaded host.
  const sid = await until(
    "the session's registration with the listener",
    () => registrations()[0]?.body.session_id as string | undefined,
    closed,
    STARTUP_DEADLINE_MS
  );
  if (terminal !== undefined) {
    // The profile's model on the status line under the editor: the terminal's startup is over.
    await until(
      "the terminal's editor",
      () => (terminal.screen().includes("Stand-in standin-model") ? true : undefined),
      closed,
      STARTUP_DEADLINE_MS
    );
  }
  const connection: NatsConnection = await connect({ servers: nats });
  cleanup.push(() => connection.close());
  const codec = StringCodec();
  // The subject the stream stored a message on, which every frame carries: the session's own
  // inbox, where the listener's role lane forwards a role card and Dispatch addresses a Send. A
  // session always follows it, so no frame of a case is dropped by the follow filter.
  const stored = `notifications.agent.${sid}`;

  // Sends one frame as the listener's relay sends it (`sendRelayFrame`), under the sequence
  // after the last: one stream for the whole case. A run whose binary died fails the wait at
  // once rather than at the frame's own deadline.
  let streamSeq = 0;
  const relayFrame = async (what: string, envelope: string): Promise<void> => {
    streamSeq += 1;
    try {
      await sendRelayFrame({
        connection,
        newHeaders: headers,
        sessionID: sid,
        subject: stored,
        payload: codec.encode(envelope),
        seq: streamSeq,
        withinMs: DEADLINE_MS,
        stopped: closed,
      });
    } catch (error) {
      throw new Error(`${what} failed: ${String(error)}`);
    }
  };

  const relayWatched = async (what: string, envelope: string): Promise<readonly string[]> => {
    streamSeq += 1;
    const inbox = createInbox();
    const answers = connection.subscribe(inbox);
    cleanup.push(async () => answers.unsubscribe());
    const words: string[] = [];
    void (async () => {
      for await (const word of answers) words.push(codec.decode(word.data));
    })();
    const frame = headers();
    frame.set(RelayHeader.subject, stored);
    frame.set(RelayHeader.streamSeq, String(streamSeq));
    frame.set(RelayHeader.streamID, STANDIN_STREAM_ID);
    frame.set(RelayHeader.delivered, "1");
    frame.set(RelayHeader.storedAt, new Date().toISOString());
    // Published once: the relay subject is subscribed before the session's first registration,
    // which `startSession` and `restart` wait for (envoy.ts's `subscribeRelay`).
    connection.publish(deliverSubject(sid), codec.encode(envelope), {
      headers: frame,
      reply: inbox,
    });
    await until(what, () => (words.length > 0 ? true : undefined), closed);
    if (words[0] !== "received") {
      throw new Error(`${what}: the session answered ${JSON.stringify(words)}, not received`);
    }
    return words;
  };

  let taken = 0;
  const session: CardSession = {
    root,
    sid,
    turns,
    frames,
    async nextTurn(what) {
      const turn = await until(what, () => turns[taken], closed);
      taken += 1;
      return turn;
    },
    async card(label) {
      const summary = `CARD-${label}-${randomUUID().slice(0, 8)}`;
      await relayFrame(
        `the session to take card ${label}`,
        forwardedRoleEnvelope(summary, `${label}-${randomUUID()}`)
      );
      return summary;
    },
    relay: relayFrame,
    relayWatched,
    async send(label, urgency) {
      const body = `SEND-${label}-${randomUUID().slice(0, 8)}`;
      const messageID = randomUUID();
      sends.set(messageID, body);
      await relayFrame(
        `the session to take Send ${label}`,
        directDispatchEnvelope(sid, messageID, body, label, "steer", urgency)
      );
      await until(
        `Dispatch's accept of Send ${label}`,
        () =>
          requests.some((request) => request.path.includes(`/messages/${messageID}/deliveries/`))
            ? true
            : undefined,
        closed
      );
      return body;
    },
    async lateBtw(label) {
      const body = `BTW-${label}-${randomUUID().slice(0, 8)}`;
      // The relay subject is subscribed before the registration that asks for consumer delivery
      // (envoy.ts's `subscribeRelay`, which confirms the subscription first), so a frame
      // published once such a registration has reached the stand-in has a subscriber.
      await until(
        "the registration that asks for consumer delivery",
        () =>
          requests.some(
            (request) =>
              request.path === "/v1/interests/subscribe" && request.body.delivery !== undefined
          )
            ? true
            : undefined,
        closed
      );
      const frame = headers();
      frame.set(RelayHeader.subject, stored);
      // The next sequence of the one stream this case publishes on, shared with relayFrame, so a
      // late frame never repeats or rewinds a sequence the session's ledger has already read: its
      // lateness is the delivered count below and nothing else.
      streamSeq += 1;
      frame.set(RelayHeader.streamSeq, String(streamSeq));
      frame.set(RelayHeader.streamID, RELAY_STREAM);
      // Delivered before: the frame is late (`ConsumerFrame.late`), which is what makes a BTW an
      // inert card rather than a side turn.
      frame.set(RelayHeader.delivered, "2");
      frame.set(RelayHeader.storedAt, new Date().toISOString());
      // The session answers the frame on its reply subject, one word at a time: `received` as it
      // decodes, then `done` once the card is written.
      const inbox = createInbox();
      const answers = connection.subscribe(inbox);
      const words: string[] = [];
      void (async () => {
        for await (const word of answers) words.push(codec.decode(word.data));
      })();
      connection.publish(
        deliverSubject(sid),
        codec.encode(directDispatchEnvelope(sid, randomUUID(), body, label, "btw")),
        { headers: frame, reply: inbox }
      );
      await until(
        `the session's done for the late BTW ${label}`,
        () => (words.includes("done") ? true : undefined),
        closed
      );
      answers.unsubscribe();
      return body;
    },
    async command(command) {
      const current = rpc;
      if (current === undefined) throw new Error("an interactive session reads no RPC command");
      const id = randomUUID();
      const waiting = Promise.withResolvers<Record<string, unknown>>();
      responses.set(id, waiting);
      current.send({ ...command, id });
      const response = await Promise.race([waiting.promise, current.closed]);
      if (response.success !== true) {
        throw new Error(`omp refused ${String(command.type)}: ${JSON.stringify(response)}`);
      }
      return response;
    },
    async state() {
      const response = await session.command({ type: "get_state" });
      return RpcStateSchema.parse(response.data);
    },
    async settled() {
      await until(
        "the session to settle",
        async () => ((await session.state()).isSettled ? true : undefined),
        closed
      );
    },
    async cardEntries() {
      // The pane's state is read before the file and never after: a read that caught half a
      // line while the pane then exited would be marked "gone" and parsed, and that parse error
      // would stand in for the real failure, which is that omp exited (dispatch://INFRA-2188).
      const writer = running ? "alive" : "gone";
      const file = (await session.state()).sessionFile;
      if (file === undefined) throw new Error("get_state named no session file");
      if (storage === "file") return readCardLines(file, writer, CARDS_WHOLE_MS);
      // The host keys the session's rows by the path it names; none before it stores the session.
      return cardLines(sqlSessionText(database, file) ?? "", writer);
    },
    until: (what, probe) => until(what, probe, closed),
    releaseStartup,
    async setupHeld() {
      await until(
        "the open-asks read the prompt's setup awaits",
        () => (requests.some((request) => request.path === "/api/v1/asks/open") ? true : undefined),
        closed
      );
    },
    releaseSetup,
    async restart() {
      const stopping = rpc;
      if (stopping === undefined) throw new Error("an interactive session is not restarted");
      const before = registrations(sid).length;
      // The stop is this case's own: the process it ends reports nothing as a failure.
      rpc = undefined;
      await stopping.end();
      spawn(["--resume", sid]);
      await until(
        "the restarted session's registration of the same session",
        () => (registrations(sid).length > before ? true : undefined),
        closed
      );
    },
    async type(typed) {
      if (terminal === undefined) throw new Error("an RPC session has no terminal");
      terminal.write(typed);
      // Enter only once the composer holds the text, so the keystrokes reach the editor as typing.
      await until(
        "the typed text in the composer",
        () => (terminal.screen().includes(typed) ? true : undefined),
        closed
      );
      terminal.write("\r");
    },
  };
  return session;
}

/**
 * Ends a negative check: the session has settled with no Messages request after the `expected`
 * ones, and a probe prompt is the next request, on that prompt. Answers the probe and waits for
 * the session to settle again. Returns the probe's request. A request past the `expected` ones
 * fails it at once, with the message it was started on, since a held request never settles.
 */
async function probe(session: CardSession, expected: number): Promise<Request> {
  await session.until("the session to settle", async () => {
    const extra = session.turns[expected];
    if (extra !== undefined) {
      throw new Error(
        `Messages request ${expected} came before the probe, on ${lastMessage(extra.request)}`
      );
    }
    return (await session.state()).isSettled ? true : undefined;
  });
  expect(session.turns.length).toBe(expected);
  const prompt = `PROBE-${randomUUID().slice(0, 8)}`;
  await session.command({ type: "prompt", message: prompt });
  const turn = await session.nextTurn("the probe's model request");
  expect(turn.index).toBe(expected);
  expect(lastMessage(turn.request)).toContain(prompt);
  turn.reply([text("Probe answered.")]);
  await session.settled();
  expect(session.turns.length).toBe(expected + 1);
  return turn.request;
}

/**
 * Resolves as `promise` does, or fails on `what` once `ms` have passed without it settling. A
 * real timer, as what it bounds is the real binary's answer time; a passing case never waits on it.
 */
async function within<T>(what: string, ms: number, promise: Promise<T>): Promise<T> {
  const late = Promise.withResolvers<never>();
  const timer = setTimeout(() => late.reject(new Error(`${what}: no answer within ${ms} ms`)), ms);
  try {
    return await Promise.race([promise, late.promise]);
  } finally {
    clearTimeout(timer);
  }
}

// A card queued while a turn runs used to be a steer, and the host drains its steering queue one
// message per step, first in first out: five cards ahead of a person's Send held the Send back
// five steps. As asides, the cards wait behind the Send, which reaches the model at the first step
// after it was sent.
test.skipIf(skip)(
  "a person's Send sent after five cards reaches the model at the next step, ahead of all five",
  async () => {
    const session = await startSession();
    await session.command({ type: "prompt", message: "Start case a." });
    const held = await session.nextTurn("the first model request");

    const cards: string[] = [];
    for (let n = 1; n <= 5; n += 1) cards.push(await session.card(`a${n}`));
    const send = await session.send("a");
    await session.until("the Send in the host's steering queue", async () =>
      (await session.state()).queuedMessages.steering.some((chip) => chip.includes(send))
        ? true
        : undefined
    );
    // A tool the host runs to its end: a `bash` call is moved to the background when a message
    // is queued, and its result starts a run of its own.
    held.reply([call("read", { path: "." })]);

    const next = await session.nextTurn("the model request after the held one");
    const firstWithSend = session.turns.find((turn) => conversation(turn.request).includes(send));
    expect(firstWithSend?.index).toBe(held.index + 1);
    const messages = conversation(next.request);
    for (const card of cards) {
      expect(messages).toContain(card);
      expect(messages.indexOf(send)).toBeLessThan(messages.indexOf(card));
    }
    next.reply([text("Read the Send and the cards.")]);

    // Every card was read in a run: none gets a wake.
    const request = await probe(session, next.index + 1);
    expect(conversation(request)).not.toMatch(WAKE_TEXT);
  },
  120_000
);

/** How an interrupt case holds the run when it interrupts it. */
type Hold = "model request" | "tool call";

/**
 * Interrupts a run with `abort` (the RPC `abort` a Legion daemon sends, or `abort_and_restore_queue`,
 * the TUI's Esc) while `hold` holds it and three cards are queued: the cards are folded into the
 * context with no turn, and the next prompt's request carries them.
 */
async function interruptKeepsCards(
  abort: "abort" | "abort_and_restore_queue",
  hold: Hold
): Promise<void> {
  const session = await startSession();
  await session.command({ type: "prompt", message: `Start case b, ${abort} during a ${hold}.` });
  const first = await session.nextTurn("the first model request");
  if (hold === "tool call") {
    // A real tool that runs until it is killed or released: the started marker says the shell
    // is in its loop, and the release at cleanup ends a shell an abort left behind.
    const started = path.join(session.root, "tool-started");
    const release = path.join(session.root, "tool-release");
    cleanup.push(() => writeFile(release, ""));
    first.reply([
      call("bash", {
        command: `touch '${started}'; while [ ! -e '${release}' ]; do sleep 0.1; done; echo released`,
      }),
    ]);
    await session.until("the blocking tool call to start", async () =>
      (await Bun.file(started).exists()) ? true : undefined
    );
  }
  const requestsBefore = session.turns.length;

  const cards = [await session.card("b1"), await session.card("b2"), await session.card("b3")];
  await session.command({ type: abort });

  // No turn after the interrupt: the cards are folded, and no wake follows them.
  const request = await probe(session, requestsBefore);
  const entries = await session.cardEntries();
  for (const card of cards) {
    expect(entries.filter((entry) => entry.includes(card))).toHaveLength(1);
    expect(conversation(request)).toContain(card);
  }
  expect(conversation(request)).not.toMatch(WAKE_TEXT);
}

for (const abort of ["abort", "abort_and_restore_queue"] as const) {
  for (const hold of ["model request", "tool call"] as const) {
    test.skipIf(skip)(
      `an RPC ${abort} during a held ${hold} keeps the three queued cards, folded with no turn`,
      () => interruptKeepsCards(abort, hold),
      120_000
    );
  }
}

// An interrupt while the host sets a prompt up leaves extensions no sign (src/card-wake.ts, the
// `setup` marker). The prompt marks the host busy, and its run's `agent_start` comes only after the
// `before_agent_start` handlers, envoy.ts's open-asks read among them, which the stand-in holds
// here. A card sent then is queued as an aside. RPC `abort` cancels the setup with no loop running,
// so no aborted assistant message comes, and abort's `finally` folds the card under the latch the
// abort set (agent-session.ts `#resumeStrandedIrcAsides`): a wake for it would start the turn the
// person stopped. The hold is released after the abort, inside the read's 3 s timeout, which would
// otherwise let the prompt run.
test.skipIf(skip)(
  "an interrupt while a prompt is set up keeps a card sent then folded, with no turn",
  async () => {
    const session = await startSession({ holdSetup: true });
    await session.command({ type: "prompt", message: "Start case e." });
    await session.setupHeld();
    const card = await session.card("e1");
    // The host is busy setting the prompt up, and no run has started.
    expect((await session.state()).isSettled).toBe(false);
    const agentStarted = (): boolean =>
      session.frames.some((frame) => "type" in frame && frame.type === "agent_start");
    expect(agentStarted()).toBe(false);
    await session.command({ type: "abort" });
    session.releaseSetup();

    // No turn after the interrupt: the card is folded, and no wake follows it.
    const request = await probe(session, 0);
    const entries = await session.cardEntries();
    expect(entries.filter((entry) => entry.includes(card))).toHaveLength(1);
    expect(conversation(request)).toContain(card);
    expect(conversation(request)).not.toMatch(WAKE_TEXT);
  },
  120_000
);

// An aside that reaches the host after the run's last step (here: during the run's automatic
// compaction), while an interrupt since the last user prompt is still in effect, which a
// card-started run does not end. Oh My Pi 18.12.2-sami.20261026 folds it into the context with no
// turn, and pi-envoy's wake gives it one; from v18.12.2-sami.20261027 the host starts that turn
// for the card's `triggerTurn` itself. Either way the card gets one turn.
test.skipIf(skip)(
  "a card that lands during a run's automatic compaction, in a session interrupted earlier, gets a turn",
  async () => {
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
    });
    await session.command({ type: "prompt", message: "Start case c." });
    await session.nextTurn("the first model request");
    // The interrupt the host keeps in effect until the next user prompt.
    await session.command({ type: "abort" });
    await session.settled();

    // An idle card gets a turn under the interrupt; its reply reports usage past the threshold.
    // The session got no card while it started, so this one is still in the startup phase
    // (src/card-wake.ts, `CardWake`): it goes into the context with no turn, and its wake
    // starts the turn, as its steer did before.
    const first = await session.card("c1");
    const cardTurn = await session.nextTurn("the turn the idle card's wake started");
    expect(conversation(cardTurn.request)).toContain(first);
    expect(lastMessage(cardTurn.request)).toMatch(WAKE_TEXT);
    cardTurn.reply([text("Read the first card.")], 150_000);

    const summary = await session.nextTurn("the automatic compaction's summary request");
    expect(isSummarization(summary.request)).toBe(true);
    const framesBefore = session.frames.length;
    const late = await session.card("c2");
    summary.reply([text("The conversation so far.")]);

    // A compaction can make more than one summary call (a split turn's prefix is summarised on
    // its own); each is answered as it comes, and the first other request is the card's turn.
    let woken = await session.nextTurn("the turn the folded card got");
    while (isSummarization(woken.request)) {
      woken.reply([text("The conversation so far.")]);
      woken = await session.nextTurn("the turn the folded card got");
    }
    expect(conversation(woken.request)).toContain(late);
    const startedBy = lastMessage(woken.request);
    expect(WAKE_TEXT.test(startedBy) || startedBy.includes(late)).toBe(true);
    // The card-started run ended with a continuation still owed: the late aside.
    const agentEndsSince = (from: number): object[] =>
      session.frames.slice(from).filter((frame) => "type" in frame && frame.type === "agent_end");
    const continuing = (frame: object): boolean =>
      "isTerminal" in frame && frame.isTerminal === false;
    expect(agentEndsSince(framesBefore).some(continuing)).toBe(true);
    const framesAtWake = session.frames.length;
    woken.reply([text("Read the late card.")]);
    await session.settled();
    expect(agentEndsSince(framesAtWake).some((frame) => !continuing(frame))).toBe(true);
    expect(session.turns.length).toBe(woken.index + 1);
  },
  120_000
);

// Plan mode is the host's other fold with no turn, and no case here reaches it: the pinned build's
// `/plan` is a TUI-only built-in (a `handleTui` and no `handle` in slash-commands/builtin-modes.ts),
// which RPC's builtin dispatch (`executeAcpBuiltinSlashCommand`) skips, so over RPC the text is an
// ordinary prompt and no RPC surface reaches plan mode. A plan-mode fold reaches pi-envoy as the
// same card `message_end` with no interrupt, so its wake runs the fold path the compaction case
// above proves, and src/card-wake.test.ts's fold cases cover the rules.

/** What of a persisted `envoy-message` entry the hand-over assertions read: the host keeps the
 *  card's `details` (src/card-wake.ts, `EnvoyCardMarker`) on the entry it writes to the file. */
const CardEntrySchema = z.object({
  type: z.literal("custom_message"),
  customType: z.literal("envoy-message"),
  details: z.object({ handOver: z.string() }),
});

// The first idle card of a session that got none while it started is still in the startup phase
// (src/card-wake.ts, `CardWake`): it goes into the context with no turn, and its wake
// starts the turn and ends the phase. An idle card after that starts its turn itself.
//
// The host state pi-envoy read at each hand-over (src/card-wake.ts, `CardHandOver`) survives the
// host's own persistence path: it is in the `details` of the `envoy-message` entry the host
// writes to the session file, which is all a later reading of the stored transcripts has. Both
// states asserted are the only one their host condition can produce. The first card is
// `startup`: `deliveryFor` reads the startup phase still open, the host idle and the mode `rpc`,
// which is that state and nothing else, and the wake's turn proves the phase held. That turn
// ends the phase, so the second card, handed once the host has settled again, is read as idle
// with no run and no prompt being set up: `idle`.
test.skipIf(skip)(
  "a card that reaches an idle session starts a turn, and its hand-over state reaches the file",
  async () => {
    const session = await startSession();
    const first = await session.card("d1");
    const woken = await session.nextTurn("the turn the first idle card's wake started");
    expect(conversation(woken.request)).toContain(first);
    expect(lastMessage(woken.request)).toMatch(WAKE_TEXT);
    woken.reply([text("Read the first card.")]);
    await session.settled();

    const card = await session.card("d2");
    const turn = await session.nextTurn("the turn the idle card started");
    expect(lastMessage(turn.request)).toContain(card);
    turn.reply([text("Read the card.")]);
    await probe(session, turn.index + 1);

    // Read off the file the host wrote, never the extension's own record of what it handed over.
    const entries = await session.cardEntries();
    const handOver = (label: string): string => {
      const written = entries.filter((entry) => entry.includes(label));
      expect(written).toHaveLength(1);
      return CardEntrySchema.parse(JSON.parse(written[0] as string)).details.handOver;
    };
    expect(handOver(first)).toBe("startup");
    expect(handOver(card)).toBe("idle");
  },
  120_000
);

// The third hand-over an Envoy card can be given (extensions/envoy.ts, `handCard`): the inert
// card, a steer with `triggerTurn: false`. A BTW is answered on Dispatch and never shown as a
// card that starts a turn, and one whose relay frame is late — delivered before, or stored before
// this connection's epoch began (`ConsumerFrame.late`) — can no longer be answered there: it is
// written to the transcript as one card and nothing more. That is the only path `handCard` hands
// an inert card on, and the state it stamps is `inert` whatever the host is doing, since
// `UnwrittenFrames` holds the card until the host is settled and hands it only then (`hostSettled`).
// What `lateBtw` publishes is a relay frame, so a late BTW reaches the session over its own
// consumer's relay subject, which the stand-in answers every registration with. The state is read
// off the file the host wrote, as the two cases above read theirs: an inert card the host
// persisted without `handOver` would be read, in a later reading of the stored transcripts, as a
// card written before any state was stamped at all.
test.skipIf(skip)(
  "a late BTW is written as an inert card, and its hand-over state reaches the file",
  async () => {
    const session = await startSession();
    // An inert card is handed only to a settled host; one sent before that waits for one.
    await session.settled();
    const btw = await session.lateBtw("i1");

    // The card started no turn of its own: the session's first model request is the probe's. A
    // session that ran no turn has written no file yet, so the probe is also what puts the card
    // the host holds on disk, as it is in every other case that reads the file.
    await probe(session, 0);

    const written = (await session.cardEntries()).filter((entry) => entry.includes(btw));
    expect(written).toHaveLength(1);
    expect(CardEntrySchema.parse(JSON.parse(written[0] as string)).details.handOver).toBe("inert");
  },
  120_000
);

// Several cards that reach an idle session together. pi-envoy reads `isIdle` when it hands each
// card, and the host checks `isStreaming` only after its own awaits, so the first card starts the
// run and the rest find the session streaming. A card handed as a steer then joins the host's one
// steering queue, which the agent loop releases one message per step: the rest of the cards would
// reach the model one step at a time. As asides, every one is taken at the run's next step. The
// contract checked: every card reaches the model by the run's second request, each once, in the
// order it was sent, each with the time the session got it, and the session file holds each once.
test.skipIf(skip)(
  "cards that reach an idle session together are all read by the second step, each once, in order",
  async () => {
    const session = await startSession();
    // The first idle card of a session that got none while it started is a startup card, which
    // its wake reads; the burst comes after the startup phase.
    const first = await session.card("f0");
    const woken = await session.nextTurn("the turn the first idle card's wake started");
    expect(conversation(woken.request)).toContain(first);
    woken.reply([text("Read the first card.")]);
    await session.settled();

    const labels = ["f1", "f2", "f3", "f4", "f5", "f6"];
    // Every request goes out before any receipt is awaited, in this order on one connection.
    const cards = await Promise.all(labels.map((label) => session.card(label)));
    const started = await session.nextTurn("the turn the burst started");
    // A tool the host runs to its end, so the run takes another step.
    started.reply([call("read", { path: "." })]);
    const next = await session.nextTurn("the run's second model request");

    const messages = conversation(next.request);
    let previous = -1;
    for (const card of cards) {
      expect(messages.split(card)).toHaveLength(2);
      expect(messages.indexOf(card)).toBeGreaterThan(previous);
      previous = messages.indexOf(card);
    }
    // Each card in the request also says when the session got it (src/card-age.ts).
    expect(messages.match(/\\n {2}delivered: /g)?.length ?? 0).toBeGreaterThanOrEqual(
      cards.length + 1
    );
    next.reply([text("Read the burst.")]);
    await probe(session, next.index + 1);
    const entries = await session.cardEntries();
    for (const card of cards) {
      expect(entries.filter((entry) => entry.includes(card))).toHaveLength(1);
    }
  },
  120_000
);

// A card that reaches pi-envoy while Oh My Pi starts holds none of the host's RPC input. The
// stand-in holds the registration envoy.ts's `session_start` awaits, with the relay subject
// already open (it is opened before the registration that asks for the consumer), so both cards
// reach pi-envoy inside the host's startup. The pinned build (18.18.6-sami.20261119-162325) reads
// RPC input while its startup runs (`rpc-mode.ts` `runRpcMode`): it answers at once a command
// that starts no turn, as `get_state`, holds one that starts a turn or changes the session
// (`waitsForStartup`) until its startup is over, and calls `initializeExtensions` with
// `drainSends: false`, so that startup waits on no extension send. So the `get_state` written
// once both cards are in is answered while the startup is still held. 18.16.0 held every RPC
// command but `set_ask_dialog`, in arrival order, until its startup was over, after
// `initializeExtensions` had returned, and this test then held that the `get_state` written during
// startup went unanswered until the startup was released. On 18.12.2, when this test was written,
// RPC mode also drained every startup send first (`runner.sends.drain()`), so a card's own turn
// there would have held `get_state` and `prompt` back until it ended.
test.skipIf(skip)(
  "a card that arrives while the session starts holds no RPC input",
  async () => {
    const session = await startSession({ holdStartup: true });
    const cards = [await session.card("s1"), await session.card("s2")];
    await within("get_state written while the startup is held", 5_000, session.state());
    // The cards wait for the startup: neither has started a turn while it is held.
    expect(session.turns).toHaveLength(0);
    session.releaseStartup();

    // The cards' turn is held at its model request, and the host still answers RPC commands.
    const woken = await session.nextTurn("the turn the startup cards' wake started");
    await within("get_state written while the turn is held", 5_000, session.state());

    const messages = conversation(woken.request);
    for (const card of cards) expect(messages).toContain(card);
    expect(lastMessage(woken.request)).toMatch(WAKE_TEXT);
    woken.reply([text("Read the startup cards.")]);
    await probe(session, woken.index + 1);
  },
  120_000
);

// A `low` card of the daemon or of a Dispatch event (src/card-wake.ts, `lowCard`) goes into the
// session's stored context with no turn: as `nextTurn` with no `triggerTurn`, which the host
// appends to the context and to the session's store at once, and only to a settled host, since a
// busy one keeps such a card in memory only (src/unwritten-frames.ts, the `stored` hand-over). The
// first turn after that reads it, whatever starts that turn, and its frame is answered `done` once
// the session's store holds it. Every other card is delivered as before. `a low card is read at the
// next turn` and `a low card stored on a branch with no reply` run on SQL storage, the storage of
// every Legion workflow session, since what they check hangs on how the store answers a stored
// card. The other cases run on file storage, which covers the file witness: the pi-envoy CI job
// has fifteen minutes for all of its steps (ci-job-pi-envoy.yaml), and these cases take about four.

/** How long a case watches a session that holds a `low` card for a turn that must not come. */
const QUIET_MS = 30_000;

/** The profile line that keeps a long `bash` call in the foreground: by default the host moves it
 *  to the background when a message is queued, and its result starts a run of its own. */
const FOREGROUND_BASH = ["bash:", "  autoBackground:", "    enabled: false"];

/** A fresh label, and the text of a card that carries it. */
function marked(prefix: string): { readonly label: string; readonly text: string } {
  const id = randomUUID();
  return { label: `${prefix}-${id}`, text: `${prefix.toUpperCase()}-${id.slice(0, 8)}` };
}

/** What of a persisted `low` card's entry the cases read: the host keeps its `details`. */
const StoredEntrySchema = z.object({
  type: z.literal("custom_message"),
  customType: z.literal("envoy-message"),
  details: z.object({ handOver: z.string(), urgency: z.string() }),
});

/**
 * Fails if the session makes a Messages request past the `expected` ones within `ms`, or omp
 * exits. A real clock, deliberately: a host that starts no turn emits nothing to await, and only
 * elapsed time shows the absence.
 */
async function noRequestFor(session: CardSession, ms: number, expected: number): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    // Throws at once when omp has exited.
    await session.until("omp to be running", () => true);
    const extra = session.turns[expected];
    if (extra !== undefined) {
      throw new Error(`Messages request ${expected} came, on ${lastMessage(extra.request)}`);
    }
    await Bun.sleep(200);
  }
  expect(session.turns.length).toBe(expected);
}

/** That `request`'s conversation holds `first`, and holds it before `second`. */
function expectBefore(request: Request, first: string, second: string): void {
  const messages = conversation(request);
  expect(messages).toContain(first);
  expect(messages).toContain(second);
  expect(messages.indexOf(first)).toBeLessThan(messages.indexOf(second));
}

/** Writes an extension of `source` to a scratch file the case removes, and returns its path. */
async function fixtureExtension(name: string, source: string): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "card-delivery-fixture-"));
  cleanup.push(() => rm(directory, { recursive: true, force: true }));
  const file = path.join(directory, name);
  await writeFile(file, source);
  return file;
}

/** Waits until the session's store holds the card that carries `card`, and returns that line. The
 *  host stores no session before the session's first assistant message, so a store that does not
 *  hold the session yet holds no card. */
async function storedLine(session: CardSession, card: string): Promise<string> {
  return session.until(`the card ${card} in the session's store`, async () => {
    const entries = await session.cardEntries().catch((error: unknown) => {
      if (error instanceof Error && "code" in error && error.code === "ENOENT") return [];
      throw error;
    });
    return entries.find((entry) => entry.includes(card));
  });
}

/** Waits until the session answers `done` to the frame of `card`, whose words are `words`: the
 *  card is in the session's store, so the stream's copy and the resume point below it go. */
async function answeredDone(
  session: CardSession,
  card: string,
  words: readonly string[]
): Promise<void> {
  await session
    .until(`the done of the card ${card}`, () => (words.includes("done") ? true : undefined))
    .catch((error: unknown) => {
      throw new Error(`${String(error)}; the session answered ${JSON.stringify(words)}`);
    });
}

test.skipIf(skip)(
  "a low card is read at the next turn",
  async () => {
    const session = await startSession({ storage: "sql" });
    await session.settled();

    // A daemon notice, sent low, to an idle session: no turn, and the task's turn reads it.
    const notice = marked("low-notice");
    const noticeWords = await session.relayWatched(
      "the session to take the low notice",
      daemonEnvelope(notice.text, notice.label, "low")
    );
    await noRequestFor(session, QUIET_MS, 0);
    // The session has had no reply, so its store holds nothing yet: the frame is not done.
    expect(noticeWords).not.toContain("done");
    const task = `TASK-${randomUUID().slice(0, 8)}`;
    await session.command({ type: "prompt", message: task });
    const first = await session.nextTurn("the task's first model request");
    expect(lastMessage(first.request)).toContain(task);
    expectBefore(first.request, notice.text, task);
    first.reply([text("Did the task.")]);
    await session.settled();
    // The task's reply put the session in its store, and the notice with it.
    await answeredDone(session, notice.text, noticeWords);

    // A Dispatch event the daemon wrote, sent low: no turn, and a med card's turn reads it.
    const comment = marked("low-comment");
    const commentWords = await session.relayWatched(
      "the session to take the low Dispatch event",
      daemonCommentEnvelope(comment.text, comment.label, "low")
    );
    await noRequestFor(session, 10_000, 1);
    // The store holds the session now: the stored card is done, and waits for no later card.
    await answeredDone(session, comment.text, commentWords);
    const card = marked("med-card");
    await session.relay("the session to take the med card", daemonEnvelope(card.text, card.label));
    const second = await session.nextTurn("the turn the med card started");
    expectBefore(second.request, comment.text, card.text);
    // The stored card reaches the model once: the med card's turn did not hand it again.
    expect(conversation(second.request).split(comment.text).length - 1).toBe(1);
    second.reply([text("Read the cards.")]);
    await probe(session, second.index + 1);

    for (const low of [notice.text, comment.text]) {
      const { details } = StoredEntrySchema.parse(JSON.parse(await storedLine(session, low)));
      expect(details).toEqual({ handOver: "inert", urgency: "low" });
    }
  },
  180_000
);

// A session's run takes a card handed while it streams at its next step, so a `low` card that
// comes then is held until the session is idle and stored then, and the turn after reads it.
test.skipIf(skip)(
  "a low card that comes during a turn is read at the turn after it",
  async () => {
    // A tool call of 20 seconds, during which a low card and then a med card come.
    const session = await startSession({ config: FOREGROUND_BASH });
    await session.command({ type: "prompt", message: "Start the long tool call." });
    const first = await session.nextTurn("the first model request");
    const started = path.join(session.root, "tool-started");
    first.reply([call("bash", { command: `touch '${started}'; sleep 20; echo slept` })]);
    await session.until("the 20 s tool call to start", async () =>
      (await Bun.file(started).exists()) ? true : undefined
    );
    const low = marked("low-during");
    const lowWords = await session.relayWatched(
      "the session to take the low card",
      daemonEnvelope(low.text, low.label, "low")
    );
    const med = marked("med-during");
    await session.relay(
      "the session to take the med card",
      daemonEnvelope(med.text, med.label, "med")
    );
    const next = await session.nextTurn("the run's model request after the tool call");
    expect(conversation(next.request)).toContain(med.text);
    expect(conversation(next.request)).not.toContain(low.text);
    next.reply([text("Read the med card.")]);
    // The run made no model call for the low card after its last one: the card is stored.
    await storedLine(session, low.text);
    await session.settled();
    await answeredDone(session, low.text, lowWords);
    await noRequestFor(session, 5_000, next.index + 1);
    const waking = marked("med-after");
    await session.relay(
      "the session to take the med card after the run",
      daemonEnvelope(waking.text, waking.label, "med")
    );
    const after = await session.nextTurn("the turn the med card started");
    expectBefore(after.request, low.text, waking.text);
    after.reply([text("Read both cards.")]);
    await session.settled();

    // A turn that ends with a continuation, which starts the next turn at once, as the phase
    // reminder's does: the low card that came during the turn waits through it, and is stored
    // once the session is idle.
    const reminder = `CONTINUE-${randomUUID().slice(0, 8)}`;
    const continuation = await fixtureExtension(
      "stop-continuation.ts",
      `export default function stopContinuation(pi) {
  let continued = false;
  pi.on("session_stop", async (event) => {
    if (continued || event.signal?.aborted) return undefined;
    continued = true;
    return { continue: true, additionalContext: process.env.CARD_DELIVERY_CONTINUATION };
  });
}
`
    );
    const continued = await startSession({
      extensions: [continuation],
      env: { CARD_DELIVERY_CONTINUATION: reminder },
    });
    await continued.command({ type: "prompt", message: "Start the continued turn." });
    const held = await continued.nextTurn("the first model request");
    const lowBeforeEnd = marked("low-continued");
    const lowBeforeEndWords = await continued.relayWatched(
      "the session to take the low card",
      daemonEnvelope(lowBeforeEnd.text, lowBeforeEnd.label, "low")
    );
    held.reply([text("Done for now.")]);
    const resumed = await continued.nextTurn("the continuation's model request");
    expect(conversation(resumed.request)).toContain(reminder);
    expect(conversation(resumed.request)).not.toContain(lowBeforeEnd.text);
    resumed.reply([text("Still done.")]);
    await storedLine(continued, lowBeforeEnd.text);
    await continued.settled();
    await answeredDone(continued, lowBeforeEnd.text, lowBeforeEndWords);
    const wakingAgain = marked("med-continued");
    await continued.relay(
      "the session to take the med card",
      daemonEnvelope(wakingAgain.text, wakingAgain.label, "med")
    );
    const later = await continued.nextTurn("the turn the med card started");
    expect(later.index).toBe(resumed.index + 1);
    expectBefore(later.request, lowBeforeEnd.text, wakingAgain.text);
    later.reply([text("Read both cards.")]);
    await continued.settled();
  },
  240_000
);

test.skipIf(skip)(
  "a low card survives a stop of the session",
  async () => {
    const session = await startSession();
    await session.command({ type: "prompt", message: "Start the turn before the stop." });
    const held = await session.nextTurn("the first model request");
    const low = marked("low-stop");
    const lowWords = await session.relayWatched(
      "the session to take the low card",
      daemonEnvelope(low.text, low.label, "low")
    );
    held.reply([text("Done before the stop.")]);
    // Once the turn is over the card is in the session's store, which is what a stop keeps: a
    // card handed while the turn ran would stay in Oh My Pi's memory and go with the process.
    await storedLine(session, low.text);
    await session.settled();
    // Answered done before the stop: the listener sends the frame to no later process.
    await answeredDone(session, low.text, lowWords);

    await session.restart();
    const card = marked("med-restart");
    await session.relay(
      "the restarted session to take the med card",
      daemonEnvelope(card.text, card.label, "med")
    );
    const turn = await session.nextTurn("the first model request after the restart");
    expectBefore(turn.request, low.text, card.text);
    turn.reply([text("Read both cards.")]);
    await session.settled();
  },
  180_000
);

test.skipIf(skip)(
  "a low card at startup is owed no wake",
  async () => {
    // No start-turn mark: the card comes while Oh My Pi starts, held inside its startup.
    const session = await startSession({ holdStartup: true });
    const low = marked("low-startup");
    const lowWords = await session.relayWatched(
      "the starting session to take the low card",
      daemonEnvelope(low.text, low.label, "low")
    );
    session.releaseStartup();
    await session.state();
    await noRequestFor(session, QUIET_MS, 0);
    // The session has had no reply, so its store holds nothing yet: the frame is not done.
    expect(lowWords).not.toContain("done");
    const card = marked("med-startup");
    await session.relay(
      "the session to take the med card",
      daemonEnvelope(card.text, card.label, "med")
    );
    const turn = await session.nextTurn("the turn the med card started");
    expectBefore(turn.request, low.text, card.text);
    turn.reply([text("Read both cards.")]);
    await session.settled();
    await answeredDone(session, low.text, lowWords);

    // A start-turn mark, as the Legion entry keeps it for its controller's start: unsent from
    // `session_start`, sent with the start prompt (its `input`), cleared at its `agent_start`.
    const startupTurn = path.resolve(import.meta.dir, "../../pi-shared/src/startup-turn.ts");
    const mark = await fixtureExtension(
      "start-turn-mark.ts",
      `import { clearStartupTurn, markStartupTurnPending, markStartupTurnSent } from ${JSON.stringify(startupTurn)};
export default function startTurnMark(pi) {
  let session = "";
  let sent = false;
  pi.on("session_start", async (_event, context) => {
    session = context.sessionManager.getSessionId();
    markStartupTurnPending(session);
  });
  pi.on("input", async () => {
    if (!sent) {
      sent = true;
      markStartupTurnSent(session);
    }
    return undefined;
  });
  pi.on("agent_start", async () => {
    clearStartupTurn(session);
  });
}
`
    );
    const withMark = await startSession({ holdStartup: true, extensions: [mark] });
    const lowMarked = marked("low-marked");
    const lowMarkedWords = await withMark.relayWatched(
      "the starting session to take the low card",
      daemonEnvelope(lowMarked.text, lowMarked.label, "low")
    );
    withMark.releaseStartup();
    await withMark.state();
    const start = `START-${randomUUID().slice(0, 8)}`;
    await withMark.command({ type: "prompt", message: start });
    const startTurn = await withMark.nextTurn("the start turn's model request");
    expect(lastMessage(startTurn.request)).toContain(start);
    startTurn.reply([text("Started.")]);
    await withMark.settled();
    await noRequestFor(withMark, QUIET_MS, startTurn.index + 1);
    await answeredDone(withMark, lowMarked.text, lowMarkedWords);
    const markedCard = marked("med-marked");
    await withMark.relay(
      "the session to take the med card",
      daemonEnvelope(markedCard.text, markedCard.label, "med")
    );
    const later = await withMark.nextTurn("the turn the med card started");
    expectBefore(later.request, lowMarked.text, markedCard.text);
    later.reply([text("Read both cards.")]);
    await withMark.settled();
  },
  300_000
);

test.skipIf(skip)(
  "a low card spares an interactive session",
  async () => {
    const session = await startSession({ interactive: true });
    const low = marked("low-terminal");
    const lowWords = await session.relayWatched(
      "the terminal to take the low card",
      daemonEnvelope(low.text, low.label, "low")
    );
    await noRequestFor(session, QUIET_MS, 0);
    // The session has had no reply, so its store holds nothing yet: the frame is not done.
    expect(lowWords).not.toContain("done");
    const typed = `TYPED-${randomUUID().slice(0, 8)}`;
    await session.type(typed);
    const turn = await session.nextTurn("the typed message's model request");
    expect(lastMessage(turn.request)).toContain(typed);
    expectBefore(turn.request, low.text, typed);
    turn.reply([text("Read the card and the message.")]);
    await answeredDone(session, low.text, lowWords);
  },
  180_000
);

// The cards that stay as they were: a daemon card of any other urgency or none, a card from
// another session, a message Dispatch delivers to the session, and an ask's card from Dispatch,
// each of them at `low` where it can say so. Each starts its own turn.
test.skipIf(skip)(
  "every other card starts a turn as today",
  async () => {
    const session = await startSession();
    await session.settled();
    const kinds: readonly (readonly [
      string,
      (card: { label: string; text: string }) => Promise<string>,
    ])[] = [
      ...(["med", "high", "blocking"] as const).map(
        (urgency) =>
          [
            `a daemon card at ${urgency}`,
            async (card: { label: string; text: string }) => {
              await session.relay(urgency, daemonEnvelope(card.text, card.label, urgency));
              return card.text;
            },
          ] as const
      ),
      [
        "a daemon card with no urgency",
        async (card) => {
          await session.relay("no urgency", daemonEnvelope(card.text, card.label));
          return card.text;
        },
      ],
      [
        "another session's card at low",
        async (card) => {
          await session.relay("a peer", peerEnvelope(session.sid, card.text, card.label, "low"));
          return card.text;
        },
      ],
      ["a message Dispatch delivers at low", (card) => session.send(card.label, "low")],
      [
        "an ask's card from Dispatch at low",
        async (card) => {
          await session.relay("an answer", askAnsweredEnvelope(card.text, card.label, "low"));
          return card.text;
        },
      ],
    ];
    for (const [index, [name, deliver]] of kinds.entries()) {
      const shown = await deliver(marked(`other-${index}`));
      const turn = await session.nextTurn(`the turn ${name} started`);
      expect(turn.index).toBe(index);
      expect(conversation(turn.request)).toContain(shown);
      turn.reply([text(`Read ${name}.`)]);
      await session.settled();
    }
    await probe(session, kinds.length);
  },
  240_000
);

/** The entries Oh My Pi's SQL store holds for the session `file` names, each line parsed. */
function storeEntries(database: string, file: string): Record<string, unknown>[] {
  return (sqlSessionText(database, file) ?? "")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
}

/** The role of a message entry, or undefined for any other entry. */
function entryRole(entry: Record<string, unknown>): unknown {
  const { message } = entry;
  return entry.type === "message" && typeof message === "object" && message !== null
    ? (message as Record<string, unknown>).role
    : undefined;
}

// Oh My Pi stores every entry of a session once any entry of it is an assistant message, on any
// branch of its tree, and appends a stored card at the leaf it has then. The stored-card check reads
// only the branch the session is on (`storedEntries`, `storeHoldsBranch`). A person's edit of the
// first prompt moves the leaf to before it, a branch with no reply, where a low card is stored, and
// a move back to the reply leaves that card on a branch the check never reads again: the store
// holds it, and its frame is never answered done for the life of the process, holding a place of
// the session consumer's ack-pending and its resume point (dispatch://INFRA-3686/spec R5).
test.skipIf(skip)(
  "a low card stored on a branch with no reply, then left for one with a reply, is answered done",
  async () => {
    const session = await startSession({ storage: "sql" });
    await session.command({ type: "prompt", message: "First task." });
    const first = await session.nextTurn("the first model request");
    first.reply([text("Done.")]);
    await session.settled();
    const file = (await session.state()).sessionFile;
    if (file === undefined) throw new Error("get_state named no session file");
    const database = path.join(session.root, "sessions.db");
    const entries = await session.until("the first reply in the store", () => {
      const held = storeEntries(database, file);
      return held.some((entry) => entryRole(entry) === "assistant") ? held : undefined;
    });
    const prompt = entries.find((entry) => entryRole(entry) === "user");
    const reply = entries.find((entry) => entryRole(entry) === "assistant");
    if (typeof prompt?.id !== "string" || typeof reply?.id !== "string") {
      throw new Error("the store holds no prompt and reply with ids");
    }
    // The person edits the first prompt: the leaf moves to before it.
    await session.command({ type: "navigate_tree", entryId: prompt.id });
    const low = marked("low-tree");
    const words = await session.relayWatched(
      "the session to take the low card",
      daemonEnvelope(low.text, low.label, "low")
    );
    await session.until("the low card in the store", () =>
      sqlSessionText(database, file)?.includes(low.text) ? true : undefined
    );
    await noRequestFor(session, 12_000, 1);
    // The person goes back to the first reply without sending the edit.
    await session.command({ type: "navigate_tree", entryId: reply.id });
    await noRequestFor(session, 12_000, 1);
    expect(words).toContain("done");
  },
  180_000
);
