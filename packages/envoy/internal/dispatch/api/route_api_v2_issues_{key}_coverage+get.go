package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/coverage", Auth: authAny, Versions: onlyV2,
		Description: "What each requirement of the issue's spec lacks: {issue, spec_artifact_id, has_plan, " +
			"requirements, gaps}. A requirement is a `requirement` block of the spec; requirements lists them in document " +
			"order, each with its reference, its words - a link's text, never its target - and how many `check` and " +
			"`plan_task` blocks of any document of the issue or of any descendant issue, at any status, name it in " +
			"serves. gaps is one line per gap - `<block id> has no check`, and `<block id> has no plan task` only once " +
			"the issue has a plan document - or the single line `the spec marks no requirements` for a spec that marks " +
			"none. Nothing is ever refused for a gap. 404 ISSUE_NOT_FOUND.",
		Handler:  s.getIssueCoverageV2,
		Resource: "coverage", Kind: kindReport,
		Shape:    routeShape{Status: http.StatusOK, Response: model.IssueCoverage{}},
		Refusals: documentRefusals(undeclaredKeyRefusal, issueMissingRefusal),
	}
})
