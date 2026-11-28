import { type RefObject, useCallback, useEffect, useMemo, useRef } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { parseIssuePath } from "../refs/routes";
import { bringOnScreen } from "./identity-block";
import { issueAddress, presetPath } from "./issue-address";
import { abortOnReaderAct } from "./useNodeEditing";

/**
 * The presses of the header's count of what waits on the reader (dispatch://INFRA-3940 R5, the
 * plan's ST-3). What the count counts is Dashboard's list (`features/dashboard/useWaitingItems.ts`,
 * dispatch://INFRA-5702/spec#b-r-header-count), so a press walks that list and no other:
 *
 * - Where Dashboard is not what the address draws - another view, Documents' list, an opened
 *   document, or Dashboard's own address with a node beside it, which the outline draws - the
 *   press pushes Dashboard's explicit address, which closes a document, and the focus lands on
 *   the list's first item once Dashboard has drawn it (`useWaitingPress`).
 * - Where Dashboard is drawn, the bare address included, the press writes no history entry and
 *   moves the focus (`useWaitingWalk`): onto the item after the one the focus last stood on, by
 *   the list as it stands at the press, which draws the next five when that item is the sixth,
 *   and onto the first item, the list drawing its first five again, after the last.
 *
 * A press is held here, in memory, and not in the history entry it writes: an entry's state
 * outlives the press, so Back, Forward or a reload would put it to work again. A press whose focus
 * is still owed - Dashboard is not drawn yet, its list is out, or the item is one the list has not
 * drawn yet - ends at the reader's next act (`abortOnReaderAct`, decisions 375 and 413), at the
 * focus it lands, or where the address leaves Dashboard, so it never lands after the reader has
 * moved on.
 */

/** What a press asks of the Dashboard that is drawn. */
interface Walk {
  readonly advance: () => void;
}

/** The Dashboard drawn on the new page, which a press moves the focus in. */
let walker: Walk | undefined;

/** The press whose focus is still owed. */
let owed: AbortController | undefined;

/** Holds a press whose focus is owed, in place of the one held before it. */
function owe(): void {
  owed?.abort();
  const press = abortOnReaderAct(() => {
    if (owed === press) owed = undefined;
  });
  owed = press;
}

/** Ends the press whose focus is owed: it landed, or it is no longer the reader's. */
function settle(): void {
  owed?.abort();
  owed = undefined;
}

/** The mark every item of the list carries (`dashboard/WaitingList.tsx`), by which this finds the
 *  item that holds the focus and the item it moves the focus to; the rest is the item's key, its
 *  kind and its id. */
const ITEM_PREFIX = "dashboard-item-";

/** The item the focus last stood on, and the order of the list when it did. */
interface Stood {
  readonly key: string;
  readonly order: readonly string[];
}

/**
 * The item the next press moves the focus to, by the list `keys` as it stands at the press: the
 * item after the one the focus last stood on, `wrapped` where that is past the last and the press
 * comes round to the first; the first item where the focus stood on none. An item answered since
 * the focus stood on it is no longer in the list, so the press goes on to the next item after it,
 * in the order the list had then, that the list still holds, or comes round where none is.
 */
function nextWaiting(
  keys: readonly string[],
  stood: Stood | undefined
): { readonly key: string; readonly wrapped: boolean } | undefined {
  const first = keys.at(0);
  if (first === undefined) return undefined;
  if (stood === undefined) return { key: first, wrapped: false };
  const at = keys.indexOf(stood.key);
  if (at !== -1) {
    const next = keys.at(at + 1);
    return next === undefined ? { key: first, wrapped: true } : { key: next, wrapped: false };
  }
  const after = stood.order
    .slice(stood.order.indexOf(stood.key) + 1)
    .find((key) => keys.includes(key));
  return after === undefined ? { key: first, wrapped: true } : { key: after, wrapped: false };
}

/** The item `key` as the list draws it, or null while the list draws none. */
function itemOf(box: HTMLElement | null, key: string): HTMLElement | null {
  const wanted = `${ITEM_PREFIX}${key}`;
  for (const item of box?.querySelectorAll<HTMLElement>(`[data-testid^="${ITEM_PREFIX}"]`) ?? []) {
    if (item.dataset.testid === wanted) return item;
  }
  return null;
}

