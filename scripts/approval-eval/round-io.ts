import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import type { CaseKind } from "./build-corpus";
import { numberOf, pool, recordOf, stringOf } from "./corpus-io";
import type { SliceName } from "./draw-io";
import type { FaultKind } from "./models";
import type { RecipeName } from "./recipes";
import type { ReplayedDocument } from "./replay";
import type { RetrievalRoute } from "./retrieval";

/**
 * The two files of a round this module owns: `rows.jsonl`, one line per judgement, and the
 * evidence cache, one file per case.
 *
 * `run-round.ts` writes both, and four commands read them back: the runner's own resume,
 * `analyze.ts`, `consensus.ts` and `fresh-agent.ts`. So the shape of each file, where it sits
 * and the rule that reads one record of it live here, and the writer and every reader import
 * them. A reading that had drifted from the writing is what this module is for: a version
 * raised in one copy and not the other, or a row one reader accepted and another refused.
 *
 * A round directory holds a third file, which is not one of these: `round.json`, the summary
 * the runner writes when it stops. Its shape is not kept here because one command writes it
 * and one reads it: `run-round.ts` and, of the commands here, `analyze.ts` alone.
 *
 * The types the row and the cache are built from come from the modules that make those values,
 * and every one of those imports is type-only: it is erased, so importing this module runs no
 * model call, no Dispatch read and no command.
 */

/** Which endpoint an arm's model is called on. */
export type Endpoint = "claude" | "jev";

/** One judgement, as one line of `rows.jsonl`. It holds no Dispatch text of any kind. */
export interface JudgementRow {
  arm: string;
  recipe: RecipeName;
  model: string;
  endpoint: Endpoint;
  caseId: string;
  slice: SliceName;
  kind: CaseKind;
  /** Which of the `--k` judgements of this case and arm this is, counting from 0. */
  judgement: number;
  /** The seed that decided this judgement's evidence order. */
  seed: number;
  /** The option labels the model chose, and empty where no answer could be read. */
  verdict: string[];
  probabilities: Record<string, number> | null;
  confidence: number | null;
  inputTokens: number | null;
  outputTokens: number | null;
  latencyMs: number;
  attempts: number;
  stateCut: boolean;
  /** Which index found the examples, and null for a recipe that carries none. */
  retrievalRoute: RetrievalRoute | null;
  examples: number;
  excluded: number | null;
  promptBytes: number;
  fault: FaultKind | null;
  /** The body of the attempt that ended the ladder, scrubbed of every credential. */
  faultDetail: string | null;
  at: string;
}

/** Where a round's rows sit, which is the file every reader of a round opens first. */
export function rowsPath(round: string): string {
  return join(round, "rows.jsonl");
}

/**
 * One line of `rows.jsonl` read back, or null where it is no row the runner wrote.
 *
 * A line whose `verdict` is not a list is no row of this file, though the runner's own writing
 * never makes one: every reading of a row takes the verdict as a list of labels, so a line
 * carrying anything else would be read as a judgement that chose something unsayable rather
 * than refused. The runner reads such a line as a judgement to ask again, which is what an
 * unreadable row is for.
 */
export function rowOf(line: string): JudgementRow | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return null;
  }
  const record = recordOf(parsed);
  if (record === null) return null;
  if (typeof record.arm !== "string" || typeof record.caseId !== "string") return null;
  if (typeof record.judgement !== "number" || !Array.isArray(record.verdict)) return null;
  // The rest of the line is what the runner wrote, whose shape is `JudgementRow`.
  return record as unknown as JudgementRow;
}

/** What one reading of `rows.jsonl` found: the rows, and how much of the file they were. */
export interface RoundRows {
  rows: JudgementRow[];
  /** How many lines held something, which is what the rows are counted against. */
  read: number;
  /** How many of those lines were no row, which a report prints so the gap is visible. */
  unreadable: number;
}

