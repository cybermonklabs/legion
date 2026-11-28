package legion

//ci:reads packages/contracts/fixtures/daemon-api/controller.json os.ReadFile (daemonAPIFixture, ../../../../contracts/fixtures/daemon-api): what the daemon's own internal/api TestControllerRouteGoldens writes from GET /legion/v1/controller once a session registered, which the controller read must read
//ci:reads packages/contracts/fixtures/daemon-api/controller-none.json os.ReadFile (daemonAPIFixture, ../../../../contracts/fixtures/daemon-api): what the same golden writes from GET /legion/v1/controller while no session is registered, which the controller read must read as none

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
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

// stuffing is an endless run of the letter a: the padding of a paddedState.
type stuffing struct{}

func (stuffing) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// paddedState streams a state document of size bytes: INFRA's daemon block and one issue,
// INFRA-1 in implementing, whose pad member, which the decode ignores, takes the bytes the rest
// leaves. A reader, so no test holds the document whole.
func paddedState(size int) io.Reader {
	const head = `{"daemon":{"project":"INFRA"},"issues":{"INFRA-1":{"key":"INFRA-1","phase":"implementing","pad":"`
	const tail = `"}}}`
	return io.MultiReader(strings.NewReader(head), io.LimitReader(stuffing{}, int64(size-len(head)-len(tail))), strings.NewReader(tail))
}

// streamedState is a daemon stand-in answering GET /legion/v1/state with the size-byte document
// paddedState streams. It declares the document's Content-Length where declared is set; otherwise
// net/http sends the document chunked. written receives, once the answer is over, the bytes of the
// document the stand-in wrote before the document ended or the client hung up.
func streamedState(t *testing.T, size int, declared bool) (server *httptest.Server, written <-chan int64) {
	t.Helper()
	wrote := make(chan int64, 1)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if declared {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		sent, _ := io.Copy(w, paddedState(size))
		select {
		case wrote <- sent:
		default:
		}
	}))
	t.Cleanup(server.Close)
	return server, wrote
}

// TestTheStateReadAsksForTheObservedView: a whole read asks for `?view=observed`, the view of the
// route that carries only the members State decodes, and no other query.
func TestTheStateReadAsksForTheObservedView(t *testing.T) {
	asked := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r
		_, _ = w.Write([]byte(`{"issues":{}}`))
	}))
	defer server.Close()

	if _, _, err := New(server.URL).State(t.Context()); err != nil {
		t.Fatalf("read the state: %v", err)
	}
	request := <-asked
	if request.URL.Path != StatePath {
		t.Errorf("the read asked for %s, want %s", request.URL.Path, StatePath)
	}
	if query := request.URL.Query(); len(query) != 1 || !slices.Equal(query["view"], []string{"observed"}) {
		t.Errorf("the read's query is %v, want view=observed alone", query)
	}
}

// TestAStateOverTheOldCapIsReadWhole: a state of 9 MiB, over the 8 MiB that the INFRA daemon's
// whole state passes about three days after 2026-11-20, is read whole and its issue is observed.
func TestAStateOverTheOldCapIsReadWhole(t *testing.T) {
	const size = 9 << 20
	server, _ := streamedState(t, size, false)

	state, bytes, err := New(server.URL).State(t.Context())
	if err != nil {
		t.Fatalf("a read of a %d-byte state: %v", size, err)
	}
	if bytes != size {
		t.Errorf("bytes = %d, want the %d the daemon sent", bytes, size)
	}
	observed := slices.ContainsFunc(state.Observations(), func(o Observation) bool {
		return o.IssueKey == "INFRA-1" && o.Field == FieldPhase && JSONString(o.Value) == PhaseImplementing
	})
	if !observed {
		t.Errorf("observations = %v, want INFRA-1's phase %q", state.Observations(), PhaseImplementing)
	}
}

