// The harness shared by the tests that run the real Oh My Pi binary (each plugin's `*-omp.test.ts`):
// a stand-in server that is the model gateway and whatever else a test answers, a profile whose
// every model role is that stand-in, one `omp --mode rpc` child with only the caller's extensions
// loaded, one interactive `omp` under a pseudo-terminal whose screen a test reads as an operator
// sees it (`spawnInteractive`), and the Legion pane runner (`runLegionPane`) that puts those
// together as one Legion pane against a stand-in daemon and listener. The profile format, the
// flags and the RPC stream are the Oh My Pi pin's (the repository's .omp-pin), so a pin bump that
// changes one is fixed here once.
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { deliverSubject, type RelayReply } from "@legion/envoy-client/consumer-delivery";
import { RelayHeader } from "@legion/envoy-client/relay-frame";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { DeliveryAnswer } from "@legion/envoy-client/transport";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { loopbackHost } from "@legion/test-fixtures/loopback-host";
import { readStandinBody } from "@legion/test-fixtures/standin-body";
import { Terminal } from "@xterm/headless";
import { z } from "zod";
import { linkOmpNatives } from "./omp-natives";
import { publishTaskFrame, taskRelayConnection, taskStandIn } from "./task-stand-in";

export interface Request {
  readonly path: string;
  readonly body: Record<string, unknown>;
}

export type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> };

/**
 * One scripted reply: the blocks, or a function of the Messages request it answers, so a reply can
 * be built from the previous turn's tool result (`toolResultsIn(request).at(-1)`).
 */
export type Reply = readonly Block[] | ((request: Request) => readonly Block[]);

/** What a test undoes after each case, run last first. */
export type Cleanup = (() => Promise<void>)[];

/**
 * A scratch root with the profile's home, the session's workspace and its transcript directory.
 * The home's Oh My Pi natives are hardlinks to `binary`'s one cached copy (omp-natives.ts), so
 * a case writes none of them.
 */
export async function ompRoot(
  binary: string,
  prefix: string,
  cleanup: Cleanup
): Promise<{
  readonly home: string;
  readonly workspace: string;
  readonly sessions: string;
  readonly root: string;
}> {
  const root = await mkdtemp(path.join(os.tmpdir(), prefix));
  cleanup.push(() => rm(root, { recursive: true, force: true }));
  const home = path.join(root, "home");
  const workspace = path.join(root, "workspace");
  const sessions = path.join(root, "sessions");
  for (const directory of [path.join(home, ".omp", "agent"), workspace, sessions]) {
    await mkdir(directory, { recursive: true });
  }
  await linkOmpNatives(binary, home);
  return { root, home, workspace, sessions };
}

/**
 * Serves every request with `answer`, recording each first. `answer` gets the request's parsed JSON
 * body, empty for a GET, and the request itself, whose `signal` aborts when the client gives the
 * request up. Returns the recorded requests and the server's base URL.
 */
export function serveStandin(
  cleanup: Cleanup,
  answer: (
    url: URL,
    body: Record<string, unknown>,
    request: globalThis.Request
  ) => Response | Promise<Response>
): { readonly requests: Request[]; readonly base: string } {
  const requests: Request[] = [];
  const server = Bun.serve({
    port: 0,
    // An address of its own, out of reach of a client of another test that still dials this port
    // at 127.0.0.1 (`loopbackHost`); the clients' URLs are built from `base` below.
    hostname: loopbackHost(),
    async fetch(request) {
      const url = new URL(request.url);
      // A body that is no JSON object is refused by name, and is neither recorded nor handed to
      // `answer` (`readStandinBody`).
      const { body, refusal } = await readStandinBody(request);
      if (refusal !== undefined) return refusal;
      requests.push({ path: url.pathname, body });
      return answer(url, body, request);
    },
  });
  cleanup.push(async () => {
    await server.stop(true);
  });
  return { requests, base: server.url.origin };
}

/** The stream the stand-in listener's answers and relay frames name, one for every case here. */
export const STANDIN_STREAM_ID = "standin-stream";

/**
 * What a registration answer carries beyond the interest: the consumer, on
 * `/v1/interests/subscribe`, and nothing on any other listener route, which answers an interest
 * alone. The session is the one the request names, `fallbackSessionID` when it names none. The
 * answer itself is `@legion/envoy-client/test/subscribe-answer`'s, which every stand-in listener
 * in this repository builds its consumer answer with, so none of them drifts from what the
 * client accepts: a registration answered without one is refused in the client
 * (`parseSubscribeAnswer`), as the listener's own refusal is, and leaves the session deaf until
 * a later registration is accepted.
 */
export function deliveryFor(
  pathname: string,
  body: { readonly session_id?: unknown } | undefined,
  fallbackSessionID: string
): { readonly delivery?: DeliveryAnswer } {
  if (pathname !== "/v1/interests/subscribe") return {};
  const named = typeof body?.session_id === "string" ? body.session_id : fallbackSessionID;
  return {
    delivery: consumerDelivery(named === "" ? fallbackSessionID : named, {
      streamID: STANDIN_STREAM_ID,
    }),
  };
}

/**
 * What a relay frame needs of a NATS connection: one request carrying `Headers`, the header set
 * the frame's `newHeaders` makes. `nats`'s `NatsConnection` is one for `nats`'s `MsgHdrs`. Its
 * options, and their `headers`, are optional as `nats`'s own `request` declares them, which is
 * what lets that connection be one; `sendRelayFrame` always sends both.
 */
export interface RelayConnection<Headers extends RelayFrameHeaders = RelayFrameHeaders> {
  request(
    subject: string,
    payload: Uint8Array,
    options?: { readonly timeout: number; readonly headers?: Headers }
  ): Promise<{ readonly data: Uint8Array }>;
}

/** What a relay frame needs of a NATS header set; `nats`'s `headers()` makes one. */
export interface RelayFrameHeaders {
  set(name: string, value: string): void;
}

/** One frame of the stand-in stream, as `sendRelayFrame` sends it. */
export interface RelayFrame<Headers extends RelayFrameHeaders = RelayFrameHeaders> {
  /** Typed by the header set `newHeaders` makes, never inferred from the connection: `nats`'s
   * `request` takes its headers as optional, and `MsgHdrs | undefined` is no header set. */
  readonly connection: RelayConnection<NoInfer<Headers>>;
  /** Makes an empty header set; `nats`'s `headers` is one. */
  readonly newHeaders: () => Headers;
  /** The session the listener relays to, which the deliver subject names. */
  readonly sessionID: string;
  /** The subject the stream stored the message on, which the frame carries. */
  readonly subject: string;
  readonly payload: Uint8Array;
  /** Where the stream stored it; one case is one stream, so its frames are ordered by this. */
  readonly seq: number;
  /** When the stream stored it, RFC 3339; now by default. */
  readonly storedAt?: string;
  /** Which delivery of the frame this is; the first by default. */
  readonly delivered?: number;
  /** How long the frame is offered before this fails; 30 s by default. */
  readonly withinMs?: number;
  /** A run that ended: its error fails the wait at once, rather than at the deadline. */
  readonly stopped?: { readonly error?: unknown };
}

