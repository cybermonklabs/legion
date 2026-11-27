package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A record's retry (retryRecord) is work its request leaves behind: it runs after that request
// has answered, so it takes its connection from the background pool, as every such piece of work
// does, and none from the request pool, whose connections admission hands out to requests. Each
// record here - a create's near_copy_checks row and a decision block's related_checks row - has its
// first attempt held up past its second by a lock on its table, as a migration's lock would hold
// it. The retry's insert then waits on that lock on a background-pool connection, and once the
// lock is released the request pool has handed out the first attempt's one connection and no
// other, the background pool the retry's, and the row is written.
func TestARecordRetryTakesNoRequestPoolConnection(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, "RETRY", "Orchard pickers cannot log a crate")
	actor := model.Actor{Kind: "user", ID: "alice"}
	nearCopyID := uuid.NewString()
	blockID := "retry-" + uuid.NewString()
	for _, record := range []struct {
		table   string
		write   func()
		written string
		key     string
	}{
		{
			table: "near_copy_checks",
			write: func() {
				// The row is built as the create builds it (newNearCopyCheckRow), which encodes the
				// candidates the write binds and the column refuses null for.
				check := &nearCopyCheck{outcome: nearCopyPassed}
				row := newNearCopyCheckRow(nearCopyID, "RETRY", actor, "Orchard pickers cannot log a crate", "", "", check)
				row.issue = issue
				served.recordNearCopyCheck(context.Background(), row, check)
			},
			written: `select count(*) from near_copy_checks where id = $1::uuid`,
			key:     nearCopyID,
		},
		{
			table: "related_checks",
			write: func() {
				served.recordRelatedChecks(context.Background(), relatedCheckRecord{
					source:      suggestionSource{kind: "decision_block", issueKey: issue, blockID: blockID, actor: actor},
					suggestions: &model.Suggestions{Outcome: relatedNotMade},
					record:      relatedRecord{project: "RETRY", reason: relatedReasonTimeout, terms: []string{}, totalMS: 1},
				})
			},
			written: `select count(*) from related_checks where source_block_id = $1`,
			key:     blockID,
		},
	} {
		t.Run(record.table, func(t *testing.T) {
			lockTx, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin the lock transaction: %v", err)
			}
			t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
			if _, err := lockTx.Exec(context.Background(), "lock table "+record.table+" in access exclusive mode"); err != nil {
				t.Fatalf("lock %s: %v", record.table, err)
			}
			requests := settledAcquires(t, database, store.RequestsPool)
			background := settledAcquires(t, database, store.BackgroundPool)

			record.write()
			if got := served.relatedRecordRetrying.Load(); got != 1 {
				t.Fatalf("records being retried after the first attempt = %d, want 1 (its first attempt held up by the lock)", got)
			}
			// The retry's insert, waiting on the lock, is a backend of the background pool's. The
			// read runs on the lock's own transaction, so it takes no connection of the pools
			// measured, and it drops that transaction's statistics snapshot before each look, which
			// would otherwise answer every look of the transaction with what the first one saw.
			activity := func(query string, args ...any) pgx.Row {
				t.Helper()
				if _, err := lockTx.Exec(context.Background(), `select pg_stat_clear_snapshot()`); err != nil {
					t.Fatalf("clear the statistics snapshot: %v", err)
				}
				return lockTx.QueryRow(context.Background(), query, args...)
			}
			deadline := time.Now().Add(testwait.Remaining(t, 30*time.Second))
			for {
				var waiting bool
				if err := activity(`
					select exists (select from pg_stat_activity
					 where datname = current_database() and state = 'active' and wait_event_type = 'Lock'
					   and application_name = $1 and query like '%insert into ' || $2::text || '%')
				`, "dispatch-"+store.BackgroundPool, record.table).Scan(&waiting); err != nil {
					t.Fatalf("read the waiting inserts: %v", err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					var active []string
					if err := activity(`
						select coalesce(array_agg(application_name || ' ' || coalesce(wait_event_type, '-') || ' ' || left(query, 80)), '{}')
						  from pg_stat_activity
						 where datname = current_database() and state = 'active' and pid <> pg_backend_pid()
					`).Scan(&active); err != nil {
						t.Fatalf("read the active backends: %v", err)
					}
					t.Fatalf("no insert into %s waits on the lock from the background pool before the test binary's deadline: the retry runs elsewhere; active: %q", record.table, active)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := lockTx.Rollback(context.Background()); err != nil {
				t.Fatalf("release %s: %v", record.table, err)
			}
			served.detached.wait()
			tookRequests := poolAcquires(database, store.RequestsPool) - requests
			tookBackground := poolAcquires(database, store.BackgroundPool) - background

			if tookRequests != 1 {
				t.Errorf("the record took %d request-pool connection(s), want 1, its first attempt's: its retry is work its request leaves behind and runs on the background pool", tookRequests)
			}
			if tookBackground < 1 {
				t.Errorf("the record took %d background-pool connection(s), want its retry's", tookBackground)
			}
			var rows int
			if err := database.Pool.QueryRow(context.Background(), record.written, record.key).Scan(&rows); err != nil {
				t.Fatalf("read the %s row: %v", record.table, err)
			}
			if rows != 1 {
				t.Fatalf("%s holds %d rows of the record, want 1", record.table, rows)
			}
		})
	}
}
