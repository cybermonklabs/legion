package githubproxy

// grant_test.go holds the tests of the keeper's grant ownership, its stop and the order of its
// log lines: a grant is owned from the moment the broker names it, every revocation reads that
// one ledger, the stop revokes in two passes that neither a hanging ask nor a stalled log holds
// back, and no line about an ask is written before it lands. The rig, the fake broker and the
// stand-ins they share are in forwarder_test.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// requireEveryGrantRevoked fails t unless every grant the broker made, g1 to g<asks>, was revoked.
func requireEveryGrantRevoked(t *testing.T, b *fakeBroker) {
	t.Helper()
	revoked := b.revokedIDs()
	for n := 1; n <= b.asks(); n++ {
		if id := fmt.Sprintf("g%d", n); !slices.Contains(revoked, id) {
			t.Errorf("grant %s was never revoked; the broker saw revocations %v", id, revoked)
		}
	}
}

// TestStopRevokesTheHeldGrants: when the forwarder stops, at the end of the role's generation, it
// revokes the grant it holds.
func TestStopRevokesTheHeldGrants(t *testing.T) {
	r := newRig(t, nil)
	grant := r.awaitGrant()
	r.shutdown()
	if revoked := r.broker.revokedIDs(); !slices.Contains(revoked, grant.ID) {
		t.Fatalf("the broker saw revocations %v; want %s revoked at the stop", revoked, grant.ID)
	}
}

// TestAFailedRevokeAtTheStopIsTriedAgain: with no ask in flight, the stop's first revocation of
// the grant it holds fails once; the stop runs its second pass for that grant and revokes it.
//
// The named revert: run the second pass only where an ask was in flight (as at ac00d281), and a
// revocation that failed once at the stop is never tried again.
func TestAFailedRevokeAtTheStopIsTriedAgain(t *testing.T) {
	r := newRig(t, func(_ *Config, b *fakeBroker) { b.revokeFailures = 1 })
	r.awaitGrant()
	r.shutdown()
	if attempts := r.broker.revokeAttempts(); attempts < 2 {
		t.Errorf("the stop tried %d revocations; want the failed one tried again", attempts)
	}
	requireEveryGrantRevoked(t, r.broker)
}

// TestARetryAtTheStopWithNoAskInFlightHasTheLandedWaitToo: with no ask in flight, the stop's
// first revocation of the grant it holds fails at once, and the broker takes revokeBound plus half
// of landedBound to end it on the retry. The second pass, which waited for no ask, has
// landedBound beside its revokeBound, so the retry ends the grant within stopBound.
//
// The named revert: give the second pass revokeBound alone whether or not it waited for an ask
// (N12 of the final review's round 7), and the retry's time limit ends before the broker answers.
func TestARetryAtTheStopWithNoAskInFlightHasTheLandedWaitToo(t *testing.T) {
	r := newRig(t, func(_ *Config, b *fakeBroker) { b.revokeFailures, b.retryWait = 1, revokeBound+landedBound/2 })
	r.awaitGrant()
	r.shutdown()
	if attempts := r.broker.revokeAttempts(); attempts != 2 {
		t.Errorf("the stop tried %d revocations; want the failed one and one retry", attempts)
	}
	requireEveryGrantRevoked(t, r.broker)
}

// stallingWriter is a log sink whose writes of a line holding match block until free is called,
// as a role's stderr does when nothing drains it; stalled is closed when the first such write
// begins. Every other line is kept.
type stallingWriter struct {
	match   string
	stalled chan struct{}
	release chan struct{}
	once    sync.Once
	freed   sync.Once
	lockedBuffer
}

func newStallingWriter(match string) *stallingWriter {
	return &stallingWriter{match: match, stalled: make(chan struct{}), release: make(chan struct{})}
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.once.Do(func() { close(w.stalled) })
		<-w.release
	}
	return w.lockedBuffer.Write(p)
}

// free lets every stalled write through, once.
func (w *stallingWriter) free() { w.freed.Do(func() { close(w.release) }) }

