import { expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { awaitSettled, transcriptEntries, transcriptSettled } from "./transcript-entries";

const KIND = "legion.stall-watch";

/** A directory of this file's own, removed after the test that made it. */
const scratchDir = temporaryDirectories("transcript-entries");

/** A transcript entry as a pane writes one. */
const entry = (customType: string, n: number): string =>
  JSON.stringify({ customType, data: { n, openMs: 7_004 } });

/** A sessions directory holding one `.jsonl` whose text is `text`. */
async function sessions(text: string): Promise<string> {
  const dir = scratchDir();
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
  await expect(transcriptEntries(dir, KIND, "gone")).rejects.toThrow(SyntaxError);
});

test("a complete line that is malformed throws whatever the writer is doing", async () => {
  const dir = await sessions(`${entry(KIND, 1)}\n{"customType":"${KIND}",}\n`);
  await expect(transcriptEntries(dir, KIND, "alive")).rejects.toThrow(SyntaxError);
  await expect(transcriptEntries(dir, KIND, "gone")).rejects.toThrow(SyntaxError);
});

test("a directory that does not hold exactly one transcript reads as empty", async () => {
  const none = scratchDir();
  expect(await transcriptEntries(none, KIND, "alive")).toEqual([]);
  const two = await sessions(`${entry(KIND, 1)}\n`);
  await writeFile(path.join(two, "second.jsonl"), `${entry(KIND, 2)}\n`);
  expect(await transcriptEntries(two, KIND, "alive")).toEqual([]);
});

test("a transcript is settled unless it ends mid-line; an empty or absent one is settled", async () => {
  expect(await transcriptSettled(await sessions(`${entry(KIND, 1)}\n`))).toBe(true);
  expect(await transcriptSettled(await sessions(""))).toBe(true);
  expect(await transcriptSettled(await sessions(`${entry(KIND, 1)}\n{"cus`))).toBe(false);
  expect(await transcriptSettled(scratchDir())).toBe(true);
});

test("the wait for a settled transcript returns at once, and throws naming it at its bound", async () => {
  await awaitSettled(await sessions(`${entry(KIND, 1)}\n`), 5_000);
  const mid = await sessions(`${entry(KIND, 1)}\n{"cus`);
  await expect(awaitSettled(mid, 50)).rejects.toThrow(`the transcript under ${mid}`);
});