/**
 * The walk of the Dashboard drawn on the new page (`features/dashboard/DashboardTab.tsx`, which
 * holds how many items of the list are drawn): the focus on each item of `items`, one press after
 * the other. `active` is false on today's page, which draws no count and takes no press. The press
 * lands once `loaded` says the list holds every read its items come from, on the item the walk
 * names then, and a press that needs the sixth item draws the next five first and lands once they
 * are drawn. Returns the ref of the element that holds the list, in which an item that takes the
 * focus, by a press or by the reader's own move, is the one the next press goes on from.
 */
export function useWaitingWalk({
  active,
  drawn,
  items,
  loaded,
  setDrawn,
  step,
}: {
  active: boolean;
  /** How many items the list draws. */
  drawn: number;
  items: readonly { readonly id: string; readonly kind: string }[];
  loaded: boolean;
  setDrawn: (count: number) => void;
  /** How many items one more draw adds. */
  step: number;
}): RefObject<HTMLDivElement | null> {
  const box = useRef<HTMLDivElement>(null);
  const keys = useMemo(() => items.map((item) => `${item.kind}-${item.id}`), [items]);
  const stood = useRef<Stood | undefined>(undefined);
  // What a press reads is what stands as it is made, not what stood when the walk was handed out.
  const latest = useRef({ drawn, keys, loaded, setDrawn });
  latest.current = { drawn, keys, loaded, setDrawn };
  const advance = useCallback(() => {
    const now = latest.current;
    if (!now.loaded) {
      if (owed === undefined) owe();
      return;
    }
    const next = nextWaiting(now.keys, stood.current);
    if (next === undefined) {
      settle();
      return;
    }
    const at = now.keys.indexOf(next.key);
    if (at >= now.drawn) now.setDrawn(Math.ceil((at + 1) / step) * step);
    else if (next.wrapped && now.drawn > step) now.setDrawn(step);
    const item = itemOf(box.current, next.key);
    if (item === null) {
      if (owed === undefined) owe();
      return;
    }
    // The item takes the focus as the walk's stop and is no Tab stop. The list draws it with no
    // tab index (`WaitingList.tsx`, INFRA-5702's), so the walk gives the item one as it lands.
    item.tabIndex = -1;
    item.focus({ preventScroll: true });
    bringOnScreen(item);
    settle();
  }, [step]);
  useEffect(() => {
    if (!active) return;
    const mine: Walk = { advance };
    walker = mine;
    return () => {
      if (walker === mine) walker = undefined;
    };
  }, [active, advance]);
  useEffect(() => {
    const list = box.current;
    if (!active || list === null) return;
    const stand = (event: FocusEvent) => {
      const item =
        event.target instanceof Element
          ? event.target.closest<HTMLElement>(`[data-testid^="${ITEM_PREFIX}"]`)
          : null;
      const key = item?.dataset.testid?.slice(ITEM_PREFIX.length);
      if (key !== undefined) stood.current = { key, order: latest.current.keys };
    };
    list.addEventListener("focusin", stand);
    return () => list.removeEventListener("focusin", stand);
  }, [active]);
  // A press whose focus is owed lands in the commit that draws the item it names: the first item
  // once the list holds it, or the sixth once the next five are drawn. Every render looks, since
  // that is when the list, the reads and the number drawn move; it acts once, and not at all after
  // the reader's act.
  useEffect(() => {
    if (active && loaded && owed !== undefined) advance();
  });
  return box;
}

/**
 * The press of the header's count, for the issue `issueKey`: where Dashboard is what the address
 * draws, it asks the Dashboard drawn to walk (`useWaitingWalk`), or holds the press for the
 * Dashboard that is still being fetched; anywhere else it holds the press and pushes Dashboard's
 * explicit address. A press held while the address leaves Dashboard, or while the page is left, is
 * dropped, so it is never the reader's after they moved on.
 */
export function useWaitingPress(issueKey: string): () => void {
  const navigate = useNavigate();
  const { hash, pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const address = route === undefined ? undefined : issueAddress(route, search, hash, undefined);
  const onDashboard = address?.preset === "dashboard" && address.target === undefined;
  useEffect(() => {
    if (!onDashboard) settle();
  }, [onDashboard]);
  useEffect(() => settle, []);
  return () => {
    if (!onDashboard) {
      owe();
      navigate(presetPath(issueKey, "dashboard"));
    } else if (walker === undefined) owe();
    else walker.advance();
  };
}
