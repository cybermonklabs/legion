// How a case reads the transcript of a pane it drives, under this package's own shape: one
// `.jsonl` under a sessions directory. The withholding rule itself is
// `@legion/test-fixtures/transcript-lines`, shared with packages/pi-envoy. Nothing here starts a
// pane, so `transcript-entries.test.ts` drives all of it in milliseconds and without the binary.
import { readdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import { endsWhole, type Writer, writtenLines } from "@legion/test-fixtures/transcript-lines";
import { until } from "@legion/test-fixtures/wait";

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
 * Waits through the shared wait until `transcriptSettled` holds, and fails naming the directory at
 * the row's deadline. One line is one `write`, so a wait that reaches the deadline is a transcript
 * nothing is finishing rather than a poll that was too quick.
 */
export async function awaitSettled(sessions: string): Promise<void> {
  await until(`the transcript under ${sessions} to end on a whole line`, () =>
    transcriptSettled(sessions)
  );
}
