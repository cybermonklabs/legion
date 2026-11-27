package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/handoffs", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("List an issue's handoffs, each {id, issue_key, phase, version, author, created_at}, a "+
			"closed issue's too. phase=<phase> keeps one phase's (a lowercase slug, %s, 400 HANDOFF_INPUT otherwise). "+
			"include=body adds each handoff's body, the exact text written, and for a caller that sent Dispatch-Trees: "+
			"1 its field_trees, as GET /api/v2/handoffs/{id} gives them. sort=-created_at (the default: newest first, "+
			"then by phase, then the newest version), created_at, version or -version (a phase's newest version first: "+
			"phase=<phase>&sort=-version&limit=1&include=body reads the phase's latest handoff, as GET "+
			"/api/v1/issues/{key}/handoffs/{phase}/latest does). %s {items, total, limit, offset}. 404 "+
			"ISSUE_NOT_FOUND for an issue that does not exist.",
			contracts.HandoffPhasePattern, includedPageLimitText(v2HandoffList, issueHandoffsV2Query{})),
		Handler:  s.listIssueHandoffsV2,
		Resource: "handoffs", Kind: kindCollection, Key: "{id}",
		Query:    issueHandoffsV2Query{},
		List:     v2HandoffList,
		Shape:    routeShape{Status: http.StatusOK, Response: page[handoffV2]{}},
		Refusals: listRefusals(issueMissingRefusal, handoffInputRefusal),
	}
})
