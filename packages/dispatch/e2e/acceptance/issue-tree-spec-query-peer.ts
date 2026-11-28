/** `bun issue-tree-spec-query-peer.ts <server> <cookie file> <schema version> <document> <edits>`:
 *  the one write of an issue's room that c-tree-page's driver, issue-tree-spec-query.sh, makes as
 *  the person whose `Cookie` header the file holds. It opens one document socket on DOCUMENT, a
 *  converted document's id, which the server admits to its issue's room, and speaks the Hocuspocus
 *  framing the browser's provider speaks, as issue-room-peer.ts does: the token frame carries the
 *  schema version, a sync step 1 follows the server's Authenticated answer, and the room's sync
 *  step 2 brings the room's document, every document's fragment of the issue's room in it.
 *  EDITS is a JSON list of {fragment, block, find, with}: in the fragment named fragment (a
 *  document's id), the element whose blockId is block holds the text find, and the edit changes
 *  only the part of find that with does not share at its start and at its end, so a sentence that
 *  differs by one word changes by that word. Every edit runs in one Yjs transaction, so the socket
 *  sends the room one update, which the server answers with one SyncStatus frame. It prints one
 *  line and exits:
 *    `wrote <edits> edits in <updates> update(s) answered <status>`, exit 0, once the server
 *    answered the update;
 *    `error <why>`, exit 1, otherwise, before any edit is sent where an edit finds nothing.
 *  It waits PEER_ANSWER_MS (60000 where unset) for each answer of the server. */
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

type Edit = { fragment: string; block: string; find: string; with: string };

const [server, cookieFile, schemaVersion, documentID, editsJSON] = Bun.argv.slice(2);
if (
  server === undefined ||
  cookieFile === undefined ||
  schemaVersion === undefined ||
  documentID === undefined ||
  editsJSON === undefined
) {
  console.error(
    "usage: bun issue-tree-spec-query-peer.ts <server> <cookie file> <schema version> <document> <edits>"
  );
  process.exit(2);
}
const edits = JSON.parse(editsJSON) as Edit[];
const origin = new URL(server).origin;
const cookie = (await Bun.file(cookieFile).text()).trim();

const doc = new Y.Doc();
// What the socket has heard and sent: its scope once Authenticated, the reason of a denial, the
// close, whether the room's sync step 2 arrived, the count of updates it sent, the room's sync
// steps 1 it answers once synced, and each SyncStatus the server answered.
const peer: {
  socket?: WebSocket;
  scope?: string;
  denied?: string;
  closed?: string;
  synced: boolean;
  sent: number;
  asks: Uint8Array[];
  statuses: number[];
} = { synced: false, sent: 0, asks: [], statuses: [] };

function finish(line: string, code: number): never {
  console.log(line);
  peer.socket?.close();
  process.exit(code);
}

function sendSync(subtype: number, payload: Uint8Array): void {
  if (peer.socket === undefined || peer.socket.readyState !== WebSocket.OPEN) return;
  const encoder = encoding.createEncoder();
  encoding.writeVarString(encoder, documentID);
  encoding.writeVarUint(encoder, messageSync);
  encoding.writeVarUint(encoder, subtype);
  encoding.writeVarUint8Array(encoder, payload);
  peer.socket.send(encoding.toUint8Array(encoder));
  if (subtype !== syncStep1) peer.sent += 1;
}

async function until(test: () => boolean): Promise<boolean> {
  const deadline = Date.now() + answerWithinMs;
  while (!test()) {
    if (Date.now() > deadline) return false;
    await Bun.sleep(50);
  }
  return true;
}

