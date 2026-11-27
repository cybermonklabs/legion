/** `bun outside-agent-probe.ts <command> [arguments]`: the calls of the outside agent's acceptance
 *  that need a broker credential, which a shell cannot make: every call carries a DPoP proof that
 *  only the holder of the token's key can sign. `outside-agent-public.sh` runs it for rows P3 and
 *  P5-P8, `outside-agent-codex.sh` for rows C1 and C3-C5, and `outside-agent-tailnet.sh` runs its
 *  `peer` command for the peer session of row P4.
 *
 *  It trades the machine's identity for a broker token as every session of the machine does
 *  (`brokerTokenFor`, `@legion/envoy-client/broker-credential`), from the broker settings of its
 *  environment as they stand: `AGENT_SECRETS_URL`, `AGENT_SECRETS_MACHINE_IDENTITY`,
 *  `AGENT_SECRETS_PROJECTS` and the rest. It acts as the session `OUTSIDE_PROBE_SESSION` names, at
 *  the listener `OUTSIDE_LISTENER_URL` and the Dispatch `OUTSIDE_DISPATCH_URL` names.
 *
 *  Each command but `peer` prints one line of JSON on stdout and exits 0 when it read an answer,
 *  whatever the answer says, so the leg that runs it judges it. An HTTP answer is printed as
 *  `{status, code, detail}`: its status, the refusal's code where the body names one, and the
 *  body's words, cut to 300 characters. It prints `{"unknown": "<what it could not read>"}` and
 *  exits 2 when it read none: no token, a listener or Dispatch that did not answer, a socket that
 *  never opened. It prints no token, no proof and no secret value.
 *
 *  A Dispatch call takes its URL from Dispatch's generated contract (`routePath` in
 *  web/src/api/paths.ts), sends its request body as that contract types the route's body
 *  (`Body`), and reads its answer's fields as it types the route's answer (`Result`) and its
 *  refusal (`Refusal`), so a route or a field the contract renames is a type error here; a body
 *  of another shape is read field by field all the same.
 *
 *  Commands:
 *    whoami                  GET /api/v1/whoami at Dispatch, with its kind, owner, project and
 *                            scopes.
 *    interest <topic>        POST /v1/interests/subscribe for the session's own agent subject and
 *                            <topic>; an admitted topic is unsubscribed again.
 *    publish <topic>         POST /v1/messages/publish of <topic>.
 *    sessions [<dir>]        GET /v1/sessions, with `sessions`, the session ids it lists, and
 *                            `at_dir`, the one registered in <dir>, or null.
 *    relay-sub <subject>     opens the session's relay socket and asks it for <subject>:
 *                            {granted: true} or {granted: false, code}; {status} when the
 *                            listener serves no socket there.
 *    github-event <topic> <seconds>
 *                            follows <topic> and waits on the relay socket for one of its events,
 *                            with `event`, its subject, or null after <seconds>, and `sockets`,
 *                            how many it opened: when the edge closes one (at its token's
 *                            expiry), it trades a fresh token and opens another.
 *    expiry <grace> <longest>
 *                            trades a token, opens a relay socket with it and waits for the
 *                            listener to close it: {traded_at, expires_at, closed_at, code} in
 *                            Unix ms; code null when it was still open <grace> seconds after the
 *                            token's expiry, or when the token lasts past <longest> seconds, which
 *                            it reports without waiting.
 *    send <session> <text>   POST /v1/messages/send to <session>.
 *    agent-token             POST /api/v1/me/agent-tokens; a token it was given is revoked again
 *                            at once, and its plaintext is never printed. `revoked` is true only
 *                            when Dispatch answered the revoke 2xx; it is false, with a `detail`
 *                            saying why, when the mint named no token id, or when the revoke was
 *                            refused or gave no answer.
 *    approve                 POST /api/v1/credential-requests/<a fresh id>/approve.
 *    trade                   one fresh trade: {traded: true, expires_at}, or {traded: false,
 *                            status, detail} when the broker refused it (row T5).
 *    hold <seconds>          opens the session's relay socket and prints {opened_at}, then
 *                            {closed_at, code} when it closes, or code null after <seconds>.
 *    peer                    a scripted peer of row P4, for a local stack with no model: registers
 *                            the session, opens its relay socket and answers each message from
 *                            then on, until it is stopped. To `outside-e2e ping <nonce> at <ms>`
 *                            it answers the sender `outside-e2e pong <nonce> at <Unix ms>` and then
 *                            `outside-e2e hello <nonce> at <Unix ms>`; to
 *                            `outside-e2e reply <nonce> at <ms>`, `outside-e2e done <nonce> at
 *                            <Unix ms>`. It prints one line of JSON per message it reads, with the
 *                            time it read it, and per answer it sends. With
 *                            OUTSIDE_FOREIGN_LISTENER_URL set it first does what `foreign` does.
 *    foreign                 registers `<session>-foreign` at OUTSIDE_FOREIGN_LISTENER_URL, under
 *                            a token that names OUTSIDE_FOREIGN_PROJECT alone, prints whether
 *                            that listener lists it, and keeps it registered until it is stopped:
 *                            the session outside the project that row P6 must not find.
 *    issue <project> <title> GET /api/v1/issues of <project>: `key`, the key of its issue titled
 *                            <title>, or null (the Codex leg's scratch issue, row C3).
 *    comment-find <issue> <text>
 *                            GET /api/v1/issues/<issue>/comments: `found`, whether a comment
 *                            holds <text>, its `id`, and its author as Dispatch records it:
 *                            `author_kind`, `author_id` (the session, for a broker token's
 *                            write), `proven`, `owner` and `project`, each null where absent
 *                            (commentAuthor; row C3).
 *    receive <seconds> <text>
 *                            registers the session, opens its relay socket, prints {ready: true}
 *                            once the socket holds its own subject, and waits up to <seconds> for
 *                            a message holding <text>: {read, from, at}, the sender's session and
 *                            the time it read it in Unix ms, or {read: null} (row C5).
 */
