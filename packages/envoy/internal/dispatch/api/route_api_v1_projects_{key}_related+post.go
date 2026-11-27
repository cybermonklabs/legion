package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/related",
	Auth:        authAny,
	Description: fmt.Sprintf("The GET form of this route with its fields in a JSON body {title, text, exclude_issue, exclude_issues, keywords_only}, exclude_issues a list of issue keys: the related-item check over a title and a text as long as an issue's spec (the body's 1 MiB bound; a title over %d characters is 400 CAP_EXCEEDED), writing nothing and waiting at most 5 seconds. The two forms do not answer alike. This one is the check an agent runs to ask a question, so it answers other_projects beside related: at most 2 answered question asks of other projects, each with its ask state, its answer and the project it stands in (owner.project), ranked by meaning alone and offered with no claim that they answer the question. The GET form answers none.", contracts.IssueTitleMax),
	Handler:     (*server).postRelated,
	// The POST form of the related-item check writes nothing. Its scope class is the project its
	// path names, which is the project it searches and the one a scoped credential may run it
	// over. Its answer also carries the answered questions of other projects (other_projects).
	// That leg reads no project's visibility: which private projects' questions it may show is a
	// read rule, and no write check of this table decides it.
	WriteClass: scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Suggestions{}, Request: relatedRequest{},
	},
})
