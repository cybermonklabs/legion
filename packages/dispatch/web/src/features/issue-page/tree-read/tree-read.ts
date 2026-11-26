import {
  focusManager,
  hashKey,
  matchQuery,
  type Query,
  type QueryClient,
  type QueryFilters,
  type QueryFunctionContext,
  type QueryKey,
  queryOptions,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import { liveFeedUp, onLiveFeedBack } from "../../../api/live-socket";
import type { Query as RouteQuery } from "../../../api/paths";
import {
  CONVERSATION_KINDS,
  conversationTreeKey,
  issueTreeKey,
  TREE_FILTERS,
  type TreeFilter,
} from "../../../api/queries";
import { type RefreshTarget, refreshQueries } from "../../../api/query-refresh";
import type {
  IssueDetails,
  IssueTreeNode,
  IssueTreeRead,
  IssueTreeTuple,
} from "../../../api/types";
import { EVENT_FRESH } from "../../refs/reference-target";
import { parseIssuePath } from "../../refs/routes";
import { writesOf } from "../../writes/write-store";
import { presetOf } from "../issue-address";
import type { Preset } from "../page-words";
import { isFilter } from "../presets";
import {
  blocksOfRole,
  compareCommentPositions,
  mergeCommentPage,
  mergeTreePages,
  nextTreeCursor,
  nodesRemaining,
  olderCommentsBefore,
  type TreeBlock,
} from "./tree-nodes";

/**
 * The issue's tree read (`GET /api/v1/issues/{key}/tree`), which the model serves: the one file
 * of the issue page that names the route. Every other file reads the answer through the names
 * below, which are the generated contract's (`IssueTreeRead`, `IssueTreeNode`, `IssueTreeTuple`).
 */
export type TreeAnswer = IssueTreeRead;
export type TreeAnswerNode = IssueTreeNode;
export type TreeTuple = IssueTreeTuple;

/** Every view of the tree a read asks for (`?view=`), in the order the filters read them: a
 *  role document's, one for each filter (`TREE_FILTERS`, Spec's, Verification's and Plan's),
 *  then the decisions view, the issue's decisions wherever they are written. That view plays no
 *  role, so `blocksOf` takes no such name. */
export const TREE_VIEWS = [...TREE_FILTERS, "decisions"] as const;

/** One view of the tree, a member of `TREE_VIEWS` by its type, so that list holds every view. */
export type TreeView = (typeof TREE_VIEWS)[number];

/** One view's key: under the tree's (`issueTreeKey`), so every event of the issue reaches it
 *  too (`event-refresh.ts`). */
export function viewKey(key: string, view: TreeView): readonly ["issue-tree", string, TreeView] {
  return [...issueTreeKey(key), view] as const;
}

/** Resets every view of issue `key` the cache holds (`resetQueries`) for a load of the issue
 *  (`ViewReadProvider`), so a view a past load held is gone and each is read once: the open one at
 *  the load, at once, whether a write of the issue is out or not, and another when its filter is
 *  next on. It marks none, and clears the mark a write's drop put on a view (`droppedWhileHeld`):
 *  after a load the view is a load's first read. `subscribeTreeFreshness` resets none: it
 *  refreshes each held view through `refreshQueries`, and the view keeps what it draws until its
 *  new answer lands. */
export function resetViews(queryClient: QueryClient, key: string): void {
  for (const name of TREE_VIEWS) {
    const queryKey = viewKey(key, name);
    const view = queryClient.getQueryCache().find({ exact: true, queryKey });
    if (view === undefined) continue;
    droppedWhileHeld.delete(view);
    treePagesOf.delete(view);
    void queryClient.resetQueries({ exact: true, queryKey });
  }
}

/** Drops every view of issue `key` for a write's own callback, as Dispatch answers the write,
 *  after which a held view could be older than the tree the page draws: a Keep, however it ends,
 *  and a Change once Dispatch took it (`DecisionActions.tsx`), each of which reads the tree again
 *  besides. The write whose callback drops them has landed or been refused, so a view it drops is
 *  read at once, as a load's; but one it drops after a read of it while another write of the
 *  issue is out, which holds it, is a refresh: held as an answered one and read once as the last
 *  write settles (`droppedWhileHeld`, `mayReadNow`). One never read reads at once. */
export function dropViews(queryClient: QueryClient, key: string): void {
  for (const name of TREE_VIEWS) {
    const queryKey = viewKey(key, name);
    const view = queryClient.getQueryCache().find({ exact: true, queryKey });
    if (view === undefined) continue;
    if (heldByWrites.has(view)) {
      const { fetchStatus, status } = view.state;
      if (status !== "pending" || fetchStatus !== "idle") droppedWhileHeld.add(view);
    }
    void queryClient.resetQueries({ exact: true, queryKey });
  }
}

/** The page's two outlines (R1, R10): Conversation, which draws Conversation's read - the tree
 *  read with `kind` repeated for `CONVERSATION_KINDS` - and Graph, which draws the tree read with
 *  no view. Each outline's read stands under a key of its own, both under the tree's
 *  (`issueTreeKey`), so every event of the issue reaches both. */
type Outline = "conversation" | "graph";

/** The tree route's query as the page writes it: the route's own (`view`, `kind`, `v`, `cursor`,
 *  `comments_before`). `routeUrl` writes a list as one `kind=` per value (`paths.ts`'
 *  `pathWithQuery`). */
type TreeQuery = RouteQuery<"/api/v1/issues/{key}/tree", "get">;

/** What each outline's read asks the route for, beside a page's `cursor` or `comments_before`.
 *  Listed in the order the tree's subscription asks for them (`subscribeTreeFreshness`). */
const OUTLINE_QUERIES: Readonly<Record<Outline, TreeQuery>> = {
  graph: {},
  conversation: { kind: [...CONVERSATION_KINDS] },
};

/** The key an outline's answer stands under for issue `key`: Graph's is the tree's own key,
 *  Conversation's its own under it. A case for each outline and no default, so an outline added
 *  to `Outline` has no key until it is given one here, and each key is written by its own name,
 *  which the census of query keys reads (`api/corrected-post-families.test.ts`). */
const outlineKey = (outline: Outline, key: string): QueryKey => {
  switch (outline) {
    case "graph":
      return issueTreeKey(key);
    case "conversation":
      return conversationTreeKey(key);
  }
};

/** How far back the older comment pages the reader loaded into an outline read reach, by the
 *  read's query: the `comments_before` its answer held once the last of them was merged, or null
 *  where no older comment remained. A read with no older page loaded has none. The mark is the
 *  query's, not its answer's: the cache keeps a copy of each answer it is given (TanStack's
 *  structural sharing), so a read made again finds it, and reads as far back again (`readTree`).
 *  Held as plain objects, as `heldByWrites` is. */
const commentPagesOf = new WeakMap<object, { readonly reached: string | null }>();

/** How far into its walk a tree read, Conversation's read or a view's has read, by the read's
 *  query, as its last read or the reader's last press of its next page left it
 *  (`readNextTreePage`): how many pages it read (`pages`), how many nodes of the walk they held
 *  (`walked`), and each cursor it asked for (`cursors`). A read made again reads as far
 *  (`readTree`), and a press fails on a cursor the walk asked for already. The mark is the
 *  query's, as `commentPagesOf`'s is, and a load's reset of a view clears it (`resetViews`). */
const treePagesOf = new WeakMap<
  object,
  { readonly cursors: ReadonlySet<string>; readonly pages: number; readonly walked: number }
>();

/**
 * The pages of the tree read, of Conversation's read or of one view, the page holds: on a load
 * the first page alone, which holds the newest comment page (R15), and no next page. A next page
 * is read only where the reader presses the control for the rest or an address names a node no
 * page read yet holds (`readNextTreePage`), and the answer names the page after the last one read
 * while one remains (`mergeTreePages`). A read made again - a refetch, a retry, a write's re-read
 * (R6) - starts from the first page, since a cursor names a place in the walk as it stood when the
 * page that gave it was answered, and where the reader had loaded more than the first it reads the
 * next pages until it holds as many nodes of the walk as those pages held (`treePagesOf`), so no
 * row they had loaded leaves; it looks at that mark again after each page, so a page the reader's
 * press merged while this read was out is read too. An outline read then reads again the older
 * comment pages its answer had loaded (`withLoadedComments`). A page that names a cursor the read
 * has asked for already fails the read with an error that names it (`nextCursorOf`): the server
 * named one page twice, so the pages past it are missing. A page that fails fails the read too. A
 * failure of the tree read with no answer held draws the outline's failed-read line and its
 * Retry, and a view's leaves its filter drawn off the tree (`presets.ts`' `presetView`); over a
 * held answer the page keeps that answer, as TanStack keeps a query's data through a failed
 * refetch. A read TanStack gives up (`signal`: a reset, the last observer gone) ends its page in
 * flight and asks for no next one. It waits for nothing before its first page: what holds a
 * refresh back while a write of the issue is out is the query's `enabled` (`TREE_FRESH`), so a
 * read that is begun is sent.
 */
async function readTree(
  key: string,
  query: TreeQuery,
  { client, queryKey, signal }: Pick<QueryFunctionContext, "client" | "queryKey" | "signal">
): Promise<TreeAnswer> {
  const held = client.getQueryCache().find({ exact: true, queryKey });
  const first = await api.getIssueTree(key, query, { signal });
  const pages: [TreeAnswer, ...TreeAnswer[]] = [first];
  const asked = new Set<string>();
  let walked = first.nodes?.length ?? 0;
  let cursor = nextCursorOf(key, query, first, asked);
  for (;;) {
    const loaded = held === undefined ? undefined : treePagesOf.get(held);
    if (cursor === undefined || loaded === undefined || loaded.pages < 2) break;
    if (walked >= loaded.walked) break;
    asked.add(cursor);
    signal.throwIfAborted();
    const page = await api.getIssueTree(key, { ...query, cursor }, { signal });
    pages.push(page);
    walked += page.nodes?.length ?? 0;
    cursor = nextCursorOf(key, query, page, asked);
  }
  if (held !== undefined) treePagesOf.set(held, { cursors: asked, pages: pages.length, walked });
  const merged = mergeTreePages(pages);
  return query.view === undefined
    ? withLoadedComments(key, query, merged, { client, queryKey, signal })
    : merged;
}

/** The cursor of the page after `page` (`nextTreeCursor`), which the read asks for next: one the
 *  read has asked for already (`asked`) fails it, naming the cursor, since the pages past it are
 *  missing and asking again would read without end. */
function nextCursorOf(
  key: string,
  query: TreeQuery,
  page: TreeAnswer,
  asked: ReadonlySet<string>
): string | undefined {
  const cursor = nextTreeCursor(page);
  if (cursor !== undefined && asked.has(cursor)) {
    throw new Error(
      `${readName(key, query)} named the cursor "${cursor}" twice, so the pages after it are missing.`
    );
  }
  return cursor;
}

/** The read `query` asks for, as its failure names it. */
function readName(key: string, query: TreeQuery): string {
  if (query.view !== undefined) return `The ${query.view} view of ${key}`;
  return query.kind === undefined ? `The tree read of ${key}` : `Conversation's read of ${key}`;
}

/**
 * `answer`, an outline read's first pages read again, with each older comment page the reader had
 * loaded into its query (`commentPagesOf`) read again too (R6, R15): from the new answer's own
 * `comments_before`, page by page, until it reaches as far back as those pages did, or, where they
 * held every older comment, until none remains. So a refetch after a write keeps every older
 * comment the reader had loaded. It looks at the mark again after each page, so a page the
 * reader's press merged while this read was out is read too. A position the walk has read already
 * fails the read, naming it, as a cursor named twice does. A query with no older page loaded
 * reads none: the page reads an older page only where the reader asked for one.
 */
async function withLoadedComments(
  key: string,
  query: TreeQuery,
  answer: TreeAnswer,
  { client, queryKey, signal }: Pick<QueryFunctionContext, "client" | "queryKey" | "signal">
): Promise<TreeAnswer> {
  const held = client.getQueryCache().find({ exact: true, queryKey });
  let merged = answer;
  const asked = new Set<string>();
  for (;;) {
    const loaded = held === undefined ? undefined : commentPagesOf.get(held);
    const before = olderCommentsBefore(merged);
    if (loaded === undefined || before === undefined) break;
    if (loaded.reached !== null && compareCommentPositions(before, loaded.reached) <= 0) break;
    if (asked.has(before)) {
      throw new Error(
        `${readName(key, query)} named the comment page "${before}" twice, so the older comments are missing.`
      );
    }
    asked.add(before);
    signal.throwIfAborted();
    const page = await api.getIssueTree(key, { ...query, comments_before: before }, { signal });
    merged = mergeCommentPage(merged, page);
  }
  if (held !== undefined && merged !== answer) {
    commentPagesOf.set(held, { reached: olderCommentsBefore(merged) ?? null });
  }
  return merged;
}

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

/** How the tree read and its views are kept fresh: as `EVENT_FRESH`, except that none of
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
  ...EVENT_FRESH,
  enabled: mayReadNow,
  refetchOnMount: mayReadOnMount,
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  retryOnMount: mayReadOnMount,
} as const;

/** The read of `outline`, one query over the pages read (`readTree`), under its key
 *  (`outlineKey`): Graph's under the tree's own key (`issueTreeQuery`), Conversation's under
 *  its own (`conversationTreeKey`), the kinds it names the last member of its key, in their one
 *  order, so its key is the same in every load. Both are kept fresh as the tree read is
 *  (`TREE_FRESH`, `subscribeTreeFreshness`). */
export const outlineQuery = (key: string, outline: Outline) =>
  queryOptions({
    ...TREE_FRESH,
    queryKey: outlineKey(outline, key),
    queryFn: (context) => readTree(key, OUTLINE_QUERIES[outline], context),
  });

/** The issue's whole tree: Graph's read, with no view. */
export const issueTreeQuery = (key: string) => outlineQuery(key, "graph");

function viewRouteQuery(view: TreeView, version: number | undefined): TreeQuery {
  return version === undefined ? { view } : { view, v: version };
}

/** One view of the issue's tree, one query over the pages read (`readTree`). A plain query rather
 *  than one per page, so an observer that is never enabled can watch a view a slot read. A filter
 *  turned on (`ViewReadProvider`) reads its view as any new observer of the tree's reads does
 *  (`TREE_FRESH`): where the view was never read, where a write or an event marked its answer
 *  stale, and, where its read was refused, only where it was asked for again since. A filter
 *  turned off and on again asks for nothing, so it reads a refused view no more. A read at a
 *  `version` asks for the view at that version (`v`) on each page it reads, and is a read of its
 *  own: its key is the view's with `{ v: version }` after it, still under the tree's, so every
 *  event of the issue reaches it, while the read at none sends no `v` and keeps the view's key
 *  (`viewKey`). */
export const viewQuery = (key: string, view: TreeView, version?: number) =>
  queryOptions({
    ...TREE_FRESH,
    queryKey:
      version === undefined
        ? viewKey(key, view)
        : ([...viewKey(key, view), { v: version }] as const),
    queryFn: (context) => readTree(key, viewRouteQuery(view, version), context),
  });

/**
 * `read` as an observer of the key the page's tree fills (`useIssueTree.ts`), which sends no
 * request of its own: it is never enabled, so drawing it, drawing it again once its answer has
 * aged, or the window taking the focus asks for nothing, and the caller draws whatever the
 * tree's own read holds. Its `refetch` still fetches, which is what a Retry asks. The Spec view
 * watches each read it draws from with it, and the contents slot the spec's blocks.
 */
export function treeRead<T extends object>(read: T) {
  return { ...read, enabled: false } as const;
}

/** Which outline read a load of `preset` holds (R1, R4, R13): Graph's in Graph (R10),
 *  Conversation's in Conversation and beside each filter, where the Contents list reads it, and
 *  neither in any other view: Dashboard, Threads and Documents, which their own parts add to the
 *  row, hold none and need no line here. */
export function outlineOf(preset: Preset): Outline | undefined {
  if (preset === "graph") return "graph";
  return preset === "conversation" || isFilter(preset) ? "conversation" : undefined;
}

/**
 * Which outline read the page holds, as the view of the address the router is at chooses it
 * (`outlineOf`). Every reader of the outline - the model (`useIssueTree`), the header's count,
 * the view row's counts, the Contents list, the stage's views, the refused read's line
 * (`TreeReadFailed`) - reads the address itself (`useOutlineChoice`) and so the one read it
 * holds, and a load sends that read once and no other. No provider stands around the slots for
 * it: the one thing the readers share, whether Conversation's read is sent on Spec, is the query
 * cache's (`outlineBegun`).
 */
interface OutlineChoice {
  /** The filter on where it reads a view of the tree, whose view read holds that role's blocks
   *  (`TreeRead.blocksOf`); undefined in every other view. */
  readonly filter: TreeFilter | undefined;
  /** The outline whose read the page holds, or undefined where the view the address draws holds
   *  neither. */
  readonly outline: Outline | undefined;
  /** The view the address draws (`presetOf`): Conversation's where the issue page does not read
   *  the route. */
  readonly preset: Preset;
  /** Whether a reader sends that read with the load: Graph's in Graph, and Conversation's in
   *  every view that holds it but Spec, whose own view read holds the memo and the statements the
   *  view draws. On Spec the stage sends it from 1024 px up, where the Contents pane beside the
   *  view reads it (`ViewReadProvider`), and the Outline sheet below as it opens
   *  (`useStartOutlineRead`); each reader watches it, sending nothing, until one of them has. */
  readonly sends: boolean;
}

/** The outline read of the address the router is at (`OutlineChoice`): a route the issue page
 *  does not read (`parseIssuePath`) is the page with no filter on. */
function useOutlineChoice(): OutlineChoice {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const preset = route === undefined ? "conversation" : presetOf(route);
  const outline = outlineOf(preset);
  return {
    filter: TREE_FILTERS.find((filter) => filter === preset),
    outline,
    preset,
    sends: outline === "graph" || (outline === "conversation" && preset !== "spec"),
  };
}

/** Whether an outline read has been sent: it is out, or it has answered or been refused since
 *  the cache made it. A reader that does not send the read itself (`OutlineChoice.sends`) takes
 *  it from then on as a reader that does. */
function outlineBegun(query: Pick<Query, "state">): boolean {
  return query.state.status !== "pending" || query.state.fetchStatus !== "idle";
}

/** The outline read `choice` holds, as a reader observes it: sent where `choice` sends it, and
 *  else watched until something sent it (`outlineBegun`); Conversation's read, watched and never
 *  sent, where the view the address draws holds neither. */
function observedOutline(key: string, { outline, sends }: OutlineChoice) {
  return {
    ...outlineQuery(key, outline ?? "conversation"),
    enabled: (query: Pick<Query, "state">) =>
      outline !== undefined && (sends || outlineBegun(query)) && mayReadNow(query),
  };
}

function useOutlineRead(key: string) {
  const choice = useOutlineChoice();
  return { choice, read: useQuery(observedOutline(key, choice)) };
}

/** Sends Conversation's read of issue `key` where nothing has sent it (`outlineBegun`), and
 *  nothing where the cache holds it sent, so a load reads it once (R13): what the Outline sheet
 *  calls as it opens, which on Spec below 1024 px is the first reader that sends it, and what the
 *  stage calls on Spec from 1024 px up (`ViewReadProvider`). */
export function useStartOutlineRead(key: string): () => void {
  const queryClient = useQueryClient();
  return useCallback(() => {
    const options = outlineQuery(key, "conversation");
    const held = queryClient.getQueryCache().find({ exact: true, queryKey: options.queryKey });
    if (held === undefined || !outlineBegun(held)) void queryClient.prefetchQuery(options);
  }, [key, queryClient]);
}

/** One role's blocks from the read that holds them (`TreeRead.blocksOf`), answered as the blocks
 *  read of that role's document answered them (`artifactBlocksQuery`), so a caller's lines that
 *  read these members stay as they are. The state is that read's own; whether that read runs is
 *  read apart, by the views that draw it (`useBlocksRunning`). */
export interface TreeBlocksRead {
  readonly data: TreeBlock[] | undefined;
  readonly errorUpdateCount: number;
  readonly isError: boolean;
  readonly refetch: () => Promise<unknown>;
}

/** The tree read, and how far it has got. */
export interface TreeRead {
  /** The pages read merged: the first, from the load on, and each next page the reader's press
   *  or an address read since (`readNextTreePage`). */
  readonly answer: TreeAnswer | undefined;
  /** Whether the read holds an answer, which is what a view that draws from the blocks it holds
   *  waits on: from the moment its first page holds data. A refetch that fails over an answer
   *  already held keeps it held - TanStack keeps a query's data through a failed refetch - so the
   *  view keeps the text it drew, which waiting on `blocksRead` would blank, since that failure
   *  turns it false. Both are false before the first answer and after a first read that fails. */
  readonly blocksHeld: boolean;
  /** Whether the read answered each page it read with success. A read that fails, a first one or a
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
  /** Reads the tree again from the first page, as far as the pages loaded reach: what a Retry of a
   *  failed read asks. */
  readonly refetch: () => Promise<unknown>;
}

/**
 * The outline read the page holds (`OutlineChoice`) alone, for the issue page's model
 * (`useIssueTree`) and for a surface that needs the blocks of a role document, or how the read
 * stands, and none of the model's other reads. Each value it answers keeps its identity while the
 * read's answer and state stand, so a caller's memo built on one is not built again on every
 * render; a role's blocks are made the first time a caller asks for them, so a caller that reads
 * none costs none.
 *
 * A role's blocks come from the read that holds them (R5): under that role's filter its view
 * read, which `ViewReadProvider` sends and this watches without sending; in Graph Graph's read;
 * and elsewhere the outline read, which holds the memo, the criteria and the statements. A read
 * nothing has sent - Conversation's on Spec below 1024 px until the Outline sheet opens - is not
 * pending: nothing is out, and the reads that wait on every read of the page do not wait on it.
 */
export function useTreeRead(issue: IssueDetails): TreeRead {
  const { choice, read } = useOutlineRead(issue.key);
  const { filter } = choice;
  const {
    data: answer,
    errorUpdateCount,
    fetchStatus,
    isError,
    isPending,
    isSuccess,
    refetch,
  } = read;
  const sent = choice.outline !== undefined && (choice.sends || fetchStatus !== "idle");
  const view = useQuery({ ...viewQuery(issue.key, filter ?? TREE_FILTERS[0]), enabled: false });
  const viewAnswer = filter === undefined ? undefined : view.data;
  const blocks = useMemo(
    () => byRole((role) => blocksOfRole(role === filter ? viewAnswer : answer, role, issue)),
    [answer, filter, issue, viewAnswer]
  );
  const viewErrors = filter === undefined ? 0 : view.errorUpdateCount;
  const viewRefused = filter !== undefined && view.isError;
  const viewRefetch = view.refetch;
  const blocksOf = useMemo(
    () =>
      byRole(
        (role): TreeBlocksRead =>
          role === filter
            ? {
                data: blocks(role),
                errorUpdateCount: viewErrors,
                isError: viewRefused,
                refetch: viewRefetch,
              }
            : { data: blocks(role), errorUpdateCount, isError, refetch }
      ),
    [blocks, errorUpdateCount, filter, isError, refetch, viewErrors, viewRefetch, viewRefused]
  );
  return useMemo(
    () => ({
      answer,
      blocksHeld: answer !== undefined,
      blocksRead: isSuccess,
      blocksOf,
      failed: answer === undefined && errorUpdateCount > 0,
      pending: sent && isPending,
      refetch,
    }),
    [answer, blocksOf, errorUpdateCount, isPending, isSuccess, refetch, sent]
  );
}

/** Whether the read that holds `role`'s blocks of issue `key` runs (TanStack Query's
 *  `isFetching`): its first read, a refresh, or a Retry. That read is the one `useTreeRead`'s
 *  `blocksOf` takes them from: under `role`'s own filter its view read, and else the outline read
 *  the page holds (`OutlineChoice`). Observers of their own beside `useTreeRead`'s, which reads
 *  none of it, so a reader of the tree is drawn again as each read of it starts and ends only
 *  where it reads this: a view that draws a failed read's Retry busy while it runs, and asks
 *  nothing more of it meanwhile. Neither sends anything the page would not: the view is watched
 *  and never sent, as `useTreeRead` watches it. */
export function useBlocksRunning(key: string, role: TreeFilter): boolean {
  const { choice, read } = useOutlineRead(key);
  const view = useQuery({ ...viewQuery(key, choice.filter ?? TREE_FILTERS[0]), enabled: false });
  return role === choice.filter ? view.isFetching : read.isFetching;
}

/** The two reads of issue `key` a decision's Keep and Change look for their decision in
 *  (`DecisionActions.tsx`), in the order they look: first the read its row is drawn from - on the
 *  Decisions view the decisions view read, and in every other view the outline read the page
 *  holds (`OutlineChoice`) - then the other. Both are watched and never sent, since `useTreeRead`
 *  sends the outline read and `ViewReadProvider` the view: for a surface that holds the issue's
 *  key and not its reads. */
export function useDecisionReads(
  key: string
): readonly [TreeAnswer | undefined, TreeAnswer | undefined] {
  const choice = useOutlineChoice();
  const outline = useQuery({ ...observedOutline(key, choice), enabled: false }).data;
  const view = useQuery({ ...viewQuery(key, "decisions"), enabled: false }).data;
  return choice.preset === "decisions" ? [view, outline] : [outline, view];
}

/** Whether the outline read the page holds answers older comments it has not read
 *  (`OlderComments.remain`), and how to read the next page of them. */
export interface OlderComments {
  /** Reads the next older page into the outline read's answer (`readOlderComments`): one
   *  request however often it is asked while that page is out. */
  readonly read: () => Promise<void>;
  /** Whether older comments remain: the read's answer names where the next older page starts
   *  (`comments_before`). */
  readonly remain: boolean;
}

/** The older comments of the outline read the page holds (R15), for the two controls that read
 *  them: the one above the outline's first row under the issue, and the one at the foot of the
 *  Contents list. Neither reads anything until it is pressed. */
export function useOlderComments(issue: IssueDetails): OlderComments {
  const queryClient = useQueryClient();
  const {
    choice,
    read: { data: answer },
  } = useOutlineRead(issue.key);
  const outline = choice.outline;
  const remain =
    outline !== undefined && answer !== undefined && olderCommentsBefore(answer) !== undefined;
  const read = useCallback(
    () =>
      outline === undefined
        ? Promise.resolve()
        : readOlderComments(queryClient, issue.key, outline),
    [issue.key, outline, queryClient]
  );
  return useMemo(() => ({ read, remain }), [read, remain]);
}

/** The older comment pages out, by client, each by its query and the position it reads from
 *  (`readOlderComments`). */
const commentPagesOut = new WeakMap<QueryClient, Map<string, Promise<void>>>();

/** Starts a page read only if that client has no matching read out. */
function joinedPage(
  pagesOut: WeakMap<QueryClient, Map<string, Promise<void>>>,
  client: QueryClient,
  page: string,
  readPage: () => Promise<void>
): Promise<void> {
  let pages = pagesOut.get(client);
  if (pages === undefined) {
    pages = new Map();
    pagesOut.set(client, pages);
  }
  const reading = pages.get(page);
  if (reading !== undefined) return reading;
  const read = readPage().finally(() => pages.delete(page));
  pages.set(page, read);
  return read;
}

function writeKeepingStale(
  client: QueryClient,
  query: Pick<Query, "state">,
  queryKey: QueryKey,
  answer: TreeAnswer
): void {
  const stale = query.state.isInvalidated;
  client.setQueryData(queryKey, answer);
  if (stale) void client.invalidateQueries({ exact: true, queryKey, refetchType: "none" });
}

/**
 * Reads the next older comment page of `outline`'s read of issue `key` (R15), one request with
 * `?comments_before=` the answer the cache holds names (`comments_before`), no view and no cursor,
 * beside the kinds Conversation's read names, and merges it into that answer in the cache, where
 * the read's order puts each node (`mergeCommentPage`). A press while that page is out joins it,
 * and one after it merged reads the next, so each page is read once whichever control asks for
 * it. Where the cache holds no answer, or one with no older comment, it reads nothing. A page that
 * lands once the answer has moved on - a read made again from the first page, which reads again
 * each page the reader had loaded (`withLoadedComments`) - is not merged into it. The merge writes
 * the answer by `setQueryData`, which the tree's subscription takes as no write of the issue: it
 * reacts to a read, an invalidation, a reset and a refusal (`subscribeTreeFreshness`). A query an
 * event or a write had marked stale stays marked, so the read it owes is still made. A refused
 * page leaves the answer as it was, and the press may be made again.
 */
export function readOlderComments(
  queryClient: QueryClient,
  key: string,
  outline: Outline
): Promise<void> {
  const { queryKey } = outlineQuery(key, outline);
  const held = queryClient.getQueryData<TreeAnswer>(queryKey);
  const before = held === undefined ? undefined : olderCommentsBefore(held);
  if (before === undefined) return Promise.resolve();
  return joinedPage(commentPagesOut, queryClient, `${hashKey(queryKey)} ${before}`, () =>
    api
      .getIssueTree(key, { ...OUTLINE_QUERIES[outline], comments_before: before })
      .then((older) => {
        const now = queryClient.getQueryData<TreeAnswer>(queryKey);
        const query = queryClient.getQueryCache().find({ exact: true, queryKey });
        if (now === undefined || query === undefined || olderCommentsBefore(now) !== before) return;
        const merged = mergeCommentPage(now, older);
        commentPagesOf.set(query, { reached: olderCommentsBefore(merged) ?? null });
        writeKeepingStale(queryClient, query, queryKey, merged);
      })
  );
}

/** A read whose pages past the first the page reads one at a time (R15): an outline's
 *  (`Outline`) or a view's (`TreeView`), the decisions view among them. */
type PagedRead = Outline | TreeView;

/** What `read` asks the tree route for beside a page's `cursor`, and the key its answer stands
 *  under in the cache: a view's at `version` where it is read at one (`viewQuery`). */
function pagedQuery(
  key: string,
  read: PagedRead,
  version: number | undefined
): { readonly query: TreeQuery; readonly queryKey: QueryKey } {
  if (read === "conversation" || read === "graph") {
    return { query: OUTLINE_QUERIES[read], queryKey: outlineQuery(key, read).queryKey };
  }
  return {
    query: viewRouteQuery(read, version),
    queryKey: viewQuery(key, read, version).queryKey,
  };
}

/** The next pages out, by client, each by its query and the cursor it reads
 *  (`readNextTreePage`). */
const treePagesOut = new WeakMap<QueryClient, Map<string, Promise<void>>>();

/**
 * Reads the page after the last one `read`'s answer of issue `key` holds (R15): one request with
 * the `cursor` that answer names, beside what the read asks the route for, and merges the page
 * into that answer in the cache, after the pages it holds, so each node stands where the read's
 * order puts it (`mergeTreePages`); the answer then names the page after it, and how many nodes
 * the read holds past it, where one remains. A press while that page is out joins it, and one
 * after it merged reads the next, so each page is read once whatever asks for it: the control
 * after the last row, its key, or an address that names a node no page read yet holds
 * (`useReadUntilHeld`). Where the cache holds no answer, or one that names no next page, it reads
 * nothing. A page that names a cursor the read has asked for already fails, naming the cursor,
 * and is not merged: the server named one page twice, so the pages past it are missing. A page
 * that lands once the answer has moved on - a read made again from the first page - is not merged
 * into it. The merge writes the answer by `setQueryData`, which the tree's subscription takes as
 * no write of the issue (`subscribeTreeFreshness`), and marks how far the read has got
 * (`treePagesOf`), so a read made again reads as far (`readTree`). A query an event or a write had
 * marked stale stays marked, so the read it owes is still made. A refused page leaves the answer
 * as it was, and the press may be made again. A view read at a `version` (`viewQuery`) reads its
 * next page at that version too, under its own key.
 */
export function readNextTreePage(
  queryClient: QueryClient,
  key: string,
  read: PagedRead,
  version?: number
): Promise<void> {
  const { query, queryKey } = pagedQuery(key, read, version);
  const held = queryClient.getQueryData<TreeAnswer>(queryKey);
  const cursor = held === undefined ? undefined : nextTreeCursor(held);
  if (cursor === undefined) return Promise.resolve();
  return joinedPage(treePagesOut, queryClient, `${hashKey(queryKey)} ${cursor}`, () =>
    api.getIssueTree(key, { ...query, cursor }).then((answered) => {
      const now = queryClient.getQueryData<TreeAnswer>(queryKey);
      const cached = queryClient.getQueryCache().find({ exact: true, queryKey });
      if (now === undefined || cached === undefined || nextTreeCursor(now) !== cursor) return;
      const walk = treePagesOf.get(cached);
      const asked = new Set([...(walk?.cursors ?? []), cursor]);
      nextCursorOf(key, query, answered, asked);
      treePagesOf.set(cached, {
        cursors: asked,
        pages: (walk?.pages ?? 1) + 1,
        walked: (walk?.walked ?? now.nodes?.length ?? 0) + (answered.nodes?.length ?? 0),
      });
      writeKeepingStale(queryClient, cached, queryKey, mergeTreePages([now, answered]));
    })
  );
}

/** The next page of a read whose answer names one (`cursor`, R15): how many nodes the read holds
 *  after the pages read (`left`, the answer's `remaining`, undefined where it gives none), and
 *  how to read it (`readNextTreePage`), one request however often it is asked while that page is
 *  out. */
export interface NextTreePage {
  readonly left: number | undefined;
  readonly read: () => Promise<void>;
}

/** The next page of the outline read the page holds (`OutlineChoice`), for the control after the
 *  outline's last row; undefined while its answer names none. It reads nothing until asked. */
export function useNextTreePage(issue: IssueDetails): NextTreePage | undefined {
  const {
    choice,
    read: { data: answer },
  } = useOutlineRead(issue.key);
  return useNextPage(issue.key, choice.outline, answer);
}

/** The next page of `view`'s view read of issue `key`, for the control after the view's last
 *  row; undefined while the view holds no answer, or one that names none. Its observer is never
 *  enabled, so it sends nothing: the view is read where `ViewReadProvider` reads it. */
export function useNextViewPage(key: string, view: TreeView): NextTreePage | undefined {
  const answer = useQuery({ ...viewQuery(key, view), enabled: false }).data;
  return useNextPage(key, view, answer);
}

/** The next page of `read`'s `answer` (`NextTreePage`), the same value while the answer names the
 *  same page and count. */
function useNextPage(
  key: string,
  read: PagedRead | undefined,
  answer: TreeAnswer | undefined
): NextTreePage | undefined {
  const queryClient = useQueryClient();
  const cursor = answer === undefined ? undefined : nextTreeCursor(answer);
  const left = answer === undefined ? undefined : nodesRemaining(answer);
  const readNext = useCallback(
    () => (read === undefined ? Promise.resolve() : readNextTreePage(queryClient, key, read)),
    [key, queryClient, read]
  );
  return useMemo(
    () => (read === undefined || cursor === undefined ? undefined : { left, read: readNext }),
    [cursor, left, read, readNext]
  );
}

/** How the reading of a read's next pages for a node no page read yet holds stands
 *  (`useReadUntilHeld`). */
export interface PagesReading {
  /** Whether a next page is being read: out, or asked for as the reading's effect runs. */
  readonly reading: boolean;
  /** Whether the page the reading last asked for was refused, while the read still names that
   *  page as its next: nothing past it is read until a read of it answers. */
  readonly refused: boolean;
}

/**
 * Reads the next pages of the outline read the page holds, or of `outline`'s read where one is
 * given, one at a time, while `wanted`: an address, a chip or a key names a node no page read yet
 * holds (R15). Each page merged draws the tree again, and the caller says again whether it holds
 * the node, so the reading ends with the page that brings it, or with the last page. Answers
 * whether it is still reading, which the address's landing waits on as it waits on the reads the
 * tree is built from: once the node is drawn it lands on it, and once no page is left it falls
 * back as it does today. A page refused stops the reading at its cursor, and the hook answers it
 * refused for as long as the read names that page as its next: that is no last page, so the
 * address's landing waits on it as on a page out, and the control after the outline's last row,
 * which reads that page again, stays as its Retry; once that page answers, the reading goes on.
 * `outline`'s read, where it is given, is watched and never sent here: the caller sends it.
 */
export function useReadUntilHeld(
  issue: IssueDetails,
  wanted: boolean,
  outline?: Outline
): PagesReading {
  const queryClient = useQueryClient();
  const choice = useOutlineChoice();
  const read = outline ?? choice.outline;
  const answer = useQuery(
    outline === undefined
      ? observedOutline(issue.key, choice)
      : { ...outlineQuery(issue.key, outline), enabled: false }
  ).data;
  const cursor = answer === undefined ? undefined : nextTreeCursor(answer);
  // The cursor whose page was refused, past which nothing is read for the address.
  const [refusedAt, setRefusedAt] = useState<string | undefined>(undefined);
  const named = wanted && read !== undefined && cursor !== undefined;
  const refused = named && cursor === refusedAt;
  const reading = named && !refused;
  useEffect(() => {
    if (!reading || read === undefined || cursor === undefined) return;
    readNextTreePage(queryClient, issue.key, read).catch(() => setRefusedAt(cursor));
  }, [cursor, issue.key, queryClient, read, reading]);
  return { reading, refused };
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
 * Keeps the tree read and its views as fresh as the issue's other reads, and so each outline read
 * the page holds: Graph's, which is the tree read, and Conversation's (`Outline`), whichever it
 * holds, each read again with the pages and the older comment pages it had loaded (`readTree`). Two
 * signals tell it what can change them, and nothing is judged from the issue's other reads:
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
 * meanwhile, so that settle asks for the tree and every view, the decisions view among them; with
 * the feed up a write's event tells, and a write Dispatch refused, which changed nothing, reads
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

/** Each outline read the page may hold of issue `key`, each by its own key (`outlineKey`):
 *  Graph's, the tree read, and Conversation's (`OUTLINE_QUERIES`). Each is kept fresh as the tree
 *  read is (`subscribeTreeFreshness`), and a view follows whichever the page holds; a write whose
 *  answer changes what the outline draws reads them again by the same targets
 *  (`DecisionActions.tsx`). */
export function outlineTargets(key: string): readonly RefreshTarget[] {
  return (Object.keys(OUTLINE_QUERIES) as Outline[]).map((outline) => ({
    exact: true,
    queryKey: outlineKey(outline, key),
  }));
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
  // Each outline read the page may hold: Graph's, the tree read, and Conversation's
  // (`OUTLINE_QUERIES`). Each is kept fresh as the tree read is, and a view follows whichever the
  // page holds.
  const outlines = outlineTargets(key);
  // Each view's reads, by the view's key as a prefix: its read at no version, under that key, and
  // each read of it at a version (`viewQuery`), under that key with `{ v }` after it.
  const views: readonly RefreshTarget[] = TREE_VIEWS.map((view) => ({
    queryKey: viewKey(key, view),
  }));
  const treeAndViews: readonly RefreshTarget[] = [...outlines, ...views];
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
      if (outlines.some((outline) => matchQuery(outline, query))) watcher.release();
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
      if (action.type === "error") {
        const target: RefreshTarget = { exact: true, queryKey: query.queryKey };
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
    for (const query of treeAndViews.flatMap((target) => cache.findAll(target))) {
      if (query.state.fetchStatus !== "idle") owedOnRefusal.add(query.queryHash);
      else if (query.state.status === "error") {
        refused.push({ exact: true, queryKey: query.queryKey });
      }
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
  // nothing of what the writes did nor of an event lost meanwhile, the tree and every view, the
  // decisions view among them; with it up, each tree and view read owed - sent or asked for while
  // a write was out (`owedReads`), or marked stale meanwhile - and nothing for a write that
  // changed nothing.
  const settle = (): void => {
    if (!settling) return;
    settling = false;
    if (stopped) return;
    releaseReads();
    const owed = !liveFeedUp(queryClient)
      ? treeAndViews
      : treeAndViews
          .flatMap((target) => cache.findAll(target))
          .filter((query) => owedReads.has(query.queryHash) || query.state.isInvalidated)
          .map((query): RefreshTarget => ({ exact: true, queryKey: query.queryKey }));
    owedReads.clear();
    if (owed.length > 0) refreshQueries(queryClient, owed);
  };
  // The hold, as the write store tells of the issue's writes: the first going out while none is
  // begins it and holds the tree and its views (`heldByWrites`); the last landing, refused or let
  // go ends it (`settle`).
  const unwrite = writes.onIssueWrite(key, (phase, write) => {
    if (phase === "out") {
      const begins = !holding();
      writesOut.add(write.id);
      // A write of the issue that goes out as the last settles: the hold goes on.
      settling = false;
      if (begins) holdReads();
      return;
    }
    if (!writesOut.delete(write.id) || writesOut.size > 0) return;
    settling = true;
    queueMicrotask(settle);
  });
  const watcher: TreeFreshness = {
    holders: 0,
    release: () => {
      if (stopped || watcher.holders > 0) return;
      if (outlines.some((outline) => cache.find(outline) !== undefined)) return;
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
