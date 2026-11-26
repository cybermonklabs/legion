/** `bun issue-room-peer.ts <server> <cookie file> <schema version>`: the document socket peer of
 *  c-one-room and c-room-cutover, which lib-tree.sh's tree_peer_start runs as a coprocess. It holds
 *  document sockets, each by a name, as the person whose `Cookie` header the file holds, and
 *  answers each command it reads on stdin with one line on stdout. A socket speaks the Hocuspocus
 *  framing the browser's provider speaks: each frame starts with the document name the socket
 *  asked for, the token frame carries the schema version, a sync step 1 follows the server's
 *  Authenticated answer, and the server answers each update it is sent with one SyncStatus frame.
 *  It waits PEER_ANSWER_MS (60000 where unset) for a socket's answer.
 *    open NAME DOCUMENT       opens NAME on DOCUMENT and syncs it; a closed NAME reconnects keeping
 *                             its document, so its sync step 1 presents the state vector it held:
 *                             `opened NAME <scope> <fragment,...>`, or `refused NAME <why>`
 *    freeze NAME / thaw NAME  stops taking the room's updates into NAME, or applies those held
 *    sv NAME                  `sv NAME <base64 state vector>`
 *    fragments NAME           `fragments NAME <fragment,...>`, every shared type of NAME's
 *                             document but the marks map; `open` answers the same list
 *    append NAME FRAGMENT J   appends the block the JSON J describes - {type, id, text, attrs?} -
 *                             at the end of FRAGMENT and waits for the server's answer:
 *                             `appended NAME <1|0>`, or `unsent NAME` on a closed socket
 *    blocks NAME FRAGMENT     `blocks NAME <json>`: each element of FRAGMENT with a blockId, {id,
 *                             type, text}, in document order
 *    wait-close NAME SECONDS  `closed NAME <code> <reason>` once the socket closes, else
 *                             `open NAME`
 *    close NAME               `closed NAME`
 *    quit */
import * as decoding from "lib0/decoding";
import * as encoding from "lib0/encoding";
import * as Y from "yjs";

// Bun's WebSocket client takes request headers; the DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

const messageSync = 0;
const messageAuth = 2;
const messageSyncStatus = 8;
const syncStep1 = 0;
const syncStep2 = 1;
const syncUpdate = 2;
const authToken = 0;
const authDenied = 1;
const authAuthenticated = 2;
const answerWithinMs = Number(process.env.PEER_ANSWER_MS ?? "60000");

const [server, cookieFile, schemaVersion] = Bun.argv.slice(2);
if (server === undefined || cookieFile === undefined || schemaVersion === undefined) {
  console.error("usage: bun issue-room-peer.ts <server> <cookie file> <schema version>");
  process.exit(2);
}
const origin = new URL(server).origin;
const cookie = (await Bun.file(cookieFile).text()).trim();

type Peer = {
  doc: Y.Doc;
  asked: string;
  socket?: WebSocket;
  scope?: string;
  denied?: string;
  synced: boolean;
  frozen: boolean;
  held: Uint8Array[];
  sent: number;
  statuses: number[];
  closed?: string;
};
const peers = new Map<string, Peer>();

function frame(name: string, write: (encoder: encoding.Encoder) => void): Uint8Array {
  const encoder = encoding.createEncoder();
  encoding.writeVarString(encoder, name);
  write(encoder);
  return encoding.toUint8Array(encoder);
}

function sendSync(peer: Peer, subtype: number, payload: Uint8Array): boolean {
  if (peer.socket === undefined || peer.socket.readyState !== WebSocket.OPEN) return false;
  peer.socket.send(
    frame(peer.asked, (encoder) => {
      encoding.writeVarUint(encoder, messageSync);
      encoding.writeVarUint(encoder, subtype);
      encoding.writeVarUint8Array(encoder, payload);
    })
  );
  if (subtype !== syncStep1) peer.sent += 1;
  return true;
}

