import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { issueAskThreadsQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { CommentWithTrees, IssueDetails } from "../../../api/types";
import { standsFailed } from "../../refs/reference-target";
import { type DecisionNode, decisionNodes } from "../decision-nodes/decision-nodes";
import {
  type ChangeComment,
  decisionNodeChanges,
  deletedPostIds,
  parentsIn,
  treeComments,
} from "../decision-review";
import type { TreeNode } from "../issue-tree";
import { changeUnread, coverageStatementOf, treeContext } from "../tree-drafts";
import { holdsPlanTask } from "../tree-read/tree-nodes";
import {
  outlineQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  treeRead,
  useDecisionsView,
  useReadOlderDecisionComments,
} from "../tree-read/tree-read";
import { issueMessagesQuery, viewTree } from "../useIssueTree";
import { askIdOf, type DecisionsParts, decisionsParts, placements } from "./decisions-parts";

/**
 * The one seam between the page's reads and the Decisions view: it
 * turns the decisions view, the tree read and the issue's asks into the view's parts, so no
 * other file of the view names a read. Once the server's decisions model gives the view its
 * entries, that model's read replaces this module and leaves the view as it is.
 *
 * The decisions are the decision nodes of the decisions view (`?view=decisions`), which holds
 * the issue's decisions wherever they are written. The page holds that view on the issue's node
 * (`TreeNode.views`), and each decision is read as every reader of a decision reads it
 * (`decisionNodes`). A decision's Change is the comment the read its decision is read from hangs
 * under its node (`decision-review.ts`' `decisionNodeChanges`), and no quote is matched for it.
 * Either read can omit an older Change. A decision that may have one (`changeUnread`) stays in
 * the review list with "+" and is excluded from the count until the older pages resolve it.
 * After the first drawing, the seam reads those pages from the same answer as its decisions.
 * The same read answers on each comment node what its author did to it (`treeComments`):
 * which Changes their author deleted (`deletedPostIds`), and when its author last edited a
 * Change, which its row draws. A view that holds no decision gives none, and no error.
 *
 * The nodes the parts stand on are that view read's too (R2): the tree its answer builds by the
 * page's one tree rule, as each filter's view read builds its own (`useDecisionsTree`,
 * `useIssueTree.ts`' `viewTree`), so each question and each decision it holds is a node there,
 * with its words and its state. Conversation's read is paged, and the page reads a next page of
 * it only when the reader asks for one (R15): a question or a decision past the pages read so far
 * stands in its part all the same, and is no row of the view only for want of a page.
 * Conversation's read gives that tree only what the view read leaves out - each statement a tie
 * names, each node a question or a decision hangs under - where it holds it, so the tree waits
 * for that read to answer. Which address names a statement is the coverage read's too, which is
 * no paged read: a tie to a statement past the pages of Conversation's read decides a decision's
 * part as one to a statement the tree holds (`decisionsParts`' `coverageStatement`, by the rule
 * the review list is read by everywhere, `tree-drafts.ts`' `coverageStatements`), and the row
 * draws its line for that statement from the coverage read's row of it: its first line, at its
 * own address.
 *
 * The page reads the view the first time the filter is on in a load (`ViewReadProvider`), and
 * every reader takes its answer through `tree-read.ts`' `useDecisionsView`: while a Keep or a
 * Change has dropped the view (`DecisionActions.tsx`), the view draws the answer the drop took,
 * with its failed line over it where that read is refused, and every change of the issue marks
 * it stale and reads it again while the answer stays drawn (`subscribeTreeFreshness`). Until
 * the view holds an answer, Conversation's read stands in (`tree-read.ts`' `outlineOf`): the
 * decisions and their Changes are the decision nodes of the tree read the page holds beside the
 * view, which has the same nodes with the same members, and the comments it hangs under them,
 * and the nodes the parts stand on are the page's tree, built from the pages of that read held
 * so far. So every decision that read holds stands in its part with its state, never in the
 * folded foot for want of a row, and the focus stays with it.
 *
 * The reads are the page's own, under the keys it reads them by (`useIssueTree.ts`,
 * `ViewReadProvider`): Conversation's read, the decisions view, the issue's asks and its
 * coverage, the asks by the tree's own definition of that read (`issueAskThreadsQuery`), and the
 * other reads the page's tree is built from, which the view read's tree is built from too; each
 * watched and never sent, so the view adds no request of its own and reads none of them again
 * when it is opened.
 *
 * The view draws the model's parts as the model gives them. A decision another person made
 * stands in `others`, which the view draws under "Decided by others", after the reader's own
 * decisions and before the agents' at the foot, each row naming that person; where the reader
 * made every decision `others` is empty, and the view draws no part for it.
 *
 * The seam says too, beside the parts, whether the view knows what the issue holds
 * (`readsHeld`), which the outline's keys (`useTreeKeymap.ts`) and the body's empty line read.
 * Conversation's read is the one the tree the view is drawn over waits for (`useDecisionsTree`),
 * and the one a decision and its Change are read from until the view answers, so until that read
 * has answered a decision may stand in a part that is not its own, and until it and the asks
 * read have answered, a view with no question or no decision to go to may be one whose questions
 * and decisions have not arrived. So until both have answered, `q` and `w` with nowhere to go say
 * nothing rather than that there is none, and the body draws no line that the issue holds
 * nothing, as the outline does (`slots/body.tsx`).
 *
 * The seam says too which of the two kinds of read the entries come from stands refused
 * (`unreadable`), which of them is being read now (`retrying`) and how many times each has
 * failed (`failures`), and asks one of them again (`retry`). The decisions' reads are the tree
 * read and the decisions view: they stand refused where the tree read stands failed, or where the
 * decisions view stands failed and Conversation's read does not stand in for it - the page
 * draws the view's answer a Keep or a Change dropped, or the tree read is still out - and their
 * Retry reads again each of the two that stands failed, or the tree read where neither does. The
 * asks read is the other: the open questions are the issue's open asks, which only the asks read
 * says, and it says which answered question is a person's decision only where the read carries
 * no decision on the ask, whose `links` decide it otherwise (`decisionNodes`). A read stands
 * refused by the page's one rule for a failed read (`standsFailed`): it holds no answer, and a
 * fetch of it failed since it last held one. Its status is not the rule: TanStack Query sets a
 * read that holds no answer back to pending, its error cleared, the moment it runs again, and the
 * read stays refused until it answers. The body draws a line with a Retry for each refused read,
 * over the entries the page still holds of it - none, but for the decisions view's answer a drop
 * took, whose rows stand under the line; while the read runs again the line stays, with the focus
 * its Retry holds, the Retry reads busy, and a Retry pressed then asks nothing more. The line
 * says the read's failure again only for a new one: a run that ends with none, cancelled or
 * paused while the browser is offline, leaves the count as it was, and with it the line's alert.
 * How a read stands and how it is asked again belong to the read, and only this module names
 * one, so they are read here and the body knows a read only by the word its line is drawn for. A
 * read whose refetch fails over an answer it already holds keeps that answer, which the view
 * still draws from, so it is not among the refused.
 *
 * The seam says too which nodes are a write-up of an answered question (`writeUps`) and which
 * question each one restates: a `reading` node the tree places under an answered ask
 * (`tree-read/tree-nodes.ts`'s `draftReading`), which restates the person's answer, and which
 * the view draws no row for (R9), so an address naming one lands on the row of the question it
 * answers. The asks read says which ask is answered and the tree read where each reading
 * stands, so only with both can the seam tell. A decision that restates a person's answer is
 * none of them: the tree holds no node of it (`decision-nodes.ts`' `restatesAnswer`, INFRA-1196's
 * decision 513), so no part holds it and no count counts it, a reading of it stands under the
 * question it restates and is a write-up of that question, and an address naming the decision
 * lands on that question by the rule every view lands an address by (`issue-address.ts`'
 * `pathToTarget`).
 *
 * The module's exports are its two hooks and `changesOf`, which the parts' tests read the Changes
 * by: the view's parts with how their reads stand (`useDecisionsEntries`), whose answer's type is
 * its own, and the tree the view is drawn over (`useDecisionsTree`), which the view's slot reads
 * so the view's words filter and the node its address names are read over the parts' own nodes.
 * So the read that replaces this module has those names to give the view and nothing else of it
 * to keep.
 */

/** The two kinds of read the entries come from, in the order the body says which of them stands
 *  refused: the decisions' reads, the tree read and the decisions view, and the asks. */
type DecisionsRead = "decisions" | "asks";

/** The seam's answer: the view's parts, and beside them whether the view knows what the issue
 *  holds, which of its reads stands refused, which runs now and how many times each has failed,
 *  and how to ask one again. */
interface DecisionsEntries {
  /** How many times each of the decisions' reads and the asks has failed, by its word (TanStack
   *  Query's `errorUpdateCount`, the tree read's and the decisions view's together). It keys the
   *  alert of a refused read's line (`FailedReadLine`'s `failure`), so each new failure is said
   *  again, and a run that ends with none, cancelled or paused while the browser is offline, says
   *  nothing again. */
  readonly failures: Readonly<Record<DecisionsRead, number>>;
  /** Which entry stands in which part, each part in the order it is drawn, as the model gives
   *  them: another person's decisions stand in `others`. */
  readonly parts: DecisionsParts;
  /** Whether the view knows what the issue holds: Conversation's read, which the tree the view
   *  is drawn over waits for, and the asks have both answered. A read that failed holds no
   *  answer. */
  readonly readsHeld: boolean;
  /** Asks `read` again, by its own queries' refetch: the asks read, and for the decisions each
   *  of the tree read and the decisions view that stands refused, or the tree read, which says
   *  what the view read leaves out, where neither does. */
  readonly retry: (read: DecisionsRead) => void;
  /** Each of the decisions' reads and the asks, in that order, that is being read now, a first
   *  read, a Retry's or any other (TanStack Query's `isFetching`): for the decisions, the tree
   *  read or the decisions view. A refused read stays refused while it runs again, so its line
   *  stays drawn, its Retry busy. */
  readonly retrying: readonly DecisionsRead[];
  /** Each of the decisions' reads and the asks, in that order, that stands refused
   *  (`standsFailed`): it holds no answer, and a fetch of it failed since it last held one. A read
   *  stays here while it runs again, until it answers. */
  readonly unreadable: readonly DecisionsRead[];
  /** Whether the decisions view read has answered, its answer held on the issue's node
   *  (`TreeNode.views`), or stands refused (`standsFailed`). Until then a question or a decision
   *  past the pages of Conversation's read held so far, which only that read holds, may yet come
   *  into the tree the view is drawn over, so the body says nothing yet of whether it draws the
   *  node an address names (`DecisionsFilterBody`'s `onRevealDrawn`). Refused, the tree is the
   *  one the answer a Keep or a Change dropped builds, where the page draws that answer, and else
   *  Conversation's, and the body says what it draws of that. */
  readonly viewSettled: boolean;
  /** Each write-up of an answered question of this issue - a `reading` node the tree places
   *  under an answered ask - by its node's id, with the node id of the question it restates. The
   *  list draws no row for a write-up, so an address naming one lands on the row of the question
   *  it answers. Undefined while the asks read or the decisions' reads hold no answer and are not
   *  refused: until both answer, the tree may not yet place a reading under the ask it reads. */
  readonly writeUps: ReadonlyMap<string, ReadonlySet<string>> | undefined;
}

/** The Decisions view's parts and how the reads they come from stand, from the issue, the
 *  reader's login, the whole tree the view was drawn over and the tree as the Decisions view
 *  leaves it (`presetView`). */
export function useDecisionsEntries({
  issue,
  reader,
  root,
  view,
}: {
  issue: IssueDetails;
  /** Who is reading, which tells their own decisions from other people's. */
  reader: string | undefined;
  /** The whole tree `view` was drawn over: the tree the decisions view read builds
   *  (`useDecisionsTree`), which the slot hands its body, and Conversation's until that read
   *  holds an answer. It holds each ask and each decision and each statement a tie names, and on
   *  the issue's node the decisions view the page holds (`TreeNode.views`). */
  root: TreeNode;
  /** The tree as the Decisions view leaves it. */
  view: TreeNode;
}): DecisionsEntries {
  // The tree read the page holds in the Decisions view, Conversation's read, which holds each
  // statement a tie names, and the decisions and their Changes until the view answers.
  const issueTreeRead = useQuery(treeRead(outlineQuery(issue.key, "conversation")));
  const tree = issueTreeRead.data;
  // The decisions view read as this load holds it, by the one rule every reader of it takes
  // (`useDecisionsView`): whether it stands refused, whether it runs, how many times it failed,
  // and its refetch. The page reads it (`ViewReadProvider`) and holds its answer on the issue's
  // node (`TreeNode.views`).
  const {
    errorUpdateCount: viewFailures,
    isFetching: viewFetching,
    refetch: readView,
    refused: viewRefused,
  } = useDecisionsView(issue.key);
  // The issue's asks, watched the same way: the page's tree reads them under this key. The
  // decision the read carries on an answered ask, by its `links`, makes it a person's decision;
  // the asks read is only the fallback where the read carries none (`decisionNodes`).
  const asksRead = useQuery(treeRead(issueAskThreadsQuery(issue.key)));
  const asks = asksRead.data;
  // The decisions view the page holds, and the tree read wherever the view holds no answer: the
  // decisions are read from it, and the Changes it hangs under them, with what each Change's
  // author did to it, as its node answers that (`changesOf`).
  const viewAnswer = root.views?.decisions;
  const held = viewAnswer ?? tree;
  const decisions = useMemo(
    () => (held === undefined ? undefined : decisionNodes(held, { asks, issue })),
    [asks, held, issue]
  );
  const changes = useMemo(() => changesOf(held, decisions ?? []), [decisions, held]);
  // The statements the coverage read holds, watched the same way: the page's tree reads it under
  // this key. A tie names a statement by the rule the review list is read by everywhere
  // (`coverageStatements`), beside the tree's own requirement nodes, so a statement past the
  // pages of Conversation's read the page holds decides a decision's part all the same, and the
  // row draws it from the read's row of it (`coverageStatementOf`).
  const coverage = useQuery(treeRead(issueCoverageQuery(issue.key))).data;
  const coverageStatement = useMemo(() => coverageStatementOf(issue, coverage), [coverage, issue]);
  // The tree the view is drawn over waits for Conversation's read (`useDecisionsTree`): the view
  // knows what the issue holds once that read has answered, and the asks read too.
  const readsHeld = tree !== undefined && asks !== undefined;
  // A read stands refused by the page's one rule (`standsFailed`), as the Spec view judges the
  // same reads: it holds no answer, and a fetch of it failed since it last held one. Not by its
  // status: a refetch that fails over the answer the read last gave sets the error and keeps that
  // answer, which the view draws from, and a read that holds no answer reads pending again, its
  // error cleared, the moment it runs again, though it stays refused until it answers. A refused
  // decisions view counts unless Conversation's read stands in for it: it counts where the page
  // draws the answer a Keep or a Change dropped, so the line and its Retry stand over the rows
  // that answer gives, and where the tree read holds no answer either.
  const treeRefused = standsFailed(issueTreeRead);
  const decisionsRefused =
    treeRefused || (viewRefused && (viewAnswer !== undefined || tree === undefined));
  const asksRefused = standsFailed(asksRead);
  // Whether the decisions view read has said what it holds: its answer is on the issue's node, or
  // it stands refused.
  const viewSettled = viewAnswer !== undefined || viewRefused;
  const unreadable = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsRefused ? (["decisions"] as const) : []),
      ...(asksRefused ? (["asks"] as const) : []),
    ],
    [asksRefused, decisionsRefused]
  );
  const decisionsFetching = issueTreeRead.isFetching || viewFetching;
  const asksFetching = asksRead.isFetching;
  const retrying = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsFetching ? (["decisions"] as const) : []),
      ...(asksFetching ? (["asks"] as const) : []),
    ],
    [asksFetching, decisionsFetching]
  );
  const decisionsFailures = issueTreeRead.errorUpdateCount + viewFailures;
  const asksFailures = asksRead.errorUpdateCount;
  const failures = useMemo<Readonly<Record<DecisionsRead, number>>>(
    () => ({ asks: asksFailures, decisions: decisionsFailures }),
    [asksFailures, decisionsFailures]
  );
  // Each read is asked again by its own query, so a Retry asks for its reads and no other.
  const { refetch: readTree } = issueTreeRead;
  const { refetch: readAsks } = asksRead;
  const retry = useCallback(
    (read: DecisionsRead) => {
      if (read === "asks") {
        void readAsks();
        return;
      }
      if (treeRefused || !viewRefused) void readTree();
      if (viewRefused) void readView();
    },
    [readAsks, readTree, readView, treeRefused, viewRefused]
  );
  const unread = useMemo(
    () => decisions?.some((row) => changeUnread(row, changes.has(row.ref), held)) === true,
    [changes, decisions, held]
  );
  useReadOlderDecisionComments(issue.key, held, unread, viewAnswer !== undefined);
  // How this issue addresses a node, which is the one rule the tree was built with, so a
  // `shaped` tie resolves to the node it names (`tree-drafts.ts`).
  const nodeIdOf = useMemo(() => treeContext({ issue }).nodeIdOf, [issue]);
  // Each write-up of an answered question the tree holds, by its node's id, with the question it
  // restates: a `reading` node at its first placement in the reading order, which is where the
  // tree places it (`placements`), under an ask that holds an answer, the ask the tree read
  // names it a reading of (`tree-read/tree-nodes.ts`'s `draftReading`).
  const restating = useMemo(() => {
    const placed = placements(root);
    const found = new Map<string, ReadonlySet<string>>();
    for (const { node, parent } of placed.values()) {
      if (node.type !== "reading" || parent === undefined) continue;
      const held = placed.get(parent.id)?.node;
      if (held === undefined || askIdOf(held) === undefined || held.answer === undefined) continue;
      found.set(node.id, new Set([held.id]));
    }
    return found;
  }, [root]);
  // From the model's inputs alone: how a read stands is no input of the model, so while only
  // that changes the parts keep their identity and nothing the body works out from them is
  // worked out again. A write-up the address names is no row of the view (R9): the model draws
  // it as the node the address names (`named`), and it is left out here.
  const parts = useMemo<DecisionsParts>(() => {
    const model = decisionsParts({
      asks,
      changes,
      coverageStatement,
      decisions,
      issueKey: issue.key,
      nodeIdOf,
      reader,
      root,
      view,
    });
    return {
      ...model,
      named: model.named.filter((entry) => !restating.has(entry.node.id)),
    };
  }, [
    asks,
    changes,
    coverageStatement,
    decisions,
    issue.key,
    nodeIdOf,
    reader,
    restating,
    root,
    view,
  ]);
  // Until the asks read and the decisions' reads have answered, or stand refused, the tree may
  // not yet place a reading under the answered ask it reads, so no write-up is told yet.
  const writeUps =
    (asks === undefined && !asksRefused) || (decisions === undefined && !decisionsRefused)
      ? undefined
      : restating;
  return { failures, parts, readsHeld, retry, retrying, unreadable, viewSettled, writeUps };
}

