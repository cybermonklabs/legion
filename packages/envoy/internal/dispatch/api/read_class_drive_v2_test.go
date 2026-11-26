package api

import (
	"net/http"
	"strings"
)

// Each v2 read has a driven request, even where the main-side v1 drive's SQL assertion
// applies unchanged. New paths and query keys are written explicitly where they differ.
func init() {
	copyRead := func(v2, v1, target string) {
		drive := readRowDrives["GET /api/v1"+v1]
		if target == "" {
			drive.target = strings.Replace(drive.target, "/api/v1", "/api/v2", 1)
		} else {
			drive.target = "/api/v2" + target
		}
		readRowDrives["GET /api/v2"+v2] = drive
	}
	for _, path := range []string{
		"", "/agents", "/agents/{session_id}/messages", "/artifacts/{id}/blocks/{block_id}",
		"/artifacts/{id}/events", "/artifacts/{id}/reviews", "/artifacts/{id}/subscribers", "/artifacts/{id}/versions/{number}",
		"/block-links", "/broadcasts", "/broadcasts/{id}", "/credential-requests", "/credential-grants", "/deploys", "/machine-logins",
		"/handoffs/{id}", "/issues", "/issues/{key}/collaborators", "/issues/{key}/coverage",
		"/issues/{key}/decisions", "/issues/{key}/references", "/issues/{key}/steps",
		"/me/agent-tokens", "/me/answers", "/near-copy-checks/{id}", "/priorities", "/projects/{key}/architecture",
		"/projects/{key}/architecture-sources", "/projects/{key}/legion/board", "/projects/{key}/legion/daemon",
		"/projects/{key}/plans", "/reference-targets", "/settings/delivery", "/settings/new-issue", "/users",
	} {
		copyRead(path, path, "")
	}
	copyRead("/agents/{session_id}/answers", "/agents/{session_id}/answers", "/agents/$session/answers?from=$answersSince")
	copyRead("/artifacts/{id}", "/artifacts/{id}/text", "/artifacts/$notes?include=text,blocks")
	copyRead("/artifacts/{id}/asks", "/artifacts/{id}/asks", "/artifacts/$guide/asks?include=thread&limit=10")
	copyRead("/artifacts/{id}/changes", "/artifacts/{id}/changes", "/artifacts/$notes/changes?version_from=1&version_to=2")
	copyRead("/artifacts/{id}/comments", "/artifacts/{id}/comments", "")
	copyRead("/artifacts/{id}/references", "/artifacts/{id}/references", "/artifacts/$spec/references?direction=out")
	copyRead("/block-schema", "/schema/blocks", "/block-schema")
	copyRead("/ci-reconcile-passes", "/ci-reconcile/passes", "/ci-reconcile-passes")
	copyRead("/comments/{id}", "/comments/{id}", "/comments/$comment?include=blocks")
	copyRead("/comments/{id}/corrections", "/comments/{id}/history", "/comments/$correctedComment/corrections")
	copyRead("/dashboard-builds", "/dashboard/builds", "/dashboard-builds")
	copyRead("/deploy-runs/{id}", "/delivery/runs/{id}", "/deploy-runs/$pipelineRun")
	copyRead("/events", "/events/log", "/events?after=0")
	copyRead("/going-actions", strings.TrimPrefix(goingCataloguePath, "/api/v1"), "/going-actions")
	copyRead("/issues/{key}", "/issues/{key}", "/issues/$issue?include=design_gates,reach,legion")
	copyRead("/issues/{key}/asks", "/issues/{key}/asks", "/issues/$issue/asks?include=thread&limit=10")
	copyRead("/issues/{key}/events", "/issues/{key}/events", "")
	copyRead("/issues/{key}/checks", "/issues/{key}/results", "/issues/$issue/checks")
	checks := readRowDrives["GET /api/v2/issues/{key}/checks"]
	for index, member := range checks.volatile {
		if index == 0 {
			checks.volatile = append([]volatileMember(nil), checks.volatile...)
		}
		checks.volatile[index].path = strings.Replace(member.path, "checks.", "items.", 1)
	}
	readRowDrives["GET /api/v2/issues/{key}/checks"] = checks
	copyRead("/issues/{key}/role-holder-set", "/issues/{key}/role-holders", "/issues/$issue/role-holder-set")
	copyRead("/known-sessions", "/agents/known", "/known-sessions?id=$session")
	copyRead("/limit-set", "/limits", "/limit-set")
	copyRead("/me", "/whoami", "/me")
	copyRead("/me/signins", "/signins", "/me/signins")
	copyRead("/me/threads", "/me/replies", "/me/threads?state=inbox")
	copyRead("/messages/{id}", "/messages/{id}", "/messages/$message?include=blocks")
	copyRead("/messages/{id}/corrections", "/messages/{id}/history", "/messages/$correctedMessage/corrections")
	copyRead("/nodes", "/people/{login}/nodes", "/nodes?person=$person")
	copyRead("/priorities/{id}/acts", "/priorities/{id}/history", "/priorities/$priority/acts")
	copyRead("/search-coverage", "/search/coverage", "/search-coverage")
	copyRead("/projects/{key}/issues", "/issues", "/projects/$project/issues")
	copyRead("/projects/{key}/near-copy-checks", "/near-copy-checks", "/projects/$project/near-copy-checks")
	copyRead("/references", "/references", "/references?to=$issueRef&after=0")
	copyRead("/related-check-stats", "/related-checks", "/related-check-stats")
	copyRead("/search-results", "/search/results", "/search-results?q=drive")
	copyRead("/settings/credential-broker", "/credentials/capabilities", "/settings/credential-broker")
	copyRead("/settings/text-model", "/going/capability", "/settings/text-model")
	copyRead("/_test/artifacts/rooms", "/artifacts/_test/rooms", "/_test/artifacts/rooms")
	copyRead("/_test/artifacts/{id}/room-counters", "/artifacts/{id}/_test/room-counters", "/_test/artifacts/$spec/room-counters")
	copyRead("/_test/artifacts/{id}/live-write-pauses/{point}", "/artifacts/{id}/_test/live-write-pauses/{point}", "/_test/artifacts/$spec/live-write-pauses/after-commit?wait=100ms")
	copyRead("/_test/messages/queued", "/messages/_test/queued", "/_test/messages/queued")

	read := func(pattern, target, sql string) {
		readRowDrives["GET /api/v2"+pattern] = rowDrive{target: "/api/v2" + target, statement: statementOf(sql)}
	}
	read("/agents/{session_id}/artifacts", "/agents/$session/artifacts", `\bfrom\s+artifacts\s+where\b`)
	read("/artifacts/{id}/versions", "/artifacts/$notes/versions", `\bfrom\s+artifact_versions\s+v\s+where\b`)
	read("/asks", "/asks?project=$project&include=thread&limit=10", `\bfrom\s+asks\s+a\b`)
	asks := readRowDrives["GET /api/v2/asks"]
	asks.volatile = []volatileMember{{"as_of", readTime}}
	readRowDrives["GET /api/v2/asks"] = asks
	read("/asks/{id}", "/asks/$ask?include=thread", `\bfrom\s+ask_followers\s+where\s+ask_id\s*=\s*any\b`)
	read("/issues/{key}/artifacts", "/issues/$issue/artifacts?include=text,blocks&limit=10", `\bfrom\s+artifacts\s+where\b`)
	// Even when the first reader populated the response cache, the handler reads its watermark.
	read("/issues/{key}/comments", "/issues/$issue/comments", `\bfrom\s+comment_watermarks\b`)
	read("/issues/{key}/handoffs", "/issues/$issue/handoffs", `\bfrom\s+handoffs\s+where\s+issue_key\s*=\s*\$1\s+and\b`)
	read("/issues/{key}/messages", "/issues/$issue/messages", `\bfrom\s+messages\b`)
	read("/issues/{key}/subscribers", "/issues/$issue/subscribers", `\bfrom\s+issues\s+where\b`)
	read("/me/marks", "/me/marks?kind=issue", `\bfrom\s+user_issue_state\b`)
	read("/me/marks/{kind}/{id}", "/me/marks/document/$notes", `\bfrom\s+user_document_reads\b`)
	read("/projects", "/projects?include=default_assignee", `\bfrom\s+projects\b`)
	read("/projects/{key}/artifacts", "/projects/$project/artifacts?include=text,blocks&limit=10", `\bfrom\s+artifacts\s+where\b`)
	read("/repositories", "/repositories", `\bfrom\s+repo_projects\b`)
	copyRead("/repositories/{owner}/{repo}/app-installation", "/github/app-installation/{owner}/{repo}", "/repositories/acme/widgets/app-installation")

	for _, pattern := range []string{"/delivery/timeline", "/delivery/measures", "/delivery/pipeline"} {
		drive := getWriteDrives["GET /api/v1"+pattern]
		drive.target = strings.Replace(drive.target, "/api/v1", "/api/v2", 1)
		getWriteDrives["GET /api/v2"+pattern] = drive
	}
	going := getWriteDrives["GET /api/v1/going/requests/{id}/stream"]
	going.target = "/api/v2/going-requests/$going/stream"
	getWriteDrives["GET /api/v2/going-requests/{id}/stream"] = going
	for _, pattern := range []string{"/credential-grants", "/credential-requests"} {
		key := "GET /api/v2" + pattern
		drive := readRowDrives[key]
		drive.target = "/api/v2" + pattern // the one caller needs no approver=me selector
		if pattern == "/credential-requests" {
			drive.status = http.StatusNotFound
		}
		readRowDrives[key] = drive
	}
}