async function until(test: () => boolean, ms = answerWithinMs): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (!test()) {
    if (Date.now() > deadline) return false;
    await Bun.sleep(50);
  }
  return true;
}

function receive(peer: Peer, data: ArrayBuffer): void {
  const decoder = decoding.createDecoder(new Uint8Array(data));
  decoding.readVarString(decoder);
  const type = decoding.readVarUint(decoder);
  if (type === messageAuth) {
    const subtype = decoding.readVarUint(decoder);
    const text = decoding.readVarString(decoder);
    if (subtype === authAuthenticated) {
      peer.scope = text;
      sendSync(peer, syncStep1, Y.encodeStateVector(peer.doc));
    } else if (subtype === authDenied) {
      peer.denied = text;
    }
    return;
  }
  if (type === messageSyncStatus) {
    peer.statuses.push(decoding.readVarUint(decoder));
    return;
  }
  if (type !== messageSync) return;
  const subtype = decoding.readVarUint(decoder);
  const payload = decoding.readVarUint8Array(decoder);
  if (subtype === syncStep1) {
    sendSync(peer, syncStep2, Y.encodeStateAsUpdate(peer.doc, payload));
    return;
  }
  if (subtype === syncStep2 || subtype === syncUpdate) {
    if (peer.frozen) peer.held.push(payload);
    else Y.applyUpdate(peer.doc, payload, "server");
    if (subtype === syncStep2) peer.synced = true;
  }
}

async function open(name: string, asked: string): Promise<string> {
  let peer = peers.get(name);
  if (peer !== undefined && peer.closed === undefined) return `refused ${name} it is open already`;
  if (peer === undefined) {
    const doc = new Y.Doc();
    peer = { doc, asked, synced: false, frozen: false, held: [], sent: 0, statuses: [] };
    peers.set(name, peer);
    const held = peer;
    doc.on("update", (update: Uint8Array, source: unknown) => {
      if (source !== "server") sendSync(held, syncUpdate, update);
    });
  }
  Object.assign(peer, {
    asked,
    scope: undefined,
    denied: undefined,
    closed: undefined,
    synced: false,
    sent: 0,
    statuses: [],
  });
  const url = `${origin.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(asked)}?schema_version=${encodeURIComponent(schemaVersion)}`;
  const socket = new BunWebSocket(url, { headers: { Cookie: cookie, Origin: origin } });
  socket.binaryType = "arraybuffer";
  peer.socket = socket;
  const held = peer;
  socket.addEventListener("open", () =>
    socket.send(
      frame(asked, (encoder) => {
        encoding.writeVarUint(encoder, messageAuth);
        encoding.writeVarUint(encoder, authToken);
        encoding.writeVarString(encoder, schemaVersion);
      })
    )
  );
  socket.addEventListener("message", (event) => {
    if (event.data instanceof ArrayBuffer) receive(held, event.data);
  });
  socket.addEventListener("close", (event) => {
    held.closed = `${event.code} ${event.reason || "-"}`;
  });
  socket.addEventListener("error", () => {
    held.closed ??= "error -";
  });
  const settled = await until(
    () => held.synced || held.denied !== undefined || held.closed !== undefined
  );
  if (held.denied !== undefined) return `refused ${name} denied: ${held.denied}`;
  if (held.closed !== undefined) return `refused ${name} closed: ${held.closed}`;
  if (!settled) return `refused ${name} no sync within ${answerWithinMs / 1000} s`;
  await until(() => held.statuses.length >= held.sent);
  return `opened ${name} ${held.scope} ${fragments(held)}`;
}

// The fragments a socket's document holds: every shared type but the marks map (docs'
// marksMapName).
function fragments(peer: Peer): string {
  return [...peer.doc.share.keys()]
    .filter((name) => name !== "marks")
    .sort()
    .join(",");
}