// TestTheStateReadIsBoundedByTheRequestTimeout: the window is the daemon's own bound on the read,
// sixty seconds (dispatch://INFRA-3931's D9), and New gives it to every client it builds, so a
// daemon that accepts a connection and never answers ends one whole read rather than holding its
// project's runner for good.
func TestTheStateReadIsBoundedByTheRequestTimeout(t *testing.T) {
	if RequestTimeout != 60*time.Second {
		t.Fatalf("RequestTimeout = %s, want the daemon's own 60s bound on the read", RequestTimeout)
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

// TestABodyOverTheCapIsAFailedRead: a daemon answering more than ResponseLimit, 64 MiB, is a
// failed read, never a parse of the first 64 MiB, and the failure names the cap and what the read
// knows of the answer's size. A Content-Length over the cap is refused before the body is read,
// and the failure and the size name that length. A body with no Content-Length is read to one byte
// past the cap and no further, whether it ends soon after the cap or never ends: the failure says
// the read stopped there, and the size is that count, ResponseLimit+1. So a daemon that streams
// without end costs one read of the cap and one byte. A body exactly at the cap is read, so the
// bound refuses only what is over it.
func TestABodyOverTheCapIsAFailedRead(t *testing.T) {
	if ResponseLimit != 64<<20 {
		t.Fatalf("ResponseLimit = %d, want 64 MiB (%d)", ResponseLimit, 64<<20)
	}
	stopped := fmt.Sprintf("the answer is over the %d-byte cap: it declares no Content-Length, and the read stopped at %d bytes",
		ResponseLimit, ResponseLimit+1)
	for _, over := range []struct {
		name     string
		size     int
		declared bool
		failure  string
		bytes    int64
		// sentBelow is a bound on the bytes the daemon wrote before the client hung up, 0 for none.
		// A daemon writes past what the client read only into the connection's buffers.
		sentBelow int64
	}{
		{"one byte over the cap, chunked", ResponseLimit + 1, false, stopped, ResponseLimit + 1, 0},
		{"with its Content-Length", 70_000_000, true, "the answer of 70000000 bytes is over the 67108864-byte cap", 70_000_000, ResponseLimit},
		{"chunked", 70_000_000, false, stopped, ResponseLimit + 1, 0},
		{"chunked and without end", 1 << 40, false, stopped, ResponseLimit + 1, 2 * ResponseLimit},
	} {
		t.Run(over.name, func(t *testing.T) {
			server, written := streamedState(t, over.size, over.declared)

			_, bytes, err := New(server.URL).State(t.Context())
			if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), over.failure) {
				t.Errorf("the failure reads %q, want it to name the cap and what the read knows of the size: %q", err, over.failure)
			}
			if int64(bytes) != over.bytes {
				t.Errorf("bytes = %d, want %d", bytes, over.bytes)
			}
			if over.sentBelow == 0 {
				return
			}
			select {
			case sent := <-written:
				if sent >= over.sentBelow {
					t.Errorf("the daemon wrote %d bytes before the client hung up, want fewer than %d: the read stops at the cap", sent, over.sentBelow)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the daemon was still writing 30s after the read ended: the client did not hang up")
			}
		})
	}

	// A document one byte over the cap, which stateServer writes in one call: the failure names
	// the cap's value, 67108864 bytes, and the size is ResponseLimit+1, where the read stopped.
	over := stateServer(t, stateOfSize(ResponseLimit+1))

	_, bytes, err := New(over.URL).State(t.Context())
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "67108864-byte cap") {
		t.Errorf("the refusal = %q, want it to name the 67108864-byte cap", err)
	}
	if bytes != ResponseLimit+1 {
		t.Errorf("bytes = %d, want the point the read stopped (%d)", bytes, ResponseLimit+1)
	}

	atTheCap, _ := streamedState(t, ResponseLimit, false)
	state, bytes, err := New(atTheCap.URL).State(t.Context())
	if err != nil {
		t.Fatalf("a read of a body exactly at the cap: %v", err)
	}
	if bytes != ResponseLimit {
		t.Errorf("bytes = %d, want the cap (%d)", bytes, ResponseLimit)
	}
	if phase := state.Issues["INFRA-1"].Phase; phase != PhaseImplementing {
		t.Errorf("INFRA-1's phase = %q, want %q", phase, PhaseImplementing)
	}
}

