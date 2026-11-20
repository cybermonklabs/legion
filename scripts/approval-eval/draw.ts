import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  answeredTime,
  CannotRunError,
  type Corpus,
  cutTime,
  listOf,
  newSeed,
  outDirectory,
  pool,
  randomStream,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import { type DispatchConfig, dispatchConfig, readJson } from "./dispatch-read";
import {
  type BacklogRoots,
  BUILD_CHAIN_NAMES,
  type BuildChain,
  chainRefusal,
  type DrawnSlices,
  type DrawRecord,
  type DrawRule,
  drawFactsOf,
  drawPath,
  type FinalTestDraw,
  type FinalTestTree,
  rootFactsOf,
  rootReadingOf,
  SLICES,
  type SliceName,
  slicesPath,
} from "./draw-io";
import {
  type AskModel,
  DEFAULT_NEVER_DECIDE_MODEL,
  type IssueFacts,
  type NeverDecideAnswer,
  type NeverDecideKind,
  type NeverDecideOptions,
  neverDecide,
  neverDecideDefinition,
} from "./never-decide";
import { type RenderGuard, renderCase, renderChecked, renderDocument } from "./render";
import { replayCase } from "./replay";
import {
  RESERVE_TREE_CUT,
  RESERVE_VARIABLE,
  type Registration,
  readReferenceOf,
  reserveOf,
} from "./reserve";
import {
  changesAfterAskOf,
  idsInFiles,
  NEAR_DUPLICATE_JACCARD,
  readTrees,
  shinglesOf,
  TREE_OUTCOMES,
  type TreeGrouping,
  type TreeOutcome,
  treeGroupingOf,
} from "./trees";

/**
 * Draws the post-cut cases of a corpus into the four slices the pre-analysis plan's table names:
 * fit, report, anchor and reserve (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2, "The
 * slices").
 *
 * A case's group is its issue, and a whole group goes into one slice, so two decisions of one
 * subject never land on opposite sides of a split: a document belongs to one issue, so this
 * grouping holds every case of a document together as well. A group whose size would take a
 * slice past its target goes to the next slice, which is why the targets are reached to within
 * one group. The draw is at random under a seed it prints, so the same seed over the same
 * corpus draws the same slices. `draw.json` records the key the groups were taken under and how
 * many groups there were, since a later reading of the slices turns on which key was used.
 *
 * Four kinds of case never reach the draw. A question offering fewer than two options cannot be
 * judged as a choice, and a question whose answer names no option cannot be scored against what
 * the person chose, since set equality with an empty answer is nothing an arm could agree with.
 * A case the never-decide rule holds is one the shipped skill would refuse, and a coverage
 * number counting it would be larger than anything the skill can deliver; the counts and the
 * kinds held are printed and written. And a case whose answer from the rule could not be read
 * is left out as well, because a rule about what must not be decided may not decide by
 * accident; those are counted and named too. A draw under a registered read of the reserve
 * applies no never-decide rule, so the last two kinds do not arise in it, and leaves no case of
 * an eligible tree out as either of the first two (below).
 *
 * An approval case whose answer names no option stops the whole draw instead: every approval
 * case of this corpus carries the option the person chose, so one that does not is the corpus
 * read wrongly and not a case to pass over.
 *
 * The report slice is the final test (`analyze.ts` calls it "the test set"), and item 6 of the
 * reserve's version 2 (`reserve.ts`, dispatch://INFRA/artifact/infra-2897-final-test-reserve-md)
 * says which groups it may take. A draw over a corpus read under a registered read of the reserve
 * is the claim's: it groups the cases by issue tree over every parent link the log recorded
 * (`trees.json`, `trees.ts`) instead of by issue, joins two groups wherever a case of each has
 * near-duplicate evidence (a Jaccard similarity of word 5-grams of 0.9 or more,
 * dispatch://INFRA-3189 v8, "Grouping"), and puts each group under item 6, over every case of the
 * corpus in it and not only the drawn ones. A group in which any ask of anyone opened before the
 * reserve's cut (a) gives no case to any slice. A group holding a case the reserve does not hold,
 * or one whose root at its ask's opening the rule of its time did not hold (b), one with an ask a
 * scorer's record names (c, `--scorer-records`), or one with an ask a session of the claim's round
 * opened (d, `--round-sessions`), goes to a development slice whole, and the reserved cases it
 * holds are named there. Only the other groups may go into report, and they try it first.
 * `draw.json` counts the groups under each outcome, lists each near-duplicate join, and lists, for
 * each group with any, the parent changes made after its first ask opened, with the actor that
 * made each. The claim's draw applies no never-decide rule, which reads Dispatch as it stands at
 * the run (the claim's point 9): it reads no backlog roots and puts no case to the rule, so no
 * case is held and none is left out as `rule-unreadable`; no case of an eligible tree is left out
 * as a question no arm can be scored on either, so every eligible tree goes to report whole with
 * every case it holds, and S10's fact check and judges decide never-decide, as they do live.
 * `draw.json` records that the rule was not applied (`neverDecideApplied`), and the claim run and
 * the sha256 of each of the three files of the build it read, `snapshot.json`, `corpus.jsonl` and
 * `trees.json` (`claimRun`), so the next stage of the run (`s10-inputs.ts`) takes only this draw
 * of that build's bytes. Such a draw refuses an `--out` holding another draw's record, one of
 * another claim run or another build, or files and no record (`claimOutRefusal`); into its own
 * claim run's directory over the same build it writes the same files again, the draw being
 * determined by the corpus and the seed. A draw that reads no reserve groups by issue as before,
 * and `draw.json` records that its report slice is no final test.
 *
 * What is pinned and what is live. The cases are the corpus's, and each document is read at the
 * version the corpus pins, so those are the same bytes on every run. The one live reading is
 * the list of roots carrying the `legion` label that stand on the backlog or in icebox, which
 * the never-decide rule needs and which Dispatch answers as it stands now: `draw.json` writes
 * that list, with the time it was read, beside its count. `--backlog-roots <file>` reads the
 * list from a file in the shape `draw.json` writes instead of from Dispatch, so a re-run that
 * must reproduce an earlier draw passes both `--seed` and `--backlog-roots <that draw's
 * draw.json>` and then turns on the corpus and the seed alone. The claim's draw reads no such
 * list, and refuses `--backlog-roots` and `--cache`.
 *
 * The rule itself is a model call per case. `--cache <dir>` keeps those answers between runs,
 * keyed on the prose of the definition among other things, so a redraw of a frozen definition
 * pays for none of them again and a changed definition is asked afresh by construction.
 *
 * The anchor slice is written with a descriptor per case - the ask, the first document of the
 * request that could be read, the version it was asked about and the SHA-256 of that one
 * document's body as `renderDocument` renders it, the rest of the rendered case being outside
 * the digest; a case with no readable document is digested over its whole rendering. Nothing
 * here reads `anchor-descriptors.json` back: it is written for a later round, whose own digest
 * of a case differs as soon as the bytes it covers do.
 */

