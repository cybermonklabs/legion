import { readFile, realpath } from "node:fs/promises";
import { isAbsolute, join, resolve, sep } from "node:path";
import type { AskOption, CaseRow, CaseVerdict } from "./build-corpus";
import type { Interval } from "./stats";

/**
 * What the approval-eval commands share: a built corpus read back, the rule that reads a verdict
 * out of an answer, the readers of unknown JSON, the counts, shares and tables their records
 * print, and the two checks every one of these commands makes on its flags.
 *
 * The types come from `build-corpus.ts`, which writes them, and from `stats.ts`, which measures
 * what is printed, and both imports are type-only: they are erased, so importing this module
 * runs none of the build and makes no measurement.
 */

/** The flags were wrong, and the usage says how. */
export class UsageError extends Error {
  override readonly name = "UsageError";
}

/** An output path names somewhere inside the workspace, which no byte of a corpus may touch. */
export class OutputPathError extends Error {
  override readonly name = "OutputPathError";
}

/** The directory named is not a corpus this can read back. */
export class CorpusError extends Error {
  override readonly name = "CorpusError";
}

/** What a build read, as `snapshot.json` holds it. */
export interface CorpusSnapshot {
  person: string;
  throughEventId: number;
  eventsRead: number;
  keptEvents: number;
  readStartedAt: string;
  readFinishedAt: string;
}

/** One built corpus, read back whole: what it was built from, and every case of it. */
export interface Corpus {
  /** The directory it was read from, resolved. */
  directory: string;
  snapshot: CorpusSnapshot;
  /** The cases in the order `corpus.jsonl` holds them, which is answer time. */
  rows: CaseRow[];
  byCaseId: Map<string, CaseRow>;
}

