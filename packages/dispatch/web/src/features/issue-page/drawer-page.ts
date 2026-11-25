import { useEffect, useMemo } from "react";

import { primarySpec } from "../../api/issue-cache";
import type { IssueDetails } from "../../api/types";
import { parseIssuePath } from "../refs/page-routes";
import { issueAddress } from "./issue-address";
import { type MarginDrawerPage, useMarginDrawer } from "./margin-slot";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * Hands the shell's Details drawer what it draws in the new page's words and the address rule it
 * follows, for as long as the page's root is mounted (R4, R5, R12): the
 * drawer is the shell's and is loaded with every page, so it imports neither the page's word list
 * nor its model, and reads both from here through `MarginDrawer.page`.
 *
 * The rule is `issue-address.ts`'s: an address names an item by its own route, `?ask=` or
 * `?comment=`, and that item stands beside the document the address opens only where the address
 * opens one at it - `?mark=`, or the item at a pinned version (`documentItem`). `issue` is the
 * issue as the root read it, whose spec a `/spec` address names; undefined while it is read.
 */
export function useDrawerPage(issue: IssueDetails | undefined): void {
  const setPage = useMarginDrawer()?.setPage;
  const specSlug = primarySpec(issue)?.slug;
  const page = useMemo<MarginDrawerPage>(
    () => ({
      chrome: pageChrome,
      routeItem: ({ hash, pathname, search }) => {
        const route = parseIssuePath(pathname, search);
        if (route === undefined) return undefined;
        const address = issueAddress(route, search, hash, specSlug);
        return address.target?.kind === "item"
          ? { besideDocument: address.documentItem !== undefined, id: address.target.id }
          : undefined;
      },
      words: { close: CONTROL_LABELS.close, details: CONTROL_LABELS.details },
    }),
    [specSlug]
  );
  useEffect(() => {
    if (setPage === undefined) return;
    setPage(page);
    return () => setPage(undefined);
  }, [page, setPage]);
}
