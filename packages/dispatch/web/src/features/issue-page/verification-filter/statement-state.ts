import type { CheckBlock, IssueCoverage } from "../../../api/types";
import { isCheck, isStatement, type TreeNode } from "../issue-tree";
import { borrowedReads } from "../model-rows/model-drafts";
import { type CheckState, coverageNotes, type StatementState } from "../page-words";
import type { CheckStateReads } from "../tree-drafts";
import { checkStanding, newestFirst, standingResults, standsAtBasis } from "../tree-drafts";
import { outOfCounts } from "../tree-fold";
import { nodesInOrder } from "../tree-keys";
import { isForeign } from "../tree-read/tree-nodes";
import type { TreeAnswerNode } from "../tree-read/tree-read";

/**
 * How each statement of the issue stands, and the one commit the page names for its results.
 *
 * The Verification filter draws each statement with the checks that serve it and their latest
 * results. R17 adds three readings over that, and all three are here, pure, so the body draws
 * them and reads nothing of its own:
 *
 * - **One commit for the issue's results.** The page names it once, where the results are read,
 *   rather than on every result (`VerificationFilterBody.tsx`). It is the commit of the newest
 *   result recorded against any check of the issue that still stands, against the words its
 *   check has now: a withdrawn result is no evidence, as the server counts it
 *   (`results_comparison.go`'s `passStandsOf` skips it, and `tree-drafts.ts`'s `checkStanding`
 *   reads only the results a retraction left standing), so a withdrawal may not move the commit
 *   the page names; and a result recorded against words its check no longer has stands for
 *   nothing either (the server's `standingCommit`), so it names no commit. It is what the body
 *   names, and nothing more: it decides no check's state and no statement's.
 * - **Met, Not met, Unknown.** A statement reads Met where every check of it reads Passed, Not
 *   met where one reads Failed, and Unknown otherwise - no check at all, a result recorded
 *   against another commit than its check compares with, a withdrawn one, or a check Dispatch
 *   could not read. The checks' own states say which of those it is, in the words
 *   `NODE_STATE_WORDS.check` declares.
 * - **Covered.** How many of the issue's statements carry a check. It is covered and never proven
 *   or met: a statement that has a check is not a statement that is true (R17, R22). It is the
 *   coverage read's count (`issueCovered`), which answers every statement of the issue's spec,
 *   less the rows `countedCoverage` leaves out. It is not a count of the statements the view
 *   draws: the view read answers its nodes a page at a time, and the server ends a page at a
 *   bound of bytes as well as of nodes, so the statements on its first page move with every node
 *   written before the cut - a check an issue under this one adds under the first statement
 *   pushes the last one onto the next page. While a document of the issue is still being indexed
 *   (`coverageNotes`), the coverage read's counts may not hold what serves a statement yet, so
 *   the summary draws that document's line in place of any count.
 *
 * How one check stands is `tree-drafts.ts`'s `checkStanding`, the page's one reading of it:
 * each of its results read against the commit the server names for that check, the basis it
 * compares with (`commit_stands`), as the server reads it, where the newest result over the
 * check's words now at that basis decides - a fail there is Failed whatever older pass the
 * server still counts, and a newer result at another commit says nothing about it. Two checks
 * of one issue compare with different commits, so no check is read against the one commit the
 * page names.
 * The tree's check node holds it too (`tree-read/tree-nodes.ts`), so this body, the outline's row
 * and the filter row's failing count say one word for one check, and it is
 * the word the check's result says of itself (`check-facts.ts`'s `resultFacts`).
 *
 * A statement is a requirement of the issue's spec (`isStatement`) and a check a check of its
 * Verification document (`isCheck`): a `requirement` or `check` block of any other document is a
 * block and nothing more, which this reading neither draws as a statement nor counts as a check.
 *
 * The model holds no React and makes no request: the body draws it, and the unit rows read the
 * same functions.
 */

