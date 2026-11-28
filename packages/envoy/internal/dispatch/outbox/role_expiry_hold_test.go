package outbox

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// armTakeover sets deps's holdRead hook to make another process the publisher hold's holder
// (writePublisherHold) the next time a holder transaction's first read runs while the returned
// flag is set, and clears the flag when it does: a test sets the flag just before the pass whose
// record it means to roll back, and a flag still set after that pass means no record ran.
func armTakeover(t *testing.T, database *store.Store, deps *Deps) *atomic.Bool {
	t.Helper()
	armed := new(atomic.Bool)
	deps.holdRead = func() {
		if armed.CompareAndSwap(true, false) {
			if err := writePublisherHold(context.Background(), database, uuid.New()); err != nil {
				t.Errorf("take the publisher hold over: %v", err)
			}
		}
	}
	return armed
}

// A takeover of the publisher hold that meets a holder transaction's final lock waits for that
// transaction to commit, so the new holder reads every write the old holder's record made - a note
// it adds to a window among them - before it can post that window. With the final lock taken away,
// the takeover goes through while the record is still open, and the old holder commits after the
// new one holds the hold.
//
// Not parallel: this row can observe a second process's lock wait in the database.
func TestATakeoverOfThePublisherHoldWaitsForAnOpenExpiryRecord(t *testing.T) {
	ctx := testwait.Context(t)
	rig := newRoleRig(t)
	const role = "expiry-record-open"
	locked, release := make(chan struct{}), make(chan struct{})
	unlock := sync.OnceFunc(func() { close(release) })
	var recordPID, takerPID atomic.Uint32
	database := tracedStore(t, rig.database, expiryLockTrace{record: &recordPID, taker: &takerPID})
	deps := rig.deps()
	deps.Store = database
	deps.holdLocked = func() {
		close(locked)
		<-release
	}
	outbox := holding(t, deps)
	event := expiredOnce(t, rig, outbox, role)
	passed := make(chan struct{})
	defer func() {
		unlock()
		testwait.Receive(t, "the holder's pass returning once the record is released", passed)
	}()
	go func() {
		defer close(passed)
		outbox.passKeptExpiries(ctx, unfenced{})
	}()
	testwait.Receive(t, "the expiry record reaching its final hold check", locked)
	tookOver := make(chan error, 1)
	go func() { tookOver <- writePublisherHold(ctx, database, uuid.New()) }()
	waitFor(t, "the takeover's backend waiting for the expiry record's backend", func() bool {
		select {
		case err := <-tookOver:
			t.Fatalf("another process took the publisher hold (%v) while the holder's expiry record was open", err)
		default:
		}
		var waiting bool
		if err := rig.database.Pool.QueryRow(ctx, `
			select exists (
				select from pg_stat_activity where pid = $1 and wait_event_type = 'Lock'
				and $2 = any(pg_blocking_pids(pid))
			)
		`, takerPID.Load(), recordPID.Load()).Scan(&waiting); err != nil {
			t.Fatalf("read which backend the takeover waits on: %v", err)
		}
		return waiting
	})
	unlock()
	testwait.Receive(t, "the holder's pass returning once the record is released", passed)
	if err := testwait.Receive(t, "the takeover completing once the record committed", tookOver); err != nil {
		t.Fatalf("the takeover failed once the record committed: %v", err)
	}
	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 {
		t.Fatalf("the holder's record wrote %+v, want its one expiry, committed before the takeover", rows)
	}
}

// expiryLockTrace identifies the backends that perform the final hold check and the takeover.
type expiryLockTrace struct {
	record, taker *atomic.Uint32
}

func (tr expiryLockTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, "select holder = $2 and expires_at > now() from duty_holds"):
		tr.record.Store(conn.PgConn().PID())
	case strings.Contains(data.SQL, "insert into duty_holds"):
		tr.taker.Store(conn.PgConn().PID())
	}
	return ctx
}

