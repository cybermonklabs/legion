package legion

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stateServer is a daemon stand-in answering GET /legion/v1/state with body, and anything else
// with 404.
func stateServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestTheStateReadIsBoundedByTheRequestTimeout: the window is the plan's D14, twenty seconds, and
// New gives it to every client it builds, so a daemon that accepts a connection and never answers
// ends one pass rather than holding the minute's loop for good.
func TestTheStateReadIsBoundedByTheRequestTimeout(t *testing.T) {
	if RequestTimeout != 20*time.Second {
		t.Fatalf("RequestTimeout = %s, want the 20s of the plan's D14", RequestTimeout)
	}
	if window := New("http://daemon.internal.example").http.Timeout; window != RequestTimeout {
		t.Fatalf("New gives a client a %s window, want RequestTimeout (%s)", window, RequestTimeout)
	}
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()

	// The client's window is what ends the read, shrunk here so the test does not wait the
	// production twenty seconds out.
	client := New(silent.URL, WithTimeout(150*time.Millisecond))

	started := time.Now()
	_, bytes, err := client.State(t.Context())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a daemon that never answers: err = %v, want one wrapping ErrUnavailable", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the read waited %s, want the client's own window", waited)
	}
	if bytes != 0 {
		t.Errorf("bytes = %d, want none: nothing was read", bytes)
	}
}

// TestABodyOverTheCapIsAFailedRead: a daemon answering more than ResponseLimit is a failed read,
// never a parse of the first 8 MiB. A body exactly at the cap is read, so the bound refuses only
// what is over it.
func TestABodyOverTheCapIsAFailedRead(t *testing.T) {
	// Valid JSON throughout, so what refuses it is the cap and not the decode.
	envelope := `{"issues":{},"pad":"` + `"}`
	padding := strings.Repeat("a", ResponseLimit+1-len(envelope))
	over := stateServer(t, `{"issues":{},"pad":"`+padding+`"}`)

	_, bytes, err := New(over.URL).State(t.Context())
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
	}
	if bytes != ResponseLimit+1 {
		t.Errorf("bytes = %d, want the point the read stopped (%d)", bytes, ResponseLimit+1)
	}

	atTheCap := stateServer(t, `{"issues":{},"pad":"`+padding[:len(padding)-1]+`"}`)
	state, bytes, err := New(atTheCap.URL).State(t.Context())
	if err != nil {
		t.Fatalf("a read of a body exactly at the cap: %v", err)
	}
	if bytes != ResponseLimit {
		t.Errorf("bytes = %d, want the cap (%d)", bytes, ResponseLimit)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none", state.Issues)
	}
}

// newFieldsState is a state document as a daemon on dispatch://INFRA-980 serves it: every issue
// holding a slot in admission.active, children included; waiting ordered by tree root; a blocked
// parent waiting for its children; a new admission.trees; a new per-issue tree; and a higher
// contract version. Every one of those is a field this package names nowhere, except tree, which
// it keeps optional.
const newFieldsState = `{
  "daemon": {"project": "INFRA", "contract": 47, "schema": 212},
  "admission": {
    "cap": 4,
    "active": ["INFRA-1", "INFRA-2"],
    "waiting": ["INFRA-3"],
    "trees": ["INFRA-1"],
    "blocked": [{"issue": "INFRA-1", "waitingOn": ["INFRA-2"], "kind": "children"}]
  },
  "issues": {
    "INFRA-1": {
      "key": "INFRA-1", "generation": 3, "phase": "implementing", "status": "doing",
      "tree": "INFRA-1",
      "slot": {"index": 0, "admittedAt": "2026-11-03T10:00:00Z"},
      "architect": {"session": "s-1", "state": "working", "pluginContract": 47},
      "workers": {"implementer": {"claim": {"session": "s-2", "state": "working", "pluginContract": 47}, "rounds": 1}}
    },
    "INFRA-2": {
      "key": "INFRA-2", "generation": 1, "phase": "planning", "status": "doing",
      "tree": "INFRA-1",
      "slot": {"index": 1, "admittedAt": "2026-11-03T10:01:00Z"},
      "workers": {"planner": {"claim": {"session": "s-3", "state": "ready", "pluginContract": 47}, "rounds": 1}}
    }
  },
  "pendingStatusWrites": [], "heldOutboxRows": [], "strandedClaims": [],
  "capabilities": [], "consumers": [], "repositories": {"owners": [], "mapped": []},
  "settings": {"concurrency": 4}
}`

