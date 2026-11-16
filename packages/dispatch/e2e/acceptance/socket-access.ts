/** `bun socket-access.ts doc <server> <header file> <document id> <schema version>` and
 *  `bun socket-access.ts live <server> <header file>`: opens one of Dispatch's two sockets as the
 *  caller whose credential the header file holds - one `Name: value` line, a `Cookie` or an
 *  `Authorization` - and prints what that caller is answered, in one form for every outcome, so a
 *  driver compares two callers' answers, or the answers to two ids, byte for byte.
 *
 *  - An upgrade the server refuses prints `http <status> <body>`. Bun's WebSocket reports a
 *    refused upgrade as an error with no status, so the upgrade is asked once over a plain
 *    connection first, and the socket is opened only when that answer is 101.
 *  - `doc` sends the one token frame the browser's Hocuspocus provider sends (as socket-scope.ts
 *    does) and prints `authenticated <scope>` or `denied <reason>`, or `closed <code> <reason>`
 *    for a socket closed before either.
 *  - `live` subscribes to the event feed from the log's first event and prints the
 *    `events_start` frame and then every event frame as the server sent it, one per line, until
 *    no frame has arrived for `quietMs`: the replay of the whole log as this caller may read it.
 *
 *  Exit 0 for any answer, a refusal included; 1 when the server could not be reached or gave no
 *  answer within ten seconds; 2 for a wrong call. e2e/acceptance/project-access.sh runs it. */
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
/** How long the event feed must be quiet before its replay is taken as whole. */
const quietMs = 1_500;

function usage(): never {
  console.error(
    "usage: bun socket-access.ts doc <server> <header file> <document id> <schema version>\n" +
      "       bun socket-access.ts live <server> <header file>"
  );
  process.exit(2);
}

const [mode, server, headerFile, documentId, schemaVersion] = Bun.argv.slice(2);
if (server === undefined || headerFile === undefined) usage();
if (mode === "doc" && (documentId === undefined || schemaVersion === undefined)) usage();
if (mode !== "doc" && mode !== "live") usage();

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

/** The event feed's replay from the log's first event, as the lines it printed. */
function liveAnswer(socket: WebSocket): Promise<string> {
  const answer = Promise.withResolvers<string>();
  const lines: string[] = [];
  let quiet: Timer | undefined;
  const settled = () => answer.resolve(lines.join("\n"));
  socket.addEventListener("open", () =>
    socket.send(
      JSON.stringify({
        type: "subscribe",
        sub: "e",
        view: { kind: "events" },
        resume: { since: "0" },
      })
    )
  );
  socket.addEventListener("message", (event) => {
    if (typeof event.data !== "string") return;
    const frame = JSON.parse(event.data) as { type?: string };
    if (frame.type === "heartbeat") return;
    lines.push(event.data);
    clearTimeout(quiet);
    quiet = setTimeout(settled, quietMs);
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
    const timer = setTimeout(() => {
      console.error(`no answer on ${path} within ${answerWithinMs / 1000} s`);
      process.exit(1);
    }, answerWithinMs);
    const answer = await (mode === "doc" ? documentAnswer(socket) : liveAnswer(socket));
    clearTimeout(timer);
    console.log(answer);
    socket.close();
  }
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}
