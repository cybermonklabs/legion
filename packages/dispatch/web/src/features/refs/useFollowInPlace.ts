import { type MouseEvent, useCallback } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { dispatchOrigins } from "../../api/origins";
import { isPlainClick } from "./picture-lightbox";

/** The path roots the server answers itself and the page has no route for: its API, sign-in,
 *  sockets, health and built assets (`serverRoots` and `assetRoots` of
 *  `packages/envoy/internal/dispatch/routes/router.go`). */
const SERVER_ROOT = /^\/(?:api|v1|auth|ws|healthz|assets)(?:\/|$)/u;

/**
 * Follows an address of this application in drawn words in place, on a plain click, as a link
 * the router draws does: the words are drawn as HTML (`MarkdownBody`, `linkRowReferences`), and
 * their links would load the whole page again - and with it every part of the page that holds
 * state, such as the command bar's panel at the side. An address at any origin of this Dispatch
 * (`dispatchOrigins`) is followed on the page's own, where the reader holds a session. A click
 * with a key held or with another button, a link to a download, to another window, to an origin
 * Dispatch does not name or to a path the server answers itself (`SERVER_ROOT`), and a click a
 * picture or a router link already answered (`DispatchPicture`, `Link`) are the browser's. A link
 * to the address the reader is at replaces it, as the router's `Link` does, so Back leaves the
 * page as it does from a tie's chip to that address. Put it on the element that holds the words,
 * as its `onClick`.
 */
export function useFollowInPlace(): (event: MouseEvent<HTMLElement>) => void {
  const navigate = useNavigate();
  const here = useLocation();
  const current = `${here.pathname}${here.search}${here.hash}`;
  return useCallback(
    (event: MouseEvent<HTMLElement>) => {
      if (event.defaultPrevented || !isPlainClick(event.nativeEvent)) return;
      const anchor = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(anchor instanceof HTMLAnchorElement) || !event.currentTarget.contains(anchor)) return;
      if (anchor.hasAttribute("download") || anchor.getAttribute("target") !== null) return;
      const url = new URL(anchor.href, window.location.href);
      if (!dispatchOrigins().includes(url.origin) || SERVER_ROOT.test(url.pathname)) return;
      event.preventDefault();
      const path = `${url.pathname}${url.search}${url.hash}`;
      navigate(path, { replace: path === current });
    },
    [current, navigate]
  );
}
