import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { roleDocument } from "../../../api/issue-cache";
import { artifactBlocksQuery } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { EVENT_FRESH, standsFailed } from "../../refs/reference-target";
import { wordsOf } from "../../refs/tree-fragment";
import { decisionChanges } from "../decision-review";
import type { DocumentBlocks, TreeNode } from "../issue-tree";
import { treeContext } from "../tree-drafts";
import { nodesInOrder } from "../tree-keys";
import { issueAsksQuery, issueCommentsQuery } from "../useIssueTree";
import {
  askIdOf,
  type DecisionsEntry,
  type DecisionsParts,
  decisionsParts,
  isReading,
  placements,
} from "./decisions-parts";

/**
 * The one seam between today's reads and the Decisions view: it turns the decisions read, the
 * issue's asks, its comments and the decisions document's blocks into the view's parts, so no
 * other file of the view names a read. Once the server's decisions model gives the view its
 * entries, that model's read replaces this module and leaves the view as it is.
 *
 * The reads are the ones the page's tree is built from: the three reads the tree reads, under
 * the keys `useIssueTree.ts` reads them by, the asks by the tree's own definition of that read
 * (`issueAsksQuery`), and the decisions document's blocks under the tree's own key for them
 * (`artifactBlocksQuery`, for the document `roleDocument` names), so the view adds no request of
 * its own. Each takes the tree's freshness too (`EVENT_FRESH`), whose own doc gives the reason.
 * The blocks are why a Change stands under the decision whose section holds its anchor
 * (`decision-review.ts`): the quote alone cannot say which, since two decisions can hold the
 * same words. Until the blocks answer, a Change stands under the first decision whose words hold
 * its quote. A read that holds no decisions, or that has not answered yet, gives no decision and
 * no error: the view draws the questions alone.
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
 * The seam says too which decisions are a person's write-up of an answered question
 * (`writeUps`), which the model draws no entry for (`isReading`), and which questions each one
 * restates: an address naming a write-up lands on the row of a question it answers, and only
 * the decisions read and the asks can tell a write-up from a person's decision of its own.
 *
 * The hook is the module's one export and its answer's type is its own, so the read that
 * replaces this module has one name to give the view and nothing else of it to keep.
 */

/** The two reads that say which entries there are, in the order the body says which of them
 *  stands refused. */
type DecisionsRead = "decisions" | "asks";

const NO_READS_OUT: readonly DecisionsRead[] = [];

/** The seam's answer: the view's parts, and beside them whether the reads that say which
 *  entries there are hold their answers, which of them stands refused, which runs now and how
 *  many times each has failed, and how to ask one again. */
