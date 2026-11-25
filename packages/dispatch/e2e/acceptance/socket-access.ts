/** `bun socket-access.ts doc <server> <header file> <document id> <schema version>`,
 *  `bun socket-access.ts live <server> <header file> <ready file> <written file>`,
 *  `bun socket-access.ts watch <server> <header file> <ready file> <written file> <seconds>` and
 *  `bun socket-access.ts agent <server> <header file> <session id> <seconds>`: opens
 *  one of Dispatch's two sockets as the caller whose credential the header file holds - one
 *  `Name: value` line, a `Cookie` or an `Authorization` - and prints what that caller is answered,
 *  in one form for every outcome, so a driver compares two callers' answers, or the answers to two
 *  ids, byte for byte. The server is an `http://` or an `https://` origin; an `https://` one is
 *  asked over TLS, on port 443 when it names none.
 *
 *  - An upgrade the server refuses prints `http <status> <body>`. Bun's WebSocket reports a
 *    refused upgrade as an error with no status, so the upgrade is asked once over a connection of
 *    its own first, and the socket is opened only when that answer is 101.
 *  - `doc` sends the one token frame the browser's Hocuspocus provider sends (as socket-scope.ts
 *    does) and prints `authenticated <scope>` or `denied <reason>`, or `closed <code> <reason>`
 *    for a socket closed before either.
 *  - `live` subscribes to the event feed from now, with no resume, writes the ready file once the
 *    feed's `events_start` frame has come, waits for the driver to make the written file (its
 *    writes have committed; it names, one a line, the comments this caller must be sent), and
 *    prints the `events_start` frame and then every frame the server sent, one per line, until
 *    the feed has sent every event GET /api/v1/events/log lets this caller read after the
 *    `events_start` (readableEventIds): what this caller is sent live of the writes the driver
 *    made. The feed sends an event only once the server's event horizon has passed it, which any
 *    transaction open on the Postgres instance holds back, so no quiet period can say the feed has
 *    sent everything; the log, read by the same rule, says what it owes. Each event's id, in the
 *    frame and in the event, is printed as `<event>`: two servers number their events apart, and
 *    which events a caller is sent is the answer, not their numbers.
 *  - `watch` subscribes as `live` does and writes the ready file at the same moment, then prints
 *    every frame the server sent but heartbeats, each as the server wrote it and the moment it
 *    comes, from the `events_start` frame until `<seconds>` after the written file appeared,
 *    however busy or quiet the feed was: what this caller was sent in that window, which a driver
 *    may read while it runs. The driver has `writtenWithinMs` to make the written file. A
 *    subscription the server ends (`ended`, as it ends one whose read failed, the socket left
 *    open) or refuses (`error`) sends nothing more, so the probe then prints
 *    `closed ended <reason>` or `closed refused <code>` and stops, as it does at a close: its
 *    window was not covered. e2e/acceptance/restricted-sweep.sh runs it.
 *  - `agent` subscribes to the live socket's agent view of one session's conversation, and prints
 *    every frame the server sent but heartbeats, each as the server wrote it, until `<seconds>`
 *    after the first; an `ended` or `error` frame ends it as it ends `watch`.
 *    e2e/acceptance/restricted-sweep.sh runs it.
 *
 *  Exit 0 for any answer, a refusal included; 1 when the server could not be reached, gave no
 *  answer within ten seconds (for `watch`, within ten seconds beyond the written file's wait and
 *  the window; for `agent`, beyond the window), or (live) did not send what its log owes within
 *  three minutes of the `events_start`; 2 for a wrong call. e2e/acceptance/project-access.sh and
 *  project-access-blob.sh run `doc` and `live`. */
import { createDecoder, readVarString, readVarUint } from "lib0/decoding";
import { createEncoder, toUint8Array, writeVarString, writeVarUint } from "lib0/encoding";

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

/** Hocuspocus's message type for authentication, and its token and answer subtypes. */
const authMessage = 2;
const tokenSubtype = 0;
const permissionDeniedSubtype = 1;
const authenticatedSubtype = 2;

