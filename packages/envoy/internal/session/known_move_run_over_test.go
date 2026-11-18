package session

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/testwait"
)

// A move whose run is over is no failure to repair in the background: the run is superseded and
// the run the next connection event brings moves the store again, so Rewatch returns the move's
// error and arms no retry. Over is the bus's rule (bus.RunEnded): the context ended, or the
// connection is not CONNECTED, which a move on a connection nats.go has just set RECONNECTING meets
// with its context still live. A move that fails on a live run, against a bucket deleted under the
// store, arms the retry. The listener's hook reads the same rule for its WARN
// (cmd/listener, TestTheKnownSessionHookLogsNothingForARunThatIsOver).
func TestAMoveWhoseRunIsOverArmsNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange puts the store's connection in the case's state and returns the move's context.
		arrange func(t *testing.T, srv *testnats.Server, client *bus.Client) context.Context
		arms    bool
	}{
		{
			name: "the connection is reconnecting and the context is live",
			arrange: func(t *testing.T, srv *testnats.Server, client *bus.Client) context.Context {
				srv.Stop(t)
				if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool { return client.Conn.Status() == nats.RECONNECTING }) {
					t.Fatalf("the connection reads %v 10s after its server stopped, want RECONNECTING", client.Conn.Status())
				}
				return context.Background()
			},
		},
		{
			name: "the context ended",
			arrange: func(*testing.T, *testnats.Server, *bus.Client) context.Context {
				ended, end := context.WithCancel(context.Background())
				end()
				return ended
			},
		},
		{
			name: "the run is live and the bucket is gone",
			arrange: func(t *testing.T, _ *testnats.Server, client *bus.Client) context.Context {
				if err := client.JS().DeleteKeyValue(bus.KnownSessionBucket); err != nil {
					t.Fatalf("delete the known-session bucket: %v", err)
				}
				return context.Background()
			},
			arms: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			client, err := bus.Connect([]string{uri}, bus.WithName(t.Name()))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			// The bus's own connection settings (internal/bus connect): it buffers nothing while it
			// reconnects and reconnects forever, so a move on it while RECONNECTING fails at once
			// rather than waiting out its request's bound, and it never closes.
			if buffer, reconnects := client.Conn.Opts.ReconnectBufSize, client.Conn.Opts.MaxReconnect; buffer != -1 || reconnects != -1 {
				t.Fatalf("the connection has ReconnectBufSize %d and MaxReconnect %d, want -1 and -1 as the bus sets them", buffer, reconnects)
			}
			known, err := OpenKnownSessions(client.Conn,
				WithKnownSessionReplicas(1),
				WithKnownSessionLogger(slog.New(slog.NewTextHandler(&lockedWriter{}, nil))),
				// A retry the move arms waits an hour before its first attempt, so nothing retires
				// it while the test reads it.
				withKnownSessionRetry(time.Hour, time.Hour))
			if err != nil {
				t.Fatalf("open the known-session store: %v", err)
			}
			t.Cleanup(known.StopWatch)
			ready, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelReady()
			if err := known.WaitForCacheReady(ready); err != nil || !known.Opened() {
				t.Fatalf("the store's watcher is not open (WaitForCacheReady %v, Opened %t); the case needs one open", err, known.Opened())
			}
			if repairRunning(known) {
				t.Fatal("a retry runs before the move; the case needs none")
			}

			ctx := tc.arrange(t, srv, client)
			if err := known.Rewatch(ctx, client.Conn); err == nil {
				t.Fatal("the move succeeded; the case needs one that fails")
			}
			if armed := repairRunning(known); armed != tc.arms {
				t.Fatalf("the failed move armed the store's retry: %t, want %t", armed, tc.arms)
			}
		})
	}
}
