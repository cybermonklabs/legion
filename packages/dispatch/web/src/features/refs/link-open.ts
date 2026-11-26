import type { OpenLink } from "@legion/proof-editor";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useLayoutEffect, useRef } from "react";
import { useNavigate } from "react-router-dom";

import { linkHref, openInNewTab } from "../shell/new-tab";
import { hrefTarget } from "./link-address";
import { MARKDOWN_BODY_SELECTOR } from "./markdown-engine";
import { fetchReferenceHref, needsSpecSlug } from "./reference-target";
import { type DispatchReferenceRoute, parseNodeReference, referenceHref } from "./routes";

/**
 * How a link of a document or a rendered body opens (dispatch://INFRA-2160, R1), the one rule for
 * every surface: a `dispatch://` reference and a link to a page of this application open that
 * page in the same tab, with no new load of the application; another site, a mail address and a
 * phone number open in a new tab; a Ctrl, Cmd or middle click opens a Dispatch page in a new tab
 * too; any other scheme opens nothing, and so does a reference to a person, whom no page shows
 * (R5). The document editor calls it through its `openLink` option (`@legion/proof-editor`'s
 * `src/dispatch-link-open.ts`, from `ProofDocument`, `VersionView` and `PriorityDescription`),
 * and `LinkClickHost` for every rendered body. A key that follows a link in this tab follows it
 * by the same rule, save that another site loads in this tab (`useLinkFollower`).
 */

/** Same-origin paths the server answers itself rather than this application: its API (the bytes
 *  routes a picture or a download reads included), sign-in and the sockets
 *  (`packages/envoy/internal/dispatch/routes/router.go`). */
const SERVER_PATH = /^\/(?:api|auth|ws)(?:\/|$)/;

/** Where a link's target goes: a Dispatch node by its reference, a page of this application, a
 *  same-origin address the server answers itself, or away from Dispatch. A page carries its path,
 *  which the router opens in place, and its whole address on this origin, which a new tab opens:
 *  the path alone can start with `//` (`/.//elsewhere.example/x`, or this origin followed by
 *  `//elsewhere.example/x`), and `window.open` reads such a path as an address on another site. */
export type LinkDestination =
  | { readonly kind: "reference"; readonly route: DispatchReferenceRoute }
  | { readonly href: string; readonly kind: "page"; readonly path: string }
  | { readonly kind: "server"; readonly href: string }
  | { readonly kind: "away"; readonly href: string };

/** Where `target`, a link's own target read against `page` (the address the reader is on), goes;
 *  undefined for a scheme no link of Dispatch opens (`javascript:`, `data:`) or a `dispatch://`
 *  reference that names nothing. */
export function linkDestination(
  target: string,
  page: string = window.location.href
): LinkDestination | undefined {
  if (target.startsWith("dispatch://")) {
    // The target is a node its author named, and opening it is a read, so it is read as a node's
    // reference, whatever the length of its name.
    const route = parseNodeReference(target);
    return route === undefined ? undefined : { kind: "reference", route };
  }
  let url: URL;
  try {
    url = new URL(target, page);
  } catch {
    return undefined;
  }
  if (url.protocol === "mailto:" || url.protocol === "tel:") {
    return { href: url.href, kind: "away" };
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    return undefined;
  }
  if (url.origin !== new URL(page).origin) {
    return { href: url.href, kind: "away" };
  }
  if (SERVER_PATH.test(url.pathname)) {
    return { href: url.href, kind: "server" };
  }
  return { href: url.href, kind: "page", path: `${url.pathname}${url.search}${url.hash}` };
}

/** The page's opener of a link's destination, as `linkDestination` reads its target, in the page
 *  or, where `newTab`, in a new tab. It is one function for as long as the component lives, and
 *  opens through the router and the query client of the latest render: the editor reads its
 *  options once, and `LinkClickHost` adds its listener once. */
function useDestinationOpener(): (
  destination: LinkDestination | undefined,
  newTab: boolean
) => void {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const latest = useRef({ navigate, queryClient });
  latest.current = { navigate, queryClient };
  return useCallback((destination, newTab) => {
    const { navigate, queryClient } = latest.current;
    const go = (path: string) => {
      if (newTab) {
        openInNewTab(path);
      } else {
        navigate(path);
      }
    };
    switch (destination?.kind) {
      case "away":
        window.open(destination.href, "_blank", "noopener,noreferrer");
        return;
      case "server":
        // The editor has already taken the click, so the load the browser would have made is
        // made here.
        if (newTab) {
          openInNewTab(destination.href);
        } else {
          window.location.assign(destination.href);
        }
        return;
      case "page":
        // A new tab opens the page's whole address, never its path (`LinkDestination` says why).
        // The router reads the path as one on this origin whatever run of slashes starts it: React
        // Router's `resolvePath` collapses the run to one.
        if (newTab) {
          openInNewTab(destination.href);
        } else {
          navigate(destination.path);
        }
        return;
      case "reference":
        // A block of a spec pinned to a version opens that version under the spec's slug,
        // which only the issue names; every other reference's href is known at once. A
        // reference that opens no page of this dashboard - a person, whom nothing shows (R5) -
        // goes nowhere: the press is taken all the same and stops here.
        if (needsSpecSlug(destination.route)) {
          void fetchReferenceHref(queryClient, destination.route).then((href) => {
            if (href !== undefined) go(href);
          });
        } else {
          const href = referenceHref(destination.route);
          if (href !== undefined) go(href);
        }
        return;
      case undefined:
        return;
    }
  }, []);
}

