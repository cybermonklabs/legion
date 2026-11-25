import type { LensInvoked } from "@legion/contracts";
import {
  type QueryClient,
  queryOptions,
  useIsMutating,
  useMutationState,
  useQueryClient,
} from "@tanstack/react-query";
import { useLayoutEffect, useRef } from "react";
import { type Location, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";

import { api, writeRequest } from "../../../api/client";
import { type RefreshTarget, refreshQueries } from "../../../api/query-refresh";
import { parseIssuePath } from "../../refs/routes";
import { useKeyedWrite } from "../../writes/useKeyedWrite";
import { issueAddress, presetPath } from "../issue-address";
import { LENS_WORDS } from "../page-words";
import { type Outline, outlineQuery } from "../tree-read/tree-read";
import { outlineRowPath } from "./store-link";

/**
 * The one module of the issue page that names the lens read, the lens catalogue and the
 * invocation of a lens. Both reads sit outside the issue's own keys, so no event refresh of the
 * issue reads them again: each is read once in a load. The app's retries stand. It also says
 * which outline an address opens (`lensPlaceOf`) and where a row of each outline is
 * (`rowPathIn`), which the Lens act and the chips of a lens line read.
 */

/** What makes a read held once in a load: an answer never goes stale or is let go, and the
 *  window taking the focus reads nothing again. */
const HELD_FOR_THE_LOAD = {
  gcTime: Number.POSITIVE_INFINITY,
  refetchOnWindowFocus: false,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The lens read of the lens at `address`, read at a press of `LENS_WORDS.show_chain`. */
export const lensChainQuery = (address: string, enabled: boolean) =>
  queryOptions({
    ...HELD_FOR_THE_LOAD,
    enabled,
    queryFn: () => api.getLenses(address),
    queryKey: ["lens-read", address],
  });

/** The lens catalogue, in its own order, read when the menu of lens kinds opens. A failed
 *  read is read again at the next opening. */
export const lensKindsQuery = (enabled: boolean) =>
  queryOptions({
    ...HELD_FOR_THE_LOAD,
    enabled,
    queryFn: async () => (await api.getBlockSchema()).lens_kinds ?? [],
    queryKey: ["lens-kinds"],
  });

/**
 * The id in the tree read of the lens a 201 made: what `?node=` names to land on its row, which
 * `pathToTarget` matches against `TreeNode.readId`. The 201's `id` is the lens's store id, the
 * uuid of its address `dispatch://lens/<uuid>`, and the read gives a lens that id. This form
 * follows the tree read's contract as written, since the server does not serve that read yet;
 * the browser rows check it once it does.
 */
export function lensReadId(invoked: LensInvoked): string {
  return invoked.id;
}

/** The address the reader stands at, and the router's way of moving it. */
interface Standing {
  readonly location: Location;
  readonly navigate: NavigateFunction;
}

/** Where the reader stands now, as `useStandingAddress` keeps it: nothing once the component
 *  that keeps it is gone. */
export interface StandingAddress {
  readonly current: Standing | undefined;
}

/**
 * Where the reader stands, kept current for as long as the component that calls this is drawn,
 * and nothing from the moment it is gone. `TreeRows` calls it, since it is drawn for as long as
 * the outline is and no longer. A pick's 201 can come long after the pick, since the write store
 * tries the write again past its deadline, and by then the row may have lost the focus, which
 * takes its menu away, and the reader may be on another page: what a render of the pick held of
 * the address says nothing of where the reader is then, and neither does what a menu that is gone
 * last kept. It is kept as the address is drawn (a layout effect), so no answer that comes after
 * a move reads the address from before it.
 */
export function useStandingAddress(): StandingAddress {
  const location = useLocation();
  const navigate = useNavigate();
  const standing = useRef<Standing | undefined>(undefined);
  useLayoutEffect(() => {
    standing.current = { location, navigate };
    return () => {
      standing.current = undefined;
    };
  }, [location, navigate]);
  return standing;
}

/** The issue an address names and the outline it opens, read as the page's slots read their
 *  address: Conversation or Graph, and none in a filter or any other view (`outline`), beside
 *  which the page may hold Conversation's read but draws no outline (`tree-read/tree-read.ts`'
 *  `outlineOf`). What `TreeRows` offers the Lens act by, what a chip links by, and the one
 *  address a landing moves. Undefined for an address that names no issue. */
export function lensPlaceOf(
  pathname: string,
  search: string
): { readonly issueKey: string; readonly outline: Outline | undefined } | undefined {
  const route = parseIssuePath(pathname, search);
  if (route === undefined) return undefined;
  const { preset } = issueAddress(route, search, "", undefined);
  const outline = preset === "conversation" || preset === "graph" ? preset : undefined;
  return { issueKey: route.key, outline };
}

/** The address of the row of the node whose id in the tree read is `readId` in `outline` of the
 *  issue `key`, after the words filter `filter` (`?q=`) where one is given: Conversation's on the
 *  issue's own address (`outlineRowPath`), and Graph's on Graph's own (`presetPath`) with the same
 *  query, so `outlineRowPath` stays the one builder of a row's `?node=` (R7, R9). */
export function rowPathIn(outline: Outline, key: string, readId: string, filter?: string): string {
  const path = outlineRowPath(key, readId, filter);
  if (outline === "conversation") return path;
  return `${presetPath(key, "graph")}${path.slice(path.indexOf("?"))}`;
}

/**
 * Reads again the outline read the page holds for `outline` of the issue `issueKey`, Graph's or
 * Conversation's (`outlineQuery`), through the app's refresh scheduler (`refreshQueries`), and
 * answers once that read is done: when the first read of it that begins from the refresh on has
 * settled. The scheduler answers nothing itself and may begin that read later than its call: once
 * a read already out has settled, or a second after the last read began. Where nothing observes
 * the read, the scheduler reads nothing and only marks it stale, to be read when a page draws it,
 * and an observer that holds its answer for good (`isStatic`) reads nothing either, so this
 * answers at once; and it answers as soon as the read is let go, or its last observer goes, while
 * it waits, since no read comes then. It never answers with a timer of its own. It reads no view
 * of the issue and leaves the other outline's read as it is (R6): no view draws a lens, a finding
 * or a risk, so a lens leaves every view current.
 */
function readTreeAgain(
  queryClient: QueryClient,
  issueKey: string,
  outline: Outline
): Promise<void> {
  const cache = queryClient.getQueryCache();
  const tree: RefreshTarget = { exact: true, queryKey: outlineQuery(issueKey, outline).queryKey };
  const query = cache.find(tree);
  const { promise, resolve } = Promise.withResolvers<void>();
  if (query === undefined || !query.isActive() || query.isStatic()) {
    resolve();
  } else {
    // Subscribed before the refresh, since the scheduler can begin the read within its call.
    let began = false;
    const unsubscribe = cache.subscribe((event) => {
      if (event.query !== query) return;
      if (event.type === "updated" && event.action.type === "fetch") began = true;
      // A read settles when it answers, fails or is called off, each of which leaves the query
      // idle; data written into the cache while the read is out leaves it fetching.
      const settled = began && query.state.fetchStatus === "idle";
      if (settled || event.type === "removed" || !query.isActive()) {
        unsubscribe();
        resolve();
      }
    });
  }
  refreshQueries(queryClient, [tree]);
  return promise;
}

/** What the menu of one node needs of the lenses invoked on it. */
export interface LensInvocation {
  /** Invokes a lens of kind `kind` on the node (`useInvokeLens`). */
  readonly invoke: (kind: string) => Promise<LensInvoked | null>;
  /** Whether an invocation on the node is out, whichever menu made it: from the pick until its
   *  write settles, with Dispatch's answer or once the reader stopped trying. It ends before
   *  `invoke` answers, since `invoke` reads the tree again after the write settles. */
  readonly pending: boolean;
  /** Why the newest invocation on the node was refused, until `forget`: Dispatch's answer
   *  (`ApiError`), or the store's own error once the reader stopped trying. */
  readonly refusal: Error | undefined;
  /** Lets go of every invocation on the node that has its answer, and so of its refusal. */
  readonly forget: () => void;
}

/**
 * Invokes lenses on the node at the address `on`, on the issue `issueKey`, from the outline
 * `outline` the address opens as the menu is drawn, Conversation or Graph. Each pick is one
 * write, under a key minted at the pick and in the reader's name (`useKeyedWrite`), and sends
 * `on` and `kind` and nothing more. It answers the lens made, `null` where Dispatch made it and
 * kept no answer, or throws the refusal (`ApiError`). The menu says a refusal itself, so the
 * shell's indicator shows the write only while it is out.
 *
 * The node's invocations are kept in the query client under a key of the node's own, and one
 * that has its answer stays there until `forget` lets it go, so its refusal outlives the menu
 * that made it: the menu is drawn only while its row has the focus, and a row that lost the focus
 * while its pick was out says the refusal once it is focused again. For the same reason whether
 * one is out is read from there too (`pending`), as `useSending` reads a send: `mutate` counts the
 * write as out from the pick's own task, before its first await, and a menu drawn again while
 * the write is out reads it.
 *
 * A landing reads the outline's read again through the app's refresh scheduler, as the page does
 * for a write it sees, so it never calls off a read already out and keeps to one start a second:
 * the write and the event it raises read the open view's tree once or twice, never more, and the
 * other outline's not at all (R6). It reads no view of the issue and resets none, since no view
 * draws a lens, a finding or a risk: a view read before the write is still current. It waits for
 * the outline's read (`readTreeAgain`), so the outline holds the lens's row when the address
 * names it. Then, where Dispatch answered the lens it made and the reader still stands on this
 * issue in the same outline, it replaces the address with that outline's row of the lens
 * (`?node=`, `rowPathIn`), keeping the words filter (`?q=`) and dropping every other part of the
 * address, so the page lands on the new row and a reload lands there too. The address is the one
 * that stands as the read answers (`standing`), whichever row has the focus then. A reader who
 * has left the outline, for another page, another issue, the other outline or one of this
 * issue's views, is left where they went, and so is one who has come back to it since: the
 * outline drawn again keeps an address of its own.
 */
export function useInvokeLens(
  issueKey: string,
  on: string,
  outline: Outline,
  standing: StandingAddress
): LensInvocation {
  const queryClient = useQueryClient();
  const mutationKey = ["lens-invocation", issueKey, on];
  const made = useKeyedWrite({
    // An invocation that has its answer stays in the cache, though no menu observes it once its
    // row has lost the focus, until `forget` lets it go.
    gcTime: Number.POSITIVE_INFINITY,
    inline: true,
    label: () => LENS_WORDS.menu_title,
    mutationKey,
    method: "POST",
    write: (kind: string, bounds) =>
      api.send(
        writeRequest("post", "/api/v1/issues/{key}/lenses", { key: issueKey }, { kind, on }),
        bounds
      ),
  });
  // The cache lists the invocations in the order they were made, so the last is the newest.
  const refusal =
    useMutationState({
      filters: { exact: true, mutationKey },
      select: ({ state }) => (state.status === "error" ? state.error : null),
    }).at(-1) ?? undefined;
  const pending = useIsMutating({ exact: true, mutationKey }) > 0;
  return {
    forget: () => {
      const cache = queryClient.getMutationCache();
      for (const answered of cache.findAll({ exact: true, mutationKey })) {
        if (answered.state.status !== "pending") cache.remove(answered);
      }
    },
    invoke: async (kind) => {
      const invoked = await made.mutateAsync(kind);
      await readTreeAgain(queryClient, issueKey, outline);
      const now = standing.current;
      const place =
        now === undefined ? undefined : lensPlaceOf(now.location.pathname, now.location.search);
      const same = place?.issueKey === issueKey && place.outline === outline;
      if (invoked !== null && now !== undefined && same) {
        const filter = new URLSearchParams(now.location.search).get("q") ?? undefined;
        now.navigate(rowPathIn(outline, issueKey, lensReadId(invoked), filter), { replace: true });
      }
      return invoked;
    },
    pending,
    refusal,
  };
}