const answerWithinMs = 10_000;
/** How long the event feed has, from its `events_start`, to send what its log owes the caller.
 *  Only a failure ends on it: an open transaction anywhere on the instance holds the feed back,
 *  and the probe waits that out rather than taking a silence as the end. */
const settleWithinMs = 180_000;
/** How soon the event log is read again while it does not yet hold every comment the driver
 *  named: its read is capped at the server's safe horizon, which an open transaction holds below
 *  the driver's writes. A pause between reads, never what decides the answer. */
const logPollMs = 200;
/** How long `watch` waits for its driver's written file once its feed has started. */
const writtenWithinMs = 60_000;
/** The longest delay a timer holds: Bun fires a longer one at once. */
const longestTimerMs = 2 ** 31 - 1;

function usage(): never {
  console.error(
    "usage: bun socket-access.ts doc <server> <header file> <document id> <schema version>\n" +
      "       bun socket-access.ts live <server> <header file> <ready file> <written file>\n" +
      "       bun socket-access.ts watch <server> <header file> <ready file> <written file> <seconds>\n" +
      "       bun socket-access.ts agent <server> <header file> <session id> <seconds>"
  );
  process.exit(2);
}

/** What the probe was asked: its mode, each with the arguments it reads, and how long the answer
 *  may take (`answerMs`), which a window adds to. */
type Call = { readonly answerMs: number } & (
  | { readonly mode: "doc"; readonly documentId: string; readonly schemaVersion: string }
  | { readonly mode: "live"; readonly readyFile: string; readonly writtenFile: string }
  | {
      readonly mode: "watch";
      readonly readyFile: string;
      readonly writtenFile: string;
      readonly windowMs: number;
    }
  | { readonly mode: "agent"; readonly sessionId: string; readonly windowMs: number }
);

/** A window in milliseconds from `seconds`: a whole number of seconds, at least one, and short
 *  enough that the answer's bound, `answerWithinMs`, `beyond` and the window, still fits a timer. */
function windowOf(seconds: string | undefined, beyond: number): number {
  const windowMs = Number(seconds) * 1000;
  if (!/^[1-9][0-9]*$/.test(seconds ?? "") || answerWithinMs + beyond + windowMs > longestTimerMs) {
    usage();
  }
  return windowMs;
}

const [mode, server, headerFile, third, fourth, fifth] = Bun.argv.slice(2);
if (
  server === undefined ||
  headerFile === undefined ||
  third === undefined ||
  fourth === undefined
) {
  usage();
}
const call: Call = (() => {
  switch (mode) {
    case "doc":
      return { answerMs: answerWithinMs, documentId: third, mode, schemaVersion: fourth };
    case "live":
      return { answerMs: answerWithinMs, mode, readyFile: third, writtenFile: fourth };
    case "watch": {
      // `watch` answers only after its driver's write and its window, so its bound adds both.
      const windowMs = windowOf(fifth, writtenWithinMs);
      return {
        answerMs: answerWithinMs + writtenWithinMs + windowMs,
        mode,
        readyFile: third,
        windowMs,
        writtenFile: fourth,
      };
    }
    case "agent": {
      const windowMs = windowOf(fourth, 0);
      return { answerMs: answerWithinMs + windowMs, mode, sessionId: third, windowMs };
    }
    default:
      return usage();
  }
})();
const url = new URL(server);
if (url.protocol !== "http:" && url.protocol !== "https:") usage();

const origin = url.origin;
const credential = (await Bun.file(headerFile).text()).trim();
const separator = credential.indexOf(":");
if (separator < 1) {
  console.error(`${headerFile} holds no "Name: value" line`);
  process.exit(2);
}
const headers: Record<string, string> = {
  [credential.slice(0, separator).trim()]: credential.slice(separator + 1).trim(),
  Origin: origin,
};
const path =
  call.mode === "doc"
    ? `/ws/doc/${encodeURIComponent(call.documentId)}?schema_version=${encodeURIComponent(call.schemaVersion)}`
    : "/ws/live";

