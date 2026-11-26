import type { CheckBlock } from "../../../api/types";
import { isCheck, isStatement, type TreeNode } from "../issue-tree";
import type { CheckState, StatementState } from "../page-words";
import {
  checkStanding,
  namedCommit,
  newestFirst,
  standsAtBasis,
  stateAtCommit,
  statementState,
} from "../statement-rules";

/**
 * How each statement of the issue stands, and the one commit the page names for its results.
 *
 * The Verification filter draws each statement with the checks that serve it and their latest
 * results. R17 adds three readings over that, and all three are here, pure, so the body draws
 * them and reads nothing of its own:
 *
 * - **One commit for the issue's results.** The page names it once, where the results are read,
 *   rather than on every result (`VerificationFilterBody.tsx`). It is `statement-rules.ts`'s
 *   `namedCommit`: the commit of the newest result recorded against any check of the issue that
 *   still stands, against the words its check has now. Every statement is read at it (R6).
 * - **Met, Not met, Unknown.** A statement reads Met where every check of it passed at that
 *   commit, Not met where one failed there, and Unknown otherwise - no check at all, a result
 *   recorded at another commit than the one the page names, a result recorded against another
 *   commit than its check compares with, a withdrawn one, or a check Dispatch could not read. It
 *   is `statement-rules.ts`'s `statementState` over each check read by `stateAtCommit`, the rule
 *   Dashboard's two tiles read too, so a tile and this body count one statement alike.
 * - **Covered.** How many statements carry a check. It is covered and never proven or met: a
 *   statement that has a check is not a statement that is true (R17, R22).
 *
 * How one check stands on its own row is `statement-rules.ts`'s `checkStanding`, the page's one
 * reading of it: each of its results read against the commit the server names for that check, the
 * basis it compares with (`commit_stands`), as the server reads it, where the newest result over
 * the check's words now at that basis decides - a fail there is Failed whatever older pass the
 * server still counts, and a newer result at another commit says nothing about it. Two checks of
 * one issue compare with different commits, so no check's row is read against the one commit the
 * page names (decision 152): a check whose pass at its own basis is older than the commit the page
 * names reads Passed on its row, and Out of date in its statement's state.
 * The tree's check node holds the same reading (`addChecks`), so this body, the outline's row,
 * the fold's frontier and the filter row's failing count say one word for one check, and it is
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
   *  `statement-rules.ts`'s `standsAtBasis`, which `checkStanding` reads too, so this mark and
   *  the check's state never disagree. */
  readonly atBasis: boolean;
  /** How the check stands at the one commit the page names (`stateAtCommit`), which its
   *  statement's state reads (R6, `statementState`), or `undefined` where `state` is. The row
   *  draws `state`, the check's own (decision 152), so a pass at its own basis older than the
   *  commit the page names reads Passed on its row and Out of date here. */
  readonly atCommit?: CheckState;
  /** Every other result of the check, newest first, which the body folds into one line with
   *  their count under the latest (a check with 431 results drew a page 149,936 px tall). */
  readonly earlier: readonly TreeNode[];
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
  /** How many of the statements carry a check (R17's covered count). */
  readonly covered: number;
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
 * The reading the body draws: the one commit, each statement with its checks and their latest
 * results, how each statement stands, and how many carry a check.
 *
 * `view` is the tree the Verification filter left (`presets.ts`'s `presetView`): its top rows
 * are the statements the filter draws as context, each with the checks that serve it under it
 * and each check's results under that. `checks` is the issue's results read, which the page has
 * already made for its tree (`useIssueTree.ts`), and which is what holds a result's commit, its
 * verdict and its withdrawal; a check node is tied to its block by its own address, which the
 * tree carries (`tree-drafts.ts` writes `check.ref` into the node's `reference`).
 */
export function verificationReading({
  checks,
  view,
}: {
  readonly checks: readonly CheckBlock[];
  readonly view: TreeNode;
}): VerificationReading {
  const commit = namedCommit(checks);
  const read = checkReader(checks, commit);
  const statements = view.children.filter(isStatement).map((node): StatementReading => {
    const readings = node.children.filter(isCheck).map(read);
    return { checks: readings, node, state: statementState(readings) };
  });
  return {
    ...(commit === undefined ? {} : { commit }),
    covered: statements.filter((statement) => carriesCheck(statement.node, statement.checks))
      .length,
    loose: view.children.filter(isCheck).map(read),
    others: view.children.filter((node) => !isStatement(node) && !isCheck(node)),
    statements,
  };
}

/**
 * Whether a statement carries a check (R17's covered): a check of this issue drawn under it,
 * or a check the coverage read counts for it that this issue's tree does not hold - a
 * descendant issue's, which R10 counts and this issue's results read does not answer for
 * (`coverage.go`, `TreeNode.counts`).
 */
function carriesCheck(node: TreeNode, checks: readonly CheckReading[]): boolean {
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
 * Met in one view and something else in the other (R17), each check read at the one commit the
 * page names (`namedCommit`, R6). `root` is the whole tree, where each check sits under every
 * statement it serves.
 */
export function statementStandings(
  root: TreeNode,
  checks: readonly CheckBlock[]
): ReadonlyMap<string, StatementStanding> {
  const read = checkReader(checks, namedCommit(checks));
  const standings = new Map<string, StatementStanding>();
  const walk = (node: TreeNode): void => {
    if (isStatement(node) && !standings.has(node.id)) {
      const readings = node.children.filter(isCheck).map(read);
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

/** How many statements stand in each state, which the head draws as its counts and its bar. */
export function statementCounts(
  statements: readonly StatementReading[]
): Readonly<Record<StatementState, number>> {
  const counts: Record<StatementState, number> = { met: 0, not_met: 0, unknown: 0 };
  for (const statement of statements) counts[statement.state] += 1;
  return counts;
}

/** How one check node of the tree reads (`checkReading`) where the page names `commit`: the node
 *  is tied to its block by its own address, which the tree carries (`tree-drafts.ts` writes
 *  `check.ref` into the node's `reference`). */
function checkReader(
  checks: readonly CheckBlock[],
  commit: string | undefined
): (node: TreeNode) => CheckReading {
  const blocks = new Map(
    checks.filter((check) => check.ref !== "").map((check) => [check.ref, check])
  );
  return (node) => checkReading(node, blocks.get(node.reference ?? ""), commit);
}

/**
 * How one check stands, and the result the body draws under it.
 *
 * The state, and the result it stands on, are `checkStanding`'s (`statement-rules.ts`): the one
 * reading of a check the tree's node holds too, which reads each result at the commit the server
 * names for the check, and which the check's row draws. How it stands at `commit`, the one the
 * page names, is `stateAtCommit`'s over that reading, which its statement's state reads. What
 * this adds is the body's own: whether that result stands at the check's basis, the latest result
 * the body draws under the check, and every other result, which the body folds under it.
 */
function checkReading(
  node: TreeNode,
  block: CheckBlock | undefined,
  commit: string | undefined
): CheckReading {
  if (block === undefined) return { atBasis: false, earlier: [], node };
  const standing = checkStanding(block);
  const { state, stands } = standing;
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
    atCommit: stateAtCommit(standing, commit),
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
