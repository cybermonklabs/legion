// biome-ignore assist/source/organizeImports: this file, which today's pages load, changes by added lines only (R12 of INFRA-5706), so the focus's context and its reader come in on an import of their own
import { type RefObject, useCallback, useLayoutEffect, useRef, useState } from "react";
import { createContext, useContext } from "react";

/** The room between two cards one region of the margin stacks, in pixels. */
const CARD_GAP = 8;

/**
 * Where the anchored region's own content starts, in viewport terms, with the margin's scroll
 * taken back out. A card's `top` is an offset inside that content, so measuring a mark against
 * the region's *current* box folds the margin's scroll into the placement: the margin scrolls a
 * linked card into view, the next relayout places the card that much lower, the column grows,
 * and the correction scrolls further still. Subtracting the scroll of every scrollport above the
 * region leaves the mark's offset in the document's own layout, which is all a placement may
 * depend on. One read per layout pass: the answer is the same for every card in it.
 */
function regionOrigin(region: HTMLElement): number {
  let scrolled = 0;
  const root = region.ownerDocument.documentElement;
  for (let node = region.parentElement; node !== null && node !== root; node = node.parentElement) {
    scrolled += node.scrollTop;
  }
  return region.getBoundingClientRect().top + scrolled;
}

export { regionOrigin };

/**
 * The key of the card in focus in a region's stack, or none: the column of comments on text
 * provides it (R2 of INFRA-5706), and a region under no provider stacks as it always has.
 */
export const CardStackFocus = createContext<string | undefined>(undefined);

/**
 * The tops of one region's cards with `focused` the key of the card in focus (`CardStackFocus`):
 * `keys` the cards in their order down the region, `tops` the top each card wants (none where it
 * wants none yet), `heights` each card's height. Every card stands as `useCardStack` places it -
 * at the larger of its wanted top and 8 px under the card before it, and under the card before it
 * where it wants none - but the card in focus, which stands at its wanted top. Each card above it
 * moves up as far as it must to end 8 px or more above the card under it, and each card under it
 * stands at the larger of its wanted top and 8 px under the card above. With no card in focus,
 * or one that wants no top, the tops are the stack's own.
 */
export function focusTops(
  keys: readonly string[],
  tops: ReadonlyMap<string, number | undefined>,
  heights: ReadonlyMap<string, number>,
  focused: string | undefined
): ReadonlyMap<string, number> {
  const at = focused === undefined ? -1 : keys.indexOf(focused);
  const pinned = focused === undefined || at < 0 ? undefined : tops.get(focused);
  const placed = new Map<string, number>();
  let bottom = 0;
  keys.forEach((key, index) => {
    const top =
      index === at && pinned !== undefined ? pinned : Math.max(tops.get(key) ?? bottom, bottom);
    placed.set(key, top);
    bottom = top + (heights.get(key) ?? 0) + CARD_GAP;
  });
  if (pinned === undefined) return placed;
  // Up from the card in focus: each card ends 8 px or more above the card under it.
  let ceiling = pinned;
  for (const key of keys.slice(0, at).reverse()) {
    const top = Math.min(placed.get(key) ?? 0, ceiling - CARD_GAP - (heights.get(key) ?? 0));
    placed.set(key, top);
    ceiling = top;
  }
  return placed;
}

/** One region's stack of cards (`useCardStack`). */
export interface CardStack {
  /** The ref of each card's box, by its key, which the stack reads the card's height from. */
  readonly cardRef: (key: string) => (element: HTMLDivElement | null) => void;
  /** How tall the region stands: the bottom of its lowest card. */
  readonly height: number;
  /** The ref of the region the cards are stacked in, whose origin each card is placed from. */
  readonly region: RefObject<HTMLElement | null>;
  /** A card's top inside the region, by its key: where the stack placed it, and the region's top
   *  for a card it has not placed yet, for the one commit before its pass, which runs before the
   *  page is painted. One rule for every region, so none places a card by a read of its own. */
  readonly topOf: (key: string) => number;
}