/** The three, in the order the head counts them. */
export const STATEMENT_STATES: readonly StatementState[] = ["met", "not_met", "unknown"];

/** One check of a statement, as the body draws it. */
export interface CheckReading {
  /** Whether the result its state stands on stands for something at the commit the server names
   *  for the check: at the basis the check compares with (`commit_stands` is `at_basis`) and
   *  recorded against the check's words now, as the server's `standingCommit` reads it. It is
   *  `tree-drafts.ts`'s `standsAtBasis`, which `checkStanding` reads too, so this mark and the
   *  check's state never disagree. */
  readonly atBasis: boolean;
  /** Every other result of the check, newest first, which the body folds into one line with
   *  their count under the latest (a check with 431 results drew a page 149,936 px tall). */
  readonly earlier: readonly TreeNode[];
  /** The key of the issue under this one that the check is of, where it is that issue's check
   *  the Verification view's read answers (`checkReader`), which the body draws as a link to that
   *  issue; absent on every check of this issue. */
  readonly issue?: string;
  /** The result the body draws under the check: the newest that still stands, or, where every
   *  result of it was withdrawn, the newest withdrawn one, since that is what the reader is
   *  owed. Absent where the check has no result at all, and where the results read has not
   *  answered for the check. */
  readonly latest?: TreeNode;
  /** The id of the result the check's state stands on (`CheckStanding.stands`), which the body
   *  marks wherever it draws it; absent where none stands. */
  readonly standsOn?: string;
  /** The check's own node, with its words and the four attributes its body draws. */
  readonly node: TreeNode;
  // A check of another issue has no state where the Verification view's read answers no words'
  // sum on it or no `result` on one of its results.
  /** How the check stands, in the seven states the page has a word for, or `undefined` while
   *  the results read has not answered for it: a check the page has not read how it stands
   *  draws no state, because none of the seven is true of it. */
  readonly state?: CheckState;
}

/** One statement of the issue, with the checks that serve it. */
export interface StatementReading {
  readonly checks: readonly CheckReading[];
  readonly node: TreeNode;
  readonly state: StatementState;
}

/** What the Verification filter's body draws. */
export interface VerificationReading {
  /** The one commit the page names for the issue's results, or `undefined` where no result
   *  that stands names one. */
  readonly commit?: string;
  /** The checks the filter draws that serve no statement of the issue: a `serves` naming
   *  nothing places the check under the issue (`tree-drafts.ts`), and the filter draws it as a
   *  row of its own, so the body draws it too rather than dropping a node the filter kept. */
  readonly loose: readonly CheckReading[];
  /** Every other row the filter drew: a node an address named, which outranks the filter
   *  (`presets.ts`), and which the body draws by its words so nothing the filter kept is lost
   *  (R19, R20). */
  readonly others: readonly TreeNode[];
  readonly statements: readonly StatementReading[];
}

/** How many characters of a commit the page draws, which is the short form the approved
 *  prototype draws ("a2ea3fd269"). */
export const COMMIT_CHARACTERS = 10;

/** A commit as the page draws it: its first ten characters, and the whole of a shorter one. */
export function shortCommit(sha: string): string {
  return sha.slice(0, COMMIT_CHARACTERS);
}

/**
 * The one commit the page names for the issue's results (R17): the commit of the newest result
 * recorded against any check of the issue that still stands, against the words its check has
 * now, and names one. A result nobody withdrew is the evidence the page reads, so a withdrawn
 * result names no commit here; nor does one recorded against words its check no longer has
 * (`check_words_moved`), which the server's `standingCommit` (`results_comparison.go`) gives no
 * standing; and a result recorded with no commit at all names none either.
 *
 * It is the commit the Verification filter's body names, and nothing reads a check's or a
 * statement's state at it: each check is read at the commit the server names for that check
 * (`tree-drafts.ts`'s `checkStanding`).
 */
