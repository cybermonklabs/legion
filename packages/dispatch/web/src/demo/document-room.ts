// The ask card demo's document sockets (packages/dispatch/e2e/ask-card-demo/README.md):
// `/ws/doc/<artifact id>`, answered in the page as Dispatch's document service answers the
// Hocuspocus provider of features/doc/connection.ts. The socket is admitted read-write, its
// SyncStep1 is answered with the document its spec's markdown parses to, and each SyncStep2 and
// update it sends is applied and acknowledged with a SyncStatus, which the pending sync pairs
// with the frames it sent. A document starts again from its fixture on a reload.
import * as decoding from "lib0/decoding";
import * as encoding from "lib0/encoding";
import * as sync from "y-protocols/sync";
import * as Y from "yjs";
// The server's own block schema, which `GET /api/v1/schema/blocks` answers with. Its JSON reads
// each attribute's `kind` as a plain string, which the schema's type names one by one.
import blockSchema from "../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../api/types";
import { FRAGMENT_NAME } from "../features/doc/roots";
import { documentById } from "./document-fixtures";

// Hocuspocus's message types, and its Auth message's own (`@hocuspocus/common`).
const SYNC = 0;
const AUTH = 2;
const STATELESS = 5;
const SYNC_STATUS = 8;
const AUTH_TOKEN = 0;
const AUTHENTICATED = 2;
/** Shorter than the provider's 30 s silence check, which closes a socket that hears nothing. */
const KEEPALIVE_MS = 10_000;

/** Each document's text, built once per page from its fixture. */
const documents = new Map<string, Promise<Y.Doc>>();

async function buildDocument(artifactId: string, markdown: string): Promise<Y.Doc> {
  const [{ createHeadlessProof }, { prosemirrorToYXmlFragment }] = await Promise.all([
    import("@legion/proof-editor/headless"),
    import("y-prosemirror"),
  ]);
  // The same block ids on every load, so a link to a block of the demo keeps naming it.
  let minted = 0;
  const engine = await createHeadlessProof({
    blockId: () => {
      minted += 1;
      return `${artifactId}-block-${minted}`;
    },
    blockSchema: blockSchema as unknown as BlockSchema,
  });
  const doc = new Y.Doc();
  prosemirrorToYXmlFragment(engine.parseMarkdown(markdown), doc.getXmlFragment(FRAGMENT_NAME));
  return doc;
}

function frame(name: string, type: number, write: (encoder: encoding.Encoder) => void): Uint8Array {
  const encoder = encoding.createEncoder();
  encoding.writeVarString(encoder, name);
  encoding.writeVarUint(encoder, type);
  write(encoder);
  return encoding.toUint8Array(encoder);
}

/** The document service's side of one socket. */
export interface DocumentSocket {
  receive(frame: Uint8Array): void;
  close(): void;
}

/** The document service's side of a socket opened at `url`, which writes its frames to the page
 *  through `send`; undefined where `url` names no document socket of a fixture. */
export function openDocumentSocket(
  url: string,
  send: (frame: Uint8Array) => void
): DocumentSocket | undefined {
  const match = /\/ws\/doc\/([^/?#]+)/.exec(url);
  const artifactId = match === null ? undefined : decodeURIComponent(match[1] ?? "");
  const fixture = artifactId === undefined ? undefined : documentById(artifactId);
  if (artifactId === undefined || fixture === undefined) return undefined;
  let built = documents.get(artifactId);
  if (built === undefined) {
    built = buildDocument(artifactId, fixture.markdown);
    documents.set(artifactId, built);
  }
  const document = built;
  // This socket's updates, told from another socket's on the same document, which it is sent.
  const origin = {};
  const forward = (update: Uint8Array, from: unknown) => {
    if (from !== origin)
      send(frame(artifactId, SYNC, (encoder) => sync.writeUpdate(encoder, update)));
  };
  void document.then((doc) => doc.on("update", forward));
  const keepalive = setInterval(
    () => send(frame(artifactId, STATELESS, (encoder) => encoding.writeVarString(encoder, ""))),
    KEEPALIVE_MS
  );

  const answer = (doc: Y.Doc, bytes: Uint8Array) => {
    const decoder = decoding.createDecoder(bytes);
    const name = decoding.readVarString(decoder);
    const type = decoding.readVarUint(decoder);
    if (type === AUTH) {
      if (decoding.readVarUint(decoder) !== AUTH_TOKEN) return;
      send(
        frame(name, AUTH, (encoder) => {
          encoding.writeVarUint(encoder, AUTHENTICATED);
          encoding.writeVarString(encoder, "read-write");
        })
      );
      return;
    }
    // Awareness has no other reader in the page, so only the document's own frames are answered.
    if (type !== SYNC) return;
    const reply = encoding.createEncoder();
    encoding.writeVarString(reply, name);
    encoding.writeVarUint(reply, SYNC);
    if (sync.readSyncMessage(decoder, reply, doc, origin) === sync.messageYjsSyncStep1) {
      send(encoding.toUint8Array(reply));
      send(frame(name, SYNC, (encoder) => sync.writeSyncStep1(encoder, doc)));
      return;
    }
    send(frame(name, SYNC_STATUS, (encoder) => encoding.writeVarUint(encoder, 1)));
  };
  // Frames are answered in the order they came, once the document is built.
  let answered: Promise<unknown> = document;
  return {
    receive(bytes) {
      answered = answered.then(async () => answer(await document, bytes));
    },
    close() {
      clearInterval(keepalive);
      void document.then((doc) => doc.off("update", forward));
    },
  };
}