/**
 * What a session's first answer to a relay frame says about that frame: for each word the
 * protocol holds, whether the session took it, and what the word means there
 * (`createConsumerDelivery`, packages/envoy-client/src/consumer-delivery.ts). `received` is the
 * one word that is a take, since a session sends it as soon as the frame decodes and before it
 * decides what to do with it; every other word is a refusal, and a word this table does not hold
 * is a refusal too, so a word nobody has met yet is never read as a take by default.
 *
 * This is the one place those meanings are written. A reader that spelled them itself read a
 * `nak` as a take, then a second reader did, then this helper read a first `done` as one, so the
 * table and the one case per row that drives it are what make the next word a row rather than a
 * round.
 */
export const relayFirstAnswers = {
  received: { takes: true, means: "the frame decoded, so the session took it" },
  progress: {
    takes: false,
    means: "the session holds a frame unwritten, which no frame is answered with first",
  },
  done: {
    takes: false,
    means: "a frame the session could not decode and dropped, when it comes first",
  },
  nak: { takes: false, means: "the session cannot take the frame" },
} as const satisfies Readonly<
  Record<RelayReply, { readonly takes: boolean; readonly means: string }>
>;

/**
 * The words of that table a frame is taken by, derived from the table itself rather than written
 * again: a row added with `takes: true` is a take here the moment it is added, and a row the
 * producing client's `RelayReply` does not hold is a compile error. Today that is `received`
 * alone.
 */
export type RelayTake = {
  [Word in keyof typeof relayFirstAnswers]: (typeof relayFirstAnswers)[Word]["takes"] extends true
    ? Word
    : never;
}[keyof typeof relayFirstAnswers];

/** What the table says of `word`, which holds no row for a word outside the protocol. */
function relayAnswerMeaning(
  word: string
): { readonly takes: boolean; readonly means: string } | undefined {
  return (
    relayFirstAnswers as Readonly<
      Record<string, { readonly takes: boolean; readonly means: string }>
    >
  )[word];
}

/** Whether `word`, a session's first answer, is one of those, read from the table alone. */
function relayTakes(word: string): word is RelayTake {
  return relayAnswerMeaning(word)?.takes === true;
}

/** How long a send waits for a session to take its frame when the caller names no bound of its
 *  own: long enough for a real Oh My Pi to start and register, since most sends are made before
 *  the session subscribes its deliver subject. */
const RELAY_SEND_DEFAULT_MS = 30_000;

/**
 * Sends one frame as the listener's relay of a session's consumer sends it: the stored subject,
 * sequence, stream and time in its headers (`decodeRelayFrame`), on the session's deliver
 * subject. Sent as a request, whose answer is the session's own word: until that session has
 * subscribed the subject the server reports no responder, so the frame goes again, as the one
 * frame it is, and a copy of a frame the session already holds is answered and never shown twice
 * (`createConsumerDelivery`).
 *
 * Answers the word the session took the frame with, which is `RelayTake`, the take rows of
 * `relayFirstAnswers` derived from the table itself rather than named again here. What each word
 * a session can answer first means, and which of them is a take, is that table, and this reads
 * the answer through it: a caller that awaits a send would otherwise read a frame the session
 * dropped, or one it refused, as a frame it took.
 */
export async function sendRelayFrame<Headers extends RelayFrameHeaders>(
  frame: RelayFrame<Headers>
): Promise<RelayTake> {
  // Read once, so the refusal below names the bound this send actually kept.
  const within = frame.withinMs ?? RELAY_SEND_DEFAULT_MS;
  const deadline = Date.now() + within;
  for (;;) {
    const headers = frame.newHeaders();
    headers.set(RelayHeader.subject, frame.subject);
    headers.set(RelayHeader.streamSeq, String(frame.seq));
    headers.set(RelayHeader.streamID, STANDIN_STREAM_ID);
    headers.set(RelayHeader.delivered, String(frame.delivered ?? 1));
    headers.set(RelayHeader.storedAt, frame.storedAt ?? new Date().toISOString());
    let answered: string;
    try {
      const reply = await frame.connection.request(deliverSubject(frame.sessionID), frame.payload, {
        timeout: 1_000,
        headers,
      });
      answered = new TextDecoder().decode(reply.data);
    } catch (error) {
      if (frame.stopped?.error !== undefined) throw frame.stopped.error;
      if (Date.now() > deadline) {
        throw new Error(
          `${frame.sessionID} took no relay frame on ${frame.subject} within ` +
            `${within} ms: ${String(error)}`
        );
      }
      // A real clock, deliberately: a session that has not yet subscribed its deliver subject
      // emits no signal here — the server only answers the next send with no responder.
      await Bun.sleep(100);
      continue;
    }
    if (!relayTakes(answered)) {
      const means =
        relayAnswerMeaning(answered)?.means ?? "a word the relay protocol does not hold";
      throw new Error(
        `${frame.sessionID} answered ${JSON.stringify(answered)} on ${frame.subject}: ${means}, so it took no frame`
      );
    }
    return answered;
  }
}

/** One page of JetStream's `$JS.API.STREAM.NAMES` answer, or its error. */
const StreamNamesPage = z.object({
  total: z.number().optional(),
  streams: z.array(z.string()).nullable().optional(),
  error: z.object({ code: z.number(), description: z.string() }).optional(),
});

/** JetStream's `$JS.API.STREAM.DELETE.<name>` answer, or its error. */
const StreamDeleted = z.object({
  success: z.boolean().optional(),
  error: z
    .object({ code: z.number(), err_code: z.number().optional(), description: z.string() })
    .optional(),
});

/** JetStream's error code for a stream it does not hold (`JSStreamNotFoundErr`), which its
 * answer carries with code 404 and "stream not found". */
const STREAM_NOT_FOUND = 10059;

/** The streams on the server that capture a session's relay subject, by JetStream's own match. */
async function relayCapturingStreams(connection: RelayConnection): Promise<string[]> {
  const subject = deliverSubject("*");
  const names: string[] = [];
  for (;;) {
    let reply: { readonly data: Uint8Array };
    try {
      reply = await connection.request(
        "$JS.API.STREAM.NAMES",
        new TextEncoder().encode(JSON.stringify({ subject, offset: names.length })),
        { timeout: 5_000 }
      );
    } catch (error) {
      // nats.js names the server's no-responders status "503": a server without JetStream holds
      // no stream, so none captures the subject.
      if (names.length === 0 && error instanceof Error && "code" in error && error.code === "503") {
        return names;
      }
      throw new Error(`list the NATS server's streams on ${subject}: ${String(error)}`);
    }
    const page = StreamNamesPage.parse(JSON.parse(new TextDecoder().decode(reply.data)));
    if (page.error !== undefined) {
      throw new Error(`list the NATS server's streams on ${subject}: ${page.error.description}`);
    }
    const streams = page.streams ?? [];
    names.push(...streams);
    if (streams.length === 0 || names.length >= (page.total ?? 0)) return names;
  }
}

