package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAFailedEventHeadReadNamesNoPerson: the event head GET /auth/whoami reads for a person
// (personEventHead) names no person in its error when what that person sees cannot be read,
// since the error reaches the 503 EVENT_HEAD_READ_FAILED log line. The test's own database loses
// its projects table, which the viewer's resolution reads and the log's head does not, so only
// the resolution fails.
//
// Control: the person back in the error's text; the test fails naming them.
func TestAFailedEventHeadReadNamesNoPerson(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	if _, err := database.Pool.Exec(t.Context(), `alter table projects rename to projects_gone`); err != nil {
		t.Fatalf("take the projects table away: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", seedAlice)
	_, err := served.personEventHead(request)
	if err == nil {
		t.Fatal("the event head of a person whose projects cannot be read answered no error")
	}
	if strings.Contains(err.Error(), seedAlice) || strings.Contains(err.Error(), strings.Split(seedAlice, "@")[0]) {
		t.Errorf("the event head's error = %q, which names the person", err)
	}
}
