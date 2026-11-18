import {
  focusManager,
  type Mutation,
  matchMutation,
  matchQuery,
  type Query,
  type QueryClient,
  type QueryFilters,
  queryOptions,
  replaceEqualDeep,
  useQuery,
} from "@tanstack/react-query";
import { useMemo } from "react";

import {
  api,
  type IssueTreeAnswer,
  type IssueTreeNode,
  type IssueTreeTuple,
} from "../../../api/client";
import { liveFeedUp, onLiveFeedBack } from "../../../api/live-socket";
import {
  MIN_REFETCH_INTERVAL_MS,
  type RefreshTarget,
  refreshQueries,
} from "../../../api/query-refresh";
import type { IssueDetails } from "../../../api/types";
import { pastDeadlineKey } from "../../../hooks/useSending";
import { refetchFailedOnFocus } from "../../refs/reference-target";
import { blocksOfRole, mergeTreePages, type TreeBlock } from "./tree-nodes";

/**
 * The issue's tree read (`GET /api/v1/issues/{key}/tree`), which the model serves: the one file
 * of the issue page that names the route. Every other file reads the answer through the names
 * below, so the day the answer's shape moves onto the generated contract only these aliases
 * change.
 */
export type TreeAnswer = IssueTreeAnswer;
export type TreeAnswerNode = IssueTreeNode;
export type TreeTuple = IssueTreeTuple;

/** The views of the tree a filter reads (`?view=`). Each is the query of the document of the
 *  same role, so the name says which role's blocks a caller asks for as well (`blocksOfRole`). */
export type TreeFilter = "spec" | "verification" | "plan";

/** Every view of the tree a read asks for (`?view=`): a role document's (`TreeFilter`), and the
 *  decisions view, the issue's decisions wherever they are written. That view plays no role, so
 *  `blocksOf` takes no such name. */
export type TreeView = TreeFilter | "decisions";

/** The views of the tree the filters read, one for each filter, in this order: Spec's,
 *  Verification's and Plan's role documents, and the issue's decisions wherever they are
 *  written (`?view=decisions`). */
export const TREE_VIEWS: readonly TreeView[] = ["spec", "verification", "plan", "decisions"];

/** The tree read's key. It is not under `["issue", key]`, so a write that invalidates the
 *  issue's key by prefix does not reach it: `subscribeTreeFreshness` refreshes it, `exact` since
 *  every view's key is under it, after a write or an event changed a query of the issue, and a
 *  write of the page may refresh it by this key itself, through `refreshQueries`. */
export function issueTreeKey(key: string): readonly ["issue-tree", string] {
  return ["issue-tree", key];
}

/** One view's key: under the tree's, and refreshed by `subscribeTreeFreshness` alone too. */
export function viewKey(key: string, view: TreeView): readonly ["issue-tree", string, TreeView] {
  return ["issue-tree", key, view];
}

/** Resets every view of issue `key` the cache holds (`resetQueries`) for a load of the issue
 *  (`ViewReadProvider`), so a view a past load held is gone and each is read once: the open one at
 *  the load, at once, whether a write of the issue is out or not, and another when its filter is
 *  next on. It marks none. `subscribeTreeFreshness` resets none: it refreshes each held view
 *  through `refreshQueries`, and the view keeps what it draws until its new answer lands. */
export function resetViews(queryClient: QueryClient, key: string): void {
  for (const view of TREE_VIEWS) {
    void queryClient.resetQueries({ exact: true, queryKey: viewKey(key, view) });
  }
}

/** Drops every view of issue `key` for a write's own callback, as Dispatch answers the write - a
 *  Settle, a Keep, a Change (`DecisionActions.tsx`) - after which a held view could be older
 *  than the tree the page draws. A view it drops after a read of it, while a write of the issue
 *  holds it, is a refresh: held as an answered one and read once as the last write settles
 *  (`droppedWhileHeld`, `mayReadNow`). One never read still reads at once. */
export function dropViews(queryClient: QueryClient, key: string): void {
  for (const name of TREE_VIEWS) {
    const view = queryClient.getQueryCache().find({ exact: true, queryKey: viewKey(key, name) });
    if (view !== undefined && heldByWrites.has(view)) {
      const { fetchStatus, status } = view.state;
      if (status !== "pending" || fetchStatus !== "idle") droppedWhileHeld.add(view);
    }
  }
  resetViews(queryClient, key);
}

/**
 * Every page of the tree read, or of one view, asked for in turn: the first, then each next page
 * the page before names (`cursor`), and the pages merged once the last answers, so the tree is
 * built once and never from part of the walk. A page that names no next page is the last,
 * whether its cursor is null, left out of the body or empty, as a server that drops an empty
 * field writes it. A cursor the walk has asked for already ends it there with the pages read, so
 * a server that names one page again cannot hold the walk in a loop. A page that fails fails the
 * read, and a read made again - a refetch, a retry - starts from the first page, since a cursor
 * names a place in the walk as it stood when the page that gave it was answered. A read TanStack
 * gives up (`signal`: a reset, the last observer gone) ends its page in flight and asks for no
 * next one. It waits for nothing before its first page: what holds a refresh back while a write
 * of the issue is out is the query's `enabled` (`TREE_FRESH`), so a read that is begun is sent.
 */
async function readTree(
  key: string,
  view: TreeView | undefined,
  signal: AbortSignal
): Promise<TreeAnswer> {
  const first = await api.getIssueTree(key, { view }, { signal });
  const pages: [TreeAnswer, ...TreeAnswer[]] = [first];
  const asked = new Set<string>();
  let cursor: string | null | undefined = first.cursor;
  while (typeof cursor === "string" && cursor !== "" && !asked.has(cursor)) {
    asked.add(cursor);
    signal.throwIfAborted();
    const page = await api.getIssueTree(key, { cursor, view }, { signal });
    pages.push(page);
    cursor = page.cursor;
  }
  return mergeTreePages(pages);
}

/**
 * How the page's reads are kept fresh: by the events that can change them, never by age, so a
 * slot the stage mounts again past the app's 30 s `staleTime` sends no read; only a read whose
 * last answer failed is read again on focus (`refetchFailedOnFocus`), so a read that failed
 * recovers on the open page while a healthy one stays quiet. Every read of `useIssueTree` that an
 * event refreshes takes it, which names it `EVENT_FRESH`; the tree read and its views take it,
 * with the changes `TREE_FRESH` names.
 */
