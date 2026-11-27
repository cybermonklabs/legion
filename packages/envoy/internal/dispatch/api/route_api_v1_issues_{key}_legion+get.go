package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/legion",
	Auth:        authAny,
	Description: "One issue's Legion state, from what the Legion intake recorded of the daemon of the issue's project: {issue, project, configured, state, read, runs_revision}. project is the project of the issue. configured is true when that project names a Legion daemon. state is null when the daemon does not record the issue. Otherwise state is {phase, phase_since, phase_start_seen, roles, slot, waiting_line, blocked_by, waiting_on}. phase_since is when Dispatch stored the phase. phase_start_seen is false when that phase is the first value that Dispatch stored for the issue. roles is [{role, state}], one entry for each role with a claim, or [] when no role has one. slot is the index of the slot that the issue holds, or null. waiting_line is {place, length}, or null when the daemon's waiting line does not name the issue. place counts from 1, and length is the number of issues in that line. blocked_by lists the issues that block the issue, in the order the daemon stored them, or [] when no issue blocks it. waiting_on is the daemon's object exactly as stored, or null. read is the newest read of the daemon, {at, ok, error}, or null when no read was recorded. runs_revision counts the tuples of the issue's runs. It is 0 for an issue with no run. Each write of a run that stores something new makes it larger, and a write sent again leaves it. A project that names no daemon is not refused. Its issue answers configured false, state null and read null, whatever rows an earlier daemon left, and its runs_revision. The one refusal is 404 NOT_FOUND, for a key that names no issue.",
	Handler:     (*server).getLegionIssue,
	Successor:   deletedFor(http.MethodGet, "/issues/{key}?include=legion"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: legionIssueResponse{}},
})