const USAGE = `usage: bun scripts/approval-eval/draw.ts --corpus <dir> --cut <date> --out <dir> \\
  [--seed <n>] [--backlog-roots <file>] [--cache <dir>] \\
  [--scorer-records <file>]... [--round-sessions <file>]

Draws the post-cut cases of a corpus into the fit, report, anchor and reserve slices. Under a
registered read of INFRA-2897's reserve (APPROVAL_EVAL_RESERVE, claim.ts), which takes the
--seed its registration names, the cases are grouped by issue tree and report, the final test,
takes only the groups item 6 of the reserve's version 2 leaves eligible, and they try it first;
such a draw applies no never-decide rule, so it takes no --backlog-roots and no --cache.

Options:
  --corpus <dir>          Required. A directory holding corpus.jsonl and snapshot.json, and
                          trees.json for a draw under a read of the reserve.
  --cut <date>            Required. An ISO 8601 time, for example 2026-11-05T00:00:00Z. A case
                          answered at or after it is drawn; a case answered before it is
                          training.
  --out <dir>             Required. Where slices.json, anchor-descriptors.json and draw.json
                          are written. An absolute path outside this repository's workspace.
                          Under a read of the reserve, a directory holding another claim run's
                          draw, a draw of another build, or files and no draw.json is refused.
  --seed <n>              The seed of the draw. With none, one is made and printed.
  --backlog-roots <file>  The roots carrying the legion label that stand on the backlog or in
                          icebox, read from this file rather than from Dispatch. The shape is
                          the one draw.json writes, so an earlier draw's draw.json names its
                          own list. With --seed, this makes a re-run reproduce that draw from
                          the corpus alone. Refused under a read of the reserve.
  --cache <dir>           Where the never-decide rule's answers are kept between runs. Without
                          it every case is asked of the model again. Refused under a read of
                          the reserve.
  --scorer-records <file> Required under a read of the reserve, and refused without one; may be
                          given more than once. A record of what a live scorer read: every id
                          it names counts as an ask the scorer read (item 6 c). A file that names
                          no id is refused.
  --round-sessions <file> Required under a read of the reserve, and refused without one. The ids
                          of the sessions of the round that makes the claim (item 6 d). A file
                          that names no id is refused.

Exit status: 0 when the draw was written, 2 when it could not run.`;

/** How many cases of each kind each slice is drawn for. The rest is the reserve. */
export interface SliceTargets {
  fit: number;
  report: number;
  anchor: number;
}

/** The targets of the plan's table, by kind of case. */
export const TARGETS: Readonly<Record<CaseKind, SliceTargets>> = {
  approval: { fit: 80, report: 250, anchor: 48 },
  question: { fit: 20, report: 50, anchor: 12 },
};

/** The kinds of case, in the order every count of them is held and printed. */
export const KINDS: readonly CaseKind[] = ["approval", "question"];

/** What a case is grouped by, which `draw.json` records and a later reading of it reads. */
export const GROUP_KEY = "issue";

/**
 * What a draw under a read of the reserve groups by: the issue tree over every parent link,
 * joined over near-duplicate evidence.
 */
export const TREE_GROUP_KEY = "tree";

/** How many option a question must offer to be judged as a choice at all. */
const LEAST_OPTIONS = 2;

/**
 * How many cases are read from Dispatch and put to the never-decide rule at once. The rule is
 * one model call per case, so this is what the whole draw's wall time turns on.
 */
const RENDER_CONCURRENCY = 16;

/** How many issues one page of the issue list asks for, which is that route's own maximum. */
const ISSUE_PAGE = 250;

/** How many case ids of each slice the command prints. */
const IDS_SHOWN = 10;

/**
 * Whether this draw holds the text it puts to the rule to `assertNoLeak`. One value for the
 * whole draw, read by the rendering that says what was done to its text and by the record that
 * writes the bound below, so a draw cannot carry a sentence saying the guard ran and a bound
 * asking for it. `NOT_HELD_TO_GUARD` in `render.ts` gives the reason this is `"none"`.
 */
const DRAW_GUARD: RenderGuard = "none";

/**
 * The bound this draw holds its own reading of the never-decide rule's answers under, which is
 * the bound of an unguarded draw. No branch can derive it, so it is written here and joined to
 * the sentence the rendering wrote of its text.
 */
const RENDERING_BOUND =
  "That holds while the question does: a reading that asks this rule what the person decided, " +
  "or that puts its answer into the agreement measurement, needs the text held to " +
  "`assertNoLeak` in full, both of its rules, first.";

/**
 * What `rule.renderingCheck` records: what this draw's renderings said was done to them, under
 * the bound above where no guard was run over them. The fact is the renderings' own, so a
 * change to what the draw does to the text it puts to the rule changes the record with it. A
 * draw that rendered nothing has no such fact and says that, rather than leaving the bound
 * standing over nothing, and a draw whose renderings were held to the guard has met that bound
 * already, so its sentences stand without it.
 */
export function renderingCheckOf(checks: readonly string[], guard: RenderGuard): string {
  if (checks.length === 0) return "No case was rendered in this draw, so no text was checked.";
  const said = checks.join(" ");
  return guard === "none" ? `${said} ${RENDERING_BOUND}` : said;
}

interface Flags {
  corpus: string;
  cut: string;
  out: string;
  seed: number | undefined;
  backlogRoots: string | undefined;
  cache: string | undefined;
  scorerRecords: string[];
  roundSessions: string | undefined;
}

/** One post-cut case as the draw was given it: the row, the rule's answer and its document. */
export interface DrawCase {
  row: CaseRow;
  /**
   * What the never-decide rule said over this case's rendered evidence, and null in a draw under
   * a registered read of the reserve, which applies no rule (`drawSlices`).
   */
  hold: NeverDecideAnswer | null;
  /** The first document of the approval request that could be read, and null where none was. */
  documentId: string | null;
  /** The version that document was asked about. */
  requestedVersion: number | null;
  /** The SHA-256 of that document as rendered, or of the rendered case where there is none. */
  sha256: string;
}

/** What a later round reads to prove it scored the same case over the same bytes. */
export interface AnchorDescriptor {
  caseId: string;
  askId: string;
  kind: CaseKind;
  documentId: string | null;
  requestedVersion: number | null;
  sha256: string;
}

/** Why a case the rule did not hold could still not be drawn. */
export type UndrawableKind = "too-few-options" | "no-option-chosen" | "rule-unreadable";

/** The undrawable kinds, in the order they are counted and printed. */
const UNDRAWABLE_KINDS: readonly UndrawableKind[] = [
  "too-few-options",
  "no-option-chosen",
  "rule-unreadable",
];

/** What each undrawable kind is called where a count of it is printed. */
const UNDRAWABLE_NAMES: Readonly<Record<UndrawableKind, string>> = {
  "too-few-options": "questions with too few options",
  "no-option-chosen": "questions answered with no option",
  "rule-unreadable": "unreadable never-decide answers",
};

