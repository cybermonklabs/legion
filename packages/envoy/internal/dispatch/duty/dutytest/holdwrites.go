// Package dutytest records the writes of one duty hold's row (duty.Hold) on a test's database,
// each on the database's clock and each at the first moment it could be seen committed, for the
// tests that time a take-over of the hold: the outbox's killed-holder test and cmd/dispatch's
// two-process rows. It is an ordinary package rather than a _test.go file so both can import one
// recorder: Go cannot share test-only code across packages.
package dutytest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// channel is the channel the recording trigger notifies on every write it records.
const channel = "test_hold_writes"

// Database is the part of a store's pool the recorder runs its statements on.
type Database interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Write is one write of the hold's row: the holder it names; Started, its statement's now(), the
// moment the row's lapse is counted from (duty's expiries are now() plus the TTL); Changed, its
// clock_timestamp() when the row changed, before its transaction committed; and Seen, the
// database's clock_timestamp() of the first read that found it committed, which is at or after
// its commit, since no other session can read a write before that. Seq orders the writes: each
// write of the one row holds the row's lock until it commits, so the writes commit in Seq order.
type Write struct {
	Seq                    int64
	Holder                 uuid.UUID
	Started, Changed, Seen time.Time
}

// Writes is the record of the writes of one hold's row, read as each commits.
type Writes struct {
	db Database

	mu      sync.Mutex
	writes  []Write
	grew    chan struct{}
	readErr error
}

// Record installs a trigger on the hold name's row of duty_holds in db's database that records
// each write (Write) and notifies, and reads every write as it commits, through a connection
// listen dials, until t ends.
func Record(t testing.TB, db Database, listen func(context.Context) (*pgx.Conn, error), name string) *Writes {
	t.Helper()
	ctx := context.Background()
	literal := "'" + strings.ReplaceAll(name, "'", "''") + "'"
	if _, err := db.Exec(ctx, `
		create table test_hold_writes (seq bigserial primary key, holder uuid not null,
			started timestamptz not null, changed timestamptz not null);
		create function test_record_hold_write() returns trigger language plpgsql as $$
		begin
			insert into test_hold_writes (holder, started, changed) values (new.holder, now(), clock_timestamp());
			perform pg_notify('`+channel+`', new.holder::text);
			return null;
		end $$;
		create trigger test_record_hold_write after insert or update on duty_holds for each row
			when (new.name = `+literal+`) execute function test_record_hold_write();
	`); err != nil {
		t.Fatalf("record each write of the hold %s: %v", name, err)
	}
	conn, err := listen(ctx)
	if err != nil {
		t.Fatalf("connect the hold's listener: %v", err)
	}
	if _, err := conn.Exec(ctx, `listen `+channel); err != nil {
		t.Fatalf("listen for the hold's writes: %v", err)
	}
	w := &Writes{db: db, grew: make(chan struct{})}
	reading, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.read(reading, conn)
	}()
	t.Cleanup(func() {
		stop()
		<-done
		_ = conn.Close(context.Background())
	})
	return w
}

// read waits for each notification and reads, at once and on the same connection, every write
// committed since the last it read, dating each by that read's clock_timestamp(). It ends with
// ctx, or at the first statement that fails, which every later wait reports.
func (w *Writes) read(ctx context.Context, conn *pgx.Conn) {
	var last int64
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			w.fail(ctx, err)
			return
		}
		rows, err := conn.Query(ctx, `select seq, holder, started, changed, clock_timestamp() from test_hold_writes where seq > $1 order by seq`, last)
		if err != nil {
			w.fail(ctx, err)
			return
		}
		read, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Write])
		if err != nil {
			w.fail(ctx, err)
			return
		}
		if len(read) == 0 {
			continue
		}
		last = read[len(read)-1].Seq
		w.mu.Lock()
		w.writes = append(w.writes, read...)
		close(w.grew)
		w.grew = make(chan struct{})
		w.mu.Unlock()
	}
}

// fail records why the reads stopped, unless ctx ended them.
func (w *Writes) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	w.readErr = err
	close(w.grew)
	w.grew = make(chan struct{})
	w.mu.Unlock()
}

// wait returns the first write found by find in the writes read so far and those read within
// within, failing t, which waits for what, when none is.
func (w *Writes) wait(t testing.TB, within time.Duration, what string, find func([]Write) (Write, bool)) Write {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		w.mu.Lock()
		found, ok := find(w.writes)
		grew, err := w.grew, w.readErr
		w.mu.Unlock()
		if ok {
			return found
		}
		if err != nil {
			t.Fatalf("wait for %s: the hold's writes are no longer read: %v", what, err)
		}
		select {
		case <-grew:
		case <-timer.C:
			t.Fatalf("no %s within %v", what, within)
		}
	}
}

// All is every write read so far, in Seq order.
func (w *Writes) All() []Write {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Write(nil), w.writes...)
}

