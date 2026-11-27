import { type RefObject, useEffect, useLayoutEffect, useState } from "react";
import { DETAILS_ATTRIBUTE } from "./margin-slot";

/**
 * How tall the band that stays at the top of the screen is right now, on the document element,
 * so anything that must not sit under it reads one number rather than guessing: the tree's
 * focused row scrolls clear of it, and a keyboard target is "on screen" only below this line
 * (`e2e/issue-page.ts`'s `onScreen`). From 761 px up the band is the identity block, the issue's
 * key and title; at 760 px and below the header scrolls away and the band is the filter row,
 * which stays (the prototype's `#hdr-sticky` and `#viewbar`). Unset while no identity block is
 * drawn.
 */
export const IDENTITY_HEIGHT_PROPERTY = "--issue-identity-height";

/** The widths the header scrolls away at and the filter row stays: the prototype's own media
 *  query, which the header's and the filter row's `max-[761px]:` classes repeat, Tailwind's
 *  `max-[761px]:` being a width under 761 px. */
export const HEADER_SCROLLS_QUERY = "(max-width: 760px)";

/** The mark the filter row's own element carries (`IssuePresets.tsx`), by which the band is found
 *  at the widths where the filter row is what stays. */
export const FILTER_ROW_ATTRIBUTE = "data-filter-row";

/** How far the page has to have scrolled before the identity block draws its shadow, in CSS
 *  pixels: the prototype's own `scrollY > 40` for `#hdr-sticky.stuck`. */
const CONDENSED_AFTER = 40;

/** How tall the box at the foot of the screen is while it is drawn - the one place to write,
 *  which `IssueComposer.tsx` publishes under this name on the document element - so a target
 *  brought on screen clears it as it clears the band at the top. Unset while no box is drawn. */
export const FOOT_HEIGHT_PROPERTY = "--issue-composer-height";

/** The attribute the box at the foot of the screen carries (`IssueComposer.tsx`), which
 *  `FOOT_BOX` selects, as `DETAILS_ATTRIBUTE` marks Details. */
export const FOOT_BOX_ATTRIBUTE = "data-foot-box";

/** The box at the foot of the screen. Where Details stands beside the outline, Tab goes from a
 *  row of the outline to the box and through it to Details, so the outline keeps the facts of the
 *  node the focus leaves for the box as it does for Details, until the focus leaves the box for
 *  the rest of the page (`IssueTree.tsx`'s `releaseFocus`, decision 570). */
export const FOOT_BOX = `[${FOOT_BOX_ATTRIBUTE}]`;

/** The test id of the box at the foot of the screen (`IssueComposer.tsx`), by which a test finds
 *  it; the page finds it by `FOOT_BOX`. */
export const FOOT_BOX_TEST_ID = "issue-composer";

/** How far from the band a target is placed when it has to be scrolled to: enough that it does
 *  not sit flush against it. */
export const CLEARANCE = 8;

/**
 * Brings `element` inside the viewport, clear of the band that stays at the top of the screen
 * and of the box that stays at its foot, which is what "on screen" means on this page (R9): the
 * focused row of the tree after every move, and whatever a chord opens (`useIssueKeymap.ts`).
 * Each height is the one number its owner publishes rather than a measurement taken here, so
 * nothing guesses at it. An element already whole and clear is left where it is, so a key never
 * scrolls the page for nothing; one under the foot's box is scrolled up until its bottom clears
 * the box where it fits between the two, and to the top band otherwise.
 *
 * Details' drawer (`margin/Margin.tsx`) is a layer over the page, the block included, with a
 * scroll box of its own: a target inside it is brought into that box, and the page under it,
 * which scrolling could not move the target by, stays where the reader had it.
 */
export function bringOnScreen(element: HTMLElement): void {
  if (element.closest(`aside[${DETAILS_ATTRIBUTE}]`) !== null) {
    element.scrollIntoView({ block: "nearest" });
    return;
  }
  const published = getComputedStyle(document.documentElement);
  const height = (property: string) => {
    const value = Number.parseFloat(published.getPropertyValue(property));
    return Number.isFinite(value) ? value : 0;
  };
  const clear = height(IDENTITY_HEIGHT_PROPERTY);
  const floor = window.innerHeight - height(FOOT_HEIGHT_PROPERTY);
  const box = element.getBoundingClientRect();
  if (box.top >= clear && box.bottom <= floor) return;
  window.scrollBy({
    top:
      box.bottom > floor && box.height <= floor - clear
        ? box.bottom - floor + CLEARANCE
        : box.top - clear - CLEARANCE,
  });
}

