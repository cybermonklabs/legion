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
import { type AgentText, useAgentText } from "../../refs/AgentName";
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
  parseNodeReference,
} from "../../refs/routes";
import { wordsOf } from "../../refs/tree-fragment";
import { retryRead } from "../details-error";
import { targetNaming } from "../issue-tree";
import { newPageIssue } from "../new-page-parameter";
import { type LineNames, referenceStandIns } from "../one-line";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { withoutDirectiveLines } from "../RowText";
import { citedReferences, knownIssueKeys, rowReferences } from "../row-references";
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

/** The query parameter that opens the new issue page, and the one value of it that does. They
 *  are twins of `new-page-parameter.tsx`'s `PARAMETER` and `NEW_PAGE`, which that file keeps to
 *  itself, and of `RunRoute.tsx`'s, which reads them. They are not shared: exporting them from
 *  `new-page-parameter.tsx` would change a line of that file, and R6 admits none this round; this
 *  file may not import `RunRoute.tsx`, which loads `RunPage.tsx`, an importer of this file; and
 *  `RunRoute.tsx` imports no word list, which this file does. `RunPage.tsx` reads them here. */
export const NEW_PAGE_PARAMETER = "issue_page";
export const NEW_PAGE = "new";

/** What a run's row adds to its link on an issue page that `?issue_page=new` opened, so the link
 *  opens the run's page for the same reader (`RunRoute.tsx`, decision 533). */
const NEW_PAGE_SEARCH = `?${NEW_PAGE_PARAMETER}=${NEW_PAGE}`;

/** The query parameter by which a run's row on an issue page that `?issue_page=new` opened names
 *  that issue's key in its link, after `NEW_PAGE_SEARCH`: the run's page cannot know from its own
 *  read which issue opened it, and keeps `?issue_page=new` on its links to that issue
 *  (`RunPage.tsx`, decision 533). */
export const OPENING_ISSUE = "issue";

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
 *  run's own page, by an address that carries `?issue_page=new` (`NEW_PAGE_SEARCH`) and the key
 *  of the issue `newPage` (`OPENING_ISSUE`) where `newPage` names one. Only the key and the link
 *  hold the run's id. The question's plain words are the words of the server's reading of it
 *  (`text_tree`), and of its stored text's reading where the read sent none (`storedReading`),
 *  on one line, with each bare address as its asker wrote it. `NamedRunRows` names the
 *  references only where the list draws them. */
function runRow(run: TreeAnswerNode, newPage: string | undefined): ExtraReferenceRow {
  const excerpt = wordsOf(run.text_tree ?? storedReading(run.text, run))
    .replace(/\s+/gu, " ")
    .trim();
  const path = buildRunPath(run.id);
  return {
    ...(run.made_at === null ? {} : { at: run.made_at }),
    edge: NODE_REFERENCES_WORDS.edges.changed_by_run,
    ...(excerpt === "" ? {} : { excerpt }),
    href:
      newPage === undefined
        ? path
        : `${path}${NEW_PAGE_SEARCH}&${OPENING_ISSUE}=${encodeURIComponent(newPage)}`,
    key: run.id,
    kind: sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction),
  };
}

/** The references a run's one-line places cite (`namedReferences`), each once, parted as
 *  `RunReferenceNames` names them. */
interface RunReferences {
  /** Each target the one batch may ask for (`useRunTargets`), by the route the text reader read
   *  it to: every address but a party's, and each key written bare, as its issue's own
   *  reference. */
  readonly batched: ReadonlyMap<string, DispatchReferenceRoute>;
  /** Each party, which a read of its own names (`useReferenceNames`). */
  readonly parties: readonly string[];
}