export const REFRESHED_BY_EVENTS = {
  refetchOnWindowFocus: refetchFailedOnFocus,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The tree and view queries the page's one subscription asked for again since their last read
 *  began (`subscribeTreeFreshness`): each of its asks records every tree and view query it asks
 *  for - its refresh after a change of the issue, and its ask for a refused read on the live feed
 *  coming back or a window focus while the feed is down - and the query's next read clears the
 *  record as it begins. TanStack keeps no such record: a refused read is marked stale by its
 *  refusal already, and marking it stale again records nothing new. Held as plain objects, since
 *  each query is typed by its own key and answer. */
const askedAgain = new WeakSet<object>();

/** Whether a new observer of the tree or of a view may read it (`TREE_FRESH`): where its last
 *  read was not refused, or where the page's subscription asked for it again since that read
 *  began (`askedAgain`). It takes the query by its state, so the tree's query and each view's,
 *  each typed by its own answer, take the one predicate. */
function mayReadOnMount(query: Pick<Query, "state">): boolean {
  return query.state.status !== "error" || askedAgain.has(query);
}

/** The tree and view queries a write of their issue holds, from the first such write going out
 *  to the last settling, as the page's subscription records them (`subscribeTreeFreshness`). Held
 *  as plain objects, as `askedAgain` is. */
const heldByWrites = new WeakSet<object>();

/** The views a write's own callback dropped (`dropViews`) while a write of their issue held them,
 *  after a read of them - answered, refused or still out - until the last write settles, as the
 *  page's subscription records it: TanStack runs the callback before the write leaves `pending`.
 *  A view never read that a drop resets holds nothing either way, and its first read is still
 *  sent at once; a load's reset (`resetViews`) marks none. Held as plain objects, as `askedAgain`
 *  is. */
const droppedWhileHeld = new WeakSet<object>();

/** Whether TanStack may read the tree or a view of its own accord or for a refresh (`TREE_FRESH`'s
 *  `enabled`): always for a read that holds neither an answer nor a refusal yet - a first load, a
 *  view's first filter press, one a load reset (`resetViews`) - and for any other only while no
 *  write of its issue holds it (`heldByWrites`), since a read sent then reads a tree Dispatch may
 *  not have changed yet. A view a write's own callback dropped after a read of it, while a write
 *  holds it (`dropViews`, `droppedWhileHeld`), holds nothing either, but is a refresh, held as an
 *  answered one. A disabled query is no query of a refresh: the app's client refetches none
 *  (`api/query-client.ts`), the refresh scheduler only marks it stale (`api/query-refresh.ts`)
 *  and a new observer reads none of it, so nothing is sent while the write is out, and the
 *  subscription reads what was asked for once the last write settles. The reader's Retry is an
 *  observer's `refetch`, which TanStack sends whatever `enabled` says. */
function mayReadNow(query: Pick<Query, "state">): boolean {
  if (!heldByWrites.has(query)) return true;
  return query.state.status === "pending" && !droppedWhileHeld.has(query);
}

/** How the tree read and its views are kept fresh: as `REFRESHED_BY_EVENTS`, except that none of
 *  TanStack's own reads asks again for a read Dispatch refused, and none is sent for a refresh
 *  while a write of the issue holds it (`mayReadNow`). A refused read, whether it holds
 *  an answer or not, stays refused until the reader's Retry, an event of the issue, the live feed
 *  coming back, or a window focus while the feed is down: the page's one subscription asks for it
 *  again on each of those but the Retry (`subscribeTreeFreshness`). So no window focus reads one
 *  of TanStack's own accord, nor does the browser saying the network is back, which it may say
 *  while the feed never went; and a new observer - a slot drawn, a surface opened, a filter
 *  pressed, the reader coming back to the issue - reads a refused one only where the
 *  subscription asked for it again since its last read began (`mayReadOnMount`, as both
 *  `retryOnMount`, for a read that holds no answer, and `refetchOnMount`, for one that holds an
 *  answer). The views are owed besides at each load of the issue, answered or refused: a load
 *  resets them (`resetViews`), so each is read once, the open one at the load, at once, with a
 *  write of the issue out or none, and another when its filter is next on. A read that holds an
 *  answer which a write or an event marked stale while nothing observed it is read when something
 *  next observes it, and a read never made is made. */
const TREE_FRESH = {
  ...REFRESHED_BY_EVENTS,
  enabled: mayReadNow,
  refetchOnMount: mayReadOnMount,
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  retryOnMount: mayReadOnMount,
} as const;

/** The issue's whole tree, one query over every page (`readTree`). */
export const issueTreeQuery = (key: string) =>
  queryOptions({
    ...TREE_FRESH,
    queryKey: issueTreeKey(key),
    queryFn: ({ signal }) => readTree(key, undefined, signal),
  });

/** One view of the issue's tree, one query over every page (`readTree`). A plain query rather
 *  than one per page, so an observer that is never enabled can watch a view a slot read. A filter
 *  turned on (`ViewReadProvider`) reads its view as any new observer of the tree's reads does
 *  (`TREE_FRESH`): where the view was never read, where a write or an event marked its answer
 *  stale, and, where its read was refused, only where the page's subscription asked for it again
 *  since that read began. A filter turned off and on again asks for nothing, so it reads a
 *  refused view no more. */
export const viewQuery = (key: string, view: TreeView) =>
  queryOptions({
    ...TREE_FRESH,
    queryKey: viewKey(key, view),
    queryFn: ({ signal }) => readTree(key, view, signal),
  });

/** One role's blocks from the tree read, answered as the blocks read of that role's document
 *  answered them (`artifactBlocksQuery`), so a caller's lines that read these members stay as
 *  they are. The state is the tree read's own. */
export interface TreeBlocksRead {
  readonly data: TreeBlock[] | undefined;
  readonly isError: boolean;
  readonly isSuccess: boolean;
  readonly refetch: () => Promise<unknown>;
}

/** The tree read, and how far it has got. */
export interface TreeRead {
  /** Every page merged, once the last has answered. */
  readonly answer: TreeAnswer | undefined;
  /** Whether the read holds an answer, which is what a view that draws from the blocks it holds
   *  waits on. A refetch that fails over an answer already held keeps it held - TanStack keeps a
   *  query's data through a failed refetch - so the view keeps the text it drew, which waiting
   *  on `blocksRead` would blank, since that failure turns it false. Both are false before the
   *  first answer and after a first read that fails. */
  readonly blocksHeld: boolean;
  /** Whether the read answered every page with success. A read that fails, a first one or a
   *  refetch, turns it false, even where an answer is still held. It is what a view reads that
   *  must tell "not read yet" from "holds none": until it is true a typed block - the spec's
   *  summary among them, which the head waits on before it says a spec has none
   *  (`IssueThreeThings.tsx`) - may be missing because it has not arrived rather than because
   *  the issue holds none. A view that draws from the blocks it holds reads `blocksHeld`
   *  instead. */
  readonly blocksRead: boolean;
  /** The typed blocks of the document that plays `role` (`blocksOfRole`), so a view reads its
   *  blocks here and costs no request of its own; the same value for a role until the answer or
   *  the read's state changes. */
  readonly blocksOf: (role: TreeFilter) => TreeBlocksRead;
  /** Whether the read was refused with no answer held, its retries spent, and holds none yet:
   *  from that refusal until a read answers, the reads asked again in between included, whoever
   *  asked them - the reader's Retry, an event, the live feed coming back, a focus while the
   *  feed is down. TanStack puts a query that holds no answer back to `pending` while it is read
   *  again, so this reads its count of refusals instead, which only a reset of the query clears.
   *  A refetch that fails over a held answer is no such failure, since the page still draws what
   *  it holds. */
  readonly failed: boolean;
  /** Whether the read holds no answer and has not settled since its last read began: its first
   *  read is out, or a read asked again after a refusal is, while `failed` holds too. It is what
   *  holds back a read that waits on every other read of the page, as the batched name read does
   *  (`useIssueTree`). */
  readonly pending: boolean;
  /** Reads the tree again, every page from the first: what a Retry of a failed read asks. */
  readonly refetch: () => Promise<unknown>;
}

/**
 * The tree read alone, for the issue page's model (`useIssueTree`) and for a surface that needs
 * the blocks of a role document, or how the read stands, and none of the model's other reads.
 * Each value it answers keeps its identity while the read's answer and state stand, so a
 * caller's memo built on one is not built again on every render; a role's blocks are made the
 * first time a caller asks for them, so a caller that reads none costs none.
 */
export function useTreeRead(issue: IssueDetails): TreeRead {
  const {
    data: answer,
    errorUpdateCount,
    isError,
    isPending,
    isSuccess,
    refetch,
  } = useQuery(issueTreeQuery(issue.key));
  const blocks = useMemo(
    () => byRole((role) => blocksOfRole(answer, role, issue)),
    [answer, issue]
  );
  const blocksOf = useMemo(
    () => byRole((role): TreeBlocksRead => ({ data: blocks(role), isError, isSuccess, refetch })),
    [blocks, isError, isSuccess, refetch]
  );
  return useMemo(
    () => ({
      answer,
      blocksHeld: answer !== undefined,
      blocksRead: isSuccess,
      blocksOf,
      failed: answer === undefined && errorUpdateCount > 0,
      pending: isPending,
      refetch,
    }),
    [answer, blocksOf, errorUpdateCount, isPending, isSuccess, refetch]
  );
}

/** `make`'s value for each role, made on the first ask for that role and the same value on
 *  every ask after. */
function byRole<Value>(make: (role: TreeFilter) => Value): (role: TreeFilter) => Value {
  const made = new Map<TreeFilter, { readonly value: Value }>();
  return (role) => {
    let held = made.get(role);
    if (held === undefined) {
      held = { value: make(role) };
      made.set(role, held);
    }
    return held.value;
  };
}

/**
 * Keeps the tree read and its views as fresh as the issue's other reads, which no write
 * reaches directly: their keys are not under `["issue", key]` (`issueTreeKey`). A query of the
 * issue (`["issue", key, …]`, `["asks", key, …]`, `["comments", key, …]` and
 * `["issue-messages", key, …]`) has changed when it is:
 *
 * - invalidated: a page write's `invalidateQueries`, and an event's own refresh
 *   (`api/query-refresh.ts`' `owe`), whose targets name `["issue", key]` for every event of the
 *   issue, a Keep's included (`api/event-refresh.ts`), while a write of the issue is out too
 *   (below);
 * - written by `setQueryData` while the live feed is down: a status change writes that way
 *   (TanStack's `success` action with `manual`), so such a write refreshes the tree with no
 *   event to do it. While the feed is up the write's event refreshes the tree, so a write that
 *   puts its answer in the cache before Dispatch holds it - the margin's resolve does as it is
 *   pressed, and invalidates the comments once Dispatch answers - reads the tree for that answer
 *   and for its event, twice at most, and never before Dispatch holds it;
 * - answered by a read that began in a turn after the tree's last read began (`answeredNewer`),
 *   where the query was marked invalid as that read began, or the answer is other than the data
 *   it held when that tree read began - the issue read's other in a field someone's act moves
 *   (`UNACTED_ISSUE_FIELDS`). TanStack reports an invalidation only when a query turns invalid,
 *   so a write whose invalidation lands while the query is invalid already - its last read
 *   failed, a refresh it owes (`owe`) has not begun, its read for an earlier write is still out -
 *   raises no event, and the read made for it, which the app's client sends at once or once the
 *   one out settles, is how that write shows, whatever it answers: where the query's first read
 *   failed there is nothing held to compare it with. A read of a valid query that answers what it
 *   held, as one made again for its age does, changes nothing; nor does any read that began
 *   before the tree's or in the same turn, which the tree read answers already, as the reads a
 *   page coming back to the issue sends, or a refresh of every query the page holds, begin
 *   together.
 *
 * On any of them it reads the tree again and marks every held view stale, the decisions view
 * among them, which reads the open view again while each view keeps the answer it holds, and
 * drawn, until its new one lands. Every change of one turn is one refresh, made in the microtask
 * after it.
 *
 * No refresh reads the tree or a view while a write of the issue is out, with the live feed up
 * or down: no event says when a write has reached Dispatch, and a tree read made then reads a
 * tree Dispatch may not have changed yet. A write is the issue's where its mutation key names the
 * issue (an element equal to its key), where it began while the issue's page drew its tree, or
 * where its key is exactly the past-deadline key of a write of the issue still out, as a
 * composer's send held past its deadline is. A write begun on another page that is none of these
 * holds nothing here, though its key equals or extends the key of one of them, or names no issue.
 * While one is out the tree and each view that holds an answer or a refusal is disabled
 * (`mayReadNow`), so the app's client, the refresh scheduler and a new observer send none of it,
 * and nothing waits in a query function; a first load, a view's first filter press and the
 * reader's Retry are sent at once, and each is owed one more read once the last write settles,
 * which answers the write. A view a write's own callback drops meanwhile (`dropViews`) - a
 * Settle, a Keep, a Change, as Dispatch answers the write and before TanStack lets it go - is no
 * first read but a refresh: held as an answered one, and read once at that settle; a load's reset
 * (`resetViews`) is not. The asks of a held read the subscription hears are owed at that settle
 * too (`owedReads`): a stale mark put on it - by a refresh of every query, the write store's for
 * a write that lands with no answer or the event feed's after an
 * incomplete replay, and by the subscription's own asks - which a held refused read hears as
 * well, since it is marked valid while held (`hearAsks`); a focus's ask while the live feed is
 * down and the feed's return; and a new observer, whose read the hold holds, where the read is
 * marked stale and the observer may read it (`mayReadOnMount`). A `refetchQueries` of a held read
 * is not heard: TanStack's client skips a disabled query and tells nothing of it, and nothing in
 * the page asks for the tree or a view that way. While the
 * writes are out an invalidation of a query of the issue is a change, as it is with none out: an
 * event may refresh reads that answer what they held, and the trigger is what the tree owes, not
 * its answer. So a write Dispatch refuses, whose rollback puts the comments back and reads them
 * again, reads the tree once as it settles, and an event landing meanwhile is read in that one
 * read. A query's answer is a change besides where it differs from the data the query held when
 * the first of the writes went out. Every change heard while the writes are out is the one
 * refresh, run a microtask after the last of them settles, answered or refused, so a write
 * pressed as it settles keeps the hold (`settle`): the refresh held back asks for the tree and
 * every view, the owed ones among them, and with none held back each owed read is asked for, so
 * each is read once. So the margin's resolve with the feed down reads the tree once, after
 * Dispatch answered, and a write that lands with no answer, whose write store refreshes every
 * query while it is still out (`write-store.ts`), reads the tree and the open view once, as the
 * last write out settles.
 *
 * Two kinds of write hold nothing, and the event that follows each is read at once: a write of
 * the issue made on another page - the inbox's drawer, a list - which names no issue and begins
 * while the issue's tree is not drawn; and a write the write store makes with no mutation, of a
 * kind with no `started` - a plain write, a pin, a broadcast, a row action of the issues table, an
 * offer of the search - with its Retry and its restore after a reload, which the mutation cache
 * never tells of. A composer's send holds its name in the mutation cache on each press, Retry
 * and restore after a reload (`features/conversation/held-sends.ts`), so the Conversation
 * composer's, which names the issue, holds the tree each time.
 *
 * A window focus reads the tree and its views only while the live feed is down (`liveFeedUp`):
 * a refused one is read again then, or, where a write of the issue is out, once the last write
 * out settles while the feed is still down, and a read of the issue the focus sends may change it
 * as above. While the feed is up its events say what changed, so a focus reads neither, and what
 * the reads of the issue a focus sends answer is no change. The feed coming back after a gap
 * (`onLiveFeedBack`) with the gap replayed whole reads a refused tree or view again at once,
 * since no replayed event need name the issue; after an incomplete replay the event feed's
 * refresh of every query reads each one something observes, and the subscription asks for none
 * of those, which would read it twice, but only for a refused one nothing observes, which that
 * refresh marks stale and no more. A tree or view read out as a focus or a whole replay asks for
 * every refused one is owed: where it then ends refused it is asked for once more, where it
 * answers nothing more is, and either way it is owed no more.
 *
 * A refused tree or view read, its retries spent, with or without an answer held, so stays
 * refused until the reader's Retry, an event of the issue, the feed coming back or a focus while
 * the feed is down. Each ask of the subscription records every tree and view query it asks for
 * as asked for again (`askedAgain`) until its next read begins, and a new observer reads a
 * refused one only where it is so recorded (`TREE_FRESH`): a refused one nothing observes as the
 * subscription asks for it is read when something next observes it, and no slot drawn, surface
 * opened or filter pressed reads one otherwise.
 *
 * All of it goes through the app's refresh scheduler (`refreshQueries`), which never cancels a
 * read in flight - a walk of many pages is not started over by each event of a burst - reads a
 * query that is out once more when it settles, and starts one at most once a second. So the
 * subscription starts no tree read while one is out, and owes one at most after it: a change
 * while the tree's first read is out, or after that read failed, is one more read once it
 * settles; a write and the event it raises read the tree once or twice, never more. A read that
 * began in the turn the change was seen in is not asked for again, since it answers the change
 * already, as a refresh of every query the page holds reads the tree and the open view itself.
 * The tree's and the views' own keys are no query of the issue, and what the refresh's own calls
 * raise is ignored (`calling`), so a refresh never starts another. The tree read it asks for is
 * held wherever a read of the issue the scheduler owes is still to begin, whether or not the
 * scheduler would hold the tree itself: one observed, marked invalid, answered and none out, due
 * a second after its last read began - as the comments read an event marked invalid is when its
 * last read began under a second ago - or one the event marked invalid while its read was out, so
 * owed once more as that read lands, as the scheduler reads it then: the comments, asks, issue or
 * messages read alike. The refresh marks it stale at once and asks for it once the scheduler
 * begins it at once and every such read has begun, an owed read out until it lands, so it begins
 * inside the refresh's own call, and a change heard meanwhile joins that one held ask. Such a
 * read pays the change the tree read pays, and begun after the tree's, while still marked
 * invalid or answering other than the tree's read saw, it would count as newer and read the tree
 * once more for one event. A read of the tree anyone else begins first - a Keep's, a stage's as
 * the reader comes back - answers that ask, as it pays a refresh the scheduler owes, and is no
 * read of the refresh's own; and the ask goes to the scheduler, which marks the read stale where
 * nothing draws it, as the reader leaves.
 *
 * It is one subscription for each client and issue, which outlives the stage that asks for it:
 * kept while a stage holds it, and after, for as long as the cache holds the issue's tree. A
 * change made while no stage of the issue is mounted - the event of another person's write while
 * the reader is elsewhere - marks the tree stale, as the scheduler marks any query nothing draws,
 * so the tree is read again when the reader comes back, a refused one too, since the refresh
 * asked for it again; and a refresh that lands after the stage went sends nothing. Answers the
 * release of this stage's hold.
 */
export function subscribeTreeFreshness(queryClient: QueryClient, key: string): () => void {
  let byKey = freshness.get(queryClient);
  if (byKey === undefined) {
    byKey = new Map();
    freshness.set(queryClient, byKey);
  }
  const held = byKey.get(key) ?? watchTree(queryClient, key, byKey);
  held.holders += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    held.holders -= 1;
    held.release();
  };
}

