/** `bun outside-agent-probe.ts <command> [arguments]`: the calls of the outside agent's acceptance
 *  that need a broker credential, which a shell cannot make: every call carries a DPoP proof that
 *  only the holder of the token's key can sign. `outside-agent-public.sh` runs it for rows P3 and
 *  P5-P8, and `outside-agent-tailnet.sh` runs its `peer` command for the peer session of row P4.
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
 *                            with `event`, its subject, or null after <seconds>.
 *    expiry <grace> <longest>
 *                            trades a token, opens a relay socket with it and waits for the
 *                            listener to close it: {traded_at, expires_at, closed_at, code} in
 *                            Unix ms; code null when it was still open <grace> seconds after the
 *                            token's expiry, or when the token lasts past <longest> seconds, which
 *                            it reports without waiting.
 *    send <session> <text>   POST /v1/messages/send to <session>.
 *    agent-token             POST /api/v1/me/agent-tokens; a token it was given is revoked again
 *                            at once, and its plaintext is never printed.
 *    approve                 POST /api/v1/credential-requests/<a fresh id>/approve.
 *    peer                    the peer of row P4: registers the session, opens its relay socket and
 *                            answers each message from then on, until it is stopped. To
 *                            `outside-e2e ping <nonce>` it answers the sender
 *                            `outside-e2e pong <nonce> at <Unix ms>`, and to
 *                            `outside-e2e ack <nonce>`, `outside-e2e done <nonce> at <Unix ms>`. It
 *                            prints one line of JSON per message it reads and per answer it sends.
 *                            With OUTSIDE_FOREIGN_LISTENER_URL set it first registers
 *                            `<session>-foreign` there, under a token that names no project, and
 *                            keeps it registered: the session outside the project row P6 must not
 *                            find; it prints whether that listener lists it.
 */
import { randomUUID } from "node:crypto";
import { type Envelope, EnvelopeSchema } from "@legion/contracts";
import { brokerTokenFor, type TradedToken } from "@legion/envoy-client/broker-credential";

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

/** An answer as the leg reads it: its status, the refusal's code where the body names one, and
 *  the body's words, cut short. Dispatch and the listener spell a refusal differently: a `code`
 *  beside an `error` text, an `error` object holding both, or an `error` text alone. */
function flat(answer: Answer): { status: number; code: string | null; detail: string | null } {
  const at = (value: unknown, name: string): unknown =>
    typeof value === "object" && value !== null ? Reflect.get(value, name) : undefined;
  const error = at(answer.body, "error");
  const code = at(answer.body, "code") ?? at(error, "code");
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

/** Registers the session with its own agent subject and `extra`, as a session's subscribe does. */
function subscribe(extra: readonly string[]): Promise<Answer> {
  return call("POST", listener("/v1/interests/subscribe"), {
    session_id: session(),
    dir: process.cwd(),
    title: "outside-e2e probe",
    topics: [`notifications.agent.${session()}`, ...extra],
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
 *  identity and no project, every 30 s: the session outside the project that row P6 must not
 *  find on the edge's list. It prints, once, whether that listener took the registration and
 *  lists the session, which is what makes P6's absence mean something. */
async function foreignSession(): Promise<void> {
  const listenerUrl = (env.OUTSIDE_FOREIGN_LISTENER_URL ?? "").trim().replace(/\/+$/, "");
  if (listenerUrl === "") return;
  const foreign = `${session()}-foreign`;
  const trade = brokerTokenFor({ ...env, AGENT_SECRETS_PROJECTS: undefined }, foreign);
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
  print({ foreign, registered: registered.status, listed: named });
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
    print({ read: text, from });
    const asked = /^outside-e2e (ping|ack) ([A-Za-z0-9-]{1,64})$/.exec(text);
    if (asked === null || from === "") continue;
    const [, kind, nonce] = asked;
    // A frame delivered twice is answered once.
    if (answered.has(`${kind} ${nonce}`)) continue;
    answered.add(`${kind} ${nonce}`);
    const reply = `outside-e2e ${kind === "ping" ? "pong" : "done"} ${nonce} at ${Date.now()}`;
    const sent = await sendMessage(from, reply).catch((error: unknown) => ({
      status: 0,
      body: String(error),
    }));
    print({ sent: reply, to: from, status: sent.status });
  }
  clearInterval(heartbeat);
  throw new Unreadable("the peer's relay socket closed");
}

async function main(): Promise<void> {
  const [command, ...args] = Bun.argv.slice(2);
  switch (command) {
    case "whoami": {
      const answer = await call("GET", dispatch("/api/v1/whoami"));
      const read = (name: string): unknown =>
        typeof answer.body === "object" && answer.body !== null
          ? Reflect.get(answer.body, name)
          : undefined;
      print({
        ...flat(answer),
        kind: read("kind") ?? null,
        owner: read("owner") ?? null,
        project: read("project") ?? null,
        scopes: read("scopes") ?? null,
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
        print({ ...flat(followed), event: null });
        return;
      }
      const relay = await openRelay();
      if (!("socket" in relay))
        throw new Unreadable(`the listener serves no relay socket (${relay.status})`);
      const own = await ask(relay, `${deliverPrefix}${session()}`);
      if (!own.granted)
        throw new Unreadable(`the relay socket refused the session's own subject (${own.code})`);
      const prefix = topic.replace(/\.>$/, ".");
      const until = Date.now() + waitMs;
      for (;;) {
        const frame = await nextFrame(relay, Math.max(0, until - Date.now()));
        if (frame === undefined) break;
        if (frame.op !== "msg") continue;
        receipt(relay, frame);
        const subject = envelopeOf(frame)?.topic ?? frame.subject ?? "";
        if (subject.startsWith(prefix)) {
          print({ ...flat(followed), event: subject });
          relay.socket.close(1000, "the probe is done");
          return;
        }
      }
      print({ ...flat(followed), event: null });
      relay.socket.close(1000, "the probe is done");
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
      const answer = await call("POST", dispatch("/api/v1/me/agent-tokens"), {
        name: `outside-e2e-probe-${randomUUID().slice(0, 8)}`,
      });
      const made: unknown = answer.body;
      const id: unknown =
        typeof made === "object" && made !== null ? Reflect.get(made, "id") : undefined;
      if (answer.status < 300) {
        // A token the agent should never have been given is ended at once, and never printed.
        if (typeof id === "string") {
          await call("DELETE", dispatch(`/api/v1/me/agent-tokens/${encodeURIComponent(id)}`)).catch(
            () => undefined
          );
        }
        print({ status: answer.status, code: null, detail: null, revoked: typeof id === "string" });
        return;
      }
      print(flat(answer));
      return;
    }
    case "approve":
      print(
        flat(
          await call("POST", dispatch(`/api/v1/credential-requests/${randomUUID()}/approve`), {
            code: "ABCD-EFGH",
          })
        )
      );
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
    default:
      throw new Unreadable(`unknown command ${JSON.stringify(command ?? "")}`);
  }
}

try {
  await main();
  process.exit(0);
} catch (error) {
  print({ unknown: error instanceof Error ? error.message : String(error) });
  process.exit(2);
}
