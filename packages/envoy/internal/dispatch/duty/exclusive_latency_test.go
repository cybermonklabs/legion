package duty_test

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// slowWrites delays the write carrying the period read while slow is set.
type slowWrites struct {
	net.Conn
	slow    *atomic.Bool
	delay   time.Duration
	stalled *atomic.Int32
}

func (c slowWrites) Write(b []byte) (int, error) {
	if c.slow.Load() && bytes.Contains(b, []byte(duty.ReadPeriod)) {
		c.stalled.Add(1)
		time.Sleep(c.delay)
	}
	return c.Conn.Write(b)
}

// Two ticks due one period apart must both run even if the first period read stalls. The first
// tick is due 50 ms before the current database hour. Only the write carrying the period read
// stalls for 100 ms, after the gate would measure the time since due and before PostgreSQL reads
// its clock. Using now() less that elapsed time would count the first tick in the second tick's
// period, causing the gate to refuse the second tick.
func TestExclusiveCountsATickStalledAfterItsMeasureInThePeriodItWasDueIn(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	const name = "suggestion-outcomes"
	const period = time.Hour
	const early, delay = 50 * time.Millisecond, 100 * time.Millisecond

	var slow atomic.Bool
	var stalled atomic.Int32
	config := database.Pool.Config()
	config.MaxConns = 1
	// Exec mode sends the SQL in the same write as Bind and Execute, even after warm-up.
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return slowWrites{Conn: conn, slow: &slow, delay: delay, stalled: &stalled}, nil
	}
	leases, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the stalling leases pool: %v", err)
	}
	t.Cleanup(func() {
		testwait.Returns(t, "every connection of the stalling leases pool to come back, so the pool closes", leases.Close)
	})

	// Open the lease connection before stalling the period read.
	if !duty.Exclusive(ctx, nil, leases, name+"-warmup", tickDue, period, func(context.Context) {}) {
		t.Fatal("the unstalled warmup tick did not run")
	}

	due := databasePeriodBoundary(t, database, period).Add(-early)
	slow.Store(true)
	ran := duty.Exclusive(ctx, nil, leases, name, due, period, func(context.Context) {})
	slow.Store(false)
	if !ran {
		t.Fatal("the first tick did not run, so this row measures nothing")
	}
	if got := stalled.Load(); got != 1 {
		t.Fatalf("the first tick stalled %d writes, want exactly one (the period read)", got)
	}
	next := due.Add(period)
	ran = false
	if !duty.Exclusive(ctx, nil, leases, name, next, period, func(context.Context) { ran = true }) || !ran {
		t.Errorf("the tick due one period after a stalled one was refused (first due %v before a boundary, its period read stalled %v); a lone loop lost that period",
			early, delay)
	}
}
