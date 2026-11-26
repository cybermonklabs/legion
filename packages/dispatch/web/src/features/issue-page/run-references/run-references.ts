import { useQuery } from "@tanstack/react-query";
import {
  createElement,
  Fragment,
  type ReactNode,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { referenceTargetsQuery } from "../../../api/queries";
import type { GraphEdge, IssueDetails } from "../../../api/types";
import { isPartyRoute } from "../../refs/party-references";
import { useReferenceNames } from "../../refs/plain-text";
import type { ExtraReferenceRow } from "../../refs/ReferencedBy";
import {
  NOTHING_PUBLISHED,
  PublishedReferenceNames,
  type PublishedReferences,
  useKnownReferences,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import { EVENT_FRESH, standsFailed } from "../../refs/reference-target";
import {
  buildDispatchReference,
  buildRunPath,
  type DispatchReferenceRoute,
} from "../../refs/routes";
import { wordsOf } from "../../refs/tree-fragment";
import { retryRead } from "../details-error";
import { targetNaming } from "../issue-tree";
import { referenceStandIns } from "../one-line";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { withoutDirectiveLines } from "../RowText";
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
 *  it. `NamedRunRows` names the references only where the list draws them. */
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

/** How a run's one-line place names a reference it cites (`referenceStandIns`), which writes an
 *  issue as its key and asks this for every other reference. A target that is no party is named
 *  by the page's name for it once the one batch has named it (`useRunTargets`), and by the words
 *  its author wrote until then, and wherever the batch answered that nothing answers to the
 *  target now, that the reader may not see it, that it is ambiguous or that it has no words to
 *  name it by (decision 350); it is never read on its own. A party (a person, a role or an
 *  agent's session), which the batch does not ask for, is named by a read of its own
 *  (`useReferenceNames`, decision 340), and by the words its author wrote until that read answers
 *  and wherever it names nothing, as a row of the issue page names one (`row-references.ts`'s
 *  `rowReferenceWords`). No reference is drawn by its short form. */
type RunNameOf = (reference: string) => string | undefined;

/** The references a run's one-line places cite (`namedReferences`), each once, in the order the
 *  texts cite them. */
interface RunReferences {
  /** Each target the one batch asks a name for (`useRunTargets`), by the route `rowReferences`
   *  parsed it to: every reference but an issue and a party. */
  readonly batched: ReadonlyMap<string, DispatchReferenceRoute>;
  /** Each party, which the batch does not ask for and a read of its own names
   *  (`useReferenceNames`). */
  readonly parties: readonly string[];
  /** The words its author wrote for each reference of either, which it is drawn as until it has
   *  a name and wherever it has none. */
  readonly written: ReadonlyMap<string, string>;
}

/** The references that `texts` name by the page's name, as a one-line place reads them: the
 *  addresses `referenceStandIns` asks a name for, found as its `standIn` finds them, by
 *  `rowReferences` in each text without a typed block's source lines, with no bare key. That
 *  leaves out an issue, which is named by its key, and a reference on a typed block's own line,
 *  which no line draws. Each is sorted here, once, by the route that reader parsed it to: a party
 *  (`isPartyRoute`) to a read of its own, and every other target to the one batch.
 *
 *  The set holds every reference of the whole texts, so it holds more than a line draws where a
 *  caller cuts its line from a text, as the run's page cuts a changed node's first line
 *  (`RunPage.tsx`'s `namedFirstLine`). That cut is made on each name's own length, which only the
 *  batch's answer gives, so before that answer no cut can say which references the line draws: a
 *  reference that a line cut off while an earlier one stood as its author wrote it can come into
 *  the line once the earlier one is named by fewer characters. A set taken from that cut would
 *  leave such a reference to a second batch, where the page sends one (R4). A reference past the
 *  cut costs the batch one address more, and a party past it a read of its own. */
function namedReferences(texts: readonly string[]): RunReferences {
  const batched = new Map<string, DispatchReferenceRoute>();
  const parties: string[] = [];
  const written = new Map<string, string>();
  for (const text of texts) {
    for (const one of rowReferences(withoutDirectiveLines(text), [])) {
      // One reference written in two forms - a `dispatch://` address and a dashboard address of
      // the same target, say - is drawn by the first form the texts hold, in their order, since
      // one `RunNameOf` names it in every line they draw.
      if (one.route.kind === "issue" || written.has(one.reference)) continue;
      written.set(one.reference, one.written);
      if (isPartyRoute(one.route)) parties.push(one.reference);
      else batched.set(one.reference, one.route);
    }
  }
  return { batched, parties, written };
}

/** The names of the `batched` targets that the page has not published or asked for, read in one
 *  batched read of reference targets (`referenceTargetsQuery`, decision 515) and published from
 *  its answer, so each line reads them from the store and mounts no read of its own. A reference
 *  this read asked for stays in its set while `batched` cites it, although its answer is now
 *  published: dropping it would drop the query that holds the answer. */
function useRunTargets(
  batched: ReadonlyMap<string, DispatchReferenceRoute>,
  issue: IssueDetails | undefined
): PublishedReferences {
  const known = useKnownReferences();
  // The batch is chosen once the first commit has laid out, so a publisher mounted in that same
  // commit has registered what it asks for. Until then nothing is asked for in the store.
  const [ready, setReady] = useState(false);
  useLayoutEffect(() => {
    if (batched.size > 0) setReady(true);
  }, [batched.size]);
  const held = useRef<ReadonlySet<string>>(NOTHING_PUBLISHED.asked);
  const asked = useMemo(() => {
    if (!ready) return NOTHING_PUBLISHED.asked;
    const next = new Set<string>();
    for (const [ref, route] of batched) {
      if (
        !held.current.has(ref) &&
        (known.asked.has(ref) ||
          known.names.has(ref) ||
          known.gone.has(ref) ||
          known.ambiguous.has(ref) ||
          known.unnamed.has(ref))
      ) {
        continue;
      }
      next.add(ref);
      // A whole spec is titled by its issue (`useIssueTree.ts`'s `treeReferences`).
      if (route.kind === "spec" && route.block === undefined && route.key !== issue?.key) {
        next.add(buildDispatchReference({ key: route.key, kind: "issue" }));
      }
    }
    const previous = held.current;
    return next.size === previous.size && [...next].every((ref) => previous.has(ref))
      ? previous
      : next;
  }, [batched, issue?.key, known, ready]);
  useLayoutEffect(() => {
    held.current = asked;
  }, [asked]);
  const query = useMemo(() => referenceTargetsQuery([...asked]), [asked]);
  const { data } = useQuery({ ...query, ...EVENT_FRESH });
  // The key is the set itself, so a set that changes - a run the tree read adds cites a new
  // comment, or another publisher stops naming a reference this set left to it - is a read under
  // a new key, which holds no answer while it is out, nor after it fails. Meanwhile the answer of
  // the set the read answered last stands for each reference the new set asks for, so a name
  // already drawn stays drawn. The new set keeps every reference of the set before that the lines
  // still cite (`held`), so its answer, once it comes, names each of those and stands alone. Only
  // references the set asks for are published, so a reference the lines cite again after dropping
  // it is asked for again, not left to a name this read published for an earlier set. The held
  // answer is read from the cache by an observer that never fetches, which also keeps it from
  // garbage collection. This is the pattern of `useIssueTree.ts`'s `useLastAnswered`, written
  // again here rather than shared, since this round admits no line of that file (R6, decision
  // 529). The ref is written during render only with the set the read answers in that render, so
  // it never stands over a newer one.
  const lastAnswered = useRef(asked);
  if (data !== undefined) lastAnswered.current = asked;
  const answeredSet = lastAnswered.current;
  const heldQuery = useMemo(() => referenceTargetsQuery([...answeredSet]), [answeredSet]);
  const { data: heldAnswer } = useQuery({ ...heldQuery, ...EVENT_FRESH, enabled: false });
  const targets = useMemo(
    () => data?.targets ?? heldAnswer?.targets.filter((target) => asked.has(target.ref)),
    [asked, data, heldAnswer]
  );
  const published = useMemo(
    () =>
      asked.size === 0
        ? NOTHING_PUBLISHED
        : {
            ...(targets === undefined ? NOTHING_PUBLISHED : targetNaming({ issue, targets })),
            asked,
          },
    [asked, issue, targets]
  );
  usePublishReferenceNames(asked.size === 0 ? undefined : published);
  return useMemo(
    () =>
      ready || batched.size === 0
        ? published
        : { ...NOTHING_PUBLISHED, asked: new Set(batched.keys()) },
    [batched, published, ready]
  );
}

/** Draws a run's one-line places (`draw`) with each reference they cite named as `RunNameOf`
 *  says: a target by the page's name for it, read in one batch (`useRunTargets`), a party by a
 *  read of its own, and each by the words its author wrote where it has no name. `issue` is the
 *  issue the page shows, whose own documents are named by the issue read; the run's page shows
 *  none. The batch's references are handed to the lines as asked for from the first render,
 *  before the store holds them, so no line mounts a read of its own while the batch is out. */
export function RunReferenceNames({
  draw,
  issue,
  texts,
}: {
  draw: (nameOf: RunNameOf) => ReactNode;
  issue?: IssueDetails;
  texts: readonly string[];
}): ReactNode {
  const references = useMemo(() => namedReferences(texts), [texts]);
  const published = useRunTargets(references.batched, issue);
  const props = { children: createElement(RunNameReader, { draw, references }), published };
  return createElement(PublishedReferenceNames, props);
}

function RunNameReader({
  draw,
  references: { batched, parties, written },
}: {
  draw: (nameOf: RunNameOf) => ReactNode;
  references: RunReferences;
}): ReactNode {
  // A target the batch asks for is named by the name the store projected from the batch's
  // answer, and is never read on its own: that read would be the read per target that decision
  // 515 removes. Only a party is read here, as every line reads one, since the batch does not ask
  // for it; the read answers it no name while it is out and where Dispatch knows no such party.
  const { names } = useKnownReferences();
  const { names: partyNames, reads } = useReferenceNames(parties, undefined);
  const nameOf = useMemo<RunNameOf>(
    () => (reference) => {
      const name = batched.has(reference) ? names.get(reference) : partyNames.get(reference)?.name;
      return name === undefined || name === "" ? written.get(reference) : name;
    },
    [batched, names, partyNames, written]
  );
  return createElement(Fragment, null, reads, draw(nameOf));
}

/** Draws Details' run rows (`draw`) with each question's references named, mounted only while
 *  Referenced by draws its list, so a shut list reads no name. Its count is taken on the rows
 *  unnamed (`useRunRows`), whose number the naming does not change. */
export function NamedRunRows({
  draw,
  issue,
  rows,
}: {
  draw: (rows: readonly ExtraReferenceRow[]) => ReactNode;
  issue: IssueDetails;
  rows: readonly ExtraReferenceRow[];
}): ReactNode {
  const texts = useMemo(
    () => rows.flatMap((row) => (row.excerpt === undefined ? [] : [row.excerpt])),
    [rows]
  );
  return createElement(RunReferenceNames, {
    draw: (nameOf) => createElement(RunExcerpts, { draw, nameOf, rows }),
    issue,
    texts,
  });
}

function RunExcerpts({
  draw,
  nameOf,
  rows,
}: {
  draw: (rows: readonly ExtraReferenceRow[]) => ReactNode;
  nameOf: RunNameOf;
  rows: readonly ExtraReferenceRow[];
}): ReactNode {
  // The rows are named anew whenever the names change, and `draw` reads them as it draws; a row
  // whose question no name changes is handed on as it came.
  const named = useMemo(
    () =>
      rows.map((row) => {
        if (row.excerpt === undefined) return row;
        const references = referenceStandIns(nameOf);
        const excerpt = references.named(references.standIn(row.excerpt));
        return excerpt === row.excerpt ? row : { ...row, excerpt };
      }),
    [nameOf, rows]
  );
  return draw(named);
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
  /** The unnamed rows (`runRows`), or none while the read holds no answer. */
  readonly rows: readonly ExtraReferenceRow[];
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
 * are none.
 */
export function useRunRows(issue: IssueDetails, open: boolean): RunRowsRead {
  const tree = issueTreeQuery(issue.key);
  const read = useQuery({ ...tree, enabled: false, select: answerRows });
  useQuery({ ...tree, subscribed: open });
  const { data: rows, errorUpdateCount, refetch } = read;
  const failed = standsFailed(read);
  // `isFetching` is read only while the read stands failed, the one time its Retry is drawn.
  const retrying = failed && read.isFetching;
  return useMemo(
    () => ({
      failed,
      failure: errorUpdateCount,
      retry: retryRead({ refetch }),
      retrying,
      rows: rows ?? NO_ROWS,
      settled: rows !== undefined || failed,
    }),
    [errorUpdateCount, failed, refetch, retrying, rows]
  );
}
