package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/sessiontiming"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestTheIdleRigRoleProcessRegistersThroughABusyListener: the rig's role process registers with
// the Envoy listener whatever answer the listener's load makes it give, as a plugin does, on both
// of its registration calls (subscribe and roles/set). A call the listener did not answer, or
// answered 5xx, is made again: after the answer's Retry-After, or after sessiontiming's
// CallRetryDelay when the answer names none. An answer that comes later than any deadline the
// process's client could keep is one the process waits for, because its client keeps none. A 4xx
// is a refusal, and the process does not call again. The process used to end at the first failure
// of either call, and its pod was never ready: on a machine at load5 1,100 to 2,000 the listener
// answered POST /v1/interests/subscribe after the 20 s the client gave it.
//
// The least gap between two calls is read from the listener's own record of when each call
// arrived: it is the process's pacing, which the rows hold to the pause the answer asked for, and
// no bound of the test's.
func TestTheIdleRigRoleProcessRegistersThroughABusyListener(t *testing.T) {
	subscribe := func(p *idleRigRoleProcess) error { return p.subscribe(nil) }
	claimRole := func(p *idleRigRoleProcess) error { return p.claimRole() }
	const (
		subscribePath = "/v1/interests/subscribe"
		rolesPath     = "/v1/roles/set"
	)
	held := newIdleRigHeldAnswer()
	for _, row := range []struct {
		name string
		call func(*idleRigRoleProcess) error
		path string
		// first answers the listener's first call; every later call lands.
		first func(w http.ResponseWriter, r *http.Request, p *idleRigRoleProcess)
		// held, when set, is the first answer, held until the test releases it.
		held *idleRigHeldAnswer
		// calls is how many calls the listener takes, and gap the least time between the first
		// and the second when there are two.
		calls int
		gap   time.Duration
		// refused says the call ends with the first answer's error.
		refused bool
	}{
		{name: "subscribe, a 503 with Retry-After", call: subscribe, path: subscribePath,
			first: idleRigBusyListenerAnswers(http.StatusServiceUnavailable, "1"), calls: 2, gap: time.Second},
		{name: "subscribe, a 502 with no Retry-After", call: subscribe, path: subscribePath,
			first: idleRigBusyListenerAnswers(http.StatusBadGateway, ""), calls: 2, gap: sessiontiming.CallRetryDelay},
		{name: "subscribe, no answer at all", call: subscribe, path: subscribePath,
			first: idleRigBusyListenerHangsUp, calls: 2, gap: sessiontiming.CallRetryDelay},
		{name: "subscribe, an answer held past the client's own deadline", call: subscribe, path: subscribePath,
			first: held.answer, held: held, calls: 1},
		{name: "subscribe, a 409 refusal", call: subscribe, path: subscribePath,
			first: idleRigBusyListenerAnswers(http.StatusConflict, ""), calls: 1, refused: true},
		{name: "roles set, a 503 with Retry-After", call: claimRole, path: rolesPath,
			first: idleRigBusyListenerAnswers(http.StatusServiceUnavailable, "1"), calls: 2, gap: time.Second},
		{name: "roles set, a 400 refusal", call: claimRole, path: rolesPath,
			first: idleRigBusyListenerAnswers(http.StatusBadRequest, ""), calls: 1, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			listener := &idleRigBusyListener{path: row.path, first: row.first}
			p := listener.start(t)
			returned := make(chan error, 1)
			go func() { returned <- row.call(p) }()
			if row.held != nil {
				// The listener's server waits for the held call when it closes, so a row that
				// ends early releases the answer first: a cleanup runs before those registered
				// ahead of it.
				t.Cleanup(row.held.release)
				testwait.Eventually(t, "the held call of "+row.path+" to reach the listener", func() bool {
					select {
					case <-row.held.arrived:
						return true
					case err := <-returned:
						t.Fatalf("%s returned before the listener took the call: %v", row.path, err)
					default:
					}
					return false
				})
				// The answer is held for as long as the test holds it, and whether the call
				// outlasts that is settled by what could end it: a deadline the client keeps.
				idleRigClientKeepsNoDeadline(t, p.client)
				row.held.release()
			}
			err := testwait.Receive(t, "the call of "+row.path+" to return", returned)
			switch {
			case row.refused && err == nil:
				t.Fatalf("%s answered a refusal first, and the call reported no error; want the refusal", row.path)
			case !row.refused && err != nil:
				t.Fatalf("%s: %v, want the call landed on the listener's answer after the first", row.path, err)
			}
			calls := listener.callTimes()
			if len(calls) != row.calls {
				t.Fatalf("the listener took %d calls of %s, want %d", len(calls), row.path, row.calls)
			}
			if row.calls == 2 {
				if gap := calls[1].Sub(calls[0]); gap < row.gap {
					t.Errorf("the second call came %s after the first, want at least %s", gap, row.gap)
				}
			}
		})
	}
}