/**
 * What the draw was given is not what it takes it for, so no draw over it would mean anything:
 * a corpus read wrongly, or an issue list answering a root that is no root.
 */
export class DrawError extends CannotRunError {
  override readonly name = "DrawError";
}

/** One case left out before the draw, and why. */
export interface ExcludedCase {
  caseId: string;
  kind: CaseKind;
  reason: string;
  /** The kind of hold, for a case the never-decide rule held. */
  heldKind?: NeverDecideKind;
  /** Which rule left it out, for a case no arm could be scored on. */
  undrawableKind?: UndrawableKind;
}

/** What one draw did: the slices, the counts they reached and what never reached them. */
export interface Draw {
  seed: number;
  targets: Readonly<Record<CaseKind, SliceTargets>>;
  slices: DrawnSlices;
  counts: Record<SliceName, Record<CaseKind, number>>;
  /** What the cases were grouped by, a whole group going into one slice. */
  groupKey: string;
  /** How many groups there were under that key. */
  groupCount: number;
  /** How many of those groups hold a case of each kind, a group being able to hold both. */
  groups: Record<CaseKind, number>;
  held: ExcludedCase[];
  heldByKind: Record<string, number>;
  undrawable: ExcludedCase[];
  undrawableByKind: Record<UndrawableKind, number>;
  anchors: AnchorDescriptor[];
  /** Where report, the final test, came from; null for a draw that read no reserve. */
  finalTest: FinalTestDraw | null;
}

/**
 * What a draw under a registered read of the reserve is given: the read's reference, which cases
 * the reserve holds, the groups item 6 puts the cases in, each with its outcome, and the post-cut
 * cases the command left out before rendering any case because their group had an ask before the
 * cut (item 6 a): their documents were read only into the shingles the near-duplicate joins
 * compare. Only an eligible group may go into report. Such a draw puts no case to the never-decide
 * rule (`drawSlices`).
 */
export interface FinalTestSource {
  reference: string;
  holds: (row: CaseRow) => boolean;
  grouping: TreeGrouping;
  beforeCut: readonly CaseRow[];
}

/** One group of cases, which goes into one slice whole. */
interface Group {
  key: string;
  /** How many cases of each kind the group holds, which is what a slice must have room for. */
  counts: Record<CaseKind, number>;
  cases: DrawCase[];
}

/** `items` in a random order under `seed`, by a Fisher-Yates walk of that one stream. */
function shuffled<T>(items: readonly T[], seed: number): T[] {
  const order = [...items];
  const random = randomStream(seed);
  for (let index = order.length - 1; index > 0; index -= 1) {
    const swap = Math.floor(random() * (index + 1));
    const here = order[index];
    const there = order[swap];
    if (here === undefined || there === undefined) continue;
    order[index] = there;
    order[swap] = here;
  }
  return order;
}

/**
 * The group a case belongs to. Under a read of the reserve it is the case's issue tree over every
 * parent link the log recorded, joined over near-duplicate evidence (`trees.ts`). Otherwise it is
 * its issue, whether it is an approval request or a question: a document belongs to one issue, so
 * this key holds together every case one document held together and the cases of two documents
 * of one issue besides. A case naming no issue is its own group, since nothing else is known to
 * share it.
 */
function groupKeyOf(row: CaseRow, finalTest: FinalTestSource | null): string {
  if (finalTest !== null) return finalTest.grouping.groupOf(row);
  return `${GROUP_KEY}:${row.issueKey ?? row.askId}`;
}

/** The slices and the counts each starts at. */
function emptyDraw(seed: number, groupKey: string): Draw {
  const counts = {} as Record<SliceName, Record<CaseKind, number>>;
  const slices = {} as DrawnSlices;
  for (const slice of SLICES) {
    counts[slice] = { approval: 0, question: 0 };
    slices[slice] = [];
  }
  return {
    seed,
    targets: TARGETS,
    slices,
    counts,
    groupKey,
    groupCount: 0,
    groups: { approval: 0, question: 0 },
    held: [],
    heldByKind: {},
    undrawable: [],
    undrawableByKind: { "too-few-options": 0, "no-option-chosen": 0, "rule-unreadable": 0 },
    anchors: [],
    finalTest: null,
  };
}

/** The slices any group that is not eligible tries, under a read of the reserve: never report. */
const DEVELOPMENT_ORDER: readonly SliceName[] = SLICES.filter((slice) => slice !== "report");

/**
 * The draw itself: the cases the rule holds and the questions no arm could be scored on are left
 * out, the rest are grouped, the groups are shuffled under `seed`, and each group goes into the
 * first slice its whole size still fits.
 *
 * Under `finalTest`, a draw over a registered read of the reserve, the groups are the issue trees
 * joined over near-duplicate evidence, and each carries its outcome under item 6, read over every
 * case of the corpus in it. A group whose outcome is `ask-before-cut` gives no case to any slice:
 * its cases are named in `finalTest.beforeCut`. No never-decide answer is asked for (the claim's
 * point 9): no case is held and none is left out as `rule-unreadable`, and no
 * case of an eligible tree is left out as a question no arm can be scored on, so every case of
 * an eligible tree is drawn. An `eligible` group goes to report whole, whatever report's targets:
 * the final test is every eligible tree, which item 8 lets only never-decide and the run's own
 * (c) and (d) narrow, never-decide being S10's own to decide in the claim, and the reserve is
 * read once, so a tree sent elsewhere for want of room would be lost to it. Every other group
 * skips report, and its questions no arm can be scored on are left out as outside a read. The
 * reserved cases that went to another slice are named in `finalTest.heldOutsideReport`, each
 * group's parent changes after its first ask opened in `finalTest.parentChanges`, each
 * near-duplicate join in `finalTest.nearDuplicates`, and every eligible tree, with each of its
 * cases, in `finalTest.trees`.
 *
 * It throws `DrawError` over an approval case whose answer names no option, which is the corpus
 * read wrongly rather than a case to leave out, and over a case whose answer of the rule is not
 * the draw's mode: one that carries none outside a registered read, which applies the rule to
 * every case, and one that carries one under a read, which asks the rule for none.
 */