// TestAStalledLogDoesNotHoldBackTheRevokeAtTheStop: when the forwarder's log stalls on the line
// about a grant it just got (its stderr not drained), a stop still revokes that grant at once,
// well inside the stop bound: the grant is held before any line about it is written, and the
// revocation waits for the grant to be held, not for the line about it.
//
// The named reverts: put the grant in held only when the ask is published, after its log lines
// (as at d377b67), and the grant is never revoked while the log stalls; have the stop wait for
// the ask to end, and the revocation comes only when the stop bound runs out.
func TestAStalledLogDoesNotHoldBackTheRevokeAtTheStop(t *testing.T) {
	logs := newStallingWriter("holds a grant")
	r := newRig(t, func(cfg *Config, _ *fakeBroker) { cfg.Log = slog.New(slog.NewTextHandler(logs, nil)) })
	t.Cleanup(logs.free) // runs before the rig's own shutdown, which waits for the stop to end
	select {
	case <-logs.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder wrote no line about a grant")
	}
	r.stop()
	deadline := time.Now().Add(stopBound / 2)
	for time.Now().Before(deadline) && !slices.Contains(r.broker.revokedIDs(), "g1") {
		time.Sleep(20 * time.Millisecond)
	}
	if revoked := r.broker.revokedIDs(); !slices.Contains(revoked, "g1") {
		t.Fatalf("the broker saw revocations %v %s into a stop while the log stalled; want g1 revoked at once", revoked, stopBound/2)
	}
	logs.free()
	select {
	case <-r.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarder did not stop once its log drained")
	}
}

// TestAnAskInFlightAtTheStopDoesNotKeepTheHeldGrantsLive: g1 lands and is live for an hour, then
// the renewal ask hangs, as a slow broker or a request it holds pending makes it, and the forwarder
// stops. g1 is revoked at once: the stop revokes the grants it holds before it waits for the ask in
// flight, each with a time limit of its own, so the wait cannot use up the time the revocation
// needs.
//
// The named revert: wait for the ask in flight first and revoke afterwards under the same limit
// (as at aa1a419f), and the revocation runs on a context that has ended and revokes nothing.
func TestAnAskInFlightAtTheStopDoesNotKeepTheHeldGrantsLive(t *testing.T) {
	r := newRig(t, func(cfg *Config, b *fakeBroker) {
		b.hangFrom, b.entered = 2, make(chan struct{}, 1)
		cfg.RenewBefore = time.Hour - 300*time.Millisecond
	})
	if g := r.awaitGrant(); g.ID != "g1" {
		t.Fatalf("the first grant is %s, want g1", g.ID)
	}
	select {
	case <-r.broker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder made no renewal ask")
	}
	r.stop()
	deadline := time.Now().Add(landedBound / 2)
	for time.Now().Before(deadline) && !slices.Contains(r.broker.revokedIDs(), "g1") {
		time.Sleep(20 * time.Millisecond)
	}
	if revoked := r.broker.revokedIDs(); !slices.Contains(revoked, "g1") {
		t.Fatalf("the broker saw revocations %v %s into a stop with the renewal ask hanging; want g1, held and live for an hour, revoked at once, before the wait for the ask", revoked, landedBound/2)
	}
	select {
	case <-r.stopped:
	case <-time.After(stopBound + 5*time.Second):
		t.Fatal("the forwarder did not stop")
	}
}

// TestAGrantWhoseReleaseFailsIsStillRevoked: the broker made the grant, then its release fails.
// The forwarder owns the grant from the moment the broker named it, before its release is asked
// for, so the grant is revoked, and every grant the broker made is revoked by the stop at the
// latest.
//
// The named revert: put the grant in held only once its release and bundle succeeded (as at
// aa1a419f), and a grant whose release fails is never revoked.
func TestAGrantWhoseReleaseFailsIsStillRevoked(t *testing.T) {
	r := newRig(t, func(_ *Config, b *fakeBroker) { b.valuesErr = errors.New("the broker's database is down") })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !slices.Contains(r.broker.revokedIDs(), "g1") {
		time.Sleep(20 * time.Millisecond)
	}
	r.shutdown()
	if r.broker.asks() == 0 {
		t.Fatal("the forwarder made no ask")
	}
	requireEveryGrantRevoked(t, r.broker)
}

// TestARefusedGrantWhoseRevokeFailsIsRevokedAtTheStop: the forwarder refuses a release (a name
// released as its value) and its revocation of that grant fails once. The grant stays in the one
// ledger every revocation reads, so the stop revokes it.
//
// The named revert: revoke a refused grant once and drop it whatever the broker answered (as at
// aa1a419f), and the stop never revokes it.
func TestARefusedGrantWhoseRevokeFailsIsRevokedAtTheStop(t *testing.T) {
	r := newRig(t, func(_ *Config, b *fakeBroker) { b.asValue, b.revokeFailures = true, 1 })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && r.broker.revokeAttempts() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if r.broker.revokeAttempts() == 0 {
		t.Fatal("the forwarder tried no revocation of the grant it refused")
	}
	r.shutdown()
	requireEveryGrantRevoked(t, r.broker)
}