/**
 * Deletes every JetStream stream on the server `connection` reaches that captures a session's
 * relay subject (`deliverSubject`), and answers their names. Every case that sends frames with
 * `sendRelayFrame` calls it once it holds its connection, before its first frame. The
 * connection's `request` rejects a request nobody answers with an error whose `code` is "503", as
 * nats.js does: a server without JetStream holds no stream, and nothing is deleted there.
 *
 * A stream whose subjects take a relay subject answers each frame itself, with its publish ack
 * (`{"stream":"ENVOY_NOTIFICATIONS","seq":2}`), beside the session that holds the subject, and a
 * request takes whichever answer comes first: that ack reads as "a word the relay protocol does
 * not hold", and a frame to a subject nobody holds is answered where it should meet no responder.
 * Envoy's own stream never takes one (`DeliverSubjectPrefix`, packages/envoy/internal/bus/stream.go:
 * a relay frame travels over core NATS). The daemon's Go tests make ENVOY_NOTIFICATIONS on all of
 * `notifications.>`, and on a server both suites share (LEGION_TEST_NATS_URL in a pod with no
 * Docker) a stream a Go test leaves there stays for the Bun tests that run next. Deleting it is
 * what testnats.URL does to every stream on that server for each Go test; this deletes only the
 * streams that capture a relay subject, and fails naming any it could not.
 *
 * Two cases can call it at once (pi-legion's `bun test --parallel` runs files side by side), both
 * list the same stream, and the one whose delete comes second is answered "stream not found". A
 * stream that is gone is what that call wanted, so the answer counts as deleted; the list read
 * again after the deletes is what proves none is left, and it fails naming any stream it finds.
 */
export async function deleteRelayCapturingStreams(
  connection: RelayConnection
): Promise<readonly string[]> {
  const found = await relayCapturingStreams(connection);
  for (const name of found) {
    const reply = await connection.request(`$JS.API.STREAM.DELETE.${name}`, new Uint8Array(), {
      timeout: 5_000,
    });
    const deleted = StreamDeleted.parse(JSON.parse(new TextDecoder().decode(reply.data)));
    if (deleted.success === true || deleted.error?.err_code === STREAM_NOT_FOUND) continue;
    throw new Error(
      `delete the NATS server's stream ${name}, which captures a relay subject: ${deleted.error?.description ?? "no success in its answer"}`
    );
  }
  const left = await relayCapturingStreams(connection);
  if (left.length > 0) {
    throw new Error(
      `the NATS server still holds streams that capture a relay subject: ${left.join(", ")}`
    );
  }
  return found;
}

/** One provider the stand-in serves and the models it offers under it. Every provider is the same
 * stand-in at the same base URL, so a case that needs two models tells them apart by the `model`
 * of the Messages request it is answering. */
export interface StandinProvider {
  /** The provider key, as a selector's first half (`standin` in `standin/standin-model`). */
  readonly provider: string;
  /** The model ids, in the order the profile lists them; the first of the first provider is what
   * every model role names. */
  readonly models: readonly string[];
  /** The models also take images, which makes snapcompact a compaction method they can run; text
   * only by default. */
  readonly images?: boolean;
}

/** What `writeStandinProfile` writes when a caller names no providers: the one model every pane
 * that needs no chain runs on. */
export const DEFAULT_STANDIN_PROVIDERS: readonly StandinProvider[] = [
  { provider: "standin", models: ["standin-model"] },
];

/**
 * Writes the profile under `home`: the stand-in at `base` as its providers, each keyed by a
 * literal like a plain API-key caller, every model role on the first model of the first provider,
 * and every provider a devbox or runner could answer from without it disabled, so no turn reaches
 * a real model. `config` lines are appended to config.yml. The option `providers` names what the
 * case's own models are — a fallback chain needs a second model to step to — and defaults to the
 * single `standin/standin-model` every other case runs on. The option `registered` names the
 * providers a case's own extension registers at runtime (`pi.registerProvider`): models.yml
 * carries none of them, but `enabledModels` must still name each, or the session lists none of its
 * models and `set_model` answers "Model not found".
 */
export async function writeStandinProfile(
  home: string,
  base: string,
  config: readonly string[] = [],
  {
    providers = DEFAULT_STANDIN_PROVIDERS,
    registered = [],
  }: { providers?: readonly StandinProvider[]; registered?: readonly string[] } = {}
): Promise<void> {
  const agent = path.join(home, ".omp", "agent");
  const first = providers[0];
  const firstModel = first?.models[0];
  if (first === undefined || firstModel === undefined) {
    throw new Error("a stand-in profile needs at least one provider with one model");
  }
  // What every model role names: the first model of the first provider.
  const role = `${first.provider}/${firstModel}`;
  await writeFile(
    path.join(agent, "models.yml"),
    [
      "providers:",
      ...providers.flatMap(({ provider, models, images }) => [
        `  ${provider}:`,
        `    baseUrl: ${base}/anthropic`,
        "    auth: apiKey",
        "    api: anthropic-messages",
        "    apiKey: standin-key",
        "    models:",
        ...models.flatMap((id) => [
          `      - id: ${id}`,
          `        name: Stand-in ${id}`,
          "        reasoning: false",
          `        input: ${images === true ? "[text, image]" : "[text]"}`,
          "        contextWindow: 200000",
          "        maxTokens: 8000",
          "        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}",
        ]),
      ]),
      "",
    ].join("\n")
  );
  await writeFile(
    path.join(agent, "config.yml"),
    [
      "enabledModels:",
      ...providers.map(({ provider }) => `  - ${provider}/*`),
      ...registered.map((provider) => `  - ${provider}/*`),
      "disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]",
      "modelRoles:",
      ...["default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"].map(
        (name) => `  ${name}: ${role}`
      ),
      ...config,
      "",
    ].join("\n")
  );
}

export interface Rpc {
  /** Writes one RPC command to omp's stdin. */
  send(command: Record<string, unknown>): void;
  /** Closes omp's stdin and waits for it to exit. */
  end(): Promise<void>;
  /**
   * Rejects, naming omp's stderr, once omp closes its RPC stream, as it does when it exits. Race it
   * against a step's own settle; it carries a handler, so a case that has settled never sees it.
   */
  readonly closed: Promise<never>;
  /**
   * The status omp exited with, for a case whose proposition is the exit itself: the stall
   * watch's last step ends the worker with a status the daemon reads as a death
   * (`pi-legion/extensions/legion-stall-watchdog-omp.test.ts`). It settles only when omp has
   * exited, so a case that expects it alive races it against its own clock.
   */
  readonly exited: Promise<number>;
  /** The process id of the omp it started: the one process whose own steps a timer record a
   *  case hands it through the environment is read for. */
  readonly pid: number;
}

