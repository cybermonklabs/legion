package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/collaborators", Auth: authAny, Versions: onlyV2,
		Description: "Everyone who worked an issue, running or not, one row each: a person (kind user, login) is the " +
			"assignee or anyone who claimed it, posted a comment, message, ask or document version on it or its " +
			"documents, or answered one of its asks, newest first; an agent session (kind session, session_id, title?) " +
			"is such an event's actor that is a session (the Legion daemon's own writer left out), or a session the " +
			"listener's Legion agent registry records for the issue, which adds legion {role, role_token, started_at, " +
			"replaced_by, replaced_at}. The people come first, then the sessions. Each row carries reasons (assignee, " +
			"claimed, posted, answered) and last_at; no running state. limit 1 to 200 (default 50) and offset; {items, " +
			"total, limit, offset, issue_key, legion_registry}, legion_registry unavailable when the listener did not " +
			"answer, the rest still answered. 404 ISSUE_NOT_FOUND.",
		Handler:  s.listIssueCollaboratorsV2,
		Resource: "collaborators", Kind: kindCollection,
		Query:    issueCollaboratorsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: issueCollaboratorsV2{}},
		Refusals: listRefusals(issueMissingRefusal),
	}
})
