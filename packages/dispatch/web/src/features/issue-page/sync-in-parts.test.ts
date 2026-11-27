import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { HocuspocusProvider } from "@hocuspocus/provider";
import { DOCUMENT_SCHEMA_CLOSE_CODE } from "@legion/contracts";
import { waitFor } from "@testing-library/react";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import * as Y from "yjs";
import { loadDocumentTransport } from "../doc/connection";
import {
  artifactId,
  authenticatedFrame,
  awarenessFrame,
  closeStores,
  frame,
  rowsOf,
  savedEdit,
  serverWith,
  syncStatusFrame,
  syncStep1Frame,
  syncStep2Frame,
  varBytes,
} from "../doc/pending-fixture";
import { splitSync, splitSyncInWorker } from "./sync-parts";

class RoomSocket extends EventTarget {
  static opened: RoomSocket[] = [];
  binaryType = "arraybuffer";
  readyState = 1;
  readonly sent: Uint8Array[] = [];

  constructor(readonly url: string) {
    super();
    RoomSocket.opened.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  send(message: Uint8Array) {
    this.sent.push(message.slice());
  }

  receive(message: Uint8Array) {
    this.dispatchEvent(new MessageEvent("message", { data: message.slice().buffer }));
  }

  close(code = 1000) {
    if (this.readyState === 3) return;
    this.readyState = 3;
    queueMicrotask(() => this.dispatchEvent(new CloseEvent("close", { code })));
  }
}

function captured(shape: string): Uint8Array {
  const encoded: unknown = JSON.parse(
    readFileSync(new URL(`./fixtures/sync-frames/${shape}.json`, import.meta.url), "utf8")
  );
  if (typeof encoded !== "string") throw new Error(`expected captured ${shape} SyncStep2`);
  const decoder = createDecoder(Buffer.from(encoded, "base64"));
  readVarString(decoder);
  expect(readVarUint(decoder)).toBe(0);
  expect(readVarUint(decoder)).toBe(1);
  return readVarUint8Array(decoder);
}

function sentSync(socket: RoomSocket, step: number) {
  return socket.sent.flatMap((bytes) => {
    const decoder = createDecoder(bytes);
    readVarString(decoder);
    if (readVarUint(decoder) !== 0 || readVarUint(decoder) !== step) return [];
    return [readVarUint8Array(decoder)];
  });
}

async function runCase(
  run: (rig: Awaited<ReturnType<typeof setup>>) => Promise<void>,
  before?: (artifact: string) => Promise<void>,
  splitter?: (update: Uint8Array) => Promise<Uint8Array[]>,
  shape = "multiple"
) {
  const Native = globalThis.WebSocket;
  RoomSocket.opened = [];
  globalThis.WebSocket = RoomSocket as unknown as typeof WebSocket;
  let rig: Awaited<ReturnType<typeof setup>> | undefined;
  try {
    const id = artifactId();
    await before?.(id);
    rig = await setup(id, splitter, shape);
    await run(rig);
  } finally {
    rig?.connection.destroy();
    globalThis.WebSocket = Native;
    closeStores();
  }
}

async function setup(
  id: string,
  splitter: ((update: Uint8Array) => Promise<Uint8Array[]>) | undefined,
  shape: string
) {
  const update = captured(shape);
  const split = Promise.withResolvers<Uint8Array[]>();
  const splitCalls: Uint8Array[] = [];
  const admissions: boolean[] = [];
  const marks: string[] = [];
  const origin: unknown[] = [];
  const order: string[] = [];
  const connect = await loadDocumentTransport();
  const options = {
    fragment: "prosemirror",
    schemaVersion: 1,
    schemaCreatable: 1,
    firstSyncInParts: (bytes: Uint8Array) => {
      splitCalls.push(bytes);
      return splitter === undefined ? split.promise : splitter(bytes);
    },
    onAdmission: (readOnly: boolean) => {
      admissions.push(readOnly);
      order.push("admitted");
    },
    onOutsideSchema: () => {
      marks.push("outside-schema");
    },
    onSynced: () => {
      marks.push("synced");
    },
    onPending: () => {},
    onStatus: () => {},
  };
  const connection = connect(id, options);
  connection.doc.on("afterTransaction", (transaction) => {
    if (!transaction.local) {
      origin.push(transaction.origin);
      order.push("remote-apply");
    }
  });

  const socketAt = async (index = 0) => {
    await waitFor(() => expect(RoomSocket.opened[index]?.sent.length).toBeGreaterThan(0), {
      timeout: 10_000,
    });
    return RoomSocket.opened[index];
  };
  const first = await socketAt();
  const updateListeners = () => connection.doc._observers.get("update")?.size ?? 0;
  const hold = () => first.receive(syncStep2Frame(update, id));
  const later = (authenticate = true) => {
    if (authenticate) first.receive(authenticatedFrame("read-write", id));
    first.receive(syncStep1Frame(new Uint8Array([0]), id));
    first.receive(awarenessFrame(id));
    first.receive(syncStatusFrame(true, id));
    first.receive(frame(id, [0, 2], varBytes(update)));
  };
  const play = async () => {
    split.resolve(splitSync(update));
    await waitFor(() => expect(marks).toContain("synced"));
  };
  return {
    id,
    connection,
    first,
    update,
    hold,
    later,
    play,
    split,
    splitCalls,
    admissions,
    marks,
    origin,
    order,
    socketAt,
    updateListeners,
  };
}

test("connect detaches both update listeners before the socket speaks", () =>
  runCase(async (r) => {
    expect(r.updateListeners()).toBe(0);
  }));

test("the first SyncStep2 goes to the splitter and is not delivered to Hocuspocus", () =>
  runCase(async (r) => {
    r.hold();
    await waitFor(() => expect(r.splitCalls).toEqual([r.update]));
    expect(r.origin).toEqual([]);
    expect(r.marks).toEqual([]);
  }));

test("frames arriving behind SyncStep2 stay held while split parts are pending", () =>
  runCase(async (r) => {
    r.hold();
    const sent = r.first.sent.length;
    r.later();
    await waitFor(() => expect(r.splitCalls).toHaveLength(1));
    expect(r.first.sent).toHaveLength(sent);
    expect(r.admissions).toEqual([]);
    expect(r.origin).toEqual([]);
  }));

test("each part applies as the provider's update in its own task and sends nothing", () =>
  runCase(async (r) => {
    r.hold();
    const parts = splitSync(r.update, 2);
    const writes = sentSync(r.first, 2).length;
    const boundaries: number[] = [];
    r.connection.doc.on("afterTransaction", (tr) => {
      if (tr.local) return;
      boundaries.push(r.origin.length);
      queueMicrotask(() => boundaries.push(-r.origin.length));
    });
    r.split.resolve(parts);
    await waitFor(() => expect(r.origin.length).toBeGreaterThanOrEqual(parts.length));
    expect(
      r.origin.slice(0, parts.length).every((origin) => origin instanceof HocuspocusProvider)
    ).toBe(true);
    expect(boundaries.slice(0, 4)).toEqual([1, -1, 2, -2]);
    expect(sentSync(r.first, 2)).toHaveLength(writes);
  }));

for (const shape of ["empty", "single", "multiple", "reconnect"]) {
  test(`after the last part of ${shape}, both listeners return, admission replays and synced fires once`, () =>
    runCase(
      async (r) => {
        r.hold();
        r.later();
        expect(r.admissions).toEqual([]);
        await r.play();
        expect(r.admissions).toEqual([false]);
        expect(r.marks).toEqual(["synced"]);
        expect(r.updateListeners()).toBe(2);
        expect(r.order.indexOf("admitted")).toBeGreaterThanOrEqual(0);
        expect(r.origin.length).toBeGreaterThan(0);
        const one = new Y.Doc();
        try {
          Y.applyUpdate(one, r.update);
          expect(Y.encodeStateVector(r.connection.doc)).toEqual(Y.encodeStateVector(one));
          expect(Y.encodeStateAsUpdate(r.connection.doc)).toEqual(Y.encodeStateAsUpdate(one));
        } finally {
          one.destroy();
        }
      },
      undefined,
      undefined,
      shape
    ));
}

test("after sync a new edit is stored and sends a provider update", () =>
  runCase(async (r) => {
    r.hold();
    r.later();
    await r.play();
    r.connection.doc.getMap("marks").set("test-edit", "kept");
    await waitFor(() => expect(sentSync(r.first, 2)).toHaveLength(1));
    await waitFor(async () => expect(await rowsOf(r.id)).toHaveLength(1));
  }));

async function finishAfterClose(r: Awaited<ReturnType<typeof setup>>, code: number) {
  const admitted = code === DOCUMENT_SCHEMA_CLOSE_CODE;
  if (admitted) r.first.receive(authenticatedFrame("read-write", r.id));
  r.hold();
  r.later(!admitted);
  r.first.close(code);
  r.split.resolve(splitSync(r.update, 2));
  const next = await r.socketAt(1);
  expect(r.connection.doc._observers.get("update")?.size).toBe(2);
  next.receive(authenticatedFrame("read-write", r.id));
  next.receive(syncStep2Frame(r.update, r.id));
  await waitFor(() => expect(r.marks).toEqual(["synced"]));
  expect(r.admissions).toEqual(admitted ? [false, false] : [false]);
  expect(r.splitCalls).toHaveLength(1);
}

test("closing a socket during parts finishes the parts and drops that socket's later frames", () =>
  runCase((r) => finishAfterClose(r, 1006)));

test("a reconnect's SyncStep2 bypasses the splitter", () =>
  runCase(async (r) => {
    r.hold();
    r.later();
    await r.play();
    r.first.close(1006);
    const next = await r.socketAt(1);
    next.receive(authenticatedFrame("read-write", r.id));
    next.receive(syncStep2Frame(captured("reconnect"), r.id));
    await waitFor(() => expect(r.admissions).toEqual([false, false]));
    expect(r.splitCalls).toHaveLength(1);
    expect(r.marks).toEqual(["synced"]);
  }));

test("destroy while parts are pending applies no later part and delivers no later frame", () =>
  runCase(async (r) => {
    r.hold();
    r.later();
    await waitFor(() => expect(r.splitCalls).toHaveLength(1));
    r.connection.destroy();
    r.split.resolve(splitSync(r.update, 1));
    // Deliver the resolved split's continuation and its next task without a wall-clock sleep.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(r.origin).toEqual([]);
    expect(r.marks).toEqual([]);
    expect(r.admissions).toEqual([]);
  }));

test("without Worker construction, split and each application are still separated", () => {
  const phases: string[] = [];
  return runCase(
    async (r) => {
      r.connection.doc.on("afterTransaction", (tr) => {
        if (!tr.local) phases.push("apply");
      });
      r.hold();
      r.later();
      await waitFor(() => expect(r.marks).toEqual(["synced"]));
      expect(phases.slice(0, 4)).toEqual([
        "worker failed",
        "split task",
        "split task ended",
        "apply",
      ]);
      expect(r.admissions).toEqual([false]);
      expect(r.updateListeners()).toBe(2);
    },
    undefined,
    (update) =>
      splitSyncInWorker(update, {
        maxStructs: 2,
        createWorker() {
          phases.push("worker failed");
          throw new Error("Worker cannot be constructed");
        },
        schedule(run: () => void) {
          // An actual separate task without a wall-clock delay of any particular length.
          setImmediate(() => {
            phases.push("split task");
            run();
            queueMicrotask(() => phases.push("split task ended"));
          });
        },
      })
  );
});

test("pending restore sends the saved edit after the parts and stores no part", () => {
  let saved: Uint8Array | undefined;
  return runCase(
    async (r) => {
      // Use the matching base of the stored edit for this row, not an unrelated captured client.
      const base = serverWith(71);
      try {
        r.first.receive(syncStep2Frame(Y.encodeStateAsUpdate(base), r.id));
        r.first.receive(authenticatedFrame("read-write", r.id));
        r.split.resolve(splitSync(Y.encodeStateAsUpdate(base), 1));
        await waitFor(() => expect(r.marks).toEqual(["synced"]));
        expect(await rowsOf(r.id)).toHaveLength(1);
        const sent = sentSync(r.first, 2);
        expect(sent).toContainEqual(saved);
      } finally {
        base.destroy();
      }
    },
    async (id) => {
      const base = serverWith(71);
      try {
        saved = await savedEdit(id, base, " restored");
      } finally {
        base.destroy();
      }
    }
  );
});

test("a schema close during parts reconnects without announcing outside-schema", () =>
  runCase((r) => finishAfterClose(r, DOCUMENT_SCHEMA_CLOSE_CODE)));

test("a schema switch after sync holds the same Y.Doc and block objects", () =>
  runCase(async (r) => {
    r.hold();
    r.later();
    await r.play();
    const doc = r.connection.doc;
    const name = [...doc.share.keys()].find((key) => key !== "marks");
    if (name === undefined) throw new Error("the real capture must hold a document fragment");
    const root = doc.getXmlFragment(name);
    const element = root.toArray().find((child) => child instanceof Y.XmlElement);
    expect(element).toBeDefined();
    r.first.close(DOCUMENT_SCHEMA_CLOSE_CODE);
    const next = await r.socketAt(1);
    next.receive(authenticatedFrame("read-write", r.id));
    next.receive(syncStep2Frame(r.update, r.id));
    await waitFor(() => expect(r.admissions).toEqual([false, false]));
    expect(r.connection.doc).toBe(doc);
    expect(doc.getXmlFragment(name)).toBe(root);
    expect(root.toArray()).toContain(element);
    expect(r.marks).toEqual(["synced"]);
  }));

test("a socket closed with the schema code before any message tries once more", () =>
  runCase(async (r) => {
    r.first.close(DOCUMENT_SCHEMA_CLOSE_CODE);
    await r.socketAt(1);
    expect(r.marks).toEqual([]);
    expect(r.splitCalls).toEqual([]);
  }));

test("a second schema close before any message refuses the connection", () =>
  runCase(async (r) => {
    r.first.close(DOCUMENT_SCHEMA_CLOSE_CODE);
    const second = await r.socketAt(1);
    second.close(DOCUMENT_SCHEMA_CLOSE_CODE);
    await waitFor(() => expect(r.marks).toEqual(["outside-schema"]));
    expect(RoomSocket.opened).toHaveLength(2);
  }));
