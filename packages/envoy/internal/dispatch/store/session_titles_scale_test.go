package store

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// The inputs of TestSessionTitlesBootScale, which only scripts/session-titles-scale.sh sets:
//   - sessionTitlesScaleURLEnv names a Postgres server the test may create and drop databases on,
//     one database for each run, and never a server that holds anything else of value. CI never
//     sets it, so the test skips there. Its database path is ignored, as
//     DISPATCH_TEST_DATABASE_URL's is.
//   - sessionTitlesScaleNewestEnv is production's newest event id on the day of the run, read
//     through GET /api/v1/events/log. The floor of events each run loads is the larger of
//     sessionTitlesScaleMinEvents and 1.25 times it (dispatch://INFRA-5573, R4).
//   - sessionTitlesScaleSessionsEnv lists the session counts of the runs, space-separated; one
//     run for each, the events spread evenly over that many sessions.
//   - sessionTitlesScaleLoadEnv is the negative control's alone: each run loads this many events
//     instead of the floor, which the run's count check must refuse.
const (
	sessionTitlesScaleURLEnv      = "DISPATCH_SESSION_TITLES_SCALE_URL"
	sessionTitlesScaleNewestEnv   = "DISPATCH_SESSION_TITLES_SCALE_NEWEST_EVENT_ID"
	sessionTitlesScaleSessionsEnv = "DISPATCH_SESSION_TITLES_SCALE_SESSIONS"
	sessionTitlesScaleLoadEnv     = "DISPATCH_SESSION_TITLES_SCALE_LOAD"

	sessionTitlesScaleSessionsDefault = "110000 11000"
	// sessionTitlesScaleMinEvents is R4's floor before production's own size is read.
	sessionTitlesScaleMinEvents = 1_500_000
	// sessionTitlesScaleBound is R4's bound on the four migrations and the fill together: with
	// the 30 s that bootProbeTimeout gives the boot's database probe and the boot's other steps
	// before /healthz, it keeps the boot inside the compose deployer's 90 s (wait_for_health in
	// deploy/scripts/autodeploy.sh).
	sessionTitlesScaleBound = 60 * time.Second
	// sessionTitlesScaleIssues is more issues than production held on 2026-11-23 (13,296), across
	// sessionTitlesScaleProjects projects, for 20261117100010's index build. Of them, the open and
	// done P0 issues stand in production's proportions of that day: 161 open and 540 done of
	// 13,279.
	sessionTitlesScaleIssues   = 15_000
	sessionTitlesScaleProjects = 22
	// sessionTitlesScaleBatch is how many events one insert statement of the load writes.
	sessionTitlesScaleBatch = 100_000
)

// sessionTitlesSliceVersions are the four migrations of dispatch://INFRA-5573: each run migrates
// its database with every other migration of the shipped set first, as a deployed database stands
// before the release that carries the four, and the boot's Migrate then applies these alone.
var sessionTitlesSliceVersions = []int{20261117100008, 20261117100010, 20261117100015, 20261117100016}

