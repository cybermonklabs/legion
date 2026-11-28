package duty_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// slowWrites is a database connection whose every write waits delay first while slow is set: a
// loaded process descheduled, or a network path that stalls, between the tick and the database
// answering its period query.
type slowWrites struct {
	net.Conn
	slow  *atomic.Bool
	delay time.Duration
}

func (c slowWrites) Write(b []byte) (int, error) {
	if c.slow.Load() {
		time.Sleep(c.delay)
	}
	return c.Conn.Write(b)
}

// Two ticks due one period apart must both run even if the first period read stalls. The first
// tick is due 50 ms before the current database hour, and its connection stalls every write by
// 100 ms, including the period query. Subtracting the time since due measured before that query
// from the database's now() still puts the first tick past the boundary. That would count it
// against the second tick's period and make the gate refuse the second tick.
func TestExclusiveCountsATickStalledAfterItsMeasureInThePeriodItWasDueIn(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	const name = "suggestion-outcomes"
	const period = time.Hour
	const early, delay = 50 * time.Millisecond, 100 * time.Millisecond

	var slow atomic.Bool
	config := database.Pool.Config()
	config.MaxConns = 1
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return slowWrites{Conn: conn, slow: &slow, delay: delay}, nil
	}
	leases, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the stalling leases pool: %v", err)
	}
	t.Cleanup(func() {
		testwait.Returns(t, "every connection of the stalling leases pool to come back, so the pool closes", leases.Close)
	})

	// Open the lease connection and prepare the gate's statements before stalling writes.
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
	next := due.Add(period)
	ran = false
	if !duty.Exclusive(ctx, nil, leases, name, next, period, func(context.Context) { ran = true }) || !ran {
		t.Errorf("the tick due one period after a stalled one was refused (first due %v before a boundary, its writes stalled %v each); a lone loop lost that period",
			early, delay)
	}
}
