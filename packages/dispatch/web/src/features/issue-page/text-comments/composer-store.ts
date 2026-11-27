import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useSyncExternalStore } from "react";
import { useLocation } from "react-router-dom";

import type { MarkComposePlace } from "../../margin/margin-context";
import { parseIssuePath } from "../../refs/routes";
import { useMediaQuery } from "../../shell/useDialog";
import { CONTENTS_PANE_QUERY } from "../contents-frame";
import { useMarginDrawer } from "../margin-slot";
import type { SelectedWords, TextComposerOutcome } from "./SelectionComposer";

/**
 * The composer of R5's one state for one app (`useTextComposer`), which the column of comments on
 * text (`TextMargin.tsx`), each node's sheet below 1024 px (`NodeThreads.tsx`), the control of a
 * selection (`SelectionComment.tsx`) and a block's editor (`StagedDocument.tsx`, through
 * `useMarkComposePlace`) share wherever each is drawn on the page: held beside the query client, so
 * it is reached from anywhere under the client's provider with no provider of its own.
 */

/** The attribute of the box that holds a composer of this part, in the column or in a node's
 *  sheet. */
export const TEXT_COMPOSER_ATTRIBUTE = "data-text-composer";

/** The words a person selected, as the control hands them to the column to open its composer on
 *  (R5): the node they are in, by its tree id (`TreeNode.id`), and what the server needs to find
 *  them again. */
export type TextComposerRequest = SelectedWords;

/** The composer of this part: the request it is open on, or undefined while none is open, and
 *  what its last send left the page to do. */
export interface TextComposer {
  /** Closes the composer, keeping its draft as `MentionComposer` keeps one. */
  readonly close: () => void;
  /** Closes the composer on its send's 201 and holds what the page does next (R5): the new
   *  thread's root takes the focus once the re-read draws it, or one sentence says a reply's
   *  quote was not kept. */
  readonly land: (outcome: TextComposerOutcome) => void;
  /** What the last send left the page to do, until the page has done it (`settle`) or a
   *  composer opens again. */
  readonly landed: TextComposerOutcome | undefined;
  /** Opens the composer in the column beside the request's node, or in that node's sheet below
   *  1024 px; the column is drawn for it. Details closes first where it is open (R2). */
  readonly open: (request: TextComposerRequest) => void;
  readonly request: TextComposerRequest | undefined;
  /** Says the page has done what the last send left it to do. */
  readonly settle: () => void;
}

/** The composer's one state for one app: the issue it is open on and the request, and what its
 *  last send left for that issue's page, which the column and the control share wherever each is
 *  drawn on the page; and the column's place for a block's editor's composes while a column is
 *  mounted (`useMarkComposePlace`). */
interface ComposerStore {
  held: { readonly key: string; readonly request: TextComposerRequest } | undefined;
  landed: { readonly key: string; readonly outcome: TextComposerOutcome } | undefined;
  readonly listeners: Set<() => void>;
  place: MarkComposePlace | undefined;
  /** How many sheets stand under the row of each node (`useSheet`), and the nodes that have one,
   *  a set replaced at each change so that a reader of it (`useComposerPlace`) sees a new value. */
  readonly sheetCounts: Map<string, number>;
  sheets: ReadonlySet<string>;
  readonly subscribe: (listener: () => void) => () => void;
}

/** Each app's composer state, by its query client: one store per app, as there is one client,
 *  so it is reached from anywhere under the client's provider with no provider of its own. */
const composerStores = new WeakMap<QueryClient, ComposerStore>();

/** The composer state of `client`'s app, made on first use. */
function composerStoreOf(client: QueryClient): ComposerStore {
  let store = composerStores.get(client);
  if (store === undefined) {
    const listeners = new Set<() => void>();
    store = {
      held: undefined,
      landed: undefined,
      listeners,
      place: undefined,
      sheetCounts: new Map(),
      sheets: new Set(),
      subscribe: (listener) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
    };
    composerStores.set(client, store);
  }
  return store;
}

/**
 * Records that a sheet stands under the row of `nodeId` while the caller is mounted, which a row
 * site mounts at its part's gate (G6). From 1024 px the column takes a composer beside any node;
 * below it only a node's sheet does (R5), so where no row draws one there is no place for it
 * (`useComposerPlace`). With no node, records nothing.
 */
export function useSheet(nodeId: string | undefined): void {
  const store = composerStoreOf(useQueryClient());
  useEffect(() => {
    if (nodeId === undefined) return;
    const change = (by: 1 | -1) => {
      const count = (store.sheetCounts.get(nodeId) ?? 0) + by;
      if (count > 0) store.sheetCounts.set(nodeId, count);
      else store.sheetCounts.delete(nodeId);
      store.sheets = new Set(store.sheetCounts.keys());
      tell(store);
    };
    change(1);
    return () => change(-1);
  }, [store, nodeId]);
}

/**
 * Whether a composer opened on a node has a place at the window's width: the column from 1024 px
 * (`CONTENTS_PANE_QUERY`), below it the node's sheet (`useSheet`). A control that would open a
 * composer with no place stands nowhere, since it would promise a write the page cannot open.
 */
export function useComposerPlace(): (nodeId: string) => boolean {
  const store = composerStoreOf(useQueryClient());
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  const sheets = useSyncExternalStore(store.subscribe, () => store.sheets);
  return useCallback((nodeId) => wide || sheets.has(nodeId), [wide, sheets]);
}

