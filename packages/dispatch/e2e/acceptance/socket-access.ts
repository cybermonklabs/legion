/** `bun socket-access.ts doc <server> <header file> <document id> <schema version>` and
 *  `bun socket-access.ts live <server> <header file> <ready file> <written file>`: opens one of
 *  Dispatch's two sockets as the caller whose credential the header file holds - one
 *  `Name: value` line, a `Cookie` or an `Authorization` - and prints what that caller is answered,
 *  in one form for every outcome, so a driver compares two callers' answers, or the answers to two
 *  ids, byte for byte.
 *
 *  - An upgrade the server refuses prints `http <status> <body>`. Bun's WebSocket reports a
 *    refused upgrade as an error with no status, so the upgrade is asked once over a plain
 *    connection first, and the socket is opened only when that answer is 101.
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
 *
 *  Exit 0 for any answer, a refusal included; 1 when the server could not be reached, gave no
 *  answer within ten seconds, or (live) did not send what its log owes within three minutes of
 *  the `events_start`; 2 for a wrong call. e2e/acceptance/project-access.sh and
 *  project-access-blob.sh run it. */
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

function usage(): never {
  console.error(
    "usage: bun socket-access.ts doc <server> <header file> <document id> <schema version>\n" +
      "       bun socket-access.ts live <server> <header file> <ready file> <written file>"
  );
  process.exit(2);
}

const [mode, server, headerFile, third, fourth] = Bun.argv.slice(2);
if (server === undefined || headerFile === undefined) usage();
if (mode !== "doc" && mode !== "live") usage();
if (third === undefined || fourth === undefined) usage();
const [documentId, schemaVersion] = mode === "doc" ? [third, fourth] : [undefined, undefined];
const [readyFile, writtenFile] = mode === "live" ? [third, fourth] : [undefined, undefined];

const origin = new URL(server).origin;
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
  mode === "doc"
    ? `/ws/doc/${encodeURIComponent(documentId ?? "")}?schema_version=${encodeURIComponent(schemaVersion ?? "")}`
    : "/ws/live";

/** The server's answer to a WebSocket upgrade of `path`: its status, and its body when it is not
 *  101. Asked over a plain connection, since only a raw read of the response sees a refusal. */
async function upgradeAnswer(): Promise<{ status: number; body: string }> {
  const url = new URL(server ?? "");
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
  let raw = "";
  const settle = (connection: { end(): void }) => {
    const head = raw.indexOf("\r\n\r\n");
    if (head < 0) return;
    const status = Number(raw.slice(9, 12));
    if (status === 101) {
      received.resolve({ status, body: "" });
      connection.end();
      return;
    }
    const length = /\r\ncontent-length: *(\d+)/i.exec(raw.slice(0, head));
    const body = raw.slice(head + 4);
    if (length?.[1] !== undefined && Buffer.byteLength(body) >= Number(length[1])) {
      received.resolve({ status, body });
      connection.end();
    }
  };
  const connection = await Bun.connect({
    hostname: url.hostname,
    port: Number(url.port),
    socket: {
      data(socket, chunk) {
        raw += chunk.toString();
        settle(socket);
      },
      close() {
        const head = raw.indexOf("\r\n\r\n");
        if (head < 0) received.reject(new Error(`the upgrade of ${path} closed with no answer`));
        else received.resolve({ status: Number(raw.slice(9, 12)), body: raw.slice(head + 4) });
      },
      error(_socket, error) {
        received.reject(error);
      },
    },
  });
  connection.write(request);
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
function documentAnswer(socket: WebSocket): Promise<string> {
  const answer = Promise.withResolvers<string>();
  const token = createEncoder();
  writeVarString(token, documentId ?? "");
  writeVarUint(token, authMessage);
  writeVarUint(token, tokenSubtype);
  writeVarString(token, schemaVersion ?? "");
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
function liveAnswer(socket: WebSocket, settling: () => void): Promise<string> {
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
        await Bun.write(readyFile ?? "", "ready\n");
        while (!(await Bun.file(writtenFile ?? "").exists())) await Bun.sleep(50);
        const comments = (await Bun.file(writtenFile ?? "").text())
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

try {
  const upgrade = await upgradeAnswer();
  if (upgrade.status !== 101) {
    console.log(`http ${upgrade.status} ${upgrade.body}`);
  } else {
    const socket = new BunWebSocket(`${origin.replace(/^http/, "ws")}${path}`, { headers });
    socket.binaryType = "arraybuffer";
    let timer = setTimeout(() => {
      console.error(`no answer on ${path} within ${answerWithinMs / 1000} s`);
      process.exit(1);
    }, answerWithinMs);
    const settling = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        console.error(
          `${path} did not send what GET /api/v1/events/log owes within ${settleWithinMs / 1000} s`
        );
        process.exit(1);
      }, settleWithinMs);
    };
    const answer = await (mode === "doc" ? documentAnswer(socket) : liveAnswer(socket, settling));
    clearTimeout(timer);
    console.log(answer);
    socket.close();
  }
} catch (error) {
  // An error can come with the socket still open and its bound still set: exit, not wait them out.
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
}
