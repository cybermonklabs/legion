import { type RefObject, useEffect, useLayoutEffect, useState } from "react";
import { DETAILS_ATTRIBUTE } from "../margin/margin-slot";

/**
 * How tall the band that stays at the top of the screen is right now, on the document element,
 * so anything that must not sit under it reads one number rather than guessing: the tree's
 * focused row scrolls clear of it (dispatch://INFRA-1196 R9), and a keyboard target is "on
 * screen" only below this line (`e2e/issue-page.ts`'s `onScreen`). From 761 px up the band is the
 * identity block, the issue's key and title; at 760 px and below the header scrolls away and the
 * band is the filter row, which stays (ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md,
 * the prototype's `#hdr-sticky` and `#viewbar`). Unset while no identity block is drawn.
 */
export const IDENTITY_HEIGHT_PROPERTY = "--issue-identity-height";

/** The widths the header scrolls away at and the filter row stays: the prototype's own media
 *  query, which the header's and the filter row's `max-[760px]:` classes repeat. */
export const HEADER_SCROLLS_QUERY = "(max-width: 760px)";

/** The mark the filter row's own element carries (`IssuePresets.tsx`), by which the band is found
 *  at the widths where the filter row is what stays. */
export const FILTER_ROW_ATTRIBUTE = "data-filter-row";

/** How far the properties line has to have gone under the identity block before the block counts
 *  as stuck, in CSS pixels: a fractional layout leaves a hair between the two without the page
 *  having scrolled. */
const STUCK_SLACK = 1;

/** How tall the box at the foot of the screen is while it is drawn - the one place to write,
 *  which `IssueComposer.tsx` publishes under this name on the document element - so a target
 *  brought on screen clears it as it clears the band at the top. Unset while no box is drawn. */
export const FOOT_HEIGHT_PROPERTY = "--issue-composer-height";

/** How far from the band a target is placed when it has to be scrolled to: enough that it does
 *  not sit flush against it. */
const CLEARANCE = 8;

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
 * The block is stuck once the properties line, the element after it, has started to go under it.
 * `restoringTo` is the place the page is about to put the reader back at, in pixels down the
 * document, where it has one (`IssuePage`'s `panelScroll`): the shadow is drawn for that place
 * in the commit that restores it, rather than a frame later.
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
    const block = ref.current;
    if (block === null) return;
    const measure = () => {
      const under = block.nextElementSibling?.getBoundingClientRect().top;
      setStuck(
        under === undefined
          ? window.scrollY > STUCK_SLACK
          : under < block.getBoundingClientRect().bottom - STUCK_SLACK
      );
    };
    window.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", measure);
    measure();
    return () => {
      window.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
    };
  }, [ref]);

  // The block sits at the top of the page's own column, so a page restored past `STUCK_SLACK`
  // is a page the block is stuck on, at the widths where it stays. The measurement above takes
  // over as soon as the reader scrolls again.
  return {
    condensed:
      restoringTo === undefined
        ? stuck
        : restoringTo > STUCK_SLACK && !window.matchMedia(HEADER_SCROLLS_QUERY).matches,
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