// TestAStateWithTheNewAdmissionFieldsParses: the decode is loose, so a daemon that grows the route
// under dispatch://INFRA-980 is still read. Nothing here assumes a slot holder is a root: INFRA-2
// is a child of INFRA-1 and holds slot 1, and its slot is read from its own record.
func TestAStateWithTheNewAdmissionFieldsParses(t *testing.T) {
	server := stateServer(t, newFieldsState)

	state, bytes, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("read a state carrying dispatch://INFRA-980's new fields: %v", err)
	}
	if bytes != len(newFieldsState) {
		t.Errorf("bytes = %d, want the %d the daemon sent", bytes, len(newFieldsState))
	}
	if len(state.Issues) != 2 {
		t.Fatalf("issues = %v, want both", state.Issues)
	}
	child := state.Issues["INFRA-2"]
	if child.Tree == nil || *child.Tree != "INFRA-1" {
		t.Errorf("INFRA-2's tree = %v, want the root INFRA-1 the new field names", child.Tree)
	}
	if child.Slot == nil || child.Slot.Index != 1 {
		t.Errorf("INFRA-2's slot = %v, want slot 1 read from the issue's own record", child.Slot)
	}
	if child.Workers["planner"].Claim.State != "ready" {
		t.Errorf("INFRA-2's planner = %v, want its claim state", child.Workers["planner"])
	}
}

// TestAStateFromTodaysDaemonParses: the same decode reads a daemon that carries none of those
// fields — no tree, no admission.trees — so the two shapes are read by one reader.
func TestAStateFromTodaysDaemonParses(t *testing.T) {
	const today = `{"daemon":{"project":"INFRA","contract":46},
	  "admission":{"cap":4,"active":["INFRA-1"],"waiting":[],"blocked":[]},
	  "issues":{"INFRA-1":{"key":"INFRA-1","phase":"reviewing","status":"doing",
	    "slot":{"index":0,"admittedAt":"2026-11-03T10:00:00Z"},
	    "workers":{"reviewer":{"claim":{"session":"s-9","state":"working"},"rounds":2}}}}}`
	server := stateServer(t, today)

	state, _, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("read today's state: %v", err)
	}
	issue := state.Issues["INFRA-1"]
	if issue.Tree != nil {
		t.Errorf("tree = %v, want none: this daemon sends no tree field", *issue.Tree)
	}
}

// TestAMalformedOrRefusedAnswerIsAFailedRead: a body that is not the route's JSON, and a status
// that is not 200, are each a failed read, so the sampler records the failure and leaves the last
// good observations standing.
func TestAMalformedOrRefusedAnswerIsAFailedRead(t *testing.T) {
	malformed := stateServer(t, `{"issues": {`)
	if _, bytes, err := New(malformed.URL).State(t.Context()); !errors.Is(err, ErrUnavailable) || bytes == 0 {
		t.Fatalf("a malformed body: err = %v bytes = %d, want a failed read of what it sent", err, bytes)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer refusing.Close()
	_, _, err := New(refusing.URL).State(t.Context())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a 503: err = %v, want a failed read naming the status", err)
	}
}

// TestAStateReadFollowsNoRedirect: a daemon answers its own routes and redirects nowhere, so a
// 3xx is the answer it gave — a failed read naming the status — and not a second call. Following
// one would read a state at a host no project's setting names.
func TestAStateReadFollowsNoRedirect(t *testing.T) {
	elsewhere := stateServer(t, `{"issues":{"INFRA-1":{"key":"INFRA-1","phase":"implementing"}}}`)
	var followed int
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed++
		http.Redirect(w, r, elsewhere.URL+StatePath, http.StatusFound)
	}))
	defer redirecting.Close()

	state, _, err := New(redirecting.URL).State(t.Context())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a read of a daemon that redirects: err = %v, want a failure naming the 302", err)
	}
	if len(state.Issues) != 0 {
		t.Errorf("the read answered %v, want nothing: the daemon it names did not answer it", state.Issues)
	}
	if followed != 1 {
		t.Errorf("the client made %d calls, want the one the setting's host answered", followed)
	}
}

// TestAFailedCallNamesNoAddress: legion_reads.error is shown on the project's board to every
// reader of the project, and the daemon's address belongs to the deployment, so what is recorded
// is the transport's reason without the URL net/http's *url.Error carries or the host and port
// its *net.OpError names.
func TestAFailedCallNamesNoAddress(t *testing.T) {
	// A port nothing listens on: the dial fails, which is the failure a daemon that is down
	// gives, and the one whose text names the address.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	_, _, err := New("http://" + address).State(t.Context())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a daemon that is down: err = %v, want one wrapping ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), address) {
		t.Errorf("the failure reads %q, want no daemon address in it", err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("the failure reads %q, want the transport's own reason", err)
	}
}
