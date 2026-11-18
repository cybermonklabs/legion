package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The known-session store's reconnect hook answers nil whatever its move did, and logs a WARN for
// a move that failed while its run was live. A move whose run is over is no failure, by the rule
// the bus decides a superseded run by (bus.RunEnded): its context ended, or its connection is not
// CONNECTED. The second half is the one a context alone misses: nats.go sets the connection
// RECONNECTING before the run's context hears of it, and a request made in between fails at once
// (the bus buffers nothing while it reconnects), so the move fails with the context still live.
// The bus reads that run as superseded, and the hook must not WARN about it either. A move that
// fails on a live run, against a bucket deleted under the store, is still the hook's WARN.
func TestTheKnownSessionHookLogsNothingForARunThatIsOver(t *testing.T) {
	const moveFailed = "the known-session cache did not move to the recovered connection"
	for _, tc := range []struct {
		name string
		// arrange puts the store's connection in the case's state and returns the hook's context.
		arrange func(t *testing.T, srv *testnats.Server, client *bus.Client) context.Context
		warns   bool
	}{
		{
			name: "the connection is reconnecting and the context is live",
			arrange: func(t *testing.T, srv *testnats.Server, client *bus.Client) context.Context {
				srv.Stop(t)
				testwait.Until(t, "the connection to read RECONNECTING once its server stopped", func() (nats.Status, bool) {
					status := client.Conn.Status()
					return status, status == nats.RECONNECTING
				})
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
			warns: true,
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
			known, err := session.OpenKnownSessions(client.Conn, session.WithKnownSessionReplicas(1))
			if err != nil {
				t.Fatalf("open the known-session store: %v", err)
			}
			t.Cleanup(known.StopWatch)
			if err := known.WaitForCacheReady(testwait.Context(t)); err != nil || !known.Opened() {
				t.Fatalf("the store's watcher is not open (WaitForCacheReady %v, Opened %t); the case needs one open", err, known.Opened())
			}
			lines := &cmdtest.LockedBuffer{}
			logger := logging.NewWithWriter("test", lines)

			ctx := tc.arrange(t, srv, client)
			if err := knownSessionMoveHook(known, logger)(ctx, client.Conn); err != nil {
				t.Fatalf("the hook answered %v, want nil whatever its move did", err)
			}
			warned := strings.Contains(lines.String(), moveFailed)
			if warned != tc.warns {
				t.Fatalf("the hook's WARN %q logged: %t, want %t; the hook's log:\n%s", moveFailed, warned, tc.warns, lines.String())
			}
			// The move the hook made failed: one made again under the same context and connection
			// does. Without that the missing WARN would prove nothing.
			if err := known.Rewatch(ctx, client.Conn); err == nil {
				t.Fatal("a move under the case's context and connection succeeded; the case needs one that fails")
			}
		})
	}
}
