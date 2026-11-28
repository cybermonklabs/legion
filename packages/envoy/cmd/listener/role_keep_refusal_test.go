// A kept role's handover that the listener's grant refuses ends at the refusal (handOverKeptRole),
// beside the handover rows of role_keep_handover_test.go, whose fixtures it reuses.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A walk whose forward the server refuses under the listener's grant ends the handover at once,
// with that refusal carried out to its report, rather than being walked again on the widening
// cadence until the handover's window ends with the role's guard held: the grant refuses the
// forward again however often it is sent. The lane keeps its copy for the role's next claim and
// for the sweep. The handover's pauses run on its clock, which the test never moves, so a handover
// that walked the lane again would first arm its pause there and wait in it.
func TestAHandoverEndsAtAForwardTheGrantRefuses(t *testing.T) {
	t.Parallel()
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-handover-denied"
		holder = "ses_kept_handover_denied"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-00")
	if _, err := keepRoleMessage(harness.config, role, item); err != nil {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var forwards atomic.Int32
	clk := clocktest.New(time.Now())
	cfg := harness.config
	cfg.clock = clk
	cfg.forwardRole = inlineForward(func(subject string, _ contracts.Envelope, _ string, _ time.Duration) error {
		forwards.Add(1)
		return fmt.Errorf("bus: %w to %q: nats: permissions violation", bus.ErrPublishDenied, subject)
	})

	ctx, cancel := context.WithCancel(testwait.Context(t))
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		_, err := handOverKeptRole(ctx, cfg, role, time.Now)
		ended <- err
	}()
	end := endedOrPaused(t, clk, roleKeepDrainRetryPause, ended)
	if end.paused {
		t.Fatalf("the handover whose forward the grant refuses armed its first pause of %s to walk the lane again after %d forwards (its clock held %v); want it ended at the refusal",
			roleKeepDrainRetryPause, forwards.Load(), end.armed)
	}
	if !errors.Is(end.err, bus.ErrPublishDenied) {
		t.Fatalf("the handover whose forward the grant refuses = %v, want the refusal carried out", end.err)
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the handover forwarded %d times, want 1 forward and an end with no pause before another", got)
	}
}

// A walk the grant refuses ends the handover even on a pass whose resolution of the role's holder
// did not answer, where the handover would otherwise read the role's claim record and, finding it
// names a session, walk the lane again after its pause. Here the holder's session read fails once,
// so the first pass walks with no holder; its copy is past its hour, so the walk writes the role's
// cursor to end it, and the grant refuses that write. A handover that walked again would resolve
// the holder a second time, a second read of the session bucket, after a pause armed on its clock.
//
// The server reports its refusal of the cursor write on the connection and never answers the
// write, so the write learns of it only once its wait for an acknowledgement ends (sendStored).
// That wait runs on a clock the test moves (bus.WithPublishAcknowledgementClock), and the test
// moves it only once the client has recorded the refusal, so what the handover meets is the
// server's refusal and not the end of a wait. The client logs each report of the connection and
// then records it, on the one goroutine nats.go runs the connection's callbacks on, in the order
// the server sent them: the refusal of a marker published after the cursor's refusal was logged
// is logged only once the cursor's refusal is recorded.
func TestAHandoverEndsAtAWalkTheGrantRefusesWhileItsHolderReadFailed(t *testing.T) {
	t.Parallel()
	const (
		role   = "kept-handover-denied-unresolved"
		holder = "ses_kept_handover_denied_unresolved"
		marker = "envoy.test.kept-handover-denied-marker"
	)
	cursorSubject := bus.KeptRoleCursorSubject(role)
	acknowledgements := clocktest.New(time.Now())
	clientLog := &lockedBuffer{}
	harness := newListenerDeliveryHarnessOn(t, deniedGrantClientWith(t, []bus.ConnectOption{
		bus.WithPublishAcknowledgementClock(acknowledgements),
		bus.WithLogger(slog.New(slog.NewTextHandler(clientLog, nil))),
	}, cursorSubject, marker))
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-00")
	if _, err := keepRoleMessage(harness.config, role, item); err != nil {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleBeyondTheSessionCache(t, harness, holder, role)
	// The holder's session read fails once, as a read the bus missed does, and every read is
	// counted, a direct get and a read from the stream's leader alike (faultSessionReads).
	var reads atomic.Int32
	faultSessionReads(t, harness.sessions, func(string) error {
		if reads.Add(1) == 1 {
			return errors.New("the session bucket's read did not answer")
		}
		return nil
	})

	clk := clocktest.New(time.Now())
	cfg := harness.config
	cfg.clock = clk
	ctx, cancel := context.WithCancel(testwait.Context(t))
	defer cancel()
	pastItsHour := func() time.Time { return time.Now().Add(2 * roleKeepWindow) }
	ended := make(chan error, 1)
	go func() {
		_, err := handOverKeptRole(ctx, cfg, role, pastItsHour)
		ended <- err
	}()

	refusalOf := func(subject string) func(string) bool {
		return func(line string) bool {
			return strings.Contains(line, "envoy nats async error") && strings.Contains(line, subject)
		}
	}
	lines := clientLog.Stream()
	testwait.Next(t, "the server's refusal of the walk's cursor write, in the client's log", lines, refusalOf(cursorSubject))
	if err := harness.client.Conn.Publish(marker, nil); err != nil {
		t.Fatalf("publish the marker the grant refuses: %v", err)
	}
	testwait.Next(t, "the server's refusal of the marker, in the client's log", lines, refusalOf(marker))
	armed := acknowledgements.AwaitArmed(t, "the cursor write's wait for its acknowledgement", 1)
	acknowledgements.Set(armed[len(armed)-1])

	end := endedOrPaused(t, clk, roleKeepDrainRetryPause, ended)
	if end.paused {
		t.Fatalf("the handover whose walk the grant refuses armed its first pause of %s to walk the lane again (its clock held %v); want it ended at the refusal:\n%s",
			roleKeepDrainRetryPause, end.armed, harness.logs.String())
	}
	if !errors.Is(end.err, bus.ErrPublishDenied) {
		t.Fatalf("the handover whose walk the grant refuses = %v, want the refusal carried out:\n%s", end.err, harness.logs.String())
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("the handover read the holder's session %d times, want 1: a second resolution is a second pass over a walk the grant refused", got)
	}
}