/**
 * The rows a round has written so far.
 *
 * One policy for a file that is not there, for every reader: it is a round that has written no
 * row, and no failure. A round writes `rows.jsonl` as its first judgement comes back, so the
 * file is missing exactly while the round has nothing to report, which is the same state as a
 * file holding no row — and a reader that threw over the one and reported the other would be
 * calling one state two things. Whether no rows is an error is then each command's own
 * question, answered where it knows what it wanted them for: `analyze.ts` reports a round as
 * far as it has gone, while `consensus.ts` and `fresh-agent.ts` refuse a round naming no case
 * of their slice.
 */
export async function readRows(round: string): Promise<RoundRows> {
  let text: string;
  try {
    text = await readFile(rowsPath(round), "utf8");
  } catch {
    return { rows: [], read: 0, unreadable: 0 };
  }
  const rows: JudgementRow[] = [];
  let unreadable = 0;
  let read = 0;
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    read += 1;
    const row = rowOf(line);
    if (row === null) unreadable += 1;
    else rows.push(row);
  }
  return { rows, read, unreadable };
}

/** The shape of the evidence cache, raised when what a cache holds is no longer what is read. */
export const CACHE_VERSION = 1;

/** Where a round keeps its evidence cache, which holds one file per case. */
export function evidenceDirectory(round: string): string {
  return join(round, "evidence");
}

/** Where one case's evidence is cached, keyed by the digest of its case id. */
export function cachePath(round: string, caseId: string): string {
  const digest = createHash("sha256").update(caseId, "utf8").digest("hex").slice(0, 16);
  return join(evidenceDirectory(round), `${digest}.json`);
}

/** What the retrieval answered for one case, as the cache holds it. */
export interface CachedNearest {
  route: RetrievalRoute;
  excluded: number;
  degraded: string | null;
  allowed: number;
  pages: number;
  neighbours: { caseId: string; place: number; score: number }[];
}

/** What the evidence cache holds for one case. It holds no judgement and no answer. */
export interface CachedEvidence {
  version: number;
  caseId: string;
  builtAt: string;
  issueTitle: string | null;
  documents: ReplayedDocument[];
  /** How many examples the retrieval was run for, so a round wanting more rebuilds it. */
  examples: number;
  nearest: CachedNearest | null;
}

/** Why one case's evidence was not read, which each caller answers for itself. */
export type EvidenceMiss =
  /** No file is cached for that case. */
  | "no file"
  /** A file is there, and it is no cache of that case at this version. */
  | "another case or version";

/** One case's evidence read back, or why it was not. */
export type EvidenceRead =
  | { cache: CachedEvidence; miss: null }
  | { cache: null; miss: EvidenceMiss };

/**
 * What the retrieval recorded, null where it recorded none, and undefined where what the entry
 * holds is no reading of it, which makes the whole entry no cache.
 */
function nearestOf(value: unknown): CachedNearest | null | undefined {
  if (value === null || value === undefined) return null;
  const record = recordOf(value);
  const route = stringOf(record?.route);
  if (record === null || (route !== "search" && route !== "lexical")) return undefined;
  if (!Array.isArray(record.neighbours)) return undefined;
  const neighbours: { caseId: string; place: number; score: number }[] = [];
  for (const one of record.neighbours) {
    const neighbour = recordOf(one);
    const caseId = stringOf(neighbour?.caseId);
    const place = numberOf(neighbour?.place);
    const score = numberOf(neighbour?.score);
    if (caseId === null || place === null || score === null) return undefined;
    neighbours.push({ caseId, place, score });
  }
  return {
    route,
    excluded: numberOf(record.excluded) ?? 0,
    degraded: stringOf(record.degraded),
    allowed: numberOf(record.allowed) ?? 0,
    pages: numberOf(record.pages) ?? 0,
    neighbours,
  };
}

/**
 * One cache entry read from the text of its file, or null where that text is no entry of this
 * version. The entry names the case it is of, so a reader that has one case in hand checks the
 * name and a reader walking the whole cache takes it.
 */