export function namedCommit(checks: readonly CheckBlock[]): string | undefined {
  const named = checks.flatMap((check) =>
    standingResults(check).filter(
      (result) => result.commit_sha !== "" && result.check_words_moved !== true
    )
  );
  return newestFirst(named)[0]?.commit_sha;
}

/**
 * The reading the body draws: the one commit, each statement with its checks and their latest
 * results, and how each statement stands.
 *
 * `view` is the tree the Verification filter left (`presets.ts`'s `presetView`): its statements
 * are the requirements the filter draws as context, top-level or nested under another statement,
 * in document order, each with the checks that serve it under it and each check's results under
 * that. `checks` is the issue's results read, which the page has already made for its tree
 * (`useIssueTree.ts`), and which is what holds a result's commit, its verdict and its
 * withdrawal; a check node is tied to its block by its own address, which the tree carries
 * (`tree-drafts.ts` writes `check.ref` into the node's `reference`).
 * A check of an issue under this one is read from the Verification view's read the view's issue
 * node holds (`checkReader`), and names no commit.
 */
export function verificationReading({
  checks,
  view,
}: {
  readonly checks: readonly CheckBlock[];
  readonly view: TreeNode;
}): VerificationReading {
  const read = checkReader(checks, view);
  const commit = namedCommit(checks);
  const statements: StatementReading[] = [];
  const seen = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (isStatement(node) && !seen.has(node.id)) {
      seen.add(node.id);
      const readings = node.children.filter(isCheck).map(read);
      statements.push({ checks: readings, node, state: statementState(readings) });
    }
    for (const child of node.children) walk(child);
  };
  for (const child of view.children) walk(child);
  return {
    ...(commit === undefined ? {} : { commit }),
    loose: view.children.filter(isCheck).map(read),
    others: view.children.filter((node) => !isStatement(node) && !isCheck(node)),
    statements,
  };
}

/** How the issue's covered count stands (R17): how many of its statements carry a check out of
 *  how many it has, or, while a document of the issue is still being indexed, the coverage read's
 *  own lines saying so (`coverageNotes`) in place of any count. */
export type IssueCovered =
  | { readonly covered: number; readonly kind: "count"; readonly total: number }
  | { readonly kind: "owed"; readonly notes: readonly string[] };

/**
 * The issue's covered count, from the coverage read it is given
 * (`GET /api/v1/issues/{key}/coverage`): every `requirement` block of the issue's spec is one of
 * its rows (`requirements`), and one is covered where a check of the issue or of an issue under
 * it serves it (`checks`). The summary gives it that read less the rows `countedCoverage` leaves
 * out, so the count can move as the view loads more statements. While a document of the issue is
 * still being indexed (`coverageNotes`), the coverage read's counts may not hold what serves a
 * statement yet, so this answers those lines in place of any count.
 */
export function issueCovered(coverage: IssueCoverage | undefined): IssueCovered {
  const notes = coverageNotes(coverage);
  if (notes.length > 0) return { kind: "owed", notes };
  const requirements = coverage?.requirements ?? [];
  return {
    covered: requirements.filter((requirement) => requirement.checks > 0).length,
    kind: "count",
    total: requirements.length,
  };
}

/**
 * The coverage read less each row no count reads (R3): a row the read marks `proposed` or
 * `closed`, and a row whose statement `view` holds, at any depth, out of the counts
 * (`outOfCounts`), paired by the address the tree gave that statement, which is the row's own
 * `ref` (`tree-read/tree-nodes.ts`' `keptReference`).
 *
 * It bridges the coverage read's walk (`coverage_tuples.go`), which reads a question's block node
 * and not its ask node, and so leaves a statement under an overtaken question unclosed, until
 * INFRA-1253's fix of that walk (INFRA-1196's decision 528). The `outOfCounts` half covers only
 * the statements the view has loaded (`TreeNode.moreToRead`); the `coverage` half covers the
 * rest, as the read marks them.
 */
