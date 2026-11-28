package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// shrinkRemakeChunk sets remakeChunk to 100 for the test, and puts the production value back at
// its end, so a remake of several chunks takes hundreds of texts rows and not thousands. Call it
// before a remake or a loop starts: the cleanups run in reverse, so the chunk is put back only
// once the loop has stopped.
func shrinkRemakeChunk(t *testing.T) {
	t.Helper()
	chunk := remakeChunk
	remakeChunk = 100
	t.Cleanup(func() { remakeChunk = chunk })
}

// loopChecks counts the checks of the key remake loop on the pool it traces: each statement that
// asks whether some texts row is on another reading (readingOtherStatement).
type loopChecks struct{ checks atomic.Int64 }

func (c *loopChecks) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_catalog.min(x.reading)") {
		c.checks.Add(1)
	}
	return ctx
}

func (c *loopChecks) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The key remake loop remakes the keys of every texts row after a change of the reading while it
// runs. 250 texts rows, two and a half chunks of the 100 the test sets (shrinkRemakeChunk), are on
// the reading. The loop starts with a check every 200 ms, and the reading changes after three
// checks that found every row on the reading that runs, as a migration changes it. Within 2 s, ten
// checks, every row is on the new reading. The loop takes no client of GitHub, and the remake owes
// no read, so no read of GitHub follows. Control in the same test: with no loop, the same change
// leaves the 250 rows on the old reading for those 2 s. It fails where the loop checks only at its
// start, or where a check that finds rows on another reading remakes none or some.
func TestTheKeyRemakeLoopRemakesKeysAfterAChangeWhileItRuns(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	shrinkRemakeChunk(t)
	const rows, window = 250, 2 * time.Second
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
		if testwait.Holds(window, 100*time.Millisecond, func() bool {
			other = rowsOnAnotherReading(t, ctx, pool, change.schema)
			return other != rows
		}) {
			t.Errorf("the control: with no loop, %d of %d rows are on another reading within %s of the change, want %d for %s", other, rows, window, rows, window)
		}
		applyReading(t, ctx, pool, change.original)
		if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != 0 {
			t.Fatalf("after the original reading came back %d rows are on another reading, want 0", other)
		}
	}) {
		return
	}

	checks := &loopChecks{}
	traced := storetest.Traced(t, pool, checks, nil)
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		KeyRemakeLoop(loop, traced, 200*time.Millisecond)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	// The loop checks at once and then every 200 ms. The change comes after three checks that
	// found every row on the reading that runs.
	waitFor(t, 10*time.Second, func() bool { return checks.checks.Load() >= 3 }, func() string {
		return fmt.Sprintf("the loop made %d checks in 10 s, want 3 before the change", checks.checks.Load())
	})
	applyReading(t, ctx, pool, change.changed)
	changedAt := time.Now()
	other := rows
	if !testwait.Holds(window, 50*time.Millisecond, func() bool {
		other = rowsOnAnotherReading(t, ctx, pool, change.schema)
		return other == 0
	}) {
		t.Errorf("%d of %d rows are on another reading %v after the change, want 0 within %s", other, rows, time.Since(changedAt).Round(time.Millisecond), window)
	}
	stop()
	<-done
	noReadFollows(t, ctx, pool)
}

// remakeStatements records the rows each statement of RemakeKeys remade on the pool it traces:
// the chunk statement (remakeKeysStatement), told by its row lock.
type remakeStatements struct {
	mu   sync.Mutex
	rows []int64
}

type remakeStatementKey struct{}

func (r *remakeStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "for update of t skip locked") {
		return context.WithValue(ctx, remakeStatementKey{}, true)
	}
	return ctx
}

func (r *remakeStatements) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(remakeStatementKey{}) == nil || data.Err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, data.CommandTag.RowsAffected())
}

// Two remakes at once take disjoint rows. 300 texts rows, three chunks of the 100 the test sets
// (shrinkRemakeChunk), are on another reading after a change of the reading. Two RemakeKeys calls
// start together. Their counts add up to 300, and no row stays on another reading. It fails where a
// remake counts a row that the other remade: a chunk taken with no lock and updated with no test of
// the reading (500 of 300, and 5,000 of 3,000 at the earlier size). The update's test of the reading
// dropped alone changes nothing a test can read: it repeats the chunk's condition on rows the same
// statement holds locked (`for update ... skip locked`, which PostgreSQL checks again on the row it
// locks), so no row the chunk took can fail it. It also fails where the two remakes stop while rows
// are on another reading (a remake of one chunk: 200 of 300). Each chunk statement the two remakes
// run remakes at most the 100 rows the test sets, and at least three remake rows, so the chunk the
// statements take is the shrunk one: with a chunk of 1,000, 300 rows are one or two statements, and
// the test would read no remake of several chunks.
func TestTwoRemakesTakeDisjointRows(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	shrinkRemakeChunk(t)
	const rows = 300
	storeTextsRows(t, ctx, pool, 1, rows)
	change := prepareReadingChange(t, ctx, pool)
	applyReading(t, ctx, pool, change.changed)
	if other := rowsOnAnotherReading(t, ctx, pool, change.schema); other != rows {
		t.Fatalf("%d rows are on another reading after the change, want %d", other, rows)
	}
	statements := &remakeStatements{}
	traced := storetest.Traced(t, pool, statements, nil)

	var counts [2]int
	var failures [2]error
	start := make(chan struct{})
	var remakes sync.WaitGroup
	for i := range counts {
		remakes.Go(func() {
			<-start
			counts[i], failures[i] = RemakeKeys(ctx, traced)
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
	statements.mu.Lock()
	chunks := slices.Clone(statements.rows)
	statements.mu.Unlock()
	remaking := 0
	for _, remade := range chunks {
		if remade > int64(remakeChunk) {
			t.Errorf("a chunk statement remade %d rows, more than the %d the test sets: the remakes did not take the shrunk chunk (statements %v)", remade, remakeChunk, chunks)
		}
		if remade > 0 {
			remaking++
		}
	}
	if remaking < rows/remakeChunk {
		t.Errorf("%d chunk statements remade rows (%v), want at least %d: 300 rows are three chunks of %d", remaking, chunks, rows/remakeChunk, remakeChunk)
	}
	t.Logf("the two remakes remade %d and %d rows, in chunk statements of %v rows", counts[0], counts[1], chunks)
	noReadFollows(t, ctx, pool)
}
