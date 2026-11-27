package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/references", Auth: authAny, Versions: onlyV2,
		Description: "An issue's references, one hop in each direction, one list at a time. The issue's own content " +
			"is its documents and their blocks, its asks and their parts, and its comments and messages and their " +
			"blocks; a link is a mention, or a typed pointer while GET /api/v2/references lists it. relation names " +
			"the list: links, every node the issue's own content links to; linked_from, every node that links to the " +
			"issue or to its own content; or members (the default), the rows of links whose node is a document, the " +
			"documents /api/v1 answers as members. Each list has one row for each node, a block or a part counted as " +
			"its document or item; the issue and its own content are in no list, nor is a node that only a " +
			"structural edge joins to the issue, such as its parent or a child. Each row is {node, name, kinds, via, " +
			"artifact}: the node as GET /api/v2/references names it, the words shown for it, the distinct kinds of " +
			"its links in order, the lowest of the issue's nodes at the near end, and on a row of members or a row " +
			"of links whose node is a document, the document's row (its own fields, latest_version, " +
			"latest_version_at, latest_version_size and version_count, and no approval; its versions are GET " +
			"/api/v2/artifacts/{id}/versions), null on every other row. A caller that sent Dispatch-Trees: 1 is also " +
			"given name_tree on each row: its name as the tree a reader is shown. Each list is in the order of the node's " +
			"kind, then its id. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}, total the " +
			"rows of the list. The answer carries an ETag over the page it answers, its relation and its rows " +
			"whole; If-None-Match answers 304. Every statement of the read shares one 2 s budget, and a read that " +
			"reaches it reads nothing and answers 503 REFERENCES_TIMEOUT, to be asked again later. 404 " +
			"ISSUE_NOT_FOUND.",
		Handler:  s.listIssueReferencesV2,
		Resource: "references", Kind: kindCollection,
		Query: issueReferencesV2Query{},
		Shape: routeShape{
			Status: http.StatusOK, Response: page[referenceRowV2]{},
			Also: []routeAnswer{{Status: http.StatusNotModified}},
		},
		Refusals: listRefusals(issueMissingRefusal, apicode.Refusal{Condition: apicode.ReferencesBudgetSpent, Code: apicode.ReferencesTimeout}),
	}
})
