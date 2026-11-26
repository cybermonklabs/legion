import type { QueryClient } from "@tanstack/react-query";

import type { Artifact, ArtifactWithTrees, DocumentRole, Issue, IssueDetails } from "./types";

/** The issue's primary document (its spec), or undefined until the details have loaded. It is
 *  the details' own row, so its approval carries the tree of its review reason. */
export function primarySpec(
  issue: Pick<IssueDetails, "artifacts" | "primary_artifact_id"> | undefined
): ArtifactWithTrees | undefined {
  return issue?.artifacts?.find((artifact) => artifact.id === issue.primary_artifact_id);
}

/** The issue's document in `role` - `verification.md` or `plan.md`, which the issue names in
 *  `verification_artifact_id` and `plan_artifact_id` - or undefined while the details are
 *  unread, when the issue holds none (`""`), and on a Dispatch older than that role (absent). The
 *  id the issue names is the one answer, so a document whose name was changed by hand cannot put
 *  a second document on the tab. A decisions document is no part this build reads: decisions are
 *  nodes of the spec. */
export function roleDocument(
  issue:
    | Pick<IssueDetails, "artifacts" | "plan_artifact_id" | "verification_artifact_id">
    | undefined,
  role: Extract<DocumentRole, "plan" | "verification">
): ArtifactWithTrees | undefined {
  const id = role === "plan" ? issue?.plan_artifact_id : issue?.verification_artifact_id;
  return id === undefined || id === ""
    ? undefined
    : issue?.artifacts?.find((artifact) => artifact.id === id);
}

/** The artifact among `artifacts` that an address names - the `<name>` of an `artifact/<name>`
 *  reference or of an issue's `/artifacts/<name>` page - found as the server's read route finds it
 *  (`resolveDocumentReference`): the artifact whose slug it is, else the document whose name it
 *  is. A document renamed to a slug-shaped name keeps the slug its first name gave it, so an
 *  address written with its current name opens it and is drawn as it. An owner's names and slugs
 *  never name two artifacts (`renameArtifact`), and only a document, never a file or an image, is
 *  found by its name. One rule for every list that answers it - an issue summary's documents, an
 *  issue's artifacts - so a link drawn from a reference and the page it opens find the same
 *  artifact. The name is the document read's alone: the block read and the reference graph
 *  resolve only the slug, so a block named through the name answers not-found and reads as its
 *  words, and a reference written through the name is no backlink of the document. The picker
 *  writes only the slug. Undefined until the list is read, and once it is read without it. */
export function artifactAtAddress<T extends Pick<Artifact, "kind" | "name" | "slug">>(
  artifacts: readonly T[] | null | undefined,
  address: string
): T | undefined {
  const listed = artifacts ?? [];
  return (
    listed.find((candidate) => candidate.slug === address) ??
    listed.find((candidate) => candidate.kind === "doc" && candidate.name === address)
  );
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
