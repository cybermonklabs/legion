package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// `legion replay collect` prints each ancestor on a Dispatch parent chain whose events Dispatch did
// not give, which window.json names in missingAncestors (spec R10: the record's missing input is
// reported). The window's one line names ACME-1, which stands under ACMEAUX-1, which stands under
// ACMEAUX-2; the Dispatch the command reads serves the events of ACME-1 and ACMEAUX-1, lists no
// issue of ACME, holds an empty event log, and answers that it has no ACMEAUX-2.
func TestReplayCollectPrintsEachAncestorDispatchDoesNotShow(t *testing.T) {
	filed := func(id int, key, parent string) string {
		project, _, _ := strings.Cut(key, "-")
		return fmt.Sprintf(`{"id":%d,"issue_key":%q,"artifact_id":null,"project":%q,"seq":1,"type":"issue.created",`+
			`"actor":{"kind":"user","id":"acme-person"},"notify":true,"created_at":"2001-03-05T08:30:00Z",`+
			`"payload":{"key":%q,"project":%q,"status":"todo","parent":%q,"blocked_by":[],"priority":null}}`, id, key, project, key, project, parent)
	}
	served := map[string]string{"ACME-1": filed(1, "ACME-1", "ACMEAUX-1"), "ACMEAUX-1": filed(2, "ACMEAUX-1", "ACMEAUX-2")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key, events := strings.CutPrefix(r.URL.Path, "/api/v1/issues/")
		key, events = strings.CutSuffix(key, "/events")
		switch {
		case r.Method != http.MethodGet:
		case r.URL.Path == "/api/v1/events/log":
			fmt.Fprint(w, "[]")
			return
		case r.URL.Path == "/api/v1/issues":
			fmt.Fprintf(w, `{"issues":[],"total":0,"limit":%s,"offset":0}`, r.URL.Query().Get("limit"))
			return
		case events && served[key] != "":
			fmt.Fprintf(w, "[%s]", served[key])
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"ISSUE_NOT_FOUND","error":"issue not found"}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("DISPATCH_URL", server.URL)
	dir := t.TempDir()
	logs, token := filepath.Join(dir, "daemon.log"), filepath.Join(dir, "dispatch-token")
	line := `{"time":"2001-03-05T09:30:00Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-1","slots":1,"cap":1}`
	if err := os.WriteFile(logs, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("a-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := legionRun(t, "replay", "collect", "--project", "ACME", "--from", "2001-03-05T09:00:00Z", "--to", "2001-03-05T10:00:00Z",
		"--before", "1h", "--log", logs, "--out", filepath.Join(dir, "window"), "--dispatch-token-file", token, "--collector", record.MainRule.Commit)
	want := "legion replay collect: window.json names the ancestors on a Dispatch parent chain that Dispatch does not show (missingAncestors): ACMEAUX-2\n"
	if code != 0 || !strings.Contains(stdout, want) {
		t.Errorf("legion replay collect = %d, stderr %q, stdout:\n%s\nwant 0 and the line\n%s", code, stderr, stdout, want)
	}
}
