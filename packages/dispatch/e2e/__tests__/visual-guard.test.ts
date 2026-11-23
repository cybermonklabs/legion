import { expect, test } from "bun:test";
import {
  createEncoder,
  toUint8Array,
  writeVarString,
  writeVarUint,
  writeVarUint8Array,
} from "lib0/encoding";
import * as Y from "yjs";

import { documentWrite, followServer, isSeededReadMark, unlistedShotReads } from "../visual-guard";

// The visual review's write guard (e2e/visual-guard.ts) decides from a request's method, path and
// body, and from a document frame's bytes read against the server's document, which writes it
// stops. These rows hold those decisions on frames and requests built the way the page builds
// them, and the controls that a write passes for nothing: a read mark with any other field, a
// sync reply carrying structs, and an edit beside a derived attribute.

/** A frame as the page's Hocuspocus provider and the server send it: the document's name, the
 *  message type, then a y-protocols sync message of `subtype` carrying `payload`. */
function frame(document: string, messageType: number, subtype: number, payload: Uint8Array) {
  const encoder = createEncoder();
  writeVarString(encoder, document);
  writeVarUint(encoder, messageType);
  writeVarUint(encoder, subtype);
  writeVarUint8Array(encoder, payload);
  return toUint8Array(encoder);
}

/** The server's document as the page loads it: a heading, a paragraph and an ordered list. */
function seeded(): Y.Doc {
  const server = new Y.Doc();
  const heading = new Y.XmlElement("heading");
  heading.setAttribute("level", "2");
  heading.insert(0, [new Y.XmlText("Plan")]);
  const paragraph = new Y.XmlElement("paragraph");
  paragraph.insert(0, [new Y.XmlText("seeded text")]);
  const list = new Y.XmlElement("ordered_list");
  list.insert(0, [new Y.XmlElement("list_item")]);
  server.getXmlFragment("prosemirror").insert(0, [heading, paragraph, list]);
  return server;
}

/** The page's copy of the server's document, and the guard's shadow of it after the server's
 *  sync reply reached the page. */
function loaded(server: Y.Doc): { page: Y.Doc; shadow: Y.Doc } {
  const shadow = new Y.Doc();
  followServer(frame("spec", 0, 1, Y.encodeStateAsUpdate(server)), shadow);
  const page = new Y.Doc();
  Y.applyUpdate(page, Y.encodeStateAsUpdate(server));
  return { page, shadow };
}