export function drawSlices(
  cases: readonly DrawCase[],
  seed: number,
  finalTest: FinalTestSource | null = null
): Draw {
  const draw = emptyDraw(seed, finalTest === null ? GROUP_KEY : TREE_GROUP_KEY);
  const drawable: DrawCase[] = [];
  const outcomeOf = (key: string): TreeOutcome =>
    finalTest?.grouping.outcomes.get(key) ?? "not-held-at-opening";
  // Item 6 (a): the cases the command left out before rendering, and below any given case whose
  // tree had an ask before the cut, go to no slice.
  const beforeCut: string[] =
    finalTest === null ? [] : finalTest.beforeCut.map((row) => row.caseId);
  const leaveOut = (row: CaseRow, undrawableKind: UndrawableKind, reason: string): void => {
    draw.undrawable.push({ caseId: row.caseId, kind: row.kind, reason, undrawableKind });
    draw.undrawableByKind[undrawableKind] += 1;
  };
  for (const drawCase of cases) {
    const row = drawCase.row;
    if (row.kind === "approval" && row.selected.length === 0) {
      throw new DrawError(
        `the approval case ${row.caseId} names no option the person chose, which the corpus ` +
          "build cannot produce: the draw stops rather than passing over it"
      );
    }
    // The draw's mode, read once for the case: under a registered read the outcome of its tree,
    // and outside one the never-decide rule's answer, which only that draw asks for.
    const outcome = finalTest === null ? null : outcomeOf(groupKeyOf(row, finalTest));
    const hold = drawCase.hold;
    if ((outcome === null) !== (hold !== null)) {
      throw new DrawError(
        outcome === null
          ? `the case ${row.caseId} carries no never-decide answer, which a draw that reads no ` +
              "reserve applies to every case"
          : `the case ${row.caseId} carries a never-decide answer, which a draw under a ` +
              "registered read never asks for"
      );
    }
    if (outcome === "ask-before-cut") {
      // Item 6 (a): an ask of the tree opened before the cut, so a scorer may have read the tree
      // before any of its cases came; it gives no case to any slice of the claim's draw.
      beforeCut.push(row.caseId);
      continue;
    }
    // Every eligible tree goes to report whole (the claim's point 9): no rule's answer is read
    // under a read, and a question no arm can be scored on is left out only of the other groups.
    if (outcome === "eligible") {
      drawable.push(drawCase);
      continue;
    }
    if (hold?.held === true) {
      const heldKind = hold.kind;
      draw.held.push({
        caseId: row.caseId,
        kind: row.kind,
        reason: hold.reason,
        ...(heldKind === null ? {} : { heldKind }),
      });
      const name = heldKind ?? "unnamed";
      draw.heldByKind[name] = (draw.heldByKind[name] ?? 0) + 1;
      continue;
    }
    if (hold !== null && hold.fault !== null) {
      // Neither held nor decided: a rule about what must not be decided may not decide by
      // accident, so the case leaves the draw and is named with what came back.
      leaveOut(row, "rule-unreadable", `the never-decide answer could not be read: ${hold.fault}`);
      continue;
    }
    if (row.kind === "question" && row.options.length < LEAST_OPTIONS) {
      leaveOut(
        row,
        "too-few-options",
        `a question offering ${row.options.length} options cannot be judged as a choice`
      );
      continue;
    }
    if (row.kind === "question" && row.selected.length === 0) {
      leaveOut(
        row,
        "no-option-chosen",
        "a question the person answered with no option cannot be scored by set equality with " +
          "what they chose"
      );
      continue;
    }
    drawable.push(drawCase);
  }
  const groups = new Map<string, Group>();
  // Sorted by case id, then grouped by sorted key: the draw turns on the seed alone and not on
  // the order `corpus.jsonl` happens to hold its cases in.
  for (const drawCase of [...drawable].sort((one, other) =>
    one.row.caseId < other.row.caseId ? -1 : 1
  )) {
    const key = groupKeyOf(drawCase.row, finalTest);
    const group = groups.get(key);
    if (group === undefined) {
      groups.set(key, {
        key,
        counts: { approval: 0, question: 0, [drawCase.row.kind]: 1 },
        cases: [drawCase],
      });
    } else {
      group.counts[drawCase.row.kind] += 1;
      group.cases.push(drawCase);
    }
  }
  const ordered = [...groups.values()].sort((one, other) => (one.key < other.key ? -1 : 1));
  let finalTestGroups = 0;
  const heldOutsideReport: string[] = [];
  const inReport = new Set<string>();
  for (const group of shuffled(ordered, seed)) {
    draw.groupCount += 1;
    for (const kind of KINDS) if (group.counts[kind] > 0) draw.groups[kind] += 1;
    const eligible = finalTest !== null && outcomeOf(group.key) === "eligible";
    if (eligible) finalTestGroups += 1;
    // An eligible group goes to report whole and past its targets: the final test is every
    // eligible tree, and the reserve is read once.
    let taken: SliceName = eligible ? "report" : "reserve";
    for (const slice of eligible ? [] : finalTest === null ? SLICES : DEVELOPMENT_ORDER) {
      if (slice === "reserve") break;
      // A group holding both kinds goes where both still fit: one group, one slice, whole.
      const fits = KINDS.every(
        (kind) => group.counts[kind] <= TARGETS[kind][slice] - draw.counts[slice][kind]
      );
      if (fits) {
        taken = slice;
        break;
      }
    }
    for (const kind of KINDS) draw.counts[taken][kind] += group.counts[kind];
    if (eligible) inReport.add(group.key);
    if (finalTest !== null && taken !== "report") {
      for (const drawCase of group.cases) {
        if (finalTest.holds(drawCase.row)) heldOutsideReport.push(drawCase.row.caseId);
      }
    }
    for (const drawCase of group.cases) {
      draw.slices[taken].push(drawCase.row.caseId);
      if (taken !== "anchor") continue;
      draw.anchors.push({
        caseId: drawCase.row.caseId,
        askId: drawCase.row.askId,
        kind: drawCase.row.kind,
        documentId: drawCase.documentId,
        requestedVersion: drawCase.requestedVersion,
        sha256: drawCase.sha256,
      });
    }
  }
  if (finalTest !== null) {
    const outcomes = Object.fromEntries(TREE_OUTCOMES.map((outcome) => [outcome, 0])) as Record<
      TreeOutcome,
      number
    >;
    const parentChanges: FinalTestDraw["parentChanges"] = [];
    const rows = [...cases.map((drawCase) => drawCase.row), ...finalTest.beforeCut];
    const casesOfKey = new Map<string, string[]>();
    for (const row of rows) {
      const key = groupKeyOf(row, finalTest);
      const held = casesOfKey.get(key);
      if (held === undefined) casesOfKey.set(key, [row.caseId]);
      else held.push(row.caseId);
    }
    const keys = [...casesOfKey.keys()].sort();
    const trees: FinalTestTree[] = [];
    for (const key of keys) {
      const outcome = outcomeOf(key);
      outcomes[outcome] += 1;
      const tree = finalTest.grouping.groups.get(key);
      const changes = tree === undefined ? [] : changesAfterAskOf(tree);
      if (changes.length > 0) parentChanges.push({ group: key, outcome, changes });
      // Every case of the tree: a draw under a registered read draws each case of an eligible
      // tree, so each such tree is in report, and the claim's measure counts it there.
      if (outcome === "eligible") {
        trees.push({
          group: key,
          slice: inReport.has(key) ? "report" : null,
          cases: [...(casesOfKey.get(key) ?? [])].sort(),
        });
      }
    }
    draw.finalTest = {
      reference: finalTest.reference,
      groups: finalTestGroups,
      outcomes,
      beforeCut: { total: beforeCut.length, cases: beforeCut },
      heldOutsideReport: { total: heldOutsideReport.length, cases: heldOutsideReport },
      parentChanges,
      nearDuplicates: { threshold: NEAR_DUPLICATE_JACCARD, joins: finalTest.grouping.joins },
      trees,
    };
  }
  return draw;
}

