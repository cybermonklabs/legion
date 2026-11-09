// How a case reads a transcript the pane it drives is still writing. Every reader here is pure or
// reads a file; nothing starts a pane, so `transcript-entries.test.ts` drives all of it in
// milliseconds and without the Oh My Pi binary.
import { readdir, readFile } from "node:fs/promises";
import * as path from "node:path";

/** Whether something may still be appending to the file being read. */
export type Writer = "alive" | "gone";

/**
 * Every line the writer has finished: the lines before the last newline, empties dropped. A
 * trailing fragment with no newline yet is a line a write has not finished, so it is left for the
 * next read and a poll answers "not yet" rather than throwing on half an object
 * (dispatch://INFRA-2188). A line that is complete and malformed is returned, so parsing it still
 * throws: that is a defect in whatever wrote it. The harness's own RPC frame reader buffers to a
 * newline for the same reason (`@legion/pi-shared/test/omp-harness`).
 */
export function completeLines(text: string): string[] {
  const end = text.lastIndexOf("\n");
  return (end === -1 ? "" : text.slice(0, end)).split("\n").filter(Boolean);
}

/**
 * The lines of `text` a reader may parse now. While the writer is alive an unfinished last line is
 * withheld; once it is gone every line is parsed, so a record written last is never withheld for
 * ever and a fragment that will never be finished throws where it is written rather than timing
 * out a poll.
 */
export function writtenLines(text: string, writer: Writer): string[] {
  return writer === "alive" ? completeLines(text) : text.split("\n").filter(Boolean);
}

/** The one `.jsonl` under `sessions`, or `undefined` when the directory does not hold exactly one. */
async function soleTranscript(sessions: string): Promise<string | undefined> {
  const files = (await readdir(sessions, { recursive: true })).filter((file) =>
    file.endsWith(".jsonl")
  );
  return files.length === 1 ? path.join(sessions, files[0] ?? "") : undefined;
}

/**
 * The entries of `customType` in the one `.jsonl` under `sessions`; `[]` when the directory does
 * not hold exactly one. `writer` says whether something may still be appending.
 */
export async function transcriptEntries(
  sessions: string,
  customType: string,
  writer: Writer
): Promise<Record<string, unknown>[]> {
  const file = await soleTranscript(sessions);
  if (file === undefined) return [];
  return writtenLines(await readFile(file, "utf8"), writer)
    .map((line): unknown => JSON.parse(line))
    .flatMap((entry) =>
      typeof entry === "object" &&
      entry !== null &&
      "customType" in entry &&
      entry.customType === customType
        ? [entry as Record<string, unknown>]
        : []
    );
}

/**
 * Whether the transcript holds no half-written line. An absence read taken while it does would be
 * the absence of a line still being written, not of the record the case says is not there. True
 * when the directory does not hold exactly one transcript — nothing half-written is there to wait
 * for, and `transcriptEntries` reads that directory as empty too.
 */
export async function transcriptSettled(sessions: string): Promise<boolean> {
  const file = await soleTranscript(sessions);
  if (file === undefined) return true;
  const text = await readFile(file, "utf8");
  return text === "" || text.endsWith("\n");
}

/**
 * Waits until `transcriptSettled` holds, polling every 25 ms, and throws naming the directory at
 * `withinMs`. One line is one `write`, so a wait that reaches its bound is a transcript nothing
 * is finishing rather than a poll that was too quick.
 */
export async function awaitSettled(sessions: string, withinMs: number): Promise<void> {
  const deadline = Date.now() + withinMs;
  while (!(await transcriptSettled(sessions))) {
    if (Date.now() > deadline) {
      throw new Error(`the transcript under ${sessions} still ends mid-line`);
    }
    await Bun.sleep(25);
  }
}
