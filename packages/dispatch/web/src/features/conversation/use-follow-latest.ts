import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { putTheBoxAt, scrollportTop, whatScrolls, whereTheReaderIs } from "../doc/readers-place";
import { onMarkdownLatePaint } from "../refs/useRenderedMarkdown";

const FOLLOW_POSITION_TOLERANCE_PX = 1;
/** Every card and reply carries `data-turn`; the first in the document is the newest card. */
const NEWEST_TURN_SELECTOR = "[data-turn]";
/** The Conversation's docked composer: sticky above the list from `sm` up, fixed at the foot of
 *  the screen below it (`MentionComposer`'s docked form). */
const COMPOSER_SELECTOR = '[aria-label="Comment composer"]';

/** The box that carries the conversation: the column of a reading pane, or the page itself
 *  (`null`) where the page scrolls it. Every reading of where the reader is below is taken
 *  against it rather than against the window, which in a column is a number the reader never
 *  moves - so the reader always read as following the latest turn, and every placement was
 *  written to a page the column does not move (`features/doc/readers-place.ts`). The box is
 *  found from the newest card, or from the composer while no card is drawn. */
function theBoxThatCarriesThem(): HTMLElement | null {
  const inIt =
    document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR) ??
    document.querySelector<HTMLElement>(COMPOSER_SELECTOR);
  return inIt === null ? null : whatScrolls(inIt);
}

/**
 * The band of the screen the docked composer covers, as an IntersectionObserver `rootMargin`: the
 * top of the screen down to the sticky composer's bottom from `sm` up, or the foot of the screen
 * up to the fixed composer's top below it. A card's head in that band is under the composer.
 */
function composerBand(): string {
  const composer = document.querySelector<HTMLElement>(COMPOSER_SELECTOR);
  if (composer === null) return "0px";
  const style = window.getComputedStyle(composer);
  if (style.position === "sticky") {
    const top = Number.parseFloat(style.top) || 0;
    return `${-Math.round(top + composer.offsetHeight)}px 0px 0px 0px`;
  }
  if (style.position === "fixed") {
    const covered = window.innerHeight - composer.getBoundingClientRect().top;
    return `0px 0px ${-Math.max(0, Math.round(covered))}px 0px`;
  }
  return "0px";
}