/** One issue's subscription (`subscribeTreeFreshness`), and how many stages hold it. */
interface TreeFreshness {
  holders: number;
  /** Ends the subscription where no stage holds it and the cache holds no tree of the issue. */
  readonly release: () => void;
}

/** Each client's subscriptions, by the issue's key. */
const freshness = new WeakMap<QueryClient, Map<string, TreeFreshness>>();

/**
 * The fields of the issue read (`IssueDetails`) that move with no act of anyone: its claim
 * holder's liveness (`holder_state`, `last_seen`, `takeable_at`) and its route's reach
 * (`route_holder`, `route_status`, `route_status_reason`), which Dispatch resolves on each read
 * from its copy of the listener's session list, keeps in no table and puts on no event (the
 * contract's `IssueClaimLiveness` and `IssueRouteReach`). An answer of the issue read that moved in
 * these alone says nothing the tree read answers, so it reads the tree no more.
 */
const UNACTED_ISSUE_FIELDS: Partial<Record<keyof IssueDetails, true>> = {
  holder_state: true,
  last_seen: true,
  route_holder: true,
  route_status: true,
  route_status_reason: true,
  takeable_at: true,
};

/** Whether two answers of the issue read differ in no field but `UNACTED_ISSUE_FIELDS`. A field an
 *  answer did not change keeps the object of the answer it replaces (TanStack's structural
 *  sharing, which `issueQuery` keeps), so each field is compared by identity. */
