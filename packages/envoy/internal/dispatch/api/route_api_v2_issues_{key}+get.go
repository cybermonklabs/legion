package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}", Auth: authAny, Versions: onlyV2,
		Description: "Read one issue: its own fields, and artifacts, each of its documents, files and images in " +
			"creation order as GET /api/v2/issues/{key}/artifacts lists them. include= adds, each as a member of its " +
			"name: blocks, the keys of the issues whose blocked_by names it, sorted, [] when none, which no row of GET " +
			"/api/v2/issues carries; comment_count, how many comments GET /api/v2/issues/{key}/comments lists with no " +
			"artifact=, 0 when it holds none; progress {tasks, children}, its spec's task-list items and its direct " +
			"children as {done, total}, each null when it has none; open_asks, its open asks, oldest first, each as GET " +
			"/api/v2/asks answers it; children, its direct children, each with a rollup of its subtree; design_gates, the " +
			"document each design-gate step records and whether the deployment's gate holds the issue's tree on it, null " +
			"where the daemon said " +
			"nothing, [{step, artifact_id, holds}] in step order; reach {route_status, route_holder, route_status_reason, " +
			"holder_state, last_seen, takeable_at}, where its route reaches now and what state the session holding its " +
			"claim is in, read from the Envoy listener, each null where there is none or the listener did not answer; " +
			"referenced_by_count, how many references to it stand; and legion {issue, project, configured, state, read, " +
			"runs_revision}, its Legion state from what the Legion intake recorded of the daemon of its project, the body a " +
			"/ws/live legion_issue view of it carries. In legion, configured is whether that project names a Legion " +
			"daemon, state is null where the daemon does not record the issue and otherwise {phase, phase_since, " +
			"phase_start_seen, roles, slot, waiting_line, blocked_by, waiting_on}, waiting_on in the shape GET " +
			"/api/v1/issues/{key}/legion answers it, read is the newest read of the daemon " +
			"{at, ok, error} or null where none was recorded, and runs_revision counts the tuples of its runs, 0 with no " +
			"run, and grows with each write of a run that stores something new. A project that names no daemon is not " +
			"refused: its issue answers configured false, state null and read null. 404 ISSUE_NOT_FOUND for an issue " +
			"that does not exist.",
		Handler:  s.getIssueV2,
		Resource: "issues", Kind: kindItem, Key: "{key}",
		Query:    issueReadV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: issueReadV2{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
	}
})
