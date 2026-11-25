import { primarySpec } from "../../api/issue-cache";
import type { CheckBlock, CheckResult, DisplayTree, IssueCoverage } from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { parseNodeReference } from "../refs/routes";
import type { IssueTreeReads, TreeNode } from "./issue-tree";
import type { CheckState, StatementState } from "./page-words";

/**
 * The pure rules the tree and Dashboard both read: how the issue addresses a node (`nodeIdOf`,
 * `blockNodeId`), which references name a statement of the review list (`coverageStatements`),
 * how a check stands (`checkStanding`), how a statement stands over its checks
 * (`statementState`), and how a title is read as one text run (`titleReading`).
 *
 * They hold no React, make no request, and import no code of the tree's model - `tree-drafts.ts`,
 * `issue-tree.ts`, `useIssueTree.ts` - only its types, so Dashboard reads the one rule the tree
 * reads without carrying the tree in its chunk (R14).
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/**
 * How `issue` addresses a node: the node a `dispatch://` reference names, as the tree's node ids
 * write it - the spec, a document, a block of either, an ask, a comment, a message, the issue
 * itself - and undefined for a reference to another issue or to anything else. It reads only the
 * issue's key and its spec's slug (`primarySpec`), so a caller that needs a node id and no chip
 * calls it without building a whole `treeContext` (`tree-drafts.ts`), and `treeContext`
 * addresses nodes by it too, so the two never differ.
 */
export function nodeIdOf(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">
): (reference: string) => string | undefined {
  const key = issue.key;
  const specSlug = primarySpec(issue)?.slug ?? "";
  return (reference: string): string | undefined => {
    const route = parseNodeReference(reference);
    if (route === undefined || !("key" in route) || route.key !== key) return undefined;
    const block = "block" in route ? route.block : undefined;
    if (route.kind === "spec") {
      return block === undefined ? specSlug : blockNodeId(specSlug, block.id);
    }
    if (route.kind === "artifact") {
      return block === undefined ? route.slug : blockNodeId(route.slug, block.id);
    }
    if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
      return route.id;
    }
    return route.kind === "issue" ? key : undefined;
  };
}

/**
 * Whether a reference names a statement of `issue` as the review list reads one (R35): a
 * requirement the coverage read `coverage` holds, each compared as the tree addresses it - the
 * requirement by the node `tree-drafts.ts`'s `addRequirements` draws for it, the reference by
 * `nodeIdOf` - so the spec's block written short (`spec#b-…`) and by the spec's own slug name the
 * same requirement, and another issue's address names none. Nothing is one while that read has
 * not answered, and no other block is one: not a `requirement` block of the spec that read does
 * not hold, which its row draws as a statement all the same (`Draft.statement`), nor a block of
 * any other document. It is the one rule both readers of the list read, the tree's `waiting`
 * (`tree-drafts.ts`'s `addDecisions`) and the header's count, whose caller hands it to
 * `waitingOnReader`, so the count counts the very decisions the tree marks.
 */
export function coverageStatements(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">,
  coverage: IssueCoverage | undefined
): (reference: string) => boolean {
  const specSlug = primarySpec(issue)?.slug ?? "";
  const statements = new Set(
    (coverage?.requirements ?? []).map((requirement) => blockNodeId(specSlug, requirement.block_id))
  );
  const nodeIdOfReference = nodeIdOf(issue);
  return (reference: string): boolean => {
    const id = nodeIdOfReference(reference);
    return id !== undefined && statements.has(id);
  };
}

