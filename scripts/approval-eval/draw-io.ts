import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import type { CaseKind } from "./build-corpus";
import { CorpusError, listOf, recordOf, stringOf } from "./corpus-io";
import type { ExcludedCase, SliceTargets, UndrawableKind } from "./draw";
import type { IssueFacts, NeverDecideSource } from "./never-decide";
import type { NearDuplicateJoin, ParentChange, TreeOutcome } from "./trees";

/**
 * The two files one draw writes that are read back, each with one owner: `draw.json`, the
 * record the draw writes of itself, and `slices.json`, the case ids it drew into each slice.
 *
 * `draw.ts` writes both into the directory holding the slices, and five commands read them
 * back: `run-round.ts` for the cases of the scored slices and the cut its dating rule applies,
 * `analyze.ts` for those cases, that cut and the key the cases were grouped by, `strata.ts`
 * for the cut, `refusals.ts` for the issue facts the never-decide rule needs, and the claim run's
 * later stages, `s10-inputs.ts` and `analyze.ts --claim-s10`, for the final test's trees and the
 * claim run and build the draw was made in (`claimDrawOf`). So the shape of each file, where it
 * sits and the rule that reads each fact out of it live here, and the writer and every reader
 * import them.
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

/**
 * What the never-decide rule was for one draw, what applying it cost, and what was checked of
 * the text it judged.
 */
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
  /**
   * How many of the rule's answers each model gave, by the model each answer says served it:
   * `the issue's facts` for the kind that takes no call, `none` for an answer naming no model.
   * A draw written before draws recorded it has none.
   */
  servedModels?: Record<string, number>;
  /** One line per case the rule read: where its answer came from and the model that served it. */
  answers?: { caseId: string; source: NeverDecideSource; servedModel: string | null }[];
  /**
   * What was done to the text the rule was handed, and the bound under which that stops
   * holding. A reader of this draw meets the rule's answers in `excluded.held` and may take
   * them for judgements made over checked text; the answers alone say nothing of what was done
   * to the text behind them, so the draw says it here, where that reader is.
   *
   * It is a sentence and not a flag because a flag would let a later commit set it true with
   * nothing true. For the same reason the first part of it is no constant written at the
   * record: `renderChecked` in `render.ts` renders the case and writes that part in the branch
   * that does what it says, so a text no guard ran over cannot be recorded as a checked one.
   * Where that branch ran no guard, the draw joins its own bound - what a later reading of
   * these answers needs first - to the sentence it was handed, since no branch can derive that
   * bound. A draw whose renderings were held to the guard has met that bound already, so its
   * sentence stands alone.
   */
  renderingCheck: string;
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
 * One tree item 6 leaves eligible, as a draw under a registered read records it: every case of
 * the corpus in it, and whether its drawable cases went to report. A draw under a registered read
 * puts every eligible tree in report whole, whatever report's targets, since item 8 lets only
 * never-decide and the run's own (c) and (d) narrow the final test; it applies no never-decide
 * rule (`DrawRecord.neverDecideApplied`), since the claim leaves never-decide to S10's fact check
 * and judges, as they decide it live, and it leaves no case of such a tree out as a question no
 * arm can be scored on. The claim's measure is read over these trees. A tree in no slice is one a
 * draw written before that left every case of out, and it counts there too.
 */
export interface FinalTestTree {
  /** The tree's group key, as `trees.ts` makes it. */
  group: string;
  /** Report, where its drawable cases went, and null where it had no drawable case. */
  slice: "report" | null;
  /** Every case of the corpus in the tree, left-out ones included, sorted. */
  cases: string[];
}

/**
 * The claim run a stage after the build was made in, and the build it read: the reply that
 * started the run (`claimRunOf` in `reserve.ts`) as the build's `snapshot.json` records it, and
 * the sha256 of the bytes read of each of the build's three files (`Corpus` in `corpus-io.ts`,
 * `readTrees` in `trees.ts`). A draw under a registered read records it (`DrawRecord.claimRun`),
 * and every later stage of the run records the same chain and takes only the files it names
 * (`s10-inputs.ts`), so no directory, and no file of one, can be put in another's place.
 */
export interface BuildChain {
  id: string;
  snapshotSha256: string;
  corpusSha256: string;
  treesSha256: string;
}

/** What each field of a `BuildChain` is called where a refusal names it, in the order compared. */
export const BUILD_CHAIN_NAMES: Readonly<Record<keyof BuildChain, string>> = {
  id: "claim run",
  snapshotSha256: "snapshot.json",
  corpusSha256: "corpus.jsonl",
  treesSha256: "trees.json",
};