// TestABodyCutShortPastTheCapStopsAtTheCap: a daemon that drops the connection in the middle of a
// chunked body, after more than the cap, is a failed read over the cap. The read stops one byte
// past the cap, before the drop, so the failure says the read stopped there and the size is
// ResponseLimit+1, not the bytes the daemon sent.
func TestABodyCutShortPastTheCapStopsAtTheCap(t *testing.T) {
	const sent = ResponseLimit + 1<<20
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(paddedState(2*sent), sent))
		w.(http.Flusher).Flush()
		// The server closes the connection, and the body never gets its last chunk.
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	_, bytes, err := New(server.URL).State(t.Context())
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a body cut short past the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
	}
	if want := fmt.Sprintf("the answer is over the %d-byte cap: it declares no Content-Length, and the read stopped at %d bytes",
		ResponseLimit, ResponseLimit+1); !strings.Contains(err.Error(), want) {
		t.Errorf("the failure reads %q, want %q", err, want)
	}
	if bytes != ResponseLimit+1 {
		t.Errorf("bytes = %d, want the %d the read stopped at", bytes, ResponseLimit+1)
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
	if int64(bytes) != int64(len(newFieldsState)) {
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

// TestTheStateReadStatesNoContractAndKeepsP0Holders: a whole read of the daemon's state states no
// plugin contract, so a daemon places it in no pod and serves it the body of the daemon's own
// contract, which carries `admission.p0Holders` (the daemon's api.contractsFor; the daemon's own
// row, TestACallerInNoPodStatingNoContractIsServedTheP0Holders, pins that side). The decode keeps
// the number and the read's admission observation records it, which is what the board's header
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
// that is not 200, are each a failed read, so a whole read records the failure and leaves the last
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

// controllerServer is a daemon stand-in answering GET /legion/v1/controller with status and body,
// and anything else with 404. asked receives the request of each controller read it answers.
func controllerServer(t *testing.T, status int, body string) (server *httptest.Server, asked <-chan *http.Request) {
	t.Helper()
	requests := make(chan *http.Request, 8)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != ControllerPath {
			http.NotFound(w, r)
			return
		}
		select {
		case requests <- r.Clone(r.Context()):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// goldenControllerSession is the session the daemon's own golden of GET /legion/v1/controller
// names, read as the daemon wrote it and without the decode under test, or "" where it names none.
func goldenControllerSession(t *testing.T, body []byte) string {
	t.Helper()
	var document map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("read the golden %s: %v", body, err)
	}
	locator, named := document["controllerLocator"]
	if !named {
		return ""
	}
	var session string
	if err := json.Unmarshal(locator["sessionId"], &session); err != nil || session == "" {
		t.Fatalf("the golden's controllerLocator names no sessionId: %s (%v)", body, err)
	}
	return session
}

// TestTheControllerReadNamesTheSessionTheDaemonNames: a controller read of a daemon that answers
// its own goldens of GET /legion/v1/controller (the daemon's internal/api TestControllerRouteGoldens)
// names the session controller.json names, and no session for controller-none.json, the `{}` a
// daemon answers while none is registered. It asks the route with no query, no credential and no
// Legion-Plugin-Contract: the route authenticates nobody, and its answer is the same at every
// contract.
func TestTheControllerReadNamesTheSessionTheDaemonNames(t *testing.T) {
	for _, tc := range []struct {
		golden string
		named  bool
	}{
		{"controller.json", true},
		{"controller-none.json", false},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			body, err := os.ReadFile(daemonAPIFixture(tc.golden))
			if err != nil {
				t.Fatalf("read the daemon's golden: %v", err)
			}
			want := goldenControllerSession(t, body)
			if (want != "") != tc.named {
				t.Fatalf("the golden %s names %q, want a session %v: this test proves nothing", tc.golden, want, tc.named)
			}
			server, asked := controllerServer(t, http.StatusOK, string(body))

			session, named, err := New(server.URL).Controller(t.Context())
			if err != nil || named != tc.named || session != want {
				t.Fatalf("the controller read = %q, %v, %v; want %q, %v and no error", session, named, err, want, tc.named)
			}
			var request *http.Request
			select {
			case request = <-asked:
			default:
				t.Fatal("the daemon's controller route was asked nothing, so this test proves nothing")
			}
			if request.URL.RawQuery != "" {
				t.Errorf("the read's query is %q, want none", request.URL.RawQuery)
			}
			// The daemon's pluginContractHeader (packages/daemon/internal/api/shaping.go).
			for _, name := range []string{"Authorization", "Legion-Plugin-Contract"} {
				if sent := request.Header.Values(name); len(sent) != 0 {
					t.Errorf("the read sends %s %v, want none", name, sent)
				}
			}
		})
	}
}

// TestAControllerReadOfADaemonWithoutTheRouteIsItsOwnFailure: a daemon of a build before
// GET /legion/v1/controller answers it 404, its catch-all's answer, and the read is
// ErrNoControllerRoute, which is not ErrUnavailable: what it calls for is a daemon rollout, not a
// wait for a daemon that is down.
func TestAControllerReadOfADaemonWithoutTheRouteIsItsOwnFailure(t *testing.T) {
	older := stateServer(t, `{"issues":{}}`)

	session, named, err := New(older.URL).Controller(t.Context())
	if !errors.Is(err, ErrNoControllerRoute) || errors.Is(err, ErrUnavailable) || named || session != "" {
		t.Fatalf("a controller read of a daemon without the route = %q, %v, %v; want ErrNoControllerRoute alone", session, named, err)
	}
}

// TestAControllerReadIsHeldToItsOwnBound: a controller read gives up at ControllerTimeout, 2 s,
// though the client's own window is the whole read's 60 s, whether the daemon sends no answer or
// sends its status and then stalls its body; the failure is ErrUnavailable and names the bound.
func TestAControllerReadIsHeldToItsOwnBound(t *testing.T) {
	if ControllerTimeout != 2*time.Second {
		t.Fatalf("ControllerTimeout = %s, want 2s", ControllerTimeout)
	}
	for _, tc := range []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
	}{
		{"a daemon that sends no answer", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
		{"a daemon that stalls its body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"controllerLocator":`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slow := httptest.NewServer(http.HandlerFunc(tc.serve))
			t.Cleanup(slow.Close)

			started := time.Now()
			_, named, err := New(slow.URL).Controller(t.Context())
			waited := time.Since(started)
			if !errors.Is(err, ErrUnavailable) || named || !strings.Contains(err.Error(), "no answer within 2s") {
				t.Fatalf("a controller read of %s: named %v, err %v; want ErrUnavailable naming the 2s bound", tc.name, named, err)
			}
			if waited < ControllerTimeout || waited >= RequestTimeout {
				t.Errorf("the read gave up after %s, want its own %s bound and not the client's %s window", waited, ControllerTimeout, RequestTimeout)
			}
		})
	}
}

