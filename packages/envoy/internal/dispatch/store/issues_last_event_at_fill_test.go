package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newestEventFillMigration is the migration that fills each issue's newest-event pair
// (last_event_at, last_event_at_seq) from its event of last_seq.
const newestEventFillMigration = "20261125094600_issues_last_event_at_backfill.up.sql"

// newestEventFillWait bounds how long the test waits for the fill to end while a second
// transaction holds one of its rows. It is far above what a fill of three rows takes, so a wait
// that runs out is the fill waiting on that row, not a slow machine.
const newestEventFillWait = 20 * time.Second

// TestTheNewestEventFillNeverWaitsOnAHeldIssueRow holds 20261125094600's fill to
// R5 of dispatch://INFRA-5791: it never waits on a row lock. A request takes its issues' rows in
// the order its statements reach them, so a fill that waited for a row a request holds could hold
// the row that request asks for next, and Postgres would abort one of the two. The fill leaves a
// held row's pair as it was, which the read probes, and the row's next append writes the pair.
//
// Three issues, each with one event and a null pair, are written by COPY. A second transaction
// takes the row of the middle one `for no key update`, as a request's write does. The fill then
// runs as its migration file holds it (migrationFiles), on a connection of its own, and the test
// reads, through the pool, where that backend is until it ends (pg_stat_activity and
// pg_blocking_pids, as storetest.CountLockWaits reads them; that package imports this one): a
// fill that waits behind the second transaction fails the test. Once the fill has ended, with the
// row still held, the held issue's pair is null and the other two hold the pair of their event of
// last_seq. Then the second transaction appends an event to the held issue as this release's
// QueueAppendWithLogins writes one, and commits, and the issue's pair is that event's.
func TestTheNewestEventFillNeverWaitsOnAHeldIssueRow(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTestStore(t, latestVersion)
	fill, err := migrationFiles.ReadFile("migrations/" + newestEventFillMigration)
	if err != nil {
		t.Fatalf("read %s: %v", newestEventFillMigration, err)
	}
	const project = "FILL"
	keys := []string{project + "-1", project + "-2", project + "-3"}
	held := keys[1]
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee, next_number) values ($1, 'The fill', 'alice', 4)`, project); err != nil {
		t.Fatalf("write the project: %v", err)
	}
	var issues, events [][]any
	for number, key := range keys {
		issues = append(issues, []any{key, project, number + 1, "Filled " + key, "todo", `{"kind":"user","id":"alice"}`, key, 1})
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
	if _, err := holder.Exec(ctx, `select key from issues where key = $1 for no key update`, held); err != nil {
		t.Fatalf("hold the row of %s: %v", held, err)
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
			if err != nil {
				t.Fatalf("the fill while a second transaction holds the row of %s: %v", held, err)
			}
		default:
			var behind bool
			if err := store.Pool.QueryRow(ctx, `
				select coalesce(bool_or(wait_event_type = 'Lock' and $2 = any(pg_blocking_pids(pid))), false)
				from pg_stat_activity where pid = $1
			`, fillerPID, holderPID).Scan(&behind); err != nil {
				t.Fatalf("read where the fill waits: %v", err)
			}
			if behind {
				var blocked string
				if err := store.Pool.QueryRow(ctx, `select query from pg_stat_activity where pid = $1`, fillerPID).Scan(&blocked); err != nil {
					t.Fatalf("read the fill's blocked statement: %v", err)
				}
				t.Fatalf("the fill waits behind the transaction that holds the row of %s: %s", held, statementNamed(blocked))
			}
			if time.Now().After(deadline) {
				t.Fatalf("the fill did not end within %s while a second transaction holds the row of %s", newestEventFillWait, held)
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		break
	}
	t.Logf("the fill ended while the second transaction holds the row of %s, and never waited behind it", held)

	pairs := func() map[string]bool {
		t.Helper()
		rows, err := store.Pool.Query(ctx, `
			select i.key, i.last_event_at_seq is not distinct from i.last_seq and i.last_event_at is not distinct from e.created_at,
			       i.last_event_at is null and i.last_event_at_seq is null
			from issues i left join events e on e.issue_key = i.key and e.seq = i.last_seq
			where i.project_key = $1
		`, project)
		if err != nil {
			t.Fatalf("read the pairs: %v", err)
		}
		named := map[string]bool{}
		if _, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
			var key string
			var names, null bool
			if err := row.Scan(&key, &names, &null); err != nil {
				return "", err
			}
			if !names && !null {
				t.Errorf("%s holds a pair that names no event of it and is not null", key)
			}
			named[key] = names
			return key, nil
		}); err != nil {
			t.Fatalf("read the pairs: %v", err)
		}
		return named
	}
	after := pairs()
	for _, key := range keys {
		switch {
		case key == held && after[key]:
			t.Errorf("after the fill, the held issue %s holds the pair of its event, want it null: the fill wrote a row another transaction held", key)
		case key != held && !after[key]:
			t.Errorf("after the fill, %s holds no pair of its event of last_seq, want the fill to have written it", key)
		}
	}

	if _, err := holder.Exec(ctx, `
		with appended as (
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ($1, (select last_seq from issues where key = $1) + 1, 'comment.created', '{"kind":"user","id":"alice"}', '{}', false)
			returning seq, created_at
		)
		update issues set last_seq = appended.seq, last_event_at = appended.created_at, last_event_at_seq = appended.seq,
			updated_at = greatest(issues.updated_at, appended.created_at)
		from appended where issues.key = $1
	`, held); err != nil {
		t.Fatalf("append an event to %s: %v", held, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the append to %s: %v", held, err)
	}
	if !pairs()[held] {
		t.Errorf("after an append to the held issue %s, its pair names no event of last_seq, want the append's", held)
	}
}

// statementNamed is how a failure names the statement a backend runs: the first line of its text
// in pg_stat_activity. For a migration that is the comment naming its file, since Postgres keeps
// only the start of a long text there (track_activity_query_size) and the fill's text opens with
// its comments.
func statementNamed(statement string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(statement), "\n")
	return strings.TrimSpace(first)
}
