import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import type { IssueDetails } from "../../api/types";
import type { IssueRoute } from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";

/**
 * What the new issue page's stage and view row read off the issue and its address
 * (dispatch://INFRA-1196 part N's R1, R3, R6): the document or file the address opens in place of
 * the outline, the issue's spec and the documents of the plan's and the checks' roles, which a
 * view's version control reads, and the first word `?q=` marks in an opened document.
 *
 * The page's root has read the issue already (`IssueOutlinePage.tsx`), so this reads nothing of
 * its own. `main`'s `features/issue/useIssueDetail.ts` is today's page's, and does not change.
 */
export function useIssueDetail(issue: IssueDetails, route: IssueRoute) {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const artifactRouteSlug = artifactRoute?.slug;
  const highlightTerm = firstHighlightTerm(new URLSearchParams(search).get("q") ?? "");
  // The document or file the address opens in place of the outline (R3), the spec among them:
  // its own address is where every document of the issue is opened.
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? undefined
      : issue.artifacts?.find(({ slug }) => slug === artifactRouteSlug);
  return {
    artifactRoute,
    highlightTerm,
    planArtifact: roleDocument(issue, "plan"),
    primaryArtifact: primarySpec(issue),
    selectedArtifact,
    verificationArtifact: roleDocument(issue, "verification"),
  };
}
