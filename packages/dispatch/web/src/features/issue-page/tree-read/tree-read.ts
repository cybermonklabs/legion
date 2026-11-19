import {
  focusManager,
  matchQuery,
  type Query,
  type QueryClient,
  type QueryFilters,
  queryOptions,
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
import { issueTreeKey } from "../../../api/queries";
import { type RefreshTarget, refreshQueries } from "../../../api/query-refresh";
import type { IssueDetails } from "../../../api/types";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { refetchFailedOnFocus } from "../../refs/reference-target";
import { writesOf } from "../../writes/write-store";
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

/** The filters that read a view of the tree, in this order. Decisions reads none: its view is
 *  the decisions read (`issueDecisionsQuery`), which the load reads already. */
export const TREE_FILTERS: readonly TreeFilter[] = ["spec", "verification", "plan"];

/** One view's key: under the tree's (`issueTreeKey`), so every event of the issue reaches it
 *  too (`event-refresh.ts`). */
export function viewKey(
  key: string,
  filter: TreeFilter
): readonly ["issue-tree", string, TreeFilter] {
  return [...issueTreeKey(key), filter] as const;
}

/** Resets every view of issue `key` the cache holds (`resetQueries`) for a load of the issue
 *  (`ViewReadProvider`), so a view a past load held is gone and each is read once: the open one at
 *  the load, at once, whether a write of the issue is out or not, and another when its filter is
 *  next on. It marks none, and clears the mark a write's drop put on a view (`droppedWhileHeld`):
 *  after a load the view is a load's first read. The issue page calls only this one.
 *  `subscribeTreeFreshness` resets none: it refreshes each held view through `refreshQueries`,
 *  and the view keeps what it draws until its new answer lands. */
export function resetViews(queryClient: QueryClient, key: string): void {
  for (const filter of TREE_FILTERS) {
    const queryKey = viewKey(key, filter);
    const view = queryClient.getQueryCache().find({ exact: true, queryKey });
    if (view === undefined) continue;
    droppedWhileHeld.delete(view);
    void queryClient.resetQueries({ exact: true, queryKey });
  }
}

/** Drops every view of issue `key` for a write's own callback, as Dispatch answers the write,
 *  after which a held view could be older than the tree the page draws. Its callers would be a
 *  Settle, a Keep reading the decisions again and a Change; no surface calls it yet - the Keep and
 *  the Change of `DecisionActions.tsx` do not - and the issue page calls only `resetViews`. The
 *  write whose callback drops them has landed, so a view it drops is read at once, as a load's;
 *  but one it drops after a read of it while another write of the issue is out, which holds it, is
 *  a refresh: held as an answered one and read once as the last write settles
 *  (`droppedWhileHeld`, `mayReadNow`). One never read reads at once. */
export function dropViews(queryClient: QueryClient, key: string): void {
  for (const filter of TREE_FILTERS) {
    const queryKey = viewKey(key, filter);
    const view = queryClient.getQueryCache().find({ exact: true, queryKey });
    if (view === undefined) continue;
    if (heldByWrites.has(view)) {
      const { fetchStatus, status } = view.state;
      if (status !== "pending" || fetchStatus !== "idle") droppedWhileHeld.add(view);
    }
    void queryClient.resetQueries({ exact: true, queryKey });
  }
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
  view: TreeFilter | undefined,
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

/** Whether a new observer of the tree or of a view may read it (`TREE_FRESH`): where its last
 *  read was not refused, or where something asked for it again since that refusal - an event of
 *  the issue, a refresh of every query, the page's subscription - which marked it stale. The
 *  subscription marks each refused read valid as it is refused (`subscribeTreeFreshness`), so a
 *  refused read is stale only once it is asked for again. It takes the query by its state, so the
 *  tree's query and each view's, each typed by its own answer, take the one predicate. */
function mayReadOnMount(query: Pick<Query, "state">): boolean {
  return query.state.status !== "error" || query.state.isInvalidated;
}

/** The tree and view queries a write of their issue holds, from the first such write going out
 *  to the last settling, as the page's subscription records them (`subscribeTreeFreshness`). Held
 *  as plain objects, since each query is typed by its own key and answer. */
const heldByWrites = new WeakSet<object>();

/** The views a write's own callback dropped (`dropViews`) while another write of their issue held
 *  them, after a read of them - answered, refused or still out - until the last write settles or
 *  a load resets them (`resetViews`). A view never read that a drop resets holds nothing either
 *  way, and its first read is still sent at once. Held as plain objects, as `heldByWrites` is. */
const droppedWhileHeld = new WeakSet<object>();

/** Whether TanStack may read the tree or a view of its own accord or for a refresh (`TREE_FRESH`'s
 *  `enabled`): always for a read that holds neither an answer nor a refusal yet - a first load, a
 *  view's first filter press, one a load reset (`resetViews`) - and for any other only while no
 *  write of its issue holds it (`heldByWrites`), since a read sent then reads a tree Dispatch may
 *  not have changed yet. A view a write's own callback dropped after a read of it, while another
 *  write holds it (`dropViews`, `droppedWhileHeld`), holds nothing either, but is a refresh, held
 *  as an answered one. A disabled query is no query of a refresh: the app's client refetches none
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
 *  while a write of the issue holds it (`mayReadNow`). A refused read, whether it holds an answer
 *  or not, stays refused until the reader's Retry, an event of the issue, the live feed coming
 *  back, or a window focus while the feed is down: every event of the issue names the tree's key
 *  (`event-refresh.ts`), and the page's one subscription asks for it on the other two
 *  (`subscribeTreeFreshness`). So no window focus reads one of TanStack's own accord, nor does the
 *  browser saying the network is back, which it may say while the feed never went; and a new
 *  observer - a slot drawn, a surface opened, a filter pressed, the reader coming back to the
 *  issue - reads a refused one only where it was asked for again since its refusal
 *  (`mayReadOnMount`, as both `retryOnMount`, for a read that holds no answer, and
 *  `refetchOnMount`, for one that holds an answer). The views are owed besides at each load of
 *  the issue, answered or refused: a load resets them (`resetViews`), so each is read once, the
 *  open one at the load, at once, with a write of the issue out or none, and another when its
 *  filter is next on. A read that holds an answer which a write or an event marked stale while
 *  nothing observed it is read when something next observes it, and a read never made is made. */
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
 *  stale, and, where its read was refused, only where it was asked for again since. A filter
 *  turned off and on again asks for nothing, so it reads a refused view no more. */
export const viewQuery = (key: string, filter: TreeFilter) =>
  queryOptions({
    ...TREE_FRESH,
    queryKey: viewKey(key, filter),
    queryFn: ({ signal }) => readTree(key, filter, signal),
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
 * Keeps the tree read and its views as fresh as the issue's other reads. Two signals tell it what
 * can change them, and nothing is judged from the issue's other reads:
 *
 * - every event of the issue names the tree's key, and with it each view's (`event-refresh.ts`):
 *   the app's refresh scheduler reads the tree and each open view once for it (`refreshQueries`),
 *   which never cancels a read in flight - a walk of many pages is not started over by each event
 *   of a burst - reads a query that is out once more when it settles, and starts one at most once
 *   a second, and marks stale a view no filter has on, which is read when its filter is next on;
 * - every write of the issue the write store makes - one whose request's path names the issue,
 *   or whose surface names it (`WriteSpec.issue`) - holds the tree and its views from going out to
 *   landing or refusal (`onIssueWrite`). A write of another issue, or one naming none, holds
 *   nothing here.
 *
 * No read of the tree or of a view is sent while a write of the issue is out, with the live feed
 * up or down: no event says when a write has reached Dispatch, and a tree read made then reads a
 * tree Dispatch may not have changed yet. While one is out the tree and each view that holds an
 * answer or a refusal is disabled (`mayReadNow`), so the app's client, the scheduler and a new
 * observer send none of it; a first load, a view's first filter press and the reader's Retry are
 * sent at once, and each is owed one more read once the last write settles. An ask of a held read
 * - an event's, a refresh of every query, the subscription's own - only marks it stale, and is
 * owed at that settle too (`owedReads`); so is a view a write's own callback drops while another
 * write of the issue is out (`dropViews`). The hold ends a microtask after the last write landed
 * or was refused, so a write that goes out as it settles keeps it (`settle`), and each owed read
 * is asked for then, once: one observed is read once, and one nothing observes when something
 * next does. With the live feed down nothing tells of what the writes did, nor of an event lost
 * meanwhile, so that settle asks for the tree, every view and the decisions read besides; with the
 * feed up a write's event tells, and a write Dispatch refused, which changed nothing, reads
 * nothing. A surface's callback that runs once its write has landed (TanStack's `onSuccess`)
 * runs after the hold ended.
 *
 * A refused tree or view read, its retries spent, with or without an answer held, stays refused
 * until the reader's Retry, an event of the issue, the live feed coming back or a window focus
 * while the feed is down. TanStack marks a refused read stale, and tells of no stale mark put on a
 * query already stale, so the subscription marks each refused read valid once it is refused
 * (`markValid`): the next ask marks it stale again, heard, which is what lets a new observer read
 * it (`mayReadOnMount`) and what a hold owes. A window focus asks for each refused one only while
 * the feed is down (`liveFeedUp`) and no write of the issue is out, whose settle with the feed
 * down asks for the tree and every view itself; the feed coming back with its gap replayed whole
 * asks for each at once (`onLiveFeedBack`), since no replayed event need name the issue, and
 * after an incomplete replay the event feed's refresh of every query does. A read out as a focus
 * or a whole replay asks is owed: where it then ends refused it is asked for once more
 * (`owedOnRefusal`).
 *
 * It is one subscription for each client and issue, which outlives the stage that asks for it:
 * kept while a stage holds it, and after, for as long as the cache holds the issue's tree.
 * Answers the release of this stage's hold.
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

/** The subscription of issue `key`, entered in `byKey` until it ends. */
function watchTree(
  queryClient: QueryClient,
  key: string,
  byKey: Map<string, TreeFreshness>
): TreeFreshness {
  const cache = queryClient.getQueryCache();
  const writes = writesOf(queryClient);
  const tree: RefreshTarget = { exact: true, queryKey: issueTreeKey(key) };
  const decisions: RefreshTarget = {
    exact: true,
    queryKey: issueDecisionsQuery(key, true).queryKey,
  };
  const views: readonly RefreshTarget[] = TREE_FILTERS.map((filter) => ({
    exact: true,
    queryKey: viewKey(key, filter),
  }));
  const treeAndViews: readonly RefreshTarget[] = [tree, ...views];
  // The tree's query and each view's, by the prefix they share.
  const treeQueries: QueryFilters = { queryKey: issueTreeKey(key) };
  let stopped = false;
  // The writes of the issue out, by id, as the write store tells of them (`onIssueWrite`): each
  // from going out to landing, refusal or being let go.
  const writesOut = new Set(writes.writesOutOf(key).map((write) => write.id));
  // Whether the hold's end waits for its microtask (`settle`).
  let settling = false;
  const holding = (): boolean => writesOut.size > 0 || settling;
  // The tree and view reads owed one read once the last write of the hold settles: each sent
  // while a write was out - a first load, a view's first filter press, the reader's Retry - and
  // each asked for while the write held it (`mayReadNow`), which sent nothing.
  const owedReads = new Set<string>();
  // The tree and view reads out when a window focus while the live feed was down, or the feed back
  // with its gap replayed whole, asked for every refused one (`askRefused`): each is asked for
  // once more where it ends refused.
  const owedOnRefusal = new Set<string>();
  // Whether the last write of the hold landed with the store refreshing every query
  // (`onIssueWrite`'s `refreshedAll`), which read every read the hold did not hold.
  let refreshedAll = false;
  // Marks a refused read valid, so its next ask is heard: TanStack marks a refused read stale,
  // and tells of no stale mark put on a query already stale, so an event's ask, or a refresh of
  // every query, of a refused read nothing observes would leave its next observer nothing to read
  // (`mayReadOnMount`), and one a write holds would not be owed. The mark is set on the query's
  // state with no notice to its observers: none draws whether a read is stale, and a notice would
  // draw every surface of it again for nothing.
  const markValid = (query: Query): void => {
    const { fetchStatus, isInvalidated, status } = query.state;
    if (status !== "error" || !isInvalidated || fetchStatus !== "idle") return;
    query.state = { ...query.state, isInvalidated: false };
  };
  const unsubscribe = cache.subscribe((event) => {
    const { query } = event;
    if (!matchQuery(treeQueries, query)) return;
    const hash = query.queryHash;
    if (event.type === "added") {
      // A tree or view query made while a write holds the issue's is held too, once it holds an
      // answer or a refusal (`mayReadNow`).
      if (holding()) heldByWrites.add(query);
      return;
    }
    if (event.type === "removed") {
      owedReads.delete(hash);
      owedOnRefusal.delete(hash);
      if (matchQuery(tree, query)) watcher.release();
      return;
    }
    if (event.type !== "updated") return;
    const { action } = event;
    // A read begun while a write of the issue is out - a first load, a view's first filter press,
    // the reader's Retry, which a write holds none of - reads a tree Dispatch may not have changed
    // yet, and is owed one more read once the last write settles. An ask of a held read sends
    // nothing (`mayReadNow`): the stale mark it puts on the query is the ask, owed then too.
    if ((action.type === "fetch" || action.type === "invalidate") && holding()) {
      owedReads.add(hash);
    }
    // A view a write's own callback dropped while another write of the issue holds it, after a
    // read of it, is owed the same way (`droppedWhileHeld`). The drop marks it before the reset,
    // which TanStack tells of before it asks for the reads it reset, so none is sent.
    if (action.type === "setState" && droppedWhileHeld.has(query)) owedReads.add(hash);
    // A refused read is marked valid once the cache has told every listener of the refusal.
    if (action.type === "error") queueMicrotask(() => markValid(query));
    // A read a focus or the feed's return found out has ended: where it ended refused it is asked
    // for once more, outside this notification, since the cache is still dispatching the action
    // that ended it, and after it is marked valid, so the ask is heard.
    if (owedOnRefusal.has(hash) && query.state.fetchStatus === "idle") {
      owedOnRefusal.delete(hash);
      const target =
        action.type === "error" ? treeAndViews.find((one) => matchQuery(one, query)) : undefined;
      if (target !== undefined) {
        queueMicrotask(() => {
          if (!stopped) refreshQueries(queryClient, [target]);
        });
      }
    }
  });
  // Takes the tree and each view whose read was refused and is not out, to ask for again: a window
  // focus while the live feed is down does, once no write of the issue is out, and the feed coming
  // back with its gap replayed whole. A refused view whose filter is off then is read when its
  // filter is next on, since the ask marks it stale (`mayReadOnMount`). A read out then is owed as
  // it is taken (`owedOnRefusal`), whoever asks for the rest, and asked for once more where it
  // ends refused.
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
    if (refused.length > 0) refreshQueries(queryClient, refused);
  };
  // A window focus while the live feed is down reads a refused tree or view again; while a write
  // of the issue is out it reads nothing, since a read made then reads a tree Dispatch has not
  // changed yet, and the write's settle with the feed down reads the tree and every view itself.
  // While the feed is up its events tell, and a focus reads neither.
  const unfocus = focusManager.subscribe((focused) => {
    if (!focused || stopped || holding() || liveFeedUp(queryClient)) return;
    askRefused();
  });
  // The live feed coming back after a gap with the gap replayed whole reads a refused tree or
  // view again: no replayed event need name the issue. After an incomplete replay the event feed's
  // refresh of every query asks for each.
  const unfeed = onLiveFeedBack(queryClient, (replayComplete) => {
    if (replayComplete) askRefused();
  });
  // Holds the tree and each view of the issue (`heldByWrites`). With `oweReadsOut`, each read out
  // now is owed one more read too (`owedReads`): a subscription that begins while a write of the
  // issue is out finds the first load out, which the stage sends as it mounts, before it holds
  // this subscription, and so sent while the write was out.
  const holdReads = (oweReadsOut = false): void => {
    for (const query of cache.findAll(treeQueries)) {
      heldByWrites.add(query);
      if (oweReadsOut && query.state.fetchStatus !== "idle") owedReads.add(query.queryHash);
    }
  };
  // Lets the tree and each view of the issue go.
  const releaseReads = (): void => {
    for (const query of cache.findAll(treeQueries)) {
      heldByWrites.delete(query);
      droppedWhileHeld.delete(query);
    }
  };
  // A refused read from before this subscription began is marked valid as one refused since is,
  // and a subscription that begins while a write of the issue is out holds its reads at once.
  for (const query of cache.findAll(treeQueries)) markValid(query);
  if (holding()) holdReads(true);
  // The hold's end, a microtask after the last write of the issue landed or was refused, so a
  // write that goes out in the write store's telling of that settle keeps the hold. It lets the
  // tree and its views go, and asks for what the writes held, each once, so one observed is read
  // once and one nothing observes when something next does: with the live feed down, which tells
  // nothing of what the writes did nor of an event lost meanwhile, the tree, every view and the
  // decisions read - the decisions not where the last write's landing refreshed every query,
  // which read them already; with it up, each tree and view read owed - sent or asked for while
  // a write was out (`owedReads`), or marked stale meanwhile - and nothing for a write that
  // changed nothing.
  const settle = (): void => {
    if (!settling) return;
    settling = false;
    if (stopped) return;
    releaseReads();
    const owed = !liveFeedUp(queryClient)
      ? [tree, ...(refreshedAll ? [] : [decisions]), ...views]
      : treeAndViews.filter((target) => {
          const query = cache.find(target);
          if (query === undefined) return false;
          return owedReads.has(query.queryHash) || query.state.isInvalidated;
        });
    owedReads.clear();
    if (owed.length > 0) refreshQueries(queryClient, owed);
  };
  // The hold, as the write store tells of the issue's writes: the first going out while none is
  // begins it and holds the tree and its views (`heldByWrites`); the last landing, refused or let
  // go ends it (`settle`).
  const unwrite = writes.onIssueWrite(key, (phase, write, refreshed) => {
    if (phase === "out") {
      const begins = !holding();
      writesOut.add(write.id);
      // A write of the issue that goes out as the last settles: the hold goes on.
      settling = false;
      if (begins) holdReads();
      return;
    }
    if (!writesOut.delete(write.id) || writesOut.size > 0) return;
    refreshedAll = refreshed;
    settling = true;
    queueMicrotask(settle);
  });
  const watcher: TreeFreshness = {
    holders: 0,
    release: () => {
      if (stopped || watcher.holders > 0 || cache.find(tree) !== undefined) return;
      stopped = true;
      releaseReads();
      unsubscribe();
      unfocus();
      unfeed();
      unwrite();
      byKey.delete(key);
    },
  };
  byKey.set(key, watcher);
  return watcher;
}
