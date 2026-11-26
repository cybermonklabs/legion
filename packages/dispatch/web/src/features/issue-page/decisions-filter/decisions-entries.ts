import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import type { Comment, CommentWithTrees, IssueDetails } from "../../../api/types";
import { standsFailed } from "../../refs/reference-target";
import { type DecisionNode, decisionNodes } from "../decision-nodes/decision-nodes";
import { type ChangeComment, decisionNodeChanges, deletedPostIds } from "../decision-review";
import type { TreeNode } from "../issue-tree";
import { treeContext } from "../tree-drafts";
import { outlineQuery, type TreeAnswer, treeRead, viewQuery } from "../tree-read/tree-read";
import { issueAskThreadsQuery, issueCommentsQuery } from "../useIssueTree";
import { askIdOf, type DecisionsParts, decisionsParts, placements } from "./decisions-parts";

/**
 * The one seam between the page's reads and the Decisions view: it
 * turns the decisions view, the tree read, the issue's asks and its comments into the view's
 * parts, so no other file of the view names a read. Once the server's decisions model gives the
 * view its entries, that model's read replaces this module and leaves the view as it is.
 *
 * The decisions are the decision nodes of the decisions view (`?view=decisions`), which holds
 * the issue's decisions wherever they are written. The page holds that view on the issue's node
 * (`TreeNode.views`), and each decision is read as every reader of a decision reads it
 * (`decisionNodes`). A decision's Change is the comment the tree read hangs under its node
 * (`decision-review.ts`' `decisionNodeChanges`), so no quote is matched for it. The comments read
 * says what the tree read answers no mark of: which Changes their author deleted, which ask
 * nothing any more (`deletedPostIds`), and when its author last edited a Change, which its row
 * draws. A view that holds no decision gives none, and no error.
 *
 * The page reads the view the first time the filter is on in a load (`ViewReadProvider`). A Keep
 * or a Change drops every view the page holds (`DecisionActions.tsx`), and every change of the
 * issue marks it stale and reads it again while the answer stays drawn
 * (`subscribeTreeFreshness`). Until the view answers, and where its read fails, the decisions
 * are the decision nodes of the tree read the page holds beside the view, Conversation's read
 * (`tree-read.ts`' `outlineOf`), which has the same nodes with the same members. So every
 * decision stands in its part with its state, never in the folded foot for want of a row, and
 * the focus stays with it.
 *
 * The reads are the page's own, under the keys it reads them by (`useIssueTree.ts`,
 * `ViewReadProvider`): Conversation's read, the decisions view, the issue's asks and its
 * comments, the asks and the comments by the tree's own definitions of those reads
 * (`issueAskThreadsQuery`, `issueCommentsQuery`), each watched and never sent, so the view adds
 * no request of its own and reads none of them again when it is opened.
 *
 * The view draws the model's parts as the model gives them. A decision another person made
 * stands in `others`, which the view draws under "Decided by others", after the reader's own
 * decisions and before the agents' at the foot, each row naming that person; where the reader
 * made every decision `others` is empty, and the view draws no part for it.
 *
 * The seam says too, beside the parts, whether the view knows what the issue holds
 * (`readsHeld`), which the outline's keys (`useTreeKeymap.ts`) and the body's empty line read.
 * The view's rows are the tree read's: the parts walk the page's tree (`decisionsParts`), which
 * holds the issue alone until the tree read answers (`issue-tree.ts`'s `buildIssueTree`), so
 * before then no decision has a row to stand on, whatever the decisions view holds. So until the
 * tree read has answered, and the asks read too, a view with no question or no decision to go to
 * may be one whose questions and decisions have not arrived, so `q` and `w` with nowhere to go
 * say nothing rather than that there is none, and the body draws no line that the issue holds
 * nothing, as the outline does (`slots/body.tsx`).
 *
 * The seam says too which of the two kinds of read the entries come from stands refused
 * (`unreadable`), which of them is being read now (`retrying`) and how many times each has
 * failed (`failures`), and asks one of them again (`retry`). The decisions' reads are the tree
 * read and the decisions view: they stand refused where the tree read stands failed, or where it
 * is still out and the decisions view stands failed, and their Retry reads again each of the two
 * that stands failed, or the tree read where neither does. The asks read is the other: the open
 * questions are the issue's open asks, which only the asks read says, and it says which answered
 * question is a person's decision only where the read carries no decision on the ask, whose
 * `links` decide it otherwise (`decisionNodes`). A read stands refused by the page's one rule
 * for a failed read (`standsFailed`): it holds no answer, and a fetch of it failed since it last
 * held one. Its status is not the rule: TanStack Query sets a read that holds no answer back to
 * pending, its error cleared, the moment it runs again, and the read stays refused until it
 * answers. The body draws a line with a Retry for each refused read, in place of the entries that
 * read would give; while the read runs again the line stays, with the focus its Retry holds, the
 * Retry reads busy, and a Retry pressed then asks nothing more. The line says the read's failure
 * again only for a new one: a run that ends with none, cancelled or paused while the browser is
 * offline, leaves the count as it was, and with it the line's alert. How a read stands and how it
 * is asked again belong to the read, and only this module names one, so they are read here and
 * the body knows a read only by the word its line is drawn for. A read whose refetch fails over
 * an answer it already holds keeps that answer, which the view still draws from, so it is not
 * among the refused.
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
 * The hook is the module's one export but `changesOf`, which the parts' tests read the Changes
 * by, and its answer's type is its own, so the read that replaces this module has one name to
 * give the view and nothing else of it to keep.
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
  /** Whether the view knows what the issue holds: the tree read, which its rows come from, and
   *  the asks have both answered. A read that failed holds no answer. */
  readonly readsHeld: boolean;
  /** Asks `read` again, by its own queries' refetch: the asks read, and for the decisions each
   *  of the tree read and the decisions view that stands refused, or the tree read, which the
   *  rows come from, where neither does. */
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
  /** Each write-up of an answered question of this issue - a `reading` node the tree places
   *  under an answered ask - by its node's id, with the node id of the question it restates. The
   *  list draws no row for a write-up, so an address naming one lands on the row of the question
   *  it answers. Undefined while the asks read or the decisions' reads hold no answer and are not
   *  refused: until both answer, the tree may not yet place a reading under the ask it reads. */
  readonly writeUps: ReadonlyMap<string, ReadonlySet<string>> | undefined;
}

