package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An operator reading plain `legion state` is told which roles have not been given the work their
// phase entry names, each by issue and role with the two numbers (api.Issue.PhaseEntry and
// GivenEntry), on one line however many there are; with none the line says so. The daemon sends
// such a role that entry's work on every admission pass and on a sweep of its own each minute, so
// the line is what an operator reads to see a difference the passes are not closing.
//
// A root is held to another rule, because the architect is the role that works `admitted` and
// every root reads as behind for the moments between a transition and its delivery: the line
// names a root only where the state names an arm of the daemon's architect-stalled reading for it
// (api.Issue.ArchitectStalls), and prints each arm's words (record.ArchitectStallArm.Words), the
// words the daemon's wake begins with. The daemon reads the arms by the predicates its own wake
// uses, so the line reads none of its own: a root whose fields would have read as an arm to a
// reader of the times, the claim and the gate is named by nothing while the state names no arm.
//
// The numbers are the architect's where the root stands in a phase its architect works, and past
// it they are that phase's worker's, so the architect of such a root is named with its arm and no
// numbers, and its worker by the numbers where they differ.
//
// The role is the one the issue's current phase names (phase.RoleFor): an issue whose phase
// names no role carries neither number, so the line never names one.
func TestStatePlainNamesTheRolesBehindTheirEntryAndTheRootsTheStateNamesAnArmFor(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	daemon := `"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"pendingStatusWrites":[],"strandedClaims":[]`
	// LEGION-208's implementer and LEGION-210's reviewer are behind the entry their rows name,
	// LEGION-209's tester holds the work its row names, and LEGION-211 is held, a phase that
	// names no role and carries neither number.
	workers := `"LEGION-208":{"key":"LEGION-208","generation":1,"phase":"implementing","status":"in_progress","workers":{},"phaseEntry":3,"givenEntry":2},` +
		`"LEGION-209":{"key":"LEGION-209","generation":1,"phase":"testing","status":"testing","workers":{},"phaseEntry":2,"givenEntry":2},` +
		`"LEGION-210":{"key":"LEGION-210","generation":2,"phase":"reviewing","status":"needs_review","workers":{},"phaseEntry":5,"givenEntry":4},` +
		`"LEGION-211":{"key":"LEGION-211","generation":1,"phase":"held","status":"in_progress","workers":{}}`
	// A root whose transition raised an entry the daemon has still to deliver, which reads as
	// behind and is no failure. Beside it three roots whose fields alone look like each arm — a
	// task queued twenty minutes ago that no turn confirmed, a failed architect claim, two hours at
	// admitted with no design gate registered — and for which the state names no arm: the daemon's
	// reading is what names a root, so the line names none of the four.
	unnamed := fmt.Sprintf(`"LEGION-300":{"key":"LEGION-300","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-300",`+
		`"workers":{},"architect":{"session":"ses_architect_300","state":"working","pluginContract":81},`+
		`"phaseEntry":2,"givenEntry":1,"slot":{"index":0,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-300","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-301":{"key":"LEGION-301","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-301",`+
		`"workers":{},"architect":{"session":"ses_architect_301","state":"ready","pluginContract":81},`+
		`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"slot":{"index":1,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-301","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-302":{"key":"LEGION-302","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-302",`+
		`"workers":{},"architect":{"session":"ses_architect_302","state":"failed","pluginContract":81},`+
		`"phaseEntry":2,"givenEntry":1,"slot":{"index":2,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-302","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-303":{"key":"LEGION-303","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-303",`+
		`"workers":{},"architect":{"session":"ses_architect_303","state":"working","pluginContract":81},`+
		`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"entryConfirmedAt":%q,"slot":{"index":3,"admittedAt":%q}}`,
		at(-time.Minute), at(-20*time.Minute), at(-25*time.Minute), at(-30*time.Minute),
		at(-115*time.Minute), at(-114*time.Minute), at(-2*time.Hour))
	// One root for each arm the state names, a root it names two arms for, and a root in
	// implementing whose architect's claim cannot be reached while its implementer is behind the
	// entry its row names: its architect is named with the arm and no numbers, and its
	// implementer with the numbers, which are the implementer's.
	named := `"LEGION-400":{"key":"LEGION-400","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-400",` +
		`"workers":{},"phaseEntry":1,"givenEntry":1,"architectStalls":["took-nothing"]},` +
		`"LEGION-401":{"key":"LEGION-401","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-401",` +
		`"workers":{},"phaseEntry":2,"givenEntry":1,"architectStalls":["claim-unreachable"]},` +
		`"LEGION-402":{"key":"LEGION-402","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-402",` +
		`"workers":{},"phaseEntry":1,"givenEntry":1,"architectStalls":["slow-before-gate"]},` +
		`"LEGION-403":{"key":"LEGION-403","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-403",` +
		`"workers":{},"phaseEntry":1,"givenEntry":1,"architectStalls":["took-nothing","claim-unreachable"]},` +
		`"LEGION-404":{"key":"LEGION-404","generation":1,"phase":"implementing","status":"in_progress","tree":"LEGION-404",` +
		`"workers":{},"phaseEntry":3,"givenEntry":2,"architectStalls":["claim-unreachable"]}`
	for _, tc := range []struct {
		name, issues, want string
	}{
		{
			name:   "two roles behind their entry, by issue key",
			issues: `{"issues":{` + workers + `}}`,
			want: "roles not given their phase entry, and roots a wake names: 2 — LEGION-208 implementer: entry 3, given 2; " +
				"LEGION-210 reviewer: entry 5, given 4\n",
		},
		{
			name:   "a root whose entries merely differ, and roots whose fields look stalled while the state names no arm",
			issues: `{"issues":{` + unnamed + `}}`,
			want:   "roles not given their phase entry, and roots a wake names: none\n",
		},
		{
			name:   "each root the state names an arm for, with the words of each arm",
			issues: `{"issues":{` + named + `}}`,
			want: "roles not given their phase entry, and roots a wake names: 6 — " +
				"LEGION-400 architect: entry 1, given 1, the architect took nothing; " +
				"LEGION-401 architect: entry 2, given 1, the architect's claim cannot be reached; " +
				"LEGION-402 architect: entry 1, given 1, the root is slow before its gate; " +
				"LEGION-403 architect: entry 1, given 1, the architect took nothing, the architect's claim cannot be reached; " +
				"LEGION-404 architect: the architect's claim cannot be reached; " +
				"LEGION-404 implementer: entry 3, given 2\n",
		},
		{
			name:   "none",
			issues: `{"issues":{` + `"LEGION-209":{"key":"LEGION-209","generation":1,"phase":"testing","status":"testing","workers":{},"phaseEntry":2,"givenEntry":2}` + `}}`,
			want:   "roles not given their phase entry, and roots a wake names: none\n",
		},
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