/** A chunked body's text, once its last chunk has come, and otherwise undefined. A chunk's size
 *  counts bytes, so the body is read as bytes and decoded whole. */
function dechunked(body: Buffer): string | undefined {
  let rest = body;
  const chunks: Buffer[] = [];
  for (;;) {
    const line = rest.indexOf("\r\n");
    if (line < 0) return undefined;
    const size = Number.parseInt(rest.subarray(0, line).toString(), 16);
    if (Number.isNaN(size)) return undefined;
    if (size === 0) return Buffer.concat(chunks).toString();
    const end = line + 2 + size;
    if (rest.length < end + 2) return undefined;
    chunks.push(rest.subarray(line + 2, end));
    rest = rest.subarray(end + 2);
  }
}

/** The server's answer to a WebSocket upgrade of `path`: its status, and its body when it is not
 *  101. Asked over a connection of its own, TLS for an `https://` server, since only a raw read of
 *  the response sees a refusal. A refusal's body is read to its length, to its last chunk, or to
 *  the connection's close. */
async function upgradeAnswer(): Promise<{ status: number; body: string }> {
  const secure = url.protocol === "https:";
  const key = Buffer.from(crypto.getRandomValues(new Uint8Array(16))).toString("base64");
  const request = [
    `GET ${path} HTTP/1.1`,
    `Host: ${url.host}`,
    "Connection: Upgrade",
    "Upgrade: websocket",
    "Sec-WebSocket-Version: 13",
    `Sec-WebSocket-Key: ${key}`,
    ...Object.entries(headers).map(([name, value]) => `${name}: ${value}`),
    "",
    "",
  ].join("\r\n");
  const received = Promise.withResolvers<{ status: number; body: string }>();
  // Bytes, not text: a chunk's size and a Content-Length count bytes, and a character a read
  // splits in two decodes whole only once both halves have come.
  let raw = Buffer.alloc(0);
  const settle = (connection: { end(): void }) => {
    const head = raw.indexOf("\r\n\r\n");
    if (head < 0) return;
    const status = Number(raw.subarray(9, 12).toString());
    if (status === 101) {
      received.resolve({ status, body: "" });
      connection.end();
      return;
    }
    const fields = raw.subarray(0, head).toString();
    const length = /\r\ncontent-length: *(\d+)/i.exec(fields);
    const body = raw.subarray(head + 4);
    if (length?.[1] !== undefined && body.length >= Number(length[1])) {
      received.resolve({ status, body: body.toString() });
      connection.end();
      return;
    }
    const chunks = /\r\ntransfer-encoding: *chunked/i.test(fields) ? dechunked(body) : undefined;
    if (chunks !== undefined) {
      received.resolve({ status, body: chunks });
      connection.end();
    }
  };
  // The request is written once the connection is open, and over TLS that is once the handshake
  // is done: a write before it sends nothing, and the server, which waits for the request, never
  // answers. What a write could not take is written again when the socket drains.
  let unsent = Buffer.from(request);
  const send = (socket: { write(data: Buffer): number }) => {
    if (unsent.length > 0) unsent = unsent.subarray(Math.max(socket.write(unsent), 0));
  };
  const connection = await Bun.connect({
    // A bracketed IPv6 literal is the URL's spelling; the connect takes the address alone.
    hostname: url.hostname.replace(/^\[(.*)\]$/, "$1"),
    port: Number(url.port || (secure ? 443 : 80)),
    tls: secure,
    socket: {
      open: send,
      drain: send,
      data(socket, chunk) {
        raw = Buffer.concat([raw, chunk]);
        settle(socket);
      },
      close() {
        const head = raw.indexOf("\r\n\r\n");
        if (head < 0) received.reject(new Error(`the upgrade of ${path} closed with no answer`));
        else {
          received.resolve({
            status: Number(raw.subarray(9, 12).toString()),
            body: raw.subarray(head + 4).toString(),
          });
        }
      },
      error(_socket, error) {
        received.reject(error);
      },
    },
  });
  const timer = setTimeout(
    () =>
      received.reject(
        new Error(`no answer to the upgrade of ${path} within ${answerWithinMs / 1000} s`)
      ),
    answerWithinMs
  );
  try {
    return await received.promise;
  } finally {
    clearTimeout(timer);
    connection.end();
  }
}

