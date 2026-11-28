import { primarySpec } from "../../api/issue-cache";
import type {
  CheckBlock,
  CheckResult,
  DisplayTree,
  IssueCoverage,
  IssueDetails,
} from "../../api/types";
import { newestTimeFirst } from "../../lib/timestamps";
import { parseNodeReference } from "../refs/routes";
import type { CheckState, StatementState } from "./page-words";

/**
 * The pure rules the tree and Dashboard both read: how the issue addresses a node (`nodeIdOf`,
 * `blockNodeId`), which references name a statement of the review list (`coverageStatements`),
 * how a check stands on its own row (`checkStanding`), the one commit the page names for the
 * issue's results (`namedCommit`), how a check stands at that commit (`stateAtCommit`), how a
 * statement stands over its checks read there (`statementState`), and how a title is read as one
 * text run (`titleReading`). Dashboard's two tiles, the Verification view and the Spec view read a
 * statement by these alone, so each draws the same Met, Unknown and Out of date (R6, R14).
 *
 * They hold no React, make no request, and import nothing of the tree's model - `tree-drafts.ts`,
 * `issue-tree.ts`, `useIssueTree.ts` - not even its types, so Dashboard reads the one rule the
 * tree reads without carrying the tree in its chunk (R14), and a part that reads one of them, as
 * `RowText.tsx` reads `titleReading`, carries none of the tree's model in its imports.
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
 * calls it without building a whole `treeContext` (`target-naming.ts`), and `treeContext`
 * addresses nodes by it too, so the two never differ.
 */