/**
 * The cards of one region of the margin - the anchored comments (`ThreadList.tsx`), the
 * decisions beside the blocks they shaped (`DecisionCard.tsx`) - stacked down it in their order:
 * each at the height it wants beside the document (`wantedTop`, read against the region's own
 * origin, `regionOrigin`), below the card before it where the two would overlap, and below the
 * card before it for one that wants no height yet. The tops and the region's height are read in
 * one pass, again whenever a card resizes, so the region follows a card the moment it changes.
 * Read at render instead, the height held a card's height from the render before: a reply
 * composer's picker that opened and closed inside a card left the region its open height, until
 * an unrelated render - the hover a pointer makes on its way to Send - read it again and the
 * margin's scroll clamped under the press. A pass that reads the same tops hands back the map
 * the stack already holds, so it draws no card again. A pass reads nothing while the region is
 * not drawn, which it is not while it holds no card. `keyOf` and `wantedTop` must keep one
 * identity while what they answer stays the same (`useCallback`), or every render reads again.
 * Under a `CardStackFocus` the card in focus stands at its wanted top, and the cards around it
 * make room for it (`focusTops`).
 */
export function useCardStack<Card>(
  cards: readonly Card[],
  keyOf: (card: Card) => string,
  wantedTop: (card: Card, origin: number) => number | undefined
): CardStack {
  const region = useRef<HTMLElement | null>(null);
  const boxes = useRef(new Map<string, HTMLDivElement>());
  const [tops, setTops] = useState<ReadonlyMap<string, number>>(() => new Map());
  const [height, setHeight] = useState(0);
  // The card in focus, where the region provides one (`CardStackFocus`), as each pass reads it.
  const focused = useContext(CardStackFocus);
  const focus = useRef(focused);
  focus.current = focused;
  const focusMeasured = useRef(focused);
  const measure = useCallback(() => {
    if (region.current === null) return;
    const origin = regionOrigin(region.current);
    const next = new Map<string, number>();
    let bottom = 0;
    for (const card of cards) {
      const key = keyOf(card);
      const top = Math.max(wantedTop(card, origin) ?? bottom, bottom);
      next.set(key, top);
      bottom = top + (boxes.current.get(key)?.offsetHeight ?? 0) + CARD_GAP;
    }
    // A region that provides a focus stands the card in focus level with what it marks, and the
    // cards around it make room (`focusTops`); every other region's pass ends with the loop.
    const inFocus = focus.current;
    if (inFocus !== undefined && next.has(inFocus)) {
      const keys = cards.map(keyOf);
      const heights = new Map(keys.map((key) => [key, boxes.current.get(key)?.offsetHeight ?? 0]));
      const wanted = new Map(cards.map((card) => [keyOf(card), wantedTop(card, origin)]));
      const placed = focusTops(keys, wanted, heights, inFocus);
      for (const [key, top] of placed) next.set(key, top);
      const last = keys.at(-1);
      if (last !== undefined) {
        bottom = (placed.get(last) ?? 0) + (heights.get(last) ?? 0) + CARD_GAP;
      }
    }
    setTops((current) =>
      current.size === next.size && [...next].every(([key, top]) => current.get(key) === top)
        ? current
        : next
    );
    setHeight(bottom);
  }, [cards, keyOf, wantedTop]);
  useLayoutEffect(() => {
    measure();
  }, [measure]);
  useLayoutEffect(() => {
    const observer = new ResizeObserver(measure);
    for (const box of boxes.current.values()) {
      observer.observe(box);
    }
    return () => observer.disconnect();
  }, [measure]);
  // A move of the focus moves the cards though none of them changed, so it measures again.
  useLayoutEffect(() => {
    if (focusMeasured.current === focused) return;
    focusMeasured.current = focused;
    measure();
  }, [focused, measure]);
  const cardRef = useCallback(
    (key: string) => (element: HTMLDivElement | null) => {
      if (element === null) {
        boxes.current.delete(key);
      } else {
        boxes.current.set(key, element);
      }
    },
    []
  );
  const topOf = useCallback((key: string) => tops.get(key) ?? 0, [tops]);
  return { cardRef, height, region, topOf };
}