/** The document socket's authentication answer. */
function documentAnswer(
  socket: WebSocket,
  documentId: string,
  schemaVersion: string
): Promise<string> {
  const answer = Promise.withResolvers<string>();
  const token = createEncoder();
  writeVarString(token, documentId);
  writeVarUint(token, authMessage);
  writeVarUint(token, tokenSubtype);
  writeVarString(token, schemaVersion);
  socket.addEventListener("open", () => socket.send(toUint8Array(token)));
  socket.addEventListener("message", (event) => {
    if (!(event.data instanceof ArrayBuffer)) return;
    const frame = createDecoder(new Uint8Array(event.data));
    readVarString(frame);
    if (readVarUint(frame) !== authMessage) return;
    const subtype = readVarUint(frame);
    const text = readVarString(frame);
    if (subtype === authenticatedSubtype) answer.resolve(`authenticated ${text}`);
    if (subtype === permissionDeniedSubtype) answer.resolve(`denied ${text}`);
  });
  socket.addEventListener("close", (event) =>
    answer.resolve(`closed ${event.code} ${event.reason}`)
  );
  return answer.promise;
}

/** The ids of the events GET /api/v1/events/log lets this caller read after `since`, as decimal
 *  strings, read once the log holds the `comment.created` event of every comment in `comments`:
 *  the log answers by the rule the event feed sends by (eventReadableSQL), capped at the safe
 *  horizon, so once it holds the driver's last writes it holds every event of theirs the feed owes
 *  this caller. While an open transaction holds the horizon below those writes the log answers
 *  without them, and it is read again. Any answer but 200 fails the probe. */
async function readableEventIds(since: string, comments: readonly string[]): Promise<Set<string>> {
  for (;;) {
    const ids = new Set<string>();
    const created = new Set<string>();
    let after = since;
    for (;;) {
      const response = await fetch(
        `${origin}/api/v1/events/log?after=${encodeURIComponent(after)}`,
        {
          headers,
        }
      );
      const body = await response.text();
      if (response.status !== 200) {
        throw new Error(
          `GET /api/v1/events/log?after=${after} answered ${response.status} ${body}`
        );
      }
      const page = JSON.parse(body) as {
        id: number | string;
        type?: string;
        payload?: { id?: unknown };
      }[];
      for (const event of page) {
        ids.add(String(event.id));
        if (event.type === "comment.created" && typeof event.payload?.id === "string") {
          created.add(event.payload.id);
        }
      }
      const last = page.at(-1);
      if (page.length < 1000 || last === undefined) break;
      after = String(last.id);
    }
    if (comments.every((comment) => created.has(comment))) return ids;
    await Bun.sleep(logPollMs);
  }
}

/** What the event feed sends this caller of the driver's writes, as the lines it printed: the
 *  feed starts with no resume, the ready file says it has, and once the written file has appeared
 *  the frames are read until the feed has sent every event readableEventIds lists, or the
 *  subscription ends. `settling` is told when the feed has started, so the caller's bound on the
 *  answer becomes the feed's (settleWithinMs). */
