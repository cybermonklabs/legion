import { readFile, realpath } from "node:fs/promises";
import { isAbsolute, join, resolve, sep } from "node:path";
import type { AskOption, CaseRow, CaseVerdict } from "./build-corpus";
import { type Interval, MINIMUM_DECIDED } from "./stats";

/**
 * What the approval-eval commands share: a built corpus read back, the rule that reads a verdict
 * out of an answer, the readers of unknown JSON, the fields a case is read by, the counts, shares
 * and tables their records print, the flag readers they all use, and the base class of every
 * error that means a command cannot run.
 *
 * The types come from `build-corpus.ts`, which writes them, and from `stats.ts`, which measures
 * what is printed, and both imports are type-only: they are erased, so importing this module
 * runs none of the build and makes no measurement.
 */

/**
 * An error that means the command cannot run. Every command's `main` catches this one class and
 * takes its own cannot-run exit status from it, so a class added to this family is caught by all
 * of them at once and none of them can be left a chain short. An error outside this family is
 * not expected and is left to throw.
 */
export class CannotRunError extends Error {
  override readonly name: string = "CannotRunError";
}

/** The flags were wrong, and the usage says how. */
export class UsageError extends CannotRunError {
  override readonly name = "UsageError";
}

/** An output path names somewhere inside the workspace, which no byte of a corpus may touch. */
export class OutputPathError extends CannotRunError {
  override readonly name = "OutputPathError";
}

/** The directory named is not a corpus this can read back. */
export class CorpusError extends CannotRunError {
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

/** Every artifact an approval case names, at whatever version it named it. */
export function artifactsOf(row: CaseRow): Set<string> {
  const ids = new Set<string>();
  for (const document of row.approval?.documents ?? []) {
    if (document.artifactId !== null) ids.add(document.artifactId);
  }
  return ids;
}

/**
 * The document an approval request is counted under: the first it names both an artifact and a
 * version for, and null where it names no such document. It is one document of the request, not
 * the request, and it answers one question alone - which document the analysis counts this case
 * by. `analyze.ts` is its only caller: it takes one case per document of this in the sensitivity
 * readings, so that the within-document correlation is out of them.
 *
 * Two neighbouring questions are not this one and are answered where they are asked. What the
 * draw records of a case is the first document the judge was actually shown, which `draw.ts`
 * reads off the replayed documents, because its digest must be over bytes the judge saw and the
 * first document the request names may be one that could not be read. What the leak guard may
 * not let a version be named at is each document's own `requestedVersion`, over that document's
 * own section of the rendering, which `render.ts` reads off every document of the request.
 * Neither goes through this, and holding the three to one definition would make a wrong answer
 * of two of them.
 */
export function leadDocumentOf(
  row: CaseRow
): { artifactId: string; requestedVersion: number } | null {
  for (const document of row.approval?.documents ?? []) {
    if (document.artifactId !== null && document.requestedVersion !== null) {
      return { artifactId: document.artifactId, requestedVersion: document.requestedVersion };
    }
  }
  return null;
}

/** The option labels one case offered, which is what an answer names. */
export function labelsOfCase(row: CaseRow): string[] {
  const labels: string[] = [];
  for (const option of row.options) {
    const label = option.label?.trim();
    if (label && !labels.includes(label)) labels.push(label);
  }
  return labels;
}

/** When a case was answered, in milliseconds. */
export function answeredTime(row: CaseRow): number {
  const time = Date.parse(row.answeredAt);
  if (Number.isNaN(time)) {
    throw new CorpusError(`case ${row.caseId} was answered at ${row.answeredAt}, which is no time`);
  }
  return time;
}

/** One whole-number flag, at or above `least`. */
export function whole(
  value: string | undefined,
  flag: string,
  fallback: number,
  least: number,
  usage: string
): number {
  if (value === undefined) return fallback;
  const number = Number(value.trim());
  if (!Number.isSafeInteger(number) || number < least) {
    throw new UsageError(`${flag} takes a whole number of ${least} or above\n\n${usage}`);
  }
  return number;
}

/** The time a `--cut` names, which a command may already hold as a date. */
export function cutTime(cut: string | Date): number {
  const time = cut instanceof Date ? cut.getTime() : Date.parse(cut);
  if (Number.isNaN(time)) {
    throw new UsageError(`--cut takes an ISO 8601 time, and ${String(cut)} is not one`);
  }
  return time;
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

/**
 * The corpus a `--corpus` names, read back from the two files this opens of the ones the build
 * wrote: `snapshot.json` and `corpus.jsonl`. The `counts.json` the build writes beside them is
 * a reading of the same rows, and this reader never opens it.
 */
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

/**
 * The sentence an answer ends with where some of its lines hold too few cases to carry a bound:
 * how many of them there are and why they carry none, and nothing at all where every line
 * carries one. `strata.ts` ends its answer over strata with it and `consensus.ts` ends its
 * answer over subsets with it, so the wording, and the verb each count takes, are chosen here
 * once rather than in each command. `MINIMUM_DECIDED` is read wherever a bound is decided.
 */
export function noBoundTail(lines: readonly { bound: number | null }[]): string {
  const small = lines.filter((line) => line.bound === null).length;
  if (small === 0) return "";
  return (
    ` ${count(small)} of ${count(lines.length)} ${small === 1 ? "holds" : "hold"} fewer than ` +
    `${MINIMUM_DECIDED} cases and so ${small === 1 ? "carries" : "carry"} no bound.`
  );
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

/**
 * A number as every one of these records prints one: a share, an agreement or a bound, to three
 * places, and to the places a caller names where what it prints is not a share.
 */
export function num(value: number, places = 3): string {
  return value.toFixed(places);
}

/**
 * A difference of two shares as these records print it, which always carries its sign so that
 * a fall reads as a fall. This is for an estimate: a value whose magnitude rounds away takes
 * `+`, so that two estimates equal at the precision printed are never printed with opposite
 * signs, which is how a reading that moved by nothing came to read as two. A bound takes the
 * other rule and `signedBound` below, so a bound is never printed through this one.
 */
export function signed(value: number): string {
  const magnitude = Math.abs(value).toFixed(3);
  return `${value < 0 && magnitude !== num(0) ? "−" : "+"}${magnitude}`;
}

/**
 * One bound of an interval as these records print it. A bound keeps the sign of a negative
 * value whatever it rounds to: a lower bound of −0.000_02 says the interval reaches below zero,
 * which is the verdict the bound is read for, and printing it `+0.000` beside a verdict saying
 * the arm did not beat the control says the opposite of what was measured. The rule `signed`
 * applies — a magnitude rounding away takes `+` — is about two estimates and is wrong here.
 */
export function signedBound(value: number): string {
  return `${value < 0 ? "−" : "+"}${Math.abs(value).toFixed(3)}`;
}

/** An interval of such differences, as these records print it, bound by bound. */
export function span(interval: Interval): string {
  return `[${signedBound(interval.lower)}, ${signedBound(interval.upper)}]`;
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