func (expiryLockTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A takeover of the publisher hold that lands after a holder transaction's first, unlocked read
// and before its final lock makes the transaction roll back: under READ COMMITTED the final lock
// sees the new holder, so the old holder commits nothing it wrote. It is checked for both holder
// transactions, each at the write that would append the event's role.undelivered record:
//   - a notice's record (recordKeptExpiry) of the event's third expiry writes no expiry, moves no
//     cursor and appends no record;
//   - a record of a copy that cannot be kept again (recordCannotKeep) leaves its row unsettled and
//     appends no record.
//
// With the final lock answering true whatever the row names, both commit.
func TestATakeoverBeforeTheFinalLockMakesTheRecordRollBack(t *testing.T) {
	t.Run("a notice's record", func(t *testing.T) {
		ctx := context.Background()
		rig := newRoleRig(t)
		const role = "expiry-takeover-notice"
		rig.silent(t, contracts.RoleTopicPrefix+role)
		event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
		deps := rig.deps()
		armed := armTakeover(t, rig.database, &deps)
		outbox := holding(t, deps)
		outbox.scan(ctx, unfenced{})
		expireUpTo(t, rig, outbox, unfenced{}, role, event.ID, maxRoleKeeps-1)
		before := expiryCursor(t, rig.database)
		if last := expire(t, rig.client, role, newestCopyOf(t, rig, role, event.ID)); last.Attempt != maxRoleKeeps {
			t.Fatalf("event %d's newest copy names attempt %d before the last expiry, want %d", event.ID, last.Attempt, maxRoleKeeps)
		}

		armed.Store(true)
		outbox.passKeptExpiries(ctx, unfenced{})

		if armed.Load() {
			t.Fatal("the pass never reached a holder transaction's first read")
		}
		if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != maxRoleKeeps-1 {
			t.Fatalf("the outbox recorded %+v after the takeover, want the %d expiries before it", rows, maxRoleKeeps-1)
		}
		if after := expiryCursor(t, rig.database); after != before {
			t.Fatalf("the expiry cursor moved from %d to %d after the takeover, want it where it was", before, after)
		}
		if records := undeliveredCount(t, rig.database, event.ID); records != 0 {
			t.Fatalf("event %d has %d role.undelivered records after the takeover, want none", event.ID, records)
		}
	})

	t.Run("a record of a copy that cannot be kept again", func(t *testing.T) {
		ctx := context.Background()
		rig := newRoleRig(t)
		const role = "expiry-takeover-cannot-keep"
		rig.silent(t, contracts.RoleTopicPrefix+role)
		event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
		deps := rig.deps()
		armed := armTakeover(t, rig.database, &deps)
		outbox := holding(t, deps)
		owedExpiry(t, rig.database, event.ID, role, 1, 999991, nil)

		armed.Store(true)
		outbox.passKeptExpiries(ctx, unfenced{})

		if armed.Load() {
			t.Fatal("the pass never reached a holder transaction's first read")
		}
		if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 || rows[0].settled {
			t.Fatalf("event %d's expiries are %+v after the takeover, want its one row unsettled", event.ID, rows)
		}
		if records := undeliveredCount(t, rig.database, event.ID); records != 0 {
			t.Fatalf("event %d has %d role.undelivered records after the takeover, want none", event.ID, records)
		}
	})
}

// An expiry record that waits on the event owner's lock - here an open transaction that holds the
// issue's row, as any write on the issue does - for longer than the publisher hold survives with
// no renewal does not lose the hold: the record holds the hold's row only across its commit, so
// the hold's renewals go on while the record waits.
//
// Not parallel: this row observes the record and renewal backends' locks.
func TestAnExpiryRecordWaitingOnTheOwnerLockKeepsThePublisherHold(t *testing.T) {
	ctx := testwait.Context(t)
	fake := clocktest.New(time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC))
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithWindowClock(fake))
	if err != nil {
		t.Fatalf("connect the outbox's NATS client: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	rig := newRoleRigOn(t, url, client, testnats.Connect(t, url))
	database, move := databaseMoment(t, rig.database, fake)
	trace := &expiryRenewalTrace{}
	rig.database = tracedStore(t, database, trace)
	const role = "expiry-record-waits"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	deps := rig.deps()
	var finalLock atomic.Bool
	deps.holdLocked = func() { finalLock.Store(true) }
	outbox := New(deps)
	outbox.holdClock = fake
	options := publisherHoldOptions
	options.Clock = fake
	hold, err := duty.TryHold(ctx, rig.database.Pool, publisherHold, outbox.instance, options)
	if err != nil || hold == nil {
		t.Fatalf("take the publisher hold: %v", err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	outbox.scan(ctx, hold)
	expireUpTo(t, rig, outbox, hold, role, event.ID, maxRoleKeeps-1)
	if last := expire(t, rig.client, role, newestCopyOf(t, rig, role, event.ID)); last.Attempt != maxRoleKeeps {
		t.Fatalf("event %d's newest copy names attempt %d before the last expiry, want %d", event.ID, last.Attempt, maxRoleKeeps)
	}
	finalLock.Store(false)

	// Another write holds the issue's row while the record that appends the event's
	// role.undelivered record waits on it, for longer than the hold's loss window.
	blocker, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var blockerPID uint32
	if err := blocker.QueryRow(ctx, `select pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("read the owner lock's backend: %v", err)
	}
	if _, err := blocker.Exec(ctx, `select 1 from issues where key = 'T-1' for update`); err != nil {
		t.Fatalf("lock T-1's row: %v", err)
	}
	passed := make(chan struct{})
	defer func() {
		_ = blocker.Rollback(context.Background())
		testwait.Receive(t, "the expiry record returning after the owner lock was released", passed)
	}()
	go func() {
		defer close(passed)
		outbox.passKeptExpiries(ctx, hold)
	}()
	waitFor(t, "the expiry record's backend blocked on the T-1 owner lock", func() bool {
		select {
		case <-passed:
			t.Fatal("the expiry record returned before waiting on the T-1 owner lock")
		default:
		}
		var blocked bool
		if err := database.Pool.QueryRow(ctx, `select $2 = any(pg_blocking_pids($1))`,
			trace.record.Load(), blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("read the expiry record's owner-lock wait: %v", err)
		}
		return blocked
	})
	lossWithoutRenewal := hold.Deadline()
	for !fake.Now().After(lossWithoutRenewal) {
		advanceRenewal(t, fake, move, hold, func() {
			var blockers []int32
			if err := database.Pool.QueryRow(ctx, `select pg_blocking_pids($1)`, trace.renewal.Load()).Scan(&blockers); err != nil {
				t.Fatalf("read whether the renewal is blocked: %v", err)
			}
			if len(blockers) != 0 {
				t.Fatalf("the publisher hold renewal waits on lock(s) held by %v while the expiry record waits on the owner lock", blockers)
			}
			select {
			case <-passed:
				t.Fatal("the expiry record returned while the owner lock was still held")
			default:
			}
		})
	}
	requireHoldLive(t, hold, "while the expiry record waited on the owner lock")
	if records := undeliveredCount(t, rig.database, event.ID); records != 0 {
		t.Fatalf("event %d has %d role.undelivered records while T-1's row was held, want the record still waiting", event.ID, records)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	testwait.Receive(t, "the expiry record committing once the owner lock is free", passed)
	requireHoldLive(t, hold, "once the issue's owner lock was released")
	var live bool
	if err := database.Pool.QueryRow(ctx, `select exists (
		select from duty_holds where name = $1 and holder = $2 and expires_at > now()
	)`, publisherHold, outbox.instance).Scan(&live); err != nil || !live || !finalLock.Load() {
		t.Fatalf("after the record: hold row live=%t (%v), final lock=%t; want both", live, err, finalLock.Load())
	}
	outbox.passKeptExpiries(ctx, hold)
	if records := undeliveredCount(t, rig.database, event.ID); records != 1 {
		t.Fatalf("event %d has %d role.undelivered records once the owner lock was free, want one", event.ID, records)
	}
}

// expiryRenewalTrace identifies the expiry record and the single-statement renewal.
type expiryRenewalTrace struct {
	record, renewal atomic.Uint32
}

func (tr *expiryRenewalTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, "from duty_holds where name = $1") && strings.HasPrefix(strings.TrimSpace(data.SQL), "select"):
		tr.record.Store(conn.PgConn().PID())
	case strings.Contains(data.SQL, "update duty_holds set expires_at = now() + make_interval"):
		tr.renewal.Store(conn.PgConn().PID())
	}
	return ctx
}

func (*expiryRenewalTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