// idleRigClientKeepsNoDeadline fails t when client keeps a deadline of its own that would end a
// call the listener has not answered yet: its Timeout, or its transport's wait for the answer's
// headers (http.DefaultTransport when it names none).
func idleRigClientKeepsNoDeadline(t *testing.T, client *http.Client) {
	t.Helper()
	if client.Timeout != 0 {
		t.Errorf("the role process's client gives each call %s, want no deadline of its own", client.Timeout)
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	switch known, ok := transport.(*http.Transport); {
	case !ok:
		t.Errorf("the role process's client calls through a %T, whose deadlines this row cannot read", transport)
	case known.ResponseHeaderTimeout != 0:
		t.Errorf("the role process's client waits %s for an answer's headers, want no deadline of its own", known.ResponseHeaderTimeout)
	}
}

// idleRigBusyListener is an Envoy listener's registration route path that answers its first call
// with first and every later one as a listener that took the call.
type idleRigBusyListener struct {
	path  string
	first func(w http.ResponseWriter, r *http.Request, p *idleRigRoleProcess)

	process *idleRigRoleProcess
	mu      sync.Mutex
	calls   []time.Time
}

// start serves the listener until t ends and answers a role process whose ENVOY_URL names it,
// built as the role process builds itself, with an environment and an argv of its pod's shape.
func (l *idleRigBusyListener) start(t *testing.T) *idleRigRoleProcess {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != l.path {
			http.NotFound(w, r)
			return
		}
		l.mu.Lock()
		l.calls = append(l.calls, time.Now())
		first, p := len(l.calls) == 1, l.process
		l.mu.Unlock()
		if first {
			l.first(w, r, p)
			return
		}
		idleRigBusyListenerLands(w, r, p)
	}))
	t.Cleanup(server.Close)
	for name, value := range map[string]string{
		"LEGION_SANDBOX": "pod-1", "LEGION_ISSUE": "", "LEGION_ROLE": "implementer", "LEGION_GENERATION": "1",
		"LEGION_DAEMON_URL": "http://127.0.0.1:1", "ENVOY_URL": server.URL, "ENVOY_NATS_URL": "nats://127.0.0.1:1",
	} {
		t.Setenv(name, value)
	}
	p, err := newIdleRigRoleProcess("http://127.0.0.1:1", []string{"legion", "--connect", "tcp://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.process = p
	l.mu.Unlock()
	return p
}

// callTimes is when each call the listener took arrived, in the order they did.
func (l *idleRigBusyListener) callTimes() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.calls...)
}

// idleRigBusyListenerAnswers is a first answer of status, with Retry-After when retryAfter is set.
func idleRigBusyListenerAnswers(status int, retryAfter string) func(http.ResponseWriter, *http.Request, *idleRigRoleProcess) {
	return func(w http.ResponseWriter, _ *http.Request, _ *idleRigRoleProcess) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		http.Error(w, fmt.Sprintf(`{"error":"the listener answered %d"}`, status), status)
	}
}

// idleRigBusyListenerHangsUp closes the call's connection with no answer at all, as a listener
// that went away mid-call does: the client reads the end of the connection, not a status.
func idleRigBusyListenerHangsUp(w http.ResponseWriter, _ *http.Request, _ *idleRigRoleProcess) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("the test server's ResponseWriter takes no hijack")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// idleRigHeldAnswer is a first answer the listener holds until the test releases it: it reports
// the call's arrival by closing arrived, and lands once release is called, which may be called
// more than once.
type idleRigHeldAnswer struct {
	arrived chan struct{}
	gate    chan struct{}
	release func()
}

func newIdleRigHeldAnswer() *idleRigHeldAnswer {
	h := &idleRigHeldAnswer{arrived: make(chan struct{}), gate: make(chan struct{})}
	h.release = sync.OnceFunc(func() { close(h.gate) })
	return h
}

func (h *idleRigHeldAnswer) answer(w http.ResponseWriter, r *http.Request, p *idleRigRoleProcess) {
	close(h.arrived)
	<-h.gate
	idleRigBusyListenerLands(w, r, p)
}

// idleRigBusyListenerLands is the listener's answer to a call it took: for a registration, the
// session's own consumer, relayed on its deliver subject; for a role claim, an empty 200.
func idleRigBusyListenerLands(w http.ResponseWriter, r *http.Request, p *idleRigRoleProcess) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/v1/interests/subscribe" {
		fmt.Fprint(w, `{}`)
		return
	}
	fmt.Fprintf(w, `{"delivery":{"mode":%q,"subject":%q}}`, idleRigDeliveryConsumer, idleRigDeliverSubjectPrefix+p.session)
}