/**
 * The whole tree the Decisions view reads, for the issue whose tree, the page's, is `root`: once
 * the page holds the decisions view read's answer on the issue's node (`TreeNode.views`) and
 * Conversation's read has answered, the tree that answer builds by the page's one tree rule, as
 * each filter's view read builds its own (`useIssueTree.ts`' `viewTree`, `TreeViews.trees`),
 * carrying the issue's node's views; until then `root` itself, Conversation's tree standing in.
 * The answer is the one the page holds (`tree-read.ts`' `useDecisionsView`), so a question or a
 * decision only that read holds keeps its row while a Keep's read is out, its row the focus,
 * and a refused Keep the refusal it draws.
 *
 * The view read holds every question and every decision of the issue, whatever page of
 * Conversation's read the page has read so far. It carries comments on the text of those nodes,
 * newest first, in comment pages of its own. Conversation's read gives the tree each statement
 * a tie names and each node a question or a decision hangs under, but not comments on decisions
 * (`contextForDecisions`). So the tree waits for that read: without it those
 * nodes are missing, and the page's tree is the issue alone until then (`issue-tree.ts`'
 * `buildIssueTree`), so the view draws no decision until it can draw each with them. The tree is
 * built from the reads the page's tree is built from, under the keys it reads them by and each
 * watched and never sent: the asks read, which says which questions wait on `viewer`, the
 * messages, which mark a message its author edited or deleted, as the answer's own nodes mark a
 * comment, the coverage and the results, and of the Plan view read's answer the issue's node
 * holds (`TreeNode.views`) whether it holds a plan task. It is built without the names read,
 * whose set of addresses is the page's tree's own: no row the view draws names a chip. A view
 * answer over one answer of Conversation's read is built once (`viewTree`).
 *
 * The view's slot draws the view over this tree (`slots/view-decisions.tsx`): the node its
 * address names, the nodes its words filter keeps, its rows and its keys are read over the nodes
 * its parts stand on.
 */