/**
 * Starts `omp --mode rpc` in `workspace` with only `extensions` loaded — absolute paths, since this
 * module lives in another package than its callers (pass `path.join(import.meta.dir, …)`) — no
 * skills, rules, LSP or title call, its transcripts under `sessions`, `home` as HOME and `bin`, when
 * given, first on PATH. `args` go on the command line after those flags, which is where a second
 * life of one pane names `--resume <session>`. `onFrame` gets each RPC frame omp prints, in order.
 * Both of omp's streams are read to the end, so a full pipe never blocks it, and the child is
 * killed at cleanup.
 */
export function spawnRpc(
  binary: string,
  options: {
    readonly extensions: readonly string[];
    readonly home: string;
    readonly workspace: string;
    readonly sessions: string;
    readonly bin?: string;
    readonly env: Record<string, string>;
    readonly args?: readonly string[];
    readonly onFrame: (frame: object) => void;
  },
  cleanup: Cleanup
): Rpc {
  const child = Bun.spawn(
    [binary, "--mode", "rpc", ...ompFlags(options), ...(options.args ?? [])],
    {
      cwd: options.workspace,
      env: { ...ompEnv(options), ...options.env },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  cleanup.push(async () => {
    child.kill("SIGKILL");
    await child.exited;
  });

  const stderr = new Response(child.stderr).text();
  const closed = (async (): Promise<never> => {
    let buffered = "";
    for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk;
      let newline = buffered.indexOf("\n");
      while (newline !== -1) {
        const frame: unknown = JSON.parse(buffered.slice(0, newline));
        buffered = buffered.slice(newline + 1);
        newline = buffered.indexOf("\n");
        if (typeof frame === "object" && frame !== null) options.onFrame(frame);
      }
    }
    throw new Error(`omp closed its RPC stream before the step settled:\n${await stderr}`);
  })();
  closed.catch(() => undefined);

  return {
    send(command) {
      child.stdin.write(`${JSON.stringify(command)}\n`);
      child.stdin.flush();
    },
    async end() {
      child.stdin.end();
      await child.exited;
    },
    closed,
    exited: child.exited,
    pid: child.pid,
  };
}

/** The flags `spawnRpc` and `spawnInteractive` start omp with: only `extensions` loaded. */
function ompFlags(options: {
  readonly extensions: readonly string[];
  readonly workspace: string;
  readonly sessions: string;
}): string[] {
  return [
    "--no-extensions",
    ...options.extensions.flatMap((file) => ["-e", file]),
    "--no-skills",
    "--no-rules",
    "--no-lsp",
    "--no-title",
    "--session-dir",
    options.sessions,
    "--cwd",
    options.workspace,
  ];
}

/** The environment `spawnRpc` and `spawnInteractive` start omp with: `home` as HOME, `bin` first
 * on PATH. */
function ompEnv(options: { readonly home: string; readonly bin?: string }): {
  HOME: string;
  PATH: string;
} {
  return {
    HOME: options.home,
    PATH: [options.bin, "/usr/local/bin:/usr/bin:/bin"].filter(Boolean).join(":"),
  };
}

/** The transcript files under `sessions`, oldest first: one per life of a session, each named
 *  `<timestamp>_<sessionId>.jsonl`, which sorts chronologically. */
async function transcripts(sessions: string): Promise<string[]> {
  return (await readdir(sessions, { recursive: true }))
    .filter((file) => file.endsWith(".jsonl"))
    .sort()
    .map((file) => path.join(sessions, file));
}

/** The session a resume reopens (`--resume <id>` among `spawnRpc`'s `args`): the newest
 *  transcript's id under `sessions`, the `<sessionId>` of its name. */
export async function resumeId(sessions: string): Promise<string> {
  const newest = (await transcripts(sessions)).at(-1);
  if (newest === undefined) throw new Error(`no transcript to resume under ${sessions}`);
  const named = /^[^_]+_(.+)\.jsonl$/.exec(path.basename(newest));
  if (named?.[1] === undefined) throw new Error(`cannot read a session id from ${newest}`);
  return named[1];
}

export interface Interactive {
  /**
   * The screen as the terminal renders it now, scrollback included: one string, each row's
   * trailing blanks trimmed and the trailing empty rows dropped. It holds only what the emulator
   * has parsed so far, so a case polls it.
   */
  screen(): string;
  /** Types `data` at the terminal, as keystrokes: `\r` is Enter, a lone `\x1b` is Esc. */
  write(data: string): void;
  /**
   * Resolves with omp's exit code once it exits and the screen holds every byte read from the
   * terminal by then, so a case that fails on the exit can show what omp printed last.
   */
  readonly exited: Promise<number>;
}

/**
 * Starts interactive `omp` (the TUI, no `--mode`) under a pseudo-terminal of `cols` × `rows`
 * (120 × 40 by default), with `spawnRpc`'s flags and environment, then `args` on its command line
 * (an `--append-system-prompt <prompt>`, a positional word Oh My Pi runs once its startup is over).
 * TERM is `xterm-256color` and OMP_SKIP_SETUP skips the first-run setup wizard, so the TUI starts
 * straight into the session; `env` is spread last. Every byte omp writes to the terminal is fed to
 * a headless xterm of the same size, which `screen` reads. The child is killed at cleanup.
 */
export function spawnInteractive(
  binary: string,
  options: {
    readonly extensions: readonly string[];
    readonly home: string;
    readonly workspace: string;
    readonly sessions: string;
    readonly bin?: string;
    readonly env: Record<string, string>;
    readonly args: readonly string[];
    readonly cols?: number;
    readonly rows?: number;
  },
  cleanup: Cleanup
): Interactive {
  const { cols = 120, rows = 40 } = options;
  const emulator = new Terminal({ cols, rows, allowProposedApi: true });
  const terminal = new Bun.Terminal({
    cols,
    rows,
    data(_terminal, data) {
      emulator.write(data);
    },
  });
  const child = Bun.spawn([binary, ...ompFlags(options), ...options.args], {
    cwd: options.workspace,
    env: { ...ompEnv(options), TERM: "xterm-256color", OMP_SKIP_SETUP: "1", ...options.env },
    terminal,
  });
  cleanup.push(async () => {
    child.kill("SIGKILL");
    await child.exited;
    terminal.close();
    emulator.dispose();
  });

  return {
    screen() {
      const buffer = emulator.buffer.active;
      const lines: string[] = [];
      for (let row = 0; row < buffer.length; row++) {
        lines.push(buffer.getLine(row)?.translateToString(true) ?? "");
      }
      return lines.join("\n").replace(/\n+$/, "");
    },
    write(data) {
      terminal.write(data);
    },
    exited: child.exited.then(
      // The emulator parses what it is fed asynchronously; an empty write's callback runs once
      // everything before it is parsed.
      (code) => new Promise<number>((resolve) => emulator.write("", () => resolve(code)))
    ),
  };
}

/**
 * The Anthropic Messages stream for one scripted reply. `inputTokens` is the input usage its
 * `message_start` reports, which a caller raises to push the session past a compaction threshold.
 */
export function messageStream(blocks: readonly Block[], id: string, inputTokens = 5): string {
  const events: [string, unknown][] = [
    [
      "message_start",
      {
        type: "message_start",
        message: {
          id,
          type: "message",
          role: "assistant",
          model: "standin-model",
          content: [],
          stop_reason: null,
          stop_sequence: null,
          usage: { input_tokens: inputTokens, output_tokens: 1 },
        },
      },
    ],
  ];
  blocks.forEach((block, index) => {
    if (block.type === "text") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
      ]);
      events.push([
        "content_block_delta",
        { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } },
      ]);
    } else {
      events.push([
        "content_block_start",
        {
          type: "content_block_start",
          index,
          content_block: {
            type: "tool_use",
            id: `toolu_${id}_${index}`,
            name: block.name,
            input: {},
          },
        },
      ]);
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        },
      ]);
    }
    events.push(["content_block_stop", { type: "content_block_stop", index }]);
  });
  const stopReason = blocks.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn";
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: { stop_reason: stopReason, stop_sequence: null },
      usage: { output_tokens: 1 },
    },
  ]);
  events.push(["message_stop", { type: "message_stop" }]);
  return events.map(([name, data]) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}

