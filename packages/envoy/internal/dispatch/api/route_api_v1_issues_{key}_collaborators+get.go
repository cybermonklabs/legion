package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/collaborators",
	Auth:        authAny,
	Description: "Everyone who worked an issue, running or not, as {issue_key, people, agents, legion_registry}. people (kind user) is one row per canonical login: the assignee, everyone who claimed it, posted a comment, message, ask or document version on it or its documents, or answered one of its asks. agents (kind session) is one row per session id: the same event actors that are sessions (the Legion daemon's legion-daemon:<PROJECT> writer left out), merged with every session the listener's Legion agent registry records for the issue, which adds legion {role, role_token, started_at, replaced_by, replaced_at} and its title. Each row carries reasons (assignee, claimed, posted, answered) and last_at; no running state. legion_registry is unavailable when the listener did not answer, and the rest still answers.",
	Handler:     (*server).getIssueCollaborators,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueCollaborators{}},
})
