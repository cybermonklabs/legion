import { type ReactNode, useContext, useMemo } from "react";
import {
  type Navigator,
  parsePath,
  type To,
  UNSAFE_NavigationContext,
  useLocation,
} from "react-router-dom";

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
 * `to` with `?issue_page=new` added where it is an address of the issue `key` that names no
 * `issue_page` of its own, after the parameters it has, its hash kept; every other target as it
 * is.
 */
function keepNewPage(key: string, to: To): To {
  const path = typeof to === "string" ? parsePath(to) : to;
  const search = path.search ?? "";
  if (parseIssuePath(path.pathname ?? "")?.key !== key) return to;
  if (new URLSearchParams(search).has(PARAMETER)) return to;
  return { ...path, search: `${search.length > 1 ? `${search}&` : "?"}${PARAMETER}=${NEW_PAGE}` };
}

/**
 * Keeps `?issue_page=new` on the addresses of the issue it opened the new page on. While the
 * address carries it, the router's navigator is handed on with `keepNewPage` applied to every
 * address it is given: the `href` of each link drawn under this (which a copy and a new tab
 * take) and each move a link, a key or the page makes (whose history entry a reload and Back
 * open). So the address carries the parameter before the page draws it, and every page that
 * reads the choice reads it there. A move to another issue or to a page that is no issue's adds
 * nothing, and from there the reader's setting holds again. A load of an address and a link
 * outside the router are not seen here. With no parameter in the address the router's own
 * navigator is handed on unchanged. No `<Router>` may stand inside another, so this provides the
 * router's own navigation context again, its navigator alone replaced.
 */
export function KeepNewIssuePage({ children }: { children: ReactNode }): ReactNode {
  const outer = useContext(UNSAFE_NavigationContext);
  const { pathname, search } = useLocation();
  const key = newPageIssue(pathname, search);
  const context = useMemo(() => {
    if (key === undefined) return outer;
    const { navigator } = outer;
    const keeping: Navigator = {
      createHref: (to) => navigator.createHref(keepNewPage(key, to)),
      encodeLocation: navigator.encodeLocation,
      go: (delta) => navigator.go(delta),
      push: (to, state, options) => navigator.push(keepNewPage(key, to), state, options),
      replace: (to, state, options) => navigator.replace(keepNewPage(key, to), state, options),
    };
    return { ...outer, navigator: keeping };
  }, [key, outer]);
  return (
    <UNSAFE_NavigationContext.Provider value={context}>
      {children}
    </UNSAFE_NavigationContext.Provider>
  );
}
