package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowStateAnswer is how long the daemon in this test takes to answer `GET /legion/v1/state`.
// It is under the time a live deployment takes and over the budget the command reads with: the
// INFRA daemon, 2368 issues and 610 of them waiting, answered that route in 7.05 s, 7.06 s and
// 7.30 s to the first byte, measured from an issue pod of its own project on 2026-11-12, with
// the whole 4.0 MB body in 7.07 s, 7.08 s and 7.32 s. A command that reads such a daemon with a
// five-second budget prints no state at all, so the waiting line this change adds to the plain
// output, and the `--json` body of the issue's two after-merge checks, reach nobody on that
// deployment.
const slowStateAnswer = 6 * time.Second

// TestStateReadsADaemonSlowerThanTheOldBudget holds `legion state` to the one thing its reader
// needs of it: a daemon that is slow to build its state is read, not given up on. The body is
// the fixture the daemon's own golden test writes, so the command parses what the daemon
// serves, and the handler waits slowStateAnswer before it answers, which is less than a live
// INFRA read takes and more than the command allowed when this test was written.
func TestStateReadsADaemonSlowerThanTheOldBudget(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", "state.json"))
	if err != nil {
		t.Fatalf("read the state fixture: %v", err)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legion/v1/state" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(slowStateAnswer)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(body); err != nil {
			t.Errorf("write the state: %v", err)
		}
	}))
	defer daemon.Close()
	t.Setenv("LEGION_DAEMON_URL", daemon.URL)

	var stdout, stderr bytes.Buffer
	if code := runState(t.Context(), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("legion state = %d, want 0; stderr %s", code, strings.TrimSpace(stderr.String()))
	}
	printed := stdout.String()
	for _, want := range []string{"admission: ", "waiting line: "} {
		if !strings.Contains(printed, want) {
			t.Errorf("legion state printed %q, want a line holding %q", printed, want)
		}
	}
}
