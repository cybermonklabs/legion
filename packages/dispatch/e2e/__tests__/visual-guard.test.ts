import { expect, test } from "bun:test";
import {
  createEncoder,
  toUint8Array,
  writeVarString,
  writeVarUint,
  writeVarUint8Array,
} from "lib0/encoding";
import * as Y from "yjs";

import { documentWrite, isSeededReadMark } from "../visual-guard";

// The visual review's write guard (e2e/visual-guard.ts) decides from a request's method, path and
// body, and from a document frame's bytes, which writes it stops. These rows hold those two
// decisions on frames and requests built the way the page builds them, and the controls that a
// write passes for nothing: a read mark with any other field, and a sync reply carrying structs.

/** A frame as the page's Hocuspocus provider sends it: the document's name, the message type,
 *  then a y-protocols sync message of `subtype` carrying `payload`. */
function frame(document: string, messageType: number, subtype: number, payload: Uint8Array) {
  const encoder = createEncoder();
  writeVarString(encoder, document);
  writeVarUint(encoder, messageType);
  writeVarUint(encoder, subtype);
  writeVarUint8Array(encoder, payload);
  return toUint8Array(encoder);
}

test("the two read marks the seed writes pass, and nothing else on their routes", () => {
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { read_replies: ["r-1"] })).toBe(
    true
  );
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", { version: 3 })).toBe(true);

  // A pin, a clear or a read-through on the same route is a write the seed never made.
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { pinned: true })).toBe(false);
  expect(
    isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", {
      read_replies: ["r-1"],
      read_through: "2026-11-23T00:00:00Z",
    })
  ).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", {})).toBe(false);
  // Another method, another route, or a body that is no object.
  expect(isSeededReadMark("POST", "/api/v1/me/documents/d-1/read", { version: 3 })).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/threads/t-1/read", { read_through: "x" })).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", undefined)).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", [3])).toBe(false);
});

test("a Yjs update and a sync reply carrying the page's structs are writes; a bare sync reply is not", () => {
  const doc = new Y.Doc();
  doc.getText("t").insert(0, "typed on the page");
  const update = Y.encodeStateAsUpdate(doc);
  expect(documentWrite(frame("spec", 0, 2, update))).toBe(
    `a Yjs update to document spec (${update.length} bytes)`
  );
  expect(documentWrite(frame("spec", 0, 1, update))).toBe(
    "a sync reply to document spec that carries structs of 1 client(s)"
  );

  // The reply a page with no change of its own sends: the server's whole state known, so no
  // struct, only the document's delete set, which every Yjs update carries whole.
  const server = new Y.Doc();
  server.getText("t").insert(0, "seeded text");
  server.getText("t").delete(0, 7);
  const page = new Y.Doc();
  Y.applyUpdate(page, Y.encodeStateAsUpdate(server));
  const reply = Y.encodeStateAsUpdate(page, Y.encodeStateVector(server));
  expect(reply.length).toBeGreaterThan(2);
  expect(documentWrite(frame("spec", 0, 1, reply))).toBeUndefined();

  // The page's own reads and the rest of the protocol pass: its state vector (sync step 1),
  // awareness (type 1) and authentication (type 2).
  expect(documentWrite(frame("spec", 0, 0, Y.encodeStateVector(page)))).toBeUndefined();
  expect(documentWrite(frame("spec", 1, 0, new Uint8Array([1, 2, 3])))).toBeUndefined();
  expect(documentWrite(frame("spec", 2, 0, new Uint8Array()))).toBeUndefined();
});

test("a document frame the guard cannot read is reported rather than let through", () => {
  expect(documentWrite(new Uint8Array([0xff]))).toMatch(
    /^a document frame the guard could not read/
  );
});