/**
 * What each of a claim draw's two files is called where a refusal names the sha256 a later stage
 * recorded of it (`ClaimDraw`), in the order compared.
 */
export const DRAW_CHAIN_NAMES: Readonly<Record<"drawSha256" | "slicesSha256", string>> = {
  drawSha256: "draw.json",
  slicesSha256: "slices.json",
};

/**
 * The one comparison every stage of the claim run holds a record to the chain it runs in with:
 * the first field `names` lists in which `found` (a record read back, whatever its shape) is not
 * `wanted`, worded as the tail of a refusal, and null where every such field is the same. A field
 * `found` does not hold is read as of no record.
 */
export function chainRefusal<T extends object>(
  found: unknown,
  wanted: T,
  names: Readonly<Partial<Record<keyof T & string, string>>>
): string | null {
  const record = recordOf(found);
  for (const [field, name] of Object.entries(names) as Array<[keyof T & string, string]>) {
    const was = record?.[field];
    const is = wanted[field];
    if (was === is) continue;
    const shown = was === undefined || was === null ? "of no record" : String(was);
    return `its ${name} is ${shown}, and this run's is ${String(is)}`;
  }
  return null;
}

/**
 * Where a draw's report slice, the final test, came from: the reserve of INFRA-2897's next final
 * test, read under a registration of `claim.ts`, by item 6 of the reserve's version 2
 * (`reserve.ts`, `trees.ts`).
 */
export interface FinalTestDraw {
  /** The reference of the registered read the corpus was read under. */
  reference: string;
  /** The drawn groups item 6 leaves eligible: the only ones report takes. */
  groups: number;
  /** How many trees with a case after the cut fell under each of item 6's outcomes. */
  outcomes: Record<TreeOutcome, number>;
  /**
   * The cases of trees in which an ask opened before the reserve's cut (item 6 a): they went to
   * no slice, and those the reserve holds stay held until it ends.
   */
  beforeCut: { total: number; cases: string[] };
  /**
   * The cases the reserve holds that went to another slice than report: their tree failed item
   * 6 (b), (c) or (d), or report had no room for it. A group goes into one slice whole, so such a
   * case is development data from this draw on. A reserved case the draw left out of every slice
   * is under `excluded`, as every such case is.
   */
  heldOutsideReport: { total: number; cases: string[] };
  /**
   * For each tree with a case after the cut, taken or left out, every change of an issue's parent
   * made after the tree's first ask opened, with the actor that made it (item 6).
   */
  parentChanges: Array<{ group: string; outcome: TreeOutcome; changes: ParentChange[] }>;
  /**
   * The near-duplicate joins the claim grouped by (dispatch://INFRA-3189 v8, "Grouping"): every
   * pair of cases of the corpus, from two groups, whose evidence has a Jaccard similarity of word
   * 5-grams at or above `threshold`, with the groups each case was in before the join.
   */
  nearDuplicates: { threshold: number; joins: NearDuplicateJoin[] };
  /** Every tree item 6 leaves eligible, in the order of its group key. */
  trees: FinalTestTree[];
}

/**
 * What a reader of a draw is handed: the facts `readDraw` read out of the bytes and checked,
 * and nothing it did not check.
 *
 * `draw.json` holds the whole of `DrawRecord` below, and a reader of one needs some of its
 * facts. Those are checked where the file is read, so the type says what the bytes hold
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
  /**
   * The eligible trees of a draw under a registered read, as `finalTest.trees` records them. A
   * draw that read no reserve, or one made before the trees were recorded, carries none.
   */
  finalTestTrees?: readonly FinalTestTree[];
  /** The reference `finalTest` records, for a draw under a registered read of the reserve. */
  finalTestReference?: string;
  /**
   * Whether the draw applied the never-decide rule, where the draw records it, which is never
   * other than whether it records the rule, the roots it was handed and how many it read.
   */
  neverDecideApplied?: boolean;
  /** The claim run and the build a draw under a registered read records. */
  claimRun?: BuildChain;
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
  /** How many roots the never-decide rule was handed; null where the draw applied no rule. */
  backlogLegionRootsRead: number | null;
  /** The roots the never-decide rule was handed; null where the draw applied no rule. */
  backlogLegionRoots: BacklogRoots | null;
  /** What the never-decide rule was for this draw; null where the draw applied no rule. */
  rule: DrawRule | null;
  /**
   * Whether the draw applied the never-decide rule. A draw under a registered read applies none:
   * it reads no backlog roots and puts no case to the rule, so no case is held and none is left
   * out as `rule-unreadable`, and every eligible tree goes to report whole with every case it
   * holds; S10's fact check and judges decide never-decide over the final test, as they decide it
   * live. Every other draw applies it. A draw made before this was recorded carries none, and
   * applied it.
   */
  neverDecideApplied?: boolean;
  /**
   * The claim run a draw under a registered read was made in and the build it read; null for a
   * draw that read no reserve. A draw made before this was recorded carries none.
   */
  claimRun?: BuildChain | null;
  excluded: DrawExcluded;
  /**
   * The final test's source, for a draw made under a registered read of the reserve. Null for a
   * draw that read no reserve, whose report slice holds cases a live scorer may have read and so
   * is no final test. A draw made before this was recorded carries none.
   */
  finalTest?: FinalTestDraw | null;
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
 * What is wrong with one field held as a string, worded as the tail of a refusal, and null
 * where nothing is: a field that is not there at all is no fault, since a fact that is not
 * there is a record that holds none, while a fact that is there and is not what it claims to
 * be is the file being no record of its kind.
 */
