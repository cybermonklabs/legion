package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/projects/{key}/related-searches", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Run the related-item check an issue create runs over a title and a text as long "+
			"as an issue's spec {title?, text?, exclude_issue?, exclude_issues?, keywords_only?} (checked as the title, a "+
			"newline and the text; either alone is enough), writing nothing - no issue, ask, suggestion or "+
			"related_checks record - and waiting at most 5 seconds. A title over %d characters is 400 CAP_EXCEEDED, "+
			"and the text is held to the body's 1 MiB bound. exclude_issue leaves that issue of the project and "+
			"everything it holds out of the results and out of the word statistics; exclude_issues leaves each of up to "+
			"%d issues of the project out of the results alone (400 EXCLUDE_ISSUE_INPUT for a key the project does not "+
			"hold, or a longer list); keywords_only true skips meaning search. Answers the create's advice.suggestions "+
			"shape: outcome found, none or not_made (a blank text is not_made, the reason in missing), search, "+
			"search_note, search_query and related, with up to 3 open issues before any item of another kind and "+
			"everything a done issue owns last. include=other_projects adds other_projects beside related: at most 2 "+
			"answered question asks of other projects, each with its ask state, its answer and the project it stands in, "+
			"ranked by meaning alone and offered with no claim that they answer the question. 404 PROJECT_NOT_FOUND for "+
			"a project that does not exist. It answers for both forms of /api/v1/projects/{key}/related, so the Link of "+
			"that route's GET names this POST: the GET's query fields go in this route's JSON body.",
			contracts.IssueTitleMax, relatedExcludeLimit),
		Handler:  s.postRelatedSearchV2,
		Resource: "related-searches", Kind: kindComputation,
		Query: relatedSearchV2Query{},
		Body:  requestBody{Kind: bodyJSON, Type: relatedRequest{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.Suggestions{}},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, valuePastBoundRefusal, excludeIssueInvalidRefusal},
	}
})