import { randomUUID } from "node:crypto";
import { type Envelope, EnvelopeSchema } from "@legion/contracts";
import { brokerTokenFor, type TradedToken } from "@legion/envoy-client/broker-credential";
import { type Body, type Refusal, type Result, routePath } from "../../web/src/api/paths";

// Bun's WebSocket client accepts request headers and a subprotocol; the repository's DOM
// WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string>; readonly protocols: string }
) => WebSocket;

/** K-E2's subprotocol, and the prefix of a session's relay subject (`bus.DeliverSubject`). */
const relaySubprotocol = "envoy-relay.v1";
const deliverPrefix = "notifications.envoy.deliver.";
/** A subject the relay socket carries for no session (the agent stream is never carried): the
 *  socket answers it `subject_not_granted`, and since the socket handles its frames in order, that
 *  answer comes only after every frame sent before it is handled. */
const neverCarried = "agentstream.outside-e2e.frames";
/** How long one HTTP call or one socket's open may take before it reads as no answer. */
const callMs = 15_000;
/** The most sockets `github-event` opens in one wait: one per token of at most 300 s over 15
 *  minutes is four, so thirty is a socket that keeps closing, which reads as no answer. */
const maxSockets = 30;
/** The largest request body the scripted model reads; a larger one is answered 413 unread. */
const modelBodyLimit = 4 * 2 ** 20;

class Unreadable extends Error {}

/** The broker settings the trade reads, and this probe's own `OUTSIDE_*` inputs. */
const env: Parameters<typeof brokerTokenFor>[0] & Record<string, string | undefined> = process.env;

function required(name: string): string {
  const value = (env[name] ?? "").trim();
  if (value === "") throw new Unreadable(`${name} is unset`);
  return value;
}

const session = (): string => required("OUTSIDE_PROBE_SESSION");

let held: TradedToken | undefined;

/** The session's token, traded once per process unless `fresh`. */
async function token(fresh = false): Promise<TradedToken> {
  if (held !== undefined && !fresh && held.token.expiresAt.getTime() - Date.now() > 10_000) {
    return held;
  }
  try {
    held = await brokerTokenFor(env, session())(AbortSignal.timeout(callMs));
  } catch (error) {
    throw new Unreadable(
      `no broker token for session ${session()}: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  return held;
}

async function headersFor(
  method: string,
  url: string,
  traded?: TradedToken
): Promise<Record<string, string>> {
  const { token: broker, key } = traded ?? (await token());
  return {
    Authorization: `DPoP ${broker.accessToken}`,
    DPoP: await key.proof({ method, url, accessToken: broker.accessToken }),
  };
}

interface Answer {
  readonly status: number;
  readonly body: unknown;
}

/** `T`'s fields, `T` a body Dispatch's generated contract types (`Result`, `Refusal`), each
 *  undefined where the body lacks it. Over a union it is each member's, so a field reads only
 *  where every member holds it: a field the contract renames, in any member, is a type error
 *  where it is read. */
type Fields<T> = { readonly [K in keyof T]: T[K] | undefined };

/** `body` read as `T` (`Fields`): the body itself where it is a JSON object or array, and no field
 *  where it is not (a text, or a JSON scalar). A body of another shape is read field by field all
 *  the same, so a field's value is tested before it is used as more than words to print. */
function fieldsOf<T extends object>(body: unknown): Fields<T> {
  const fields: object = typeof body === "object" && body !== null ? body : {};
  return fields as Fields<T>;
}

/** One call with the session's credential, or `traded`'s: its status and its body, as JSON where
 *  it is JSON. */
async function call(
  method: string,
  url: string,
  body?: unknown,
  traded?: TradedToken
): Promise<Answer> {
  const headers = await headersFor(method, url, traded);
  let response: Response;
  try {
    response = await fetch(url, {
      method,
      headers: body === undefined ? headers : { ...headers, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(callMs),
    });
  } catch (error) {
    throw new Unreadable(
      `${method} ${url} got no answer: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const text = await response.text();
  let parsed: unknown = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    // A body that is not JSON is reported as its text.
  }
  return { status: response.status, body: parsed };
}

/** A refusal as Dispatch spells it (`ErrorBody`): a `code` beside an `error` text. */
interface Spelling {
  readonly code: unknown;
  readonly error: unknown;
}

/** An answer as the leg reads it: its status, the refusal's code where the body names one, and
 *  the body's words, cut short. Dispatch and the listener spell a refusal differently: a `code`
 *  beside an `error` text, an `error` object holding both, an `error` text alone, or, for an
 *  edge listener's refusal (listenerwire.EdgeRefusal), a `reason` beside the `error` text.
 *  `refusal` is a Dispatch answer's body read as its route's refusal (`Refusal`), so a field the
 *  route renames is a type error where it is passed; a listener's answer is read by name. */
function flat(
  answer: Answer,
  refusal?: Spelling
): { status: number; code: string | null; detail: string | null } {
  const at = (value: unknown, name: string): unknown =>
    typeof value === "object" && value !== null ? Reflect.get(value, name) : undefined;
  const spelled = refusal ?? { code: at(answer.body, "code"), error: at(answer.body, "error") };
  const error = spelled.error;
  const code = spelled.code ?? at(error, "code") ?? at(answer.body, "reason");
  const words = at(error, "message") ?? error ?? at(answer.body, "message") ?? answer.body;
  return {
    status: answer.status,
    code: typeof code === "string" ? code : null,
    detail: typeof words === "string" ? words.slice(0, 300) : null,
  };
}

const listener = (path: string): string =>
  `${required("OUTSIDE_LISTENER_URL").replace(/\/+$/, "")}${path}`;
const dispatch = (path: string): string =>
  `${required("OUTSIDE_DISPATCH_URL").replace(/\/+$/, "")}${path}`;

/** The registration's request for the session's own consumer, which a listener requires of every
 *  registration (`409 not_requested` otherwise): one epoch per process, as one plugin connection. */
const delivery = { consumer: true, epoch: randomUUID() } as const;

/** Registers the session with its own agent subject and `extra`, as a session's subscribe does. */
function subscribe(extra: readonly string[]): Promise<Answer> {
  return call("POST", listener("/v1/interests/subscribe"), {
    session_id: session(),
    dir: process.cwd(),
    title: "outside-e2e probe",
    topics: [`notifications.agent.${session()}`, ...extra],
    delivery,
  });
}

interface RelayFrame {
  readonly op: string;
  readonly sid?: number;
  readonly subject?: string;
  readonly reply?: string;
  readonly data?: string;
  readonly code?: string;
}

/** One frame the socket sent, read field by field; a frame that is no JSON object with an `op` is
 *  `unreadable`. */
function frameOf(text: string): RelayFrame {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { op: "unreadable" };
  }
  if (typeof value !== "object" || value === null) return { op: "unreadable" };
  const stringField = (name: string): string | undefined => {
    const read: unknown = Reflect.get(value, name);
    return typeof read === "string" ? read : undefined;
  };
  const sid: unknown = Reflect.get(value, "sid");
  return {
    op: stringField("op") ?? "unreadable",
    sid: typeof sid === "number" ? sid : undefined,
    subject: stringField("subject"),
    reply: stringField("reply"),
    data: stringField("data"),
    code: stringField("code"),
  };
}