function sameActs(held: unknown, now: unknown): boolean {
  if (held === now) return true;
  if (typeof held !== "object" || held === null || typeof now !== "object" || now === null) {
    return false;
  }
  const before = held as Record<string, unknown>;
  const after = now as Record<string, unknown>;
  const moved = (field: string): boolean =>
    !Object.hasOwn(UNACTED_ISSUE_FIELDS, field) && before[field] !== after[field];
  return !Object.keys(before).some(moved) && !Object.keys(after).some(moved);
}

/** The subscription of issue `key`, entered in `byKey` until it ends. */
function watchTree(
  queryClient: QueryClient,
  key: string,
  byKey: Map<string, TreeFreshness>
): TreeFreshness {
  const cache = queryClient.getQueryCache();
  const tree: RefreshTarget = { exact: true, queryKey: issueTreeKey(key) };
  const issueRead: QueryFilters = { exact: true, queryKey: ["issue", key] };
  const issueQueries: readonly QueryFilters[] = [
    { queryKey: ["issue", key] },
    { queryKey: ["asks", key] },
    { queryKey: ["comments", key] },
    { queryKey: ["issue-messages", key] },
  ];
  const views: readonly RefreshTarget[] = TREE_VIEWS.map((view) => ({
    exact: true,
    queryKey: viewKey(key, view),
  }));
  const treeAndViews: readonly RefreshTarget[] = [tree, ...views];
  // The tree's query and each view's, by the prefix they share.
  const treeQueries: QueryFilters = { queryKey: issueTreeKey(key) };
  // What each query of the issue holds now, by its hash.
  const issueData = (): Map<string, unknown> => {
    const data = new Map<string, unknown>();
    for (const filters of issueQueries) {
      for (const one of cache.findAll(filters)) {
        if (one.state.data !== undefined) data.set(one.queryHash, one.state.data);
      }
    }
    return data;
  };
  let calling = false;
  let queued = false;
  let stopped = false;
  // When the tree read and each read of the issue last began, as this subscription records it:
  // once the cache has told every listener that the read began, the scheduler among them, so
  // this record is never earlier than the scheduler's own, whichever of the two hears of the read
  // first.
  const startedAt = new Map<string, number>();
  // Records that the read of `hash` began, in a microtask, so every listener has heard it began.
  const recordStart = (hash: string): void => {
    queueMicrotask(() => {
      startedAt.set(hash, Date.now());
    });
  };
  // The tree read a refresh asked for while a read of the issue the scheduler owes was still to
  // begin, until the refresh asks for it once the scheduler begins it at once (`timer`), or a read
  // of it begins. `since` is the moment from which it waits for the reads of the issue the
  // scheduler owes (`owedDues`).
  const heldAsks = new Map<RefreshTarget, { since: number; timer: number | undefined }>();
  // Whether a window focus heard while the live feed was up may still be beginning its reads:
  // TanStack begins them a few microtasks after it tells its listeners (`QueryClient.mount`), so
  // a read begun before the next task is the focus's.
  let focusing = false;
  // The writes of this issue not settled yet: each that names the issue in its mutation key (an
  // element equal to `key`, as the issue's own update does, never a key that merely begins with
  // it), or that began while this issue's page drew its tree (the tree observed as the mutation
  // cache told of the write), or whose key is exactly the past-deadline key of such a write still
  // out (`pastDeadlineKey`, `hooks/useSending.ts`): a composer's send held past its deadline, as
  // `features/conversation/held-sends.ts` holds a late send, made before the name lets go, is the
  // same send, whatever page the reader is on by then. Any other write is a write of its own,
  // judged as any: one under an equal key or another extension of it, begun on another page and
  // naming no issue, holds nothing here. And whether one of them is out (`unmutate`): the hold,
  // from the first going out to the last settling, which `settling` keeps until the settle's
  // reads run, a microtask on.
  const names = (mutation: Mutation): boolean =>
    mutation.options.mutationKey?.includes(key) === true;
  const ownWrites = new Set<Mutation>(
    queryClient.getMutationCache().findAll({ predicate: names, status: "pending" })
  );
  const ownWriteOut = (): boolean => {
    for (const one of ownWrites) {
      if (one.state.status === "pending") return true;
    }
    return false;
  };
  // Whether `mutation`'s key is exactly the past-deadline key of a write of this issue still out.
  const lateOwnSend = (mutation: Mutation): boolean => {
    for (const one of ownWrites) {
      const { mutationKey } = one.options;
      if (mutationKey === undefined || one.state.status !== "pending") continue;
      if (matchMutation({ exact: true, mutationKey: pastDeadlineKey(mutationKey) }, mutation)) {
        return true;
      }
    }
    return false;
  };
  let writing = ownWriteOut();
  let settling = false;
  const holding = (): boolean => writing || settling;
  // Whether a refresh was held back for a write of the issue out (`refresh`): it runs once the
  // last write of the issue settles (`settle`).
  let heldBack = false;
  // The data each query of the issue held when the first write of the hold went out, or, once a
  // tree read begins with no write of the issue out, what the issue held as it began; and the
  // queries whose next answer is judged against it (`answeredNewer`): each one read or
  // invalidated while a write was out, until that answer lands, which may be after the writes
  // settle.
  let heldData = writing ? issueData() : new Map<string, unknown>();
  const judged = new Set<string>();
  // The tree and view reads owed one read once the last write of the hold settles: each sent
  // while a write was out - a first load, a view's first filter press, the reader's Retry - and
  // each a refresh asked for while the write held it (`mayReadNow`), which sent nothing.
  const owedReads = new Set<string>();
  // The tree and view reads out when a window focus while the live feed was down, or the feed back
  // with its gap replayed whole, asked for every refused one (`askRefused`), or when the settle
  // asked for them for a focus a write of the issue held (`settle`): each is asked for once more
  // where it ends refused.
  const owedOnRefusal = new Set<string>();
  // Whether a window focus while the live feed was down came while a write of the issue was out:
  // its ask for a refused tree or view runs once the last write of the issue settles (`settle`).
  let refusedHeld = false;
  // The turn of the event loop each read last began in, numbered as this subscription sees
  // turns, and the turn the refresh was queued in: its first change's, or, for a refresh held
  // back, the one the last write out settled in.
  let turn = 0;
  let turnOpen = false;
  let changedIn = 0;
  const beganIn = new Map<string, number>();
  const turnNow = (): number => {
    if (!turnOpen) {
      turnOpen = true;
      turn += 1;
      queueMicrotask(() => {
        turnOpen = false;
      });
    }
    return turn;
  };
  // The turn the tree's last read began in, and the data each query of the issue held then.
  let treeBegan = 0;
  let seen = new Map<string, unknown>();
  // The turn each query of the issue's last read began in, or 0 for one whose answer the tree's
  // read answers already: a read the refresh began, or a focus while the live feed was up.
  const began = new Map<string, number>();
  // The queries of the issue whose last read, not the refresh's own, began while the query was
  // marked invalid: by its last read's failure, or by an invalidation the subscription heard or
  // one TanStack reports as nothing - a write's that lands while the query is invalid already,
  // after a failed read or an owed refresh (`owe`) - which that read may be the only sign of.
  const readWhileInvalid = new Set<string>();
  // A read or an invalidation while a write was out is judged against the data the query held
  // when the first write of the hold went out, or, once a tree read began with no write of the
  // issue out, as it began (`heldData`), as deep as the answer goes: a rollback that puts the data
  // back by `setQueryData` keeps no object of what it puts back, and a read answering the same is
  // then no change.
  const answeredNewer = (query: Query): boolean => {
    const hash = query.queryHash;
    if ((began.get(hash) ?? 0) <= treeBegan) return false;
    let held: unknown;
    let now: unknown = query.state.data;
    if (judged.has(hash)) {
      if (!heldData.has(hash)) return readWhileInvalid.has(hash);
      held = heldData.get(hash);
      now = replaceEqualDeep(held, now);
    } else {
      if (readWhileInvalid.has(hash)) return true;
      if (!seen.has(hash)) return false;
      held = seen.get(hash);
    }
    return matchQuery(issueRead, query) ? !sameActs(held, now) : held !== now;
  };
  // Asks the scheduler for `targets`, each tree and view query among them first recorded as asked
  // for again (`askedAgain`), so a new observer may read it though its last read was refused. The
  // read that answers the ask clears the record as it begins: the scheduler's own where something
  // observes the query, and where nothing does, the one its next observer sends, since the
  // scheduler only marks stale a query nothing reads. What the call raises is the subscription's
  // own (`calling`): a read the scheduler begins in it, and the stale mark it puts on one it holds
  // back.
  const ask = (targets: readonly RefreshTarget[]): void => {
    for (const target of targets) {
      const query = treeAndViews.includes(target) ? cache.find(target) : undefined;
      if (query !== undefined) askedAgain.add(query);
    }
    calling = true;
    try {
      refreshQueries(queryClient, targets);
    } finally {
      calling = false;
    }
  };
  // The moment the scheduler begins a read of `query` at once: a second after its last read
  // began. For a read that began before this subscription did, its last answer, which came after
  // it began, stands in for that start.
  const dueOf = (query: Query): number =>
    (startedAt.get(query.queryHash) ??
      Math.max(query.state.dataUpdatedAt, query.state.errorUpdatedAt)) + MIN_REFETCH_INTERVAL_MS;
  // The reads of the issue an invalidation reached while their read was out: the scheduler, or
  // the app's client, reads each once more as that read lands, until a read of it begins.
  const owedAfterOut = new Set<string>();
  // Whether a read of the issue owed so is still out, which the tree's held ask waits for until it
  // lands (`arm`). Each hash is entered only for a query of the issue, and leaves with its query.
  const owedOut = (): boolean => {
    for (const hash of owedAfterOut) {
      const one = cache.get(hash);
      if (one?.isActive() && one.state.fetchStatus !== "idle") return true;
    }
    return false;
  };
  // The moments the reads of the issue the scheduler owes are due at, each at or after `since`: a
  // query of the issue something observes, with no read out, which the scheduler holds back until
  // a second after its last read began - one marked invalid and answered, or one owed once more
  // as its read out landed (`owedAfterOut`), answered or refused, since the scheduler reads it
  // again either way. Any other query in error is owed nothing: nothing reads it until what reads
  // it again comes. One due before `since` and not begun is no read the scheduler holds back,
  // since it begins at once a read that is due.
  const owedDues = (since: number): number[] =>
    issueQueries.flatMap((filters) =>
      cache.findAll(filters).flatMap((one) => {
        const { fetchStatus, isInvalidated, status } = one.state;
        const owed =
          one.isActive() &&
          fetchStatus === "idle" &&
          (owedAfterOut.has(one.queryHash) || (isInvalidated && status === "success"));
        const due = dueOf(one);
        return owed && due >= since ? [due] : [];
      })
    );
  // Whether the tree's held ask still waits for a read of the issue the scheduler owes: one out
  // that is owed once more as it lands, or one due at or after `since` and not begun.
  const treeWaits = (since: number): boolean => owedOut() || owedDues(since).length > 0;
  // Asks for a held read once the scheduler begins it at once, so the read begins inside this
  // subscription's own call; never while a read of it is out. The tree's waits besides until
  // every read of the issue the scheduler owes has begun - an owed one still out until it lands,
  // when it waits again (the subscription's own re-arm on each answer of the issue): one that
  // began after the tree's would count as newer (`answeredNewer`), though the tree read answers
  // the change it pays. Where one due by the moment the timer fires has not begun yet - its timer
  // is due in the same millisecond - the ask waits once more, now counting only what is due from
  // then.
  const arm = (target: RefreshTarget): void => {
    const heldAsk = heldAsks.get(target);
    if (heldAsk === undefined || heldAsk.timer !== undefined) return;
    const query = cache.find(target);
    if (query?.state.fetchStatus !== "idle") return;
    if (target === tree && owedOut()) return;
    const owed = target === tree ? owedDues(heldAsk.since) : [];
    heldAsk.timer = window.setTimeout(
      () => {
        heldAsk.timer = undefined;
        if (heldAsks.get(target) !== heldAsk) return;
        const read = cache.find(target);
        if (
          read !== undefined &&
          (Date.now() < dueOf(read) || (target === tree && treeWaits(heldAsk.since)))
        ) {
          heldAsk.since = Date.now();
          arm(target);
          return;
        }
        heldAsks.delete(target);
        ask([target]);
      },
      Math.max(0, Math.max(dueOf(query), ...owed) - Date.now())
    );
  };
  // Lets a held read go: a read of it began, which answers the refresh's ask whoever began it, or
  // the reader left, and the scheduler takes the ask (`hand`).
  const drop = (target: RefreshTarget, hand: boolean): void => {
    const heldAsk = heldAsks.get(target);
    if (heldAsk === undefined) return;
    window.clearTimeout(heldAsk.timer);
    heldAsks.delete(target);
    if (hand) ask([target]);
  };
  const refresh = (): void => {
    queued = false;
    if (stopped) return;
    // No event says when a write has reached Dispatch, and a tree read made while one is out
    // reads a tree Dispatch may not have changed yet, so no refresh begins until the last write
    // of the issue out settles, with the live feed up or down, and every change heard meanwhile
    // is the one refresh it runs.
    if (holding()) {
      heldBack = true;
      return;
    }
    // A read begun in the turn the change was seen in answers it already, since what a turn
    // hears of changed before the turn began and the read reaches the server after it: a
    // whole-cache refresh reads the tree and the open view itself, and they are not asked again.
    // A tree or view read the writes held counts as begun in the turn they settled in, which is
    // the held refresh's (`unmutate`).
    const targets = [tree, ...views].filter((target) => {
      const query = cache.find(target);
      return query === undefined || beganIn.get(query.queryHash) !== changedIn;
    });
    // The tree read is held here wherever a read of the issue the scheduler owes is still to
    // begin - the comments, asks, issue or messages read an event marked stale, idle and held
    // back a second after its last read began, or out as the event landed and so owed once more
    // as it lands (`owedOut`) - whether or not the scheduler would hold the tree itself: marked
    // stale now, as the scheduler marks a read it owes, and asked for once the scheduler begins
    // it at once (`arm`). A read of the issue begun after the tree's, still marked invalid or
    // answering other than the tree's read saw, counts as newer (`answeredNewer`), so one event
    // would read the tree twice. Elsewhere the scheduler holds the tree as it would. A change
    // heard while it is held joins that one held ask.
    const holds = [tree].filter((target) => {
      const read = targets.includes(target) ? cache.find(target) : undefined;
      if (read === undefined || !read.isActive()) return false;
      if (heldAsks.has(target)) return true;
      return treeWaits(Date.now());
    });
    ask(targets.filter((target) => !holds.includes(target)));
    for (const target of holds) {
      if (!heldAsks.has(target)) heldAsks.set(target, { since: Date.now(), timer: undefined });
      calling = true;
      try {
        cache.find(target)?.invalidate();
      } finally {
        calling = false;
      }
      arm(target);
    }
  };
  // Queues one refresh for a change seen now, which every change of this turn joins.
  const queue = (): void => {
    if (queued) return;
    queued = true;
    changedIn = turnNow();
    queueMicrotask(refresh);
  };
  const unsubscribe = cache.subscribe((event) => {
    const { query } = event;
    const hash = query.queryHash;
    if (event.type === "added") {
      // A tree or view query made while a write holds the issue's is held too, once it holds an
      // answer or a refusal (`mayReadNow`).
      if (holding() && matchQuery(treeQueries, query)) heldByWrites.add(query);
      return;
    }
    if (event.type === "removed") {
      began.delete(hash);
      beganIn.delete(hash);
      readWhileInvalid.delete(hash);
      startedAt.delete(hash);
      judged.delete(hash);
      heldData.delete(hash);
      owedReads.delete(hash);
      owedOnRefusal.delete(hash);
      owedAfterOut.delete(hash);
      if (matchQuery(tree, query)) watcher.release();
      return;
    }
    if (event.type !== "updated") return;
    const { action } = event;
    if (matchQuery(treeQueries, query)) {
      if (action.type !== "fetch") {
        // A refresh asked for while a write holds the read sends nothing (`mayReadNow`): the
        // stale mark it puts on the query is the ask, owed until the last write settles.
        if (action.type === "invalidate" && holding()) owedReads.add(hash);
        // A view a write's own callback dropped (`dropViews`) while the writes hold it, after a
        // read of it, is a refresh: held as an answered one (`droppedWhileHeld`, `mayReadNow`) and
        // owed until the last write settles. The drop marks it before the reset, which TanStack
        // tells of before it asks for the reads it reset, so none is sent.
        if (action.type === "setState" && droppedWhileHeld.has(query)) owedReads.add(hash);
        // A read a focus or the feed's return found out has ended: where it ended refused it is
        // asked for once more, outside this notification, since the cache is still dispatching
        // the action that ended it.
        if (owedOnRefusal.has(hash) && query.state.fetchStatus === "idle") {
          owedOnRefusal.delete(hash);
          if (action.type === "error") {
            const target = treeAndViews.find((one) => matchQuery(one, query));
            if (target !== undefined) {
              // An ask a write holds sends nothing, and is owed at its settle.
              if (holding()) owedReads.add(hash);
              queueMicrotask(() => {
                if (!stopped) ask([target]);
              });
            }
          }
        }
        // A read the writes hold refused: its next ask is heard (`hearAsks`), once the cache has
        // told every listener of the refusal.
        if (action.type === "error" && heldByWrites.has(query)) {
          queueMicrotask(() => {
            if (holding() && heldByWrites.has(query)) hearAsks(query);
          });
        }
        // The held tree read is asked for once no read of it is out.
        if (heldAsks.has(tree) && matchQuery(tree, query)) arm(tree);
        return;
      }
      // A read begins, which answers whatever asked for the tree or the view again. One begun
      // while a write of the issue is out - a first load, a view's first filter press, the
      // reader's Retry, which a write holds none of - reads a tree Dispatch may not have changed
      // yet, and is owed one more read once the last write settles.
      askedAgain.delete(query);
      const now = turnNow();
      beganIn.set(hash, now);
      if (holding()) owedReads.add(hash);
      if (!matchQuery(tree, query)) return;
      recordStart(hash);
      // A read of the tree begun since a refresh held its ask answers that ask, whoever began it.
      if (!calling) drop(tree, false);
      treeBegan = now;
      seen = issueData();
      // The re-base: a tree read begun with no write of the issue out - the settle's own, after a
      // hold - answers every change the hold held, so an answer of the issue still to be judged
      // (`judged`) - a read the writes' invalidations owe, begun since, as the comments read the
      // event feed's refresh of every query owes after an incomplete replay during the hold - is
      // judged against what the issue held as this read began, not as the writes went out, and
      // the tree is not read a second time for it. The rule table's two rows of a write that lands
      // with no answer while a second is out and the live feed comes back in part (the tree, and
      // the Spec view with its filter on) hold it.
      if (!holding() && judged.size > 0) heldData = new Map(seen);
      return;
    }
    if (!issueQueries.some((filters) => matchQuery(filters, query))) return;
    if (action.type === "fetch") {
      recordStart(hash);
      // A read begins, which pays a read owed once its last landed.
      owedAfterOut.delete(hash);
      const own = calling || focusing;
      const now = turnNow();
      began.set(hash, own ? 0 : now);
      beganIn.set(hash, now);
      // The fetch has not cleared the mark: only the answer's success does.
      if (!own && query.state.isInvalidated) readWhileInvalid.add(hash);
      else readWhileInvalid.delete(hash);
      if (holding()) judged.add(hash);
      return;
    }
    // The held tree read waits again once a read of the issue lands (`arm`).
    arm(tree);
    if (calling || (action.type !== "invalidate" && action.type !== "success")) return;
    if (action.type === "invalidate") {
      // An invalidation is a change, while a write of the issue is out too: an event of the issue
      // may refresh reads that answer what they held - another person's new version of the plan
      // reads the comments, coverage and results again, not the tree - so the trigger is the
      // change, not its answer, and the hold reads the tree once as the last write settles. So a
      // refused write's rollback, whose invalidation is no other, reads it once there too. The
      // read the invalidation brings is still judged (`judged`), so its answer, landing after that
      // settle, reads the tree no second time.
      // Marked stale while its read is out, the read is owed once more as that one lands.
      if (query.state.fetchStatus !== "idle") owedAfterOut.add(hash);
      if (holding()) judged.add(hash);
      queue();
      return;
    }
    // A write the page put in the cache reads the tree only while the live feed is down: while it
    // is up, the write's event does.
    const changed = action.manual === true ? !liveFeedUp(queryClient) : answeredNewer(query);
    if (action.manual !== true) judged.delete(hash);
    if (changed) queue();
  });
  // Takes the tree and each view whose read was refused and is not out, to ask for again: a window
  // focus while the live feed is down does, once no write of the issue is out, and the feed coming
  // back with its gap replayed whole. A refused view whose filter is off then is read when its
  // filter is next on, since the ask records it as asked for again (`ask`). A read out then is
  // owed as it is taken (`owedOnRefusal`), whoever asks for the rest, and asked for once more
  // where it ends refused.
  const takeRefused = (): RefreshTarget[] => {
    const refused: RefreshTarget[] = [];
    for (const target of treeAndViews) {
      const query = cache.find(target);
      if (query === undefined) continue;
      if (query.state.fetchStatus !== "idle") owedOnRefusal.add(query.queryHash);
      else if (query.state.status === "error") refused.push(target);
    }
    return refused;
  };
  const askRefused = (): void => {
    const refused = takeRefused();
    if (refused.length > 0) ask(refused);
  };
  // A window focus while the live feed is down reads a refused tree or view again, or, while a
  // write of the issue is out, once the last settles (`refusedHeld`), since a read made before
  // then reads a tree Dispatch has not changed yet; while the feed is up it reads neither, and
  // what the reads of the issue it begins answer is no change.
  const unfocus = focusManager.subscribe((focused) => {
    if (!focused || stopped) return;
    if (liveFeedUp(queryClient)) {
      focusing = true;
      setTimeout(() => {
        focusing = false;
      }, 0);
      return;
    }
    if (holding()) refusedHeld = true;
    else askRefused();
  });
  // The live feed coming back after a gap with the gap replayed whole reads a refused tree or
  // view again: no replayed event need name the issue. After an incomplete replay the event feed's
  // refresh of every query reads each refused one something observes, and an ask here as well
  // would read it twice; one nothing observes that refresh only marks stale, so the subscription
  // asks for it, which reads nothing and records it as asked for again (`ask`) for its next
  // observer, as no change of the issue does while a write is out.
  const unfeed = onLiveFeedBack(queryClient, (replayComplete) => {
    if (replayComplete) {
      askRefused();
      return;
    }
    const unobserved = treeAndViews.filter((target) => {
      const query = cache.find(target);
      return query?.state.status === "error" && !query.isActive();
    });
    if (unobserved.length > 0) ask(unobserved);
  });
  // The refused tree and view reads a write holds, marked valid while it holds them
  // (`hearAsks`): TanStack marks a refused read stale at once, and marks no stale query stale
  // again nor tells of it, so a refresh of every query - the write store's for a write that lands
  // with no answer, the event feed's after an incomplete replay - would ask for a held refused
  // read unheard. Marked valid, its next stale mark is heard and owed (`owedReads`), and each is
  // marked stale again as the hold ends. The mark is set on the query's state with no notice to
  // its observers: none draws whether a read is stale, a disabled read is never stale to its
  // observers (`mayReadNow`), and a notice would draw every surface of it again for nothing.
  const markedValid = new Set<Query>();
  const hearAsks = (query: Query): void => {
    const { fetchStatus, isInvalidated, status } = query.state;
    if (status !== "error" || !isInvalidated || fetchStatus !== "idle") return;
    markedValid.add(query);
    query.state = { ...query.state, isInvalidated: false };
  };
  // Holds the tree and each view of the issue (`heldByWrites`). With `oweReadsOut`, each read out
  // now is owed one more read too (`owedReads`): a subscription that begins while a write of the
  // issue is out finds the first load out, which the stage sends as it mounts, before it holds
  // this subscription, and so sent while the write was out.
  const holdReads = (oweReadsOut = false): void => {
    for (const query of cache.findAll(treeQueries)) {
      heldByWrites.add(query);
      hearAsks(query);
      if (oweReadsOut && query.state.fetchStatus !== "idle") owedReads.add(query.queryHash);
    }
  };
  // Lets the tree and each view of the issue go, and marks stale again each refused read the
  // hold marked valid (`hearAsks`) that nothing asked for meanwhile.
  const releaseReads = (): void => {
    for (const query of cache.findAll(treeQueries)) {
      heldByWrites.delete(query);
      droppedWhileHeld.delete(query);
    }
    for (const query of markedValid) {
      const { isInvalidated, status } = query.state;
      if (cache.get(query.queryHash) === query && status === "error" && !isInvalidated) {
        query.state = { ...query.state, isInvalidated: true };
      }
    }
    markedValid.clear();
  };
  // A subscription that begins while a write of the issue is out holds its reads at once.
  if (writing) holdReads(true);
  // The hold's end, a microtask after the last write of the issue settled, so a write begun in
  // the mutation cache's notification of that settle keeps the hold (`unmutate`). It lets the
  // tree and its views go, and reads what the writes held, answered or refused: the refresh held
  // back, which asks for the tree and every view, or else each tree and view read owed - sent or
  // asked for while a write was out (`owedReads`), or marked stale meanwhile and readable by a
  // new observer (`mayReadOnMount`) - with the ask of a focus while the live feed is down, where
  // it is still down: where it came back meanwhile, its return asked for a refused tree or view
  // already - at once after a whole replay, through the event feed's refresh of every query after
  // an incomplete one - and a focus while the feed is up reads none. Each is asked for once, so
  // one observed is read once, and one nothing observes is read when something next does.
  const settle = (): void => {
    if (!settling) return;
    settling = false;
    if (stopped) return;
    releaseReads();
    for (const hash of heldData.keys()) {
      if (!judged.has(hash)) heldData.delete(hash);
    }
    const refused = refusedHeld && !liveFeedUp(queryClient) ? takeRefused() : [];
    refusedHeld = false;
    if (heldBack || queued) {
      owedReads.clear();
      heldBack = false;
      queue();
      return;
    }
    const owed = treeAndViews.filter((target) => {
      const query = cache.find(target);
      if (query === undefined) return false;
      return owedReads.has(query.queryHash) || (query.state.isInvalidated && mayReadOnMount(query));
    });
    owedReads.clear();
    const targets = [...new Set([...owed, ...refused])];
    if (targets.length > 0) ask(targets);
  };
  // The hold: the first write of the issue going out while none was begins it, with the data each
  // query of the issue then holds (`heldData`), and holds the tree and its views
  // (`heldByWrites`); the last settling, answered or refused, ends it (`settle`). A write is the
  // issue's as the mutation cache tells of it (`names`, the tree observed then, or
  // `lateOwnSend`).
  const unmutate = queryClient.getMutationCache().subscribe((event) => {
    // Only a mutation added, removed or updated changes which are pending.
    if (event.type !== "added" && event.type !== "removed" && event.type !== "updated") return;
    const { mutation } = event;
    if (event.type === "added") {
      if (
        !names(mutation) &&
        !lateOwnSend(mutation) &&
        (cache.find(tree)?.getObserversCount() ?? 0) === 0
      ) {
        return;
      }
      ownWrites.add(mutation);
    } else {
      if (!ownWrites.has(mutation)) return;
      const { status } = mutation.state;
      if (event.type === "removed" || status === "success" || status === "error") {
        ownWrites.delete(mutation);
      }
    }
    const out = ownWriteOut();
    if (out === writing) return;
    writing = out;
    if (writing) {
      // A write of the issue begun as the last settled: the hold goes on.
      if (settling) {
        settling = false;
        return;
      }
      heldData = issueData();
      holdReads();
      return;
    }
    settling = true;
    queueMicrotask(settle);
  });
  const watcher: TreeFreshness = {
    holders: 0,
    release: () => {
      if (stopped) return;
      // The reader left: each read held for a refresh goes to the scheduler.
      if (watcher.holders === 0) for (const target of [...heldAsks.keys()]) drop(target, true);
      if (watcher.holders > 0 || cache.find(tree) !== undefined) return;
      stopped = true;
      releaseReads();
      unsubscribe();
      unfocus();
      unfeed();
      unmutate();
      byKey.delete(key);
    },
  };
  byKey.set(key, watcher);
  return watcher;
}