/** The references that `texts` name by the page's name, as a one-line place reads them, in each
 *  text without a typed block's source lines: the addresses `referenceStandIns` names, found as
 *  its `standIn` finds them, by `rowReferences`, and each key a text writes bare, which that
 *  reader, handed no issue keys, reads as no reference. Each bare key is found by the reading the
 *  outline's batch asks for it by (`citedReferences`) and given its issue's route, so the one
 *  batch asks for it, since on the run's page nothing else does; a word that reading takes for a
 *  key though it names no issue (`UTF-16`) is found and asked for too. A reference on a typed
 *  block's own line is left out, since no line draws it. Each is sorted here, once, by the route
 *  that reader parsed it to, as `RunReferenceNames` names it: a party (`isPartyRoute`) apart, and
 *  every other target to the one batch.
 *
 *  The set holds every reference of the whole texts, so it holds more than a line draws where a
 *  caller cuts its line from a text, as the run's page cuts a changed node's first line
 *  (`RunPage.tsx`'s `namedFirstLine`). That cut is made on each name's own length, which only the
 *  batch's answer gives, so before that answer no cut can say which references the line draws: a
 *  reference that a line cut off while an earlier one stood as its author wrote it can come into
 *  the line once the earlier one is named by fewer characters. A set taken from that cut would
 *  leave such a reference to a second batch, where the page sends one (R4). A reference past the
 *  cut costs the batch one address more. For the same cause each party the whole texts cite gets
 *  its read, past the cut or not: a party read only once the cut line holds it would come into
 *  that line only after the batch or another party's read answered, and its read would then go
 *  out a round after the rest. */