function liveAnswer(
  socket: WebSocket,
  readyFile: string,
  writtenFile: string,
  settling: () => void
): Promise<string> {
  const answer = Promise.withResolvers<string>();
  const lines: string[] = [];
  const sent = new Set<string>();
  let owed: Set<string> | undefined;
  const settled = () => answer.resolve(lines.join("\n"));
  const settledIfPaid = () => {
    if (owed !== undefined && [...owed].every((id) => sent.has(id))) settled();
  };
  socket.addEventListener("open", () =>
    socket.send(JSON.stringify({ type: "subscribe", sub: "e", view: { kind: "events" } }))
  );
  socket.addEventListener("message", (event) => {
    if (typeof event.data !== "string") return;
    const frame = JSON.parse(event.data) as {
      type?: string;
      sub?: string;
      id?: string;
      since?: string;
      event?: { id?: unknown };
    };
    if (frame.type === "heartbeat") return;
    if (frame.type === "event") {
      if (frame.id !== undefined) sent.add(String(frame.id));
      frame.id = "<event>";
      if (frame.event !== undefined) frame.event.id = "<event>";
    }
    lines.push(JSON.stringify(frame));
    if (frame.type === "events_start") {
      settling();
      const since = frame.since ?? "0";
      void (async () => {
        await Bun.write(readyFile, "ready\n");
        while (!(await Bun.file(writtenFile).exists())) await Bun.sleep(50);
        const comments = (await Bun.file(writtenFile).text())
          .split("\n")
          .filter((line) => line !== "");
        owed = await readableEventIds(since, comments);
        settledIfPaid();
      })().catch(answer.reject);
    }
    if (frame.type === "ended" && frame.sub === "e") settled();
    settledIfPaid();
  });
  socket.addEventListener("close", (event) => {
    lines.push(`closed ${event.code} ${event.reason}`);
    settled();
  });
  return answer.promise;
}

/** A feed frame's fields as the probe reads them; any may be absent. */
interface Frame {
  readonly type?: unknown;
  readonly reason?: unknown;
  readonly code?: unknown;
}

/** A feed frame the server wrote as `data`, or undefined for a frame that is no JSON object, which
 *  `watch` and `agent` keep as the server wrote it, as they keep every frame but a heartbeat. */
function frameOf(data: string): Frame | undefined {
  let frame: unknown;
  try {
    frame = JSON.parse(data);
  } catch {
    return undefined;
  }
  return typeof frame === "object" && frame !== null ? (frame as Frame) : undefined;
}

/** The line a probe prints for a subscription the server ended (`ended`, its reason) or refused
 *  (`error`, its code), the socket left open, or undefined for any other frame: either sends
 *  nothing more, so a window it falls in is not covered, as one a close falls in is not. */
function endedBy(frame: Frame | undefined): string | undefined {
  if (frame?.type === "ended") return `closed ended ${String(frame.reason)}`;
  if (frame?.type === "error") return `closed refused ${String(frame.code)}`;
  return undefined;
}

/** What a printed window's start is given: `open` starts the window (a second call does nothing),
 *  `failed` rejects the answer, and `answered` says the answer is in, after which neither acts. */
interface WindowStart {
  readonly open: () => void;
  readonly failed: (error: unknown) => void;
  readonly answered: () => boolean;
}

/** The frame loop `watch` and `agent` share: it sends `subscribe` once the socket opens, prints
 *  every frame but a heartbeat as the server wrote it the moment it comes, so a driver reads the
 *  feed while it runs and a probe stopped early has printed all it was sent, and answers
 *  `windowMs` after the window opens. `started` is told each printed frame, and opens the window
 *  when its mode says. A subscription the server ends or refuses on the open socket, or a socket
 *  that closes, is answered then, with the line that says so. Once the answer is in, a pending
 *  start opens nothing, so no wait keeps this process running past it. */