/**
 * Every issue carrying the `legion` label that stands on the backlog or in icebox, read now.
 * That is the fact the never-decide rule's waiting-root kind needs and the rendered case does
 * not carry; it is read here, in the command, and handed to the rule as a value. Dispatch's own
 * list answers it in one page per status, which is why no case is read for on its own. The list
 * is sorted by key, so what `draw.json` writes of one reading is the same bytes however the
 * pages came back.
 *
 * An entry of a page that is no root stops the draw, as every reading of this shape does
 * (`rootReadingOf` in `draw-io.ts`). This loop used to pass over an entry whose key or status
 * it could not read, which is the drop that policy repairs: such a root is one the never-decide
 * rule is never told about, and the draw would record a list shorter than the one Dispatch
 * answered without saying so.
 */
async function backlogLegionRoots(config: DispatchConfig): Promise<BacklogRoots> {
  const issues: Array<{ key: string } & IssueFacts> = [];
  const seen = new Set<string>();
  for (const status of ["backlog", "icebox"]) {
    for (let offset = 0; ; offset += ISSUE_PAGE) {
      const path = `/api/v1/issues?label=legion&status=${status}&limit=${ISSUE_PAGE}&offset=${offset}`;
      const answer = recordOf(await readJson<unknown>(path, config));
      const page = listOf(answer?.issues);
      for (const [at, value] of page.entries()) {
        const read = rootReadingOf(value);
        if (read.root === null) {
          throw new DrawError(
            `the ${status} issue list root ${read.key ?? offset + at + 1} ${read.fault}`
          );
        }
        if (seen.has(read.root.key)) continue;
        seen.add(read.root.key);
        issues.push(read.root);
      }
      const total = answer?.total;
      if (page.length < ISSUE_PAGE || typeof total !== "number" || offset + ISSUE_PAGE >= total) {
        break;
      }
    }
  }
  issues.sort((one, other) => (one.key < other.key ? -1 : 1));
  return { readAt: new Date().toISOString(), source: "dispatch", issues };
}

/**
 * The same list, pinned: read from a file in the shape `draw.json` writes, which is either that
 * whole file or the `backlogLegionRoots` object inside it. The reading time the file carries is
 * kept, because it is when the list was true; the source becomes the file.
 *
 * The roots themselves are read by `rootReadingOf`, the one reader of that shape, which checks
 * every field and refuses an entry holding one that is no such field. What this adds is its own
 * words for that refusal, and it keeps them rather than throwing the reader's: what it reads is
 * not a corpus's own draw but a file a person named on the command line, so every way it can
 * fail is a usage error about `--backlog-roots` and says what that flag takes. Answering a
 * corpus error over a flag would be a refusal that has lost its words.
 */
export async function pinnedBacklogRoots(path: string): Promise<BacklogRoots> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(path, "utf8"));
  } catch {
    throw new UsageError(`--backlog-roots ${path} could not be read as JSON`);
  }
  const record = recordOf(parsed);
  const held = recordOf(record?.backlogLegionRoots) ?? record;
  const listed = listOf(held?.issues);
  if (held === null || listed.length === 0) {
    throw new UsageError(
      `--backlog-roots ${path} names no such roots: it takes the shape draw.json writes, ` +
        "which is an object with `issues` or a whole draw.json holding `backlogLegionRoots`"
    );
  }
  const issues: Array<{ key: string } & IssueFacts> = [];
  for (const [at, value] of listed.entries()) {
    const read = rootReadingOf(value);
    if (read.root === null) {
      throw new UsageError(
        `--backlog-roots ${path} root ${read.key ?? at + 1} ${read.fault} (it takes the shape ` +
          "draw.json writes, whose every root names a key and a status that are strings, and " +
          "labels that are a list of strings)"
      );
    }
    issues.push(read.root);
  }
  issues.sort((one, other) => (one.key < other.key ? -1 : 1));
  return { readAt: stringOf(held.readAt) ?? "unknown", source: path, issues };
}

/** What the never-decide rule is handed beside each case, and how it is asked. */
interface RuleInputs {
  issues: ReadonlyMap<string, IssueFacts>;
  options: NeverDecideOptions;
}

/**
 * One post-cut case rendered and put to the rule, which is what the draw is given. With no
 * `rule`, in a draw under a registered read, the case is rendered for its descriptor's digest
 * and put to no rule.
 */
async function drawCaseOf(
  row: CaseRow,
  rule: RuleInputs | null,
  config: DispatchConfig
): Promise<{ drawCase: DrawCase; unread: number; check: string }> {
  const replayed = row.kind === "approval" ? await replayCase(row, config) : null;
  const documents = replayed?.documents ?? [];
  // `DRAW_GUARD` is this draw's one guard choice, a decision and not an oversight:
  // `NOT_HELD_TO_GUARD` in `render.ts` holds the reason, rule by rule, and `check` is the
  // sentence the rendering wrote of what was done to its text.
  const { text, check } = renderChecked(row, documents, DRAW_GUARD);
  // The descriptor's digest is over the first document of the request that could be read, as
  // `renderDocument` renders it, which is that document's own body inside the case rendered
  // above and not its section: the heading above it, the head of the rendering and every
  // other document are outside the digest, so a case whose rendering changes only there digests
  // the same. A case with no readable document is digested whole, over the rendered case. It is
  // the first readable document and not `leadDocumentOf`, which is the first the request names
  // an artifact and a version for: a digest of a document that could not be read is no record
  // of what was drawn, so an unread document is passed over.
  const first = documents.find((document) => document.markdown !== null) ?? null;
  const digested =
    first === null || first.markdown === null
      ? text
      : renderDocument(first.markdown, row.answeredAt);
  const sha256 = createHash("sha256").update(digested, "utf8").digest("hex");
  const issue = row.issueKey === null ? null : (rule?.issues.get(row.issueKey) ?? null);
  return {
    drawCase: {
      row,
      hold:
        rule === null
          ? null
          : await neverDecide(
              {
                text,
                row,
                issue,
                documentId: first?.artifactId ?? null,
                documentVersion: first?.requestedVersion ?? null,
              },
              rule.options
            ),
      documentId: first?.artifactId ?? null,
      requestedVersion: first?.requestedVersion ?? null,
      sha256,
    },
    unread: documents.filter((document) => document.refusal !== null).length,
    check,
  };
}

