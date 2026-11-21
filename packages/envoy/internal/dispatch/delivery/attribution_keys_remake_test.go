package delivery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// readingChange is a change of the reading as a later migration makes it. original is the reading
// as the catalog holds it (pg_get_functiondef). changed is the same function with one statement
// more before its select: each text gets the same keys, and the identity of the reading changes.
// A comment or white space does not change the identity: the catalog stores the parsed body
// (prosqlbody), with no comment, and the identity leaves out the places in the text it records.
type readingChange struct {
	schema, original, changed, identity string
}

// prepareReadingChange reads the reading and makes its change. Its cleanup puts the original
// function back, and fails the test where the identity does not come back, so no later test sees
// a changed reading.
func prepareReadingChange(t *testing.T, ctx context.Context, pool *store.Pool) readingChange {
	t.Helper()
	change := readingChange{schema: readingSchema(t, ctx, pool)}
	change.identity = currentReading(t, ctx, pool, change.schema)
	if err := pool.QueryRow(ctx, `select pg_catalog.pg_get_functiondef('`+change.schema+
		`.attribution_keys(text,text,text,text,text[])'::pg_catalog.regprocedure)`).Scan(&change.original); err != nil {
		t.Fatalf("read the definition of the reading: %v", err)
	}
	const body = "BEGIN ATOMIC\n"
	if found := strings.Count(change.original, body); found != 1 {
		t.Fatalf("the definition of the reading holds %d places of %q, want 1:\n%s", found, body, change.original)
	}
	change.changed = strings.Replace(change.original, body, body+" SELECT 1;\n", 1)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		applyReading(t, ctx, pool, change.original)
		if identity := currentReading(t, ctx, pool, change.schema); identity != change.identity {
			t.Errorf("after the restore the reading is %s, want the original %s", identity, change.identity)
		}
	})
	return change
}

// applyReading replaces the reading with definition as a migration replaces it: in one
// transaction, the lock of the texts table, and then the create or replace statement.
func applyReading(t *testing.T, ctx context.Context, pool *store.Pool, definition string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the change of the reading: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `lock table pull_request_texts in share row exclusive mode`); err != nil {
		t.Fatalf("lock pull_request_texts: %v", err)
	}
	if _, err := tx.Exec(ctx, definition); err != nil {
		t.Fatalf("replace the reading: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the change of the reading: %v", err)
	}
}

// storeTextsRows stores count merged pull requests of acme/widgets from number first on, each read
// from GitHub and with its texts row. The trigger reads the keys of each texts row on the reading
// that runs now.
func storeTextsRows(t *testing.T, ctx context.Context, pool *store.Pool, first, count int) {
	t.Helper()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, title, url, event_at, checked_at, github_read_at)
		select 'acme/widgets', n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'legion/ACME-' || n, 'main', 'merged', 'Change ' || n,
			'https://github.com/acme/widgets/pull/' || n, $3::timestamptz, now(), now()
		from pg_catalog.generate_series($1::int, $1::int + $2::int - 1) n
	`, first, count, at); err != nil {
		t.Fatalf("store %d results rows: %v", count, err)
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		select h.repo, h.number, h.head_sha, h.head_ref, h.repo, h.base_ref, h.title, 'Fixes #12 of ACME-1.', array['Work on ACME-2.'], h.event_at
		from pull_request_heads h where h.repo = 'acme/widgets' and h.number between $1::int and $1::int + $2::int - 1
	`, first, count); err != nil {
		t.Fatalf("store the texts of %d rows: %v", count, err)
	}
}

// noReadFollows fails the test where some read of GitHub is owed, or where some results row lost
// its mark (github_read_at): a remake of keys owes no read.
func noReadFollows(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	var owed, unmarked int
	if err := pool.QueryRow(ctx, `select (select count(*) from pull_request_reads_owed),
		(select count(*) from pull_request_heads where github_read_at is null)`).Scan(&owed, &unmarked); err != nil {
		t.Fatalf("count the reads owed: %v", err)
	}
	if owed != 0 || unmarked != 0 {
		t.Errorf("%d reads of GitHub are owed and %d results rows lost their mark, want 0 and 0: a remake reads no pull request", owed, unmarked)
	}
}

