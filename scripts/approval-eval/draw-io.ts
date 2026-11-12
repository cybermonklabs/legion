import { readFile } from "node:fs/promises";
import { join } from "node:path";
import type { CaseKind } from "./build-corpus";
import { CorpusError, listOf, recordOf, stringOf } from "./corpus-io";
import type { ExcludedCase, SliceTargets, UndrawableKind } from "./draw";
import type { IssueFacts } from "./never-decide";

/**
 * The two files one draw writes that are read back, each with one owner: `draw.json`, the
 * record the draw writes of itself, and `slices.json`, the case ids it drew into each slice.
 *
 * `draw.ts` writes both into the directory holding the slices, and four commands read them
 * back: `run-round.ts` for the cases of the scored slices and the cut its dating rule applies,
 * `analyze.ts` for those cases, that cut and the key the cases were grouped by, `strata.ts`
 * for the cut, and `refusals.ts` for the issue facts the never-decide rule needs. So the shape
 * of each file, where it sits and the rule that reads each fact out of it live here, and the
 * writer and every reader import them.
 *
 * A draw written before a field was recorded does not carry it, which is why `readDraw` hands
 * back `DrawFacts` and not the record's own type: the record's type says what a draw writes
 * now, and `DrawFacts` says what this draw was read to hold. A fact is checked once, where
 * the bytes are read, so no caller reads a field a second time to learn whether it is really
 * there and really a string.
 *
 * The types the records are built from come from `draw.ts`, which makes those values, and that
 * import is type-only: it is erased, so importing this module runs none of the draw. The names
 * of the slices are the other way about, and live here: they are what `slices.json` holds, and
 * a module reading that file may not import the draw itself to learn them. The three a round
 * scores are the same thing, so `run-round.ts` and `analyze.ts` take them from here as well.
 */

/** One slice of the draw. */
export type SliceName = "fit" | "report" | "anchor" | "reserve";

/** The four slices, in the order a group is offered to them. */
export const SLICES: readonly SliceName[] = ["fit", "report", "anchor", "reserve"];

/** The three slices every number of a round is read over, in the order they are printed. */
export const SCORED_SLICES = ["fit", "report", "anchor"] as const;

/** One of the three slices a round scores and a manifest is written for. */
export type ScoredSlice = (typeof SCORED_SLICES)[number];

/**
 * The roots carrying the `legion` label that stand on the backlog or in icebox, as `draw.json`
 * writes them. A reader of a draw is handed the issues alone, as `DrawFacts` carries them: the
 * reading time and the source are provenance of the writing and no fact of the draw.
 */
export interface BacklogRoots {
  /** When the list was read from Dispatch, as the run that read it stamped it. */
  readAt: string;
  /** Where this run took it from: `dispatch`, or the file it was pinned from. */
  source: string;
  /** One entry per issue: its key, and the facts the never-decide rule reads. */
  issues: Array<{ key: string } & IssueFacts>;
}

/** What the never-decide rule was for one draw, and what applying it cost. */
export interface DrawRule {
  model: string;
  /** The SHA-256 of the prose the rule applied: another definition is another reading. */
  definitionSha256: string;
  /** The directory the rule's answers were kept in, and null where none was. */
  cache: string | null;
  asked: number;
  cached: number;
  inputTokens: number;
  outputTokens: number;
}

/** What the draw left out, counted and listed by the rule that left it out. */
export interface DrawExcluded {
  held: { total: number; byKind: Record<string, number>; cases: ExcludedCase[] };
  undrawable: {
    total: number;
    byKind: Record<UndrawableKind, number>;
    cases: ExcludedCase[];
  };
}

/**
 * What a reader of a draw is handed: the facts `readDraw` read out of the bytes and checked,
 * and nothing it did not check.
 *
 * `draw.json` holds the whole of `DrawRecord` below, and a reader of one needs three of its
 * facts. Those three are checked where the file is read, so the type says what the bytes hold
 * rather than what a draw of the current shape would have written, and a fact a draw does not
 * carry - one of an earlier shape, or one holding something that is no such fact - is absent
 * here rather than read a second time by every caller.
 */