interface DecisionsEntries {
  /** How many times each of the decisions read and the asks has failed, by its word (TanStack
   *  Query's `errorUpdateCount`). It keys the alert of a refused read's line (`FailedReadLine`'s
   *  `failure`), so each new failure is said again, and a run that ends with none, cancelled or
   *  paused while the browser is offline, says nothing again. */
  readonly failures: Readonly<Record<DecisionsRead, number>>;
  /** The decisions and asks reads that have not answered or stood refused, in that order. */
  readonly out: readonly DecisionsRead[];
  /** Which entry stands in which part, each part in the order it is drawn. Another person's
   *  decisions stand in `yours`, so `others` is empty. */
  readonly parts: DecisionsParts;
  /** Whether the decisions read and the asks each hold an answer: the reads that say which
   *  questions and decisions there are. The comments and the decisions document's blocks only
   *  move a decision between the review list and the reader's own, so they are not waited for.
   *  A read that failed holds none. */
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
  /** Each person's write-up of an answered question of this issue (`isReading`), by its node's
   *  id, with the node ids of the answered questions it restates. The list draws no row for a
   *  write-up, so an address naming one lands on the row of a question it answers. Undefined
   *  while the asks read holds no answer and is not refused: until it answers, an answer it
   *  brings may make any person's decision a write-up. */
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
  const asksRead = useQuery({ ...issueAsksQuery(issue.key), ...EVENT_FRESH });
  const asks = asksRead.data;
  // The issue's comments, which hold the changes a person asked of a decision
  // (`decision-review.ts`), under the key the tree reads them by (`issueCommentsQuery`).
  const comments = useQuery({ ...issueCommentsQuery(issue.key), ...EVENT_FRESH }).data;
  // The blocks of the decisions document, which name the section each Change's anchor stands
  // in and so the decision it changes, under the key the tree reads them by. An issue with no
  // decisions document reads none.
  const documentId = roleDocument(issue, "decisions")?.id;
  const blocks = useQuery({
    ...artifactBlocksQuery(documentId ?? ""),
    ...EVENT_FRESH,
    enabled: documentId !== undefined,
  }).data;
  // The comments and the blocks only move a decision between parts, so they are not waited for.
  const readsHeld = decisions !== undefined && asks !== undefined;
  // A read stands refused by the page's one rule (`standsFailed`): it holds no answer, and a
  // fetch of it failed since it last held one. Not by its status: a refetch that fails over the
  // answer the read last gave sets the error and keeps that answer, which the view draws from,
  // and a read that holds no answer reads pending again, its error cleared, the moment it runs
  // again, though it stays refused until it answers.
  const decisionsRefused = standsFailed(decisionsRead);
  const asksRefused = standsFailed(asksRead);
  const waitingForAsks = asks === undefined && !asksRefused;
  let out = NO_READS_OUT;
  if (decisions === undefined && !decisionsRefused) {
    out = waitingForAsks ? ["decisions", "asks"] : ["decisions"];
  } else if (waitingForAsks) {
    out = ["asks"];
  }
  const unreadable = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsRefused ? (["decisions"] as const) : []),
      ...(asksRefused ? (["asks"] as const) : []),
    ],
    [asksRefused, decisionsRefused]
  );
  const decisionsFetching = decisionsRead.isFetching;
  const asksFetching = asksRead.isFetching;
  const retrying = useMemo<readonly DecisionsRead[]>(
    () => [
      ...(decisionsFetching ? (["decisions"] as const) : []),
      ...(asksFetching ? (["asks"] as const) : []),
    ],
    [asksFetching, decisionsFetching]
  );
  const decisionsFailures = decisionsRead.errorUpdateCount;
  const asksFailures = asksRead.errorUpdateCount;
  const failures = useMemo<Readonly<Record<DecisionsRead, number>>>(
    () => ({ asks: asksFailures, decisions: decisionsFailures }),
    [asksFailures, decisionsFailures]
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
  const documents = useMemo<readonly DocumentBlocks[] | undefined>(
    () =>
      documentId === undefined || blocks === undefined
        ? undefined
        : [{ artifactId: documentId, blocks }],
    [documentId, blocks]
  );
  const changes = useMemo(
    () => decisionChanges(decisions, comments, issue.artifacts, documents),
    [decisions, comments, issue.artifacts, documents]
  );
  // How this issue addresses a node, which is the one rule the tree was built with, so a
  // `shaped` tie resolves to the node it names (`tree-drafts.ts`). `nodeIdOf` names no block by
  // a line, so the line rule it is handed is the reading's words, as G's tests hand it.
  const nodeIdOf = useMemo(() => treeContext({ issue }, wordsOf).nodeIdOf, [issue]);
  // From the model's inputs alone: how a read stands is no input of the model, so while only
  // that changes the parts keep their identity and nothing the body works out from them is
  // worked out again.
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
      others: [],
      yours:
        model.others.length === 0
          ? model.yours
          : newestFirst([...model.yours, ...model.others], root),
    };
  }, [asks, changes, decisions, issue.key, nodeIdOf, reader, root, view]);
  // The write-ups, read as the model reads them (`decisionsParts`): an answered question is an
  // ask the tree holds with an answer on it, named by the ask's own id, which is how a decision
  // names the question it restates, and a decision's row is the one its node's address names.
  // A node drawn under two parents is one node, read at its first placement in the reading
  // order, as the model reads it (`placements`).
  const writeUps = useMemo(() => {
    if (asks === undefined && !asksRefused) return undefined;
    const questions = new Map<string, string>();
    const decided: TreeNode[] = [];
    for (const { node } of placements(root).values()) {
      const id = askIdOf(node);
      if (id !== undefined && node.answer !== undefined) questions.set(id, node.id);
      if (node.type === "decision") decided.push(node);
    }
    const answered = new Set(questions.keys());
    const rows = new Map((decisions?.decisions ?? []).map((row) => [row.ref, row]));
    const found = new Map<string, ReadonlySet<string>>();
    for (const node of decided) {
      const row = rows.get(node.reference ?? "");
      if (row === undefined || !isReading(row, answered)) continue;
      const restated = (row.asks ?? []).flatMap((ask) => questions.get(ask) ?? []);
      found.set(node.id, new Set(restated));
    }
    return found;
  }, [asks, asksRefused, decisions, root]);
  return { failures, out, parts, readsHeld, retry, retrying, unreadable, writeUps };
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
