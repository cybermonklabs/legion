package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/open",
	Auth:        authAny,
	Description: "Open asks on open issues and project documents, with counts, by priority and then oldest first: ?author_session= for one session's own, ?project= for every open ask on a project's issues and documents, or neither for every project; at most one of the two (both is 400 AUTHOR_SESSION_OR_PROJECT_CONFLICT, and a blank one is 400 INVALID_QUERY naming it). ?waiting_on= narrows to the asks that wait on one person - a login of a person who has signed in, compared with the waiting_on_person each row names: the person the ask was handed to, else its issue's assignee, else its project's default assignee; a blank value, a repeated key or a login nobody has signed in as is 400 INVALID_QUERY. ?since= (RFC3339) sets opened_since to whether an ask of the scope, in any state, opened at or after it. No row is an ask the caller may not see. Each row is the open-ask row an ask card draws - the ask, its issue (with that issue's own assignee) or project document, the issue's priority, its newest reply, the caller's own snooze and the thread: replies, rewordings, followers, claimant, route and route_holder - with ref (the ask's page), age_seconds, owner (its issue or document again) and human_replied beside it; {session_id, as_of, opened_since, count, waiting_on_human, waiting_on_agent, asks}.",
	Handler:     (*server).listOpenAsks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: openAsksResponse{}, Params: openAsksQuery{}},
})
