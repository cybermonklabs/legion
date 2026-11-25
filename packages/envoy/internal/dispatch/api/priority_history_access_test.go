package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestAPrioritysHistoryNamesNoIssueOfAHiddenProject (c-private-unseen, R2-R4): a priority belongs to
// no project, but its history names, on each advances act, the issue at the other end, and its
// listing counts each project's work under it. An open viewer who cannot see PRIV reads neither
// PRIV's key in the history nor PRIV's work in the counts: both answer as they do when PRIV's
// issue never advanced the priority. carol, who sees PRIV, reads the act, so the history the test
// reads is one that holds it.
func TestAPrioritysHistoryNamesNoIssueOfAHiddenProject(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	hidden := issueIn(t, handler, accessCarol, privateProject)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]string{"id": "probe", "title": "Probe"}, accessCarol); response.Code != http.StatusCreated {
		t.Fatalf("carol makes the priority probe: %d %s", response.Code, response.Body.String())
	}
	read := func(login, path string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, login)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s as %s = %d %s", path, login, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	listingBefore := read("alice", "/api/v1/priorities")
	historyBefore := read("alice", "/api/v1/priorities/probe/history")

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+hidden, map[string]any{"advances": []string{"probe"}}, accessCarol); response.Code != http.StatusOK {
		t.Fatalf("carol makes %s advance probe: %d %s", hidden, response.Code, response.Body.String())
	}
	if history := read(accessCarol, "/api/v1/priorities/probe/history"); !strings.Contains(history, hidden) {
		t.Fatalf("carol, who sees PRIV, reads no advances act of %s in probe's history, so the test reads nothing: %s", hidden, history)
	}

	if history := read("alice", "/api/v1/priorities/probe/history"); history != historyBefore {
		t.Errorf("alice, who does not see PRIV, reads probe's history as %s; before %s advanced it, %s", history, hidden, historyBefore)
	}
	if listing := read("alice", "/api/v1/priorities"); listing != listingBefore {
		t.Errorf("alice, who does not see PRIV, reads the priorities as %s; before %s advanced probe, %s", listing, hidden, listingBefore)
	}
}
