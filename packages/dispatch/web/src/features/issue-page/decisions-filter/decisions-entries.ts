import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { api } from "../../../api/client";
import type { IssueDetails } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { wordsOf } from "../../refs/tree-fragment";
import { type DecisionNode, decisionNodes } from "../decision-nodes/decision-nodes";
import { type ChangeComment, decisionNodeChanges } from "../decision-review";
import type { TreeNode } from "../issue-tree";
import { treeContext } from "../tree-drafts";
import { nodesInOrder } from "../tree-keys";
import { issueTreeQuery, type TreeAnswer, viewQuery } from "../tree-read/tree-read";
import { type DecisionsEntry, type DecisionsParts, decisionsParts } from "./decisions-parts";

/**
 * The one seam between the page's reads and the Decisions view: it
 * turns the decisions view, the tree read and the issue's asks into the view's
 * parts, so no other file of the view names a read. Once the server's decisions model gives the
 * view its entries, that model's read replaces this module and leaves the view as it is.
 *
 * The decisions are the decision nodes of the decisions view (`?view=decisions`), which holds
 * the issue's decisions wherever they are written. The page holds that view on the issue's node
 * (`TreeNode.views`), and each decision is read as every reader of a decision reads it
 * (`decisionNodes`). A decision's Change is the comment the tree read hangs under its node
 * (`decision-review.ts`' `decisionNodeChanges`), so no quote is matched for it. A view that
 * holds no decision gives none, and no error.
 *
 * The page reads the view the first time the filter is on in a load (`ViewReadProvider`). A Keep
 * or a Change drops every view the page holds (`DecisionActions.tsx`), and every change of the
 * issue marks it stale and reads it again while the answer stays drawn
 * (`subscribeTreeFreshness`). Until the view answers, and where its read fails, the decisions
 * are the decision nodes of the tree read the page holds, which has the same nodes with the same
 * members. So every decision stands in its part with its state, never in the folded foot for want
 * of a row, and the focus stays with it.
 *
 * The view's rows are the tree read's: the parts walk the page's tree (`decisionsParts`), which
 * holds the issue alone until the tree read answers (`issue-tree.ts`'s `buildIssueTree`), so
 * before then no decision has a row to stand on, whatever the decisions view holds. So until the
 * tree read has answered, and the asks read too, the view does not know what the issue holds, and
 * does not say it holds no decisions (`known`). Where the tree read failed with no answer held, or
 * is still out and the decisions view failed with no answer held, it says the decisions could not
 * be read, with a Retry that reads again each of the two that failed (`failed`, `retry`). The asks
 * read is the third read the decisions come from: a person's decision is a question they
 * answered, and the open questions are the issue's open asks, which only the asks read says.
 * Where it failed with no answer held, the view says the open questions could not be read, on a
 * line of its own after that one, with a Retry that reads the asks alone again (`asksFailed`,
 * `asksRetry`). A read asked again keeps its answer while it is out and when it fails, and the
 * view draws from that answer, so only a read that failed with no answer held counts as failed.
 *
 * The reads are the page's own, under the keys it reads them by (`useIssueTree.ts`,
 * `ViewReadProvider`): the tree read, the decisions view and the issue's asks, each watched and
 * never sent, so the view adds no request of its own.
 *
 * The view draws four parts. A decision another person made is no part of its own: it stands
 * under the reader's decisions, with that person's name on its row, so the model's `others` is
 * put in `yours`, newest first, and `others` is left empty.
 */

/** The Decisions view's parts, and how the reads they come from stand. */
export interface DecisionsEntries extends DecisionsParts {
  /** Whether the view knows what the issue holds: the tree read, which its rows come from, and
   *  the asks have both answered. Until then it cannot say there are none, and `q` and `w` with
   *  nowhere to go say nothing (`useTreeKeymap.ts`' `readsHeld`). */
  readonly known: boolean;
  /** Whether the view has no rows to draw because the tree read, which they come from, failed
   *  with no answer held, or is still out and the decisions view read failed with no answer
   *  held. */
  readonly failed: boolean;
  /** Reads again each of those two reads that failed with no answer held. */
  readonly retry: () => void;
  /** Whether the asks read failed with no answer held, which leaves the view no open question
   *  and no person's decision to draw. */
  readonly asksFailed: boolean;
  /** Reads the asks again. */
  readonly asksRetry: () => void;
}

