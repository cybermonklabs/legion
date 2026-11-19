import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import type { GraphEdge, IssueDetails } from "../../../api/types";
import type { ExtraReferenceRow } from "../../refs/ReferencedBy";
import { buildRunPath } from "../../refs/routes";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { plainWords } from "../tree-drafts";
import { issueTreeQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";

/**
 * The command bar runs that changed the issue or a node of it, as rows of Details' Referenced by.
 * A run writes one `changed_by_run` tuple from each node it created or changed to itself, and the
 * tree read answers the run as a node of the `interaction` supertag with the asker's whole
 * question as its text. Only the asker's tree read answers a run at all, so another reader's tree
 * holds none and draws no row.
 */

/** The supertag the tree read answers a command bar run with. */
const RUN_SUPERTAG = "interaction";

/** The tuple kind from a node a run created or changed to that run. */
const CHANGED_BY_RUN: GraphEdge["kind"] = "changed_by_run";

const NO_ROWS: readonly ExtraReferenceRow[] = [];

/** When `run` was made, as a time that orders; a time that does not parse orders as the oldest. */
function madeAt(run: TreeAnswerNode): number {
  const time = Date.parse(run.made_at);
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

/** The row of one run: its kind and its edge in the page's words, its question read to plain
 *  words, and the run's own page. Only the key and the link hold the run's id. */
function runRow(run: TreeAnswerNode): ExtraReferenceRow {
  const excerpt = plainWords(run.text);
  return {
    at: run.made_at,
    edge: NODE_REFERENCES_WORDS.edges.changed_by_run,
    ...(excerpt.trim() === "" ? {} : { excerpt }),
    href: buildRunPath(run.id),
    key: run.id,
    kind: sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction),
  };
}

/** One row for each run of `tree` that changed a node of it, once however many nodes it changed,
 *  newest first. A run no `changed_by_run` tuple points at draws no row, and nor does a tree
 *  not read yet. */
export function runRows(tree: TreeAnswer | undefined): readonly ExtraReferenceRow[] {
  if (tree === undefined) return NO_ROWS;
  const changed = new Set<string>();
  for (const tuple of tree.tuples) {
    if (tuple.kind === CHANGED_BY_RUN) changed.add(tuple.to);
  }
  const runs: DatedRun[] = [];
  for (const node of tree.nodes) {
    // Taking the id out of the set as its node is taken keeps one run to one row.
    if (node.supertag === RUN_SUPERTAG && changed.delete(node.id)) {
      runs.push({ run: node, time: madeAt(node) });
    }
  }
  if (runs.length === 0) return NO_ROWS;
  return runs.sort(newestFirst).map(({ run }) => runRow(run));
}

/** The run rows of the issue's tree read, and how far that read has got. */
export interface RunRowsRead {
  /** The rows (`runRows`), the same value until the read's answer changes. */
  readonly rows: readonly ExtraReferenceRow[];
  /** Whether the read holds an answer or has failed: until then the rows are not yet known. */
  readonly settled: boolean;
  /** Whether the read failed. */
  readonly failed: boolean;
  /** Reads the tree again. */
  readonly retry: () => void;
  /** Whether a retry is out while the read has failed. */
  readonly retrying: boolean;
}

/**
 * The rows of the runs that changed `issue` or a node of it, from the tree read the outline made.
 * It observes that read without enabling it, so it sends no request of its own: the page's first
 * load and the reader opening Referenced by send none for runs, and only `retry` reads the tree
 * again. Only the asker's tree read holds a run, so another reader's rows are none.
 */
export function useRunRows(issue: IssueDetails): RunRowsRead {
  const read = useQuery({ ...issueTreeQuery(issue.key), enabled: false, select: runRows });
  const { data: rows = NO_ROWS, isError, isSuccess, refetch } = read;
  // `isFetching` is read only while the read has failed, the one time its Retry is drawn.
  const retrying = isError && read.isFetching;
  return useMemo(
    () => ({
      failed: isError,
      retry: () => {
        void refetch();
      },
      retrying,
      rows,
      settled: isSuccess || isError,
    }),
    [isError, isSuccess, refetch, retrying, rows]
  );
}