// TestAControllerReadOfAnotherAnswerIsUnavailable: a status that is neither 200 nor 404, a body
// over ControllerResponseLimit, 64 KiB, whether it declares its Content-Length or not, and a body
// that is not the route's object are each ErrUnavailable, naming the status, the cap or the decode;
// the body over the cap wraps ErrResponseTooLarge too and is never parsed. A body exactly at the
// cap is read, and a daemon that is down is named without its address.
func TestAControllerReadOfAnotherAnswerIsUnavailable(t *testing.T) {
	if ControllerResponseLimit != 64<<10 {
		t.Fatalf("ControllerResponseLimit = %d, want 64 KiB (%d)", ControllerResponseLimit, 64<<10)
	}
	// padded is an answer naming ses_padded, exactly size bytes long.
	padded := func(size int) string {
		const head, tail = `{"controllerLocator":{"sessionId":"ses_padded"},"pad":"`, `"}`
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	refused, _ := controllerServer(t, http.StatusInternalServerError, `{"error":"controller unavailable"}`)
	_, _, err := New(refused.URL).Controller(t.Context())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500: err = %v, want ErrUnavailable naming the status", err)
	}

	for _, declared := range []bool{true, false} {
		over := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := padded(ControllerResponseLimit + 1)
			if declared {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(over.Close)
		session, named, err := New(over.URL).Controller(t.Context())
		if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) || named || session != "" {
			t.Errorf("a body over the cap (Content-Length declared %v) = %q, %v, %v; want ErrResponseTooLarge and ErrUnavailable", declared, session, named, err)
		} else if capText := fmt.Sprintf("%d-byte cap", ControllerResponseLimit); !strings.Contains(err.Error(), capText) {
			t.Errorf("a body over the cap (Content-Length declared %v): err = %v, want it to name the %s", declared, err, capText)
		}
	}
	atTheCap, _ := controllerServer(t, http.StatusOK, padded(ControllerResponseLimit))
	if session, named, err := New(atTheCap.URL).Controller(t.Context()); err != nil || !named || session != "ses_padded" {
		t.Errorf("a body exactly at the cap = %q, %v, %v; want ses_padded read", session, named, err)
	}

	for _, body := range []string{`{"controllerLocator":`, `null`, `[]`, `{"controllerLocator":{"sessionId":7}}`, `{"controllerLocator":{"runtime":"kubernetes"}}`} {
		malformed, _ := controllerServer(t, http.StatusOK, body)
		session, named, err := New(malformed.URL).Controller(t.Context())
		if !errors.Is(err, ErrUnavailable) || named || session != "" || !strings.Contains(err.Error(), "decode") {
			t.Errorf("the body %s = %q, %v, %v; want ErrUnavailable naming the decode", body, session, named, err)
		}
	}

	address := testport.Refused(t)
	_, _, err = New("http://" + address).Controller(t.Context())
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), address) || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a controller read of a daemon that is down: err = %v, want ErrUnavailable with the transport's reason and no address", err)
	}
}