/** The Decisions view's parts and how the reads they come from stand, from the issue, the
 *  reader's login, the whole tree and the tree as the Decisions view leaves it (`presetView`). */
export function useDecisionsEntries({
  issue,
  reader,
  root,
  view,
}: {
  issue: IssueDetails;
  /** Who is reading, which tells their own decisions from other people's. */
  reader: string | undefined;
  /** The whole tree, which holds each ask and each decision and each statement a tie names, and
   *  on the issue's node the decisions view the page holds (`TreeNode.views`). */
  root: TreeNode;
  /** The tree as the Decisions view leaves it. */
  view: TreeNode;
}): DecisionsEntries {
  // The tree read the page holds in the Decisions view, Conversation's read, which holds each
  // Change under its decision, and the decisions until the view answers.
  const issueTreeRead = useQuery(treeRead(outlineQuery(issue.key, "conversation")));
  const tree = issueTreeRead.data;
  // The decisions view read, watched the same way for how it stands: the page reads it
  // (`ViewReadProvider`) and holds its answer on the issue's node (`TreeNode.views`).
  const viewRead = useQuery(treeRead(viewQuery(issue.key, "decisions")));
  // The issue's asks, watched the same way: the page's tree reads them under this key. The
  // decision the read carries on an answered ask, by its `links`, makes it a person's decision;
  // the asks read is only the fallback where the read carries none (`decisionNodes`).
  const asksRead = useQuery(treeRead(issueAskThreadsQuery(issue.key)));
  const asks = asksRead.data;
  // The decisions view the page holds, and the tree read wherever the view holds no answer.
  const held = root.views?.decisions ?? tree;
  const decisions = useMemo(
    () => (held === undefined ? undefined : decisionNodes(held, { asks, issue })),
    [asks, held, issue]
  );
  // The issue's comments, watched the same way: the page's tree reads them under this key, and
  // they say which Changes their author deleted and when one was last edited (`changesOf`).
  const comments = useQuery(treeRead(issueCommentsQuery(issue.key))).data;
  const changes = useMemo(
    () => changesOf(tree, decisions ?? [], comments),
    [comments, decisions, tree]
  );
  // The rows are the tree read's (see above): the view knows what the issue holds once that read
  // has answered, and the asks read too.
  const readsHeld = tree !== undefined && asks !== undefined;
  // A read stands refused by the page's one rule (`standsFailed`), as the Spec view judges the
  // same reads: it holds no answer, and a fetch of it failed since it last held one. Not by its
  // status: a refetch that fails over the answer the read last gave sets the error and keeps that
  // answer, which the view draws from, and a read that holds no answer reads pending again, its
  // error cleared, the moment it runs again, though it stays refused until it answers. A refused
  // decisions view counts only while neither it nor the tree read holds an answer.
  const treeRefused = standsFailed(issueTreeRead);
  const viewRefused = standsFailed(viewRead);
  const decisionsRefused = treeRefused || (held === undefined && viewRefused);
  const asksRefused = standsFailed(asksRead);
  const unreadable = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsRefused ? (["decisions"] as const) : []),
      ...(asksRefused ? (["asks"] as const) : []),
    ],
    [asksRefused, decisionsRefused]
  );
  const decisionsFetching = issueTreeRead.isFetching || viewRead.isFetching;
  const asksFetching = asksRead.isFetching;
  const retrying = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsFetching ? (["decisions"] as const) : []),
      ...(asksFetching ? (["asks"] as const) : []),
    ],
    [asksFetching, decisionsFetching]
  );
  const decisionsFailures = issueTreeRead.errorUpdateCount + viewRead.errorUpdateCount;
  const asksFailures = asksRead.errorUpdateCount;
  const failures = useMemo<Readonly<Record<DecisionsRead, number>>>(
    () => ({ asks: asksFailures, decisions: decisionsFailures }),
    [asksFailures, decisionsFailures]
  );
  // Each read is asked again by its own query, so a Retry asks for its reads and no other.
  const { refetch: readTree } = issueTreeRead;
  const { refetch: readView } = viewRead;
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
  }, [asks, changes, decisions, issue.key, nodeIdOf, reader, restating, root, view]);
  // Until the asks read and the decisions' reads have answered, or stand refused, the tree may
  // not yet place a reading under the answered ask it reads, so no write-up is told yet.
  const writeUps =
    (asks === undefined && !asksRefused) || (decisions === undefined && !decisionsRefused)
      ? undefined
      : restating;
  return { failures, parts, readsHeld, retry, retrying, unreadable, writeUps };
}

