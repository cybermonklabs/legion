package duty_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

const lockPassTable = `lock table duty_passes in access exclusive mode`

// A period read queued behind the table lock must refuse the pass at its deadline. The default
// pgx deadline watcher sets the socket deadline when the statement's context ends. The read then
// fails and pgx cancels the query and closes the connection. Postgres frees the lease when that
// session ends, not when Exclusive tries to unlock on the closed connection.
func TestExclusiveBoundsAPeriodReadThatWaitsOnALock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := testwait.Context(t)
	fake := clocktest.New(tickDue)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const name = "read-bound"

	warmPeriodStatements(t, ctx, fake, leases, name+"-warmup")
	holder := beginPassTableHolder(t, ctx, database)
	if _, err := holder.Exec(ctx, lockPassTable); err != nil {
		t.Fatalf("hold duty_passes: %v", err)
	}

	passRan := false
	answered := make(chan bool, 1)
	gateLeases := pidLeases{Leases: leases, pids: make(chan uint32, 1)}
	go func() {
		answered <- duty.ExclusiveOn(ctx, fake, logger, gateLeases, name, tickDue, time.Hour, func(context.Context) { passRan = true })
	}()
	gatePID := testwait.Receive(t, "the period read's backend PID", gateLeases.pids)
	if ran, early := storetest.WaitForQueuedBehind(t, holder, gatePID, answered); early {
		t.Fatalf("the gate answered %t before its period read queued behind duty_passes", ran)
	}
	advanceLeaseBound(t, fake, "the period read", answered, false)
	if passRan {
		t.Error("the pass ran without knowing whether its period was already served")
	}
	requireLeaseDeadlineLog(t, &logs, "dispatch duty: read the loop's last pass")
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release duty_passes: %v", err)
	}
	waitForLeaseAfterBound(t, ctx, database, name)
}

// A pass can finish while its record waits behind a table lock. The deadline must return true
// for the work already done, leave its period unserved, and let another session take the lease.
func TestExclusiveBoundsARecordThatGetsNoAnswer(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := testwait.Context(t)
	fake := clocktest.New(tickDue)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const name = "record-bound"

	warmPeriodStatements(t, ctx, fake, leases, name+"-warmup")
	holder := beginPassTableHolder(t, ctx, database)
	locked := make(chan error, 1)
	answered := make(chan bool, 1)
	gateLeases := pidLeases{Leases: leases, pids: make(chan uint32, 1)}
	go func() {
		answered <- duty.ExclusiveOn(ctx, fake, logger, gateLeases, name, tickDue, time.Hour, func(passCtx context.Context) {
			_, err := holder.Exec(passCtx, lockPassTable)
			locked <- err
		})
	}()
	gatePID := testwait.Receive(t, "the record's backend PID", gateLeases.pids)
	if err := testwait.Receive(t, "the pass to lock duty_passes from another session", locked); err != nil {
		t.Fatalf("hold duty_passes during the pass: %v", err)
	}
	if ran, early := storetest.WaitForQueuedBehind(t, holder, gatePID, answered); early {
		t.Fatalf("the gate answered %t before its record queued behind duty_passes", ran)
	}
	advanceLeaseBound(t, fake, "the record", answered, true)
	requireLeaseDeadlineLog(t, &logs, "dispatch duty: record the pass")
	// pgx returns the deadline error before its asynchronous cancel reaches Postgres. Keep the
	// table locked until the server has stopped waiting, or the upsert could still commit.
	testwait.Until(t, "the canceled record to leave the duty_passes lock queue", func() (bool, bool) {
		queued, err := storetest.QueuedBehind(ctx, holder, gatePID)
		if err != nil {
			t.Fatalf("inspect the record's lock wait: %v", err)
		}
		return queued, !queued
	})
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release duty_passes: %v", err)
	}
	if got := recordedPasses(t, database, name); got != 0 {
		t.Errorf("the timed-out record left %d rows for %s, want none", got, name)
	}
	waitForLeaseAfterBound(t, ctx, database, name)
}

// The lease take must refuse the pass when the fake reaches the take's deadline. The held write
// never reaches Postgres, so another session can take the lease without waiting for cleanup.
func TestExclusiveBoundsATakeThatGetsNoAnswer(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	fake := clocktest.New(tickDue)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const name = "take-bound"

	leases, conn := stallingLeasePool(t, ctx, database, name+"-warmup")
	conn.stall.Store(true)
	passRan := false
	answered := make(chan bool, 1)
	go func() {
		answered <- duty.ExclusiveOn(ctx, fake, logger, leases, name, tickDue, 0, func(context.Context) { passRan = true })
	}()
	testwait.Receive(t, "the take to enter its socket write", conn.entered)
	advanceLeaseBound(t, fake, "the take", answered, false)
	if passRan {
		t.Error("the pass ran without taking the lease")
	}
	requireLeaseDeadlineLog(t, &logs, "dispatch duty: take the lease")
	if !tryLeaseAfterBound(t, ctx, database, name) {
		t.Fatal("another session could not take the lease that was never sent to Postgres")
	}
}

// With no period to record, the pass stalls the next write, the unlock. The unlock deadline must
// destroy the connection rather than return it to the pool. The finished pass still counts, and
// another session can take the lease once Postgres ends the original session.
func TestExclusiveBoundsAnUnlockThatGetsNoAnswer(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	fake := clocktest.New(tickDue)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const name = "unlock-bound"

	leases, conn := stallingLeasePool(t, ctx, database, name+"-warmup")
	answered := make(chan bool, 1)
	go func() {
		answered <- duty.ExclusiveOn(ctx, fake, logger, leases, name, tickDue, 0, func(context.Context) { conn.stall.Store(true) })
	}()
	testwait.Receive(t, "the unlock to enter its socket write", conn.entered)
	advanceLeaseBound(t, fake, "the unlock", answered, true)
	requireLeaseDeadlineLog(t, &logs, "dispatch duty: free the lease")
	requireConnections(t, leases, 0, 0, "a connection whose unlock reached its deadline must be destroyed")
	waitForLeaseAfterBound(t, ctx, database, name)
}