/**
 * Publishes the height of the band that stays at the top of the screen as
 * `IDENTITY_HEIGHT_PROPERTY` - `ref`, the identity block, from 761 px up, and the filter row at
 * 760 px and below, where the header scrolls away (`HEADER_SCROLLS_QUERY`) - and answers whether
 * the block is stuck to the top of the screen, which is when it draws its shadow: the prototype's
 * `#hdr-sticky.stuck`. The block keeps its size stuck or not, the title its 22 px type and the
 * key its row, as the approved picture of the scrolled page draws them (63 px at 1440 px); the
 * properties line under it is not in the block, and scrolls away under it.
 *
 * The block draws its shadow once the page has scrolled more than `CONDENSED_AFTER` pixels, as
 * the prototype does, at the widths where it stays: at 760 px and below it scrolls away with the
 * page (`HEADER_SCROLLS_QUERY`), is never stuck, and draws none, and the shadow is measured again
 * when the width crosses that line. `restoringTo` is the place the page is about to put the
 * reader back at, in pixels down the document, where it has one (`IssuePage`'s `panelScroll`):
 * the shadow is drawn for that place in the commit that restores it, rather than a frame later.
 */
export function useIdentityBlock(
  ref: RefObject<HTMLElement | null>,
  restoringTo?: number
): { condensed: boolean } {
  const [stuck, setStuck] = useState(false);

  // The height is measured in a layout effect, so the first frame a reader sees already carries
  // it: a row placed against it on that frame would otherwise be placed against nothing. The
  // filter row is the page body's, drawn beside the header, so it is looked for at each reading
  // rather than once, and observed from when it is first found.
  useLayoutEffect(() => {
    const block = ref.current;
    if (block === null) return;
    const narrow = window.matchMedia(HEADER_SCROLLS_QUERY);
    const sizes = new ResizeObserver(() => publish());
    let watched: Element | null = null;
    const publish = () => {
      const row = document.querySelector(`[${FILTER_ROW_ATTRIBUTE}]`);
      if (row !== watched) {
        if (watched !== null) sizes.unobserve(watched);
        if (row !== null) sizes.observe(row);
        watched = row;
      }
      const band = narrow.matches ? stickyAncestor(row) : block;
      document.documentElement.style.setProperty(
        IDENTITY_HEIGHT_PROPERTY,
        `${band === null ? 0 : Math.round(band.getBoundingClientRect().height)}px`
      );
    };
    sizes.observe(block);
    narrow.addEventListener("change", publish);
    publish();
    return () => {
      sizes.disconnect();
      narrow.removeEventListener("change", publish);
      document.documentElement.style.removeProperty(IDENTITY_HEIGHT_PROPERTY);
    };
  }, [ref]);

  useEffect(() => {
    if (ref.current === null) return;
    const narrow = window.matchMedia(HEADER_SCROLLS_QUERY);
    const measure = () => setStuck(window.scrollY > CONDENSED_AFTER && !narrow.matches);
    window.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", measure);
    narrow.addEventListener("change", measure);
    measure();
    return () => {
      window.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
      narrow.removeEventListener("change", measure);
    };
  }, [ref]);

  // A page restored past `CONDENSED_AFTER` is a page the block draws its shadow on, at the widths
  // where it stays. The measurement above takes over as soon as the reader scrolls again.
  return {
    condensed:
      restoringTo === undefined
        ? stuck
        : restoringTo > CONDENSED_AFTER && !window.matchMedia(HEADER_SCROLLS_QUERY).matches,
  };
}

/** The element that stays at the top of the screen for `element`: itself or its nearest
 *  ancestor that is `position: sticky`, or null where none is. */
function stickyAncestor(element: Element | null): Element | null {
  for (let at = element; at !== null; at = at.parentElement) {
    if (getComputedStyle(at).position === "sticky") return at;
  }
  return null;
}
