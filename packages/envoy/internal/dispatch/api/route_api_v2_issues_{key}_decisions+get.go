package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/decisions", Auth: authAny, Versions: onlyV2,
		Description: "The issue's decisions: each `##` section of its decisions.md in document order - a `###` " +
			"section under `## Replaced` for a replaced one - with its heading block, ref and markdown, its text_sha256, " +
			"the ask ids its blocks mention (kind person when it has any, agent otherwise), whether a paragraph of it " +
			"starts `Unsure:`, the version its text last changed in, the keep a person recorded, whether it waits for " +
			"review, and one entry per link from its blocks to a spec block, each stale when the decision changed after " +
			"it first linked that block and the block has not changed since. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset, document}, document the dispatch:// address of its decisions.md, null with no " +
			"decisions where it has none. include=open_questions adds open_questions beside them, its open asks whose " +
			"block is in its spec, oldest first, each as GET /api/v2/asks answers it. 404 ISSUE_NOT_FOUND.",
		Handler:  s.listIssueDecisionsV2,
		Resource: "decisions", Kind: kindCollection, Key: "{block_id}",
		Query:    issueDecisionsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: issueDecisionsV2{}},
		Refusals: documentRefusals(listRefusals(issueMissingRefusal)...),
	}
})