/** An open relay socket of the session, and the frames it has read. */
interface Relay {
  readonly socket: WebSocket;
  readonly frames: AsyncGenerator<RelayFrame>;
  readonly closed: Promise<{ readonly code: number; readonly at: number }>;
}

/** Opens the session's relay socket, after a plain GET that reads the status a listener with no
 *  socket answers (404) where an upgrade would say nothing. */
async function openRelay(traded?: TradedToken): Promise<Relay | { readonly status: number }> {
  const url = listener(`/v1/sessions/${encodeURIComponent(session())}/relay`);
  const plain = await fetch(url, {
    headers: await headersFor("GET", url, traded),
    signal: AbortSignal.timeout(callMs),
  }).catch((error: unknown) => {
    throw new Unreadable(
      `GET ${url} got no answer: ${error instanceof Error ? error.message : String(error)}`
    );
  });
  // A socket route answers a GET with no upgrade 400 or 426; any other status but those says the
  // listener serves no socket to this caller, which the row reads.
  if (plain.status !== 400 && plain.status !== 426) return { status: plain.status };
  const socket = new BunWebSocket(url.replace(/^http/, "ws"), {
    headers: await headersFor("GET", url, traded),
    protocols: relaySubprotocol,
  });
  const queue: RelayFrame[] = [];
  let wake: (() => void) | undefined;
  const closed = Promise.withResolvers<{ readonly code: number; readonly at: number }>();
  socket.addEventListener("message", (event) => {
    queue.push(frameOf(String(event.data)));
    wake?.();
  });
  socket.addEventListener("close", (event) => {
    closed.resolve({ code: event.code, at: Date.now() });
    wake?.();
  });
  const opened = Promise.withResolvers<void>();
  socket.addEventListener("open", () => opened.resolve());
  socket.addEventListener("error", () =>
    opened.reject(new Unreadable(`the relay socket ${url} did not open`))
  );
  const timer = setTimeout(
    () => opened.reject(new Unreadable(`the relay socket ${url} did not open within ${callMs} ms`)),
    callMs
  );
  await Promise.race([opened.promise, closed.promise.then(() => opened.promise)]).finally(() =>
    clearTimeout(timer)
  );
  let ended = false;
  void closed.promise.then(() => {
    ended = true;
  });
  async function* frames(): AsyncGenerator<RelayFrame> {
    for (;;) {
      const next = queue.shift();
      if (next !== undefined) {
        yield next;
        continue;
      }
      if (ended) return;
      const { promise, resolve } = Promise.withResolvers<void>();
      wake = resolve;
      await promise;
    }
  }
  return { socket, frames: frames(), closed: closed.promise };
}

