import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import type { GraphEdge, IssueDetails } from "../../../api/types";
import { useReferenceNames } from "../../refs/plain-text";
import type { ExtraReferenceRow } from "../../refs/ReferencedBy";
import { standsFailed } from "../../refs/reference-target";
import { buildRunPath } from "../../refs/routes";
import { wordsOf } from "../../refs/tree-fragment";
import { retryRead } from "../details-error";
import { drawnLine, firstLineOf } from "../issue-tree";
import { referenceStandIns } from "../one-line";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { rowReferences } from "../row-references";
import { storedReading } from "../stored-reading";
import { issueTreeQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";

/**
 * The command bar runs that changed the issue or a node of it, as rows of Details' Referenced by.
 * A run writes one `changed_by_run` tuple from each node it created or changed to itself, an end
 * of the store kind `interaction` named by the run's id. The tree read answers each run such a
 * tuple of a node it holds names as a context node of that kind and of the `interaction`
 * supertag, by the run's id, with the asker's whole question as its text (`issue_tree_read.go`'s
 * `readContext`). Only the asker may see a run, so another reader's tree holds none and draws no
 * row.
 */

/** The store kind a `changed_by_run` tuple names a command bar run by, and the supertag the tree
 *  read answers the run's node with. */
const RUN = "interaction";

/** The tuple kind from a node a run created or changed to that run. */
const CHANGED_BY_RUN: GraphEdge["kind"] = "changed_by_run";

const NO_ROWS: readonly ExtraReferenceRow[] = [];

/** When `run` was made, as a time that orders; a run with no time, or a time that does not parse,
 *  orders as the oldest. */
function madeAt(run: TreeAnswerNode): number {
  const time = run.made_at === null ? Number.NaN : Date.parse(run.made_at);
  return Number.isNaN(time) ? Number.NEGATIVE_INFINITY : time;
}

/** A run with when it was made (`madeAt`), which orders it. */
type DatedRun = { readonly run: TreeAnswerNode; readonly time: number };

/** Newest first, and runs made at the same time by id, byte by byte. */
function newestFirst(a: DatedRun, b: DatedRun): number {
  if (a.time !== b.time) return a.time < b.time ? 1 : -1;
  if (a.run.id === b.run.id) return 0;
  return a.run.id < b.run.id ? -1 : 1;
}

/** The row of one run: its kind and its edge in the page's words, its question as plain words,
 *  when it was made - no `at` where the read answers no time, so the row draws none - and the
 *  run's own page. Only the key and the link hold the run's id. The question's plain words are
 *  the words of the server's reading of it (`text_tree`), and of its stored text's reading where
 *  the read sent none (`storedReading`), on one line, with each bare address as its asker wrote
 *  it: `useRunRows` names each reference in them as a one-line place names one
 *  (`namedWords`). */
function runRow(run: TreeAnswerNode): ExtraReferenceRow {
  const excerpt = wordsOf(run.text_tree ?? storedReading(run.text, run))
    .replace(/\s+/gu, " ")
    .trim();
  return {
    ...(run.made_at === null ? {} : { at: run.made_at }),
    edge: NODE_REFERENCES_WORDS.edges.changed_by_run,
    ...(excerpt === "" ? {} : { excerpt }),
    href: buildRunPath(run.id),
    key: run.id,
    kind: sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction),
  };
}

/** How a run's words name a reference they cite, by its `dispatch://` reference: the page's name
 *  for it where the page knows one, and undefined where it does not. */
export type RunNameOf = (reference: string) => string | undefined;

/** The references of `words` a one-line place names by the page's name for each
 *  (`referenceStandIns`): every one it names but an issue, which it names by its key. These are
 *  the references the page is asked to name (`useRunNames`). */
export function namedReferences(words: string): string[] {
  return rowReferences(words, []).flatMap((one) =>
    one.route.kind === "issue" ? [] : [one.reference]
  );
}

/** The names the page knows for the references `texts` cite (`namedReferences`), by the one rule
 *  every plain line names a reference by (`refs/plain-text.tsx`'s `useReferenceNames`): what the
 *  page published from its batched read, and else the target's own read. `reads` draws nothing,
 *  and the caller mounts it where it draws the words. A run is on no issue, so no role is read by
 *  one. */
export function useRunNames(texts: readonly string[]): { nameOf: RunNameOf; reads: ReactNode } {
  const addresses = useMemo(() => [...new Set(texts.flatMap(namedReferences))], [texts]);
  const { names, reads } = useReferenceNames(addresses, undefined);
  const nameOf = useMemo<RunNameOf>(() => (reference) => names.get(reference)?.name, [names]);
  return { nameOf, reads };
}

/** `words`, a run's words on one line with each bare address as written, with each reference
 *  they cite named as a one-line place names one (`referenceStandIns`, decisions 140 and 491): an
 *  issue by its key, any other target by the page's name for it (`nameOf`), and by its short form
 *  where the page knows none (`shortForm`) - never as the address that was typed. */
export function namedWords(words: string, nameOf: RunNameOf): string {
  const references = referenceStandIns(nameOf);
  return references.named(references.standIn(words));
}

/** The first line of `words`, a node's words with each bare address as written
 *  (`lineWordsRaw`), cut by R1's first-line rule (`firstLineOf`, `drawnLine`) with each reference
 *  in it unnamed, and then named as `namedWords` names one, so no cut falls inside a name: the
 *  shape every one-line place of the page draws a node's line in (`IssueContents.tsx`). */
export function namedFirstLine(words: string, nameOf: RunNameOf): string {
  const references = referenceStandIns(nameOf);
  return references.named(drawnLine(firstLineOf(references.standIn(words))));
}