function stringFault(record: Record<string, unknown>, name: string): string | null {
  const value = record[name];
  if (value === undefined || value === null) return null;
  return typeof value === "string" ? null : `holds a ${name} that is no string`;
}

/**
 * One field read as a string, null where the record holds none at all, and a refusal where it
 * holds something that is no string: the fault `stringFault` reads, in this reader's words.
 */
function stringAt(record: Record<string, unknown>, name: string, where: string): string | null {
  const fault = stringFault(record, name);
  if (fault !== null) throw new CorpusError(`${where} ${fault}`);
  return stringOf(record[name]);
}

/**
 * One entry of a list of roots, read: the issue's key and the facts the never-decide rule
 * reads, or what is malformed about the entry.
 *
 * This is the one reader of the shape, and the field list lives in it: a root is read here for
 * a draw's `backlogLegionRoots`, for the issue list route (`primary_artifact_id`, which a draw
 * writes as `primaryArtifactId`) and for the file `--backlog-roots` names, so those three
 * cannot come to read different fields or to read them under different policies.
 *
 * One policy, and it is refusal: a field that is there and is not of the type a root holds it
 * for makes the entry no root, and the fault names the field and quotes the offending label.
 * Nothing here is dropped. What that repairs is the one drop the readers it replaced made: the
 * live issue list read each entry with a reader that answered nothing for an entry whose key
 * or status was absent or of the wrong type, and the loop over a page passed that entry over,
 * so the root never reached the never-decide rule (`waitingLegionRoot` in `never-decide.ts`)
 * and the draw recorded a list shorter than the one Dispatch answered without saying so. A
 * parked `legion` root dropped that way is a hold that silently does not happen and nobody is
 * told. A refusal stops the run and names the root and the field it is wrong in, the labels
 * among the rest.
 *
 * The fault is a phrase and not a sentence because each caller words its own refusal around
 * it: a corpus's own draw is a `CorpusError` naming the file, the file a person names on the
 * command line is a usage error naming the flag, and a page of the live issue list is a
 * `DrawError` naming the status it came back under. One reading of the shape, three wordings.
 */
export interface RootReading {
  /** The root, and null where the entry is malformed. */
  root: ({ key: string } & IssueFacts) | null;
  /**
   * Which root it is, where the entry names a key that is a string, so that a refusal can name
   * it; null where it does not, and the caller then says which entry of the list it was.
   */
  key: string | null;
  /** What is malformed, and null where nothing is. */
  fault: string | null;
}

/** One entry of a list of roots, read under the one policy `RootReading` states. */
export function rootReadingOf(value: unknown): RootReading {
  const issue = recordOf(value);
  if (issue === null) return { root: null, key: null, fault: "is no issue" };
  const key = stringOf(issue.key);
  const status = stringOf(issue.status);
  if (key === null || status === null) {
    // Which of the two it is: a key or a status of the wrong type, or neither named at all.
    const fault =
      stringFault(issue, "key") ?? stringFault(issue, "status") ?? "names no key or no status";
    // The key where the entry names one, which is null exactly where the key is the bad field.
    return { root: null, key, fault };
  }
  const held = issue.labels;
  if (held !== undefined && held !== null && !Array.isArray(held)) {
    return { root: null, key, fault: "holds labels that are no list" };
  }
  const labels: string[] = [];
  for (const label of listOf(held)) {
    const read = stringOf(label);
    if (read === null) {
      return {
        root: null,
        key,
        fault: `names a label that is no string: ${JSON.stringify(label)}`,
      };
    }
    labels.push(read);
  }
  const fault =
    stringFault(issue, "parent") ??
    stringFault(issue, "primary_artifact_id") ??
    stringFault(issue, "primaryArtifactId");
  if (fault !== null) return { root: null, key, fault };
  return {
    root: {
      key,
      parent: stringOf(issue.parent),
      labels,
      status,
      primaryArtifactId: stringOf(issue.primary_artifact_id) ?? stringOf(issue.primaryArtifactId),
    },
    key,
    fault: null,
  };
}