// The key remake loop remakes the keys of every texts row after a change of the reading while it
// runs. 2,500 texts rows are on the reading. The loop starts with a check every 200 ms, and the
// reading changes after some checks, as a migration changes it. Within 5 s every row is on the new
// reading. The loop takes no client of GitHub, and the remake owes no read, so no read of GitHub
// follows. Control in the same test: with no loop, the same change leaves the 2,500 rows on the old
// reading for 5 s. It fails where the loop checks only at its start, or where a check that finds
// rows on another reading remakes none or some.
func TestTheKeyRemakeLoopRemakesKeysAfterAChangeWhileItRuns(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const rows = 2500
	storeTextsRows(t, ctx, pool, 1, rows)
	change := prepareReadingChange(t, ctx, pool)
	if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != 0 {
		t.Fatalf("%d rows are on another reading before the change, want 0", other)
	}

	if !t.Run("control_no_loop", func(t *testing.T) {
		applyReading(t, ctx, pool, change.changed)
		if identity := currentReading(t, ctx, pool, change.schema); identity == change.identity {
			t.Fatalf("the changed function keeps the identity %s: it is no change of the reading", identity)
		}
		other := rows
		if testwait.Holds(5*time.Second, 100*time.Millisecond, func() bool {
			other = rowsOnAnotherReading(t, ctx, pool, change.schema)
			return other != rows
		}) {
			t.Errorf("the control: with no loop, %d of %d rows are on another reading within 5 s of the change, want %d for 5 s", other, rows, rows)
		}
		applyReading(t, ctx, pool, change.original)
		if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != 0 {
			t.Fatalf("after the original reading came back %d rows are on another reading, want 0", other)
		}
	}) {
		return
	}

	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeyRemakeLoop(loop, pool, 200*time.Millisecond)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	// The loop checks at once and then every 200 ms. The change comes after several checks that
	// found every row on the reading that runs.
	time.Sleep(time.Second)
	applyReading(t, ctx, pool, change.changed)
	changedAt := time.Now()
	other := rows
	if !testwait.Holds(5*time.Second, 50*time.Millisecond, func() bool {
		other = rowsOnAnotherReading(t, ctx, pool, change.schema)
		return other == 0
	}) {
		t.Errorf("%d of %d rows are on another reading %v after the change, want 0 within 5 s", other, rows, time.Since(changedAt).Round(time.Millisecond))
	}
	stop()
	<-done
	noReadFollows(t, ctx, pool)
}

// Two remakes at once take disjoint rows. 3,000 texts rows are on another reading after a change of
// the reading. Two RemakeKeys calls start together. Their counts add up to 3,000, and no row stays
// on another reading. It fails where a remake counts a row that the other remade (an update with no
// test of the reading), or where the two remakes stop while rows are on another reading.
func TestTwoRemakesTakeDisjointRows(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const rows = 3000
	storeTextsRows(t, ctx, pool, 1, rows)
	change := prepareReadingChange(t, ctx, pool)
	applyReading(t, ctx, pool, change.changed)
	if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != rows {
		t.Fatalf("%d rows are on another reading after the change, want %d", other, rows)
	}

	var counts [2]int
	var failures [2]error
	start := make(chan struct{})
	var remakes sync.WaitGroup
	for i := range counts {
		remakes.Go(func() {
			<-start
			counts[i], failures[i] = RemakeKeys(ctx, pool)
		})
	}
	close(start)
	remakes.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("remake %d: %v", i+1, err)
		}
	}
	if counts[0]+counts[1] != rows {
		t.Errorf("the two remakes remade %d and %d rows, %d in all, want %d in all", counts[0], counts[1], counts[0]+counts[1], rows)
	}
	if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != 0 {
		t.Errorf("%d rows are on another reading after the two remakes, want 0", other)
	}
	t.Logf("the two remakes remade %d and %d rows", counts[0], counts[1])
	noReadFollows(t, ctx, pool)
}
