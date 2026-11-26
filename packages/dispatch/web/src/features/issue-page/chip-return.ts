import { useCallback, useEffect, useLayoutEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import { isPlainClick } from "../refs/picture-lightbox";
import { parseIssuePath } from "../refs/routes";

/** The new issue page's root (`IssueOutlinePage.tsx`), inside which the page draws every row and
 *  every chip. */
const PAGE_ROOT = "[data-issue-page]";

/** A row of the outline or of a view. */
const ROW = "[data-node-row]";

/**
 * A chip's follow to a document of the issue (dispatch://INFRA-1196 part N's R1, R2): what the
 * document it opened goes back to by Escape, the view and the row that hold the chip, with the
 * focus on the chip. It names the row and the link, not their elements: the stage draws the
 * document alone, so the row that held the chip leaves the page with the follow, and the row
 * drawn again on the way back is another element.
 */
export interface ChipRecord {
  /** The address the chip was pressed at, the view and the row that hold it: its path, its
   *  search and its fragment. */
  readonly address: string;
  /** The chip's `href`, as the row draws it, by which the row drawn again names it. */
  readonly href: string;
  /** The row that held the chip: its key (`data-row-key`, `tree-keys.ts`' `rowKeyOf`), or its
   *  node's id where a view's row carries no key. */
  readonly row: string;
}

/** The path of the document of the issue `issueKey` that `link` opens on this page - its own
 *  address, `/issues/<key>/artifacts/<slug>` - or undefined where it opens anything else: an
 *  address of another issue, of another origin, or no document. */
function documentPath(link: HTMLAnchorElement, issueKey: string): string | undefined {
  const url = new URL(link.href, window.location.href);
  if (url.origin !== window.location.origin) return undefined;
  const route = parseIssuePath(url.pathname, url.search);
  return route?.kind === "artifact" && route.key === issueKey ? url.pathname : undefined;
}

/**
 * The record of the chip whose follow opened the document the location draws now
 * (`ChipRecord`), kept by the page. A chip that names a document is drawn by part G's
 * `RowText.tsx`, whose follow navigates in place and hands the location no state, and whose
 * follow this part leaves as it is (root R39 loads `RowText.tsx` in Dashboard's chunk), so the
 * page hears the follow itself: a listener on the page's root, in the capture phase - before the
 * row's own follow moves the router - records each plain activation of a link inside a row to a
 * document of this issue, with the address it was pressed at. The record is the location's that
 * the follow then draws, held by that location's key, so it is read again on Back to the
 * document and on no other location: the same document opened from Documents' list, a version of
 * it opened from its own controls, or a link with a key held, which the browser follows
 * elsewhere, reads none. A link outside every row - Documents' list's, the header's - records
 * nothing, and its document's way back is Documents' list (`document-address.ts`).
 */
export function useChipRecord(issueKey: string): () => ChipRecord | undefined {
  const location = useLocation();
  const here = useRef(location);
  // The chip's follow, recorded as it is pressed, with the path of the document it opens, until
  // the router draws the next location.
  const followed = useRef<{ readonly path: string; readonly record: ChipRecord } | undefined>(
    undefined
  );
  // The record of the location the follow drew, by that location's key.
  const opened = useRef<{ readonly key: string; readonly record: ChipRecord } | undefined>(
    undefined
  );
  useLayoutEffect(() => {
    here.current = location;
    const follow = followed.current;
    followed.current = undefined;
    if (follow !== undefined && follow.path === location.pathname) {
      opened.current = { key: location.key, record: follow.record };
    }
  }, [location]);
  useEffect(() => {
    const root = document.querySelector(PAGE_ROOT);
    if (root === null) return;
    const press = (event: Event) => {
      if (!(event instanceof MouseEvent) || !isPlainClick(event)) return;
      const link = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(link instanceof HTMLAnchorElement)) return;
      if (link.hasAttribute("download") || link.target !== "") return;
      const row = link.closest<HTMLElement>(ROW);
      const key = row?.dataset.rowKey ?? row?.dataset.nodeId;
      const href = link.getAttribute("href");
      const path = documentPath(link, issueKey);
      if (key === undefined || href === null || path === undefined) return;
      const { hash, pathname, search } = here.current;
      followed.current = {
        path,
        record: { address: `${pathname}${search}${hash}`, href, row: key },
      };
    };
    root.addEventListener("click", press, true);
    return () => root.removeEventListener("click", press, true);
  }, [issueKey]);
  return useCallback(() => {
    const record = opened.current;
    return record !== undefined && record.key === here.current.key ? record.record : undefined;
  }, []);
}
