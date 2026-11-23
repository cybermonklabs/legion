package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A handover of a role's kept lane made while the hub holds the handshake of the listener's main
// client's reconnect gives the role's guard up within the probe's budget, while the handshake is
// still held: each pass asks the main client first whether its connection is up
// (bus.Client.LostNow), the handover returns the loss, and drainKeptRole waits for the connection
// with the guard let go (bus.Reconnected), so another handover of the role, or the sweep's test of
// it, is not held behind a handshake. Once the hold is released (testnats.HandshakeHold) the
// handover runs again under the guard and ends. The keep sweep's read of a lane head under the
// role's guard (sweptLaneHead) is the row's second case, and gives the guard up the same way.
//
// The row decides by events and reads no clock to pass: the call's probe read under the guard,
// then the row's own take of the guard while the handshake is still held, bounded only by the
// test's deadline.
//
// Two protections guard each case. The main client is asked first whether its connection is up
// (bus.Client.LostNow, before each pass of the handover and before the sweep's head read), and
// every call made under the guard is made under a context marked as made under it
// (bus.WithLockHeld, which handOverKeptRole and sweptLaneHead set), so the call's own wait for its
// connection answers within the probe's budget too (bus.Client.connForRequest). Either one gives
// the guard up, and the row passes with either removed alone. The control removes both: the
// handover's pass and the sweep's head read made without asking the main client first, under
// contexts without the mark. The lane read then waits for the reconnect with the guard held, and
// the case fails at the test's deadline.
func TestAHandoverDuringAHeldMainClientHandshakeGivesTheRolesGuardUp(t *testing.T) {
	for _, start := range []struct {
		name string
		run  func(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) error
	}{
		{"drainKeptRole", func(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) error {
			return drainKeptRole(ctx, cfg, role, time.Now)
		}},
		{"sweptLaneHead", func(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string) error {
			if release, ok := sweptLaneHead(ctx, cfg, role, false); ok {
				release()
			}
			return nil
		}},
	} {
		t.Run(start.name, func(t *testing.T) {
			hold := testnats.StartHandshakeHold(t, testnats.URL(t))
			client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1), bus.WithConnectTimeout(testnats.Remaining(t)))
			if err != nil {
				t.Fatalf("connect the main client through the handshake hold: %v", err)
			}
			t.Cleanup(client.Close)
			t.Cleanup(hold.Release)
			harness := newListenerDeliveryHarnessOn(t, client)
			cfg := harness.config
			const role = "kept-held-handshake"

			select {
			case <-hold.Hold():
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("no reconnect's CONNECT of the main client reached the server through the hold by the test's deadline")
			}
			// The call's entry is an event: the main client's probe read made while the role's
			// guard is held, which the probe's seam sees (it keeps the real read). A read made with
			// the guard free is another caller's and is passed over.
			entered := make(chan struct{})
			var once sync.Once
			restore := client.ProbeReadsDownForTest(func() bool {
				if release, free := cfg.roleKeepDrains.tryHold(role); free {
					release()
				} else {
					once.Do(func() { close(entered) })
				}
				return false
			})
			defer restore()
			ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
			defer cancel()
			ended := make(chan error, 1)
			go func() { ended <- start.run(ctx, cfg, role) }()
			select {
			case <-entered:
			case <-ctx.Done():
				hold.Release()
				t.Fatalf("%s never asked the main client's probe while it held role %s's guard, by the test's deadline: its reads under the guard wait for the handshake", start.name, role)
			}
			// Then the guard is taken, bounded only by the test's deadline: the handshake is held
			// until the row releases the hold, so a take that comes first is the call giving the
			// guard up during the handshake.
			release, taken := cfg.roleKeepDrains.hold(ctx, role)
			if !taken {
				hold.Release()
				t.Fatalf("%s still held role %s's guard at the test's deadline, during the main client's held handshake: it waited for that client's connection with the guard held", start.name, role)
			}
			release()
			select {
			case <-hold.Ended():
				t.Fatalf("%s gave the role's guard up only once the main client's held handshake had ended", start.name)
			default:
			}

			hold.Release()
			select {
			case err := <-ended:
				if err != nil {
					t.Fatalf("%s made again once the main client was back answered %v", start.name, err)
				}
			case <-time.After(testnats.Remaining(t)):
				t.Fatalf("%s had not ended by the test's deadline, though the main client was back", start.name)
			}
		})
	}
}