/** One row for each run whose node some page holds and that a `changed_by_run` tuple of some page
 *  names (`tuple.to`), once however many nodes it changed and however many pages name it, newest
 *  first. The pages are those of the tree read that the page holds. A page of older comments,
 *  read with `comments_before`, answers those comments and their tuples and no other node, so a
 *  run that changed only an older comment has its tuple there and its node in no page, and draws
 *  no row. A run no such tuple names draws no row, and nor does a read that holds no page. */
export function runRows(pages: readonly TreeAnswer[]): readonly ExtraReferenceRow[] {
  const changed = new Set<string>();
  for (const page of pages) {
    for (const tuple of page.tuples ?? []) {
      if (tuple.kind === CHANGED_BY_RUN && tuple.to.kind === RUN) changed.add(tuple.to.id);
    }
  }
  if (changed.size === 0) return NO_ROWS;
  const runs: DatedRun[] = [];
  for (const page of pages) {
    for (const node of page.nodes ?? []) {
      // Taking the id out of the set as its node is taken keeps one run to one row.
      if (node.supertag === RUN && changed.delete(node.id)) {
        runs.push({ run: node, time: madeAt(node) });
      }
    }
  }
  if (runs.length === 0) return NO_ROWS;
  return runs.sort(newestFirst).map(({ run }) => runRow(run));
}

/** The rows of the tree read's one answer, which holds every page its walk read (`readTree`):
 *  one function, so the rows keep their identity until the answer changes. */
function answerRows(answer: TreeAnswer): readonly ExtraReferenceRow[] {
  return runRows([answer]);
}

/** The run rows of the issue's tree read, and how that read stands, as Details says a read of
 *  its own failed (`DetailsErrorLine`). */
export interface RunRowsRead {
  /** The rows (`runRows`), each question's references named (`namedWords`): made anew only where
   *  the read's answer or the names the page knows change; none while the read holds no answer. */
  readonly rows: readonly ExtraReferenceRow[];
  /** The reads of the references the questions cite that the page has no name for, which draw
   *  nothing: the caller mounts them where it draws the rows (`useRunNames`). */
  readonly reads: ReactNode;
  /** Whether the rows are known: the read holds an answer, or stands failed. */
  readonly settled: boolean;
  /** Whether the read stands failed (`standsFailed`): it holds no answer, and a read of it has
   *  failed. It stays so while a read sent again is out. A refresh that fails over an answer the
   *  read holds is no such failure, and the rows that answer drew stay. */
  readonly failed: boolean;
  /** Which failure the read's line says (`DetailsErrorLine`'s `failure`): the read's
   *  `errorUpdateCount`. */
  readonly failure: number;
  /** Reads the tree again, joining a read that is out (`retryRead`). */
  readonly retry: () => void;
  /** Whether a read is out while the read stands failed: its Retry reads busy. */
  readonly retrying: boolean;
}

/**
 * The rows of the runs that changed `issue` or a node of it, from the issue's tree read with no
 * view, through two observers of that read. The first is never enabled: it draws the rows, says
 * whether they are known and whether the read stands failed, and its `retry` reads the tree
 * again. The second reads the tree: one observer kept for the life of the panel that calls this,
 * subscribed at each opening of the list (`open`) and unsubscribed at each close, as a view's
 * observer is (`ViewRead`, `view-read.tsx`). So the page's load sends no read for runs, and each
 * opening reads the tree as a new observer of it does (`TREE_FRESH`): the first sends the read
 * once where the page holds no answer, and none where it holds one that nothing marked stale, and
 * an opening sends a refused read only where something asked for it again since its refusal.
 * The second is an observer of its own rather than the first with its `enabled` turned on at
 * each opening, since TanStack reads a query that holds no answer whenever an observer's
 * `enabled` turns on, so a read Dispatch refused would be sent again at each opening. A write of
 * the issue that is out holds back a read that holds an answer, but not one that holds neither
 * an answer nor a refusal (`mayReadNow`): a first opening while a write is out sends the read,
 * and the tree's freshness asks for it again once the last write settles
 * (`subscribeTreeFreshness`). Only the asker's tree read holds a run, so another reader's rows
 * are none. Each question is a one-line place: each reference it cites is named by the page's
 * name for it where the page knows one, and else by its short form, never as the address its
 * asker typed (`namedWords`, the reading list's `referenced-by-run-question`).
 */
export function useRunRows(issue: IssueDetails, open: boolean): RunRowsRead {
  const tree = issueTreeQuery(issue.key);
  const read = useQuery({ ...tree, enabled: false, select: answerRows });
  useQuery({ ...tree, subscribed: open });
  const { data: rows, errorUpdateCount, refetch } = read;
  const questions = useMemo(
    () => (rows ?? NO_ROWS).flatMap((row) => (row.excerpt === undefined ? [] : [row.excerpt])),
    [rows]
  );
  const { nameOf, reads } = useRunNames(questions);
  const named = useMemo(
    () =>
      rows === undefined || rows.length === 0
        ? NO_ROWS
        : rows.map((row) =>
            row.excerpt === undefined ? row : { ...row, excerpt: namedWords(row.excerpt, nameOf) }
          ),
    [nameOf, rows]
  );
  const failed = standsFailed(read);
  // `isFetching` is read only while the read stands failed, the one time its Retry is drawn.
  const retrying = failed && read.isFetching;
  return useMemo(
    () => ({
      failed,
      failure: errorUpdateCount,
      reads,
      retry: retryRead({ refetch }),
      retrying,
      rows: named,
      settled: rows !== undefined || failed,
    }),
    [errorUpdateCount, failed, named, reads, refetch, retrying, rows]
  );
}