func warmPeriodStatements(t *testing.T, ctx context.Context, fake *clocktest.Fake, leases *pgxpool.Pool, name string) {
	t.Helper()
	// Prepare the statements before locking the table, so the gate queues on execution.
	if !duty.ExclusiveOn(ctx, fake, nil, leases, name, tickDue, time.Hour, func(context.Context) {}) {
		t.Fatal("the warmup pass did not run")
	}
}

func beginPassTableHolder(t *testing.T, ctx context.Context, database *store.Store) pgx.Tx {
	t.Helper()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds duty_passes: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	return holder
}

func advanceLeaseBound(t *testing.T, fake *clocktest.Fake, statement string, answered <-chan bool, wantRan bool) {
	t.Helper()
	fake.AwaitArmedAt(t, statement+" deadline", tickDue.Add(duty.LeaseCallTimeout))
	select {
	case ran := <-answered:
		t.Fatalf("the gate answered %t before the %s deadline", ran, statement)
	default:
	}
	fake.Advance(duty.LeaseCallTimeout)
	if ran := testwait.Receive(t, "the gate to answer at "+statement+" deadline", answered); ran != wantRan {
		t.Errorf("the gate answered %t at %s deadline, want %t", ran, statement, wantRan)
	}
}

// requireLeaseDeadlineLog checks the error on the named statement's line, not on a later line
// about cleanup. The test reads the buffer only after the gate returns.
func requireLeaseDeadlineLog(t *testing.T, logs *bytes.Buffer, message string) {
	t.Helper()
	output := logs.String()
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, `msg="`+message+`"`) && strings.Contains(line, context.DeadlineExceeded.Error()) {
			return
		}
	}
	t.Fatalf("no deadline error on the %q log line:\n%s", message, output)
}

func waitForLeaseAfterBound(t *testing.T, ctx context.Context, database *store.Store, name string) {
	t.Helper()
	testwait.Until(t, "another session to take lease "+name+" after the deadline", func() (bool, bool) {
		taken := tryLeaseAfterBound(t, ctx, database, name)
		return taken, taken
	})
}

func tryLeaseAfterBound(t *testing.T, ctx context.Context, database *store.Store, name string) bool {
	t.Helper()
	// The shared pool cannot hand out the gate's session, which could retake its own lock.
	other, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire another session: %v", err)
	}
	defer other.Release()
	var taken bool
	if err := other.QueryRow(ctx, duty.TakeLease, duty.LeaseNamespace, name).Scan(&taken); err != nil {
		t.Fatalf("try taking %s from another session: %v", name, err)
	}
	if taken {
		var freed bool
		if err := other.QueryRow(ctx, duty.FreeLease, duty.LeaseNamespace, name).Scan(&freed); err != nil || !freed {
			t.Fatalf("release %s from the other session: freed %t, error %v", name, freed, err)
		}
	}
	return taken
}

func stallingLeasePool(t *testing.T, ctx context.Context, database *store.Store, warmName string) (*pgxpool.Pool, *stallingConn) {
	t.Helper()
	config := database.Pool.Config()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	// An acquire-time ping must not send the first stalled write instead of the take.
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	var dialed atomic.Pointer[stallingConn]
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		wrapped := &stallingConn{Conn: conn, entered: make(chan struct{}, 1), broken: make(chan struct{})}
		dialed.Store(wrapped)
		return wrapped, nil
	}
	leases, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the stalling lease pool: %v", err)
	}
	t.Cleanup(func() {
		testwait.Returns(t, "the stalling lease pool to close", leases.Close)
	})
	if !duty.Exclusive(ctx, nil, leases, warmName, tickDue, 0, func(context.Context) {}) {
		t.Fatal("the unstalled warmup pass did not run")
	}
	// Stall only this socket. pgx uses a separate dial for its cleanup cancel request.
	conn := dialed.Load()
	t.Cleanup(func() { _ = conn.Close() })
	return leases, conn
}

// stallingConn withholds a write until pgx's context watcher sets a socket deadline or the
// connection closes. Once broken, it never sends another byte.
type stallingConn struct {
	net.Conn
	stall   atomic.Bool
	entered chan struct{}
	broken  chan struct{}
	once    sync.Once
}

func (c *stallingConn) Write(b []byte) (int, error) {
	select {
	case <-c.broken:
		return 0, os.ErrDeadlineExceeded
	default:
	}
	if !c.stall.Load() {
		return c.Conn.Write(b)
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	_, err := testwait.ReceiveInHelper("stallingConn", "pgx to set a deadline or close the stalled socket", c.broken)
	// Terminate cannot get through a broken write side. Close the socket so pgx's cleanup does
	// not have to wait on its read deadline to end the session.
	_ = c.Close()
	if err != nil {
		return 0, err
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *stallingConn) SetDeadline(at time.Time) error {
	err := c.Conn.SetDeadline(at)
	c.breakAtDeadline(at)
	return err
}

func (c *stallingConn) SetWriteDeadline(at time.Time) error {
	err := c.Conn.SetWriteDeadline(at)
	c.breakAtDeadline(at)
	return err
}

func (c *stallingConn) breakAtDeadline(at time.Time) {
	if c.stall.Load() && !at.IsZero() {
		c.once.Do(func() { close(c.broken) })
	}
}

func (c *stallingConn) Close() error {
	c.once.Do(func() { close(c.broken) })
	return c.Conn.Close()
}