function printedFrames(
  socket: WebSocket,
  subscribe: Readonly<Record<string, unknown>>,
  windowMs: number,
  started: (frame: Frame | undefined, start: WindowStart) => void
): Promise<undefined> {
  const answer = Promise.withResolvers<undefined>();
  let done = false;
  let windowTimer: Timer | undefined;
  const settled = () => {
    done = true;
    clearTimeout(windowTimer);
    answer.resolve(undefined);
  };
  const start: WindowStart = {
    open: () => {
      if (!done) windowTimer ??= setTimeout(settled, windowMs);
    },
    failed: (error) => {
      done = true;
      clearTimeout(windowTimer);
      answer.reject(error);
    },
    answered: () => done,
  };
  socket.addEventListener("open", () => socket.send(JSON.stringify(subscribe)));
  socket.addEventListener("message", (event) => {
    if (done || typeof event.data !== "string") return;
    const frame = frameOf(event.data);
    if (frame?.type === "heartbeat") return;
    console.log(event.data);
    const ended = endedBy(frame);
    if (ended !== undefined) {
      console.log(ended);
      settled();
      return;
    }
    started(frame, start);
  });
  socket.addEventListener("close", (event) => {
    if (done) return;
    console.log(`closed ${event.code} ${event.reason}`);
    settled();
  });
  return answer.promise;
}

/** What the event feed sends this caller in a window (printedFrames): the feed starts with no
 *  resume, the ready file says it has, and the window opens once the written file appears after
 *  the first `events_start`. A driver that makes no written file within `writtenWithinMs`, or a
 *  ready file that cannot be written, rejects the answer, which exits 1. */
function watchAnswer(
  socket: WebSocket,
  readyFile: string,
  writtenFile: string,
  windowMs: number
): Promise<undefined> {
  let waiting = false;
  return printedFrames(
    socket,
    { type: "subscribe", sub: "e", view: { kind: "events" } },
    windowMs,
    (frame, start) => {
      // The window follows the first `events_start` alone: a second one starts no second wait.
      if (frame?.type !== "events_start" || waiting) return;
      waiting = true;
      void (async () => {
        await Bun.write(readyFile, "ready\n");
        const deadline = Date.now() + writtenWithinMs;
        while (!start.answered() && !(await Bun.file(writtenFile).exists())) {
          if (Date.now() > deadline) {
            throw new Error(`the written file did not appear within ${writtenWithinMs / 1000} s`);
          }
          await Bun.sleep(50);
        }
      })().then(start.open, start.failed);
    }
  );
}

/** What the agent view of `sessionId` sends this caller (printedFrames), until `windowMs` after
 *  its first frame. */
function agentAnswer(socket: WebSocket, sessionId: string, windowMs: number): Promise<undefined> {
  return printedFrames(
    socket,
    { type: "subscribe", sub: "a", view: { kind: "agent", session_id: sessionId } },
    windowMs,
    (_frame, start) => start.open()
  );
}

try {
  const upgrade = await upgradeAnswer();
  if (upgrade.status !== 101) {
    console.log(`http ${upgrade.status} ${upgrade.body}`);
  } else {
    const socket = new BunWebSocket(`${origin.replace(/^http/, "ws")}${path}`, { headers });
    socket.binaryType = "arraybuffer";
    let timer = setTimeout(() => {
      console.error(`no answer on ${path} within ${call.answerMs / 1000} s`);
      process.exit(1);
    }, call.answerMs);
    const settling = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        console.error(
          `${path} did not send what GET /api/v1/events/log owes within ${settleWithinMs / 1000} s`
        );
        process.exit(1);
      }, settleWithinMs);
    };
    // However the wait ends, `watch`'s rejection included, the bound and the socket stop with it:
    // either would keep this process running, and the bound would print a second failure.
    try {
      // `watch` and `agent` print their frames as they come, and answer nothing more.
      const answer = await (call.mode === "doc"
        ? documentAnswer(socket, call.documentId, call.schemaVersion)
        : call.mode === "live"
          ? liveAnswer(socket, call.readyFile, call.writtenFile, settling)
          : call.mode === "watch"
            ? watchAnswer(socket, call.readyFile, call.writtenFile, call.windowMs)
            : agentAnswer(socket, call.sessionId, call.windowMs));
      if (answer !== undefined) console.log(answer);
    } finally {
      clearTimeout(timer);
      socket.close();
    }
  }
} catch (error) {
  // An error can come with the socket still open and its bound still set: exit, not wait them out.
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
}
