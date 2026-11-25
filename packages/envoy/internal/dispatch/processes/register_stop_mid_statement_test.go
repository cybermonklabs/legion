package processes

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Register's wait ends with its context's error when the context ends while one of the wait's
// statements is being written, not only between two tries. pgx's context watcher then sets a
// deadline on the socket and the write fails with a timeout; cmd/dispatch tells a SIGTERM during
// the wait from a failure by the context's error, so a write error that wraps no context error
// turns a stop into a logged failure and exit 1. That happened in #2733's CI run 43596122521
// (TestRegistersWaitEndsWithItsContext, "write failed: … i/o timeout") on pgx 5.11, which
// returned a failed write's error as it was; pgx 5.12 reports it as the context's error, and
// this row holds that for the registry's statements. The row forces the moment: the registry
// lock statement's write is held until the context has ended and the watcher has set its
// deadline, and only then goes to the socket. TestRegistersWaitEndsWithItsContext ends the wait
// between two tries; this row ends it during one.
//
// Register opens the registry's pool from its caller's pool's configuration, so the row hands it
// a pool whose configuration dials through the hold; that pool itself never connects.
func TestRegistersWaitEndsWithItsContextDuringAStatementsWrite(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	config := database.Pool.Config()
	config.MinConns, config.MinIdleConns = 0, 0
	// The hold reads the statement's text on the socket, so the connection is plaintext whatever
	// the test database's URL asks for: what this row holds is the driver's error, not TLS.
	config.ConnConfig.TLSConfig, config.ConnConfig.Fallbacks = nil, nil
	hold := &writeHold{marker: []byte("pg_advisory_xact_lock"), writing: make(chan struct{}), deadline: make(chan struct{})}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &heldConn{Conn: conn, hold: hold}, nil
	}
	carrier, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the pool whose configuration Register copies: %v", err)
	}
	t.Cleanup(carrier.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-hold.writing:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err = Register(ctx, store.NewPool(carrier), Instance{Commit: "c2", SigningKeys: fingerprints(t, "K0")})
	if !hold.held.Load() {
		t.Fatalf("the registry lock statement's write was never held (Register returned %v): the row forced nothing", err)
	}
	if !hold.deadlineSet.Load() {
		t.Fatalf("the context ended during the held write but no deadline reached the socket within %s, so the write was not failed by the context's end (Register returned %v)", writeHoldBound, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Register, its context ended during a statement's write, returned %v; want an error wrapping the context's error, which is how cmd/dispatch tells a stop from a failure", err)
	}
	if got := liveIDs(t, database); len(got) != 0 {
		t.Errorf("live = %v after a wait stopped mid-statement, want no process registered", got)
	}
}

// writeHoldBound is how long a held write waits for the context watcher's deadline: a give-up
// bound for a goroutine hand-over (the watcher's), never a measurement. A watcher that never sets
// one fails the row by name rather than hanging it.
const writeHoldBound = 30 * time.Second

// writeHold holds the first write whose bytes hold marker until a deadline is set on the
// connection after the hold began, and then lets it go to the socket.
type writeHold struct {
	marker      []byte
	once        sync.Once
	held        atomic.Bool
	deadlineSet atomic.Bool
	writing     chan struct{} // closed as the held write begins
	deadline    chan struct{} // closed by the first non-zero deadline set once the hold began
	closeOnce   sync.Once
}

// heldConn is a connection whose writes pass through hold.
type heldConn struct {
	net.Conn
	hold *writeHold
}

func (c *heldConn) Write(b []byte) (int, error) {
	holding := false
	if bytes.Contains(b, c.hold.marker) {
		c.hold.once.Do(func() { holding = true })
	}
	if holding {
		c.hold.held.Store(true)
		close(c.hold.writing)
		select {
		case <-c.hold.deadline:
		case <-time.After(writeHoldBound):
		}
	}
	return c.Conn.Write(b)
}

func (c *heldConn) SetDeadline(at time.Time) error {
	err := c.Conn.SetDeadline(at)
	if c.hold.held.Load() && !at.IsZero() {
		c.hold.closeOnce.Do(func() {
			c.hold.deadlineSet.Store(true)
			close(c.hold.deadline)
		})
	}
	return err
}
