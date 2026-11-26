/**
 * The one way a key opens an item in a new tab (R20): the address of the item's own link, opened
 * with `noopener`, so the new page has no handle on this one. Each Shift+O sits beside its `o`
 * and opens what `o` would open, in a new tab rather than this one. And the way `o` follows a
 * link in this tab where the page takes a plain click on it for itself (`followLink`).
 */

/** Opens `href` in a new tab of the browser. */
export function openInNewTab(href: string): void {
  window.open(href, "_blank", "noopener");
}

/** The address `element` links to: its own `href` where it is a link, else that of the link it
 *  sits in or holds first; null where there is none. */
export function linkHref(element: Element | null): string | null {
  const link = element?.closest("a[href]") ?? element?.querySelector("a[href]") ?? null;
  return link instanceof HTMLAnchorElement ? link.href : null;
}

/** Opens the address `element` links to (`linkHref`) in a new tab, and nothing where it links to
 *  none: what each Shift+O runs on the item it is pressed on. */
export function openLinkInNewTab(element: Element | null): void {
  const href = linkHref(element);
  if (href !== null) openInNewTab(href);
}

/** Follows the link `element` is (`linkHref`) in this tab, as a plain click would where the page
 *  has not taken that click for itself, and does nothing where it links to none: `o` on a thread
 *  row below 1280 px, whose title opens the row in place on a plain click, a script's included
 *  (`ThreadTitle`'s `onOpen`). */
export function followLink(element: Element | null): void {
  const href = linkHref(element);
  if (href !== null) window.location.assign(href);
}
