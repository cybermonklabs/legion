import { buildDispatchReference, nodeRouteFromHref, parseNodeReference } from "./routes";

/**
 * The address an anchor's `href` names, resolved as the browser resolves it; null where the
 * attribute is blank or absent. The sanitizer draws a link whose scheme it refuses
 * (`javascript:`, `data:`) with an empty `href`, which the browser reads as the page the reader is
 * on, an address the link never named.
 */
export function hrefTarget(anchor: HTMLAnchorElement): string | null {
  return (anchor.getAttribute("href") ?? "").trim() === "" ? null : anchor.href;
}

/**
 * The address `y` copies from a focused link (dispatch://INFRA-2160, R23): the `dispatch://`
 * reference of a link to a Dispatch item, and the URL of any other link. In order: a rendered
 * body's reference link carries its reference as `data-dispatch-ref` (`collectReferenceAnchors`,
 * `referenceTriggerProps`); an editor's link mark, a title the editor draws and a picture's
 * fallback link carry their target as `data-dispatch-href`, read back to the node's own reference
 * where it names one; a link to a page of this application names the item the page shows
 * (`nodeRouteFromHref`); and anything else - another site, a mail address, a page that names no
 * item such as the Inbox - is its resolved `href`. Undefined for an element that carries none of
 * these, which is no link to copy, and for a link with neither data attribute whose `href` names
 * nothing (`hrefTarget`).
 */
export function linkAddress(
  link: Element,
  appOrigin: string = window.location.origin
): string | undefined {
  const reference = link.getAttribute("data-dispatch-ref");
  if (reference !== null) {
    return reference;
  }
  const target = link.getAttribute("data-dispatch-href");
  if (target !== null) {
    const route = parseNodeReference(target);
    return route === undefined ? target : buildDispatchReference(route);
  }
  if (!(link instanceof HTMLAnchorElement)) {
    return undefined;
  }
  const href = hrefTarget(link);
  if (href === null) {
    return undefined;
  }
  const route = nodeRouteFromHref(href, appOrigin);
  return route === undefined ? href : buildDispatchReference(route);
}