function build(spec: {
  type: string;
  id: string;
  text: string;
  attrs?: Record<string, unknown>;
}): Y.XmlElement {
  const element = new Y.XmlElement(spec.type);
  element.setAttribute("blockId", spec.id);
  for (const [key, value] of Object.entries(spec.attrs ?? {})) {
    element.setAttribute(key, value as string);
  }
  if (spec.type === "paragraph" || spec.type === "heading") {
    element.insert(0, [new Y.XmlText(spec.text)]);
  } else {
    const paragraph = new Y.XmlElement("paragraph");
    paragraph.setAttribute("blockId", `${spec.id}-text`);
    paragraph.insert(0, [new Y.XmlText(spec.text)]);
    element.insert(0, [paragraph]);
  }
  return element;
}

function blocks(fragment: Y.XmlFragment): { id: string; type: string; text: string }[] {
  const found: { id: string; type: string; text: string }[] = [];
  const walk = (node: Y.XmlElement | Y.XmlFragment) => {
    for (const child of node.toArray()) {
      if (!(child instanceof Y.XmlElement)) continue;
      // A Yjs element's attribute in the peer's own in-memory document, never a page's state:
      // no page has to settle before this read, so one instant's answer is the answer.
      const id = child.getAttribute("blockId");
      if (typeof id === "string" && id !== "") {
        const text = child
          .toArray()
          .map((part) =>
            part instanceof Y.XmlText
              ? part.toString()
              : part instanceof Y.XmlElement
                ? part.toString()
                : ""
          )
          .join("");
        found.push({ id, type: child.nodeName, text: text.replace(/<[^>]*>/g, "") });
      }
      walk(child);
    }
  };
  walk(fragment);
  return found;
}

async function run(line: string): Promise<string> {
  const [command = "", name = "", ...rest] = line.trim().split(" ");
  const peer = peers.get(name);
  switch (command) {
    case "open":
      return open(name, rest[0] ?? "");
    case "quit":
      for (const held of peers.values()) held.socket?.close();
      process.exit(0);
  }
  if (peer === undefined) return `error ${name} no socket of that name`;
  switch (command) {
    case "freeze":
      peer.frozen = true;
      return `frozen ${name}`;
    case "thaw": {
      peer.frozen = false;
      for (const update of peer.held.splice(0)) Y.applyUpdate(peer.doc, update, "server");
      return `thawed ${name}`;
    }
    case "sv":
      return `sv ${name} ${Buffer.from(Y.encodeStateVector(peer.doc)).toString("base64")}`;
    case "fragments":
      return `fragments ${name} ${fragments(peer)}`;
    case "append": {
      const fragment = rest[0] ?? "";
      const spec = JSON.parse(rest.slice(1).join(" "));
      if (peer.closed !== undefined) return `unsent ${name}`;
      const before = peer.sent;
      peer.doc.transact(() => {
        const target = peer.doc.getXmlFragment(fragment);
        target.insert(target.length, [build(spec)]);
      }, "local");
      if (peer.sent === before) return `unsent ${name}`;
      const answered = await until(
        () => peer.statuses.length >= peer.sent || peer.closed !== undefined
      );
      if (!answered || peer.statuses.length < peer.sent) return `unsent ${name}`;
      return `appended ${name} ${peer.statuses[peer.statuses.length - 1]}`;
    }
    case "blocks":
      return `blocks ${name} ${JSON.stringify(blocks(peer.doc.getXmlFragment(rest[0] ?? "")))}`;
    case "wait-close": {
      await until(() => peer.closed !== undefined, Number(rest[0] ?? "10") * 1000);
      return peer.closed === undefined ? `open ${name}` : `closed ${name} ${peer.closed}`;
    }
    case "close":
      peer.socket?.close();
      await until(() => peer.closed !== undefined, 10_000);
      return `closed ${name}`;
    default:
      return `error ${name} unknown command ${command}`;
  }
}

for await (const line of console) {
  if (line.trim() === "") continue;
  try {
    console.log(await run(line));
  } catch (error) {
    console.log(`error - ${error instanceof Error ? error.message : String(error)}`);
  }
}
