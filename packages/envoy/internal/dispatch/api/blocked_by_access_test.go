package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestAVisibleIssueNamesNoHiddenIssueThatBlocksIt (c-private-unseen, R4): carol, who sees PUB and
// PRIV, marks PUB's issue blocked by PRIV's. A caller who does not see PRIV reads PUB's issue, the
// issue list, the issue's events and the event log, and none of them names PRIV's issue: each
// answers as it would were the blocker an issue nobody holds. A deployment may instead refuse the
// link itself; then nothing joins the two projects and the reads have nothing to hide.
func TestAVisibleIssueNamesNoHiddenIssueThatBlocksIt(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	hidden := issueIn(t, handler, accessCarol, privateProject)
	visible := issueIn(t, handler, "alice", publicProject)

	linked := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+visible, map[string]any{"blocked_by": []string{hidden}}, accessCarol)
	if linked.Code != http.StatusOK {
		t.Logf("carol's link of %s to %s is refused (%d %s), so no read can name it", visible, hidden, linked.Code, linked.Body.String())
		return
	}
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+visible, nil, accessCarol); !strings.Contains(read.Body.String(), hidden) {
		t.Fatalf("carol, who sees PRIV, reads no %s on %s, so the test reads nothing: %s", hidden, visible, read.Body.String())
	}
	// The event log answers only the events at or below the safe horizon, and a read held below
	// the link's event would pass over it unread (awaitHorizonRead says what holds it back).
	safeHorizon(t, served)

	shared := "agent-token"
	for _, read := range []struct{ who, path string }{
		{"alice", "/api/v1/issues/" + visible},
		{"alice", "/api/v1/issues?project=" + publicProject},
		{"alice", "/api/v1/issues/" + visible + "/events"},
		{"alice", "/api/v1/events/log"},
		{shared, "/api/v1/issues/" + visible},
		{shared, "/api/v1/events/log"},
	} {
		var body string
		if read.who == shared {
			body = agentRequest(t, handler, http.MethodGet, read.path, nil, shared).Body.String()
		} else {
			body = dispatchRequest(t, handler, http.MethodGet, read.path, nil, read.who).Body.String()
		}
		if at := strings.Index(body, hidden); at >= 0 {
			t.Errorf("%s, who does not see PRIV, reads %s naming %s: …%s…", read.who, read.path, hidden, body[max(0, at-160):min(len(body), at+60)])
		}
	}
}
