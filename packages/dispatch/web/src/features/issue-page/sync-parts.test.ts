import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { MessageType } from "@hocuspocus/provider";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import { messageYjsSyncStep2 } from "y-protocols/sync";
import * as Y from "yjs";

import { splitSync } from "./sync-parts";

function pathOf(shape: string): URL {
  return new URL(`./fixtures/sync-frames/${shape}.json`, import.meta.url);
}

function capturedUpdate(shape: string): Uint8Array {
  const content: unknown = JSON.parse(readFileSync(pathOf(shape), "utf8"));
  if (typeof content !== "string") throw new Error(`${shape} must contain one base64 frame`);
  const decoder = createDecoder(Buffer.from(content, "base64"));
  expect(readVarString(decoder)).not.toBe("");
  expect(readVarUint(decoder)).toBe(MessageType.Sync);
  expect(readVarUint(decoder)).toBe(messageYjsSyncStep2);
  return readVarUint8Array(decoder);
}

function assertSameUpdate(update: Uint8Array, parts: readonly Uint8Array[]) {
  const one = new Y.Doc();
  const many = new Y.Doc();
  try {
    Y.applyUpdate(one, update);
    for (const part of parts) Y.applyUpdate(many, part);
    expect(Y.encodeStateVector(many)).toEqual(Y.encodeStateVector(one));
    expect(Y.encodeStateAsUpdate(many)).toEqual(Y.encodeStateAsUpdate(one));
  } finally {
    one.destroy();
    many.destroy();
  }
}

for (const shape of ["empty", "single", "multiple", "reconnect"] as const) {
  test(`the captured ${shape} frame's parts in order give the whole sync's document`, () => {
    const update = capturedUpdate(shape);
    assertSameUpdate(update, splitSync(update, 2));
  });
}

const gc = pathOf("gc");
test("a captured frame with garbage-collected structs is equivalent after splitting", () => {
  if (!existsSync(gc)) {
    // The server must explicitly report this shape was unavailable; absence alone cannot pass.
    expect(readFileSync(pathOf("gc-unavailable"), "utf8").trim()).toBe('"none"');
    return;
  }
  const update = capturedUpdate("gc");
  expect(Y.decodeUpdate(update).structs.some((struct) => struct instanceof Y.GC)).toBe(true);
  assertSameUpdate(update, splitSync(update, 2));
});

test("a part boundary can fall inside a client's run, keeping the next struct's clock", () => {
  const update = capturedUpdate("single");
  const original = Y.decodeUpdate(update).structs;
  expect(original.length).toBeGreaterThan(1);
  const parts = splitSync(update, 1);
  expect(parts.length).toBe(original.length);
  const decoded = parts.map((part) => Y.decodeUpdate(part).structs);
  expect(decoded.every((part) => part.length === 1)).toBe(true);
  expect(decoded.flat().map((struct) => struct.id)).toEqual(original.map((struct) => struct.id));
  expect(original.some((part, at) => at > 0 && original[at - 1].id.client === part.id.client)).toBe(
    true
  );
  assertSameUpdate(update, parts);
});

test("the delete set occurs only in the last part", () => {
  const update = capturedUpdate("multiple");
  expect(Y.decodeUpdate(update).ds.clients.size).toBeGreaterThan(0);
  const parts = splitSync(update, 2);
  expect(parts.length).toBeGreaterThan(1);
  const decoded = parts.map((part) => Y.decodeUpdate(part).ds.clients);
  expect(decoded.slice(0, -1).every((part) => part.size === 0)).toBe(true);
  expect(decoded.at(-1)).toEqual(Y.decodeUpdate(update).ds.clients);
  assertSameUpdate(update, parts);
});
