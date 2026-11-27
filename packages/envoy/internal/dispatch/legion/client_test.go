package legion

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
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

// TestTheStateReadIsBoundedByTheRequestTimeout: the window is sixty seconds, room for a full read
// of the INFRA daemon's state (10.9 to 11.9 s on 2026-11-21, about 3.4 ms more with each issue),
// and New gives it to every client it builds, so a daemon that accepts a connection and never
// answers ends one pass rather than holding the minute's loop for good.
func TestTheStateReadIsBoundedByTheRequestTimeout(t *testing.T) {
	if RequestTimeout != 60*time.Second {
		t.Fatalf("RequestTimeout = %s, want 60s", RequestTimeout)
	}
	if window := New("http://daemon.internal.example").http.Timeout; window != RequestTimeout {
		t.Fatalf("New gives a client a %s window, want RequestTimeout (%s)", window, RequestTimeout)
	}
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()

	// The client's window is what ends the read, shrunk here so the test does not wait the
	// production sixty seconds out.
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

// stateOfSize is a state document of exactly size bytes, valid JSON throughout, so what refuses
// one is the cap and not the decode.
func stateOfSize(size int) string {
	const head, tail = `{"issues":{},"pad":"`, `"}`
	return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
}

// TestABodyOverTheCapIsAFailedRead: a daemon answering more than ResponseLimit, 32 MiB, is a
// failed read, never a parse of the first 32 MiB, and the refusal names the cap it passed. A body
// exactly at the cap is read, so the bound refuses only what is over it.
func TestABodyOverTheCapIsAFailedRead(t *testing.T) {
	if ResponseLimit != 32<<20 {
		t.Fatalf("ResponseLimit = %d, want 32 MiB (%d)", ResponseLimit, 32<<20)
	}
	over := stateServer(t, stateOfSize(32<<20+1))

	_, bytes, err := New(over.URL).State(t.Context())
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "33554432-byte cap") {
		t.Errorf("the refusal = %q, want it to name the 33554432-byte cap", err)
	}
	if bytes != 32<<20+1 {
		t.Errorf("bytes = %d, want the point the read stopped (%d)", bytes, 32<<20+1)
	}

	atTheCap := stateServer(t, stateOfSize(32<<20))
	state, bytes, err := New(atTheCap.URL).State(t.Context())
	if err != nil {
		t.Fatalf("a read of a body exactly at the cap: %v", err)
	}
	if bytes != 32<<20 {
		t.Errorf("bytes = %d, want the cap (%d)", bytes, 32<<20)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none", state.Issues)
	}
}

