// How a case reads the transcript of a pane it drives, under this package's own shape: one
// `.jsonl` under a sessions directory. The withholding rule itself is
// `@legion/test-fixtures/transcript-lines`, shared with packages/pi-envoy. Nothing here starts a
// pane, so `transcript-entries.test.ts` drives all of it in milliseconds and without the binary.
import { readdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import { endsWhole, type Writer, writtenLines } from "@legion/test-fixtures/transcript-lines";

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
  return endsWhole(await readFile(file, "utf8"));
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