// TestSessionTitlesBootScale times what the first boot of the release that carries
// session_titles does before it listens, on a store larger than production's
// (dispatch://INFRA-5573, R4): the four migrations of the slice, applied by Migrate as the boot
// applies them, and FillSessionTitles, timed together as one span. For each session count of
// DISPATCH_SESSION_TITLES_SCALE_SESSIONS it makes a database of its own and:
//
//  1. migrates it with every shipped migration but the four;
//  2. writes sessionTitlesScaleIssues issues, and the floor of events, every one by a session
//     actor that stamps a title, spread over that many sessions, each with a payload of about
//     1.7 KB of JSON that does not compress (production's mean, in a read of 500 events after id
//     1,072,000, is about 2 KB), as wide as a row of the heap holds whole
//     (writeSessionTitlesScaleStore);
//  3. vacuums and analyzes events and issues, as autovacuum has production's, and checkpoints;
//  4. applies the four migrations with Migrate, and runs FillSessionTitles once, timing the two
//     as one span.
//
// A run fails when it loaded fewer events than the floor, when Migrate applied anything but the
// four, when the fill left a session without its title, or when the span is longer than
// sessionTitlesScaleBound. Each run prints one line that starts `session-titles-scale:` and names
// every number its verdict rests on, on standard output, so a reader of the Job's log judges the
// run from that line.
func TestSessionTitlesBootScale(t *testing.T) {
	// Not parallel: it holds the boot's migrations and fill to a wall-clock bound
	// (sessionTitlesScaleBound, 60 s), which the load of the package's parallel tests would distort.
	server := os.Getenv(sessionTitlesScaleURLEnv)
	if server == "" {
		t.Skip(sessionTitlesScaleURLEnv + " is unset: it must name a Postgres server this test may create and drop " +
			"databases on, which packages/envoy/scripts/session-titles-scale.sh names; CI never sets it")
	}
	newest, err := strconv.ParseInt(os.Getenv(sessionTitlesScaleNewestEnv), 10, 64)
	if err != nil || newest <= 0 {
		t.Fatalf("%s = %q: want production's newest event id on the day of the run, read through GET /api/v1/events/log",
			sessionTitlesScaleNewestEnv, os.Getenv(sessionTitlesScaleNewestEnv))
	}
	floor := max(int64(sessionTitlesScaleMinEvents), int64(math.Ceil(1.25*float64(newest))))
	load := floor
	if raw := os.Getenv(sessionTitlesScaleLoadEnv); raw != "" {
		if load, err = strconv.ParseInt(raw, 10, 64); err != nil || load <= 0 {
			t.Fatalf("%s = %q: want a count of events above 0", sessionTitlesScaleLoadEnv, raw)
		}
	}
	admin, err := url.Parse(server)
	if err != nil {
		t.Fatalf("parse %s: %v", sessionTitlesScaleURLEnv, err)
	}
	admin.Path = "/postgres"
	for _, field := range strings.Fields(cmp.Or(os.Getenv(sessionTitlesScaleSessionsEnv), sessionTitlesScaleSessionsDefault)) {
		sessions, err := strconv.Atoi(field)
		if err != nil || sessions <= 0 {
			t.Fatalf("%s holds %q: want session counts above 0", sessionTitlesScaleSessionsEnv, field)
		}
		t.Run(fmt.Sprintf("%d sessions", sessions), func(t *testing.T) {
			runSessionTitlesBootScale(t, *admin, newest, floor, load, sessions)
		})
	}
}

