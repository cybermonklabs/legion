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
	if _, kept := keepRoleMessage(harness.config, role, item); !kept {
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
