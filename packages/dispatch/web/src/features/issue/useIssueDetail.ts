import { itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import { issueQuery, userStateQuery } from "../../api/queries";
import { artifactAtAddress } from "../refs/reference-target";
import { type IssueRoute, isPrimaryDocumentArtifactRoute, issueTabForRoute } from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";

export function useIssueDetail(route: IssueRoute) {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const artifactRouteSlug = artifactRoute?.slug;
  const query = new URLSearchParams(search);
  const queryItem = itemFromSearch(search);
  const commentId = queryItem?.kind === "comment" ? queryItem.id : undefined;
  const askId = queryItem?.kind === "ask" ? queryItem.id : undefined;
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const issue = useQuery(issueQuery(route.key));
  const state = useQuery(userStateQuery());
  const planArtifact = roleDocument(issue.data, "plan");
  const verificationArtifact = roleDocument(issue.data, "verification");
  const primaryArtifact = primarySpec(issue.data);
  // A page opened at a document's current name, which a reference may write where its slug kept
  // an earlier name, shows that document as the link to it drew it (`artifactAtAddress`).
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? primaryArtifact
      : artifactAtAddress(issue.data?.artifacts, artifactRouteSlug);
  const isPrimaryArtifactRoute = isPrimaryDocumentArtifactRoute(route, selectedArtifact);
  const activeTab = issueTabForRoute(route, selectedArtifact);
  const conversationFocusItemId =
    route.kind === "ask" || route.kind === "comment" || route.kind === "message"
      ? route.id
      : undefined;

  return {
    activeTab,
    artifactRoute,
    askId,
    commentId,
    conversationFocusItemId,
    isPrimaryArtifactRoute,
    highlightTerm,
    issue,
    planArtifact,
    primaryArtifact,
    selectedArtifact,
    state,
    verificationArtifact,
  };
}
