package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/landing",
	Auth:        authAny,
	Description: "One page of the dashboard read cold, in one request in place of ten to twenty-two: {viewer, shell, page}. viewer is what GET /auth/whoami answers (event_head, kind, login), with that route's own refusals, 503 EVENT_HEAD_UNAVAILABLE while no event id is proven final and 503 EVENT_HEAD_READ_FAILED when the head could not be read, since the page resumes its event feed from that id. shell is what every page reads whatever its route: the inbox, the caller's pinned issues, the projects, the caller's agent state, the unread-reply count, the pending credential requests (null without a secrets broker), the two priority fault counts (what GET /api/v1/priorities?summary=true answers) and the agent list. page carries what the page ?page= names reads before it draws: issue (with ?key=), board (?project=), inbox, or document (?project= and ?slug=); any other value, and a page without the parameter it needs, is 400 LANDING_INPUT naming what it takes. Every part but the viewer is null when it could not be read or does not exist - an issue key that is no issue, a slug that is no document, a listener that did not answer - and the client then reads that part from the route it stands in for.",
	Handler:     (*server).getLanding,
	// The landing carries one person's inbox, their `me` state and their credential requests,
	// each read from a route that needs this scope (GET /api/v1/inbox, GET /api/v1/me/state,
	// GET /api/v1/credential-requests), so the landing needs it too: without it a project-scoped
	// agent token would be served one person's inbox.
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: "It answers in one read what the reads it stands for answer (landing.go names each part's route), through those reads' own loaders, and writes only what they write. Its document parts read the document service as GET /api/v1/artifacts/{id}/text and /blocks do: " + docsCompactionNote},
	Shape:       routeShape{Status: http.StatusOK, Response: landingResponse{}, Params: landingQuery{}},
})