export function recordOf(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

export function stringOf(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

export function numberOf(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

export function listOf(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

/** The options an ask offered, as an event's payload and the ask's own route both hold them. */
export function optionsOf(value: unknown): AskOption[] {
  return listOf(value).map((entry) => {
    const option = recordOf(entry);
    return {
      id: stringOf(option?.id),
      label: stringOf(option?.label),
      description: stringOf(option?.description),
    };
  });
}

/**
 * What the person decided on an approval ask, read from the option they chose. An answer names
 * the labels, but an answer naming an option's id is read back through the options it was
 * offered. Both readings of a corpus apply this one rule, so a difference between them is a
 * difference in what the two routes say and never in how a verdict was read.
 */
export function verdictOf(selected: readonly string[], options: readonly AskOption[]): CaseVerdict {
  const first = selected[0];
  const chosen = options.find((option) => option.id === first)?.label ?? first ?? "";
  const label = chosen.trim().toLowerCase();
  if (label.startsWith("approve")) return "approve";
  if (label.startsWith("request")) return "request_changes";
  return "other";
}

/**
 * The directory a command writes to. It must be absolute and outside this repository's
 * workspace: the workspace is snapshotted whole into the working copy, so anything written there
 * would be committed.
 */
export async function outDirectory(out: string): Promise<string> {
  if (!isAbsolute(out)) {
    throw new OutputPathError(`--out takes an absolute path, and ${out} is relative`);
  }
  const wanted = resolve(out);
  const workspaces = new Set<string>();
  const named = process.env.LEGION_WORKSPACE?.trim();
  if (named) workspaces.add(resolve(named));
  workspaces.add(resolve(import.meta.dir, "..", ".."));
  for (const workspace of [...workspaces]) {
    try {
      workspaces.add(await realpath(workspace));
    } catch {
      // A workspace that is not there cannot hold the output either.
    }
  }
  for (const workspace of workspaces) {
    const prefix = workspace.endsWith(sep) ? workspace : workspace + sep;
    if (wanted === workspace || wanted.startsWith(prefix)) {
      throw new OutputPathError(
        `--out must name a path outside the repository workspace, and ${wanted} is under ${workspace}`
      );
    }
  }
  return wanted;
}

/** The corpus a `--corpus` names, read back from the three files the build wrote. */
export async function readCorpus(corpus: string): Promise<Corpus> {
  const directory = resolve(corpus);
  const snapshot = snapshotOf(directory, await textOf(directory, "snapshot.json"));
  const lines = (await textOf(directory, "corpus.jsonl")).split("\n");
  const rows: CaseRow[] = [];
  const byCaseId = new Map<string, CaseRow>();
  for (const [index, line] of lines.entries()) {
    if (line.trim() === "") continue;
    const row = caseRowOf(directory, index + 1, line);
    rows.push(row);
    byCaseId.set(row.caseId, row);
  }
  if (rows.length === 0) {
    throw new CorpusError(`${join(directory, "corpus.jsonl")} holds no case`);
  }
  return { directory, snapshot, rows, byCaseId };
}

/** A seed for a pick no `--seed` named, which the command prints so the pick can be made again. */
export function newSeed(): number {
  return Math.trunc(Math.random() * 0x1_0000_0000);
}

/** mulberry32: the seeded stream a draw, an order and a pick are all drawn from. */
export function randomStream(seed: number): () => number {
  let state = Math.trunc(seed) >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let drawn = state;
    drawn = Math.imul(drawn ^ (drawn >>> 15), 1 | drawn);
    drawn = (drawn + Math.imul(drawn ^ (drawn >>> 7), 61 | drawn)) ^ drawn;
    return ((drawn ^ (drawn >>> 14)) >>> 0) / 0x1_0000_0000;
  };
}

/**
 * One item of `items`, drawn by one step of mulberry32 from `seed`: the same seed and the same
 * list draw the same item, which is what makes a printed seed worth printing.
 */
export function pickAtRandom<T>(items: readonly T[], seed: number): T | undefined {
  if (items.length === 0) return undefined;
  const unit = randomStream(seed)();
  return items[Math.min(items.length - 1, Math.floor(unit * items.length))];
}

/** Runs `run` over `items`, `concurrency` of them at once, stopping when `stop` says to. */
export async function pool<T>(
  items: readonly T[],
  concurrency: number,
  run: (item: T) => Promise<void>,
  stop?: () => boolean
): Promise<void> {
  let next = 0;
  const workers = Array.from({ length: Math.max(1, Math.min(concurrency, items.length)) }, () =>
    (async () => {
      while (next < items.length) {
        if (stop?.() === true) return;
        const at = next;
        next = at + 1;
        await run(items[at] as T);
      }
    })()
  );
  await Promise.all(workers);
}

/** A count, as every printed count of these records is written. */
export function count(value: number): string {
  return value.toLocaleString("en-US");
}

/**
 * A count beside the noun it counts, which is how a count written into a sentence is written.
 * A command that takes one of a thing as readily as several prints "1 runs" over an input it
 * accepts wherever a sentence of it carries a bare count.
 */
export function counted(value: number, one: string, several = `${one}s`): string {
  return `${count(value)} ${value === 1 ? one : several}`;
}

/** One markdown table, its columns padded so the text reads as a table too. */
export function table(headers: readonly string[], rows: readonly (readonly string[])[]): string[] {
  const widths = headers.map((header, at) =>
    Math.max(header.length, ...rows.map((row) => (row[at] ?? "").length))
  );
  const line = (cells: readonly string[]): string =>
    `| ${cells.map((cell, at) => cell.padEnd(widths[at] ?? 0)).join(" | ")} |`;
  return [
    line(headers),
    `| ${widths.map((width) => "-".repeat(width)).join(" | ")} |`,
    ...rows.map((row) => line(row)),
  ];
}

/** A share or an agreement, to three places, as every one of these records prints one. */
export function rate(value: number): string {
  return value.toFixed(3);
}

/**
 * A difference of two shares as these records print it, which always carries its sign so that
 * a fall reads as a fall. A value whose magnitude rounds away takes `+`, so that two numbers
 * equal at the precision printed are never printed with opposite signs, which is how a reading
 * that moved by nothing came to read as two.
 */
export function signed(value: number): string {
  const magnitude = Math.abs(value).toFixed(3);
  return `${value < 0 && magnitude !== rate(0) ? "−" : "+"}${magnitude}`;
}

/** An interval of such differences, as these records print it. */
export function span(interval: Interval): string {
  return `[${signed(interval.lower)}, ${signed(interval.upper)}]`;
}

async function textOf(directory: string, name: string): Promise<string> {
  const path = join(directory, name);
  try {
    return await readFile(path, "utf8");
  } catch {
    throw new CorpusError(`${path} could not be read: --corpus takes a directory a build wrote`);
  }
}

function snapshotOf(directory: string, text: string): CorpusSnapshot {
  const path = join(directory, "snapshot.json");
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${path} is not JSON`);
  }
  const record = recordOf(parsed);
  const person = stringOf(record?.person)?.trim();
  const throughEventId = numberOf(record?.throughEventId);
  if (!person || throughEventId === null || !Number.isSafeInteger(throughEventId)) {
    throw new CorpusError(`${path} names no person and no pinned event id`);
  }
  return {
    person,
    throughEventId,
    eventsRead: numberOf(record?.eventsRead) ?? 0,
    keptEvents: numberOf(record?.keptEvents) ?? 0,
    readStartedAt: stringOf(record?.readStartedAt) ?? "",
    readFinishedAt: stringOf(record?.readFinishedAt) ?? "",
  };
}

/** One line of a corpus's `corpus.jsonl` read back, which a build wrote. */
function caseRowOf(directory: string, line: number, text: string): CaseRow {
  const where = `${join(directory, "corpus.jsonl")} line ${line}`;
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${where} is not JSON`);
  }
  const record = recordOf(parsed);
  const caseId = stringOf(record?.caseId);
  const askId = stringOf(record?.askId);
  const answeredAt = stringOf(record?.answeredAt);
  const kind = stringOf(record?.kind);
  if (!caseId || !askId || !answeredAt || (kind !== "approval" && kind !== "question")) {
    throw new CorpusError(`${where} is not a case: it names no case id, ask, answer time or kind`);
  }
  // The rest of the line is what the build wrote, whose shape is `CaseRow`.
  return record as unknown as CaseRow;
}