function send(relay: Relay, frame: RelayFrame): void {
  relay.socket.send(JSON.stringify(frame));
}

/** Acknowledges a relay frame as handled, so the session consumer does not deliver it again. */
function receipt(relay: Relay, frame: RelayFrame): void {
  if (frame.reply !== undefined && frame.reply !== "") {
    send(relay, { op: "pub", subject: frame.reply, data: Buffer.from("done").toString("base64") });
  }
}

/** The next frame within `ms`, or undefined when none came or the socket closed. */
async function nextFrame(relay: Relay, ms: number): Promise<RelayFrame | undefined> {
  const timer = Promise.withResolvers<undefined>();
  const handle = setTimeout(() => timer.resolve(undefined), ms);
  const frame = await Promise.race([relay.frames.next().then((next) => next.value), timer.promise]);
  clearTimeout(handle);
  return frame ?? undefined;
}

/** Asks the socket for `subject`, then for a subject it never carries, and reads the answers: the
 *  second refusal, which comes only once the first frame was handled, says whether the first
 *  was refused too. */
async function ask(relay: Relay, subject: string): Promise<{ granted: boolean; code?: string }> {
  send(relay, { op: "sub", sid: 1, subject });
  send(relay, { op: "sub", sid: 2, subject: neverCarried });
  let first: string | undefined;
  for (;;) {
    const frame = await nextFrame(relay, callMs);
    if (frame === undefined) {
      throw new Unreadable(`the relay socket gave no answer to a sub of ${neverCarried}`);
    }
    if (frame.op === "err" && frame.sid === 1) first = frame.code ?? "";
    if (frame.op === "err" && frame.sid === 2) {
      return first === undefined ? { granted: true } : { granted: false, code: first };
    }
  }
}

/** The envelope a relay frame carries, or undefined when its data is no envelope. */
function envelopeOf(frame: RelayFrame): Envelope | undefined {
  let value: unknown;
  try {
    value = JSON.parse(Buffer.from(frame.data ?? "", "base64").toString("utf8"));
  } catch {
    return undefined;
  }
  const parsed = EnvelopeSchema.safeParse(value);
  return parsed.success ? parsed.data : undefined;
}

function sendMessage(target: string, text: string): Promise<Answer> {
  return call("POST", listener("/v1/messages/send"), {
    source: "agent",
    source_session: session(),
    target_session: target,
    message: text,
    idempotency_key: randomUUID(),
  });
}