export function cachedEvidenceOf(text: string): CachedEvidence | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  const record = recordOf(parsed);
  const caseId = stringOf(record?.caseId);
  const examples = numberOf(record?.examples);
  if (record === null || caseId === null || examples === null) return null;
  if (numberOf(record.version) !== CACHE_VERSION || !Array.isArray(record.documents)) return null;
  const nearest = nearestOf(record.nearest);
  if (nearest === undefined) return null;
  return {
    version: CACHE_VERSION,
    caseId,
    builtAt: stringOf(record.builtAt) ?? "",
    issueTitle: stringOf(record.issueTitle),
    // The documents are what `replay.ts` handed the runner, whose shape is `ReplayedDocument`.
    documents: record.documents as ReplayedDocument[],
    examples,
    nearest,
  };
}

/**
 * One case's evidence, from the round's own cache. A case the cache does not hold comes back as
 * a `miss` naming why, and never as a throw: what to do about one is the caller's. The runner
 * builds that case's evidence again and writes it, and `fresh-agent.ts`, which scores agents
 * over the round's own evidence and can build none, refuses the case naming the file.
 */
export async function readEvidenceCache(round: string, caseId: string): Promise<EvidenceRead> {
  let text: string;
  try {
    text = await readFile(cachePath(round, caseId), "utf8");
  } catch {
    return { cache: null, miss: "no file" };
  }
  const cache = cachedEvidenceOf(text);
  if (cache === null || cache.caseId !== caseId) {
    return { cache: null, miss: "another case or version" };
  }
  return { cache, miss: null };
}

/** One case's neighbours as the round's evidence cache recorded them. */
export interface CaseNeighbours {
  caseId: string;
  /** The case ids `nearest.neighbours` named, in the order the cache holds them. */
  neighbours: string[];
  /** How many neighbours the retrieval dropped for sitting on this case's own document. */
  excluded: number;
}

/** What one walk of a round's evidence cache read, and what it could not read. */
export interface EvidenceNeighbours {
  /** One entry per cache file read, in the order the directory names them. */
  cases: CaseNeighbours[];
  /**
   * The cache files the walk could not read as an entry of this version: a file of an older
   * version, a truncated one, one holding no JSON, and one that could not be opened. Each is
   * left out of `cases`, and this is what says how many were.
   */
  unreadable: number;
}

/** How many cache files are read at once, the cache holding one file per case. */
const EVIDENCE_CONCURRENCY = 16;

/**
 * The neighbours the round's evidence cache recorded, one entry per case it holds. A round
 * still running has written part of them, and an entry that is not readable is left out and
 * counted: a reader of the proof is told how many cases its numbers are not over, so a cache
 * the round could not finish writing cannot read as a cache it finished.
 */
export async function readEvidenceNeighbours(round: string): Promise<EvidenceNeighbours> {
  const directory = evidenceDirectory(round);
  let names: string[];
  try {
    names = await readdir(directory);
  } catch {
    return { cases: [], unreadable: 0 };
  }
  const files = names.filter((name) => name.endsWith(".json")).sort();
  // Keyed by file, read in whatever order the reads come back, and taken in the order above.
  const held = new Map<string, CaseNeighbours>();
  await pool(files, EVIDENCE_CONCURRENCY, async (name) => {
    let text: string;
    try {
      text = await readFile(join(directory, name), "utf8");
    } catch {
      return;
    }
    const cache = cachedEvidenceOf(text);
    if (cache === null) return;
    held.set(name, {
      caseId: cache.caseId,
      neighbours: (cache.nearest?.neighbours ?? []).map((one) => one.caseId),
      excluded: cache.nearest?.excluded ?? 0,
    });
  });
  const cases = files
    .map((name) => held.get(name))
    .filter((one): one is CaseNeighbours => one !== undefined);
  return { cases, unreadable: files.length - cases.length };
}