function receive(data: ArrayBuffer): void {
  const decoder = decoding.createDecoder(new Uint8Array(data));
  decoding.readVarString(decoder);
  const type = decoding.readVarUint(decoder);
  if (type === messageAuth) {
    const subtype = decoding.readVarUint(decoder);
    const text = decoding.readVarString(decoder);
    if (subtype === authAuthenticated) {
      peer.scope = text;
      sendSync(syncStep1, Y.encodeStateVector(doc));
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
    if (peer.synced) sendSync(syncStep2, Y.encodeStateAsUpdate(doc, payload));
    else peer.asks.push(payload);
    return;
  }
  if (subtype === syncStep2 || subtype === syncUpdate) {
    Y.applyUpdate(doc, payload, "server");
    if (subtype === syncStep2) {
      peer.synced = true;
      for (const ask of peer.asks.splice(0)) sendSync(syncStep2, Y.encodeStateAsUpdate(doc, ask));
    }
  }
}

// The element of fragment whose blockId is block, at any depth.
function element(node: Y.XmlElement | Y.XmlFragment, block: string): Y.XmlElement | undefined {
  for (const child of node.toArray()) {
    if (!(child instanceof Y.XmlElement)) continue;
    if (child.getAttribute("blockId") === block) return child;
    const found = element(child, block);
    if (found !== undefined) return found;
  }
  return undefined;
}

// Every text under node, in document order.
function texts(node: Y.XmlElement): Y.XmlText[] {
  const found: Y.XmlText[] = [];
  for (const child of node.toArray()) {
    if (child instanceof Y.XmlText) found.push(child);
    else if (child instanceof Y.XmlElement) found.push(...texts(child));
  }
  return found;
}

// A text's characters as Yjs counts them, an embed one placeholder character.
function plain(text: Y.XmlText): string {
  return (text.toDelta() as { insert: unknown }[])
    .map((op) => (typeof op.insert === "string" ? op.insert : "\uFFFC"))
    .join("");
}

type Change = { text: Y.XmlText; at: number; remove: number; insert: string };

// The change edit makes: where its text holds find, the part of find that with does not share at
// its start and at its end.
function changeOf(edit: Edit): Change | string {
  if (!doc.share.has(edit.fragment)) {
    return `the room holds no fragment ${edit.fragment}: it holds ${[...doc.share.keys()].sort().join(",")}`;
  }
  const block = element(doc.getXmlFragment(edit.fragment), edit.block);
  if (block === undefined) return `the fragment ${edit.fragment} holds no element ${edit.block}`;
  for (const text of texts(block)) {
    const index = plain(text).indexOf(edit.find);
    if (index < 0) continue;
    let start = 0;
    while (
      start < edit.find.length &&
      start < edit.with.length &&
      edit.find[start] === edit.with[start]
    ) {
      start++;
    }
    let end = 0;
    while (
      end < edit.find.length - start &&
      end < edit.with.length - start &&
      edit.find[edit.find.length - 1 - end] === edit.with[edit.with.length - 1 - end]
    ) {
      end++;
    }
    return {
      text,
      at: index + start,
      remove: edit.find.length - start - end,
      insert: edit.with.slice(start, edit.with.length - end),
    };
  }
  return `the element ${edit.block} of ${edit.fragment} holds no text "${edit.find}"`;
}

doc.on("update", (update: Uint8Array, source: unknown) => {
  if (source !== "server") sendSync(syncUpdate, update);
});
const url = `${origin.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(documentID)}?schema_version=${encodeURIComponent(schemaVersion)}`;
const opened = new BunWebSocket(url, { headers: { Cookie: cookie, Origin: origin } });
opened.binaryType = "arraybuffer";
peer.socket = opened;
opened.addEventListener("open", () => {
  const encoder = encoding.createEncoder();
  encoding.writeVarString(encoder, documentID);
  encoding.writeVarUint(encoder, messageAuth);
  encoding.writeVarUint(encoder, authToken);
  encoding.writeVarString(encoder, schemaVersion);
  opened.send(encoding.toUint8Array(encoder));
});
opened.addEventListener("message", (event) => {
  if (event.data instanceof ArrayBuffer) receive(event.data);
});
opened.addEventListener("close", (event) => {
  peer.closed = `${event.code} ${event.reason || "-"}`;
});
opened.addEventListener("error", () => {
  peer.closed ??= "error -";
});

if (!(await until(() => peer.synced || peer.denied !== undefined || peer.closed !== undefined))) {
  finish(`error no sync of ${documentID} within ${answerWithinMs / 1000} s`, 1);
}
if (peer.denied !== undefined) {
  finish(`error the socket on ${documentID} was denied: ${peer.denied}`, 1);
}
if (!peer.synced) {
  finish(`error the socket on ${documentID} closed before the room synced: ${peer.closed}`, 1);
}
if (peer.scope !== "read-write") {
  finish(`error the socket on ${documentID} is admitted ${peer.scope}, not read-write`, 1);
}
// The room's own sync step 1, which the socket answers once synced: wait for the server's answer
// to each frame sent so far, so the count below is the edits' alone.
await until(() => peer.statuses.length >= peer.sent || peer.closed !== undefined);
const changes: Change[] = [];
for (const edit of edits) {
  const change = changeOf(edit);
  if (typeof change === "string") finish(`error ${change}`, 1);
  changes.push(change);
}
const before = peer.sent;
doc.transact(() => {
  for (const change of changes) {
    change.text.delete(change.at, change.remove);
    change.text.insert(change.at, change.insert);
  }
}, "local");
const updates = peer.sent - before;
if (updates !== 1) {
  finish(`error the ${changes.length} edits went out in ${updates} updates, want one`, 1);
}
if (
  !(await until(() => peer.statuses.length >= peer.sent || peer.closed !== undefined)) ||
  peer.statuses.length < peer.sent
) {
  finish(
    `error the server answered no SyncStatus to the update within ${answerWithinMs / 1000} s: ${peer.closed ?? "open"}`,
    1
  );
}
finish(
  `wrote ${changes.length} edits in ${updates} update(s) answered ${peer.statuses[peer.statuses.length - 1]}`,
  0
);