export interface DrawFacts {
  /**
   * The cut the draw was made at, as an ISO 8601 time that parses. A draw naming none, or
   * naming something that is no time, carries none: what a command does without a cut is its
   * own, and `run-round.ts` refuses the draw where `strata.ts` reads the corpus whole.
   */
  cut?: string;
  /** What the cases were grouped by. A draw made before this was recorded carries none. */
  groupKey?: string;
  /**
   * The roots the never-decide rule was handed, as the draw recorded them under
   * `backlogLegionRoots`. A draw recording none carries none. The reading time and the source
   * that record carries are provenance no reader of a draw reads, and are not read here.
   */
  roots?: ReadonlyArray<{ key: string } & IssueFacts>;
}

/** What one draw recorded of itself, which is what `draw.json` holds. */
export interface DrawRecord {
  /** The corpus the draw was made over, resolved. */
  corpus: string;
  /** The cut the draw was made at, as an ISO 8601 time. */
  cut: string;
  seed: number;
  targets: Readonly<Record<CaseKind, SliceTargets>>;
  counts: Record<SliceName, Record<CaseKind, number>>;
  /** What the cases were grouped by. A draw made before this was recorded carries none. */
  groupKey?: string;
  /** How many groups there were under that key, where the draw recorded the key. */
  groupCount?: number;
  groups: Record<CaseKind, number>;
  postCutCases: number;
  documentsUnread: number;
  backlogLegionRootsRead: number;
  backlogLegionRoots: BacklogRoots;
  rule: DrawRule;
  excluded: DrawExcluded;
}

/**
 * The case ids one draw drew into each slice, which is what `slices.json` holds. A slice the
 * file names no case under holds none: a draw puts a case in exactly one slice, so a slice
 * standing empty and a slice a draw of an earlier shape never wrote are the same thing to
 * every reader of them.
 */
export type DrawnSlices = Record<SliceName, string[]>;

/** Where a draw writes its record, in the directory holding the slices it drew. */
export function drawPath(slices: string): string {
  return join(slices, "draw.json");
}

/** Where a corpus carries the draw made over it, which is the `slices` directory beside it. */
export function corpusDrawPath(corpus: string): string {
  return drawPath(join(corpus, "slices"));
}

/** Where a draw writes the case ids it drew, in the directory holding the slices. */
export function slicesPath(slices: string): string {
  return join(slices, "slices.json");
}

/** Where a corpus carries the slices drawn over it, which is that same directory beside it. */
export function corpusSlicesPath(corpus: string): string {
  return slicesPath(join(corpus, "slices"));
}

/**
 * One field read as a string, null where the record holds none at all, and a refusal where it
 * holds something that is no string: a fact that is there and is not what it claims to be is
 * the file being no draw, while a fact that is not there is a draw that recorded none.
 */
function stringAt(record: Record<string, unknown>, name: string, where: string): string | null {
  const value = record[name];
  if (value === undefined || value === null) return null;
  if (typeof value !== "string") {
    throw new CorpusError(`${where} holds a ${name} that is no string`);
  }
  return value;
}

/**
 * One entry of a draw's roots: the issue's key, and the facts the never-decide rule reads.
 *
 * `issueFactsOf` builds those facts, here as for the issue list route, so which fields a root
 * carries is kept in one place and a draw's roots cannot come to carry another list than the
 * route's. What a draw adds is its own policy on how they are read: a field that is there and
 * is not of the type the record holds it for stops the reading, where that reader takes what it
 * can read and leaves the rest null.
 */
function rootAt(value: unknown, at: number, path: string): { key: string } & IssueFacts {
  const issue = recordOf(value);
  if (issue === null) throw new CorpusError(`${path} root ${at + 1} is no issue`);
  const where = `${path} root ${at + 1}`;
  const read = issueFactsOf(issue);
  if (read === null) {
    // Which of the two it is: a key or a status of the wrong type, or neither named at all.
    stringAt(issue, "key", where);
    stringAt(issue, "status", where);
    throw new CorpusError(`${where} names no key or no status`);
  }
  const key = read.key;
  const held = issue.labels;
  if (held !== undefined && held !== null && !Array.isArray(held)) {
    throw new CorpusError(`${path} root ${key} holds labels that are no list`);
  }
  for (const label of listOf(held)) {
    if (typeof label !== "string") {
      throw new CorpusError(`${path} root ${key} names a label that is no string`);
    }
  }
  stringAt(issue, "parent", `${path} root ${key}`);
  stringAt(issue, "primaryArtifactId", `${path} root ${key}`);
  return { key, ...read.facts };
}