export function countedCoverage(
  coverage: IssueCoverage | undefined,
  view: TreeNode
): IssueCoverage | undefined {
  if (coverage === undefined) return undefined;
  const outside = new Set(
    nodesInOrder(view).flatMap((node) =>
      node.type === "requirement" && node.reference !== undefined && outOfCounts(node)
        ? [node.reference]
        : []
    )
  );
  return {
    ...coverage,
    requirements: (coverage.requirements ?? []).filter(
      (row) => row.coverage !== "proposed" && row.coverage !== "closed" && !outside.has(row.ref)
    ),
  };
}

/**
 * Whether a statement carries a check (R17's covered): a check of this issue drawn under it,
 * or a check the coverage read counts for it that this issue's tree does not hold - a
 * descendant issue's, which R10 counts and this issue's results read does not answer for
 * (`coverage.go`, `TreeNode.counts`).
 */
function carriesCheck(node: TreeNode, checks: readonly CheckReading[]): boolean {
  // A proposed statement carries no check, and a proposed check, or one at or under an
  // overtaken node, covers no statement. The coverage read's count below leaves out a proposed
  // check as well, since it serves nothing yet (`queueServingBlockCounts` in `coverage.go`).
  if (outOfCounts(node)) return false;
  checks = inCounts(checks);
  return checks.length > 0 || (node.counts?.checks ?? 0) > 0;
}

/** How one statement stands, for a view that draws no check under it. */
export interface StatementStanding {
  /** Whether it carries a check (R17's covered). */
  readonly covered: boolean;
  readonly state: StatementState;
}

/**
 * How every statement of the whole tree stands, by its node id: the reading the Verification
 * filter draws beside each statement, for the Spec filter's mark, so one statement never reads
 * Met in one view and something else in the other (R17). `root` is the whole tree, where each
 * check sits under every statement it serves.
 */
export function statementStandings(
  root: TreeNode,
  checks: readonly CheckBlock[]
): ReadonlyMap<string, StatementStanding> {
  const read = checkReader(checks, root);
  const standings = new Map<string, StatementStanding>();
  const walk = (node: TreeNode): void => {
    if (isStatement(node) && !standings.has(node.id)) {
      const readings = checksUnder(node, read);
      standings.set(node.id, {
        covered: carriesCheck(node, readings),
        state: statementState(readings),
      });
    }
    for (const child of node.children) walk(child);
  };
  walk(root);
  return standings;
}

/** The readings of the checks drawn under `node` (`checkReader`): each check (`isCheck`), and
 *  each check of an issue under this one that the Verification view's read answers
 *  (`CheckReading.issue`), which the outline's tree draws with no state where its read names no
 *  result (`model-rows/model-drafts.ts`' `borrowedStanding`), so the statement reads that check
 *  as the Verification view reads it. */
function checksUnder(node: TreeNode, read: (node: TreeNode) => CheckReading): CheckReading[] {
  return node.children.flatMap((child) => {
    if (child.type !== "check") return [];
    const reading = read(child);
    return isCheck(child) || reading.issue !== undefined ? [reading] : [];
  });
}

/**
 * How a statement stands, from its checks' own states (R17): Met where every check of it reads
 * Passed, Not met where one reads Failed, and Unknown otherwise, which is where it carries no
 * check, where a check's evidence was recorded against another commit than the one the server
 * names for it, and where a check stands in any of the other five states.
 */
export function statementState(checks: readonly CheckReading[]): StatementState {
  // A proposed check, and one at or under an overtaken node, serves no statement: it is neither
  // evidence for it nor against it.
  checks = inCounts(checks);
  if (checks.length === 0) return "unknown";
  if (checks.every((check) => check.state === "passed")) return "met";
  if (checks.some((check) => check.state === "failed")) return "not_met";
  return "unknown";
}

