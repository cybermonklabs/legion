import { expect, test } from "bun:test";
import { createDecoder, readVarUint } from "lib0/decoding";
import {
  createEncoder,
  toUint8Array,
  writeVarString,
  writeVarUint,
  writeVarUint8Array,
} from "lib0/encoding";
import * as Y from "yjs";

import {
  documentWrite,
  followServer,
  isSeededReadMark,
  liveFrame,
  routeOf,
  unlistedShotReads,
  unlistedShotViews,
} from "../visual-guard";

// The visual review's write guard (e2e/visual-guard.ts) decides from a request's method, path and
// body, from a document frame's bytes read against the server's document, and from a live socket
// frame, which writes it stops; and which route a read is, against the route table. These rows
// hold those decisions on frames and requests built the way the page builds them, and the controls
// that a write passes for nothing: a read mark the seed never wrote, a sync reply that changes the
// document, an edit beside a derived attribute, and a path a broader route would match.

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

/** How many clients' structs a Yjs v1 update holds: its first number. */
function readClients(update: Uint8Array): number {
  return readVarUint(createDecoder(update));
}

/** What the page sends for `change`: the update its own transaction makes. */
function updateOf(page: Y.Doc, change: (fragment: Y.XmlFragment) => void): Uint8Array {
  const before = Y.encodeStateVector(page);
  page.transact(() => change(page.getXmlFragment("prosemirror")));
  return Y.encodeStateAsUpdate(page, before);
}

test("a read mark passes only exactly as the seed wrote it", () => {
  const seeded = { agents: { "s-1": ["r-1", "r-2"] }, documents: { "d-1": 3 } };
  expect(
    isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { read_replies: ["r-1"] }, seeded)
  ).toBe(true);
  expect(
    isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { read_replies: ["r-2", "r-1"] }, seeded)
  ).toBe(true);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", { version: 3 }, seeded)).toBe(
    true
  );

  // A version the seed did not write: one back moves the read back, one ahead moves it on.
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", { version: 0 }, seeded)).toBe(
    false
  );
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", { version: 4 }, seeded)).toBe(
    false
  );
  // A reply the seed did not mark, a session or a document it marked nothing of.
  expect(
    isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { read_replies: ["r-1", "r-9"] }, seeded)
  ).toBe(false);
  expect(
    isSeededReadMark("PUT", "/api/v1/me/agents/s-2/state", { read_replies: ["r-1"] }, seeded)
  ).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-2/read", { version: 3 }, seeded)).toBe(
    false
  );
  // A pin, a clear, a read-through or nothing at all on the same route.
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { pinned: true }, seeded)).toBe(
    false
  );
  expect(
    isSeededReadMark(
      "PUT",
      "/api/v1/me/agents/s-1/state",
      { read_replies: ["r-1"], read_through: "2026-11-23T00:00:00Z" },
      seeded
    )
  ).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", { read_replies: [] }, seeded)).toBe(
    false
  );
  expect(isSeededReadMark("PUT", "/api/v1/me/agents/s-1/state", {}, seeded)).toBe(false);
  // A path naming a property every object inherits names no session or document the seed marked.
  expect(
    isSeededReadMark(
      "PUT",
      "/api/v1/me/agents/constructor/state",
      { read_replies: ["r-1"] },
      seeded
    )
  ).toBe(false);
  expect(
    isSeededReadMark("PUT", "/api/v1/me/documents/constructor/read", { version: 3 }, seeded)
  ).toBe(false);
  expect(
    isSeededReadMark("PUT", "/api/v1/me/documents/__proto__/read", { version: 3 }, seeded)
  ).toBe(false);
  // A sign-in route holds no mark the seed wrote.
  expect(isSeededReadMark("PUT", "/auth/logout", { version: 3 }, seeded)).toBe(false);
  // Another method, another route, or a body that is no object.
  expect(isSeededReadMark("POST", "/api/v1/me/documents/d-1/read", { version: 3 }, seeded)).toBe(
    false
  );
  expect(
    isSeededReadMark("PUT", "/api/v1/me/threads/t-1/read", { read_through: "x" }, seeded)
  ).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", undefined, seeded)).toBe(false);
  expect(isSeededReadMark("PUT", "/api/v1/me/documents/d-1/read", [3], seeded)).toBe(false);
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
    write: `a sync reply to document spec that changes it (${typed.length} bytes, structs of 1 client(s))`,
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

  // A reply with no struct whose delete set deletes what the server holds is a write.
  const deleting = updateOf(page, (fragment) => {
    ((fragment.get(1) as Y.XmlElement).get(0) as Y.XmlText).delete(0, 2);
  });
  expect(readClients(deleting)).toBe(0);
  expect(documentWrite(frame("spec", 0, 1, deleting), shadow)).toMatchObject({
    write: expect.stringMatching(/^a sync reply to document spec that changes it/u),
  });

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

