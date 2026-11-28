package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// Plain `legion state` names each issue whose branch row GitHub refused for good
// (api.Issue.BranchRefused), in key order, one line each under a count: the row, what it wrote, the
// refusal's kind, when, and the row's error. A state with no refused branch says none.
func TestPlainStateNamesTheRefusedBranches(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory.
	at := time.Date(2026, 11, 27, 21, 30, 0, 0, time.UTC)
	refused := map[string]api.Issue{
		"LEGION-210": {Key: "LEGION-210", BranchRefused: &api.BranchRefusedView{Row: 140, Write: "create", Kind: "forbidden", At: at,
			Reason: "create the branch of LEGION-210: GitHub answered POST /git/refs with 403"}},
		"LEGION-208": {Key: "LEGION-208", BranchRefused: &api.BranchRefusedView{Row: 121, Write: "move", Kind: "no-node-id", At: at,
			Reason: "read the node id of acme/widgets: GitHub named none"}},
		"LEGION-209": {Key: "LEGION-209"},
	}
	want := []string{
		"refused issue branches: 2",
		"  LEGION-208 row 121, move refused as no-node-id at 2026-11-27T21:30:00Z: read the node id of acme/widgets: GitHub named none",
		"  LEGION-210 row 140, create refused as forbidden at 2026-11-27T21:30:00Z: create the branch of LEGION-210: GitHub answered POST /git/refs with 403",
	}
	if got := branchRefusedLines(refused); !slices.Equal(got, want) {
		t.Fatalf("branchRefusedLines = %q, want %q", got, want)
	}
	if got, want := branchRefusedLines(map[string]api.Issue{"LEGION-209": {Key: "LEGION-209"}}), []string{"refused issue branches: none"}; !slices.Equal(got, want) {
		t.Fatalf("branchRefusedLines with none refused = %q, want %q", got, want)
	}

	served := `{"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"pendingStatusWrites":[],"issues":{"LEGION-208":{"key":"LEGION-208","generation":2,"phase":"admitted","status":"in_progress",` +
		`"branchRefused":{"row":121,"write":"move","kind":"no-node-id","reason":"read the node id of acme/widgets: GitHub named none","at":"2026-11-27T21:30:00Z"}}}}`
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
	if line := want[0][:len("refused issue branches: ")] + "1\n" + want[1] + "\n"; !strings.Contains(out.String(), line) {
		t.Fatalf("legion state printed %q, want the lines %q", out.String(), line)
	}
}