// TestNoLineAboutAnAskIsWrittenBeforeItLands: every line an ask writes, whatever its outcome,
// comes after the ask has landed (its grant, if any, in held), so a log that stalls on such a line
// holds back no revocation at the stop. Three outcomes write a line: a grant it holds, a release it
// refuses and cannot revoke, and a grant that holds no account for GH_CONFIG_DIR's directory.
//
// The named revert: write the refused grant's line in ask, or the missing default's in write
// (as at aa1a419f), and that row's ask has not landed while its line stalls.
func TestNoLineAboutAnAskIsWrittenBeforeItLands(t *testing.T) {
	for _, tc := range []struct {
		name, stallOn, defaultOwner string
		asValue                     bool
		revokeErr                   error
	}{
		{name: "a grant it holds", stallOn: "holds a grant", defaultOwner: "acct-a"},
		{name: "a release it refuses and cannot revoke", stallOn: "the grant it refused", defaultOwner: "acct-a",
			asValue: true, revokeErr: errors.New("the broker is down")},
		{name: "a grant with no account for the default", stallOn: "GH_CONFIG_DIR's directory", defaultOwner: "acct-z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := []string{"api.github.com", "github.com"}
			broker := &fakeBroker{proxy: newBrokerProxy(t, echoServer(t, "bound").Addr().String()), lifetime: time.Hour,
				hosts: hosts, asValue: tc.asValue, revokeErr: tc.revokeErr}
			logs := newStallingWriter(tc.stallOn)
			t.Cleanup(logs.free)
			k := newKeeper(Config{Secrets: []string{"ROLE_APP"}, Reason: "the implementer of ACME-1", GitHubHosts: hosts,
				GHDir: filepath.Join(t.TempDir(), "gh"), DefaultOwner: tc.defaultOwner, RenewBefore: RenewBefore,
				Open: func() (Broker, error) { return broker, nil }}, slog.New(slog.NewTextHandler(logs, nil)))
			a := &ask{landed: make(chan struct{}), done: make(chan struct{})}
			k.asking = a
			go k.run(a, "the test")
			select {
			case <-logs.stalled:
			case <-time.After(5 * time.Second):
				t.Fatalf("the ask wrote no line holding %q", tc.stallOn)
			}
			select {
			case <-a.landed:
			case <-time.After(time.Second):
				t.Fatalf("the ask has not landed while its line holding %q stalls", tc.stallOn)
			}
			logs.free()
			select {
			case <-a.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the ask did not end once its log drained")
			}
		})
	}
}

// TestAGrantInFlightAtTheStopIsRevokedWhenItLands: an ask the broker answers only after the
// forwarder's stop began still lands a grant, and the stop waits for it, within its bound, and
// revokes it.
//
// The named revert: have revokeHeld read the grants it holds without waiting for the ask in
// flight, and the grant that lands after it is never revoked.
func TestAGrantInFlightAtTheStopIsRevokedWhenItLands(t *testing.T) {
	gate := make(chan struct{})
	r := newRig(t, func(_ *Config, b *fakeBroker) {
		b.gate, b.entered = gate, make(chan struct{}, 1)
	})
	select {
	case <-r.broker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder made no ask")
	}
	r.stop()
	time.Sleep(200 * time.Millisecond)
	close(gate)
	select {
	case <-r.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarder did not stop")
	}
	if revoked := r.broker.revokedIDs(); !slices.Equal(revoked, []string{"g1"}) {
		t.Fatalf("the broker saw revocations %v; want g1, the grant that landed during the stop", revoked)
	}
}

// TestAListenerThatFailsStillRevokesTheGrants: when the listener fails for good, Serve answers the
// failure and still revokes the grants it holds first.
//
// The named revert: have Serve return on a failed Accept before revokeHeld, and the grant stays
// live.
func TestAListenerThatFailsStillRevokesTheGrants(t *testing.T) {
	broker := &fakeBroker{proxy: newBrokerProxy(t, echoServer(t, "bound").Addr().String()), lifetime: time.Hour, hosts: []string{"api.github.com"}}
	fwd, err := Listen(Config{Role: "implementer", Listen: "127.0.0.1:0", Secrets: []string{"ROLE_APP"}, Reason: "the implementer of ACME-1",
		GitHubHosts: []string{"api.github.com"},
		GHDir:       filepath.Join(t.TempDir(), "gh"), Open: func() (Broker, error) { return broker, nil },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fwd.Serve(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fwd.Current() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if fwd.Current() == nil {
		t.Fatal("the forwarder holds no grant")
	}
	_ = fwd.listener.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve answered nil for a listener that failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its listener failed")
	}
	if revoked := broker.revokedIDs(); !slices.Equal(revoked, []string{"g1"}) {
		t.Fatalf("the broker saw revocations %v; want g1", revoked)
	}
}
