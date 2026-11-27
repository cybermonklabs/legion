package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/coverage",
	Auth:        authAny,
	Description: "What each requirement of the issue's spec lacks: {issue, spec_artifact_id, has_plan, requirements, gaps}. A requirement is a `requirement` block of the spec; requirements lists them in document order, each with its reference, its words, its block's node state, and how many `check` and `plan_task` blocks of any document of the issue or of any descendant issue, at any status, name it in serves. A check or a plan task at node state proposed is a question still open, one at node state overtaken serves nothing, and neither is counted. A requirement's text is its words with every link and dispatch:// reference dropped; a caller that sent Dispatch-Trees: 1 is also given text_tree, the requirement's whole content as the tree a reader is shown, links and references kept. gaps is one line per gap - `<block id> has no check`, and `<block id> has no plan task` only once the issue has a plan document - or the single line `the spec marks no requirements` for a spec that marks none. A requirement at node state proposed is a question still open and raises neither gap. Nothing is ever refused for a gap. This is not GET /api/v1/search/coverage, which reports how much of what meaning search ranks carries a vector.",
	Handler:     (*server).getIssueCoverage,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: model.IssueCoverage{}},
})
