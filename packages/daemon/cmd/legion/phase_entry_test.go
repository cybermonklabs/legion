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
// names a root only where a wake of the daemon names it (main.go's rootWakeArm), and names the
// arm beside the numbers. The three arms are dispatch://INFRA-2614's R7: its architect took
// nothing, its claim is failed or retired, or it is slow before its gate.
//
// The role is the one the issue's current phase names (phase.RoleFor): an issue whose phase
// names no role carries neither number, so the line never names one.
func TestStatePlainNamesTheRolesBehindTheirEntryAndTheRootsAWakeNames(t *testing.T) {
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
	// A root whose architect was queued the task of its entry a minute ago and has not confirmed
	// it yet, and one whose transition raised an entry the daemon has still to deliver: each
	// reads as behind and neither is a failure, so the line names neither.
	healthy := fmt.Sprintf(`"LEGION-300":{"key":"LEGION-300","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-300",`+
		`"workers":{},"architect":{"session":"ses_architect_300","state":"working","pluginContract":81},`+
		`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"slot":{"index":0,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-300","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-301":{"key":"LEGION-301","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-301",`+
		`"workers":{},"architect":{"session":"ses_architect_301","state":"working","pluginContract":81},`+
		`"phaseEntry":2,"givenEntry":1,"slot":{"index":1,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-301","currentVersion":1,"approvedVersion":null}}`,
		at(-time.Minute), at(-2*time.Minute), at(-time.Minute))
	// One root for each arm: a task queued 20 minutes ago that no turn confirmed, a failed
	// architect claim, and a root that has stood at admitted with no registered design gate for
	// two hours.
	woken := fmt.Sprintf(`"LEGION-400":{"key":"LEGION-400","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-400",`+
		`"workers":{},"architect":{"session":"ses_architect_400","state":"ready","pluginContract":81},`+
		`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"slot":{"index":0,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-400","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-401":{"key":"LEGION-401","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-401",`+
		`"workers":{},"architect":{"session":"ses_architect_401","state":"failed","pluginContract":81},`+
		`"phaseEntry":2,"givenEntry":1,"slot":{"index":1,"admittedAt":%q},`+
		`"designGate":{"artifactId":"d-401","currentVersion":1,"approvedVersion":null}},`+
		`"LEGION-402":{"key":"LEGION-402","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-402",`+
		`"workers":{},"architect":{"session":"ses_architect_402","state":"working","pluginContract":81},`+
		`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"entryConfirmedAt":%q,"slot":{"index":2,"admittedAt":%q}}`,
		at(-20*time.Minute), at(-25*time.Minute), at(-30*time.Minute),
		at(-115*time.Minute), at(-114*time.Minute), at(-2*time.Hour))
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
			name:   "a root whose entries merely differ, and one holding a task nobody has confirmed yet",
			issues: `{"issues":{` + healthy + `}}`,
			want:   "roles not given their phase entry, and roots a wake names: none\n",
		},
		{
			name:   "one root for each wake arm, with the arm that names it",
			issues: `{"issues":{` + woken + `}}`,
			want: "roles not given their phase entry, and roots a wake names: 3 — " +
				"LEGION-400 architect: entry 1, given 1, its architect took nothing; " +
				"LEGION-401 architect: entry 2, given 1, its claim is failed; " +
				"LEGION-402 architect: entry 1, given 1, slow before its gate\n",
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

// The plain line reads the gate arm as the daemon does: a root has registered its gate once any
// step the deployment gates holds a document, which is any gate row of the root (admit's
// architectSlowBeforeGate), and not only once its spec step does. `designGate` carries the spec
// step alone, so a deployment that gates the decisions step without the spec serves every root
// with no `designGate`, and a line that read that key alone would name every root at admitted
// after 90 minutes, a root whose decisions document is registered and waiting on a person among
// them. `gatedSteps` says which step is registered.
//
// The line reads the arms over a tree's own root alone, as the daemon does: a child at admitted
// with a slot taken two hours ago is the architect's phase by phase.RoleFor, and no wake of the
// daemon names it.
func TestStatePlainReadsAGateOfTheDecisionsStepAloneAsTheDaemonDoes(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	root := func(key, status string) string {
		gate := `{"step":"decisions","approvedVersion":null,"status":"unregistered"}`
		if status != "unregistered" {
			gate = `{"step":"decisions","artifactId":"d-` + key + `","currentVersion":1,"approvedVersion":null,"status":"` + status + `"}`
		}
		return fmt.Sprintf(`"%s":{"key":"%s","generation":1,"phase":"admitted","status":"in_progress","tree":"%s",`+
			`"workers":{},"architect":{"session":"ses_%s","state":"working","pluginContract":81},`+
			`"phaseEntry":1,"givenEntry":1,"entryQueuedAt":%q,"entryConfirmedAt":%q,"slot":{"index":0,"admittedAt":%q},`+
			`"gatedSteps":[%s]}`, key, key, key, key, at(-115*time.Minute), at(-114*time.Minute), at(-2*time.Hour), gate)
	}
	child := fmt.Sprintf(`"LEGION-502":{"key":"LEGION-502","generation":1,"phase":"admitted","status":"in_progress","tree":"LEGION-500",`+
		`"workers":{},"phaseEntry":1,"givenEntry":0,"slot":{"index":2,"admittedAt":%q}}`, at(-2*time.Hour))
	issues := `{"issues":{` + root("LEGION-500", "waiting") + "," + root("LEGION-501", "unregistered") + "," + child + `}}`
	daemon := `"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":3,"active":[],"waiting":[]},"pendingStatusWrites":[],"strandedClaims":[]`
	served := "{" + daemon + "," + strings.TrimSuffix(strings.TrimPrefix(issues, "{"), "}") + "}"
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
	want := "roles not given their phase entry, and roots a wake names: 1 — LEGION-501 architect: entry 1, given 1, slow before its gate\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("legion state printed %q, want the line %q: LEGION-500 registered its decisions step, and LEGION-502 is no tree's root", out.String(), want)
	}
}
