package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// steppedClock is a client clock (WithReconnectErrClock) that stands still until the test moves it.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// A run superseded by its connection's loss is no failure of the hooks and is logged at INFO, but
// only while the hooks have been uncovered for less than RepairGrace: a link that drops during every
// run supersedes every run, so the hooks would stay uncovered for good with nothing but INFO lines,
// and the paging alarm reads the stall's ERROR alone. Past the grace a superseded run writes that
// ERROR, as a failed one does. Both callers that report a run are held to it: the run after a
// reconnect (onReconnect) and the recovery's (recoverUntilHealthy). The clock is the client's
// own (WithReconnectErrClock), which the hook moves past the grace while its run is under way.
func TestSupersededRunsPastTheRepairGraceRaiseTheStallError(t *testing.T) {
	t.Parallel()
	const (
		supersededLine = "envoy nats reconnect hooks superseded by a newer connection event"
		warnLine       = "envoy nats reconnect hook failed; it is retried"
		errorLine      = "envoy nats reconnect hooks have not succeeded"
	)
	for _, caller := range []struct {
		name string
		// run starts a run of the hooks the hook below supersedes, on the caller's path.
		run func(t *testing.T, srv *testnats.Server, client *Client)
	}{
		{name: "onReconnect", run: func(t *testing.T, srv *testnats.Server, _ *Client) {
			srv.Stop(t)
			srv.Start(t)
		}},
		{name: "recoverUntilHealthy", run: func(t *testing.T, _ *testnats.Server, client *Client) {
			markHooksDue(client)
			recovered := make(chan struct{})
			go func() {
				client.recover()
				close(recovered)
			}()
			t.Cleanup(func() {
				client.Close()
				<-recovered
			})
		}},
	} {
		for _, grace := range []struct {
			name string
			past bool
		}{
			{name: "before the grace"},
			{name: "past the grace", past: true},
		} {
			t.Run(caller.name+"/"+grace.name, func(t *testing.T) {
				t.Parallel()
				srv, uri := testnats.Start(t)
				clock := &steppedClock{now: time.Now()}
				logs := captureLogs()
				client, err := Connect([]string{uri}, WithName(t.Name()), WithReconnectErrClock(clock.Now), WithLogger(logs.logger))
				if err != nil {
					t.Fatalf("connect: %v", err)
				}
				t.Cleanup(client.Close)
				var calls atomic.Int32
				client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
					if calls.Add(1) > 1 {
						return nil
					}
					if grace.past {
						clock.Advance(RepairGrace)
					}
					return supersede(ctx, t, srv, conn)
				})

				caller.run(t, srv, client)
				// The superseded run ends in one of its report's lines, and the wait takes whichever
				// comes.
				testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "the report of the superseded run", func() bool {
					return calls.Load() >= 1 && logs.Lines(supersededLine)+logs.Lines(warnLine)+logs.Lines(errorLine) != ""
				})
				if calls.Load() != 1 {
					t.Fatalf("the hook ran %d times before the report, want the one superseded run", calls.Load())
				}
				stall, kept := client.HookStall()
				if kept != nil {
					t.Fatalf("a superseded run kept its error for HookStall: %v", kept)
				}
				if grace.past {
					reported := logs.Lines(errorLine)
					if reported == "" || !strings.Contains(reported, `"level":"ERROR"`) || !strings.Contains(reported, errRunSuperseded.Error()) {
						t.Fatalf("a superseded run %s into the stall was not reported as the stall's ERROR; the report:\n%s%s%s",
							stall, logs.Lines(supersededLine), logs.Lines(warnLine), reported)
					}
					if lines := logs.Lines(supersededLine); lines != "" {
						t.Fatalf("a superseded run past the grace was also logged at INFO:\n%s", lines)
					}
				} else {
					if lines := logs.Lines(warnLine) + logs.Lines(errorLine); lines != "" {
						t.Fatalf("a superseded run %s into the stall was reported as a failure:\n%s", stall, lines)
					}
					if logs.Lines(supersededLine) == "" {
						t.Fatal("a superseded run before the grace logged no INFO line")
					}
				}

				srv.Start(t)
				testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "a run that covers the reconnect", func() bool {
					return calls.Load() >= 2 && !client.hooksDue()
				})
			})
		}
	}
}

// runSuperseded counts a run superseded when its context ended or its connection is not
// CONNECTED, whichever it reads first. The context is cancelled by a goroutine of its own
// (whileConnected), and nats.go sets the connection RECONNECTING before that goroutine hears of it,
// so a hook that failed at once on the connection's loss returns while the context still reads
// live: the status is what makes that run superseded rather than failed.
func TestARunWhoseConnectionLeftConnectedIsSuperseded(t *testing.T) {
	t.Parallel()
	srv, uri := testnats.Start(t)
	conn, err := nats.Connect(uri, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	if err := runSuperseded(context.Background(), conn); err != nil {
		t.Fatalf("a live run on a CONNECTED connection reads %v, want nil", err)
	}
	ended, end := context.WithCancel(context.Background())
	end()
	if err := runSuperseded(ended, conn); !errors.Is(err, errRunSuperseded) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a run whose context ended reads %v, want errRunSuperseded with the context's cause", err)
	}

	srv.Stop(t)
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool { return conn.Status() == nats.RECONNECTING }) {
		t.Fatalf("the connection reads %v 10s after its server stopped, want RECONNECTING", conn.Status())
	}
	err = runSuperseded(context.Background(), conn)
	if !errors.Is(err, errRunSuperseded) || !errors.Is(err, errRunConnectionLost) {
		t.Fatalf("a live run on a RECONNECTING connection reads %v, want errRunSuperseded for the lost connection", err)
	}
}
