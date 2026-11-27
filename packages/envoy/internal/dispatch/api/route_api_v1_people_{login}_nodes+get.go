package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/people/{login}/nodes",
	Auth:        authAny,
	Description: "One person's page: what they wrote and what reached them, newest first, read from the node and tuple store. Each row carries the node's kind and id, the relations that put it there (authored_by, delivered_to, or both), the first line of its text, the dashboard path it opens at, and the node's state and visibility (author only on its author's own page, since a node reserved for its author is on no other). A comment its author edited carries edited_at, and one its author deleted keeps its row with deleted_at and deleted_by and an empty title. It takes the login and a page - limit (1-200, default 50) and offset - and no kind, filter or edge to walk. {person, rows, total, limit, offset}. Needs the person scope, so the deployment's shared token is 403 OAUTH_SCOPE_REQUIRED. A login the store holds no person for is 404 NOT_FOUND, the answer an address nobody holds gets, and so is a person none of whose nodes the reader may see, with the same body, so the answer never says whether that person exists or holds a hidden node. The reader's own login is the one exception, answered 200 with the rows they may see, none where they may see none.",
	Handler:     (*server).listPersonNodes,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: personNodes{}, Params: myAnswersQuery{}},
})
