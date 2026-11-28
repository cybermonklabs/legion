package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/nodes", Auth: authAny, Versions: onlyV2,
		Description: "The nodes of one person, newest first, read from the node and tuple store across every project: " +
			"what they wrote and what reached a session of theirs. person=<login> names the person and is required " +
			"(400 INVALID_QUERY without it). Each row carries the node's kind and id, the relations that put it on " +
			"the list (authored_by, delivered_to, or both), the first line of its text, when it was written and the " +
			"dashboard path it opens at. A comment its author edited carries edited_at, and one its author deleted " +
			"keeps its row with deleted_at and deleted_by and an empty title. project=<key>, repeatable, keeps the " +
			"rows of each project it names; none " +
			"keeps every project's (400 INVALID_PROJECT for a value that is no project key). {items, total, limit, " +
			"offset}, limit 1 to 200 (default 50). Needs the person scope. A person none of whose nodes the reader " +
			"may see, a login nobody holds among them, lists no row, so the answer never says whether that person " +
			"exists; the reader's own login lists the rows they may see. GET /api/v1/people/{login}/nodes answers " +
			"the same rows under rows, and a 404 where this lists none for another login.",
		Handler:  s.listPersonNodesV2,
		Resource: "nodes", Kind: kindCollection, CrossProject: true,
		Query:       personNodesV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[personNode]{}},
		OAuthScopes: routeScopeSet{model.ScopePerson},
		Refusals:    listRefusals(queryProjectMalformedRefusal),
	}
})