/** The post-cut cases, rendered and put to the rule, `RENDER_CONCURRENCY` of them at a time. */
async function drawCasesOf(
  rows: readonly CaseRow[],
  rule: RuleInputs | null,
  config: DispatchConfig
): Promise<{ cases: DrawCase[]; unread: number; checks: string[] }> {
  const cases = new Array<DrawCase>(rows.length);
  // What the renderings said was done to them, each sentence once and in no worker's order, so
  // the record a draw writes does not turn on which worker got there first.
  const checks = new Set<string>();
  let unread = 0;
  // Over the indices, so each case is written back where it was read from and the order of
  // `cases` is the order of `rows`, whichever worker got there first.
  await pool([...rows.keys()], RENDER_CONCURRENCY, async (index) => {
    const row = rows[index] as CaseRow;
    const one = await drawCaseOf(row, rule, config);
    cases[index] = one.drawCase;
    checks.add(one.check);
    unread += one.unread;
  });
  return { cases, unread, checks: [...checks].sort() };
}

/**
 * The shingles of every case's evidence, which the claim joins near-duplicate cases by
 * (`treeGroupingOf`): `renderCase` over the documents `replayCase` reads, the text the rule and
 * every arm are shown, before and after the cut alike, since a near-duplicate of an earlier case
 * shares what it holds. The text goes into numbers at once and is never written or put to a
 * model. A document that cannot be read would leave a case's evidence short and could hide a
 * near-duplicate, so it stops the draw.
 */
