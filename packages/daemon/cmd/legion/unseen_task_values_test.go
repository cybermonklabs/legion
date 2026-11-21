package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An operator reading plain `legion state` is told which claims' task values nobody reported
// seeing for a whole first bound (api.State.UnseenTaskValues), each by claim with the task its
// value names and when the daemon last wrote it, on one line however many there are; with none
// the line says so. The relay to those workers failed, so the daemon charges nobody and writes
// each value again once per bound: the line is what says the listeners are not relaying.
func TestPlainStateNamesTheClaimsWhoseTaskValueIsUnseen(t *testing.T) {
	daemon := `"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"issues":{},"pendingStatusWrites":[],"strandedClaims":[]`
	for _, tc := range []struct {
		name, unseen, want string
	}{
		{
			name: "two claims, in the state's claim order",
			unseen: `"unseenTaskValues":[` +
				`{"claim":"legion-legion-legion-208-implementer","issue":"LEGION-208","role":"implementer","task":3,"writtenAt":"2026-11-13T17:05:00Z"},` +
				`{"claim":"legion-legion-legion-210-reviewer","issue":"LEGION-210","role":"reviewer","task":1,"writtenAt":"2026-11-13T17:06:30Z"}]`,
			want: "task values nobody reported seeing: 2 — legion-legion-legion-208-implementer task 3 written 2026-11-13T17:05:00Z; " +
				"legion-legion-legion-210-reviewer task 1 written 2026-11-13T17:06:30Z\n",
		},
		{name: "none", unseen: `"unseenTaskValues":[]`, want: "task values nobody reported seeing: none\n"},
		{name: "a daemon before the key", unseen: `"wakesWaiting":[]`, want: "task values nobody reported seeing: none\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := "{" + daemon + "," + tc.unseen + "}"
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