/** How many statements stand in each state, which the head draws as its counts and its bar. */
export function statementCounts(
  statements: readonly StatementReading[]
): Readonly<Record<StatementState, number>> {
  // A proposed statement, and one at or under an overtaken node, is in none of the three.
  statements = inCounts(statements);
  const counts: Record<StatementState, number> = { met: 0, not_met: 0, unknown: 0 };
  for (const statement of statements) counts[statement.state] += 1;
  return counts;
}

/** What a count reads of `readings`: each one but those whose node is a proposed statement or
 *  check or sits at or under an overtaken node (`outOfCounts`). */
export function inCounts<Reading extends { readonly node: TreeNode }>(
  readings: readonly Reading[]
): readonly Reading[] {
  return readings.filter((reading) => !outOfCounts(reading.node));
}

/** How one check node of the tree reads (`checkReading`): the node is tied to its block by its
 *  own address, which the tree carries (`tree-drafts.ts` writes `check.ref` into the node's
 *  `reference`). */
function checkReader(
  checks: readonly CheckBlock[],
  issue: TreeNode
): (node: TreeNode) => CheckReading {
  const blocks = new Map(
    checks.filter((check) => check.ref !== "").map((check) => [check.ref, check])
  );
  // A check of an issue under this one, which the results read does not answer for, is read from
  // what the Verification view's read held on `issue`, the issue's own node (`TreeNode.views`),
  // answers of the node of the check's full read id (`TreeNode.readId`), as its row's standing is
  // read (`model-rows/model-drafts.ts`' `borrowedReads`), and never from a block of this issue,
  // whose address its node can carry where the two share a block id.
  const answer = issue.views?.verification;
  const borrowed = new Map<string, TreeAnswerNode & { readonly issue: string }>();
  for (const node of answer?.nodes ?? []) {
    if (node.supertag === "check" && isForeign(node, issue.id) && !borrowed.has(node.id)) {
      borrowed.set(node.id, node);
    }
  }
  return (node) => {
    const check = node.readId === undefined ? undefined : borrowed.get(node.readId);
    if (answer === undefined || check === undefined) {
      return checkReading(node, blocks.get(node.reference ?? ""));
    }
    return { ...checkReading(node, borrowedReads(check, answer)), issue: check.issue };
  };
}

/**
 * How one check stands, and the result the body draws under it.
 * It reads them from what the results read answers of the check (`block`), or for a check of
 * another issue from what the Verification view's read does.
 *
 * The state, and the result it stands on, are `checkStanding`'s (`tree-drafts.ts`): the one
 * reading of a check the tree's node holds too, which reads each result at the commit the server
 * names for the check. What this adds is the body's own: whether that result stands at the
 * check's basis, the latest result the body draws under the check, and every other result,
 * which the body folds under it.
 */
function checkReading(node: TreeNode, block: CheckStateReads | undefined): CheckReading {
  if (block === undefined) return { atBasis: false, earlier: [], node };
  const { state, stands } = checkStanding(block);
  const results = newestFirst(block.results ?? []);
  // What the body draws under the check: the newest result that stands, and where a retraction
  // left none standing the newest withdrawn one, which is what a retracted check has to show.
  const latest = results.find((result) => result.retracted === undefined) ?? results[0];
  const resultNodes = new Map(
    node.children.filter((child) => child.type === "result").map((child) => [child.id, child])
  );
  const drawnResult = latest === undefined ? undefined : resultNodes.get(latest.id);
  return {
    atBasis: stands !== undefined && standsAtBasis(stands),
    earlier: results.flatMap((result) => {
      const held = resultNodes.get(result.id);
      return held === undefined || result.id === latest?.id ? [] : [held];
    }),
    ...(drawnResult === undefined ? {} : { latest: drawnResult }),
    node,
    ...(stands === undefined ? {} : { standsOn: stands.id }),
    state,
  };
}
