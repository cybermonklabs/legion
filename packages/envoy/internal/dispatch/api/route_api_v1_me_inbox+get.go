package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/inbox",
	Auth:        authAny,
	Description: "Everything tied to the caller's own person, in one statement: each node that holds a current relation to their person node (or to a session the server proved theirs) at either end, once, with every relation kind that ties it to them - {kind, direction (inbound, a tuple into the person; outbound, one from them), at} - and its moment, newest first. The kinds are the store's, so a kind a later write appends is an entry the day it is appended; the identity tuple gives none, and a node the caller may not see is in no entry and no count. ?kind= (a comma list of relation kinds, repeatable) narrows to the entries of those kinds and widens nothing. ?state= is the page's own narrowing - unread, inbox or dismissed - read from the lists the Inbox reads (GET /api/v1/me/replies' states, and under inbox GET /api/v1/inbox's open asks waiting on the caller); with none the entries are the walk of the tuples. Where an entry's node is a row the page draws (under no state, the inbox state's), it carries exactly one of ask (the Inbox's ask row with its thread), thread (a Reply row) or agent (an Agent row, with the conversation it draws). A narrowing never leaves in the answer an entry it cannot judge: ?project= narrows every entry by its node's own project, and a node the store holds no project of is then in none; ?issue= or ?artifact= (not both) keeps the rows the page draws of that owner and drops every other entry, an Agent row among them, since no attribute every node carries says which issue or document it belongs to. An absent limit answers every entry, but under state=dismissed one page of 200; a named one is 1-200 from ?offset=, and ?cursor= (the next_cursor of the page before it) resumes after the last entry it answered. Under state=dismissed that page is read to offset + limit + 1, which must fit in a 64-bit integer, so an offset above 9223372036854775806 less the limit is 400 INVALID_QUERY. Any other query key, an identity selector (login, person, assignee, session) among them, is 400 INVALID_QUERY naming it. The read is at the event horizon notifications read at. {person, as_of (the database time of the statement), horizon, total (every entry, taken after the visibility predicate, and under state=dismissed the page the read answers rather than the whole pile, which nothing counts), limit (the page's size: 0 when none was named, but 200 under state=dismissed), offset, next_cursor (while entries follow the page), entries}.",
	Handler:     (*server).listMeInbox,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, WriteNote: "Writes nothing itself. The read is at the event horizon notifications read at, so it waits on the horizon (sinceHorizon), as GET /api/v1/references with ?since= and the event log do, and the horizon's observation, which may run on this request's goroutine, writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader alike, not this request's."},
	Shape: routeShape{
		Status: http.StatusOK, Response: meInboxResponse{}, Params: meInboxQuery{}, RefusesUndeclared: true,
	},
})
