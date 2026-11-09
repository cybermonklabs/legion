import { afterAll, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import {
  awaitSettled,
  completeLines,
  transcriptEntries,
  transcriptSettled,
  writtenLines,
} from "./transcript-entries";

const KIND = "legion.stall-watch";

/** Every directory this file made, removed when it has run: a pod's `$TMPDIR` is the issue's own
 * volume, shared by every role of the issue. */
const scratch: string[] = [];

afterAll(async () => {
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true });
});

/** A transcript entry as a pane writes one. */
const entry = (customType: string, n: number): string =>
  JSON.stringify({ customType, data: { n, openMs: 7_004 } });

/** A directory of this file's own. */
async function scratchDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "transcript-entries-"));
  scratch.push(dir);
  return dir;
}

/** A sessions directory holding one `.jsonl` whose text is `text`. */
async function sessions(text: string): Promise<string> {
  const dir = await scratchDir();
  await writeFile(path.join(dir, "session.jsonl"), text);
  return dir;
}

test("a reader takes the entries of its own kind and drops the rest", async () => {
  const dir = await sessions(`${entry(KIND, 1)}\n${entry("other", 2)}\n${entry(KIND, 3)}\n`);
  expect(await transcriptEntries(dir, KIND, "alive")).toEqual([
    { customType: KIND, data: { n: 1, openMs: 7_004 } },
    { customType: KIND, data: { n: 3, openMs: 7_004 } },
  ]);
});

// The defect this module exists for: the pane appends to the transcript while a case polls it, so
// a read can land between a line's bytes and its newline. On the merge base that read threw
// `JSON Parse error: Expected '}'` and failed the case (dispatch://INFRA-2188).
test("a last line the writer has not finished is withheld while the writer is alive", async () => {
  const dir = await sessions(`${entry(KIND, 1)}\n${entry(KIND, 2)}\n{"customType":"${KIND}",`);
  expect(await transcriptEntries(dir, KIND, "alive")).toEqual([
    { customType: KIND, data: { n: 1, openMs: 7_004 } },
    { customType: KIND, data: { n: 2, openMs: 7_004 } },
  ]);
});

test("the same fragment is handed on once the writer is gone, so parsing it throws", async () => {
  const dir = await sessions(`${entry(KIND, 1)}\n{"customType":"${KIND}",`);
  expect(writtenLines(`${entry(KIND, 1)}\n{"cus`, "gone")).toEqual([entry(KIND, 1), '{"cus']);
  await expect(transcriptEntries(dir, KIND, "gone")).rejects.toThrow(/JSON Parse error/);
});

test("a complete line that is malformed throws whatever the writer is doing", async () => {
  const dir = await sessions(`${entry(KIND, 1)}\n{"customType":"${KIND}",}\n`);
  await expect(transcriptEntries(dir, KIND, "alive")).rejects.toThrow(/JSON Parse error/);
  await expect(transcriptEntries(dir, KIND, "gone")).rejects.toThrow(/JSON Parse error/);
});

test("a directory that does not hold exactly one transcript reads as empty", async () => {
  const none = await scratchDir();
  expect(await transcriptEntries(none, KIND, "alive")).toEqual([]);
  const two = await sessions(`${entry(KIND, 1)}\n`);
  await writeFile(path.join(two, "second.jsonl"), `${entry(KIND, 2)}\n`);
  expect(await transcriptEntries(two, KIND, "alive")).toEqual([]);
});

test("completeLines keeps every finished line and nothing after the last newline", () => {
  expect(completeLines("")).toEqual([]);
  expect(completeLines("{}")).toEqual([]);
  expect(completeLines("{}\n")).toEqual(["{}"]);
  expect(completeLines('{"a":1}\n\n{"b":2}\n{"c"')).toEqual(['{"a":1}', '{"b":2}']);
});

test("a transcript is settled unless it ends mid-line; an empty or absent one is settled", async () => {
  expect(await transcriptSettled(await sessions(`${entry(KIND, 1)}\n`))).toBe(true);
  expect(await transcriptSettled(await sessions(""))).toBe(true);
  expect(await transcriptSettled(await sessions(`${entry(KIND, 1)}\n{"cus`))).toBe(false);
  expect(await transcriptSettled(await scratchDir())).toBe(true);
});

test("the wait for a settled transcript returns at once, and throws naming it at its bound", async () => {
  await awaitSettled(await sessions(`${entry(KIND, 1)}\n`), 5_000);
  const mid = await sessions(`${entry(KIND, 1)}\n{"cus`);
  await expect(awaitSettled(mid, 50)).rejects.toThrow(`the transcript under ${mid}`);
});