function print(value: unknown): void {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

/** Keeps `<session>-foreign` registered at OUTSIDE_FOREIGN_LISTENER_URL with this machine's
 *  identity, every 30 s: the session outside the project that row P6 must not find on the edge's
 *  list. Its token names OUTSIDE_FOREIGN_PROJECT alone, a project of this machine's person other
 *  than the edge's. A token naming none would carry every project of the person (the broker fills
 *  a person's token with them), the edge's among them, and the edge would rightly list it; and a
 *  machine trading for itself names the projects its entry decides, whatever it asks. It prints,
 *  once, whether that listener took the registration and lists the session, which is what makes
 *  P6's absence mean something, and, with OUTSIDE_FOREIGN_EDGE_URL set, whether that edge admits
 *  the session's token (`edge_admits`: false for a 403 token_not_admitted, true for a 2xx, null for
 *  any other answer): an edge that admits it holds the session in its project. */
async function foreignSession(): Promise<void> {
  const listenerUrl = (env.OUTSIDE_FOREIGN_LISTENER_URL ?? "").trim().replace(/\/+$/, "");
  if (listenerUrl === "") return;
  const project = (env.OUTSIDE_FOREIGN_PROJECT ?? "").trim();
  if (project === "") {
    throw new Unreadable(
      "OUTSIDE_FOREIGN_PROJECT is unset: the session outside the project needs a project of its own"
    );
  }
  const foreign = `${session()}-foreign`;
  const trade = brokerTokenFor({ ...env, AGENT_SECRETS_PROJECTS: project }, foreign);
  const signed = async (method: string, url: string, body?: unknown): Promise<Answer> => {
    const traded = await trade(AbortSignal.timeout(callMs));
    return call(method, url, body, traded);
  };
  const register = (): Promise<Answer> =>
    signed("POST", `${listenerUrl}/v1/interests/subscribe`, {
      session_id: foreign,
      dir: process.cwd(),
      title: "outside-e2e foreign",
      topics: [`notifications.agent.${foreign}`],
      delivery,
    });
  const registered = await register().catch((error: unknown) => ({
    status: 0,
    body: String(error),
  }));
  const listed = await signed("GET", `${listenerUrl}/v1/sessions`).catch(() => ({
    status: 0,
    body: [],
  }));
  const named =
    Array.isArray(listed.body) &&
    listed.body.some(
      (entry: unknown) =>
        typeof entry === "object" && entry !== null && Reflect.get(entry, "session_id") === foreign
    );
  const edgeUrl = (env.OUTSIDE_FOREIGN_EDGE_URL ?? "").trim().replace(/\/+$/, "");
  const atEdge =
    edgeUrl === ""
      ? undefined
      : flat(
          await signed("GET", `${edgeUrl}/v1/sessions`).catch((error: unknown) => ({
            status: 0,
            body: String(error),
          }))
        );
  const edgeAdmits =
    atEdge === undefined
      ? undefined
      : atEdge.status === 403 && atEdge.code === "token_not_admitted"
        ? false
        : atEdge.status >= 200 && atEdge.status < 300
          ? true
          : null;
  print({
    foreign,
    registered: registered.status,
    listed: named,
    ...(atEdge === undefined
      ? {}
      : { edge_admits: edgeAdmits, edge_status: atEdge.status, edge_detail: atEdge.detail }),
  });
  setInterval(() => void register().catch(() => undefined), 30_000);
}

async function peer(): Promise<void> {
  await foreignSession();
  const registered = await subscribe([]);
  if (registered.status >= 300) {
    throw new Unreadable(`the peer's registration answered ${registered.status}`);
  }
  const relay = await openRelay();
  if (!("socket" in relay))
    throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
  const granted = await ask(relay, `${deliverPrefix}${session()}`);
  if (!granted.granted)
    throw new Unreadable(`the relay socket refused the peer's own subject (${granted.code})`);
  print({ peer: session(), ready: true });
  // The registration is kept fresh as a session's heartbeat keeps it.
  const heartbeat = setInterval(() => void subscribe([]).catch(() => {}), 30_000);
  const answered = new Set<string>();
  for await (const frame of relay.frames) {
    if (frame.op !== "msg") continue;
    receipt(relay, frame);
    const envelope = envelopeOf(frame);
    const text = (envelope?.payload ?? envelope?.payload_summary ?? "").trim();
    const from = envelope?.source_session ?? envelope?.sender?.session_id ?? "";
    print({ read: text, from, at: Date.now() });
    const asked = /^outside-e2e (ping|reply) ([A-Za-z0-9-]{1,64}) at [0-9]+$/.exec(text);
    if (asked === null || from === "") continue;
    const [, kind, nonce] = asked;
    // A frame delivered twice is answered once.
    if (answered.has(`${kind} ${nonce}`)) continue;
    answered.add(`${kind} ${nonce}`);
    // A ping gets a pong and then a hello, which the other side replies to; a reply gets a done.
    for (const word of kind === "ping" ? ["pong", "hello"] : ["done"]) {
      const answer = `outside-e2e ${word} ${nonce} at ${Date.now()}`;
      const sent = await sendMessage(from, answer).catch((error: unknown) => ({
        status: 0,
        body: String(error),
      }));
      print({ sent: answer, to: from, status: sent.status });
    }
  }
  clearInterval(heartbeat);
  throw new Unreadable("the peer's relay socket closed");
}

/** A keyless stand-in for the outside person's own model on the public leg's runner: an
 *  OpenAI-compatible chat-completions endpoint on 127.0.0.1 that answers every turn with one fixed
 *  line and no tool call. An Envoy card is handed to the Oh My Pi session as a turn, which a session
 *  with no model refuses and so never shows, and no model key goes on the runner. It prints {port}
 *  once it listens and serves until it is stopped. */
/** The JSON of `request`'s body, read up to `modelBodyLimit` bytes; undefined for a body past it.
 *  A body that is no JSON reads as an empty object. */
async function boundedJson(request: Request): Promise<unknown> {
  if (Number(request.headers.get("content-length") ?? "0") > modelBodyLimit) return undefined;
  const reader = request.body?.getReader();
  if (reader === undefined) return {};
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > modelBodyLimit) {
      await reader.cancel();
      return undefined;
    }
    chunks.push(value);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    return {};
  }
}

async function scriptedModel(): Promise<never> {
  const reply = "outside-e2e: the scripted model answers no message of its own.";
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      if (request.method === "GET" && url.pathname === "/v1/models") {
        return Response.json({ object: "list", data: [{ id: "scripted", object: "model" }] });
      }
      if (request.method !== "POST" || url.pathname !== "/v1/chat/completions") {
        return Response.json({ error: { message: `no route ${url.pathname}` } }, { status: 404 });
      }
      const body = await boundedJson(request);
      if (body === undefined) {
        return Response.json(
          { error: { message: "the request body is too large" } },
          { status: 413 }
        );
      }
      const id = `chatcmpl-${randomUUID()}`;
      const created = Math.floor(Date.now() / 1000);
      const usage = { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 };
      const base = { id, created, model: "scripted" };
      if (typeof body !== "object" || body === null || Reflect.get(body, "stream") !== true) {
        return Response.json({
          ...base,
          object: "chat.completion",
          choices: [
            { index: 0, message: { role: "assistant", content: reply }, finish_reason: "stop" },
          ],
          usage,
        });
      }
      const chunks = [
        { index: 0, delta: { role: "assistant", content: reply }, finish_reason: null },
        { index: 0, delta: {}, finish_reason: "stop" },
      ].map((choice) => ({ ...base, object: "chat.completion.chunk", choices: [choice] }));
      const events = [...chunks, { ...base, object: "chat.completion.chunk", choices: [], usage }]
        .map((event) => `data: ${JSON.stringify(event)}\n\n`)
        .join("");
      return new Response(`${events}data: [DONE]\n\n`, {
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
      });
    },
  });
  print({ port: server.port });
  return Promise.withResolvers<never>().promise;
}

/** GET /api/v1/issues's answer to `issue`'s list, which names a limit and an offset: one page
 *  (IssueSummaryPage), where a list that names neither answers the bare array. */