/**
 * The Change each of `decisions` waits on, by the decision's own address (`Decision.ref`), as
 * the parts read one (`decisionsParts`' `changes`): the comment the tree read hangs under the
 * decision's node that `decisionNodeChanges` gives it, with its words and the server's reading
 * of them where the read sent one, its author, the time it was made and the time its author
 * last edited it. The decisions are the decisions view's, or the tree read's, and the Changes
 * the tree read's, which answer one node by the same id. A Change its author deleted asks
 * nothing any more, and when its author last edited one is answered by `comments`, the page's
 * comments read, which the tree read answers no mark of (`deletedPostIds`). None while the tree
 * read has not answered.
 */
export function changesOf(
  tree: TreeAnswer | undefined,
  decisions: readonly DecisionNode[],
  comments?: readonly Pick<Comment, "deleted_at" | "edited_at" | "id">[]
): ReadonlyMap<string, ChangeComment & Pick<CommentWithTrees, "body_tree">> {
  const changes = new Map<string, ChangeComment & Pick<CommentWithTrees, "body_tree">>();
  if (tree === undefined) return changes;
  const asked = decisionNodeChanges(tree, undefined, deletedPostIds(comments));
  const edited = new Map((comments ?? []).map((comment) => [comment.id, comment.edited_at]));
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
      edited_at: edited.get(comment.id) ?? null,
      id: comment.id,
    });
  }
  return changes;
}
