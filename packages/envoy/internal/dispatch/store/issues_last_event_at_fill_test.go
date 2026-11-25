package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newestEventFillMigration is the migration that fills each issue's newest-event pair
// (last_event_at, last_event_at_seq) from its event of last_seq.
const newestEventFillMigration = "20261125094600_issues_last_event_at_backfill.up.sql"

// newestEventFillWait bounds how long the test waits for the fill's backend to queue behind the
// row the second transaction holds, and for the fill to end once that transaction lets go. Both
// are far above what a fill of three rows takes, so a wait that runs out is the order being
// wrong, not a slow machine.
const newestEventFillWait = 20 * time.Second

// TestTheNewestEventFillTakesTheIssueRowsInKeyOrder holds 20261125094600's fill to R5 of
// dispatch://INFRA-5791: it takes the row lock of each issue it writes in key order (`key collate
// "C"`), the order store.LockOwners takes the rows of several issues in (ownerRowsIssues), so the
// fill and such a write take any two issue rows in one order. It proves the order as the lock-step
// tests of the store's writers prove theirs (TestTheSeedsCommentWriteTakesTheStoreLockOrder in
// cmd/dispatch-seed): a second transaction holds a row the fill takes, and the test reads where
// the fill's own backend waits.
//
// Three issues, each with one event and a null pair, are written by COPY with the greatest key
// first, so a sequential scan reaches the greatest key first. The second transaction takes the
// row of the least key `for no key update`; the fill then runs as its migration file holds it
// (migrationFiles), on a connection of its own, and the test waits until that backend waits on
// the second transaction. While it waits, the second transaction takes the row of the greatest key
// `for no key update nowait`: a fill in key order holds no row yet, so the take succeeds, where a
// fill in the order of its scan holds that row, and the take fails with lock_not_available
// (55P03). Then the second transaction rolls back, and the fill ends and writes the pair of each
// of the three issues from its event.
//
// The fill's backend is watched through pg_stat_activity and pg_blocking_pids, read through the
// pool as storetest.CountLockWaits reads them, since that package imports this one.
func TestTheNewestEventFillTakesTheIssueRowsInKeyOrder(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTestStore(t, latestVersion)
	fill, err := migrationFiles.ReadFile("migrations/" + newestEventFillMigration)
	if err != nil {
		t.Fatalf("read %s: %v", newestEventFillMigration, err)
	}
	const project = "FILL"
	least, middle, greatest := project+"-1", project+"-2", project+"-3"
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee, next_number) values ($1, 'The fill', 'alice', 4)`, project); err != nil {
		t.Fatalf("write the project: %v", err)
	}
	var issues, events [][]any
	for number, key := range []string{greatest, middle, least} {
		issues = append(issues, []any{key, project, 3 - number, "Filled " + key, "todo", `{"kind":"user","id":"alice"}`, key, 1})
		events = append(events, []any{key, 1, "issue.created", `{"kind":"user","id":"alice"}`, `{}`, false, at.Add(time.Duration(number) * time.Minute)})
	}
	for _, write := range []struct {
		table   string
		columns []string
		rows    [][]any
	}{
		{"issues", []string{"key", "project_key", "number", "title", "status", "created_by", "rank", "last_seq"}, issues},
		{"events", []string{"issue_key", "seq", "type", "actor", "payload", "notify", "created_at"}, events},
	} {
		if _, err := store.Pool.CopyFrom(ctx, pgx.Identifier{write.table}, write.columns, pgx.CopyFromRows(write.rows)); err != nil {
			t.Fatalf("copy the %s rows: %v", write.table, err)
		}
	}

	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the second transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select key from issues where key = $1 for no key update`, least); err != nil {
		t.Fatalf("hold the row of %s: %v", least, err)
	}
	holderPID := holder.Conn().PgConn().PID()

	filler, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("open the fill's connection: %v", err)
	}
	t.Cleanup(func() { filler.Close(context.Background()) })
	fillerPID := filler.PgConn().PID()
	done := make(chan error, 1)
	go func() {
		_, err := filler.Exec(ctx, string(fill))
		done <- err
	}()

	for deadline := time.Now().Add(newestEventFillWait); ; {
		select {
		case err := <-done:
			t.Fatalf("the fill ended (%v) while a second transaction holds the row of %s, which it writes: it waited on no row", err, least)
		default:
		}
		var behind bool
		if err := store.Pool.QueryRow(ctx, `
			select coalesce(bool_or(wait_event_type = 'Lock' and $2 = any(pg_blocking_pids(pid))), false)
			from pg_stat_activity where pid = $1
		`, fillerPID, holderPID).Scan(&behind); err != nil {
			t.Fatalf("read where the fill waits: %v", err)
		}
		if behind {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fill never queued behind the second transaction's row of %s within %s", least, newestEventFillWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var running string
	if err := store.Pool.QueryRow(ctx, `select query from pg_stat_activity where pid = $1`, fillerPID).Scan(&running); err != nil {
		t.Fatalf("read the fill's statement: %v", err)
	}
	t.Logf("the fill (backend %d) waits behind the second transaction's row of %s, running: %.200s", fillerPID, least, running)

	if _, err := holder.Exec(ctx, `select key from issues where key = $1 for no key update nowait`, greatest); err != nil {
		if isSQLState(err, "55P03") {
			t.Fatalf("the fill holds the row of %s while it waits for the row of %s, which a second transaction holds: it takes the rows of issues out of key order", greatest, least)
		}
		t.Fatalf("take the row of %s without waiting: %v", greatest, err)
	}
	t.Logf("while the fill waits for %s, the second transaction takes the row of %s without waiting: the fill holds no row of a greater key", least, greatest)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("roll the second transaction back: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the fill once the second transaction let go: %v", err)
		}
	case <-time.After(newestEventFillWait):
		t.Fatalf("the fill did not end within %s of the second transaction letting go", newestEventFillWait)
	}

	rows, err := store.Pool.Query(ctx, `
		select i.key, i.last_event_at_seq, i.last_event_at = e.created_at
		from issues i left join events e on e.issue_key = i.key and e.seq = i.last_seq
		where i.project_key = $1
		order by i.key collate "C"
	`, project)
	if err != nil {
		t.Fatalf("read the pairs the fill wrote: %v", err)
	}
	written, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var key string
		var seq *int64
		var named *bool
		if err := row.Scan(&key, &seq, &named); err != nil {
			return "", err
		}
		if seq == nil || *seq != 1 || named == nil || !*named {
			t.Errorf("after the fill, %s holds the pair (seq %v, time of its event of last_seq %v), want the seq and the time of its event of last_seq 1", key, seq, named)
		}
		return key, nil
	})
	if err != nil {
		t.Fatalf("read the pairs the fill wrote: %v", err)
	}
	if len(written) != 3 {
		t.Fatalf("read %d issues after the fill, want the 3 written (%v)", len(written), written)
	}
}
