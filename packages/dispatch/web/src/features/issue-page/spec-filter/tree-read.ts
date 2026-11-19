import type { QueryStatus } from "@tanstack/react-query";

/**
 * `read` as an observer of the key the page's tree fills (`useIssueTree.ts`), which sends no
 * request of its own: it is never enabled, so drawing it, drawing it again once its answer has
 * aged, or the window taking the focus asks for nothing, and the caller draws whatever the
 * tree's own read holds. Its `refetch` still fetches, which is what a Retry asks. Its age and
 * its fetch on focus are set as the page sets them for a read another surface fills
 * (`refetchFailedOnFocus`), though TanStack reads neither for an observer it never enables. The
 * Spec view watches each read it draws from with it, and the contents slot the spec's blocks.
 */
export function treeRead<T extends object>(read: T) {
  return {
    ...read,
    enabled: false,
    refetchOnWindowFocus: refetchFailedOnFocus,
    staleTime: Number.POSITIVE_INFINITY,
  } as const;
}

/**
 * Window focus fetches a read again only while it is in error, so a read that failed recovers
 * on the open page while a healthy one stays quiet. A copy of `refs/reference-target.ts`'s own,
 * which that module keeps to itself; this module imports it in place of this copy once that
 * module exports it.
 */
const refetchFailedOnFocus = (query: { readonly state: { readonly status: QueryStatus } }) =>
  query.state.status === "error" ? "always" : false;
