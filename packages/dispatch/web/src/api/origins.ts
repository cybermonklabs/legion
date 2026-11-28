/**
 * The origins of this Dispatch, as the page knows them. A deployment may serve addresses beside
 * its canonical one, and still read links at addresses it served before; `/auth/whoami` names
 * them all (`AuthenticatedUser.origins`: the canonical one, each served address, each former
 * address) and `api.whoAmI` hands them here, so the page reads them from the server at run time
 * and never from its build. A dashboard URL of any of them is a reference
 * (`referenceRouteFromHref`), and opens on the origin the page is on (`hereHref`), where the
 * reader holds a session.
 *
 * A deployment that lists no second address names none, and the page reads every dashboard URL
 * against its own origin alone.
 */

/** The origins the last whoami named, the canonical one first; empty where it named none. */
let named: readonly string[] = [];

/** `dispatchOrigins`'s answer for the page's origin it was made on, kept so a reading memoized
 *  on it (`useTextReferences`) keeps one identity while neither changes. */
let answer: { here: string; origins: readonly string[] } | undefined;

/** Records the origins whoami named (`[]` where it named none). The same origins named again,
 *  as every refetch of whoami does, change nothing. */
export function setDispatchOrigins(origins: readonly string[]): void {
  if (origins.length === named.length && origins.every((origin, at) => origin === named[at])) {
    return;
  }
  named = [...origins];
  answer = undefined;
}

/** The origins a dashboard URL is read against: the page's own first, then each other origin
 *  whoami named. Where it named none, the page's own alone. */
export function dispatchOrigins(): readonly string[] {
  const here = window.location.origin;
  if (answer?.here !== here) {
    answer = { here, origins: [...new Set([here, ...named])] };
  }
  return answer.origins;
}

/** `href` as it opens on the origin the page is on: a URL of another origin of this Dispatch
 *  keeps its path, query and fragment on the page's own; any other href - one of an origin
 *  Dispatch does not name, or one that names no origin and so already opens on the page's own -
 *  is left as it is. */
export function hereHref(href: string): string {
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return href;
  }
  const here = window.location.origin;
  if (url.origin === here || !dispatchOrigins().includes(url.origin)) {
    return href;
  }
  return `${here}${url.pathname}${url.search}${url.hash}`;
}