type IssuePage = Extract<Result<"/api/v1/issues", "get">, { issues: unknown }>;
type ListedIssue = NonNullable<IssuePage["issues"]>[number];

/** One comment of an issue's comments as Dispatch answers them: the bare list, which the route
 *  answers to a read with no `order=activity`, where that one answers a page of threads. */
type ListedComment = Extract<Result<"/api/v1/issues/{key}/comments", "get">, unknown[]>[number];

/** What `comment-find` reads of the first comment of `comments` (an issue's comments as Dispatch
 *  answers them) whose body holds `wanted`: its id, and its author as Dispatch records a write
 *  made with a broker-issued token (api/broker_token.go): kind `session`, the session as `id`,
 *  `proven` true, the person as `owner`, and `project` only for a token mapped to one project.
 *  Each field absent reads null. A comment and its author are read as the route's answer types
 *  them (`ListedComment`), so a field the contract renames is a type error here. Exported for
 *  the Codex leg's tests, which give it Dispatch's shape. */
export function commentAuthor(
  comments: unknown,
  wanted: string
): Record<string, string | boolean | null> {
  const text = (value: unknown): string | null => (typeof value === "string" ? value : null);
  const match: unknown = (Array.isArray(comments) ? comments : []).find((each: unknown) =>
    String(fieldsOf<ListedComment>(each).body ?? "").includes(wanted)
  );
  const found = match === undefined ? undefined : fieldsOf<ListedComment>(match);
  const author = fieldsOf<ListedComment["author"]>(found?.author);
  return {
    found: found !== undefined,
    id: text(found?.id),
    author_kind: text(author.kind),
    author_id: text(author.id),
    proven: author.proven === true,
    owner: text(author.owner),
    project: text(author.project),
  };
}

