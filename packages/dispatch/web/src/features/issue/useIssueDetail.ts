import { itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";

import { issueQuery, userStateQuery } from "../../api/queries";
import type { IssueRoute } from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";
import { issueDocuments } from "./issue-documents";

export function useIssueDetail(route: IssueRoute) {
  const { search } = useLocation();
  const artifactRoute = route.kind === "artifact" ? route : undefined;
  const query = new URLSearchParams(search);
  const queryItem = itemFromSearch(search);
  const commentId = queryItem?.kind === "comment" ? queryItem.id : undefined;
  const askId = queryItem?.kind === "ask" ? queryItem.id : undefined;
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const issue = useQuery(issueQuery(route.key));
  const state = useQuery(userStateQuery());
  const { activeTab, drawn, isPrimaryArtifactRoute } = issueDocuments(route, issue.data);
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
    drawn,
    isPrimaryArtifactRoute,
    highlightTerm,
    issue,
    state,
  };
}
