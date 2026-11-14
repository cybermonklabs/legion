/** `bun socket-scope.ts <server> <document id> <schema version> <cookie file>`: opens one document
 *  socket on the server, as the person whose `Cookie` header the file holds, declaring the schema
 *  version, and prints the scope the server's authentication answers: `readonly` or `read-write`.
 *  It speaks the frames the browser's Hocuspocus provider speaks: the request names the version in
 *  `schema_version`, and the one token frame carries it again, as `connection.ts` sends it. The
 *  answer is read as docs/websocket_test.go TestHocuspocusAuthenticationCommunicatesSchemaReadOnly
 *  reads it: the first authentication frame, its subtype Authenticated, then its scope. A refusal,
 *  a close or no answer within ten seconds exits 1 and says which. e2e/acceptance/ci-check.sh runs
 *  it. */
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

const [server, documentId, schemaVersion, cookieFile] = Bun.argv.slice(2);
if (
  server === undefined ||
  documentId === undefined ||
  schemaVersion === undefined ||
  cookieFile === undefined
) {
  console.error("usage: bun socket-scope.ts <server> <document id> <schema version> <cookie file>");
  process.exit(2);
}

const origin = new URL(server).origin;
const cookie = (await Bun.file(cookieFile).text()).trim();
const url = `${origin.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(documentId)}?schema_version=${encodeURIComponent(schemaVersion)}`;

const token = createEncoder();
writeVarString(token, documentId);
writeVarUint(token, authMessage);
writeVarUint(token, tokenSubtype);
writeVarString(token, schemaVersion);

// Settles once: with the scope, or with why the socket gave none.
const answer = Promise.withResolvers<string>();
const socket = new BunWebSocket(url, { headers: { Cookie: cookie, Origin: origin } });
socket.binaryType = "arraybuffer";
const timer = setTimeout(
  () => answer.reject(new Error(`no authentication answer within ${answerWithinMs / 1000} s`)),
  answerWithinMs
);
socket.addEventListener("open", () => socket.send(toUint8Array(token)));
socket.addEventListener("message", (event) => {
  if (!(event.data instanceof ArrayBuffer)) return;
  const frame = createDecoder(new Uint8Array(event.data));
  readVarString(frame);
  if (readVarUint(frame) !== authMessage) return;
  const subtype = readVarUint(frame);
  const text = readVarString(frame);
  if (subtype === authenticatedSubtype) answer.resolve(text);
  if (subtype === permissionDeniedSubtype) {
    answer.reject(new Error(`the server denied the socket: ${text}`));
  }
});
socket.addEventListener("close", (event) =>
  answer.reject(
    new Error(`the socket closed before it was authenticated: ${event.code} ${event.reason}`)
  )
);
socket.addEventListener("error", () => answer.reject(new Error(`the socket to ${url} failed`)));

try {
  console.log(await answer.promise);
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
} finally {
  clearTimeout(timer);
  socket.close();
}
