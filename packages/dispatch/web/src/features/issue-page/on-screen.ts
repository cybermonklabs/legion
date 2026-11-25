import { FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";

/**
 * Whether `element` stands whole on screen, clear of everything that stays over the page: below
 * the band at the top (`IDENTITY_HEIGHT_PROPERTY`) and below the page's `scroll-padding-top`,
 * which at 760 px and below is the view row that stays there, and above the box at the foot
 * (`FOOT_HEIGHT_PROPERTY`). Where this holds, neither the page's one rule for bringing a target
 * on screen (`identity-block.ts`'s `bringOnScreen`) nor a focused row's own placing (`NodeRow.tsx`)
 * moves the page, so a target that meets it can take the focus where it stands. Each height is
 * the one number its owner publishes, as those two read them.
 */
export function wholeOnScreen(element: Element): boolean {
  const published = getComputedStyle(document.documentElement);
  const height = (property: string) => Number.parseFloat(published.getPropertyValue(property)) || 0;
  const top = Math.max(
    height(IDENTITY_HEIGHT_PROPERTY),
    Number.parseFloat(published.scrollPaddingTop) || 0
  );
  const box = element.getBoundingClientRect();
  return box.top >= top && box.bottom <= window.innerHeight - height(FOOT_HEIGHT_PROPERTY);
}