test("a read's route is the table's most specific, a literal segment over a wildcard", () => {
  expect(routeOf("/api/v1/asks/open")).toBe("/api/v1/asks/open");
  expect(routeOf("/api/v1/asks/a1f68ce1-f7c0-4951-8b84-f1971debb00e")).toBe("/api/v1/asks/{id}");
  expect(routeOf("/api/v1/issues/resolve")).toBe("/api/v1/issues/resolve");
  expect(routeOf("/api/v1/issues/CORE-1")).toBe("/api/v1/issues/{key}");
  expect(routeOf("/api/v1/issuesX")).toBeUndefined();
});

test("a read is listed only where the list names the route the table serves it by", () => {
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
  // A sign-in read the list names.
  expect(unlistedShotReads(new Set(["/auth/whoami"]))).toEqual([]);
  // A route the list does not name, though a listed wildcard would match its path, a path no
  // route of the table serves, and a sign-in read the list does not name.
  expect(
    unlistedShotReads(
      new Set([
        "/api/v1/asks/open",
        "/api/v1/issues/resolve",
        "/api/v1/search",
        "/api/v1/issues/CORE-1/asks/extra",
        "/api/v1/issuesX",
        "/auth/start",
      ])
    )
  ).toEqual([
    "/api/v1/asks/open (/api/v1/asks/open)",
    "/api/v1/issues/CORE-1/asks/extra (no route)",
    "/api/v1/issues/resolve (/api/v1/issues/resolve)",
    "/api/v1/issuesX (no route)",
    "/api/v1/search (/api/v1/search)",
    "/auth/start (sign-in read)",
  ]);
});

test("a live frame passes as a subscribe of a listed view or an unsubscribe, and nothing else", () => {
  const subscribe = (view: object) => JSON.stringify({ type: "subscribe", sub: "s", view });
  expect(liveFrame(subscribe({ kind: "events" }))).toEqual({ pass: true, view: "events" });
  expect(liveFrame(subscribe({ kind: "timeline", window: { preset: 7 }, query: "" }))).toEqual({
    pass: true,
    view: "timeline",
  });
  expect(liveFrame(JSON.stringify({ type: "unsubscribe", sub: "s" }))).toEqual({ pass: true });
  expect(liveFrame(subscribe({ kind: "agent", session_id: "s-1" }))).toEqual({ unlisted: "agent" });
  expect(liveFrame(JSON.stringify({ type: "ack", sub: "s" }))).toMatchObject({
    write: expect.stringMatching(/^a live socket frame that is no subscribe or unsubscribe/u),
  });
  expect(liveFrame("not json")).toMatchObject({ write: expect.any(String) });
  expect(liveFrame(Buffer.from([1, 2]))).toEqual({ write: "a binary live socket frame (2 bytes)" });
  expect(unlistedShotViews(new Set(["events", "pipeline", "agent", "legion"]))).toEqual([
    "agent",
    "legion",
  ]);
});