// TestANineMiBStateIsReadWhole: a state past the 8 MiB the cap was before, as the INFRA daemon's
// will be from about 2026-11-23, is read whole and parsed, not refused.
func TestANineMiBStateIsReadWhole(t *testing.T) {
	server := stateServer(t, stateOfSize(9<<20))
	state, bytes, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("a read of a 9 MiB state: %v, want it read whole", err)
	}
	if bytes != 9<<20 {
		t.Errorf("bytes = %d, want the whole body (%d)", bytes, 9<<20)
	}
	if state.Issues == nil {
		t.Errorf("the 9 MiB state parsed to no issues map, want the empty map it sent")
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

// waitsAndSettingsState is a state whose issues carry a wait in every shape the read takes — none,
// null, a string, and an object holding members Dispatch models nowhere — and whose settings hold
// a value of each JSON kind, nested ones among them.
const waitsAndSettingsState = `{
  "issues": {
    "LEGION-1": {"key": "LEGION-1", "phase": "implementing"},
    "LEGION-2": {"key": "LEGION-2", "phase": "awaiting_person", "waitingOn": null},
    "LEGION-3": {"key": "LEGION-3", "phase": "awaiting_person", "waitingOn": "a person"},
    "LEGION-4": {"key": "LEGION-4", "phase": "awaiting_person", "waitingOn": {
      "kind": "gate", "gateVersion": 4, "since": "2026-11-03T09:00:00Z", "next": {"on": ["a", "b"], "at": null}}}
  },
  "settings": {"project": "LEGION", "admission_cap": 6, "linger_hours": 1.5, "github_apps": null,
    "review": true, "hooks": [{"name": "setup"}], "gates": {"design": true}}
}`

// TestTheWaitAndTheSettingsAreRecordedAsTheDaemonSentThem: Dispatch models neither an issue's wait
// nor the settings, so a read records each as the daemon sent it, whatever its shape, beside the
// members it does model; and a settings block that is not an object is a failed read naming the
// member in fixed text, which quotes nothing of it.
func TestTheWaitAndTheSettingsAreRecordedAsTheDaemonSentThem(t *testing.T) {
	state, _, err := New(stateServer(t, waitsAndSettingsState).URL).State(t.Context())
	if err != nil {
		t.Fatalf("read a state carrying a wait of each shape: %v", err)
	}
	recorded := map[string]string{}
	for _, observation := range state.Observations() {
		recorded[observation.IssueKey+"/"+observation.Field] = string(observation.Value)
	}
	for key, want := range map[string]string{
		"LEGION-1/" + FieldWaitingOn: `null`,
		"LEGION-2/" + FieldWaitingOn: `null`,
		"LEGION-3/" + FieldWaitingOn: `"a person"`,
		"LEGION-4/" + FieldWaitingOn: `{"kind":"gate","gateVersion":4,"since":"2026-11-03T09:00:00Z","next":{"on":["a","b"],"at":null}}`,
		"LEGION-4/" + FieldPhase:     `"awaiting_person"`,
		AdmissionKey + "/" + FieldSettings: `{"admission_cap":6,"gates":{"design":true},"github_apps":null,` +
			`"hooks":[{"name":"setup"}],"linger_hours":1.5,"project":"LEGION","review":true}`,
	} {
		if got, observed := recorded[key]; !observed || got != want {
			t.Errorf("%s recorded %q (observed %t), want %s", key, got, observed, want)
		}
	}

	const kept = "settings-text-kept-out"
	_, _, err = New(stateServer(t, `{"issues":{},"settings":"`+kept+`"}`).URL).State(t.Context())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a state whose settings are a string: err = %v, want a failed read", err)
	}
	if !strings.Contains(err.Error(), `the answer's "settings"`) || strings.Contains(err.Error(), kept) {
		t.Errorf("the failed read reads %q, want it to name the settings and quote nothing of them", err)
	}
}

// TestTheStateReadStatesNoContractAndKeepsP0Holders: the sampler's read states no plugin
// contract, so a daemon places it in no pod and serves it the body of the daemon's own contract,
// which carries `admission.p0Holders` (the daemon's api.contractsFor; the daemon's own row,
// TestACallerInNoPodStatingNoContractIsServedTheP0Holders, pins that side). The decode keeps the
// number and the read's admission observation records it, which is what the board's header
// copies (dispatch://INFRA-5636). A read that stated a contract below the P0 start's entry would
// be served no number, and every board would give null.
func TestTheStateReadStatesNoContractAndKeepsP0Holders(t *testing.T) {
	const body = `{"admission":{"cap":2,"p0Holders":3,"active":["ACME-1","ACME-2","ACME-3","ACME-4","ACME-5"]},
	  "issues":{"ACME-1":{"key":"ACME-1","phase":"implementing","slot":{"index":0}}}}`
	asked := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case asked <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	state, _, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("read a state carrying admission.p0Holders: %v", err)
	}
	var header http.Header
	select {
	case header = <-asked:
	default:
		t.Fatal("the daemon was asked nothing, so this test proves nothing")
	}
	// The daemon's pluginContractHeader (packages/daemon/internal/api/shaping.go).
	if stated := header.Values("Legion-Plugin-Contract"); len(stated) != 0 {
		t.Errorf("the read states Legion-Plugin-Contract %v, want none: the daemon serves its own body, P0 holders included, to a caller in no pod that states none", stated)
	}
	if state.Admission == nil || state.Admission.P0Holders == nil || *state.Admission.P0Holders != 3 {
		t.Fatalf("the decoded admission = %+v, want the daemon's 3 P0 holders", state.Admission)
	}
	for _, observation := range state.Observations() {
		if observation.IssueKey != AdmissionKey || observation.Field != FieldAdmission {
			continue
		}
		if recorded := AdmissionOf(observation.Value); recorded == nil || recorded.P0Holders == nil || *recorded.P0Holders != 3 {
			t.Errorf("the read records the admission block %s, want the daemon's 3 P0 holders in it", observation.Value)
		}
		return
	}
	t.Fatal("the read records no admission block, so the board has no number to copy")
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
	// A port that refuses every dial: the dial fails, which is the failure a daemon that is down
	// gives, and the one whose text names the address. The test holds the port until it ends; a
	// closed server's port is any process's to take, and one that takes it answers.
	address := testport.Refused(t)

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
