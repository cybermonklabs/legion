package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logging"
)

// The tests of how a role lane failure ranks among the self-health probe's other failures. They are
// apart from self_health_role_lane_test.go because they name errRoleLaneInactive and pass the role
// lane to checkSelfHealth, which a tree without the role lane's probe lacks. That file builds on
// such a tree only beside this branch's durable_unbound_test.go, which holds the healthz helpers it
// uses (openHealthzDeps, serveHealthz), and with the checkSelfHealth call in each of the two files
// written for that tree's signature: that is how the probe's own tests run as their negative
// control.

// checkSelfHealth checks the role lane first and reports its failure last. A lost durable is a
// failure the monitor rebuilds, decided from the error the probe returns
// (isUnrecoverableSelfHealthFailure), so with the lane refused and the durable gone the error is
// the durable's. The lane is still sent again: checked after the durable, which returns early, it
// would not be.
func TestTheSelfHealthProbeReportsALostDurableBeforeARefusedRoleLaneAndStillSendsTheLane(t *testing.T) {
	rig := newRoleLaneHealthRig(t, "role-lane-health-order")
	rig.stop()
	rig.revoke(t)
	waitFor(t, "the role lane to read the revocation", func() bool { return !rig.lane.CoreSubOK() })
	from := rig.traceFrom()
	err := checkSelfHealth(rig.caches, rig.lane, func() error { return natsgo.ErrConsumerNotFound })
	if !errors.Is(err, natsgo.ErrConsumerNotFound) || errors.Is(err, errRoleLaneInactive) {
		t.Fatalf("with the durable gone and the role lane refused, the probe returned %v; want the durable's error", err)
	}
	if !isUnrecoverableSelfHealthFailure(err, rig.client, rig.caches) {
		t.Fatalf("the monitor would not rebuild for %v", err)
	}
	if err := rig.lane.Conn.Flush(); err != nil {
		t.Fatalf("flush the role lane: %v", err)
	}
	if sent := rig.sent(from); sent != 2 {
		t.Fatalf("the probe sent the refused role lane's subscription %d times, want once in each of its two queue groups", sent)
	}
}

// A role lane failure is never one the monitor rebuilds, whatever it wraps. A send of the lane's
// subscription that fails because the lane's own connection closed under it wraps
// nats.ErrConnectionClosed, which for the main bus is a failure to rebuild, and the rebuild would
// rebuild the main bus for a fault of the lane's connection. The same error outside the lane is
// still one.
func TestARoleLaneFailureIsNeverOneTheMonitorRebuilds(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	laneClosed := fmt.Errorf("%w: sending it again failed: %w", errRoleLaneInactive, natsgo.ErrConnectionClosed)
	if isUnrecoverableSelfHealthFailure(laneClosed, client, nil) {
		t.Fatalf("the monitor would rebuild for the role lane's %v", laneClosed)
	}
	if !isUnrecoverableSelfHealthFailure(natsgo.ErrConnectionClosed, client, nil) {
		t.Fatal("the monitor would not rebuild for the main bus's closed connection; the check proves nothing")
	}
}

// A refused role lane does not turn faults a rebuild repairs into a restart. The probe right after a
// rebuild checks the lane too, and while the lane is refused it cannot read healthy, so if only a
// healthy read ended the terminal count, three ticks in a row of a fault each rebuild repairs would
// stop the listener, naming the lane. Here every tick loses the durable again and every rebuild
// repairs it, with the lane refused throughout: the monitor must run twenty probes without stopping
// the listener, and must not log `self-health recovered`, since the lane never came back. The second
// row runs the same ticks with a rebuild that repairs nothing, which the monitor must stop for.
func TestARefusedRoleLaneDoesNotTurnFaultsTheRebuildRepairsIntoARestart(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	for _, row := range []struct {
		name        string
		repairs     bool
		wantStopped bool
	}{
		{name: "each rebuild repairs the durable", repairs: true, wantStopped: false},
		{name: "no rebuild repairs the durable", repairs: false, wantStopped: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			var probes atomic.Int64
			var stopped atomic.Bool
			// lost is read and written on the monitor's goroutine alone: the probe and the rebuild
			// both run there.
			lost := false
			tickStarts := true
			probe := func() error {
				probes.Add(1)
				if tickStarts {
					// The durable is lost again at the start of every tick.
					lost = true
				}
				tickStarts = true
				if lost {
					return fmt.Errorf("durable consumer: %w", natsgo.ErrConsumerNotFound)
				}
				return errRoleLaneInactive
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				runSelfHealthMonitor(ctx, logging.NewWithWriter("test", logs),
					probe,
					func(err error) bool { return isUnrecoverableSelfHealthFailure(err, client, nil) },
					client.Connected,
					func() error {
						if row.repairs {
							lost = false
						}
						// The probe after the rebuild belongs to this tick.
						tickStarts = false
						return nil
					},
					func() { stopped.Store(true) },
					roleLaneProbePeriod,
					3,
				)
			}()
			defer func() {
				cancel()
				<-done
			}()
			waitFor(t, "twenty probes, or the monitor stopping the listener", func() bool {
				return probes.Load() >= 20 || stopped.Load()
			})
			if got := stopped.Load(); got != row.wantStopped {
				t.Fatalf("the monitor stopped the listener: %v after %d probes, want %v; it logged:\n%s", got, probes.Load(), row.wantStopped, logs.String())
			}
			if strings.Contains(logs.String(), `"msg":"self-health recovered"`) {
				t.Fatalf("the monitor logged self-health recovered while the role lane stayed refused:\n%s", logs.String())
			}
		})
	}
}