async function main(): Promise<void> {
  const [command, ...args] = Bun.argv.slice(2);
  switch (command) {
    case "whoami": {
      const answer = await call("GET", dispatch(routePath("get", "/api/v1/whoami", {})));
      // Every answer holds kind and scopes; only an agent's (kind agent) holds owner and project.
      const who = fieldsOf<Result<"/api/v1/whoami", "get">>(answer.body);
      const agent = fieldsOf<Extract<Result<"/api/v1/whoami", "get">, { kind: "agent" }>>(
        answer.body
      );
      print({
        ...flat(answer, fieldsOf<Refusal<"/api/v1/whoami", "get">>(answer.body)),
        kind: who.kind ?? null,
        owner: agent.owner ?? null,
        project: agent.project ?? null,
        scopes: who.scopes ?? null,
      });
      return;
    }
    case "interest": {
      const topic = args[0] ?? "";
      if (topic === "") throw new Unreadable("interest needs a topic");
      const answer = await subscribe([topic]);
      if (answer.status < 300) {
        await call("POST", listener("/v1/interests/unsubscribe"), {
          session_id: session(),
          topics: [topic],
        }).catch(() => undefined);
      }
      print(flat(answer));
      return;
    }
    case "publish": {
      const topic = args[0] ?? "";
      if (topic === "") throw new Unreadable("publish needs a topic");
      print(
        flat(
          await call("POST", listener("/v1/messages/publish"), {
            source: "agent",
            source_session: session(),
            topic,
            message: "outside-e2e: a publish the edge must refuse",
          })
        )
      );
      return;
    }
    case "sessions": {
      // The session whose directory is args[0], where one is named: how the leg finds the id of
      // the Oh My Pi session it started.
      const dir = args[0];
      const answer = await call("GET", listener("/v1/sessions"));
      const entries = Array.isArray(answer.body) ? answer.body : [];
      const sessions: string[] = [];
      let atDir: string | null = null;
      for (const entry of entries) {
        if (typeof entry !== "object" || entry === null) continue;
        const id: unknown = Reflect.get(entry, "session_id");
        const where: unknown = Reflect.get(entry, "dir");
        if (typeof id !== "string") continue;
        sessions.push(id);
        if (dir !== undefined && where === dir) atDir = id;
      }
      print({
        ...flat(answer),
        sessions: Array.isArray(answer.body) ? sessions : null,
        at_dir: atDir,
      });
      return;
    }
    case "relay-sub": {
      const subject = args[0] ?? "";
      if (subject === "") throw new Unreadable("relay-sub needs a subject");
      await subscribe([]);
      const relay = await openRelay();
      if (!("socket" in relay)) {
        print(relay);
        return;
      }
      print(await ask(relay, subject));
      relay.socket.close(1000, "the probe is done");
      return;
    }
    case "github-event": {
      const [topic = "", seconds = ""] = args;
      const waitMs = Number(seconds) * 1000;
      if (topic === "" || !(waitMs > 0))
        throw new Unreadable("github-event needs a topic and seconds");
      const followed = await subscribe([topic]);
      if (followed.status >= 300) {
        print({ ...flat(followed), event: null, sockets: 0 });
        return;
      }
      const prefix = topic.replace(/\.>$/, ".");
      const until = Date.now() + waitMs;
      // One socket rides one token, which lasts at most 300 s, and the edge closes it at the
      // token's expiry; so whenever a socket closes before the wait ends, the probe trades a fresh
      // token, registers again and opens another, as a session's client does. The session's
      // consumer hands the next socket what the last one held unacknowledged.
      let sockets = 0;
      while (Date.now() < until) {
        if (sockets > 0) {
          if (sockets >= maxSockets) {
            throw new Unreadable(`the relay socket closed ${sockets} times before the wait ended`);
          }
          await Bun.sleep(1000);
          await token(true);
          const again = await subscribe([topic]);
          if (again.status >= 300) {
            print({ ...flat(again), event: null, sockets });
            return;
          }
        }
        const relay = await openRelay();
        if (!("socket" in relay))
          throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
        sockets += 1;
        const own = await ask(relay, `${deliverPrefix}${session()}`);
        if (!own.granted)
          throw new Unreadable(`the relay socket refused the session's own subject (${own.code})`);
        // nextFrame waits until the wait's end unless the socket closes first.
        for (;;) {
          const frame = await nextFrame(relay, Math.max(0, until - Date.now()));
          if (frame === undefined) break;
          if (frame.op !== "msg") continue;
          receipt(relay, frame);
          const subject = envelopeOf(frame)?.topic ?? frame.subject ?? "";
          if (subject.startsWith(prefix)) {
            print({ ...flat(followed), event: subject, sockets });
            relay.socket.close(1000, "the probe is done");
            return;
          }
        }
        relay.socket.close(1000, "the probe is done");
      }
      print({ ...flat(followed), event: null, sockets });
      return;
    }
    case "expiry": {
      const graceMs = Number(args[0] ?? "") * 1000;
      const longestMs = Number(args[1] ?? "") * 1000;
      if (!(graceMs > 0) || !(longestMs > 0)) {
        throw new Unreadable(
          "expiry needs the seconds to wait past the token's expiry and the longest a token may last"
        );
      }
      const tradedAt = Date.now();
      const traded = await token(true);
      const expiresAt = traded.token.expiresAt.getTime();
      // A token that lasts past the longest it may is the answer already; nothing is waited for.
      if (expiresAt - tradedAt > longestMs + 1000) {
        print({ traded_at: tradedAt, expires_at: expiresAt, closed_at: null, code: null });
        return;
      }
      await subscribe([]);
      const relay = await openRelay(traded);
      if (!("socket" in relay))
        throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
      const timer = Promise.withResolvers<undefined>();
      const handle = setTimeout(
        () => timer.resolve(undefined),
        Math.max(0, expiresAt + graceMs - Date.now())
      );
      const closed = await Promise.race([relay.closed, timer.promise]);
      clearTimeout(handle);
      print({
        traded_at: tradedAt,
        expires_at: expiresAt,
        closed_at: closed?.at ?? null,
        code: closed?.code ?? null,
      });
      relay.socket.close(1000, "the probe is done");
      return;
    }
    case "send": {
      const [target = "", ...words] = args;
      if (target === "" || words.length === 0)
        throw new Unreadable("send needs a session and a text");
      print(flat(await sendMessage(target, words.join(" "))));
      return;
    }
    case "agent-token": {
      const mint = routePath("post", "/api/v1/me/agent-tokens", {});
      const asked: Body<"/api/v1/me/agent-tokens", "post"> = {
        name: `outside-e2e-probe-${randomUUID().slice(0, 8)}`,
      };
      const answer = await call("POST", dispatch(mint), asked);
      if (answer.status >= 300) {
        print(flat(answer, fieldsOf<Refusal<"/api/v1/me/agent-tokens", "post">>(answer.body)));
        return;
      }
      // A token the agent should never have been given is ended at once, and never printed; the
      // detail names the revoke by its route, never by the token's id. An empty id names no token,
      // and routePath builds no URL for it.
      const id: unknown = fieldsOf<Result<"/api/v1/me/agent-tokens", "post">>(answer.body).id;
      let revoked = false;
      let detail: string | null = "the mint answered no token id, so nothing was revoked";
      if (typeof id === "string" && id !== "") {
        const revoke = routePath("delete", "/api/v1/me/agent-tokens/{id}", { id });
        const ended = await call("DELETE", dispatch(revoke)).catch(() => undefined);
        if (ended === undefined) {
          detail =
            "the revoke DELETE /api/v1/me/agent-tokens/{id} gave no answer, so the token is not known to be revoked";
        } else if (ended.status >= 200 && ended.status < 300) {
          revoked = true;
          detail = null;
        } else {
          detail = `the revoke DELETE /api/v1/me/agent-tokens/{id} answered ${ended.status}, so the token is not revoked`;
        }
      }
      print({ status: answer.status, code: null, detail, revoked });
      return;
    }
    case "trade": {
      // A fresh trade, told apart from no answer: a refusal carries the broker's numeric
      // status somewhere on the error or its causes; a failure with none never reached it.
      try {
        const traded = await brokerTokenFor(env, session())(AbortSignal.timeout(callMs));
        print({ traded: true, expires_at: traded.token.expiresAt.getTime() });
      } catch (error) {
        let status: unknown;
        for (let at: unknown = error; at instanceof Error && status === undefined; ) {
          const read: unknown = Reflect.get(at, "status");
          if (typeof read === "number") status = read;
          at = at.cause;
        }
        const detail = (error instanceof Error ? error.message : String(error)).slice(0, 300);
        if (typeof status !== "number") throw new Unreadable(`the trade got no answer: ${detail}`);
        print({ traded: false, status, detail });
      }
      return;
    }
    case "hold": {
      // Opens a relay socket and holds it: one line once it is open, one when it closes or
      // when <seconds> have passed with it open (code null).
      const holdMs = Number(args[0] ?? "") * 1000;
      if (!(holdMs > 0)) throw new Unreadable("hold needs the seconds to hold the socket");
      await subscribe([]);
      const relay = await openRelay();
      if (!("socket" in relay)) {
        throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
      }
      print({ opened_at: Date.now() });
      const timer = Promise.withResolvers<undefined>();
      const handle = setTimeout(() => timer.resolve(undefined), holdMs);
      const closed = await Promise.race([relay.closed, timer.promise]);
      clearTimeout(handle);
      print({ closed_at: closed?.at ?? null, code: closed?.code ?? null });
      relay.socket.close(1000, "the probe is done");
      return;
    }
    case "approve": {
      // A record id is a lowercase hex sha256; this one names no record.
      const approve = routePath("post", "/api/v1/credential-requests/{id}/approve", {
        id: "0".repeat(64),
      });
      const decision: Body<"/api/v1/credential-requests/{id}/approve", "post"> = {
        code: "ABCD-EFGH",
      };
      const answer = await call("POST", dispatch(approve), decision);
      print(
        flat(
          answer,
          fieldsOf<Refusal<"/api/v1/credential-requests/{id}/approve", "post">>(answer.body)
        )
      );
      return;
    }
    case "foreign":
      if ((env.OUTSIDE_FOREIGN_LISTENER_URL ?? "").trim() === "") {
        throw new Unreadable("OUTSIDE_FOREIGN_LISTENER_URL is unset");
      }
      await foreignSession();
      await Promise.withResolvers<never>().promise;
      return;
    case "peer":
      try {
        await peer();
      } catch (error) {
        if ((env.OUTSIDE_FOREIGN_LISTENER_URL ?? "").trim() === "") throw error;
        // The peer could not start, and the foreign session stays registered for row P6 until
        // the leg stops this process.
        print({ unknown: error instanceof Error ? error.message : String(error) });
        await Promise.withResolvers<never>().promise;
      }
      return;
    case "issue": {
      const [project = "", ...words] = args;
      const title = words.join(" ");
      if (project === "" || title === "") throw new Unreadable("issue needs a project and a title");
      const list = routePath("get", "/api/v1/issues", {}, { project, limit: 250, offset: 0 });
      const answer = await call("GET", dispatch(list));
      const page = fieldsOf<IssuePage>(answer.body);
      let key: string | null = null;
      for (const listed of Array.isArray(page.issues) ? page.issues : []) {
        const issue = fieldsOf<ListedIssue>(listed);
        if (issue.title === title && typeof issue.key === "string") key = issue.key;
      }
      print({ ...flat(answer, fieldsOf<Refusal<"/api/v1/issues", "get">>(answer.body)), key });
      return;
    }
    case "comment-find": {
      const [issue = "", ...words] = args;
      const wanted = words.join(" ");
      if (issue === "" || wanted === "")
        throw new Unreadable("comment-find needs an issue and a text");
      const comments = routePath("get", "/api/v1/issues/{key}/comments", { key: issue });
      const answer = await call("GET", dispatch(comments));
      print({
        ...flat(answer, fieldsOf<Refusal<"/api/v1/issues/{key}/comments", "get">>(answer.body)),
        ...commentAuthor(answer.body, wanted),
      });
      return;
    }
    case "receive": {
      const [seconds = "", ...words] = args;
      const waitMs = Number(seconds) * 1000;
      const wanted = words.join(" ");
      if (!(waitMs > 0) || wanted === "") throw new Unreadable("receive needs seconds and a text");
      const registered = await subscribe([]);
      if (registered.status >= 300) {
        throw new Unreadable(`the receiver's registration answered ${registered.status}`);
      }
      const relay = await openRelay();
      if (!("socket" in relay))
        throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
      const own = await ask(relay, `${deliverPrefix}${session()}`);
      if (!own.granted)
        throw new Unreadable(`the relay socket refused the receiver's own subject (${own.code})`);
      print({ ready: true });
      // The registration is kept fresh as a session's heartbeat keeps it.
      const heartbeat = setInterval(() => void subscribe([]).catch(() => {}), 30_000);
      const until = Date.now() + waitMs;
      for (;;) {
        const frame = await nextFrame(relay, Math.max(0, until - Date.now()));
        if (frame === undefined) break;
        if (frame.op !== "msg") continue;
        receipt(relay, frame);
        const envelope = envelopeOf(frame);
        const read = (envelope?.payload ?? envelope?.payload_summary ?? "").trim();
        if (!read.includes(wanted)) continue;
        const from = envelope?.source_session ?? envelope?.sender?.session_id ?? "";
        print({ read: wanted, from, at: Date.now() });
        clearInterval(heartbeat);
        relay.socket.close(1000, "the probe is done");
        return;
      }
      clearInterval(heartbeat);
      print({ read: null, from: null, at: Date.now() });
      relay.socket.close(1000, "the probe is done");
      return;
    }
    case "model":
      await scriptedModel();
      return;
    default:
      throw new Unreadable(`unknown command ${JSON.stringify(command ?? "")}`);
  }
}

if (import.meta.main) {
  try {
    await main();
    process.exit(0);
  } catch (error) {
    print({ unknown: error instanceof Error ? error.message : String(error) });
    process.exit(2);
  }
}