/**
 * How a check stands where no result of it stands at its basis, in the states
 * `NODE_STATE_WORDS.check` names: `checkStanding`'s answer for such a check, and for a check with
 * no result or no `pass_stands` at all, and read by nothing else, so no place of the page reads
 * a check other than `checkStanding` does. Whether it passed is the server's own answer over
 * every result of the check (`pass_stands`, main's e98fc56ef8): a pass recorded before a later
 * fail still stands, while a retracted pass, a pass recorded against an older wording of the
 * check and a pass at a commit the check no longer compares with stand for nothing. The other
 * states read what no pass standing is:
 *
 * - **`no-result`,** no result recorded at all, which the results say by themselves.
 * - **`unreadable`,** a payload carrying no `pass_stands` at all, which is a server that does
 *   not send the field - an older build behind the same load balancer as the one this page
 *   came from. It is read before everything the answer decides, because without it the page
 *   knows nothing of how the check stands; it is the page's own blindness and no verdict of
 *   the check's.
 * - **`retracted`,** every result of the check withdrawn (`CheckResultRetracted`, which is on
 *   the result and never on the check): nothing stands for it and nothing stands against it,
 *   so neither "Passed" nor "Failed" is true of it, and the withdrawal is what the reader is
 *   owed.
 * - **`older`,** a newest result that stands and no longer speaks for the work as it stands,
 *   whatever it found, because its commit is not one the check compared with or the check's
 *   own words have moved under it.
 * - **`failed`,** a newest result that stands and failed at a commit Dispatch could not place:
 *   a fail at the work the check compares with is `checkStanding`'s own answer, before this.
 * - **`unmarked`,** where nothing could be judged: a repository Dispatch cannot read, or a
 *   commit it cannot place.
 *
 * A retracted result is no evidence, as the server counts it (`results_comparison.go`'s
 * `passStandsOf` skips it), so `older` and `failed` are read from the newest result that still
 * stands and never from a withdrawn one: a pass taken back at the compared commit over an older
 * pass at another commit is `older`, and a pass taken back over a fail that stands is `failed`,
 * whatever commit the withdrawn pass ran at. The newest is read by the time each result was
 * recorded (`newestFirst`) and never by the order the read sent them in, as `checkStanding` and
 * the Verification filter's body read it, so the three name one result whatever that order is.
 *
 * It is how a check stands against the basis the check itself carries, which is the server's
 * own reading (`pass_stands`, `commit_stands`), where no result over the check's words now is at
 * that basis, so one check reads one state wherever it is drawn, each told apart by a word and a
 * shape (R15, R21).
 */
function checkState(check: CheckStateReads): CheckState {
  if ((check.results ?? []).length === 0) return "no-result";
  if (check.pass_stands === undefined) return "unreadable";
  if (check.pass_stands.answer === "yes") return "passed";
  if (check.pass_stands.answer === "unknown") return "unmarked";
  const standing = newestFirst(standingResults(check))[0];
  if (standing === undefined) return "retracted";
  if (standing.check_words_moved === true) return "older";
  return standing.commit_stands?.answer === "other_commit" ? "older" : "failed";
}

/** What a check's state is read from: the server's answer over its results and the results
 *  themselves. `pass_stands` is declared required by the API types and is missing from what
 *  the deployed server sends, so it is optional here and the page reads a payload without it
 *  rather than throwing over the whole issue page. */
export type CheckStateReads = Pick<CheckBlock, "results"> & {
  readonly pass_stands?: CheckBlock["pass_stands"];
};

/** How one check stands, as the server reads the commit each of its results ran at. */
export interface CheckStanding {
  /** How it stands, in the states `NODE_STATE_WORDS.check` names. */
  readonly state: CheckState;
  /** The result that state stands on: the newest result over the check's words now at its
   *  basis, a pass or a fail, where one is there; else the newest result that stands at all,
   *  and none for a check the server says passed with no pass at its basis in the read. Absent
   *  where none stands. */
  readonly stands?: CheckResult;
}

/**
 * How a check stands: the page's one reading of a check's state (R10, R17), and the only one -
 * `checkState` is its own fallback and nothing else reads it. The tree's check node holds it
 * (`tree-drafts.ts`'s `addChecks`), so the outline's row, the fold's frontier (`tree-fold.ts`)
 * and the Verification filter's failing count (`presets.ts`'s `filterSignals`) read it, and so
 * does the Verification filter's body (`statement-state.ts`'s `checkReading`): one check never
 * reads Passed on its row and Failed in the filter.
 *
 * Each result is read against the commit the server names for its own check - the basis the
 * check compares with, which the result's `commit_stands` answers (`at_basis`, `other_commit`,
 * `unknown`) - as the server's `standingCommit` and `passStandsOf` read it
 * (`results_comparison.go`), and never against one commit the page names for every check: two
 * checks of one issue compare with different commits, a `before_merge` check with the pull
 * request's head and an `after_merge` one with the branch it merged into, so a pass at its own
 * check's basis reads Passed whatever newer result another check holds at another commit, as its
 * own fact reads "Up to date" (`check-facts.ts`'s `resultFacts`). A result stands for something
 * at the basis only where nobody withdrew it and its check's words have not moved since it was
 * recorded.
 *
 * **The latest evidence at the basis decides.** A check with no result reads No result, and one
 * whose payload carries no `pass_stands` reads Could not read, as `checkState` reads them. Else,
 * where a result stands for something at the basis, the newest of them is the check's state: a
 * fail there is Failed, whatever older pass the server still counts as standing (`pass_stands`
 * says yes for any pass at a basis, later fails or not), and a pass there is Passed. A newer
 * result at another commit, or at one Dispatch could not place, says nothing about the basis,
 * so it neither hides a fail there nor turns a pass there over; and a result recorded against
 * words the check no longer has stands for nothing, since it is evidence about a check that no
 * longer reads that way. The results read sends no verdict but `pass` and `fail`
 * (`api/results.go` refuses any other). Where nothing stands at the basis, it is `checkState`'s
 * answer: Out of date for a newest result at another commit or over moved words, Retracted,
 * Cannot tell, or Failed for a newest fail at a commit Dispatch could not place.
 */
