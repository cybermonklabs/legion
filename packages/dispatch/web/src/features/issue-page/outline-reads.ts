import { useQueries } from "@tanstack/react-query";

import { api } from "../../api/client";
import { issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { refetchFailedOnFocus } from "../refs/reference-target";
import type { IssueTreeReads, TreeNode } from "./issue-tree";
import { issueAsksQuery, useIssueTree } from "./useIssueTree";

/** What each read the outline waits on takes: a copy of the tree's own rule for its reads,
 *  `useIssueTree.ts`'s `EVENT_FRESH`, which this file is to import once that file exports it.
 *  Events keep a read fresh and its age never does, so the outline's slot mounted again on a
 *  press of a view more than 30 s after the load reads none of them again, and window focus
 *  fetches again only a read in error. */
const EVENT_FRESH = {
  refetchOnWindowFocus: refetchFailedOnFocus,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** What the outline's reads give (`useOutlineReads`). Each field is one value for as long as the
 *  reads say the same; the object holding them is new on every render. */
export interface OutlineReads {
  /** The reads of the issue's conversation the tree's turns come from, each undefined until it
   *  has answered, and whether each of the six reads the tree's other nodes come from - the
   *  coverage, the decisions, the results, the asks, the comments and the messages - holds its
   *  answer (`held`). */
  readonly reads: {
    readonly asks: IssueTreeReads["asks"];
    readonly comments: IssueTreeReads["comments"];
    readonly held: boolean;
    readonly messages: IssueTreeReads["messages"];
  };
  /** Whether every read the tree's nodes come from holds its answer: `reads.held`, and every
   *  document the tree reads holding its blocks (`IssueTreeView.blocksHeld`). */
  readonly readsHeld: boolean;
  /** The whole tree (`useIssueTree`). */
  readonly root: TreeNode;
}

/**
 * The issue's tree and the reads the outline waits on, for whatever draws or lands on the
 * outline: one place for those reads and how they are kept fresh, so every caller reads each
 * of them under the one key and with the one freshness, and two callers on one page cost no
 * request more than one.
 *
 * Whether every read the tree's nodes come from holds its answer (`readsHeld`): the issue's
 * children come with the issue, the documents' blocks are `blocksHeld`, and every other node
 * comes from one of the six reads here, read under the keys `useIssueTree` reads them by, so
 * they cost no request of their own. A read that failed holds nothing either: the page cannot
 * say what it would have held. The conversation's reads are kept too, which tell a turn the
 * outline leaves out (`useAddressFallback`).
 */
export function useOutlineReads(issue: IssueDetails): OutlineReads {
  const { blocksHeld, root } = useIssueTree(issue);
  const key = issue.key;
  const reads = useQueries({
    combine: (answers): OutlineReads["reads"] => {
      const [, , , asks, comments, messages] = answers;
      return {
        asks: asks.data,
        comments: comments.data,
        held: answers.every((answer) => answer.data !== undefined),
        messages: messages.data,
      };
    },
    queries: [
      { ...issueCoverageQuery(key), ...EVENT_FRESH },
      { ...issueDecisionsQuery(key, true), ...EVENT_FRESH },
      { ...issueResultsQuery(key), ...EVENT_FRESH },
      { ...issueAsksQuery(key), ...EVENT_FRESH },
      { queryFn: () => api.listComments(key), queryKey: ["comments", key], ...EVENT_FRESH },
      {
        queryFn: () => api.listIssueMessages(key),
        queryKey: ["issue-messages", key],
        ...EVENT_FRESH,
      },
    ],
  });
  return { reads, readsHeld: reads.held && blocksHeld, root };
}
