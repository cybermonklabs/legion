import type { QueryClient } from "@tanstack/react-query";

import type { Artifact, DocumentRole, Issue, IssueDetails } from "./types";

/** The issue's primary document (its spec), or undefined until the details have loaded. */
export function primarySpec(
  issue: Pick<IssueDetails, "artifacts" | "primary_artifact_id"> | undefined
): Artifact | undefined {
  return issue?.artifacts?.find((artifact) => artifact.id === issue.primary_artifact_id);
}

type RoleDocuments = Pick<
  IssueDetails,
  "artifacts" | "plan_artifact_id" | "queries" | "verification_artifact_id"
>;

/**
 * The document the issue names for its plan or its verification query (`queries`), or undefined
 * while the details are unread and where the issue names none. The id the issue names is the one
 * answer, never a guess from an artifact's name or role, so a document whose name was changed by
 * hand cannot put a second document on the tab. Where the issue read carries `queries` they
 * decide alone.
 */
export function roleDocument(
  issue: RoleDocuments | undefined,
  role: Extract<DocumentRole, "plan" | "verification">
): Artifact | undefined {
  const id =
    issue?.queries === undefined
      ? keptRoleDocumentId(issue, role)
      : issue.queries.find((query) => query.query === role)?.artifact_id;
  return id === undefined || id === ""
    ? undefined
    : issue?.artifacts?.find((artifact) => artifact.id === id);
}

/**
 * The id a server of the release before names for `role`. It answers an issue read with no
 * `queries`, and names the document in `plan_artifact_id` or `verification_artifact_id`, `""`
 * where the issue holds none, as the page read it before `queries`.
 *
 * kept for one release: role-artifact-ids
 */
function keptRoleDocumentId(
  issue: RoleDocuments | undefined,
  role: Extract<DocumentRole, "plan" | "verification">
): string | undefined {
  return role === "plan" ? issue?.plan_artifact_id : issue?.verification_artifact_id;
}

/**
 * Applies a PATCH response to the cached `IssueDetails` for its key. The response carries only
 * the `Issue` fields, so they are merged over the details-only fields until the next full fetch.
 * `route_status` is one of those, judged for the route the details were read with: a new route
 * clears it, its holder and its reason rather than wear the old route's, and the issue's own
 * event refetches them.
 */
export function mergeIssue(queryClient: QueryClient, next: Issue): void {
  queryClient.setQueryData<IssueDetails>(["issue", next.key], (current) => {
    if (current === undefined) {
      return undefined;
    }
    const reach =
      current.route === next.route
        ? {}
        : { route_status: null, route_holder: null, route_status_reason: null };
    return { ...current, ...next, ...reach };
  });
}

/**
 * What every write of one issue's fields does once Dispatch has taken it: the answer merged
 * over the cached details, or - where the write landed with no answer to give - the issue read
 * again rather than guessed at, and the issue lists refreshed either way.
 *
 * One landing for them all (each write of the issue's fields in
 * `features/issue-page/useIssueHeaderWrites.ts`, and any later write of them from the new issue
 * page), so a field edited in Details and a field edited in the header leave the cache in the
 * same state.
 */
export function landIssueWrite(queryClient: QueryClient, key: string, next: Issue | null): void {
  if (next === null) {
    void queryClient.invalidateQueries({ queryKey: ["issue", key] });
  } else {
    mergeIssue(queryClient, next);
  }
  void queryClient.invalidateQueries({ queryKey: ["issues"] });
}
