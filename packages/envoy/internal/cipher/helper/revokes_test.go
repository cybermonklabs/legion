//go:build linux

package helper

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/testwait"
)

// waitRevokes returns once no revoke goRevoke fired off is in flight: at once when none is, and
// otherwise when the count next falls to zero, read through testwait.Receive so a revoke that never
// returns fails t naming the wait, at testwait's margin before the test binary's deadline. Only
// tests wait for the revokes a path fires off.
func (s *Server) waitRevokes(t *testing.T) {
	t.Helper()
	s.revokesMu.Lock()
	var idle <-chan struct{} = s.revokesIdle
	inFlight := s.revokesInFlight
	s.revokesMu.Unlock()
	if inFlight > 0 {
		testwait.Receive(t, "the revokes the helper fired off to return", idle)
	}
}

// armedTransport passes every request to base until it is armed; from then on it holds each
// DELETE, the revoke's request, until release is closed, and it does not watch the request's
// context. It stands for a transport that does not end with the context the caller gave it, so a
// test can hold a revoke in flight past the helper's stop. The real client ends such a request
// with its context (TestAStopEndsARevokeAnUnresponsiveBrokerHolds).
type armedTransport struct {
	base    http.RoundTripper
	armed   atomic.Bool
	reached chan struct{}
	once    sync.Once
	release chan struct{}
}

func (a *armedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if a.armed.Load() && r.Method == http.MethodDelete {
		a.once.Do(func() { close(a.reached) })
		<-a.release
		return nil, errors.New("the test's transport answers no revoke")
	}
	return a.base.RoundTrip(r)
}

// TestServeWaitsForTheRevokesItFiredOff: a revoke goes on after the answer it belongs to
// (TestUnregisterAnswersBeforeItsRevokeEnds), and Serve waits for it to return. A helper process
// exits when Serve returns, so a revoke still running then is cut off mid-request; in a test
// binary, where nothing exits, it runs on into the tests that follow and reads what they set.
// Here the transport holds the revoke's DELETE and does not watch its context, so the revoke is
// still in flight when the helper is stopped: Serve must not return. It returns once the
// transport lets the request go.
func TestServeWaitsForTheRevokesItFiredOff(t *testing.T) {
	t.Parallel()
	f := newFakeBroker(t)
	held := &armedTransport{base: f.srv.Client().Transport, reached: make(chan struct{}), release: make(chan struct{})}
	var released sync.Once
	letGo := func() { released.Do(func() { close(held.release) }) }
	t.Cleanup(letGo)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := serveBroker(t, &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"),
		MachineFile: machineFilePath(t), HTTP: &http.Client{Transport: held}, Log: log}, "", log)
	r.fake = f
	r.login(t)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}

	held.armed.Store(true)
	if un := r.call(t, Request{Op: "unregister"}); !un.OK {
		t.Fatalf("unregister with its revoke's transport held: %+v", un)
	}
	testwait.Receive(t, "the revoke unregister fired off to reach the transport", held.reached)
	r.cancel()
	select {
	case <-r.served:
		t.Fatal("Serve returned while the revoke it fired off was still in flight; a helper that exits then cuts the revoke off")
	case <-time.After(time.Second):
	}
	letGo()
	select {
	case <-r.served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10 s of the revoke's request being let go")
	}
}

// TestAStopEndsARevokeAnUnresponsiveBrokerHolds: the wait above cannot hang a stop. Every revoke
// runs under Serve's context and sends its DELETE with it, so a broker that takes the request and
// never answers holds the revoke only until that context ends: Serve returns within seconds of
// its own stop, with the broker still holding the request.
func TestAStopEndsARevokeAnUnresponsiveBrokerHolds(t *testing.T) {
	t.Parallel()
	r := startRig(t, "")
	gate, open := testGate(t)
	t.Cleanup(open)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	r.fake.mu.Lock()
	r.fake.revokeGate = gate
	r.fake.mu.Unlock()
	if un := r.call(t, Request{Op: "unregister"}); !un.OK {
		t.Fatalf("unregister with its revoke held: %+v", un)
	}
	reached := func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.revokeAttempts == 1
	}
	waitUpTo(t, 5*time.Second, reached, "unregister answered, but its revoke never reached the broker")

	stopped := time.Now()
	r.cancel()
	select {
	case <-r.served:
	case <-time.After(5 * time.Second):
		t.Fatalf("Serve did not return within 5 s of its context ending, with the broker holding the revoke's request since %s", time.Since(stopped))
	}
}