/** What the page sends for `change`: the update its own transaction makes. */
function updateOf(page: Y.Doc, change: (fragment: Y.XmlFragment) => void): Uint8Array {
  const before = Y.encodeStateVector(page);
  page.transact(() => change(page.getXmlFragment("prosemirror")));
  return Y.encodeStateAsUpdate(page, before);
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

test("a typed edit is a write, as an update and inside a sync reply", () => {
  const { page, shadow } = loaded(seeded());
  const typed = updateOf(page, (fragment) => {
    ((fragment.get(1) as Y.XmlElement).get(0) as Y.XmlText).insert(0, "typed ");
  });
  expect(documentWrite(frame("spec", 0, 2, typed), shadow)).toEqual({
    write: `a Yjs update to document spec (${typed.length} bytes)`,
  });
  expect(documentWrite(frame("spec", 0, 1, typed), shadow)).toEqual({
    write: "a sync reply to document spec that carries structs of 1 client(s)",
  });
});

test("the attributes the editor derives on load are dropped as derived, not named; an edit beside them is a write", () => {
  const { page, shadow } = loaded(seeded());
  const stamp = (fragment: Y.XmlFragment) => {
    (fragment.get(0) as Y.XmlElement).setAttribute("id", "plan");
    const item = (fragment.get(2) as Y.XmlElement).get(0) as Y.XmlElement;
    item.setAttribute("label", "1.");
    item.setAttribute("listType", "ordered");
  };
  const stamped = updateOf(page, stamp);
  expect(documentWrite(frame("spec", 0, 2, stamped), shadow)).toBe("derived");
  expect(documentWrite(frame("spec", 0, 1, stamped), shadow)).toBe("derived");

  // The editor's next stamp builds on the one the guard dropped, which the server never got: it
  // reads against the stamps the guard kept, so it is derived too, not a frame the guard cannot
  // place.
  const restamped = updateOf(page, (fragment) => {
    ((fragment.get(2) as Y.XmlElement).get(0) as Y.XmlElement).setAttribute("label", "2.");
  });
  expect(documentWrite(frame("spec", 0, 2, restamped), shadow)).toBe("derived");

  // A heading's level is a person's choice, as is any text: either beside the stamp is a write.
  const relevelled = updateOf(page, (fragment) => {
    (fragment.get(0) as Y.XmlElement).setAttribute("level", "3");
  });
  expect(documentWrite(frame("spec", 0, 2, relevelled), shadow)).toMatchObject({
    write: expect.stringMatching(/^a Yjs update to document spec/u),
  });
  const { page: other, shadow: otherShadow } = loaded(seeded());
  const stampAndType = updateOf(other, (fragment) => {
    stamp(fragment);
    ((fragment.get(1) as Y.XmlElement).get(0) as Y.XmlText).insert(0, "typed ");
  });
  expect(documentWrite(frame("spec", 0, 2, stampAndType), otherShadow)).toMatchObject({
    write: expect.stringMatching(/^a Yjs update to document spec/u),
  });

  // Read against a shadow that never heard the server, the stamp's parent is unknown: the guard
  // cannot say it changes only what the editor derives, so it is a write.
  expect(documentWrite(frame("spec", 0, 2, stamped), new Y.Doc())).toMatchObject({
    write: expect.stringMatching(/^a Yjs update to document spec/u),
  });
});

test("a bare sync reply, the page's reads and the rest of the protocol pass", () => {
  // The reply a page with no change of its own sends: the server's whole state known, so no
  // struct, only the document's delete set, which every Yjs update carries whole.
  const server = seeded();
  ((server.getXmlFragment("prosemirror").get(1) as Y.XmlElement).get(0) as Y.XmlText).delete(0, 7);
  const { page, shadow } = loaded(server);
  const reply = Y.encodeStateAsUpdate(page, Y.encodeStateVector(server));
  expect(reply.length).toBeGreaterThan(2);
  expect(documentWrite(frame("spec", 0, 1, reply), shadow)).toBeUndefined();

  // Its state vector (sync step 1), awareness (type 1) and authentication (type 2).
  expect(documentWrite(frame("spec", 0, 0, Y.encodeStateVector(page)), shadow)).toBeUndefined();
  expect(documentWrite(frame("spec", 1, 0, new Uint8Array([1, 2, 3])), shadow)).toBeUndefined();
  expect(documentWrite(frame("spec", 2, 0, new Uint8Array()), shadow)).toBeUndefined();
});

test("a document frame the guard cannot read is reported rather than let through", () => {
  expect(documentWrite(new Uint8Array([0xff]), new Y.Doc())).toMatchObject({
    write: expect.stringMatching(/^a document frame the guard could not read/u),
  });
});

test("a read is listed only where a listed route matches its whole path, a wildcard one segment", () => {
  expect(
    unlistedShotReads(
      new Set([
        "/api/v1/issues/CORE-1/asks",
        "/api/v1/artifacts/7d432ff7-f1ea-4f39-8927-99fd01738306/text",
        "/api/v1/projects/CORE/artifacts/runbook",
        "/api/v1/issues",
      ])
    )
  ).toEqual([]);
  expect(
    unlistedShotReads(
      new Set([
        "/api/v1/search",
        "/api/v1/issues/CORE-1/asks/extra",
        "/api/v1/projects/CORE/artifacts/runbook/text",
        "/api/v1/issuesX",
      ])
    )
  ).toEqual([
    "/api/v1/issues/CORE-1/asks/extra",
    "/api/v1/issuesX",
    "/api/v1/projects/CORE/artifacts/runbook/text",
    "/api/v1/search",
  ]);
});