export function checkStanding(check: CheckStateReads): CheckStanding {
  const standing = newestFirst(standingResults(check));
  // The newest result that stands for something at the commit the server names for the check:
  // over the check's words now and at its basis (`standingCommit`). A check with no result has
  // none, and one with no `pass_stands` is read before it.
  const here = check.pass_stands === undefined ? undefined : standing.find(standsAtBasis);
  if (here !== undefined) {
    return { stands: here, state: here.verdict === "pass" ? "passed" : "failed" };
  }
  const state = checkState(check);
  // No pass is at the basis in the read, so a check the server says passed stands on none of
  // its results here.
  const stands = state === "passed" ? undefined : standing[0];
  return { state, ...(stands === undefined ? {} : { stands }) };
}

/** Whether `result` stands at its check's basis: recorded against the check's words now
 *  (`check_words_moved`) and at the commit the server names for the check (`commit_stands` is
 *  `at_basis`), as the server's `standingCommit` reads it. It is the one reading of "this result
 *  stands at its check's basis" that a check's state (`checkStanding`) and its body's mark
 *  (`statement-state.ts`'s `atBasis`) share, so the two never disagree. Whether anybody withdrew
 *  the result is no part of it: both read it only of a result that stands (`standingResults`). */
export function standsAtBasis(result: CheckResult): boolean {
  return result.check_words_moved !== true && result.commit_stands?.answer === "at_basis";
}

/** Every result of `check` nobody withdrew, which is every result that stands as evidence: the
 *  server's `passStandsOf` skips a withdrawn one, and so does every reading of the page. */
export function standingResults(check: CheckStateReads): readonly CheckResult[] {
  return (check.results ?? []).filter((result) => result.retracted === undefined);
}

/** `results` newest first, by the time each was recorded. The server already answers newest
 *  first; this reads the times themselves, so the order is the model's own and no caller
 *  depends on the order a read happened to arrive in. */
export function newestFirst(results: readonly CheckResult[]): readonly CheckResult[] {
  return [...results].sort(newerFirst);
}

/** Orders two results newest first by the time each was recorded, to the microsecond the server
 *  writes (`compareTimestamps`), which `Date.parse` would cut to the millisecond and so tie. A
 *  time that cannot be read (`isTimestamp`) is the oldest possible, so a result with no readable
 *  time never outranks one that has one, two such tie, and no time throws. */
function newerFirst(first: CheckResult, second: CheckResult): number {
  const firstRead = isTimestamp(first.created_at);
  const secondRead = isTimestamp(second.created_at);
  if (firstRead && secondRead) return compareTimestamps(second.created_at, first.created_at);
  return Number(secondRead) - Number(firstRead);
}

/** One check of a statement, as the body draws it. */
export interface CheckReading {
  /** Whether the result its state stands on stands for something at the commit the server names
   *  for the check: at the basis the check compares with (`commit_stands` is `at_basis`) and
   *  recorded against the check's words now, as the server's `standingCommit` reads it. It is
   *  `standsAtBasis`, which `checkStanding` reads too, so this mark and the check's state never
   *  disagree. */
  readonly atBasis: boolean;
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

/**
 * How a statement stands, from its checks' own states (R17): Met where every check of it reads
 * Passed, Not met where one reads Failed, and Unknown otherwise, which is where it carries no
 * check, where a check's evidence was recorded against another commit than the one the server
 * names for it, and where a check stands in any of the other five states.
 */
export function statementState(checks: readonly Pick<CheckReading, "state">[]): StatementState {
  if (checks.length === 0) return "unknown";
  if (checks.every((check) => check.state === "passed")) return "met";
  if (checks.some((check) => check.state === "failed")) return "not_met";
  return "unknown";
}

/** A title as the page reads it: a reading of one paragraph holding the title as one text run,
 *  so nothing in it is parsed, since a title is no markdown: a parse would read
 *  `1. Migrate the table` as a list item with no label, and leave out the marks of
 *  ``Add `x` flag``. The line of the issue and of a child issue is projected from it
 *  (`issue-tree.ts`'s `lineReading`), and so is the name of a chip naming an issue
 *  (`treeContext`), so a row and a chip read one title by one rule. */
export function titleReading(title: string): DisplayTree {
  return {
    content: [{ content: [{ text: title, type: "text" }], type: "paragraph" }],
    type: "doc",
  };
}