// Now is the database's clock_timestamp(), read now.
func (w *Writes) Now(t testing.TB) time.Time {
	t.Helper()
	var now time.Time
	if err := w.db.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// WaitForRenewal returns the first write by holder committed after this call began, within
// within: a renewal (or the take) the caller can act on at the moment it commits. A write
// committed before the call, however late its notification is read, is not one: the call first
// reads the last write committed, and waits for one after it.
func (w *Writes) WaitForRenewal(t testing.TB, holder uuid.UUID, within time.Duration) Write {
	t.Helper()
	var committed int64
	if err := w.db.QueryRow(context.Background(), `select coalesce(max(seq), 0) from test_hold_writes`).Scan(&committed); err != nil {
		t.Fatalf("read the hold's last committed write: %v", err)
	}
	return w.wait(t, within, "write of the hold by "+holder.String()+" committed after the wait began", func(writes []Write) (Write, bool) {
		for _, write := range writes {
			if write.Seq > committed && write.Holder == holder {
				return write, true
			}
		}
		return Write{}, false
	})
}

// Take is how another holder took the hold over from Holder, whose last act came at a moment the
// caller read the database's clock just after, SignaledBy.
type Take struct {
	// Holder is the holder the take replaced.
	Holder uuid.UUID
	// SignaledBy is the read of the database's clock the caller made just after Holder's last act.
	SignaledBy time.Time
	// Took is the take: the first write naming another holder after Holder's last write.
	Took Write
	// Renewal is Holder's last write whose statement began at or before SignaledBy: the renewal
	// the row's lapse is counted from, a renewal in flight at SignaledBy included. A renewal whose
	// statement the database began only after SignaledBy is not counted, since its lapse says
	// nothing of the time since Holder's last act.
	Renewal Write
	// Last is Holder's last write of all, the row the take replaced; LateRenewals is how many of
	// Holder's writes began after SignaledBy.
	Last         Write
	LateRenewals int
	// Changes is how many writes since Renewal named another holder than the write before them:
	// the takes of the hold, renewals of each holder left out.
	Changes int
}

// Reading is the take, at the first moment it could be seen committed, less Renewal's start, on
// the database's clock: at or above the time from Holder's last act to the take, since the act
// came after Renewal began.
func (k Take) Reading() time.Duration { return k.Took.Seen.Sub(k.Renewal.Started) }

// TakeOver waits, within within, for another holder to take the hold over from holder, and
// reads the take (Take). signaledBy is a read of the database's clock (Now) made just after
// holder's last act - a kill, a stop.
func (w *Writes) TakeOver(t testing.TB, holder uuid.UUID, signaledBy time.Time, within time.Duration) Take {
	t.Helper()
	took := w.wait(t, within, "take of the hold from "+holder.String(), func(writes []Write) (Write, bool) {
		for index, write := range writes {
			if write.Holder != holder && index > 0 && writes[index-1].Holder == holder && write.Seen.After(signaledBy) {
				return write, true
			}
		}
		return Write{}, false
	})
	take := Take{Holder: holder, SignaledBy: signaledBy, Took: took}
	writes := w.All()
	for _, write := range writes {
		if write.Holder != holder || write.Seq > took.Seq {
			continue
		}
		take.Last = write
		if write.Started.After(signaledBy) {
			take.LateRenewals++
		} else {
			take.Renewal = write
		}
	}
	if take.Renewal.Seq == 0 {
		t.Fatalf("no write of the hold by %s began at or before %s: %+v", holder, signaledBy, writes)
	}
	for index, write := range writes {
		if write.Seq > take.Renewal.Seq && write.Holder != writes[index-1].Holder {
			take.Changes++
		}
	}
	return take
}

// RequireTakeWithin is TakeOver, failing t when the take came more than bound after holder's last
// renewal before signaledBy (Take.Reading), at the moment the take could first be seen committed,
// on the database's clock. what names holder's last act in the log line and the failure.
func (w *Writes) RequireTakeWithin(t testing.TB, holder uuid.UUID, signaledBy time.Time, what string, bound, within time.Duration) Take {
	t.Helper()
	take := w.TakeOver(t, holder, signaledBy, within)
	t.Logf("after %s, %s took the hold %v after the last renewal before it, at the moment the take could be seen committed, on the database's clock (its row changed %v after that renewal, %v before it was seen), and %v after a read of that clock made after %s; the earlier holder's row began %d renewals after that read, and the hold changed hands %d times since that renewal",
		what, take.Took.Holder, take.Reading(), take.Took.Changed.Sub(take.Renewal.Started), take.Took.Seen.Sub(take.Took.Changed),
		take.Took.Seen.Sub(signaledBy), what, take.LateRenewals, take.Changes)
	if take.Reading() > bound {
		t.Errorf("%s took the hold %v after the last renewal before %s, at the moment the take could be seen committed, on the database's clock, past the %v bound",
			take.Took.Holder, take.Reading(), what, bound)
	}
	return take
}
