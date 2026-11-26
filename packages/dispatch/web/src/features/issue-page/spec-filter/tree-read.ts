/**
 * `read` as an observer of the key the page's tree fills (`useIssueTree.ts`), which sends no
 * request of its own: it is never enabled, so drawing it, drawing it again once its answer has
 * aged, or the window taking the focus asks for nothing, and the caller draws whatever the
 * tree's own read holds. Its `refetch` still fetches, which is what a Retry asks. The Spec view
 * watches each read it draws from with it, and the contents slot the spec's blocks.
 */
export function treeRead<T extends object>(read: T) {
  return { ...read, enabled: false } as const;
}
