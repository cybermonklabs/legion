package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// recordOutboxWrites makes pool's database keep the id, issue, kind and payload of every outbox
// row a committed transaction writes, in a table of its own (outbox_written), which outboxWrites
// counts and writtenNotices reads. The outbox keeps no such record: it deletes a row once the row
// has run (record.Postgres.FinishOutbox), and a row with nothing left to do runs within
// milliseconds of its write, so a count of the rows the outbox holds cannot show that a row was
// written. pool must be a database of the test's own (isolatedOutboxPool, or a rig's
// testdb.Database once the daemon's boot has migrated it), since the trigger stays on it.
func recordOutboxWrites(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`create table outbox_written (id bigint, issue text, kind text, payload jsonb)`,
		`create function record_outbox_write() returns trigger language plpgsql as $$
			begin insert into outbox_written (id, issue, kind, payload) values (new.id, new.issue, new.kind, new.payload); return null; end $$`,
		`create trigger record_outbox_write after insert on outbox for each row execute function record_outbox_write()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("record the outbox's writes: %v", err)
		}
	}
}

// outboxWrites is how many rows of kind were written for issue since recordOutboxWrites, the rows
// the outbox still holds and the rows it has run and deleted alike.
func outboxWrites(t *testing.T, pool *store.Pool, issue string, kind record.OutboxKind) int {
	t.Helper()
	var written int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox_written where issue = $1 and kind = $2`,
		issue, string(kind)).Scan(&written); err != nil {
		t.Fatalf("read the %s rows written for %s: %v", kind, issue, err)
	}
	return written
}

// writtenNotices is every notice of kind written for issue since recordOutboxWrites, oldest first,
// as each was written, whatever became of its row since: still queued, delivered, or finished
// without delivery. The outbox's own table (noticesOfKind) cannot show that: a delivered notice is
// deleted, and so is one the outbox finishes undelivered because the architect that owns it has
// ended (TestANoticeForAnEndedArchitectFinishesWithoutDelivery), each when the outbox next runs it,
// so which notices that table still holds turns on when the outbox ran.
func writtenNotices(t *testing.T, pool *store.Pool, issue string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	return noticesIn(t, pool, "outbox_written", issue, kind)
}