// runSessionTitlesBootScale is one run of TestSessionTitlesBootScale, over sessions sessions.
func runSessionTitlesBootScale(t *testing.T, admin url.URL, newest, floor, load int64, sessions int) {
	ctx := context.Background()
	name := "dispatch_titles_scale_" + randomDatabaseSuffix(t)
	adminExec(t, admin, "create database "+name+" strategy file_copy")
	t.Cleanup(func() { adminExec(t, admin, "drop database if exists "+name+" with (force)") })
	// JIT off, as on production's Cloud SQL, so the boot this measures runs its statements as
	// production's does (testdb.WithoutJIT; docs/testing.md, "A database a Go test creates").
	adminExec(t, admin, testdb.WithoutJIT(name))
	database, err := Open(ctx, databaseOn(admin, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(database.Pool.Close)

	// 1. Every shipped migration but the four, in version order, as the runner applies them.
	started := time.Now()
	migrations := shippedMigrations(t)
	if err := database.prepareVersionTable(ctx, migrations); err != nil {
		t.Fatalf("prepare the version table: %v", err)
	}
	left := 0
	for _, migration := range migrations {
		if slices.Contains(sessionTitlesSliceVersions, migration.Version) {
			left++
			continue
		}
		if err := database.applyMigration(ctx, migrations, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	if left != len(sessionTitlesSliceVersions) {
		t.Fatalf("the shipped set holds %d of the slice's %d migrations %v", left, len(sessionTitlesSliceVersions), sessionTitlesSliceVersions)
	}
	t.Logf("migrated every shipped migration but the slice's four in %s", time.Since(started).Round(time.Millisecond))

	// 2. The issues and the events.
	started = time.Now()
	writeSessionTitlesScaleStore(t, database, load, sessions)
	t.Logf("wrote %d issues and %d events over %d sessions in %s", sessionTitlesScaleIssues, load, sessions, time.Since(started).Round(time.Millisecond))

	// 3. The state autovacuum leaves production's tables in, written to disk before the span.
	started = time.Now()
	for _, statement := range []string{`vacuum (analyze) events`, `vacuum (analyze) issues`, `checkpoint`} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Logf("vacuumed, analyzed and checkpointed in %s", time.Since(started).Round(time.Millisecond))

	// The heap the fill reads, and the TOAST table beside it, which a payload too wide for its row
	// moves to: it stays near empty, so each heap row carries its whole payload.
	var events, heapBytes, toastBytes, payloadBytes, payloadStored int64
	if err := database.Pool.QueryRow(ctx, `
		select count(*), pg_relation_size('events'),
		       (select coalesce(pg_total_relation_size(nullif(reltoastrelid, 0)), 0) from pg_class where oid = 'events'::regclass),
		       coalesce(avg(octet_length(payload::text)), 0)::bigint, coalesce(avg(pg_column_size(payload)), 0)::bigint
		from events
	`).Scan(&events, &heapBytes, &toastBytes, &payloadBytes, &payloadStored); err != nil {
		t.Fatalf("count the events: %v", err)
	}
	applied := sliceVersionsApplied(t, database)
	if applied != 0 {
		t.Fatalf("%d of the slice's migrations are applied before the span; want none", applied)
	}

	// 4. The span: the boot's Migrate, which finds the four alone unapplied, then the fill.
	spanStart := time.Now()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	migrated := time.Since(spanStart)
	written, err := database.FillSessionTitles(ctx)
	if err != nil {
		t.Fatalf("FillSessionTitles: %v", err)
	}
	span := time.Since(spanStart)
	fill := span - migrated

	var stored int
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from session_titles), exists(select 1 from session_titles_backfill)
	`).Scan(&stored, &owed); err != nil {
		t.Fatalf("count session_titles: %v", err)
	}
	applied = sliceVersionsApplied(t, database)

	node := cmp.Or(os.Getenv("NODE_NAME"), "unknown")
	var version string
	_ = database.Pool.QueryRow(ctx, `select current_setting('server_version')`).Scan(&version)
	verdict := "PASS"
	var failures []string
	if events < floor {
		failures = append(failures, fmt.Sprintf("the store holds %d events, fewer than R4's floor of %d", events, floor))
	}
	if applied != len(sessionTitlesSliceVersions) {
		failures = append(failures, fmt.Sprintf("Migrate applied %d of the slice's %d migrations", applied, len(sessionTitlesSliceVersions)))
	}
	if stored != sessions || int(written) != sessions || owed {
		failures = append(failures, fmt.Sprintf("the fill wrote %d rows and session_titles holds %d of %d sessions, its progress row left: %t",
			written, stored, sessions, owed))
	}
	if span > sessionTitlesScaleBound {
		failures = append(failures, fmt.Sprintf("the span took %s, longer than R4's %s", span.Round(time.Millisecond), sessionTitlesScaleBound))
	}
	if len(failures) > 0 {
		verdict = "FAIL"
	}
	fmt.Printf("session-titles-scale: %s: node %s, %d CPUs, PostgreSQL %s; production's newest event id %d, floor %d; "+
		"events %d (heap %d MiB, TOAST %d MiB, payload %d bytes as text and %d stored, on average), sessions %d, issues %d; "+
		"migrations %.3f s, fill %.3f s, span %.3f s of %s, margin %.3f s\n",
		verdict, node, runtime.NumCPU(), version, newest, floor,
		events, heapBytes>>20, toastBytes>>20, payloadBytes, payloadStored, sessions, sessionTitlesScaleIssues,
		migrated.Seconds(), fill.Seconds(), span.Seconds(), sessionTitlesScaleBound, (sessionTitlesScaleBound - span).Seconds())
	for _, failure := range failures {
		t.Error(failure)
	}
}

// sliceVersionsApplied is how many of the slice's four migrations schema_migrations records.
func sliceVersionsApplied(t *testing.T, database *Store) int {
	t.Helper()
	var applied int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from schema_migrations where version = any($1)`, sessionTitlesSliceVersions).Scan(&applied); err != nil {
		t.Fatalf("read the slice's migrations from schema_migrations: %v", err)
	}
	return applied
}

// writeSessionTitlesScaleStore writes the run's projects and issues, and load events spread evenly
// over sessions sessions. Every event is a comment.created event of one of the issues, by a
// session actor whose origin stamps a title, so the fill reads every one of them; each session is
// retitled now and then, so the newest title is not every event's. Each event's payload is a JSON
// object of about 1.7 KB, whose body is hex digests: text pglz cannot shrink by the quarter it
// needs to compress, so the payload is stored as wide as it is written. That keeps each row just
// under the 2 KB at which Postgres moves a row's widest value out of the heap
// (TOAST_TUPLE_THRESHOLD): a payload of production's mean of about 2 KB makes no heap row wider
// than this, and a wider one would leave the heap, so the fill, which reads each event's heap row,
// reads no fewer pages here than over production's events. created_at runs forward with the
// event's id, a second apart, ending now.
func writeSessionTitlesScaleStore(t *testing.T, database *Store, load int64, sessions int) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee)
		select 'SCALE' || p, 'Scale ' || p, 'alice' from generate_series(1, $1::int) p
	`, sessionTitlesScaleProjects); err != nil {
		t.Fatalf("write the projects: %v", err)
	}
	// Issue n of the run is number n of project n mod the project count. The first 161 are open
	// P0 issues and the next 540 done P0 issues, production's counts of 2026-11-23.
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, priority, status)
		select 'SCALE' || (n % $2::int + 1) || '-' || n, 'SCALE' || (n % $2::int + 1), n, 'Scale issue ' || n,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0'),
		       case when n <= 701 then 0 else 2 end,
		       case when n between 162 and 701 then 'done' else 'todo' end
		from generate_series(1, $1::int) n
	`, sessionTitlesScaleIssues, sessionTitlesScaleProjects); err != nil {
		t.Fatalf("write the issues: %v", err)
	}
	for from := int64(1); from <= load; from += sessionTitlesScaleBatch {
		to := min(from+sessionTitlesScaleBatch-1, load)
		if _, err := database.Pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
			select 'SCALE' || ((n - 1) % $3::int + 1) % $4::int + 1 || '-' || ((n - 1) % $3::int + 1),
			       (n - 1) / $3::int + 1,
			       'comment.created',
			       jsonb_build_object('kind', 'session', 'id', 'ses_scale_' || lpad(((n - 1) % $5::int)::text, 6, '0'),
			                          'origin', jsonb_build_object('session_title',
			                              'Scale session ' || ((n - 1) % $5::int) || ' title ' || ((n - 1) / $5::int / 7))),
			       jsonb_build_object('comment_id', md5(n::text), 'issue_key', 'SCALE-' || n, 'mentions', '[]'::jsonb,
			                          'body', (select string_agg(md5(n::text || ':' || i), '') from generate_series(1, 50) i)),
			       false,
			       now() - make_interval(secs => ($6::bigint - n)::double precision),
			       now() - make_interval(secs => ($6::bigint - n)::double precision)
			from generate_series($1::bigint, $2::bigint) n
			order by n
		`, from, to, sessionTitlesScaleIssues, sessionTitlesScaleProjects, sessions, load); err != nil {
			t.Fatalf("write events %d to %d: %v", from, to, err)
		}
	}
}
