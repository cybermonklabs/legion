package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/decisions",
	Auth:        authAny,
	Description: "The issue's decision records: {document, open_questions, decisions}. document is the dispatch:// address of its decisions.md, null (with no decisions) when it has none. open_questions are the issue's open asks whose block is in its spec, oldest first. Each decision is one `##` section of decisions.md in document order - a `###` section under `## Replaced` for a replaced one - with its heading block and markdown, the ask ids its blocks mention (kind person when it has any, agent otherwise), whether a paragraph of it starts `Unsure:`, the version its text last changed in, the Keep a person recorded, and one entry per link from its blocks to a spec block, each stale when the decision changed after it first linked that block and the block has not changed since.",
	Handler:     (*server).getIssueDecisions,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: issueDecisions{}},
})
