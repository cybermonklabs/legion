// packages/envoy/cmd/dev-proxy-stands/enroll_test.go
//
// enroll-pod's --projects, as the local stack enrols a person's pod with it: the keys go in the
// enrolment's body as agent-secrets enroll --pod --projects sends them, a run without the flag
// sends no projects key, and a list that is no list is refused before the broker is asked.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// enrolBroker is a broker that answers enroll-pod's two calls and keeps the enrolment's body.
type enrolBroker struct {
	mu     sync.Mutex
	bodies []map[string]any
	calls  int
}

func (b *enrolBroker) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/launcher-credentials/token":
		_, _ = io.WriteString(w, `{"credential_id": "the-launcher-credential"}`)
	case "/v1/enrollments":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.bodies = append(b.bodies, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"enrollment_id": "the-enrollment", "lease_expires_at": "2100-01-01T00:00:00Z"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestEnrollPodSendsTheProjectsItIsGiven(t *testing.T) {
	broker := &enrolBroker{}
	server := httptest.NewServer(http.HandlerFunc(broker.serve))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "pod-token")
	if err := os.WriteFile(tokenFile, []byte("the-pod-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enrol := func(extra ...string) (int, string) {
		t.Helper()
		args := append([]string{"--url", server.URL, "--service", "airlock-person", "--token-file", tokenFile,
			"--pod-token-file", tokenFile, "--pod-uid", "the-pod-uid", "--thumbprint", "the-thumbprint"}, extra...)
		var stdout, stderr strings.Builder
		code := runEnrollPod(t.Context(), args, &stdout, &stderr)
		return code, stderr.String()
	}

	if code, stderr := enrol("--projects", "PRIV CORE"); code != 0 {
		t.Fatalf("enroll-pod --projects 'PRIV CORE' exited %d: %s", code, stderr)
	}
	if code, stderr := enrol(); code != 0 {
		t.Fatalf("enroll-pod with no --projects exited %d: %s", code, stderr)
	}
	if len(broker.bodies) != 2 {
		t.Fatalf("the broker was sent %d enrolments; want 2", len(broker.bodies))
	}
	named, _ := broker.bodies[0]["projects"].([]any)
	if !slices.Equal(named, []any{"CORE", "PRIV"}) {
		t.Errorf("the enrolment named projects %v; want [CORE PRIV], sorted as agent-secrets sends them", broker.bodies[0]["projects"])
	}
	if _, sent := broker.bodies[1]["projects"]; sent {
		t.Errorf("an enrolment with no --projects sent projects %v; want no such key", broker.bodies[1]["projects"])
	}

	calls := broker.calls
	if code, stderr := enrol("--projects", "priv,core"); code != exitUsageError || !strings.Contains(stderr, "--projects is not a list of project keys") {
		t.Errorf("a --projects that is no list exited %d, stderr %q; want %d naming --projects", code, stderr, exitUsageError)
	}
	if broker.calls != calls {
		t.Errorf("a refused --projects made %d calls; want none", broker.calls-calls)
	}
}
