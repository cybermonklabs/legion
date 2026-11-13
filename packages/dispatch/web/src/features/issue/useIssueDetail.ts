import { itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import { issueQuery, userStateQuery } from "../../api/queries";
import { type IssueRoute, issueTabForRoute } from "../refs/routes";
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
  // The document or file the address opens in place of the outline (R3), the spec among them:
  // its own address is where every document of the issue is opened.
  const selectedArtifact =
    artifactRouteSlug === undefined
      ? undefined
      : issue.data?.artifacts?.find(({ slug }) => slug === artifactRouteSlug);
  const activeTab = issueTabForRoute(route);
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
    highlightTerm,
    issue,
    planArtifact,
    primaryArtifact,
    selectedArtifact,
    state,
    verificationArtifact,
  };
}
