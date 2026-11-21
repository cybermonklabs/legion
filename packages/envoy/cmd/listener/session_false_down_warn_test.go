package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// A read that asks the main client first (sessionDelivery.mainLost) and finds its connection down
// logs the line a lost read of the same bucket logs, naming the session, so a session left without
// its repair or its filter in an outage still has its line in the log. Each case reads one false
// "down" from the main client's probe at its one read:
//   - repair-record: a consumer repair's read of the session's record (registeredTarget);
//   - filter-row: the filter step's read of the session's interest row (storedFilterStep);
//   - fence-reread: the fence's read of the row again after a write landed (fenceFilterWrite).
//
// Each case also checks that the read answered the loss (bus.ErrConnectionLost), which its caller
// retries on.
//
// The controls each put one site back to returning the loss before its WARN line: that case finds
// no line, and fails.
func TestAProbeDownLogsTheLineALostReadLogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		read func(t *testing.T, rig *sessionConsumerRig, sessionID string) error
	}{
		{"repair-record", "listener could not read the record of a session whose consumer it is repairing",
			func(t *testing.T, rig *sessionConsumerRig, sessionID string) error {
				_, found, err := rig.deps.consumers.registeredTarget(context.Background(), rig.deps, sessionID, 0)
				if found != registeredReadFailed {
					t.Errorf("the repair's target = %v, want registeredReadFailed", found)
				}
				return err
			}},
		{"filter-row", "session consumer filter left unchanged: the session's interests could not be read",
			func(t *testing.T, rig *sessionConsumerRig, sessionID string) error {
				_, outcome, err := rig.deps.consumers.storedFilterStep(context.Background(), rig.deps.registry, sessionID, "the test's write")
				if outcome != filterWriteNotMade {
					t.Errorf("the filter step's outcome = %v, want filterWriteNotMade", outcome)
				}
				return err
			}},
		{"fence-reread", "session consumer filter written and left unjudged: the interest row could not be read again",
			func(_ *testing.T, rig *sessionConsumerRig, sessionID string) error {
				// The write is the test's: it reads nothing, so the fence's read of the row again
				// is the probe's one read.
				return rig.deps.consumers.fenceFilterWrite(context.Background(), rig.deps.consumers.logger, rig.deps.registry, sessionID, "the test's write",
					func() (uint64, filterWriteOutcome, error) { return 1, filterWriteLanded, nil })
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSessionConsumerRig(t)
			sessionID := "ses_false_down_warn_" + strings.ReplaceAll(tc.name, "-", "_")
			var reads atomic.Int64
			restore := rig.harness.client.ProbeReadsDownForTest(func() bool { return reads.Add(1) == 1 })
			defer restore()
			err := tc.read(t, rig, sessionID)
			restore()
			if reads.Load() == 0 {
				t.Fatal("the read never asked the main client's probe, so the row shows nothing")
			}
			if !errors.Is(err, bus.ErrConnectionLost) {
				t.Errorf("the read answered %v, want the lost connection its caller retries on", err)
			}
			logged := 0
			for _, line := range strings.Split(rig.harness.logs.String(), "\n") {
				if strings.Contains(line, tc.line) && strings.Contains(line, sessionID) {
					logged++
				}
			}
			if logged != 1 {
				t.Fatalf("the %s read logged %q for %s %d times on a probe down, want once:\n%s", tc.name, tc.line, sessionID, logged, rig.harness.logs.String())
			}
		})
	}
}