/** One entry of a draw's roots, whose every fault is the file being no draw. */
function rootAt(value: unknown, at: number, path: string): { key: string } & IssueFacts {
  const read = rootReadingOf(value);
  if (read.root === null) throw new CorpusError(`${path} root ${read.key ?? at + 1} ${read.fault}`);
  return read.root;
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
 * The eligible trees a draw under a registered read recorded, and none where it recorded no
 * final test or recorded one before the trees were. A tree that names no group, a group named
 * twice, a slice that is none of the four, a case that is no string, or a case named twice, is
 * the file being no draw: the claim's measure counts trees, so a tree read wrongly is a count
 * read wrongly, and one tree written as two would count twice.
 */
function finalTestTreesAt(value: unknown, path: string): FinalTestTree[] | null {
  if (value === undefined || value === null) return null;
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${path} holds a finalTest that is no record`);
  const held = record.trees;
  if (held === undefined) return null;
  if (!Array.isArray(held)) throw new CorpusError(`${path} holds finalTest trees that are no list`);
  const groups = new Set<string>();
  const seen = new Map<string, string>();
  return held.map((entry, at) => {
    const tree = recordOf(entry);
    const group = tree === null ? null : stringOf(tree.group);
    if (tree === null || group === null || group === "") {
      throw new CorpusError(`${path} finalTest tree ${at + 1} names no group`);
    }
    if (groups.has(group)) throw new CorpusError(`${path} names the finalTest tree ${group} twice`);
    groups.add(group);
    // A tree with no drawable case is written with a slice of null, so a missing slice is no
    // tree a draw wrote, not a tree in no slice; and a draw under a registered read puts every
    // eligible tree in report, so a tree in another slice is no such draw either.
    const slice = tree.slice;
    if (slice !== null && slice !== "report") {
      throw new CorpusError(
        `${path} finalTest tree ${group} names no slice, or another than report, where a draw ` +
          "under a registered read puts every eligible tree"
      );
    }
    if (!Array.isArray(tree.cases)) {
      throw new CorpusError(`${path} finalTest tree ${group} holds no list of cases`);
    }
    const cases = tree.cases.map((one) => {
      const caseId = stringOf(one);
      if (caseId === null) {
        throw new CorpusError(`${path} finalTest tree ${group} names a case that is no string`);
      }
      const other = seen.get(caseId);
      if (other !== undefined) {
        throw new CorpusError(
          `${path} names the case ${caseId} in the trees ${other} and ${group}`
        );
      }
      seen.set(caseId, group);
      return caseId;
    });
    return { group, slice, cases };
  });
}

/**
 * The reference of the registered read a draw's final test records, and none where the draw
 * recorded no final test. A reference that is there and is no string is the file being no draw.
 */
function finalTestReferenceAt(value: unknown, path: string): string | null {
  const record = recordOf(value);
  if (record === null) return null;
  const fault = stringFault(record, "reference");
  if (fault !== null) throw new CorpusError(`${path} finalTest ${fault}`);
  return stringOf(record.reference);
}

/**
 * The claim run a draw under a registered read records, and none where the draw recorded none.
 * One that is there and names no reply id, or no sha256 of each of the build's three files, is
 * the file being no draw: a stage after it reads the claim run's chain from it.
 */
function claimRunAt(value: unknown, path: string): BuildChain | null {
  if (value === undefined || value === null) return null;
  const record = recordOf(value);
  const id = stringOf(record?.id);
  const [snapshotSha256, corpusSha256, treesSha256] = [
    record?.snapshotSha256,
    record?.corpusSha256,
    record?.treesSha256,
  ].map((one) => {
    const sha256 = stringOf(one);
    return sha256 !== null && /^[0-9a-f]{64}$/.test(sha256) ? sha256 : null;
  });
  if (!id || !snapshotSha256 || !corpusSha256 || !treesSha256) {
    throw new CorpusError(
      `${path} holds a claimRun that names no reply id, or no sha256 of each of the build's ` +
        "snapshot.json, corpus.jsonl and trees.json"
    );
  }
  return { id, snapshotSha256, corpusSha256, treesSha256 };
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
  return drawFactsOf(text, path);
}

