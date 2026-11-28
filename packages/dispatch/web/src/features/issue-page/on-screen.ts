import { FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";

/** The band of the screen the page shows clear of everything that stays over it, in the
 *  viewport's px (`screenBand`). */
export interface ScreenBand {
  readonly bottom: number;
  readonly top: number;
}

/**
 * The band of the screen clear of everything that stays over the page: below the band at the top
 * (`IDENTITY_HEIGHT_PROPERTY`) and below the page's `scroll-padding-top`, which at 760 px and below
 * is the view row that stays there, and above the box at the foot (`FOOT_HEIGHT_PROPERTY`), where
 * a box there publishes its height; with none published, the band runs to the screen's foot. Each
 * height is the one number its owner publishes, as the page's one rule for bringing a target on
 * screen (`identity-block.ts`'s `bringOnScreen`) and a focused row's own placing (`NodeRow.tsx`)
 * read them. Read once for a walk over many rows, since nothing in the walk changes them.
 */
export function screenBand(): ScreenBand {
  const published = getComputedStyle(document.documentElement);
  const height = (property: string) => Number.parseFloat(published.getPropertyValue(property)) || 0;
  return {
    bottom: window.innerHeight - height(FOOT_HEIGHT_PROPERTY),
    top: Math.max(
      height(IDENTITY_HEIGHT_PROPERTY),
      Number.parseFloat(published.scrollPaddingTop) || 0
    ),
  };
}

/** Whether `element` stands whole inside `band`. Where it does, neither `bringOnScreen` nor a
 *  focused row's own placing moves the page, so it can take the focus where it stands. */
export function wholeIn(element: Element, band: ScreenBand): boolean {
  const box = element.getBoundingClientRect();
  return box.top >= band.top && box.bottom <= band.bottom;
}

/** Whether `element` stands whole on screen, clear of everything that stays over the page
 *  (`screenBand`). */
export function wholeOnScreen(element: Element): boolean {
  return wholeIn(element, screenBand());
}