export function nodeIdOf(
  issue: Pick<IssueDetails, "artifacts" | "key" | "primary_artifact_id">
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
 * (`tree-drafts.ts`'s `addDecisions`) and the header's count, which reads it through
 * `features/dashboard/useWaitingItems.ts`, so the count counts the very decisions the tree marks.
 */
export function coverageStatements(
  issue: Pick<IssueDetails, "artifacts" | "key" | "primary_artifact_id">,
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
 * check and a pass at a commit the check no longer compares with stand for nothing. A check that
 * reads the live deploy never comes here: `checkStanding` reads it from `pass_stands` alone
 * (`liveStanding`), as the server's `liveResultStands` decides it. The other states read what no
 * pass standing is:
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

/** What a check's state is read from: the server's answer over its results, the results
 *  themselves, and what the check reads and when it runs (`subject`, `when`), which say whether
 *  that answer is read against its program's current deploy (`readsTheLiveDeploy`). The page
 *  passes the check's own `CheckBlock`, which carries all of them. `pass_stands` is declared
 *  required by the API types and is missing from what the deployed server sends, so it is
 *  optional here and the page reads a payload without it rather than throwing over the whole
 *  issue page. */
export type CheckStateReads = Pick<CheckBlock, "results"> & {
  readonly pass_stands?: CheckBlock["pass_stands"];
  readonly subject?: CheckBlock["subject"];
  readonly when?: CheckBlock["when"];
};

/** How one check stands, as the server reads the commit each of its results ran at. */
export interface CheckStanding {
  /** How it stands, in the states `NODE_STATE_WORDS.check` names. */
  readonly state: CheckState;
  /** The result that state stands on: the newest result over the check's words now at its
   *  basis, a pass or a fail, where one is there; else the newest result that stands at all,
   *  and none for a check the server says passed with no pass at its basis in the read. For a
   *  check that reads the live deploy it is the result the server's answer names
   *  (`pass_stands.results`), where the read holds it. Absent where none stands. */
  readonly stands?: CheckResult;
}

/**
 * How a check stands on its own: the page's one reading of a check's state (R10, R17), and the
 * only one - `checkState` is its own fallback and nothing else reads it. The tree's check node
 * holds it (`tree-drafts.ts`'s `addChecks`), so the outline's row, the fold's frontier
 * (`tree-fold.ts`) and the Verification filter's failing count (`presets.ts`'s `filterSignals`)
 * read it, and so does the Verification filter's body (`statement-state.ts`'s `checkReading`):
 * one check never reads Passed on its row and Failed in the filter. A statement's state reads each
 * of its checks at the one commit the page names instead (`stateAtCommit`, R6), which starts from
 * this reading.
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
 * **A check that reads the live deploy is the server's answer alone.** A check of production or
 * of the graph, run after the merge or on the default branch (`readsTheLiveDeploy`, the page's
 * form of the server's function of that name in `results_deploys.go`), is decided by the
 * current deploy of the program it reads and not by its basis: the server counts only the
 * results recorded at the commit that deploy runs and after it went out, marks every other one
 * `not_counted` (`markNotCounted`), and answers over the rest (`liveResultStands` in
 * `results_pass_stands.go`). So such a check takes its state from `pass_stands`, with or without
 * results: `yes` is Passed and `no` for a fail is Failed, each standing on the result the answer
 * names (`pass_stands.results`) where the read holds it; `no` for no result is No result; and
 * `unknown` is Cannot tell, whatever its reason - no deploy of the program, no result since its
 * deploy, a program the check leaves unnamed, a deploy at a commit that does not hold the merge,
 * or a read that failed. A pass at its basis says nothing of it, so with no deploy, or after a
 * deploy at another commit, the page says what the route says: it cannot tell. One whose payload
 * carries no `pass_stands` reads Could not read, whatever results it holds. Every other check is
 * read as the rest of this says.
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
  if (readsTheLiveDeploy(check)) return liveStanding(check);
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

/** Whether `check` is read against the current deploy of the program it reads: a check of
 *  production or of the graph, run after the merge or on the default branch, as the server's
 *  `readsTheLiveDeploy` (`results_deploys.go`) decides which checks it compares so. */
function readsTheLiveDeploy(check: CheckStateReads): boolean {
  return (
    (check.subject === "production" || check.subject === "graph") &&
    (check.when === "after_merge" || check.when === "default_branch")
  );
}

/** How a check that reads the live deploy stands: the server's answer over the results its
 *  program's current deploy counts (`liveResultStands`), and nothing the page reads of the
 *  results itself, since which of them that deploy counts is the server's to say
 *  (`markNotCounted`). It stands on the result the answer names, the newest one that deploy
 *  counts, and on none where the answer names none or the read does not hold the one named. */
function liveStanding(check: CheckStateReads): CheckStanding {
  const answer = check.pass_stands;
  if (answer === undefined) return { state: "unreadable" };
  if (answer.answer === "unknown") return { state: "unmarked" };
  if (answer.answer === "no" && answer.reason === "no_result") return { state: "no-result" };
  const named = answer.results?.[0];
  const stands = (check.results ?? []).find((result) => result.id === named);
  const state = answer.answer === "yes" ? "passed" : "failed";
  return { state, ...(stands === undefined ? {} : { stands }) };
}

/** Whether `result` still speaks for its check, wherever its commit stands: it was recorded
 *  against the check's words now (`check_words_moved`), and the server counts it, since a result
 *  marked `not_counted`, one that names a commit its live check's current deploy does not run or
 *  that was recorded before that deploy went out (`markNotCounted`), speaks for no build that runs
 *  now. A result that does not is out of date whatever commit it ran at: it stands at no basis
 *  (`standsAtBasis`), and its own fact reads so (`check-facts.ts`'s `resultFacts`). */
export function speaksForTheCheckNow(result: CheckResult): boolean {
  return result.not_counted === undefined && result.check_words_moved !== true;
}

/** Whether `result` stands at its check's basis: it speaks for the check now
 *  (`speaksForTheCheckNow`), at the commit the server names for the check (`commit_stands` is
 *  `at_basis`), as the server's `standingCommit` reads it. It is the one reading of "this result
 *  stands at its check's basis" that a check's state (`checkStanding`) and its body's mark
 *  (`statement-state.ts`'s `atBasis`) share, so the two never disagree. Whether anybody withdrew
 *  the result is no part of it: both read it only of a result that stands (`standingResults`). */
export function standsAtBasis(result: CheckResult): boolean {
  return speaksForTheCheckNow(result) && result.commit_stands?.answer === "at_basis";
}

/** Every result of `check` nobody withdrew, which is every result that stands as evidence: the
 *  server's `passStandsOf` skips a withdrawn one, and so does every reading of the page. */
export function standingResults(check: CheckStateReads): readonly CheckResult[] {
  return (check.results ?? []).filter((result) => result.retracted === undefined);
}

/** `results` newest first, by the time each was recorded (`newestTimeFirst`). The server already
 *  answers newest first; this reads the times themselves, so the order is the model's own and no
 *  caller depends on the order a read happened to arrive in. */
export function newestFirst(results: readonly CheckResult[]): readonly CheckResult[] {
  return [...results].sort((first, second) => newestTimeFirst(first.created_at, second.created_at));
}

/**
 * The one commit the page names for the issue's results (root R17): the commit of the newest
 * result recorded against any check of the issue that still stands, against the words its check
 * has now, and names one. A result nobody withdrew is the evidence the page reads, so a withdrawn
 * result names no commit here; nor does one recorded against words its check no longer has
 * (`check_words_moved`), which the server's `standingCommit` (`results_comparison.go`) gives no
 * standing; and a result recorded with no commit at all names none either. It is `undefined`
 * where no result names one.
 *
 * The Verification view's body names it, and every statement's state is read at it (R6): each
 * check of a statement is read there by `stateAtCommit`, which Dashboard's two tiles read too, so
 * a statement reads Met only where every check that serves it passed at this commit. A check's own
 * row is not read at it: the row reads each check at the commit the server names for that check
 * (`checkStanding`, decision 152).
 */
export function namedCommit(checks: readonly CheckStateReads[]): string | undefined {
  const named = checks.flatMap((check) =>
    standingResults(check).filter(
      (result) => result.commit_sha !== "" && result.check_words_moved !== true
    )
  );
  return newestFirst(named)[0]?.commit_sha;
}

/**
 * How a check stands at the one commit the page names (`namedCommit`), which is how a statement's
 * state and Dashboard's two tiles read it (R6, root R17), so a tile and the view that draws the
 * same statements count by one rule. Passed and Failed hold only where the result the check's own
 * state stands on (`CheckStanding.stands`) was recorded at that commit; at any other commit the
 * check reads Out of date (`older`), so a pass at its own basis that another check's newer result
 * at another commit left behind leaves its statement Unknown. So does a check the server says
 * passed whose pass the read does not hold, since no result read shows it passed at that commit.
 * Each of the five other states is the check's own, since none of them is Passed or Failed at any
 * commit.
 *
 * Where no commit is named (`commit` is `undefined`: no result that stands names one), nothing is
 * read against a commit, and the check reads as it stands on its own (`checkStanding`), which is
 * how every statement read before R6 named the commit.
 *
 * A check's own row never reads it: its word is `checkStanding`'s (decision 152), so a check can
 * read Passed on its row and Out of date here.
 */
export function stateAtCommit(standing: CheckStanding, commit: string | undefined): CheckState {
  if (commit === undefined) return standing.state;
  if (standing.state !== "passed" && standing.state !== "failed") return standing.state;
  return standing.stands?.commit_sha === commit ? standing.state : "older";
}

/**
 * How a statement stands, from how its checks stand at the one commit the page names
 * (`stateAtCommit`, R6, root R17): Met where every check of it reads Passed there, Not met where
 * one reads Failed there, and Unknown otherwise, which is where it carries no check, where a
 * check's result was recorded at another commit than the one the page names, where a check's
 * evidence was recorded against another commit than the one the server names for it, and where a
 * check stands in any of the other five states. Dashboard's Met tile, the Verification view's
 * body and `statementStandings`, the Spec view's reading of the same statements, all read it, so
 * one statement reads one state in each.
 */
export function statementState(
  checks: readonly { readonly atCommit?: CheckState }[]
): StatementState {
  if (checks.length === 0) return "unknown";
  if (checks.every((check) => check.atCommit === "passed")) return "met";
  if (checks.some((check) => check.atCommit === "failed")) return "not_met";
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
