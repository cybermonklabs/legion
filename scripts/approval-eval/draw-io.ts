import { readFile } from "node:fs/promises";
import { join } from "node:path";
import type { CaseKind } from "./build-corpus";
import { CorpusError, listOf, recordOf, stringOf } from "./corpus-io";
import type { ExcludedCase, SliceName, SliceTargets, UndrawableKind } from "./draw";
import type { IssueFacts } from "./never-decide";

/**
 * `draw.json`, the record one draw writes of itself, with one owner.
 *
 * `draw.ts` writes it beside the slices it drew, and four commands read it back: `run-round.ts`
 * for the cut its dating rule applies, `analyze.ts` for that cut and the key the cases were
 * grouped by, `strata.ts` for the cut, and `refusals.ts` for the issue facts the never-decide
 * rule needs. So the shape of the file, where it sits and the rule that reads each fact out of
 * it live here, and the writer and every reader import them.
 *
 * A draw written before a field was recorded does not carry it, which is why every reading of
 * a field goes through one of the readers below rather than through the type: the type says
 * what a draw writes now, and the readers say what a draw of any age answers.
 *
 * The types the record is built from come from `draw.ts`, which makes those values, and that
 * import is type-only: it is erased, so importing this module runs none of the draw.
 */

/**
 * The roots carrying the `legion` label that stand on the backlog or in icebox, as `draw.json`
 * writes them and reads them.
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

/** Where a draw writes its record, in the directory holding the slices it drew. */
export function drawPath(slices: string): string {
  return join(slices, "draw.json");
}

/** Where a corpus carries the draw made over it, which is the `slices` directory beside it. */
export function corpusDrawPath(corpus: string): string {
  return drawPath(join(corpus, "slices"));
}

/**
 * The draw at `path`, and null where there is none there.
 *
 * One policy for every reader: a directory holding no `draw.json` carries no draw, and a
 * `draw.json` that is not the record a draw writes stops the reading. A corpus whose draw is
 * corrupt is not a corpus that was never drawn, and reading it as one would answer a question
 * about the whole corpus where the cases of a slice were asked for.
 */
export async function readDraw(path: string): Promise<DrawRecord | null> {
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
  // The rest of the file is what the draw wrote, whose shape is `DrawRecord`.
  return record as unknown as DrawRecord;
}

/** The cut a draw was made at, and null where it records none or records no time. */
export function cutOf(draw: DrawRecord | null): string | null {
  const cut = stringOf(draw?.cut ?? null);
  return cut !== null && !Number.isNaN(Date.parse(cut)) ? cut : null;
}

/** What a draw grouped its cases by, and null for a draw made before that key was recorded. */
export function groupKeyOf(draw: DrawRecord | null): string | null {
  const key = stringOf(draw?.groupKey ?? null);
  return key !== null && key !== "" ? key : null;
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
 * The facts of every root a list of roots holds, by issue key, which is how the never-decide
 * rule is handed them. Empty where the list holds none, and an entry that is no issue is left
 * out rather than stopping the reading.
 */
export function rootFactsOf(roots: unknown): Map<string, IssueFacts> {
  const facts = new Map<string, IssueFacts>();
  for (const value of listOf(recordOf(roots)?.issues)) {
    const read = issueFactsOf(value);
    if (read !== null) facts.set(read.key, read.facts);
  }
  return facts;
}
