import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { api } from "../../../api/client";
import type { IssueDetails } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { wordsOf } from "../../refs/tree-fragment";
import { decisionChanges } from "../decision-review";
import type { TreeNode } from "../issue-tree";
import { treeContext } from "../tree-drafts";
import { nodesInOrder } from "../tree-keys";
import { type DecisionsEntry, type DecisionsParts, decisionsParts } from "./decisions-parts";

/**
 * The one seam between today's reads and the Decisions view: it turns the decisions read, the
 * issue's asks and its comments into the view's parts, so no other file of the view names a
 * read. Once the server's decisions model gives the view its entries, that model's read replaces
 * this module and leaves the view as it is.
 *
 * The reads are the three the page's tree reads, under the keys `useIssueTree.ts` reads them by,
 * so the view adds no request of its own. No document plays the decisions part, since decisions
 * are nodes of the spec, so no blocks of one are read: a Change stands under the first decision
 * whose words hold its quote (`decision-review.ts`). A read that holds no decisions, or that has
 * not answered yet, gives no decision and no error: the view draws the questions alone.
 *
 * The view draws four parts. A decision another person made is no part of its own: it stands
 * under the reader's decisions, with that person's name on its row, so the model's `others` is
 * put in `yours`, newest first, and `others` is left empty.
 *
 * The seam says too whether the reads that say which questions and decisions there are, the
 * decisions read and the asks, hold their answers (`readsHeld`), which the outline's keys
 * (`useTreeKeymap.ts`) and the body's empty line read. Until they do, a view with no question
 * or no decision to go to may be one whose questions and decisions have not arrived, so `q` and
 * `w` with nowhere to go say nothing rather than that there is none, and the body draws no line
 * that the issue holds nothing, as the outline does (`slots/body.tsx`). The comments are not
 * waited for: they say of no entry whether it is there, only whether a decision stands in the
 * review list or in the reader's own, so a failed read of them still lets an issue that holds
 * nothing say so.
 *
 * The seam says too which of those two reads was refused before it first answered
 * (`unreadable`), and asks one of them alone again (`retry`). The body draws a line with a
 * Retry for each, in place of the entries that read would give. How a read stands and how it
 * is asked again belong to the read, and only this module names one, so they are read here and
 * the body knows a read only by the word its line is drawn for. A read whose refetch fails over
 * an answer it already holds keeps that answer, which the view still draws from, so it is not
 * among them; nor are the comments, which only move a decision between parts.
 *
 * The hook is the module's one export and its answer's type is its own, so the read that
 * replaces this module has one name to give the view and nothing else of it to keep.
 */

/** The two reads that say which entries there are, in the order the body says which of them
 *  was refused. */
type DecisionsRead = "decisions" | "asks";

/** The Decisions view's parts, whether the reads that say which entries there are hold their
 *  answers, and which of them was refused and how to ask it again. */
interface DecisionsEntries extends DecisionsParts {
  /** Whether the decisions read and the asks each hold an answer: the reads that say which
   *  questions and decisions there are. The comments only move a decision between the review
   *  list and the reader's own, so they are not waited for. A read that failed holds none. */
  readonly readsHeld: boolean;
  /** Asks `read` alone again, by its own query's refetch. */
  readonly retry: (read: DecisionsRead) => void;
  /** Each of the decisions read and the asks, in that order, that was refused before it first
   *  answered: it holds no answer and is in error. */
  readonly unreadable: readonly DecisionsRead[];
}

/** The Decisions view's parts, from the issue, the reader's login, the whole tree and the tree
 *  as the Decisions view leaves it (`presetView`). */
export function useDecisionsEntries({
  issue,
  reader,
  root,
  view,
}: {
  issue: IssueDetails;
  /** Who is reading, which tells their own decisions from other people's. */
  reader: string | undefined;
  /** The whole tree, which holds each ask and each decision and each statement a tie names. */
  root: TreeNode;
  /** The tree as the Decisions view leaves it. */
  view: TreeNode;
}): DecisionsEntries {
  const decisionsRead = useQuery(issueDecisionsQuery(issue.key, true));
  const decisions = decisionsRead.data;
  const asksRead = useQuery({
    queryFn: () => api.listIssueAsks(issue.key, "all"),
    queryKey: ["asks", issue.key],
  });
  const asks = asksRead.data;
  // The issue's comments, which hold the changes a person asked of a decision
  // (`decision-review.ts`), under the key the tree reads them by.
  const comments = useQuery({
    queryFn: () => api.listComments(issue.key),
    queryKey: ["comments", issue.key],
  }).data;
  // The comments only move a decision between parts, so they are not waited for.
  const readsHeld = decisions !== undefined && asks !== undefined;
  // A read was refused before it first answered where it holds no answer and is in error. A
  // refetch that fails sets the error over the answer the read last gave and keeps that answer,
  // so a read is told by the data it holds and not by its status alone.
  const decisionsRefused = decisionsRead.isError && decisions === undefined;
  const asksRefused = asksRead.isError && asks === undefined;
  const unreadable = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsRefused ? (["decisions"] as const) : []),
      ...(asksRefused ? (["asks"] as const) : []),
    ],
    [asksRefused, decisionsRefused]
  );
  // Each read is asked again by its own query, so a Retry asks for that read and no other.
  const refetchDecisions = decisionsRead.refetch;
  const refetchAsks = asksRead.refetch;
  const retry = useCallback(
    (read: DecisionsRead) => {
      void (read === "decisions" ? refetchDecisions() : refetchAsks());
    },
    [refetchAsks, refetchDecisions]
  );
  const changes = useMemo(
    () => decisionChanges(decisions, comments, issue.artifacts, undefined),
    [decisions, comments, issue.artifacts]
  );
  // How this issue addresses a node, which is the one rule the tree was built with, so a
  // `shaped` tie resolves to the node it names (`tree-drafts.ts`). `nodeIdOf` names no block by
  // a line, so the line rule it is handed is the reading's words, as G's tests hand it.
  const nodeIdOf = useMemo(() => treeContext({ issue }, wordsOf).nodeIdOf, [issue]);
  return useMemo(() => {
    const parts = decisionsParts({
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
      ...parts,
      others: [],
      readsHeld,
      retry,
      unreadable,
      yours:
        parts.others.length === 0
          ? parts.yours
          : newestFirst([...parts.yours, ...parts.others], root),
    };
  }, [
    asks,
    changes,
    decisions,
    issue.key,
    nodeIdOf,
    reader,
    readsHeld,
    retry,
    root,
    unreadable,
    view,
  ]);
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
