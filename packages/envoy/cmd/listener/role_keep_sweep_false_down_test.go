package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A handover of a role's kept lane one of whose probes of the main client reads a false "down" - a
// read a loaded box keeps waiting past the probe's budget, made here by the seam - hands the lane
// over all the same, and logs nothing at ERROR. The false "down" is keyed to the first read made in
// one function's frames (inFrame), not to a count of reads, since a call made under a context
// marked as made under a lock reads the probe too (bus.Client.connForRequest). The cases are each
// read that can be down:
//   - sweep-head: the keep sweep's read of the lane head (sweptLaneHead) gives the role's guard
//     back, waits for the connection with nothing held (bus.Reconnected, which reads nats.go
//     connected at once) and reads the head again under the guard;
//   - sweep-handover: the handover the sweep started (handOverKeptRole, under handOverSweptLane)
//     does the same and runs the handover again;
//   - drain: drainKeptRole's own handover, as before.
//
// Each case wants a second read in the same frames, the read made again after the false "down".
// The controls are each of the two sweep reads giving up on the loss rather than waiting and
// making it again: the head read passes the lane over, and the handover reports the loss as a
// failed handover at ERROR; in both the copy is not forwarded, and the case fails at the test's
// deadline.
func TestAHandoverWhoseProbeReadsAFalseDownHandsTheLaneOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		// frame is the function the false "down" is keyed to.
		frame string
	}{{"sweep-head", "sweptLaneHead"}, {"sweep-handover", "handOverKeptRole"}, {"drain", "handOverKeptRole"}} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			role := "kept-false-down-" + tc.name
			holder := "ses_kept_false_down_" + strings.ReplaceAll(tc.name, "-", "_")
			cfg := harness.config
			forwards := make(chan string, 16)
			cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
				forwards <- item.DedupeKey
				return nil
			})
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-false-down-00")
			if _, err := keepRoleMessage(cfg, role, item); err != nil {
				t.Fatalf("keep the role's copy: %s", harness.logs.String())
			}
			claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
			frame := listenerPackage + "." + tc.frame
			var reads atomic.Int64
			restore := harness.client.ProbeReadsDownForTest(func() bool {
				return inFrame(frame) && reads.Add(1) == 1
			})
			defer restore()
			ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
			defer cancel()
			if tc.name != "drain" {
				sweepOnce(ctx, cfg)
			} else if err := drainKeptRole(ctx, cfg, role, time.Now); err != nil {
				t.Fatalf("the handover answered %v", err)
			}
			select {
			case key := <-forwards:
				if !strings.HasSuffix(key, item.DedupeKey) {
					t.Fatalf("the handover forwarded %q, want the kept copy %q", key, item.DedupeKey)
				}
			case <-time.After(testnats.Remaining(t)):
				t.Fatalf("the %s case forwarded nothing by the test's deadline after the first probe read in %s read a false down (%d reads there); log: %s", tc.name, tc.frame, reads.Load(), harness.logs.String())
			}
			if reads.Load() < 2 {
				t.Fatalf("the probe was read %d times in %s, want a read made again after the false down", reads.Load(), tc.frame)
			}
			if logs := harness.logs.String(); strings.Contains(logs, `"level":"ERROR"`) || strings.Contains(logs, "level=ERROR") {
				t.Fatalf("the %s case logged at ERROR on a false down: %s", tc.name, logs)
			}
		})
	}
}