/** The page's link opener: `target` is a link's own target - a `dispatch://` reference, a web
 *  address, a `mailto:` address - and `newTab` whether the reader asked for a new tab. It is one
 *  function for as long as the component lives, as `useDestinationOpener`'s is. */
export function useLinkOpener(): OpenLink {
  const openDestination = useDestinationOpener();
  return useCallback(
    (target, { newTab }) => openDestination(linkDestination(target), newTab),
    [openDestination]
  );
}

/** The way a key follows a link in this tab where the page takes a plain click on the link for
 *  itself: `o` on a thread's short row below 1280 px, whose title opens the row in place on a
 *  plain click, a script's included (`ThreadTitle`'s `onOpen`). The address `element` links to
 *  (`linkHref`) opens as a click on it would: a page of this application through the router,
 *  with no new load of the application and nothing it holds lost. Another site loads in this
 *  tab, the tab the key asked for, rather than in a new one. Nothing opens where `element` links
 *  to none. It is one function for as long as the component lives, as `useDestinationOpener`'s
 *  is. */
export function useLinkFollower(): (element: Element | null) => void {
  const openDestination = useDestinationOpener();
  return useCallback(
    (element: Element | null) => {
      const href = linkHref(element);
      if (href === null) return;
      const destination = linkDestination(href);
      if (destination?.kind === "away") {
        window.location.assign(destination.href);
        return;
      }
      openDestination(destination, false);
    },
    [openDestination]
  );
}

/**
 * The one click handler of every rendered body (`MarkdownBody` in comments, messages, asks and
 * agent conversations, and a live `MarkdownPreview`), mounted once in the shell beside
 * `RefPreviewHost`: a primary click with no modifier on a link inside a body opens it by the rule
 * above. A click a handler already took (a picture opening its lightbox, a router link, the
 * document editor's own link) is left alone, and so are a modifier click and a middle click,
 * which the browser opens in a new tab from the link's `href` - a reference link's is its page
 * (`collectReferenceAnchors`) - and a link to an address the server answers itself. A link whose
 * `href` names nothing (`hrefTarget`) and that has neither data attribute opens nothing, whatever
 * the click.
 *
 * Its listener is attached in the commit that first draws the host, as `KeymapProvider`'s keydown
 * listener is, so no body drawn in that commit is in the page before the host listens. `AuthGate`
 * draws the shell in a render its own passive effect starts (`shellFor`), and a passive effect of
 * the commit that render makes runs in a later task, after the browser may have painted a body
 * and taken a click on one of its links, which the browser then followed itself: a page of
 * Dispatch with a new load of the application, another site in this tab.
 */
export function LinkClickHost(): ReactNode {
  const openDestination = useDestinationOpener();
  useLayoutEffect(() => {
    const onClick = (event: MouseEvent) => {
      if (event.defaultPrevented) {
        return;
      }
      // The link and the body from the path the click was dispatched along, not from the
      // target's ancestors now: a handler nearer the target (React's, at the root) may have
      // drawn the link's words again already, leaving the target out of the page while the
      // browser still follows the link it was in.
      const path = event.composedPath();
      const anchor = path.find(
        (node): node is HTMLAnchorElement =>
          node instanceof HTMLAnchorElement && node.hasAttribute("href")
      );
      if (
        anchor === undefined ||
        !path.some((node) => node instanceof Element && node.matches(MARKDOWN_BODY_SELECTOR))
      ) {
        return;
      }
      const target =
        anchor.getAttribute("data-dispatch-ref") ??
        anchor.getAttribute("data-dispatch-href") ??
        hrefTarget(anchor);
      if (target === null) {
        event.preventDefault();
        return;
      }
      if (event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) {
        return;
      }
      const destination = linkDestination(target);
      if (destination?.kind === "server") {
        return;
      }
      event.preventDefault();
      openDestination(destination, false);
    };
    document.addEventListener("click", onClick);
    // A browser that sends a middle click as an `auxclick` alone opens a new tab from it.
    document.addEventListener("auxclick", onClick);
    return () => {
      document.removeEventListener("click", onClick);
      document.removeEventListener("auxclick", onClick);
    };
  }, [openDestination]);
  return null;
}