async function evidenceShinglesOf(
  rows: readonly CaseRow[],
  config: DispatchConfig
): Promise<Map<string, number[]>> {
  const shingles = new Map<string, number[]>();
  await pool(rows, RENDER_CONCURRENCY, async (row) => {
    const documents = row.kind === "approval" ? (await replayCase(row, config)).documents : [];
    const unread = documents.find((document) => document.refusal !== null);
    if (unread !== undefined) {
      throw new DrawError(
        `a document of ${row.caseId} could not be read (${unread.refusal}), so the claim cannot ` +
          'join that case\'s near-duplicates (dispatch://INFRA-3189 v8, "Grouping")'
      );
    }
    shingles.set(row.caseId, shinglesOf(renderCase(row, documents)));
  });
  return shingles;
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    cut?: string;
    out?: string;
    seed?: string;
    "backlog-roots"?: string;
    cache?: string;
    "scorer-records"?: string[];
    "round-sessions"?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        cut: { type: "string" },
        out: { type: "string" },
        seed: { type: "string" },
        "backlog-roots": { type: "string" },
        cache: { type: "string" },
        "scorer-records": { type: "string", multiple: true },
        "round-sessions": { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const cut = values.cut?.trim();
  const out = values.out?.trim();
  if (!corpus || !cut || !out) throw new UsageError(USAGE);
  let seed: number | undefined;
  if (values.seed !== undefined) {
    seed = Number(values.seed.trim());
    if (!Number.isSafeInteger(seed) || seed < 0) {
      throw new UsageError(`--seed takes a whole number, zero or above\n\n${USAGE}`);
    }
  }
  return {
    corpus,
    cut,
    out,
    seed,
    backlogRoots: values["backlog-roots"]?.trim() || undefined,
    cache: values.cache?.trim() || undefined,
    scorerRecords: (values["scorer-records"] ?? []).map((path) => path.trim()).filter(Boolean),
    roundSessions: values["round-sessions"]?.trim() || undefined,
  };
}

function countsTable(draw: Draw): string {
  const lines = [`  ${"slice".padEnd(10)}${"approval".padStart(10)}${"question".padStart(10)}`];
  for (const slice of SLICES) {
    const counts = draw.counts[slice];
    lines.push(
      `  ${slice.padEnd(10)}${String(counts.approval).padStart(10)}` +
        `${String(counts.question).padStart(10)}`
    );
  }
  for (const kind of ["approval", "question"] as const) {
    const targets = TARGETS[kind];
    lines.push(
      `  target ${kind.padEnd(10)}fit ${targets.fit}, report ${targets.report}, ` +
        `anchor ${targets.anchor}, reserve the rest`
    );
  }
  return lines.join("\n");
}

/**
 * What `draw.json` records of the never-decide rule over the cases it read: the prose it applied,
 * what applying it cost, which model each answer says served it (admitted, held and unreadable
 * alike, and whether or not a cache was kept), and what was checked of the text it was handed.
 */
function ruleRecordOf(
  cases: readonly DrawCase[],
  checks: readonly string[],
  definition: string,
  cache: string | null
): DrawRule {
  const answered = cases.flatMap((one) =>
    one.hold === null ? [] : [{ caseId: one.row.caseId, hold: one.hold }]
  );
  const servedModels: Record<string, number> = {};
  for (const { hold } of answered) {
    const name = hold.source === "issue-facts" ? "the issue's facts" : (hold.servedModel ?? "none");
    servedModels[name] = (servedModels[name] ?? 0) + 1;
  }
  return {
    model: DEFAULT_NEVER_DECIDE_MODEL,
    definitionSha256: createHash("sha256").update(definition, "utf8").digest("hex"),
    cache,
    asked: answered.filter(({ hold }) => hold.source === "model").length,
    cached: answered.filter(({ hold }) => hold.source === "cache").length,
    inputTokens: answered.reduce((total, { hold }) => total + (hold.inputTokens ?? 0), 0),
    outputTokens: answered.reduce((total, { hold }) => total + (hold.outputTokens ?? 0), 0),
    servedModels,
    // One line per case the rule read, so each answer's served model is kept with the draw.
    answers: answered.map(({ caseId, hold }) => ({
      caseId,
      source: hold.source,
      servedModel: hold.servedModel,
    })),
    renderingCheck: renderingCheckOf(checks, DRAW_GUARD),
  };
}

/**
 * Why a draw that reads the reserve may not be made at `cut`, and null where it may: a cut later
 * than the reserve's own leaves out of the draw every case answered before it, and an eligible
 * tree can hold such a case, so its record and the claim's measure would miss requests of it.
 */
export function claimCutRefusal(cut: number): string | null {
  if (cut <= Date.parse(RESERVE_TREE_CUT)) return null;
  return (
    "a draw that reads the reserve takes a --cut at or before the reserve's cut, " +
    `${RESERVE_TREE_CUT}: a later cut leaves out of the final test's trees each request ` +
    "answered before it"
  );
}

/**
 * Why a draw under a registered read may not write into `directory`, and null where it may: a
 * directory holding nothing, or one holding this claim run's own draw of the same build, whose
 * files this draw writes again. A directory holding another draw's record, one of another claim
 * run, of another build's bytes, or of no claim run, is another run's and is refused, and so is
 * one holding files and no `draw.json`, which no run here can show are its own.
 */
export async function claimOutRefusal(
  directory: string,
  claimRun: BuildChain
): Promise<string | null> {
  const entries = await readdir(directory).catch((): string[] => []);
  if (entries.length === 0) return null;
  const path = drawPath(directory);
  const text = await readFile(path, "utf8").catch(() => null);
  if (text === null) {
    return `${directory} holds files and no draw.json, so no run here can show they are its own`;
  }
  const refusal = chainRefusal(drawFactsOf(text, path).claimRun, claimRun, BUILD_CHAIN_NAMES);
  if (refusal === null) return null;
  return (
    `${directory} holds another run's draw: ${refusal}. A draw that reads the reserve writes ` +
    "only into its own claim run's directory, over the same build"
  );
}

/**
 * The draw's mode, settled once from the corpus it read: the claim's draw under a registered read
 * of the reserve, which groups by tree and applies no never-decide rule, or a draw the rule is
 * applied to. Every later step of the command branches on this one value.
 */
type DrawMode =
  | { kind: "claim"; finalTest: FinalTestSource; claimRun: BuildChain; toRender: CaseRow[] }
  | {
      kind: "ruled";
      rule: RuleInputs & { definition: string };
      roots: BacklogRoots;
      toRender: CaseRow[];
    };

/**
 * The claim's mode: the trees, the scorers' and round's records and every case's evidence read
 * before any case is rendered, so a corpus or a file the claim cannot check stops the draw first,
 * and the cases of a group with an ask before the cut (item 6 a) left out before any of them is
 * rendered: their held cases stay held until the reserve ends. Their evidence is read only into
 * shingles, which are numbers held in memory and never written. The claim run the build was and
 * the sha256 of each of the build's three files it read are recorded, so the next stage of the
 * run takes only this draw of those bytes, and an `--out` of another draw is refused before
 * anything more is read.
 */
async function claimModeOf(
  corpus: Corpus,
  reference: string,
  post: readonly CaseRow[],
  flags: Flags,
  directory: string,
  config: DispatchConfig
): Promise<DrawMode> {
  const { trees, sha256: treesSha256 } = await readTrees(corpus.directory);
  if (trees.throughEventId !== corpus.snapshot.throughEventId) {
    throw new DrawError(
      `the corpus's trees were written through event ${trees.throughEventId} and its cases ` +
        `through ${corpus.snapshot.throughEventId}: they are not one build`
    );
  }
  const id = corpus.snapshot.reserve?.claimRun;
  if (id === undefined) {
    throw new DrawError(
      `${corpus.directory} records no claim run beside its read of the reserve, so no stage ` +
        "after this draw could show it reads this build: build it again in the claim run"
    );
  }
  const claimRun: BuildChain = {
    id,
    snapshotSha256: corpus.snapshotSha256,
    corpusSha256: corpus.corpusSha256,
    treesSha256,
  };
  const taken = await claimOutRefusal(directory, claimRun);
  if (taken !== null) throw new DrawError(taken);
  const holds = (row: CaseRow): boolean => reserveOf(row) === "held";
  const grouping = treeGroupingOf(
    corpus.rows,
    trees,
    {
      holds,
      readAskIds: await idsInFiles(flags.scorerRecords),
      roundSessions: await idsInFiles(
        flags.roundSessions === undefined ? [] : [flags.roundSessions]
      ),
    },
    await evidenceShinglesOf(corpus.rows, config)
  );
  const beforeCut = post.filter(
    (row) => grouping.outcomes.get(grouping.groupOf(row)) === "ask-before-cut"
  );
  const left = new Set(beforeCut.map((row) => row.caseId));
  console.log(
    "the rule          not applied: under a registered read, S10's fact check and judges " +
      "decide never-decide"
  );
  return {
    kind: "claim",
    finalTest: { reference, holds, grouping, beforeCut },
    claimRun,
    toRender: post.filter((row) => !left.has(row.caseId)),
  };
}

/**
 * The mode of a draw that reads no reserve: the labelled roots on the backlog or in icebox, read
 * from Dispatch or pinned from a file, and the rule's prose, put to every post-cut case.
 */
async function ruledModeOf(
  post: readonly CaseRow[],
  flags: Flags,
  config: DispatchConfig,
  ask: AskModel | undefined
): Promise<DrawMode> {
  const roots =
    flags.backlogRoots === undefined
      ? await backlogLegionRoots(config)
      : await pinnedBacklogRoots(flags.backlogRoots);
  console.log(`legion roots on the backlog or in icebox  ${roots.issues.length}`);
  console.log(
    roots.source === "dispatch"
      ? `                  from Dispatch at ${roots.readAt}`
      : `                  pinned from ${roots.source}, read at ${roots.readAt}`
  );
  const definition = await neverDecideDefinition();
  console.log(`the rule          ${DEFAULT_NEVER_DECIDE_MODEL}, one call per case`);
  console.log(
    `cache             ${flags.cache ?? "none, so every case is asked of the model again"}`
  );
  const options: NeverDecideOptions = {
    model: DEFAULT_NEVER_DECIDE_MODEL,
    cacheDirectory: flags.cache ?? null,
    definition,
    ...(ask === undefined ? {} : { ask }),
  };
  return {
    kind: "ruled",
    rule: { issues: rootFactsOf(roots.issues), options, definition },
    roots,
    toRender: [...post],
  };
}

/** What a caller may hand the command beside its flags: a test's registration, root and model. */
export interface DrawOptions {
  registration?: Registration;
  root?: string;
  /** How the never-decide rule asks its model, where a draw applies the rule. */
  ask?: AskModel;
}

async function draw(flags: Flags, options: DrawOptions): Promise<number> {
  const reading = Boolean(readReferenceOf(process.env[RESERVE_VARIABLE]));
  if (reading && flags.seed === undefined) {
    // The rule's item 6: the final test is drawn by a seed registered then, so a draw that asks to
    // read the reserve takes the one its registration's digest holds, and stops before the read.
    throw new UsageError(
      `a draw that reads the reserve takes --seed, the seed its registration names\n\n${USAGE}`
    );
  }
  if (reading && (flags.scorerRecords.length === 0 || flags.roundSessions === undefined)) {
    // Item 6 (c) and (d) are checked before the claim reads, so the draw that reads stops here.
    throw new UsageError(
      "a draw that reads the reserve takes --scorer-records and --round-sessions, which item 6 " +
        `(c) and (d) check its trees against\n\n${USAGE}`
    );
  }
  if (!reading && (flags.scorerRecords.length > 0 || flags.roundSessions !== undefined)) {
    throw new UsageError(
      `--scorer-records and --round-sessions are read only by a draw that reads the reserve\n\n${USAGE}`
    );
  }
  if (reading && (flags.backlogRoots !== undefined || flags.cache !== undefined)) {
    // The claim's point 9: its draw applies no never-decide rule, so takes nothing the rule reads.
    throw new UsageError(
      "a draw that reads the reserve applies no never-decide rule, so it takes no " +
        `--backlog-roots and no --cache\n\n${USAGE}`
    );
  }
  // Before the corpus is read: the reserve is read once, so a claim draw stops before any read.
  const refusal = reading ? claimCutRefusal(cutTime(flags.cut)) : null;
  if (refusal !== null) throw new UsageError(`${refusal}\n\n${USAGE}`);
  const corpus = await readCorpus(flags.corpus, {
    registration: options.registration,
    root: options.root,
  });
  const directory = await outDirectory(flags.out);
  const cut = cutTime(flags.cut);
  const config = await dispatchConfig();
  const post = corpus.rows.filter((row) => answeredTime(row) >= cut);
  const seed = flags.seed ?? newSeed();
  console.log(`corpus            ${corpus.directory}`);
  console.log(`cut               ${new Date(cut).toISOString()}`);
  console.log(`post-cut cases    ${post.length}`);
  console.log(`seed              ${seed} (--seed ${seed} draws these slices again)`);
  // Only a registered read opens the reserve (`readCorpus`), so a corpus read under one is the
  // claim's: it groups by tree joined over near-duplicate evidence, its report slice is the final
  // test item 6 allows, and it applies no never-decide rule (the claim's point 9), which reads
  // Dispatch as it stands at the run: S10's fact check and judges decide never-decide over the
  // final test, as they decide it live, so it reads no backlog roots and asks no model.
  const readUnder = corpus.reserve.readUnder;
  const mode =
    readUnder === null
      ? await ruledModeOf(post, flags, config, options.ask)
      : await claimModeOf(corpus, readUnder, post, flags, directory, config);
  const claim = mode.kind === "claim" ? mode : null;
  const ruled = mode.kind === "ruled" ? mode : null;
  const rendered = await drawCasesOf(mode.toRender, ruled?.rule ?? null, config);
  const drawn = drawSlices(rendered.cases, seed, claim?.finalTest ?? null);
  const ruleRecord =
    ruled === null
      ? null
      : ruleRecordOf(rendered.cases, rendered.checks, ruled.rule.definition, flags.cache ?? null);
  console.log("");
  console.log(countsTable(drawn));
  console.log("");
  console.log(
    `groups            ${drawn.groupCount} under the key ${drawn.groupKey}, of which ` +
      `${drawn.groups.approval} hold an approval case and ${drawn.groups.question} a question`
  );
  if (ruleRecord !== null) {
    console.log(`held by the never-decide rule  ${drawn.held.length}`);
    for (const [kind, count] of Object.entries(drawn.heldByKind).sort()) {
      console.log(`  ${kind.padEnd(22)}${count}`);
    }
  }
  console.log(`undrawable                     ${drawn.undrawable.length}`);
  for (const kind of UNDRAWABLE_KINDS) {
    console.log(`  ${UNDRAWABLE_NAMES[kind].padEnd(35)}${drawn.undrawableByKind[kind]}`);
  }
  if (ruleRecord !== null) {
    const settled = (ruleRecord.answers?.length ?? 0) - ruleRecord.asked - ruleRecord.cached;
    console.log(
      `the rule's calls  ${ruleRecord.asked} asked, ${ruleRecord.cached} from the cache, ` +
        `${settled} settled by the issue's facts`
    );
    for (const [name, count] of Object.entries(ruleRecord.servedModels ?? {}).sort()) {
      console.log(`  served by ${name.padEnd(30)}${count}`);
    }
    console.log(
      `the rule's tokens ${ruleRecord.inputTokens} read, ${ruleRecord.outputTokens} written`
    );
  }
  console.log(`documents that could not be read ${rendered.unread}`);
  console.log(
    drawn.finalTest === null
      ? "final test        none: the reserve was not read, so report holds cases a live scorer " +
          "may have read"
      : `final test        report from the reserve's item 6 alone, read under ` +
          `${drawn.finalTest.reference}: ${drawn.finalTest.groups} eligible groups drawn; ` +
          `${drawn.finalTest.heldOutsideReport.total} reserved cases went to another slice; ` +
          `${drawn.finalTest.beforeCut.total} cases on trees with an ask before the cut went ` +
          "to no slice"
  );
  if (drawn.finalTest !== null) {
    for (const outcome of TREE_OUTCOMES) {
      console.log(`  trees ${outcome.padEnd(24)}${drawn.finalTest.outcomes[outcome]}`);
    }
    console.log(
      `  trees with a parent change after an ask opened  ${drawn.finalTest.parentChanges.length}`
    );
    console.log(
      `  near-duplicate joins of two groups  ${drawn.finalTest.nearDuplicates.joins.length} ` +
        `(Jaccard of word 5-grams of the evidence at or above ` +
        `${drawn.finalTest.nearDuplicates.threshold})`
    );
    const trees = drawn.finalTest.trees;
    const inNoSlice = trees.filter((tree) => tree.slice === null).length;
    console.log(
      `  eligible trees in report  ${trees.length - inNoSlice}; in no slice, every case left ` +
        `out  ${inNoSlice}`
    );
  }
  console.log("");
  for (const slice of SLICES) {
    const ids = drawn.slices[slice].slice(0, IDS_SHOWN);
    console.log(`first ${ids.length} of ${slice}`);
    for (const id of ids) console.log(`  ${id}`);
  }
  await mkdir(directory, { recursive: true });
  const write = async (path: string, value: unknown): Promise<void> => {
    await writeFile(path, `${JSON.stringify(value, null, 2)}\n`, "utf8");
  };
  const anchors = join(directory, "anchor-descriptors.json");
  await write(slicesPath(directory), drawn.slices);
  await write(anchors, drawn.anchors);
  const record: DrawRecord = {
    corpus: corpus.directory,
    cut: new Date(cut).toISOString(),
    seed,
    targets: drawn.targets,
    counts: drawn.counts,
    groupKey: drawn.groupKey,
    groupCount: drawn.groupCount,
    groups: drawn.groups,
    postCutCases: post.length,
    documentsUnread: rendered.unread,
    // The one input of a ruled draw that is not the corpus: the labelled roots on the backlog or
    // in icebox, which the never-decide rule reads. The list is written whole, with the time it
    // was read, so `--backlog-roots <this file>` draws these slices again whatever Dispatch
    // shows later. The claim's draw reads none.
    backlogLegionRootsRead: ruled === null ? null : ruled.roots.issues.length,
    backlogLegionRoots: ruled?.roots ?? null,
    // What the rule was, for a ruled draw: the prose it applied, what applying it cost, and what
    // was checked of the text it was applied to. A change of one byte of that prose changes every
    // cache key, so a later draw over the same corpus and seed with another definition is a
    // different reading and says so here.
    rule: ruleRecord,
    neverDecideApplied: ruled !== null,
    claimRun: claim?.claimRun ?? null,
    excluded: {
      held: { total: drawn.held.length, byKind: drawn.heldByKind, cases: drawn.held },
      undrawable: {
        total: drawn.undrawable.length,
        byKind: drawn.undrawableByKind,
        cases: drawn.undrawable,
      },
    },
    finalTest: drawn.finalTest,
  };
  await write(drawPath(directory), record);
  console.log("");
  console.log(`written           ${slicesPath(directory)}`);
  console.log(`                  ${anchors}`);
  console.log(`                  ${drawPath(directory)}`);
  return 0;
}

/** The command: the flags, the draw, and the exit status it takes. */
export async function main(argv: readonly string[], options: DrawOptions = {}): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  try {
    return await draw(flags, options);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports `drawSlices` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
