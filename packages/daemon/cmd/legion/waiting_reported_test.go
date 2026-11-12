package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// A waiting issue is reported waiting and blocked. This half reads what an operator running
// plain `legion state` is told of one: an issue the daemon started and that waits on another
// issue stands on the `blocked on Dispatch links:` line with the keys it waits on, beside the
// issues Legion has not started that a link holds, because `blocked` is one list of everything
// Legion is waiting to start behind a Dispatch issue nobody has finished and the command names
// it whatever put each issue there (blockedSummary). What the state itself carries for them is
// the other half, under this name in packages/daemon/internal/api: the `admission` object the
// route serves. Neither half reads the other's reader, so neither proves what the other does.
//
// The state served here is the one that package's half projects from its records after an
// admission pass: the two started issues that wait — one on an unfinished Dispatch link, one on
// an open child its own first phase waits for — the root the daemon has not started that a link
// holds, and the child held at admitted. The command is given it by a server of its own, as
// every other plain-output test of this file is.
func TestAWaitingIssueIsReportedWaitingAndBlocked(t *testing.T) {
	const (
		startedLink  = "LEGION-210"
		startedChild = "LEGION-211"
		queuedRoot   = "LEGION-212"
		heldChild    = "LEGION-213"
	)
	// The four issues and what each waits on, which the served state and the line expected of
	// it are both built from: an expectation written out beside a fixture of its own would read
	// the same whatever the fixture said.
	waits := []struct{ issue, blocker string }{
		{startedLink, "LEGION-300"},
		{startedChild, "LEGION-301"},
		{queuedRoot, "LEGION-302"},
		{heldChild, "LEGION-303"},
	}
	blocked := make([]string, 0, len(waits))
	named := make([]string, 0, len(waits))
	for _, wait := range waits {
		blocked = append(blocked, fmt.Sprintf(`{"issue":%q,"blockedBy":[%q]}`, wait.issue, wait.blocker))
		named = append(named, wait.issue+" waits on "+wait.blocker)
	}
	want := fmt.Sprintf("blocked on Dispatch links: %d — %s\n", len(waits), strings.Join(named, "; "))
	served := `{"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-11-11T00:00:00Z","startedAt":"2026-11-11T00:00:00Z"},` +
		`"admission":{"cap":8,"active":["LEGION-215","LEGION-220"],` +
		fmt.Sprintf(`"waiting":[%q,%q,%q],`, startedLink, startedChild, queuedRoot) +
		fmt.Sprintf(`"trees":[%q,%q,"LEGION-215","LEGION-220"],`, startedLink, startedChild) +
		`"blocked":[` + strings.Join(blocked, ",") + `]},` +
		`"issues":{},"pendingStatusWrites":[]}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(served))
	}))
	defer server.Close()
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_ISSUE", "")

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
		t.Fatalf("legion state = %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), want) {
		t.Fatalf("legion state printed %q, want the line %q", out.String(), want)
	}

	// The same line from the summary itself, over the state's own blocked list: the command
	// prints what blockedSummary builds, and each of the four issues of the fixture stands on
	// it.
	var state struct {
		Admission struct {
			Blocked []api.BlockedIssue `json:"blocked"`
		} `json:"admission"`
	}
	if err := json.Unmarshal([]byte(served), &state); err != nil {
		t.Fatalf("decode the served state: %v", err)
	}
	if got := blockedSummary(state.Admission.Blocked) + "\n"; got != want {
		t.Fatalf("blockedSummary = %q, want %q", got, want)
	}
}