/**
 * The facts of a `draw.json` whose text the caller read itself, under `readDraw`'s one policy:
 * `s10-inputs.ts` reads the bytes once, so the sha256 it records is of the bytes these facts
 * were read from.
 */
export function drawFactsOf(text: string, path: string): DrawFacts {
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
  const trees = finalTestTreesAt(record.finalTest, path);
  if (trees !== null) facts.finalTestTrees = trees;
  const reference = finalTestReferenceAt(record.finalTest, path);
  if (reference !== null) facts.finalTestReference = reference;
  const applied = record.neverDecideApplied;
  if (applied !== undefined && typeof applied !== "boolean") {
    throw new CorpusError(`${path} holds a neverDecideApplied that is no boolean`);
  }
  // One fact written in four fields: a draw that says it applied the rule records the rule, the
  // roots and how many it read, and one that says it applied none records none of them.
  const ruled = [record.rule, record.backlogLegionRoots, record.backlogLegionRootsRead].map(
    (one) => one !== null && one !== undefined
  );
  if (applied !== undefined && ruled.some((one) => one !== applied)) {
    throw new CorpusError(
      `${path} holds a neverDecideApplied of ${applied} beside ` +
        `${ruled[0] ? "a" : "no"} rule, ${ruled[1] ? "" : "no "}backlog roots and ` +
        `${ruled[2] ? "a" : "no"} count of them read`
    );
  }
  if (applied !== undefined) facts.neverDecideApplied = applied;
  const claimRun = claimRunAt(record.claimRun, path);
  if (claimRun !== null) facts.claimRun = claimRun;
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
  return slicesOf(text, path);
}

/** The slices of a `slices.json` whose text the caller read itself, as `readSlices` reads them. */
export function slicesOf(text: string, path: string): DrawnSlices {
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

/** The claim's draw as a later stage of the claim run reads it, with the sha256 of its two files. */
export interface ClaimDraw {
  directory: string;
  trees: readonly FinalTestTree[];
  report: readonly string[];
  drawSha256: string;
  slicesSha256: string;
}

/** The bytes of one of a draw's files, refused where they are not there. */
async function drawBytesOf(path: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new CorpusError(`${path} could not be read: --draw takes the directory the draw wrote`);
  }
}

/**
 * The claim's draw at `directory`, as every later stage of the claim run takes it
 * (`s10-inputs.ts`, `analyze.ts --claim-s10`): refused unless the claim run made it under the
 * read `reference` from the build `build` names, as its record says (the claim run and the sha256
 * of each of the build's three files, each the bytes the build directory holds now, in one
 * `chainRefusal`), and records that it applied no never-decide rule and the final test's trees.
 * Each file is read once, so the sha256 a stage records is of the bytes these facts came from.
 */
export async function claimDrawOf(
  directory: string,
  build: BuildChain,
  reference: string
): Promise<ClaimDraw> {
  const drawFile = drawPath(directory);
  const slicesFile = slicesPath(directory);
  const drawBytes = await drawBytesOf(drawFile);
  const slicesBytes = await drawBytesOf(slicesFile);
  const facts = drawFactsOf(drawBytes.toString("utf8"), drawFile);
  const refusal = chainRefusal(facts.claimRun, build, BUILD_CHAIN_NAMES);
  if (refusal !== null) {
    throw new CorpusError(
      `${drawFile} is not this claim run's draw of the build it reads: ${refusal}. A stage ` +
        "takes only its own claim run's draw of the build's bytes now"
    );
  }
  if (facts.finalTestReference !== reference) {
    throw new CorpusError(
      `${drawFile} was drawn under ${facts.finalTestReference ?? "no read of the reserve"}, and ` +
        `this run reads under ${reference}`
    );
  }
  if (facts.neverDecideApplied !== false) {
    throw new CorpusError(
      `${drawFile} does not record that it applied no never-decide rule, which the claim's draw ` +
        "does not apply (draw.ts)"
    );
  }
  if (facts.finalTestTrees === undefined) {
    throw new CorpusError(`${drawFile} records no final test's trees`);
  }
  return {
    directory,
    trees: facts.finalTestTrees,
    report: slicesOf(slicesBytes.toString("utf8"), slicesFile).report,
    drawSha256: createHash("sha256").update(drawBytes).digest("hex"),
    slicesSha256: createHash("sha256").update(slicesBytes).digest("hex"),
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