/** The roots a draw recorded, read entry by entry, which is what a reader is handed. */
function rootsAt(value: unknown, path: string): Array<{ key: string } & IssueFacts> {
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${path} holds backlogLegionRoots that are no record`);
  if (!Array.isArray(record.issues)) {
    throw new CorpusError(`${path} holds no list of roots in backlogLegionRoots`);
  }
  return record.issues.map((entry, at) => rootAt(entry, at, path));
}

/**
 * The draw at `path`, and null where there is none there.
 *
 * One policy for every reader: a directory holding no `draw.json` carries no draw, and a
 * `draw.json` that is not the record a draw writes stops the reading. A corpus whose draw is
 * corrupt is not a corpus that was never drawn, and reading it as one would answer a question
 * about the whole corpus where the cases of a slice were asked for.
 *
 * Which of the two a file is, is settled here, fact by fact, so what comes back is what was
 * read and not what the writer's type would have claimed. A fact a draw does not carry is
 * absent from the answer: a draw of an earlier shape, a cut that is no time, and a group key
 * written empty are all draws that record that fact nowhere, and each reader answers for
 * itself what it does without it. A fact that is there and is not what the record holds that
 * field for - a cut that is no string, a root naming no key - stops the reading, because such
 * a file is no record a draw wrote.
 */
export async function readDraw(path: string): Promise<DrawFacts | null> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${path} is not JSON`);
  }
  const record = recordOf(parsed);
  if (record === null) throw new CorpusError(`${path} is not a draw`);
  const facts: DrawFacts = {};
  const cut = stringAt(record, "cut", path);
  if (cut !== null && !Number.isNaN(Date.parse(cut))) facts.cut = cut;
  const groupKey = stringAt(record, "groupKey", path);
  if (groupKey !== null && groupKey !== "") facts.groupKey = groupKey;
  const roots = record.backlogLegionRoots;
  if (roots !== undefined && roots !== null) facts.roots = rootsAt(roots, path);
  return facts;
}

/**
 * The slices at `path`, and null where there is none there.
 *
 * One policy for both readers, and the same one the draw's own record is read under: a
 * directory holding no `slices.json` carries no slices, and a `slices.json` that is not the
 * case ids a draw writes stops the reading. A case id that is no string is that second thing:
 * one reader dropped it and the other took it as the empty id and then refused the corpus for
 * not holding a case of that name, and both read a corrupt file as a shorter draw.
 */
export async function readSlices(path: string): Promise<DrawnSlices | null> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new CorpusError(`${path} is not JSON`);
  }
  const record = recordOf(parsed);
  if (record === null) throw new CorpusError(`${path} is not a draw's slices`);
  const slices = {} as DrawnSlices;
  for (const slice of SLICES) {
    const held = record[slice];
    if (held === undefined) {
      slices[slice] = [];
      continue;
    }
    if (!Array.isArray(held)) {
      throw new CorpusError(`${path} holds no list of case ids in the ${slice} slice`);
    }
    const ids: string[] = [];
    for (const one of held) {
      const caseId = stringOf(one);
      if (caseId === null) {
        throw new CorpusError(`${path} names a case that is no string in the ${slice} slice`);
      }
      ids.push(caseId);
    }
    slices[slice] = ids;
  }
  return slices;
}

/**
 * The issue facts of one entry of a list of roots, as the issue list route answers them
 * (`primary_artifact_id`) or as a draw holds them (`primaryArtifactId`), so one reader serves
 * both, and null where the entry is no issue.
 */
export function issueFactsOf(value: unknown): { key: string; facts: IssueFacts } | null {
  const issue = recordOf(value);
  const key = stringOf(issue?.key);
  const status = stringOf(issue?.status);
  if (key === null || status === null) return null;
  return {
    key,
    facts: {
      parent: stringOf(issue?.parent),
      labels: listOf(issue?.labels)
        .map(stringOf)
        .filter((label): label is string => label !== null),
      status,
      primaryArtifactId: stringOf(issue?.primary_artifact_id) ?? stringOf(issue?.primaryArtifactId),
    },
  };
}

/**
 * The facts of every root a reading of them holds, by issue key, which is how the never-decide
 * rule is handed them. Empty where there are none: a draw that recorded no roots, and a
 * reading of Dispatch that found none, are the same thing to the rule.
 */
export function rootFactsOf(
  roots: ReadonlyArray<{ key: string } & IssueFacts> | null | undefined
): Map<string, IssueFacts> {
  const facts = new Map<string, IssueFacts>();
  for (const { key, ...issue } of roots ?? []) facts.set(key, issue);
  return facts;
}
