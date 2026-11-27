import { useQueries, useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { artifactBlocksQuery, issueAskThreadsQuery } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { EVENT_FRESH, standsFailed } from "../../refs/reference-target";
import {
  changeDocumentIds,
  decisionChangeComments,
  decisionChanges,
  treeComments,
} from "../decision-review";
import type { DocumentBlocks, TreeNode } from "../issue-tree";
import { changeUnread, treeContext } from "../tree-drafts";
import { nodesInOrder } from "../tree-keys";
import { useReadUnloadedPages, useTreeRead } from "../tree-read/tree-read";
import {
  askIdOf,
  type DecisionsEntry,
  type DecisionsParts,
  decisionsParts,
  placements,
} from "./decisions-parts";

/**
 * The one seam between today's reads and the Decisions view: it turns the decisions read's
 * entries, the issue's asks, its comments and the blocks of the documents its decisions stand in
 * into the view's parts, so no other file of the view names a read. The decisions read answers
 * each decision as an entry of its node (`entries`): an agent's decision a `decision` block of
 * the spec or of another of the issue's documents, a person's the answered ask itself, which the
 * view reads as the page's tree reads them (`tree-read/tree-nodes.ts`).
 *
 * The reads are the ones the page's tree is built from: the two reads the tree reads, under
 * the keys `useIssueTree.ts` reads them by, the asks by the tree's own definition of that read
 * (`issueAskThreadsQuery`), and the tree read's answer, whose comments are the Changes
 * (`treeComments`): a decision node takes the Changes the read hangs under it, as the outline
 * does. The view reads one more of its own, which the tree does not make: the blocks of the
 * spec, the plan and the Verification document (`primarySpec`, `roleDocument`) that a decision a
 * Change can be asked of stands in (`changeDocumentIds`), under the page's key for them
 * (`artifactBlocksQuery`), one read for each such document, sent as the view first mounts. Each
 * takes the tree's freshness too (`EVENT_FRESH`),
 * whose own doc gives the reason. The blocks are why a Change stands under the decision whose
 * block holds its anchor (`decision-review.ts`): the quote alone cannot say which, since two
 * decisions can hold the same words. Until the blocks answer, a Change stands under the first
 * decision of its document whose words hold its quote. A read that holds no decisions, or that
 * has not answered yet, gives no decision and no error: the view draws the questions alone.
 *
 * A decision the outline marks as one whose Change may stand on a page the tree read has not
 * loaded (`tree-drafts.ts`' `changeUnread`, its state's "+") stands in the review list drawn
 * with that mark, and the list's count leaves it out until that page is read
 * (`decisions-parts.ts`' `reviewWaiting`): the seam reads the next pages and then the older
 * comment pages once the page has drawn, one at a time, until each such Change is held or no such
 * page is left (`useReadUnloadedPages`, decisions 563 (4) and 574 (B)), as the header's count
 * does, and the view draws the decision by what it finds.
 *
 * The view draws four parts. A decision another person made is no part of its own: it stands
 * under the reader's decisions, with that person's name on its row, so the model's `others` is
 * put in `yours`, newest first, and `others` is left empty.
 *
 * The seam says too, beside the parts, whether the reads that say which questions and decisions
 * there are, the decisions read and the asks, hold their answers (`readsHeld`), which the
 * outline's keys (`useTreeKeymap.ts`) and the body's empty line read. Until they do, a view with
 * no question or no decision to go to may be one whose questions and decisions have not arrived,
 * so `q` and `w` with nowhere to go say nothing rather than that there is none, and the body
 * draws no line that the issue holds nothing, as the outline does (`slots/body.tsx`). The
 * comments and the blocks are not waited for: they say of no entry whether it is there, only
 * whether a decision stands in the review list or in the reader's own, so a failed read of
 * either still lets an issue that holds nothing say so.
 *
 * The seam says too which of those two reads stands refused (`unreadable`), which of them is
 * being read now (`retrying`) and how many times each has failed (`failures`), and asks one of
 * them alone again (`retry`). A read stands refused by the page's one rule for a failed read
 * (`standsFailed`): it holds no answer, and a fetch of it failed since it last held one. Its
 * status is not the rule: TanStack Query sets a read that holds no answer back to pending, its
 * error cleared, the moment it runs again, and the read stays refused until it answers. The body
 * draws a line with a Retry for each refused read, in place of the entries that read would give;
 * while the read runs again the line stays, with the focus its Retry holds, the Retry reads busy,
 * and a Retry pressed then asks nothing more. The line says the read's failure again only for a
 * new one: a run that ends with none, cancelled or paused while the browser is offline, leaves
 * the count as it was, and with it the line's alert. How a read stands and how it is asked again
 * belong to the read, and only this module names one, so they are read here and the body knows
 * a read only by the word its line is drawn for. A read whose refetch fails over an answer it
 * already holds keeps that answer, which the view still draws from, so it is not among the
 * refused; nor are the comments and the blocks, which only move a decision between parts.
 *
 * The seam says too which nodes are a write-up of an answered question (`writeUps`) and which
 * question each one restates: a `reading` node the tree places under an answered ask
 * (`tree-read/tree-nodes.ts`'s `draftReading`), which restates the person's answer, and which
 * the view draws no row for (R9), so an address naming one lands on the row of the question it
 * answers. The asks read says which ask is answered and the tree read where each reading
 * stands, so only with both can the seam tell.
 *
 * The hook is the module's one export and its answer's type is its own, so the read that
 * replaces this module has one name to give the view and nothing else of it to keep.
 */

/** The two reads that say which entries there are, in the order the body says which of them
 *  stands refused, each by the key the outline draws its line under (`outline-reads.ts`). */
type DecisionsRead = "issue-decisions" | "asks";

/** The seam's answer: the view's parts, and beside them whether the reads that say which
 *  entries there are hold their answers, which of them stands refused, which runs now and how
 *  many times each has failed, and how to ask one again. */
interface DecisionsEntries {
  /** How many times each of the decisions read and the asks has failed, by its word (TanStack
   *  Query's `errorUpdateCount`). It keys the alert of a refused read's line (`FailedReadLine`'s
   *  `failure`), so each new failure is said again, and a run that ends with none, cancelled or
   *  paused while the browser is offline, says nothing again. */
  readonly failures: Readonly<Record<DecisionsRead, number>>;
  /** Which entry stands in which part, each part in the order it is drawn. Another person's
   *  decisions stand in `yours`, so `others` is empty. */
  readonly parts: DecisionsParts;
  /** Whether the decisions read and the asks each hold an answer: the reads that say which
   *  questions and decisions there are. The comments and the blocks of the decisions' documents
   *  only move a decision between the review list and the reader's own, so they are not waited
   *  for. A read that failed holds none. */
  readonly readsHeld: boolean;
  /** Asks `read` alone again, by its own query's refetch. */
  readonly retry: (read: DecisionsRead) => void;
  /** Each of the decisions read and the asks, in that order, that is being read now, a first
   *  read, a Retry's or any other (TanStack Query's `isFetching`). A refused read stays refused
   *  while it runs again, so its line stays drawn, its Retry busy. */
  readonly retrying: readonly DecisionsRead[];
  /** Each of the decisions read and the asks, in that order, that stands refused
   *  (`standsFailed`): it holds no answer, and a fetch of it failed since it last held one. A read
   *  stays here while it runs again, until it answers. */
  readonly unreadable: readonly DecisionsRead[];
  /** Each write-up of an answered question of this issue - a `reading` node the tree places
   *  under an answered ask - by its node's id, with the node id of the question it restates. The
   *  list draws no row for a write-up, so an address naming one lands on the row of the question
   *  it answers. Undefined while the asks read or the decisions read holds no answer and is not
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
  /** The whole tree, which holds each ask and each decision and each statement a tie names. */
  root: TreeNode;
  /** The tree as the Decisions view leaves it. */
  view: TreeNode;
}): DecisionsEntries {
  const decisionsRead = useQuery({ ...issueDecisionsQuery(issue.key, true), ...EVENT_FRESH });
  const decisions = decisionsRead.data;
  const asksRead = useQuery({ ...issueAskThreadsQuery(issue.key), ...EVENT_FRESH });
  const asks = asksRead.data;
  // The comments of the tree read's answer, which hold the changes a person asked of a decision
  // (`decision-review.ts`): a decision node's are the ones the read hangs under it, as the
  // outline reads them.
  const tree = useTreeRead(issue).answer;
  // The blocks of the spec, the plan and the Verification document, in that order and each once,
  // that a decision's block stands in, which name the decision whose block holds each Change's
  // anchor and so the decision it changes, under the key the page reads them by
  // (`artifactBlocksQuery`), though the tree reads none of them. No other document is read here,
  // a `decisions.md` an older release stored among them: a decision of one, which the tree holds
  // no node for, finds its Change by its words (R3). `combine` keeps one value for them, replaced
  // only when a document's own read changes.
  const decided = changeDocumentIds(issue, decisions);
  const documentIds = [
    ...new Set(
      [primarySpec(issue), roleDocument(issue, "plan"), roleDocument(issue, "verification")]
        .flatMap((document) => document?.id ?? [])
        .filter((id) => decided.has(id))
    ),
  ];
  const documents = useQueries({
    combine: (answers): DocumentBlocks[] =>
      answers.flatMap((answer, index) => {
        const artifactId = documentIds[index];
        return artifactId === undefined || answer.data === undefined
          ? []
          : [{ artifactId, blocks: answer.data }];
      }),
    queries: documentIds.map((id) => ({ ...artifactBlocksQuery(id), ...EVENT_FRESH })),
  });
  // The comments and the blocks only move a decision between parts, so they are not waited for.
  const readsHeld = decisions !== undefined && asks !== undefined;
  // A read stands refused by the page's one rule (`standsFailed`): it holds no answer, and a
  // fetch of it failed since it last held one. Not by its status: a refetch that fails over the
  // answer the read last gave sets the error and keeps that answer, which the view draws from,
  // and a read that holds no answer reads pending again, its error cleared, the moment it runs
  // again, though it stays refused until it answers.
  const decisionsRefused = standsFailed(decisionsRead);
  const asksRefused = standsFailed(asksRead);
  const unreadable = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsRefused ? (["issue-decisions"] as const) : []),
      ...(asksRefused ? (["asks"] as const) : []),
    ],
    [asksRefused, decisionsRefused]
  );
  const decisionsFetching = decisionsRead.isFetching;
  const asksFetching = asksRead.isFetching;
  const retrying = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsFetching ? (["issue-decisions"] as const) : []),
      ...(asksFetching ? (["asks"] as const) : []),
    ],
    [asksFetching, decisionsFetching]
  );
  const decisionsFailures = decisionsRead.errorUpdateCount;
  const asksFailures = asksRead.errorUpdateCount;
  const failures = useMemo<Readonly<Record<DecisionsRead, number>>>(
    () => ({ asks: asksFailures, "issue-decisions": decisionsFailures }),
    [asksFailures, decisionsFailures]
  );
  // Each read is asked again by its own query, so a Retry asks for that read and no other.
  const refetchDecisions = decisionsRead.refetch;
  const refetchAsks = asksRead.refetch;
  const retry = useCallback(
    (read: DecisionsRead) => {
      void (read === "issue-decisions" ? refetchDecisions() : refetchAsks());
    },
    [refetchAsks, refetchDecisions]
  );
  const changes = useMemo(() => {
    const comments = treeComments(tree);
    const placed = decisionChangeComments(decisions, comments, issue, documents, tree);
    return decisionChanges(decisions, comments, issue, documents, placed);
  }, [decisions, documents, issue, tree]);
  // Whether a decision carries the outline's mark of a Change on a page not read yet
  // (`changeUnread`): while one does, those pages are read for it.
  const unread = useMemo(
    () => (decisions?.entries ?? []).some((row) => changeUnread(row, changes.has(row.ref), tree)),
    [changes, decisions, tree]
  );
  useReadUnloadedPages(issue, () => unread);
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
      others: [],
      yours:
        model.others.length === 0
          ? model.yours
          : newestFirst([...model.yours, ...model.others], root),
    };
  }, [asks, changes, decisions, issue.key, nodeIdOf, reader, restating, root, view]);
  // Until the asks read and the decisions read have answered, or stand refused, the tree may not
  // yet place a reading under the answered ask it reads, so no write-up is told yet.
  const writeUps =
    (asks === undefined && !asksRefused) || (decisions === undefined && !decisionsRefused)
      ? undefined
      : restating;
  return { failures, parts, readsHeld, retry, retrying, unreadable, writeUps };
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