/** Tells each reader of `store` that what it holds changed. */
function tell(store: ComposerStore): void {
  for (const listener of [...store.listeners]) listener();
}

/** Sets what `store` holds - the composer open, and what its last send left - and tells each
 *  reader. A compose of a block's editor that the composer leaves with no send out - closed,
 *  replaced by another, or gone with its page - rejects the editor's promise, so the editor takes
 *  its mark back out of the document; one whose send went out was resolved then, which this does
 *  not undo. */
function holdComposer(
  store: ComposerStore,
  held: ComposerStore["held"],
  landed: ComposerStore["landed"]
): void {
  const left = store.held?.request.mark;
  store.held = held;
  store.landed = landed;
  if (left !== undefined && held?.request.mark !== left) {
    left.reject(new Error("the composer closed with no send out"));
  }
  tell(store);
}

/**
 * Closes the shell's Details drawer where it is open, as its own collapse does: what an address of
 * R8, or a composer of R5, calls before it opens a thread on text, which the column draws only
 * while Details is closed (R2).
 */
export function useCloseDetails(): () => void {
  const drawer = useMarginDrawer();
  return useCallback(() => {
    if (drawer?.open === true) drawer.setOpen(false);
  }, [drawer]);
}

/**
 * The composer's one state for the issue the router is at, which the column draws and the control
 * opens (`SelectionComment`): reachable from anywhere inside the page's providers, since it is
 * the query client's (`composerStoreOf`). A request held for another issue is no composer here.
 */
export function useTextComposer(): TextComposer {
  const store = composerStoreOf(useQueryClient());
  const { pathname, search } = useLocation();
  const key = parseIssuePath(pathname, search)?.key;
  const held = useSyncExternalStore(store.subscribe, () => store.held);
  const landedHeld = useSyncExternalStore(store.subscribe, () => store.landed);
  const closeDetails = useCloseDetails();
  const request = held !== undefined && held.key === key ? held.request : undefined;
  const landed =
    landedHeld !== undefined && landedHeld.key === key ? landedHeld.outcome : undefined;
  return useMemo(
    () => ({
      close: () => {
        if (store.held !== undefined && store.held.key === key) {
          holdComposer(store, undefined, store.landed);
        }
      },
      land: (outcome: TextComposerOutcome) => {
        if (key === undefined) return;
        const open = store.held !== undefined && store.held.key === key;
        holdComposer(store, open ? undefined : store.held, { key, outcome });
      },
      landed,
      open: (next: TextComposerRequest) => {
        if (key === undefined) return;
        closeDetails();
        holdComposer(store, { key, request: next }, undefined);
      },
      request,
      settle: () => {
        if (store.landed !== undefined && store.landed.key === key) {
          holdComposer(store, store.held, undefined);
        }
      },
    }),
    [closeDetails, key, landed, request, store]
  );
}

/**
 * Ends the composer open on the issue `key`, and what its last send left that issue's page to do,
 * as the caller - the column, which goes with its page - unmounts, so neither is open again on a
 * later visit. A composer open on another issue stays.
 */
export function useComposerGoesWithPage(key: string): void {
  const store = composerStoreOf(useQueryClient());
  useEffect(
    () => () => {
      const { held, landed } = store;
      if (held?.key !== key && landed?.key !== key) return;
      holdComposer(
        store,
        held?.key === key ? undefined : held,
        landed?.key === key ? undefined : landed
      );
    },
    [key, store]
  );
}

/**
 * Offers `place` as the column's place for a block's editor's composes (`useMarkComposePlace`)
 * while the caller - the column - is mounted, at every width where a composer has a place: from
 * 1024 px (`wide`), the column's, and below it, while a row draws a sheet (`useSheet`), the sheet
 * draws the same composer. Below 1024 px with no sheet there is no place, and the editor's composer
 * opens in the margin as it always has. The editor is handed one function for as long as the place
 * is offered, which calls `place` as the caller last drew it.
 */
export function useOfferMarkComposePlace(wide: boolean, place: MarkComposePlace): void {
  const store = composerStoreOf(useQueryClient());
  const offered = useSyncExternalStore(store.subscribe, () => wide || store.sheets.size > 0);
  const latest = useRef(place);
  latest.current = place;
  useEffect(() => {
    if (!offered) return;
    const offer: MarkComposePlace = (compose) => latest.current(compose);
    store.place = offer;
    tell(store);
    return () => {
      if (store.place !== offer) return;
      store.place = undefined;
      tell(store);
    };
  }, [offered, store]);
}

/**
 * The column's place for a compose a block's editor asks for (`MarkComposePlace`), which the
 * page hands the editor (`StagedDocument.tsx`): its selection bar and its chord open the
 * composer of R5 in the column, beside the block's node, or in the node's sheet below 1024 px,
 * and never in Details (R6). Undefined while no column of comments on text is mounted, as on
 * every page but the new issue page; with none, the editor's composer opens in the margin as it
 * always has, and Details hosts the editor's cards (`slots/details.tsx`).
 */
export function useMarkComposePlace(): MarkComposePlace | undefined {
  const store = composerStoreOf(useQueryClient());
  return useSyncExternalStore(store.subscribe, () => store.place);
}
