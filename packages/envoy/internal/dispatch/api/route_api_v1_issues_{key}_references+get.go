package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/references",
	Auth:        authAny,
	Description: "An issue's references, one hop in each direction: {links, linked_from, members, truncated}, under an ETag (If-None-Match answers 304). The issue's own content is its documents and their blocks, its asks and their parts, and its comments and messages and their blocks; a link is a mention, or a typed pointer while GET /api/v1/references lists it. links is every node the issue's own content links to, and linked_from every node that links to the issue or to its own content, one row for each node, a block or a part counted as its document or item; the issue and its own content are in neither, nor is a node that only a structural edge joins to the issue, such as its parent or a child. Each row is {node, name, kinds, via}: the node as GET /api/v1/references names it, the words shown for it, the distinct kinds of its links in order, and the lowest of the issue's nodes at the near end; each list is in the order of the node's kind, then its id, and is [] when it has no row. A caller that sent Dispatch-Trees: 1 is also given name_tree on each row: its name as the tree a reader is shown. members is the documents of links, each {artifact, depth, via} with the document's versions and its row's via, and truncated says whether the read was cut: depth is always 1 and truncated always false, since the read reads one hop, and both stay for the clients that read them. ?versions=latest leaves each member's artifact.versions holding only its latest version, under its own ETag; absent, empty or all is the full answer; another value, or versions given more than once, is 400 INVALID_QUERY. Every statement of the read shares one 2 s budget, and a read that reaches it reads nothing and answers 503 REFERENCES_TIMEOUT, to be asked again later.",
	Handler:     (*server).getIssueReferences,
	Read:        readEntry{Writes: readWritesNothing},
	// An issue's references read answers issueReferencesRead, whose rows carry their names'
	// trees, so the row names that read for the reason the architecture row above gives.
	Shape: routeShape{
		Status: http.StatusOK, Response: issueReferencesRead{}, Params: issueReferencesQuery{},
		// An If-None-Match that matches the answer's ETag is answered 304 and no body.
		Also: []routeAnswer{{Status: http.StatusNotModified}},
	},
})