/** The Decisions view's parts and how its reads stand, from the issue, the reader's login, the
 *  whole tree and the tree as the Decisions view leaves it (`presetView`). */
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
  // The tree read, which holds each Change under its decision, and the decisions until the view
  // answers, watched by an observer that is never enabled: the page's own read fetches it
  // (`useIssueTree.ts`), and the view sends none.
  const treeRead = useQuery({ ...issueTreeQuery(issue.key), enabled: false });
  const tree = treeRead.data;
  // The decisions view read, watched the same way for how it stands: the page reads it
  // (`ViewReadProvider`) and holds its answer on the issue's node (`TreeNode.views`).
  const viewRead = useQuery({ ...viewQuery(issue.key, "decisions"), enabled: false });
  // The issue's asks, which say which answered question is a person's decision
  // (`decisionNodes`), watched the same way: the page's tree reads them under this key.
  const asksRead = useQuery({
    enabled: false,
    queryFn: () => api.listIssueAsks(issue.key, "all"),
    queryKey: ["asks", issue.key],
  });
  const asks = asksRead.data;
  // The decisions view the page holds, and the tree read wherever the view holds no answer.
  const held = root.views?.decisions ?? tree;
  const decisions = useMemo(
    () => (held === undefined ? undefined : decisionNodes(held, { asks, issue })),
    [asks, held, issue]
  );
  const changes = useMemo(() => changesOf(tree, decisions ?? []), [decisions, tree]);
  // How this issue addresses a node, which is the one rule the tree was built with, so a
  // `shaped` tie resolves to the node it names (`tree-drafts.ts`). `nodeIdOf` names no block by
  // a line, so the line rule it is handed is the reading's words, as the parts' tests hand it.
  const nodeIdOf = useMemo(() => treeContext({ issue }, wordsOf).nodeIdOf, [issue]);
  const parts = useMemo(() => {
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
      others: [],
      yours:
        model.others.length === 0
          ? model.yours
          : newestFirst([...model.yours, ...model.others], root),
    };
  }, [asks, changes, decisions, issue.key, nodeIdOf, reader, root, view]);
  // The rows are the tree read's (see above): the view knows what the issue holds once that read
  // has answered, and a failed decisions view read counts only while neither read holds an answer.
  const known = tree !== undefined && asks !== undefined;
  // Failed with no answer held (see above), as the Spec view judges the same reads
  // (`SpecFilterBody.tsx`'s `failedWithNothing`).
  const treeFailed = treeRead.isError && tree === undefined;
  const viewFailed = viewRead.isError && viewRead.data === undefined;
  const failed = treeFailed || (held === undefined && viewFailed);
  const { refetch: readTree } = treeRead;
  const { refetch: readView } = viewRead;
  const retry = useCallback(() => {
    if (treeFailed) void readTree();
    if (viewFailed) void readView();
  }, [readTree, readView, treeFailed, viewFailed]);
  const asksFailed = asksRead.isError && asks === undefined;
  const { refetch: readAsks } = asksRead;
  const asksRetry = useCallback(() => {
    void readAsks();
  }, [readAsks]);
  return useMemo(
    () => ({ ...parts, asksFailed, asksRetry, failed, known, retry }),
    [asksFailed, asksRetry, failed, known, parts, retry]
  );
}

/**
 * The Change each of `decisions` waits on, by the decision's own address (`Decision.ref`), as
 * the parts read one (`decisionsParts`' `changes`): the comment the tree read hangs under the
 * decision's node that `decisionNodeChanges` gives it, with its words, its author and the time
 * it was made. The decisions are the decisions view's, or the tree read's, and the Changes the
 * tree read's, which answer one node by the same id. None while the tree read has not answered.
 */
export function changesOf(
  tree: TreeAnswer | undefined,
  decisions: readonly DecisionNode[]
): ReadonlyMap<string, ChangeComment> {
  const changes = new Map<string, ChangeComment>();
  if (tree === undefined) return changes;
  const asked = decisionNodeChanges(tree);
  for (const decision of decisions) {
    const comment = asked.get(decision.node.id);
    // A Change is a person's comment (`decisionNodeChanges`), so it always has an author: the
    // first the read lists for it.
    const author = comment?.author[0];
    if (comment === undefined || author === undefined) continue;
    changes.set(decision.ref, {
      author,
      body: comment.text,
      created_at: comment.made_at,
      id: comment.id,
    });
  }
  return changes;
}

/**
 * `entries` with the newest first, in the order G's model keeps each part
 * (`decisions-parts.ts`'s `newestFirst`), so the reader's decisions and another person's read as
 * one part: an entry the model holds no time for stands last, and two entries of one moment
 * stand in the order their nodes first stand in the page's reading order (`nodesInOrder`),
 * which is the order the outline draws them in. The same reads always draw the same list.
 */
function newestFirst(
  entries: readonly DecisionsEntry[],
  root: TreeNode
): readonly DecisionsEntry[] {
  // Each node's place in the reading order; a node drawn under two parents is one node, so its
  // first placement is its place.
  const place = new Map<string, number>();
  for (const node of nodesInOrder(root)) if (!place.has(node.id)) place.set(node.id, place.size);
  return [...entries].sort((one, other) => {
    const left = one.at;
    const right = other.at;
    if (left !== undefined && right !== undefined) {
      const newer = compareTimestamps(right, left);
      if (newer !== 0) return newer;
    } else if (left !== right) {
      return left === undefined ? 1 : -1;
    }
    // Every entry's node is a node of `root`, which the model read its entries from.
    return (place.get(one.node.id) ?? place.size) - (place.get(other.node.id) ?? place.size);
  });
}