export function useDecisionsTree(
  issue: IssueDetails,
  root: TreeNode,
  viewer: string | undefined
): TreeNode {
  const key = issue.key;
  const views = root.views;
  const answer = views?.decisions;
  const tree = useQuery(treeRead(outlineQuery(key, "conversation"))).data;
  const asks = useQuery(treeRead(issueAskThreadsQuery(key))).data;
  const coverage = useQuery(treeRead(issueCoverageQuery(key))).data;
  const messages = useQuery(treeRead(issueMessagesQuery(key))).data;
  const results = useQuery(treeRead(issueResultsQuery(key))).data;
  // Of the Plan view's answer the tree reads only whether it holds a plan task, as the page's
  // tree reads it (`useIssueTree.ts`), from the answer the page holds on the issue's node.
  const plan = views?.plan;
  const planTaskHeld = useMemo(() => holdsPlanTask(plan), [plan]);
  return useMemo(() => {
    if (answer === undefined || tree === undefined) return root;
    const built = viewTree(
      {
        asks,
        coverage,
        holdsPlanTask: planTaskHeld,
        issue,
        messages,
        results,
        tree: contextForDecisions(tree),
        viewer,
      },
      answer
    );
    return { ...built, views };
  }, [answer, asks, coverage, issue, messages, planTaskHeld, results, root, tree, viewer, views]);
}