/** What one refusal says: the filter's category and explanation, and whatever the model sent
 * before it refused. */
export interface Refusal {
  /** The category Anthropic's `stop_details` names, which the host writes into the message's
   * error text as `Refusal (<category>): <explanation>`. */
  readonly category?: string;
  readonly explanation?: string;
  /** Text the model streamed before the refusal, committed by the time it came. The host walks
   * its own fallback chain for a refusal that follows no output and ends the turn where it is for
   * one that follows text (`coding-agent/src/session/turn-recovery.ts:2068-2102` and `:3482-3492`
   * at the pinned tag: committed text is replay-unsafe), so this is the one thing that decides
   * which of the two walks a case proves. */
  readonly text?: string;
}

/**
 * The Anthropic Messages stream of one refused turn: a `message_delta` whose `stop_reason` is
 * `refusal` and whose `stop_details` names the category
 * (`packages/ai/src/providers/anthropic.ts:3240-3248` at the pinned tag). The host reads it as an
 * assistant message with `stopReason: "error"`, `stopDetails.type: "refusal"` and an
 * `errorMessage` of `Refusal (<category>): <explanation>`, which is what
 * `classifyProviderTurn` (`@legion/pi-shared/provider-refusal`) calls a content filter. It
 * carries no `fallback_credit_token`: a refusal that carries one sends the host to the provider's
 * own server-side handoff instead of its chain.
 */
export function refusalStream(id: string, refusal: Refusal = {}): string {
  const { category = "cyber", explanation = "Declined", text } = refusal;
  const events: [string, unknown][] = [
    [
      "message_start",
      {
        type: "message_start",
        message: {
          id,
          type: "message",
          role: "assistant",
          model: "standin-model",
          content: [],
          stop_reason: null,
          stop_sequence: null,
          usage: { input_tokens: 5, output_tokens: 1 },
        },
      },
    ],
  ];
  if (text !== undefined) {
    events.push([
      "content_block_start",
      { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
    ]);
    events.push([
      "content_block_delta",
      { type: "content_block_delta", index: 0, delta: { type: "text_delta", text } },
    ]);
    events.push(["content_block_stop", { type: "content_block_stop", index: 0 }]);
  }
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: {
        stop_reason: "refusal",
        stop_sequence: null,
        stop_details: { type: "refusal", category, explanation },
      },
      usage: { output_tokens: 1 },
    },
  ]);
  events.push(["message_stop", { type: "message_stop" }]);
  return events.map(([name, data]) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}

/**
 * Oh My Pi's date/cwd reminder (`packages/coding-agent/src/prompts/system/date-cwd-reminder.md` at
 * the pin) at the start of a text, with the newline or two the host puts between it and the
 * text it leads. The host sends the reminder in one of two forms (`DateCwdReminderInjector`,
 * `packages/coding-agent/src/session/date-cwd-reminder.ts`): ahead of a user turn that is new since
 * the session's previous request, or, when the local date or the working directory has changed
 * since then and the request holds no such turn, as a message of its own after the request's
 * newest message, where it stays in every later request.
 */
const DATE_CWD_REMINDER = /^<system-reminder>\nToday: [^\n]*\n<\/system-reminder>(\n\n?|$)/;

/** `message` without the host's date/cwd reminder: the reminder taken off the start of its text,
 *  and undefined when the reminder was the whole message. */
function withoutDateCwdReminder(message: unknown): unknown {
  if (typeof message !== "object" || message === null || !("content" in message)) return message;
  const { content } = message;
  if (typeof content === "string") {
    const rest = content.replace(DATE_CWD_REMINDER, "");
    if (rest === content) return message;
    return rest === "" ? undefined : { ...message, content: rest };
  }
  if (!Array.isArray(content)) return message;
  const [first, ...others] = content;
  if (typeof first !== "object" || first === null || !("text" in first)) return message;
  if (typeof first.text !== "string") return message;
  const rest = first.text.replace(DATE_CWD_REMINDER, "");
  if (rest === first.text) return message;
  const kept = rest === "" ? others : [{ ...first, text: rest }, ...others];
  return kept.length === 0 ? undefined : { ...message, content: kept };
}

/**
 * A Messages request's messages as the session sent them, without the host's date/cwd reminder in
 * either form (`DATE_CWD_REMINDER`), so a case that reads a request's newest message, or a message
 * by its place or its exact text, reads the same on a run that crosses midnight as on one that
 * does not. Measured in the stall watch's live proof
 * (`pi-legion/extensions/legion-stall-watchdog-omp.test.ts`): with the pane's local date moved on
 * between two requests, the second ended with the reminder as a message of its own, so a case that
 * took its last message for the one that started the turn waited for a turn that had already run,
 * as CI's did on a case that ran across midnight UTC (merge-group run 44963211024, 00:00:14Z on
 * 2026-11-22).
 */
export function ownMessages(body: Record<string, unknown>): unknown[] {
  const messages: unknown[] = Array.isArray(body.messages) ? body.messages : [];
  return messages.map(withoutDateCwdReminder).filter((message) => message !== undefined);
}

/** The newest of a Messages request's own messages (`ownMessages`), as JSON: the message the turn
 *  the request asks for was started on. */
export function newestMessage(body: Record<string, unknown>): string {
  return JSON.stringify(ownMessages(body).at(-1) ?? null);
}

/**
 * Whether a Messages request is a side turn rather than a turn. The host sends one as an
 * ordinary Messages request over a snapshot of the conversation whose newest message is the
 * `<btw>` block around the question, which pi-envoy adds for `ctx.runEphemeralTurn` — so the
 * stand-in answers it distinctly and nothing about it reaches the transcript.
 */
export function isSelfCheck(request: Request): boolean {
  return newestMessage(request.body).includes("<btw>");
}

