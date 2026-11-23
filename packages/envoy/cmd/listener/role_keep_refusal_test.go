// A kept role's handover that the listener's grant refuses ends at the refusal (handOverKeptRole),
// beside the handover rows of role_keep_handover_test.go, whose fixtures it reuses.

package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// A walk whose forward the server refuses under the listener's grant ends the handover at once,
// with that refusal carried out to its report, rather than being walked again on the widening
// cadence until the handover's window ends with the role's guard held: the grant refuses the
// forward again however often it is sent. The lane keeps its copy for the role's next claim and
// for the sweep.
func TestAHandoverEndsAtAForwardTheGrantRefuses(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-handover-denied"
		holder = "ses_kept_handover_denied"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-00")
	if _, err := keepRoleMessage(context.Background(), harness.config, role, item); err != nil {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(subject string, _ contracts.Envelope, _ string, _ time.Duration) error {
		forwards.Add(1)
		return fmt.Errorf("bus: %w to %q: nats: permissions violation", bus.ErrPublishDenied, subject)
	})

	// A handover that walked the lane again would make its second walk after its first pause,
	// roleKeepDrainRetryPause, and be ended by this window after several.
	ctx, cancel := context.WithTimeout(context.Background(), 4*roleKeepDrainRetryPause)
	defer cancel()
	started := time.Now()
	_, err := handOverKeptRole(ctx, cfg, role, time.Now)
	elapsed := time.Since(started)
	if !errors.Is(err, bus.ErrPublishDenied) {
		t.Fatalf("the handover whose forward the grant refuses = %v, want the refusal carried out", err)
	}
	if got := forwards.Load(); got != 1 || elapsed >= roleKeepDrainRetryPause {
		t.Fatalf("the handover forwarded %d times and ended after %s, want 1 forward and an end before its first pause of %s",
			got, elapsed.Round(time.Millisecond), roleKeepDrainRetryPause)
	}
}

// A walk the grant refuses ends the handover even on a pass whose resolution of the role's holder
// did not answer, where the handover would otherwise read the role's claim record and, finding it
// names a session, walk the lane again after its pause. Here the holder's session read fails once,
// so the first pass walks with no holder; its copy is past its hour, so the walk writes the role's
// cursor to end it, and the grant refuses that write. A handover that walked again would resolve
// the holder a second time, a second read of the session bucket.
func TestAHandoverEndsAtAWalkTheGrantRefusesWhileItsHolderReadFailed(t *testing.T) {
	const (
		role   = "kept-handover-denied-unresolved"
		holder = "ses_kept_handover_denied_unresolved"
	)
	harness := newListenerDeliveryHarnessOn(t, deniedGrantClient(t, bus.KeptRoleCursorSubject(role)))
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-00")
	if _, err := keepRoleMessage(context.Background(), harness.config, role, item); err != nil {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	claimRoleBeyondTheSessionCache(t, harness, holder, role)
	var reads atomic.Int32
	swapSessionBucket(t, harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
		return sessionReadsCounted{KeyValue: kv, reads: &reads}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pastItsHour := func() time.Time { return time.Now().Add(2 * roleKeepWindow) }
	_, err := handOverKeptRole(ctx, harness.config, role, pastItsHour)
	if !errors.Is(err, bus.ErrPublishDenied) {
		t.Fatalf("the handover whose walk the grant refuses = %v, want the refusal carried out:\n%s", err, harness.logs.String())
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("the handover read the holder's session %d times, want 1: a second resolution is a second pass over a walk the grant refused", got)
	}
}

// sessionReadsCounted is a session bucket whose first read fails, as a read the bus missed does,
// and whose every read is counted.
type sessionReadsCounted struct {
	natsgo.KeyValue
	reads *atomic.Int32
}

func (kv sessionReadsCounted) Get(key string) (natsgo.KeyValueEntry, error) {
	if kv.reads.Add(1) == 1 {
		return nil, errors.New("the session bucket's read did not answer")
	}
	return kv.KeyValue.Get(key)
}
