package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An operator reading plain `legion state` is told which roles have not been given the work their
// phase entry names, each by issue and role with the two numbers (api.Issue.PhaseEntry and
// GivenEntry), on one line however many there are; with none the line says so. The daemon sends
// such a role that entry's work on every admission pass and on a sweep of its own each minute, so
// the line is what an operator reads to see a difference the passes are not closing.
//
// The role is the one the issue's current phase names (workflow.RoleFor), which is the role the
// two numbers are of: an issue whose phase names no phase worker role carries neither number, so
// the line never names one.
func TestPlainStateNamesTheRolesNotGivenTheirPhaseEntry(t *testing.T) {
	daemon := `"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"pendingStatusWrites":[],"strandedClaims":[]`
	// LEGION-208's implementer and LEGION-210's reviewer are behind the entry their rows name,
	// LEGION-209's tester holds the work its row names, and LEGION-211 is held, a phase that
	// names no phase worker role and carries neither number.
	behind := `{"issues":{` +
		`"LEGION-208":{"key":"LEGION-208","generation":1,"phase":"implementing","status":"in_progress","workers":{},"phaseEntry":3,"givenEntry":2},` +
		`"LEGION-209":{"key":"LEGION-209","generation":1,"phase":"testing","status":"testing","workers":{},"phaseEntry":2,"givenEntry":2},` +
		`"LEGION-210":{"key":"LEGION-210","generation":2,"phase":"reviewing","status":"needs_review","workers":{},"phaseEntry":5,"givenEntry":4},` +
		`"LEGION-211":{"key":"LEGION-211","generation":1,"phase":"held","status":"in_progress","workers":{}}}}`
	level := `{"issues":{` +
		`"LEGION-209":{"key":"LEGION-209","generation":1,"phase":"testing","status":"testing","workers":{},"phaseEntry":2,"givenEntry":2},` +
		`"LEGION-211":{"key":"LEGION-211","generation":1,"phase":"held","status":"in_progress","workers":{}}}}`
	for _, tc := range []struct {
		name, issues, want string
	}{
		{
			name:   "two roles behind their entry, by issue key",
			issues: behind,
			want: "roles not given their phase entry: 2 — LEGION-208 implementer: entry 3, given 2; " +
				"LEGION-210 reviewer: entry 5, given 4\n",
		},
		{name: "none", issues: level, want: "roles not given their phase entry: none\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := "{" + daemon + "," + strings.TrimSuffix(strings.TrimPrefix(tc.issues, "{"), "}") + "}"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(served))
			}))
			defer server.Close()
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_ISSUE", "")
			t.Setenv("LEGION_DAEMON_URL", server.URL)

			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
				t.Fatalf("legion state = %d, stderr %q", code, errb.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("legion state printed %q, want the line %q", out.String(), tc.want)
			}
		})
	}
}