/** A `tool_result` block as the host sent it back to the gateway. */
export interface ToolResult {
  readonly tool_use_id: string;
  readonly is_error: boolean;
  /** The result's string content, or its text blocks joined. */
  readonly text: string;
}

/** The `tool_result` blocks of a Messages request's user messages, in conversation order. */
export function toolResultsIn(request: Request): ToolResult[] {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages.flatMap((message) => {
    if (typeof message !== "object" || message === null) return [];
    if (!("role" in message) || message.role !== "user") return [];
    if (!("content" in message) || !Array.isArray(message.content)) return [];
    return message.content.flatMap((block: unknown) => {
      if (typeof block !== "object" || block === null) return [];
      if (!("type" in block) || block.type !== "tool_result") return [];
      if (!("tool_use_id" in block) || typeof block.tool_use_id !== "string") return [];
      // The result's content is a string or content blocks, of which the text ones are its text.
      const content = "content" in block ? block.content : undefined;
      const text =
        typeof content === "string"
          ? content
          : (Array.isArray(content) ? content : [])
              .map((part: unknown) =>
                typeof part === "object" &&
                part !== null &&
                "type" in part &&
                part.type === "text" &&
                "text" in part &&
                typeof part.text === "string"
                  ? part.text
                  : ""
              )
              .join("");
      return [
        {
          tool_use_id: block.tool_use_id,
          is_error: "is_error" in block && block.is_error === true,
          text,
        },
      ];
    });
  });
}

/** A `tool_use` block calling tool `name` with `input`, and the one-line intent (`i`) every Oh My
 *  Pi tool requires, which no case reads. */
export function call(name: string, input: Record<string, unknown>): Block {
  return { type: "tool_use", name, input: { i: "Running the case's step", ...input } };
}

/** Whether a Messages request is one of a compaction's summary calls, which the host sends under
 *  their own system prompt: a host that rewords that prompt is fixed here. */
export function isSummarization(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(
    "Summarize user–AI coding-assistant conversations"
  );
}

/** The real binary and the NATS server one case runs against, as `ompNatsGuard().setup()`
 *  resolves them: `binary` is what `runLegionPane` spawns, `nats` what `LegionPaneOptions.nats`
 *  takes. */
export interface OmpNatsRun {
  /** `LEGION_TEST_OMP`: the Oh My Pi binary of the repository's .omp-pin. */
  readonly binary: string;
  /** `LEGION_TEST_NATS_URL`: the NATS server the pane's Envoy extension connects to. */
  readonly nats: string;
}

/** What `ompNatsGuard` hands a file that runs the real binary against a real NATS server. */
export interface OmpNatsGuard {
  /** What every case of the file passes `test.skipIf`. */
  readonly skip: boolean;
  /** Whether this run is on GitHub Actions (`GITHUB_ACTIONS`, not `CI`), for a file that needs a
   *  third thing of its host and so guards it the same way (a tmux on PATH, say). */
  readonly onActions: boolean;
  /** The binary and the server a case runs against; throws on GitHub Actions without them. */
  readonly setup: () => OmpNatsRun;
}

/**
 * The guard every real-Oh-My-Pi pane test opens with: `LEGION_TEST_OMP` names the binary and
 * `LEGION_TEST_NATS_URL` the NATS server each pane runs against, which `LegionPaneOptions.nats`
 * requires — a session whose `ENVOY_NATS_URL` reaches no server is given no consumer, and its
 * claim boot then ends the session (INFRA-771), so a pane without a real server proves nothing.
 *
 * A run without either variable skips, since neither a binary nor a server is on every host.
 * On GitHub Actions it throws naming the variable instead: Actions is the host that ships the
 * change, its job supplies both, and a skip there would hide the only run of the check. The
 * switch is `GITHUB_ACTIONS === "true"`, not `CI`, which agent harnesses on the devbox export.
 */