const decisionsContext = new WeakMap<TreeAnswer, TreeAnswer>();

/** Conversation supplies context for decisions, not their Changes. Otherwise a Change that
 *  Conversation holds but the decisions view has not read yet can clear the mark before the
 *  view's own older page lands. Keep the filtered answer stable for both the list and its count. */
function contextForDecisions(tree: TreeAnswer): TreeAnswer {
  const held = decisionsContext.get(tree);
  if (held !== undefined) return held;
  const nodes = tree.nodes ?? [];
  const excluded = new Set<TreeAnswerNode>();
  for (const [node, parent] of parentsIn(nodes)) {
    if (excluded.has(parent) || (node.supertag === "comment" && parent.supertag === "decision")) {
      excluded.add(node);
    }
  }
  const context =
    excluded.size === 0 ? tree : { ...tree, nodes: nodes.filter((node) => !excluded.has(node)) };
  decisionsContext.set(tree, context);
  return context;
}

/**
 * The Change each of `decisions` waits on, by the decision's own address (`Decision.ref`), as
 * the parts read one (`decisionsParts`' `changes`): the comment `answer` hangs under the
 * decision's node that `decisionNodeChanges` gives it, with its words and the server's reading
 * of them where the read sent one, its author, the time it was made and the time its author
 * last edited it. `answer` is the read the decisions are read from, which answers each of their
 * nodes by the same id: the decisions view's wherever the page holds it, and Conversation's
 * read until then. A Change past Conversation's pages may be in the decisions view's own pages.
 * Which Changes their author deleted, which ask nothing any more, and when its author last
 * edited one are the marks `answer` answers on the comment's node (`treeComments`,
 * `deletedPostIds`): a Change carries no edit time where the read answers no edit mark. None
 * while `answer` has not answered.
 */
export function changesOf(
  answer: TreeAnswer | undefined,
  decisions: readonly DecisionNode[]
): ReadonlyMap<string, ChangeComment & Pick<CommentWithTrees, "body_tree">> {
  const changes = new Map<string, ChangeComment & Pick<CommentWithTrees, "body_tree">>();
  if (answer === undefined) return changes;
  const asked = decisionNodeChanges(answer, undefined, deletedPostIds(treeComments(answer)));
  for (const decision of decisions) {
    const comment = asked.get(decision.node.id);
    // A Change is a person's comment the read records a time for (`decisionNodeChanges`), so it
    // always has an author, the first the read lists for it, and a time.
    const author = comment?.author?.[0];
    if (comment === undefined || author === undefined || comment.made_at === null) continue;
    changes.set(decision.ref, {
      author,
      body: comment.text,
      ...(comment.text_tree === undefined ? {} : { body_tree: comment.text_tree }),
      created_at: comment.made_at,
      ...(comment.edited_at === undefined ? {} : { edited_at: comment.edited_at }),
      id: comment.id,
    });
  }
  return changes;
}