export function useFollowLatest({
  enabled,
  latestItemSeq,
  turnList,
  onFirstPlacement,
  ownSendCount,
}: {
  enabled: boolean;
  /** A number that rises with the newest card's recency, or `undefined` until that card is on
   *  the page (while the caller's list is loading, failed or empty): the hook follows that card
   *  and needs nothing else about the list. It spends its one first placement on the first
   *  defined value, so a caller must not pass one before the card renders. */
  latestItemSeq: number | undefined;
  /** The list of turns the caller draws, whose first `[data-turn]` is the newest card, or `null`
   *  while no list is drawn. An IntersectionObserver on that card says whether any of it is on
   *  screen, which costs no layout read on the scroll path. */
  turnList: HTMLElement | null;
  /** Called once, in the layout effect that spends the first placement, right after the reader
   *  is placed at the newest card - so before any parent layout effect of that commit. It tells
   *  the caller the reader has just been placed for the first time: a scroll position recorded
   *  before the list was ever on the page (a tab switch's, say) is stale and must not be
   *  applied over this placement. A later arrival, own send or re-enable never calls it. */
  onFirstPlacement?: () => void;
  ownSendCount: number;
}): {
  atTop: boolean;
  jumpToLatest: () => void;
  /** Some part of the newest card is in the visible band (the screen less the docked composer's
   *  band), so the reader already sees where a jump would take them: nothing is left for `Jump to
   *  latest` to offer, and the pill drawn over the card it points at would cover that card's own
   *  controls (dispatch://INFRA-5200). Any part, not its head, so a card taller than the room
   *  under the pill is never under it either. */
  newestCardOnScreen: boolean;
  pinnedToTop: () => boolean;
} {
  const [atTop, setAtTop] = useState(true);
  const [newestCardOnScreen, setNewestCardOnScreen] = useState(false);
  const atTopRef = useRef(true);
  const previousItemSeq = useRef<number | undefined>(undefined);
  const previousSends = useRef(ownSendCount);
  /** Where the last placement left that box, so the reader's own scroll is told from it. */
  const followedAt = useRef<number | undefined>(undefined);
  // The latest callback, read when the placement happens, so a new identity never re-runs the
  // placement effect. Declared before it, so it is current in the same commit.
  const onFirstPlacementRef = useRef(onFirstPlacement);
  useLayoutEffect(() => {
    onFirstPlacementRef.current = onFirstPlacement;
  });
  const followsLatest = useCallback(() => {
    const at = whereTheReaderIs(theBoxThatCarriesThem());
    return (
      at <= 0 ||
      (followedAt.current !== undefined &&
        Math.abs(at - followedAt.current) <= FOLLOW_POSITION_TOLERANCE_PX)
    );
  }, []);

  // The newest turn must sit immediately beneath the sticky composer. Recording the actual
  // post-scroll position distinguishes that automated placement from a reader's later scroll,
  // regardless of whatever height the issue header has at this breakpoint. The composer stands
  // above the list only where it is sticky, from `sm` up; below `sm` it is fixed at the foot of
  // the screen (`MentionComposer`'s docked form), so the newest turn goes to the top of the
  // viewport and the composer's height, which its hint and a reply chip grow, is no offset. An
  // offset of the foot composer's height lands the newest turn under that composer once twice
  // its height and the turn's no longer fit above it (`e2e/phone-landing.e2e.ts`). The turn's
  // top is a viewport reading and the place written is one down the box, so the top of the box
  // is what stands between them: zero on a document page, the top of the column's padding box
  // (`scrollportTop`) in a reading pane.
  const jumpToLatest = useCallback(() => {
    const newest = document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR);
    const scroller = theBoxThatCarriesThem();
    if (newest === null) {
      putTheBoxAt(scroller, 0);
    } else {
      const composer = document.querySelector<HTMLElement>(COMPOSER_SELECTOR);
      const composerAbove =
        composer !== null && window.getComputedStyle(composer).position !== "fixed";
      const composerHeight = composerAbove ? composer.getBoundingClientRect().height : 0;
      const target =
        whereTheReaderIs(scroller) +
        newest.getBoundingClientRect().top -
        scrollportTop(scroller) -
        composerHeight;
      putTheBoxAt(scroller, Math.max(target, 0));
    }
    followedAt.current = whereTheReaderIs(scroller);
  }, []);
  useEffect(() => {
    const onScroll = () => {
      const nextAtTop = followsLatest();
      atTopRef.current = nextAtTop;
      setAtTop(nextAtTop);
    };
    // `capture`, because a column's scroll does not bubble: the reading above is taken against
    // the box carrying the conversation, and in a reading pane that box is the column.
    window.addEventListener("scroll", onScroll, { capture: true, passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll, true);
  }, [followsLatest]);

  // Whether any part of the newest card is in the visible band is read from that card itself,
  // through an IntersectionObserver: the browser reports a crossing of the band's edge off the
  // scroll path, so a scroll costs no layout read (`ConversationTab.test.tsx` holds a scroll to
  // none). The band the docked composer covers is no part of the screen here - a card under the
  // composer is a card the reader cannot see - so the observer is built with that band taken off
  // its root (`composerBand`), and built again when the band moves (the composer growing with a
  // hint or a reply chip, or a resize that docks it elsewhere) or when another card becomes the
  // newest, which a change of the list's children says.
  useEffect(() => {
    // Nothing is watched while the caller is not live: a hidden tab draws no pill.
    if (turnList === null || !enabled) {
      setNewestCardOnScreen(false);
      return;
    }
    let observer: IntersectionObserver | null = null;
    let band = "";
    let card: Element | null = null;
    const watch = () => {
      const nextBand = composerBand();
      const nextCard = turnList.querySelector(NEWEST_TURN_SELECTOR);
      if (observer !== null && nextBand === band && nextCard === card) return;
      band = nextBand;
      card = nextCard;
      observer?.disconnect();
      observer = null;
      if (card === null) {
        setNewestCardOnScreen(false);
        return;
      }
      observer = new IntersectionObserver(
        (entries) => {
          const last = entries.at(-1);
          if (last !== undefined) setNewestCardOnScreen(last.isIntersecting);
        },
        { rootMargin: band }
      );
      observer.observe(card);
    };
    watch();
    const cards = new MutationObserver(watch);
    cards.observe(turnList, { childList: true });
    const composer = document.querySelector<HTMLElement>(COMPOSER_SELECTOR);
    const sizes = composer === null ? null : new ResizeObserver(watch);
    if (composer !== null) sizes?.observe(composer);
    window.addEventListener("resize", watch);
    return () => {
      window.removeEventListener("resize", watch);
      sizes?.disconnect();
      cards.disconnect();
      observer?.disconnect();
    };
  }, [enabled, turnList]);

  // The mutation's `onSuccess` bumps `ownSendCount` and invalidates the events query in the same
  // tick, but the event feed (api/event-feed.ts) can independently invalidate the same query
  // from the server push — sometimes *before* the mutation's own callback runs. `ownSendCount` and
  // `latestItemSeq` therefore change in either order across renders, not always together, so this
  // single effect reacts to a change in either: an own send always follows regardless of scroll
  // position, and — because reaching this branch already re-settles the reader at the newest turn
  // — a same-render or later arrival from anyone else is never mistaken for the send once
  // `ownSendCount` has been recorded.
  useLayoutEffect(() => {
    if (!enabled) {
      return;
    }
    const ownSend = ownSendCount !== previousSends.current;
    previousSends.current = ownSendCount;
    if (latestItemSeq === undefined) {
      return;
    }
    const first = previousItemSeq.current === undefined;
    const changed = previousItemSeq.current !== latestItemSeq;
    previousItemSeq.current = latestItemSeq;
    if (!changed && !ownSend) {
      return;
    }
    // A server event may render before the browser dispatches the scroll event that preceded it.
    // Measure now rather than trusting the listener's last state, or that arrival can pull a reader
    // back to the newest turn before their scroll was observed.
    const followsLatestNow = first || ownSend || followsLatest();
    atTopRef.current = followsLatestNow;
    setAtTop(followsLatestNow);
    if (followsLatestNow) {
      jumpToLatest();
    }
    if (first) {
      onFirstPlacementRef.current?.();
    }
  }, [enabled, followsLatest, jumpToLatest, latestItemSeq, ownSendCount]);

  // A body longer than the parse worker's threshold formats a frame or more after the commit
  // that drew it (`refs/markdown-engine.ts`), which is after the placement above: where the turn
  // that just formatted is the newest one and the reader is still at it, it is placed again,
  // once per late paint. A reader who has scrolled away keeps their place: `followsLatest` is
  // what says so, as it does for an arrival.
  useEffect(
    () =>
      onMarkdownLatePaint((element) => {
        if (!enabled || !followsLatest()) {
          return;
        }
        if (document.querySelector(NEWEST_TURN_SELECTOR)?.contains(element) === true) {
          jumpToLatest();
        }
      }),
    [enabled, followsLatest, jumpToLatest]
  );

  return {
    atTop,
    jumpToLatest,
    newestCardOnScreen,
    pinnedToTop: followsLatest,
  };
}