export function ompNatsGuard(): OmpNatsGuard {
  const binary = process.env.LEGION_TEST_OMP;
  const nats = process.env.LEGION_TEST_NATS_URL;
  const onActions = process.env.GITHUB_ACTIONS === "true";
  return {
    skip: (binary === undefined || nats === undefined) && !onActions,
    onActions,
    setup: () => {
      if (binary === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
      if (nats === undefined) throw new Error("LEGION_TEST_NATS_URL is unset on GitHub Actions");
      return { binary, nats };
    },
  };
}

/** Which Legion pane `runLegionPane` runs, and what its stand-in answers beyond the runner's routes. */
export interface LegionPaneOptions {
  /**
   * Names every fixture string a case's assertions may meet: the scratch root's `legion-<name>-`
   * prefix, the grant ids `<name>-grant-<n>` the `legion` tool's own operations mint, the claim's
   * `<name>-secret`, the listener's `<name>-machine`, the boot token `<name>-boot`, the Dispatch
   * token `<name>-dispatch-token` and the claim token `legion-<name>-<issue>-<role>`, the issue
   * lower-cased.
   */
  readonly name: string;
  /**
   * The NATS server the pane runs against, as `ENVOY_NATS_URL`: a real one (the caller's
   * `LEGION_TEST_NATS_URL`), since the session's relay subject lives on it and a session without
   * one can register nothing and so claim nothing.
   */
  readonly nats: string;
  /** `LEGION_ROLE`. */
  readonly role: string;
  /** `LEGION_TREE`. */
  readonly tree: string;
  /** `LEGION_ISSUE`; the tree itself makes the pane the tree's root: a root architect's. */
  readonly issue: string;
  /** The daemon's assignment, a task the pane takes. Several are given one after another, each
   * once the run the one before it started has settled: a turn a refusal ended starts no
   * continuation of its own, so a case that needs a second turn names a second assignment. An
   * ordinary session (`legion: false`) takes no task, so each is what a person types there: the
   * RPC `prompt`. */
  readonly prompt: string | readonly string[];
  /**
   * False drops every `LEGION_*` variable, so the pane is an ordinary session: the Legion
   * extension stays inert, the Envoy extension's run-end ask nudge is not excluded, and each
   * `prompt` is sent as the RPC `prompt`.
   */
  readonly legion?: boolean;
  /** Configures Dispatch against the stand-in: `DISPATCH_URL` is its base, `DISPATCH_TOKEN` the
   * name's token. The case answers Dispatch's routes through `answer`. */
  readonly dispatch?: boolean;
  /** Static extras spread into the pane's environment, last. */
  readonly env?: Readonly<Record<string, string>>;
  /** Lines appended to the profile's config.yml (`writeStandinProfile`), which is where a case
   * puts the settings its pane runs under — a fallback chain, say. */
  readonly config?: readonly string[];
  /** The providers and models the pane's profile offers (`writeStandinProfile`); the single
   * `standin/standin-model` by default. */
  readonly providers?: readonly StandinProvider[];
  /** Tried first for every request the stand-in serves; `undefined` falls through to the runner's
   * own routes (the gateway, the daemon's claim and grant routes, the Envoy listener). An answer
   * may be a promise, which the stand-in awaits before it replies. */
  readonly answer?: (
    url: URL,
    body: Record<string, unknown>
  ) => Response | Promise<Response> | undefined;
  /** Runs once the scratch root, `bin` (first on the pane's PATH) and `state` exist, before the
   * profile is written and omp is spawned: what the pane's workspace must hold. */
  readonly prepare?: (paths: {
    readonly workspace: string;
    readonly bin: string;
    readonly state: string;
  }) => Promise<void>;
  /**
   * Settle when the gateway has answered nothing for this long, instead of at the host's
   * terminal `agent_end`. A `triggerTurn` steer sent from `agent_end` starts its continuation
   * after that frame, so the terminal frame is not the end of the run's provider traffic.
   */
  readonly quietMs?: number;
  /** The one word the gateway answers the run-end self-check with; `PROCEEDING` by default. */
  readonly selfCheck?: string;
  /** Gets each RPC frame omp prints, in order, in every life of the pane, for a case that waits on
   * a frame of its own (a model change, a run's end that the host marks as not terminal). */
  readonly onFrame?: (frame: object) => void;
  /**
   * Runs in the first life once every prompt's run has settled, before that life ends, with its
   * RPC: a case that starts a run some other way than a prompt (an Envoy card, a background job's
   * result) or that sends a prompt whose run the host does not settle as terminal does it here.
   */
  readonly whileUp?: (rpc: Rpc) => Promise<void>;
}

/** What a settled Legion pane left behind. */
export interface LegionPane {
  /** Every request the stand-in served: the model gateway's, the daemon's, and the listener's. */
  readonly requests: Request[];
  /** The Messages requests that were turns of the conversation, in order. */
  readonly turns: () => Request[];
  /** The Messages requests that were side turns (the self-check), in order. */
  readonly selfChecks: () => Request[];
  /** Every `tool_result` the host sent back to the gateway, in conversation order. */
  readonly toolResults: () => ToolResult[];
  /** Every entry of the persisted transcript, each life's file in turn, in the order omp wrote
   * them, each parsed whole: its `type`, its `timestamp` by the omp process's own clock, and the
   * rest of what omp wrote. */
  readonly transcript: () => Promise<unknown[]>;
  /** The persisted transcript's entries of `customType`, in order, each its `data`. */
  readonly transcriptEntries: (customType: string) => Promise<unknown[]>;
  /**
   * Replaces the `data` of the newest persisted transcript entry of `customType` with what `edit`
   * returns for it, and throws when the transcript holds none. Only between lives, while no omp
   * process holds the file: a case sets a recorded value the next life reads with it, for example
   * a due time that runs on the omp process's own clock, which no test can move.
   */
  readonly editTranscriptEntry: (
    customType: string,
    edit: (data: unknown) => unknown
  ) => Promise<void>;
  /**
   * Runs omp again over this pane's own transcript (`--resume <session>`), in the same scratch
   * root and against the same stand-in, and settles it as the first life settled: what the
   * daemon does when it relaunches a worker whose process is gone. `replies` are appended to the
   * script the first life left, and `prompt` is the assignment — or assignments — the second
   * life is given, as tasks it takes.
   */
  readonly resume: (replies: readonly Reply[], prompt: string | readonly string[]) => Promise<void>;
  /** The pane's issue workspace. */
  readonly workspace: string;
}

/**
 * Runs one Legion pane on the real Oh My Pi until its run settles: the Legion and Envoy
 * extensions from this checkout, booted against a stand-in for the daemon's claim and grant
 * routes and the Envoy listener, and the NATS server `options.nats` names, with a stand-in model
 * gateway that answers the pane's turns from `replies`. The server is real: a session's only
 * inbound is the consumer the listener builds for it, which it is relayed on its own relay
 * subject, so a pane with no NATS server holds no relay subject, every registration of its fails
 * saying so, and its Legion claim fails with it (INFRA-771). The daemon's assignment is a task the
 * pane takes (`./task-stand-in`): the stand-in holds it as the claim's value, the registration
 * answers it, and the take is answered its text; each later one is relayed to the session as a
 * task frame once the run before it has settled. An ordinary session (`legion: false`) is sent
 * each one as the RPC `prompt` instead. Nothing in a pane runs `legion` from bash (LEGION-631),
 * so no stand-in `legion` is on its PATH: `bin` holds what `prepare` put there.
 */
export async function runLegionPane(
  binary: string,
  replies: readonly Reply[],
  options: LegionPaneOptions,
  cleanup: Cleanup
): Promise<LegionPane> {
  const { name, role, tree, issue } = options;
  const legion = options.legion ?? true;
  const claimToken = `legion-${name}-${issue.toLowerCase()}-${role}`;
  // The pane's secrets: the boot token its registration spends, the secret the stand-in daemon
  // answers it with, and its Dispatch bearer.
  const bootToken = fixtureSecret("boot");
  const claimSecret = fixtureSecret("claim");
  const dispatchToken = fixtureSecret("dsp");
  // The daemon's golden registration answer (`packages/daemon/internal/api`), so a field the
  // daemon adds to it reaches the stand-in's `claims/register`.
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { root, home, workspace, sessions } = await ompRoot(binary, `legion-${name}-`, cleanup);
  const state = path.join(root, "state");
  const bin = path.join(root, "bin");
  await mkdir(bin, { recursive: true });
  await mkdir(state, { recursive: true, mode: 0o700 });
  await options.prepare?.({ workspace, bin, state });

  const scripted: Reply[] = [...replies];
  let answered = 0;
  let selfChecks = 0;
  let grants = 0;
  /** Every Messages request the stand-in took, whoever answered it: a case that answers the
   * gateway itself still has to settle on the gateway falling silent. */
  let served = 0;
  let lastAnsweredAt = 0;
  const tasks = taskStandIn();
  // The session the pane registered, which a later task is relayed to.
  let paneSession = "";
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      served += 1;
      lastAnsweredAt = Date.now();
    }
    const own = options.answer?.(url, body);
    if (own !== undefined) return own;
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      // The self-check is not a turn: it consumes no scripted reply, and the conversation's
      // next turn is answered as if it had never happened — which is what the host's snapshot
      // makes true.
      if (isSelfCheck(request)) {
        selfChecks += 1;
        const verdict = options.selfCheck ?? "PROCEEDING";
        return new Response(messageStream([{ type: "text", text: verdict }], `btw_${selfChecks}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      const reply = scripted[answered];
      answered += 1;
      if (reply === undefined) {
        return Response.json(
          {
            type: "error",
            error: { type: "invalid_request_error", message: "no reply scripted" },
          },
          { status: 400 }
        );
      }
      const blocks = typeof reply === "function" ? reply(request) : reply;
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    const taskAnswer = tasks.answer(url.pathname, body);
    if (taskAnswer !== undefined) return taskAnswer;
    if (url.pathname === "/legion/v1/claims/register") {
      paneSession = typeof body.sessionId === "string" ? body.sessionId : "";
      return Response.json({
        ...registered,
        claimToken,
        tree,
        issue,
        role,
        generation: 1,
        secret: claimSecret,
        ...tasks.registered(),
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `${name}-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener, which serves `/v1/` and nothing else: registration, the role claim, and
    // any other `/v1/` read answer with an interest. A path outside it is a route this stand-in
    // does not serve, and it is refused. A case that needs another route, Dispatch's among them,
    // serves it from `answer`. A session takes its messages from its own consumer, relayed on its
    // own deliver subject: a registration answered without one leaves the session deaf, which the
    // plugin refuses rather than accept, and with it every claim the pane makes (INFRA-771).
    if (!url.pathname.startsWith("/v1/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    const sessionID = typeof body.session_id === "string" ? body.session_id : "";
    return Response.json({
      session_id: sessionID,
      machine_id: `${name}-machine`,
      dir: workspace,
      topics: [],
      ...deliveryFor(url.pathname, body, sessionID),
    });
  });
  await writeStandinProfile(home, base, options.config ?? [], { providers: options.providers });

  // The connection a later task is relayed on, opened at the first such task.
  const relay = taskRelayConnection(options.nats, cleanup);

  /**
   * One life of the pane: omp started with `args` on its command line, given each task of
   * `prompts` once the run the one before it started has settled, and left once the last has
   * settled. The first is the value the registration answers; each later one is relayed. An
   * ordinary session's are each its RPC `prompt`, as a person types one. A run has settled when
   * the RPC stream reports its terminal agent_end: a continuation the host scheduled (the
   * follow-up) starts its turn before that, under the same run. A steer the extension sends from
   * `agent_end` instead starts its continuation after that frame, so `quietMs` waits for the
   * gateway to fall silent rather than for the frame.
   */
  const live = async (
    prompts: readonly string[],
    args: readonly string[],
    whileUp?: (rpc: Rpc) => Promise<void>
  ): Promise<void> => {
    let settled = Promise.withResolvers<void>();
    const [first] = prompts;
    if (legion && first !== undefined) tasks.give(first);
    const rpc = spawnRpc(
      binary,
      {
        extensions: [
          // The Envoy entry is the sibling plugin's: a Legion pane loads both, and the Legion
          // entry refuses to run without it.
          path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
          path.resolve(import.meta.dir, "../../pi-legion/extensions/legion.ts"),
        ],
        home,
        workspace,
        sessions,
        bin,
        args,
        env: {
          ENVOY_URL: base,
          ENVOY_NATS_URL: options.nats,
          ...(options.dispatch ? { DISPATCH_URL: base, DISPATCH_TOKEN: dispatchToken } : {}),
          ...(legion
            ? {
                LEGION_DAEMON_URL: base,
                LEGION_ROLE: role,
                LEGION_TREE: tree,
                LEGION_ISSUE: issue,
                LEGION_GENERATION: "1",
                LEGION_BOOT_TOKEN: bootToken,
                LEGION_STATE_DIR: state,
                LEGION_WORKSPACE: workspace,
              }
            : {}),
          ...options.env,
        },
        onFrame: (frame) => {
          options.onFrame?.(frame);
          if (
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false)
          ) {
            settled.resolve();
          }
        },
      },
      cleanup
    );
    for (const [index, prompt] of prompts.entries()) {
      if (index > 0) settled = Promise.withResolvers<void>();
      lastAnsweredAt = Date.now();
      const started = served;
      if (!legion) {
        rpc.send({ type: "prompt", message: prompt });
      } else if (index > 0) {
        publishTaskFrame(await relay(), paneSession, claimToken, tasks.give(prompt));
      }
      if (options.quietMs === undefined) {
        await Promise.race([settled.promise, rpc.closed]);
      } else {
        const quietMs = options.quietMs;
        const deadline = Date.now() + 90_000;
        await Promise.race([
          (async () => {
            // A real clock, deliberately: the proposition is that the real host started no
            // further turn, and a host that does nothing emits no signal to await. The whole
            // point is to see the absence, and only elapsed time shows it.
            while (Date.now() < deadline) {
              await Bun.sleep(200);
              if (served > started && Date.now() - lastAnsweredAt >= quietMs) return;
            }
            throw new Error(`the stand-in gateway never went quiet for ${quietMs} ms`);
          })(),
          rpc.closed,
        ]);
      }
    }
    if (whileUp !== undefined) await Promise.race([whileUp(rpc), rpc.closed]);
    await rpc.end();
  };
  const prompts = typeof options.prompt === "string" ? [options.prompt] : options.prompt;
  await live(prompts, [], options.whileUp);

  const turns = (): Request[] =>
    requests.filter(
      (request) => request.path === "/anthropic/v1/messages" && !isSelfCheck(request)
    );
  // Every line is parsed, the last one included: `rpc.end()` above awaited the child's exit,
  // so nothing is still appending and a trailing fragment is a defect that must throw rather
  // than be withheld (dispatch://INFRA-2188).
  const transcript = async (): Promise<unknown[]> => {
    const entries: unknown[] = [];
    for (const file of await transcripts(sessions)) {
      for (const line of (await readFile(file, "utf8")).split("\n").filter(Boolean)) {
        entries.push(JSON.parse(line));
      }
    }
    return entries;
  };
  return {
    requests,
    turns,
    selfChecks: () =>
      requests.filter(
        (request) => request.path === "/anthropic/v1/messages" && isSelfCheck(request)
      ),
    toolResults: () => {
      // Every turn carries the whole conversation so far, so the first sighting of each id is
      // its place in the conversation.
      const seen = new Set<string>();
      const results: ToolResult[] = [];
      for (const turn of turns()) {
        for (const result of toolResultsIn(turn)) {
          if (seen.has(result.tool_use_id)) continue;
          seen.add(result.tool_use_id);
          results.push(result);
        }
      }
      return results;
    },
    transcript,
    transcriptEntries: async (customType) =>
      (await transcript()).flatMap((entry) =>
        typeof entry === "object" &&
        entry !== null &&
        "customType" in entry &&
        entry.customType === customType
          ? ["data" in entry ? entry.data : undefined]
          : []
      ),
    editTranscriptEntry: async (customType, edit) => {
      for (const file of (await transcripts(sessions)).reverse()) {
        const lines = (await readFile(file, "utf8")).split("\n");
        const at = lines.findLastIndex((line) => {
          if (line === "") return false;
          const entry: unknown = JSON.parse(line);
          return (
            typeof entry === "object" &&
            entry !== null &&
            "customType" in entry &&
            entry.customType === customType
          );
        });
        if (at === -1) continue;
        const entry: Record<string, unknown> = JSON.parse(lines[at] ?? "");
        lines[at] = JSON.stringify({ ...entry, data: edit(entry.data) });
        await writeFile(file, lines.join("\n"));
        return;
      }
      throw new Error(`no transcript entry of ${customType} under ${sessions}`);
    },
    resume: async (more, prompt) => {
      scripted.push(...more);
      await live(typeof prompt === "string" ? [prompt] : prompt, [
        "--resume",
        await resumeId(sessions),
      ]);
    },
    workspace,
  };
}