function namedReferences(texts: readonly string[]): RunReferences {
  const batched = new Map<string, DispatchReferenceRoute>();
  const parties = new Set<string>();
  for (const text of texts) {
    const words = withoutDirectiveLines(text);
    for (const one of rowReferences(words, [])) {
      if (isPartyRoute(one.route)) parties.add(one.reference);
      else batched.set(one.reference, one.route);
    }
    // An address `citedReferences` names is in the set already where it names an issue, since
    // `rowReferences` reads every address that does, so only the bare keys join it here.
    for (const reference of citedReferences(words)) {
      const route = batched.has(reference) ? undefined : parseNodeReference(reference);
      if (route?.kind === "issue") batched.set(reference, route);
    }
  }
  return { batched, parties: [...parties] };
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
  const lastAsked = useRef<ReadonlySet<string>>(NOTHING_PUBLISHED.asked);
  const asked = useMemo(() => {
    if (!ready) return NOTHING_PUBLISHED.asked;
    const next = new Set<string>();
    for (const [ref, route] of batched) {
      if (
        !lastAsked.current.has(ref) &&
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
    const previous = lastAsked.current;
    return next.size === previous.size && [...next].every((ref) => previous.has(ref))
      ? previous
      : next;
  }, [batched, issue?.key, known, ready]);
  useLayoutEffect(() => {
    lastAsked.current = asked;
  }, [asked]);
  const query = useMemo(() => referenceTargetsQuery([...asked]), [asked]);
  const { data } = useQuery({ ...query, ...EVENT_FRESH });
  // The key is the set itself, so a set that changes - a run the tree read adds cites a new
  // comment, or another publisher stops naming a reference this set left to it - is a read under
  // a new key, which holds no answer while it is out, nor after it fails. Meanwhile the answer of
  // the set the read answered last (`useLastAnswered`) stands for each reference the new set asks
  // for, so a name from that answer stays drawn. A name another publisher gave is in no answer of
  // this read: where that publisher unmounts meanwhile, its reference joins the new set and reads
  // as its author wrote it until the new set's read answers. The new set keeps every reference of
  // the set before that the lines still cite (`lastAsked`), so its answer, once it comes, names
  // each of those and stands alone. Only references the set asks for are published, so a
  // reference the lines cite again after dropping it is asked for again, not left to a name this
  // read published for an earlier set. The held answer is read from the cache by an observer that
  // never fetches, which also keeps it from garbage collection.
  const answeredSet = useLastAnswered(asked, data !== undefined);
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

/** The set of references the batch answered last: `asked` while its read under that set holds an
 *  answer (`answered`), and the set it last held one for while a read under a new set is out or
 *  has failed. This is `useIssueTree.ts`'s `useLastAnswered`, written again here rather than
 *  shared, since this round admits no line of that file (R6, decision 529). The ref is written
 *  during render only with the set the read answers in that render, so it never stands over a
 *  newer one. */
function useLastAnswered(asked: ReadonlySet<string>, answered: boolean): ReadonlySet<string> {
  const last = useRef<ReadonlySet<string>>(NOTHING_PUBLISHED.asked);
  if (answered) last.current = asked;
  return last.current;
}

/**
 * Draws a run's one-line places (`draw`) with what each reference they cite is named by
 * (`LineNames`), by the rule every plain line of the page names one by (`one-line.ts`'s
 * `referenceStandIns`): an issue by its key, a space and its title, a block or a whole document
 * as its chip names it, a deleted post by its deleted line, which names its actor by the page's
 * rule for an author, and a post its author edited with its mark. This is the one statement of
 * how a run's references are named, for the run's rows in Details and for the run's page alike;
 * every other comment points here. The names come from two places:
 *
 * - A party (`isPartyRoute`: an agent, a role or a person) is named by a read of its own
 *   (`useReferenceNames`), or by the page's name for it where another surface of the page
 *   published one, and the batch never asks for it (decision 340).
 * - Every other target, an issue's address among them, and each key written bare, which the
 *   batch asks for as its issue's own reference (`namedReferences`), is named by the page's
 *   published name for it alone, and is never read on its own (decision 515). Where no other
 *   surface of the page has named it or asked for it, that name comes from the run's one
 *   batched read of reference targets (`referenceTargetsQuery`, `useRunTargets`). A key written
 *   bare is read as a reference only once the page has published its issue's title
 *   (`knownIssueKeys`).
 *
 * Each reference reads as the words its author wrote until its read names it, and wherever its
 * read answers no name (decision 350): a target that is gone, one the reader may not see, which
 * the batch answers as gone, a short reference two targets carry, which the batch answers as
 * ambiguous, a target with no words to name it by, and a party its read names nothing for, as a
 * person Dispatch does not know and a role no session holds. No reference is ever drawn by its
 * short form.
 *
 * `issue` is the issue the page shows: the rule tells a whole document of it by it
 * (`documentWord`), and the batch asks no title for its own spec, which the issue read names.
 * The run's page shows none. The batch's references are handed to what `draw` draws as asked for
 * from the first render, before the store holds them, so no surface it draws mounts a read of
 * its own for one while the batch is out.
 */
export function RunReferenceNames({
  draw,
  issue,
  texts,
}: {
  draw: (lineNames: LineNames) => ReactNode;
  issue?: IssueDetails;
  texts: readonly string[];
}): ReactNode {
  const references = useMemo(() => namedReferences(texts), [texts]);
  const published = useRunTargets(references.batched, issue);
  const reader = { draw, issue: issue?.key, parties: references.parties };
  const props = { children: createElement(RunNameReader, reader), published };
  return createElement(PublishedReferenceNames, props);
}

/** The names a run's one-line places are drawn with, as `RunReferenceNames` says, read as
 *  `RowText.tsx`'s `useRowNames` reads a row's: the page's published names, its rule for an
 *  author and the issue keys it named. This is `one-line.ts`'s `useIssueNames` with each party's
 *  own read added, written again here rather than called: that hook takes the key of the issue a
 *  page shows, and the run's page shows none, and changing it to take no issue, or exporting the
 *  `useDeletedActorsAuthor` it reads its author by, would change a line of that file, and R6
 *  admits none this round. */
function RunNameReader({
  draw,
  issue,
  parties,
}: {
  draw: (lineNames: LineNames) => ReactNode;
  issue: string | undefined;
  parties: RunReferences["parties"];
}): ReactNode {
  const { ambiguous, deleted, edited, lines, names: published } = useKnownReferences();
  const author = useDeletedActorsAuthor(deleted);
  const issues = useMemo(() => knownIssueKeys(published), [published]);
  // Only a party is read here, as `RunReferenceNames` says, and one its read names nothing for
  // stays out of the names.
  const { names: read, reads } = useReferenceNames(parties, undefined);
  const names = useMemo(() => {
    let named: Map<string, string> | undefined;
    for (const party of parties) {
      const name = read.get(party)?.name;
      if (name === undefined || name === "") continue;
      named ??= new Map(published);
      named.set(party, name);
    }
    return named ?? published;
  }, [parties, published, read]);
  const lineNames = useMemo<LineNames>(
    () => ({ ambiguous, author, deleted, edited, issue, issues, lines, names }),
    [ambiguous, author, deleted, edited, issue, issues, lines, names]
  );
  return createElement(Fragment, null, reads, draw(lineNames));
}

/** The page's rule for an author (`AgentText.author`), which a run's one-line place reads only
 *  through the deleted line of a post in `deleted`. The rule is made anew at every poll of the
 *  agent registry, and each new one would build `LineNames` and every run row anew, so the rule
 *  read before is kept while `deleted` stands and the new rule names every actor in it as the
 *  kept one does. This is `one-line.ts`'s `useDeletedActorsAuthor`, written again here rather
 *  than shared, since that file keeps it to itself: exporting it would change a line of that
 *  file, and R6 admits none this round. */
function useDeletedActorsAuthor(deleted: LineNames["deleted"]): AgentText["author"] {
  const { author } = useAgentText();
  // Each deleted post's actor as `author` names it, the name its deleted line draws, and null
  // for a post that names no actor, whose line names none (`deletedLineText`).
  const actors = JSON.stringify(
    [...deleted.values()].map(({ by }) => (by === null ? null : author(by)))
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: `author` is kept while `deleted` and `actors` stand, since `actors` is everything `author` is read for here
  return useMemo(() => author, [actors, deleted]);
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
    draw: (lineNames) => createElement(RunExcerpts, { draw, lineNames, rows }),
    issue,
    texts,
  });
}

function RunExcerpts({
  draw,
  lineNames,
  rows,
}: {
  draw: (rows: readonly ExtraReferenceRow[]) => ReactNode;
  lineNames: LineNames;
  rows: readonly ExtraReferenceRow[];
}): ReactNode {
  // The rows are named anew whenever the names change, and `draw` reads them as it draws.
  const named = useMemo(
    () =>
      rows.map((row) => {
        if (row.excerpt === undefined) return row;
        const references = referenceStandIns(lineNames);
        return { ...row, excerpt: references.named(references.standIn(row.excerpt)) };
      }),
    [lineNames, rows]
  );
  return draw(named);
}

/** One row for each run whose node some page holds and that a `changed_by_run` tuple of some page
 *  names (`tuple.to`), once however many nodes it changed and however many pages name it, newest
 *  first. The pages are those of the tree read that the page holds. A page of older comments,
 *  read with `comments_before`, answers those comments and their tuples and no other node, so a
 *  run that changed only an older comment has its tuple there and its node in no page, and draws
 *  no row. A run no such tuple names draws no row, and nor does a read that holds no page. Each
 *  row links to its run's page by an address that carries `?issue_page=new` and the key of the
 *  issue `newPage` where it names one, the issue at whose address carrying that parameter the
 *  page is (`useRunRows`), and by the plain address otherwise. */
export function runRows(
  pages: readonly TreeAnswer[],
  newPage?: string
): readonly ExtraReferenceRow[] {
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
  return runs.sort(newestFirst).map(({ run }) => runRow(run, newPage));
}

/** The rows of the tree read's one answer, which holds every page its walk read (`readTree`),
 *  each linking to its run's plain address: one function, so the rows keep their identity until
 *  the answer changes. */
function answerRows(answer: TreeAnswer): readonly ExtraReferenceRow[] {
  return runRows([answer]);
}

/** `answerRows` with each row linking to its run's page by an address that carries
 *  `?issue_page=new` and the issue `key`, for a page at an address of that issue that carries
 *  it: one function for each key, made as the page comes to such an address (`useRunRows`), so
 *  the rows keep their identity until the answer or the issue changes. */
function newPageAnswerRows(key: string): (answer: TreeAnswer) => readonly ExtraReferenceRow[] {
  return (answer) => runRows([answer], key);
}

/** Where the page is: the address `useLocation` answers, as `useRunRows`' caller hands it. */
type PageAddress = { readonly pathname: string; readonly search: string };

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
 * are none. `location` is the page's address, as its caller's `useLocation` answers it: where it
 * is an issue's address that carries `?issue_page=new` (`newPageIssue`), each row links to its
 * run's page with it and that issue's key, so a reader whose setting is off opens the run's page
 * from an issue page that parameter opened, and the run's page keeps it on that issue's links
 * (decision 533). With no `location` the page is at no such address, and each row links to its
 * run's plain address. The count is the same either way.
 */
export function useRunRows(
  issue: IssueDetails,
  open: boolean,
  location?: PageAddress
): RunRowsRead {
  const tree = issueTreeQuery(issue.key);
  const newPage =
    location === undefined ? undefined : newPageIssue(location.pathname, location.search);
  const select = useMemo(
    () => (newPage === undefined ? answerRows : newPageAnswerRows(newPage)),
    [newPage]
  );
  const read = useQuery({ ...tree, enabled: false, select });
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
