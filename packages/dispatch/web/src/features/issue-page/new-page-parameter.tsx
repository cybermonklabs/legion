import { type ReactNode, useContext, useEffect, useMemo, useRef } from "react";
import {
  createPath,
  type Navigator,
  type Path,
  parsePath,
  type To,
  UNSAFE_NavigationContext,
  useLocation,
} from "react-router-dom";

import { isPlainClick } from "../refs/picture-lightbox";
import { parseIssuePath } from "../refs/routes";

/** The query parameter that opens the new issue page on one issue's addresses. */
const PARAMETER = "issue_page";

/** The one value of `PARAMETER` that opens the new page; any other is as no parameter. */
const NEW_PAGE = "new";

/**
 * The key of the issue whose address `pathname` + `search` is, where that address carries
 * `?issue_page=new`, and undefined for every other address: an issue's address without it or with
 * another value of it, and the address of any page that is no issue's.
 */
export function newPageIssue(pathname: string, search: string): string | undefined {
  if (new URLSearchParams(search).get(PARAMETER) !== NEW_PAGE) return undefined;
  return parseIssuePath(pathname, search)?.key;
}

/**
 * `to`, read as a path, with `?issue_page=new` added where it is an address of the issue `key`
 * that names no `issue_page` of its own, after the parameters it has, its hash kept; every other
 * target as it is.
 */
function keepNewPage(key: string, to: To): Partial<Path> {
  const path = typeof to === "string" ? parsePath(to) : to;
  const search = path.search ?? "";
  if (parseIssuePath(path.pathname ?? "")?.key !== key) return path;
  if (new URLSearchParams(search).has(PARAMETER)) return path;
  return { ...path, search: `${search.length > 1 ? `${search}&` : "?"}${PARAMETER}=${NEW_PAGE}` };
}

/**
 * Keeps `?issue_page=new` on the addresses of the issue it opened the new page on. While the
 * address carries it, the router's navigator is handed on with `keepNewPage` applied to every
 * address it is given: the `href` of each link drawn under this (which a copy and a new tab
 * take) and each move a link, a key or the page makes (whose history entry a reload and Back
 * open). So the address carries the parameter before the page draws it, and every page that
 * reads the choice reads it there. A move to another issue or to a page that is no issue's adds
 * nothing, and from there the reader's setting holds again. A push to the address the reader is
 * at, once the parameter is added, replaces that entry, by the router's own rule for a link
 * (`createPath`, which counts an empty `?` or `#` as nothing): the router compares a link's
 * target, which lacks the parameter, with the address, which carries it, and so pushes where it
 * would replace. A link the router does not draw, a rendered body's `<a href>`, is heard on the
 * document, after every handler of the page: a plain click on one to an address of the issue
 * that names no `issue_page`, which no handler has taken (a `Link`, a row's words that follow
 * their own in place), is followed through the same push, so the document is not loaded again at
 * the address without the parameter. A click with a key held or with another button, and a link
 * to a download, to another window or to another origin, are the browser's. A load of an address
 * is not seen here. With no parameter in the address the router's own navigator is handed on
 * unchanged, and no click is heard. No `<Router>` may stand inside another, so this provides the
 * router's own navigation context again, its navigator alone replaced.
 */
export function KeepNewIssuePage({ children }: { children: ReactNode }): ReactNode {
  const outer = useContext(UNSAFE_NavigationContext);
  const location = useLocation();
  const key = newPageIssue(location.pathname, location.search);
  // The address the reader is at, read as a push is made: the context below changes only with
  // the issue and the router's own, never at each move within the issue.
  const here = useRef<Path>(location);
  here.current = location;
  const context = useMemo(() => {
    if (key === undefined) return outer;
    const { navigator } = outer;
    const keeping: Navigator = {
      createHref: (to) => navigator.createHref(keepNewPage(key, to)),
      encodeLocation: navigator.encodeLocation,
      go: (delta) => navigator.go(delta),
      push: (to, state, options) => {
        const kept = keepNewPage(key, to);
        if (createPath(kept) === createPath(here.current)) navigator.replace(kept, state, options);
        else navigator.push(kept, state, options);
      },
      replace: (to, state, options) => navigator.replace(keepNewPage(key, to), state, options),
    };
    return { ...outer, navigator: keeping };
  }, [key, outer]);
  // The navigator handed on, read as a link is pressed: the listener below is added once for each
  // issue kept, and the navigator is made again whenever the router's own changes.
  const handedOn = useRef(context.navigator);
  handedOn.current = context.navigator;
  useEffect(() => {
    if (key === undefined) return;
    // In the bubble phase on the document, so every handler of the page has run first: React
    // hears a click on its root, below the document, and a handler that took it prevented it.
    const follow = (event: MouseEvent) => {
      if (event.defaultPrevented || !isPlainClick(event)) return;
      const anchor = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(anchor instanceof HTMLAnchorElement)) return;
      if (anchor.hasAttribute("download") || anchor.hasAttribute("target")) return;
      const url = new URL(anchor.href, window.location.href);
      if (url.origin !== window.location.origin) return;
      // A link that names the parameter itself, a hash link among them (its `href` is resolved
      // against the address, which carries it), is the browser's.
      if (parseIssuePath(url.pathname)?.key !== key || url.searchParams.has(PARAMETER)) return;
      event.preventDefault();
      handedOn.current.push({ hash: url.hash, pathname: url.pathname, search: url.search });
    };
    document.addEventListener("click", follow);
    return () => document.removeEventListener("click", follow);
  }, [key]);
  return (
    <UNSAFE_NavigationContext.Provider value={context}>
      {children}
    </UNSAFE_NavigationContext.Provider>
  );
}
