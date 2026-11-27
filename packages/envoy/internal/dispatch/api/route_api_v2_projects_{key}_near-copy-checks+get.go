package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/near-copy-checks", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("List a project's near-copy gate answers, one row for each create the gate "+
			"answered (an external create runs no gate and has none), recorded when the create was refused or once it "+
			"was written, oldest first: those within from and to on created_at, RFC3339, from inclusive and to "+
			"exclusive (400 INVALID_QUERY when from is not before to). A row written in its create's transaction holds "+
			"that transaction's start as created_at and is seen only once it commits, so a window should end some time "+
			"before now. A row is {id, created_at, project, actor_kind, actor_id, outcome, reason, force, title, "+
			"spec_sha256, parent, issue, candidates, refuse_score, nudge_score, build_commit, read_ms, read_stage}, with "+
			"no spec: GET /api/v2/near-copy-checks/{id} reads one row with it. outcome is refused (nothing was written; "+
			"every issue create the gate refused has this row, written before the answer), forced, nudged, passed or "+
			"not_made, and "+
			"reason bound, failed or unrecorded for not_made and null otherwise; candidates are the issues the gate read "+
			"at or above nudge_score, at most %d, best first; read_ms is how long the create waited on the gate's read, "+
			"in milliseconds, and read_stage the step that read ended in (connect, terms, keys or score), both null on a "+
			"row written before they were recorded. limit 1 to %d (default %d) and offset; {items, total, "+
			"limit, offset}. 404 PROJECT_NOT_FOUND for a project that does not exist.",
			duplicateCandidateLimit, nearCopyChecksPageMax, nearCopyChecksPageMax),
		Handler:  s.listNearCopyChecksV2,
		Resource: "near-copy-checks", Kind: kindCollection, Key: "{id}",
		TimeFields: []string{"created_at"},
		Query:      nearCopyChecksV2Query{},
		Shape:      routeShape{Status: http.StatusOK, Response: page[model.NearCopyCheck]{}},
		List:       listBounds{Default: nearCopyChecksPageMax, Max: nearCopyChecksPageMax},
		Refusals:   listRefusals(projectMissingRefusal),
	}
})
