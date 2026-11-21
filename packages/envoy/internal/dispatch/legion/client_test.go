package legion

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

// TestABodyOverTheCapIsAFailedRead: a daemon answering more than ResponseLimit is a failed read,
// never a parse of the first 64 MiB, and the failure names the answer's size and the cap. A
// Content-Length over the cap is refused before the body is read. A body with no Content-Length
// is read to the cap and the rest is counted, so the size is the whole body's and not the point
// the read stopped. A body exactly at the cap is read, so the bound refuses only what is over it.
func TestABodyOverTheCapIsAFailedRead(t *testing.T) {
	for _, over := range []struct {
		name     string
		size     int
		declared bool
	}{
		{"one byte over the cap, chunked", ResponseLimit + 1, false},
		{"with its Content-Length", 70_000_000, true},
		{"chunked", 70_000_000, false},
	} {
		t.Run(over.name, func(t *testing.T) {
			server, written := streamedState(t, over.size, over.declared)

			_, bytes, err := New(server.URL).State(t.Context())
			if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
			}
			if want := fmt.Sprintf("the answer of %d bytes is over the %d-byte cap", over.size, ResponseLimit); !strings.Contains(err.Error(), want) {
				t.Errorf("the failure reads %q, want it to name the size and the cap: %q", err, want)
			}
			if bytes != over.size {
				t.Errorf("bytes = %d, want the answer's size (%d)", bytes, over.size)
			}
			if !over.declared {
				return
			}
			select {
			case sent := <-written:
				if sent >= ResponseLimit {
					t.Errorf("the daemon wrote %d bytes before the client hung up, want fewer than the cap: a Content-Length over it is refused before the body is read", sent)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the daemon was still writing 30s after the read ended: the client did not hang up")
			}
		})
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

// TestABodyCutShortPastTheCapIsAtLeastWhatWasCounted: a body over the cap whose count ends on a
// failed read, here a daemon that drops the connection in the middle of a chunked body, is a
// failed read over the cap all the same, and its size is at least the bytes counted.
func TestABodyCutShortPastTheCapIsAtLeastWhatWasCounted(t *testing.T) {
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
	if want := fmt.Sprintf("the answer of at least %d bytes is over the %d-byte cap", sent, ResponseLimit); !strings.Contains(err.Error(), want) {
		t.Errorf("the failure reads %q, want %q", err, want)
	}
	if bytes != sent {
		t.Errorf("bytes = %d, want the %d counted", bytes, sent)
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
