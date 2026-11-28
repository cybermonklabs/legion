package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/references",
	Auth:        authAny,
	Description: "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind= (an issue's wait on an issue is answered as needs, which ?kind=needs and ?kind=blocked_by both read; an issue's child_of, duplicate_of and filed_from are kinds of their own, a filed_from's node the item's canonical reference), ?since=<events.id> (mentions only, read up to the event log's safe horizon: every mention committed before the read, unless an open transaction holds the horizon back past 1 s, when the read answers the mentions at or below it and a later read the rest). A caller with no cursor starts from since=0, which lists every mention committed before the read that carries a source_seq, and after that reads again from the highest source_seq an answer showed it. A mention written with no event (a rebuild, a backfill, an older build) carries no source_seq until a later write of its source stamps it with that write's event, and until then only a read without since lists it. A signed-in page may start from GET /auth/whoami's event_head instead, which takes a cookie or a trusted header and not a bearer, so a bearer caller starts from 0. Never take a since from max(events.id) read straight from the database: event ids are not commit order, and an id taken that way can stand above an edge that commits later. An empty answer is not proof that nothing is new, so the caller reads again with the id it holds. An unpinned reference to a block of a document names the block node its address resolves to - its moved_to tuples first, then the same block id in the issue room its document is a fragment of, then the address itself - and lists the edges at the address and those at that node, newest first: a block of a document converted into its issue's room, written at the document's own address, is the room's node, which holds the links its pointers write. A ?to= reference whose item id or block id is written short (8 characters or more, never a whole uuid) names the one item of the owner the reference names that carries it: the issue of dispatch://<KEY>/ask/<prefix>, the document of a #b-<prefix>; the node answered carries the whole id, a prefix two of them carry is 409 ID_AMBIGUOUS listing both, and one nothing carries is the 404 the reference as written always gave. A whole id is read whatever owner the reference names, as it always was.",
	Handler:     (*server).getReferences,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: "Writes nothing itself. With ?since=, the read waits on the event horizon, as the event log does, and the horizon's observation, which may run on this request's goroutine, writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader alike. Without ?since= it does not read the horizon."},
	// The references read answers graphReferencesRead, whose node carries its text's tree and
	// whose pin carries the pinned text's, so the route names that read for the reason
	// GET /api/v1/projects/{key}/architecture names its own.
	Shape: routeShape{Status: http.StatusOK, Response: graphReferencesRead{}, Params: referencesQuery{}},
})
