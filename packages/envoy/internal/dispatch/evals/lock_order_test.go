package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The registry's writes and the refresher's record their changes in the change log's outbox
// (delivery_changes_pending, migration 20261117100001) and take no change log lock: each write's
// drain moves a batch of them into the log in a short transaction of its own after it commits
// (delivery.DrainPendingBatch). Before that, each took the change log's lock (6900) in a trigger
// in the middle of its transaction, and then each took it first, before any other lock; in the
// first order two writers waited on each other in a cycle. The tests below run two writers at once
// in the orders a review traced to such a deadlock, each through the call its route or the
// refresher makes, with the first writer paused, by a trigger of the test's own database, at the
// step after which the old order waited on the second.

// pauseKey is the advisory lock a paused insert waits for (pauseInserts).
const pauseKey = int64(6900_4815)

// lockOrderBound is how long a test waits for a writer before it counts the wait as stuck.
const lockOrderBound = time.Minute

// pauseInserts makes every insert into table of a row when holds wait, before the row is written,
// for a lock the answered transaction holds: the test commits it to let the insert on. The
// trigger is the test database's alone.
func pauseInserts(t *testing.T, r rig, table, when string) pgx.Tx {
	t.Helper()
	ctx := t.Context()
	for _, statement := range []string{
		`create or replace function evals_test_pause() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(tg_argv[0]::bigint);
			return new;
		end $$`,
		fmt.Sprintf(`create trigger evals_test_pause_%s before insert on %s for each row when (%s)
			execute function evals_test_pause('%d')`, table, table, when, pauseKey),
	} {
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("pause the inserts into %s: %v", table, err)
		}
	}
	holder, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the pause: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, pauseKey); err != nil {
		t.Fatalf("take the pause's lock: %v", err)
	}
	return holder
}

// race runs first until it waits on the pause holder holds, then second until it waits too,
// behind the pause or behind first, or answers, then lets first on and answers both writers'
// errors. A second writer that needs no lock first holds answers at once: in the old orders it
// waited on the change log's lock, which no eval writer takes any more. Each writer runs under
// lockOrderBound, so a wait that never ends answers the deadline. Each of whileWaiting runs once
// the second writer waits or answered, before first is let on: what a test observes of the two
// writers' waits.
func race(t *testing.T, holder pgx.Tx, first, second func(ctx context.Context) error, whileWaiting ...func()) (error, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), lockOrderBound)
	defer cancel()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- first(ctx) }()
	storetest.WaitForLockWaiters(t, holder, 1)
	go func() { secondDone <- second(ctx) }()
	var secondErr error
	secondAnswered := false
	testwait.UntilPolling(t, 10*time.Millisecond, "the second writer to wait, behind the pause or the first writer, or to answer (the writers waiting, last read)", func() (int, bool) {
		select {
		case secondErr = <-secondDone:
			secondAnswered = true
			return 0, true
		default:
		}
		waiting, err := storetest.LockWaiters(t.Context(), holder)
		if err != nil {
			t.Fatal(err)
		}
		return waiting, waiting >= 2
	})
	for _, observe := range whileWaiting {
		observe()
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("let the first writer on: %v", err)
	}
	if !secondAnswered {
		secondErr = <-secondDone
	}
	return <-firstDone, secondErr
}

// requireNoDeadlock fails the test where err is not nil, saying where it is Postgres's deadlock.
func requireNoDeadlock(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "40P01":
		t.Errorf("%s deadlocked (SQLSTATE 40P01): %v", what, err)
	case err != nil:
		t.Errorf("%s: %v", what, err)
	}
}

// TestAReadingsWriteAndTheZeroFillOfItsDayNeverDeadlock: a reading of a logged eval on a day that
// holds no row of its series yet, and the refresher's fill of that eval's zero rows through the
// same day, at once. The readings write is paused at its first day row's insert, which in the old
// order came after its readings' insert had taken the change log's lock; the fill then inserted
// the day's zero row and waited for that lock, and the readings write, let on, waited for the
// fill's uncommitted row: a deadlock. Both now land: the day holds the reading, and the days around
// it zero rows.
func TestAReadingsWriteAndTheZeroFillOfItsDayNeverDeadlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	slug := "fill-race"
	r.write(t, slug, alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 2, "measured_at": at(4, 9, 0)}))
	holder := pauseInserts(t, r, "eval_days", "new.built_by = 'readings-race'")
	readingsErr, fillErr := race(t, holder,
		func(ctx context.Context) error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: "readings-race",
				Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "b", "value": 5, "measured_at": at(6, 9, 0)})}})
			return err
		},
		func(ctx context.Context) error {
			return r.registry.newRefresh("fill-race", at(7, 12, 0), walkBatch).fillLogged(ctx, r.project, nil)
		})
	requireNoDeadlock(t, "the readings write", readingsErr)
	requireNoDeadlock(t, "the zero fill", fillErr)
	if t.Failed() {
		return
	}
	evalID := EvalID(r.project, slug)
	for _, want := range []struct {
		day time.Time
		n   int
	}{{at(4, 0, 0), 1}, {at(5, 0, 0), 0}, {at(6, 0, 0), 1}, {at(7, 0, 0), 0}} {
		if got := r.day(t, evalID, "minutes", "", want.day); got.N != want.n {
			t.Errorf("the row of %s holds n %d, want %d", want.day.Format(time.DateOnly), got.N, want.n)
		}
	}
}

// collateSeriesAsADeploymentDoes holds eval_days' series under ICU's en-US collation, which is
// what this deployment's server collates text by (prioritystore's rankKey), so the test sees two
// orders apart whatever collation the server it runs against was created with: under it `latency`
// sorts before `Queue`, and their bytes put `Queue` first.
func collateSeriesAsADeploymentDoes(t *testing.T, r rig) {
	t.Helper()
	ctx := t.Context()
	// A PostgreSQL built with ICU carries ICU collations of its own and one built without ICU
	// carries none, so this count is the server's own answer to whether the collation below can
	// be made. An error from the read itself ends the test.
	var icu bool
	if err := r.database.Pool.QueryRow(ctx, `select count(*) > 0 from pg_collation where collprovider = 'i'`).Scan(&icu); err != nil {
		t.Fatalf("ask whether this Postgres was built with ICU: %v", err)
	}
	if !icu {
		t.Skip("a PostgreSQL built with ICU must serve this test: ICU is what holds two series names in an order their bytes do not follow")
	}
	for _, statement := range []string{
		`create collation deployment_text (provider = icu, locale = 'en-US')`,
		`alter table eval_days alter column series type text collate deployment_text`,
	} {
		// The server has ICU, so a statement it refuses here is a permission or a schema problem
		// rather than an absence to pass over, and the test says so rather than skips.
		if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("hold the series under a deployment's own collation (%s): %v", statement, err)
		}
	}
	var apart bool
	if err := r.database.Pool.QueryRow(ctx, `
		select 'latency' collate deployment_text < 'Queue' collate deployment_text and 'Queue' collate "C" < 'latency' collate "C"
	`).Scan(&apart); err != nil {
		t.Fatalf("compare the two series under the collation and by bytes: %v", err)
	}
	if !apart {
		t.Fatal("this server's en-US collation does not sort `latency` before `Queue`, so the orders the test races would be one order")
	}
}

// TestAReadingsWriteAndTheZeroFillOfTwoSeriesNeverDeadlock: the race above over two series of one
// logged eval whose byte order and collation order differ, `Queue` and `latency`, held under the
// deployment's collation. Each has a row on the 4th and none after it; the readings write carries
// a reading of each on the 6th, and the fill writes each series' zero rows through the 7th. The
// readings write is paused at its insert of latency's row of the 6th, having inserted Queue's,
// since it takes the rows by their bytes.
//
// It pins that the two never deadlock, by either of two guards. Every writer of an eval's day rows
// takes the eval's lock first (lockEval, lockEvals), so the fill waits for the readings write to
// commit before it reads or inserts a row; and every statement that inserts or locks those rows
// takes them in the byte order of their keys (writeDays), so two writers that did run side by side
// would take them in one order. With the lock taken the second guard is never reached, so the
// deadlock this test looks for comes back only once both are gone: the fill, taking its rows in
// the collation's order, inserted latency's rows of the 5th to the 7th, then Queue's of the 5th,
// and waited for the readings write's uncommitted Queue row of the 6th, and the readings write,
// let on, waited for the fill's latency row of the 6th. What it observes of the first guard alone
// is that the fill waited on the eval's advisory lock while the paused readings write held it
// (requireQueuedOnTheEvalLock), which fails once the fill stops taking the lock. Both land: each
// series holds its readings on the 4th and the 6th and zero rows on the 5th and the 7th.
func TestAReadingsWriteAndTheZeroFillOfTwoSeriesNeverDeadlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	collateSeriesAsADeploymentDoes(t, r)
	slug := "fill-order"
	evalID := EvalID(r.project, slug)
	reading := func(series, key string, value float64, day int) json.RawMessage {
		return row(t, map[string]any{"series": series, "row_kind": "run", "row_key": key, "value": value, "measured_at": at(day, 9, 0)})
	}
	r.write(t, slug, alice, reading("Queue", "a", 1, 4), reading("latency", "a", 2, 4))
	holder := pauseInserts(t, r, "eval_days", "new.built_by = 'readings-race' and new.series = 'latency'")
	readingsErr, fillErr := race(t, holder,
		func(ctx context.Context) error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: "readings-race",
				Rows: []json.RawMessage{reading("Queue", "b", 3, 6), reading("latency", "b", 4, 6)}})
			return err
		},
		func(ctx context.Context) error {
			return r.registry.newRefresh("fill-race", at(7, 12, 0), walkBatch).fillLogged(ctx, r.project, nil)
		},
		func() { requireQueuedOnTheEvalLock(t, holder, evalID, "the zero fill") })
	requireNoDeadlock(t, "the readings write", readingsErr)
	requireNoDeadlock(t, "the zero fill", fillErr)
	if t.Failed() {
		return
	}
	for _, series := range []string{"Queue", "latency"} {
		for _, want := range []struct {
			day time.Time
			n   int
		}{{at(4, 0, 0), 1}, {at(5, 0, 0), 0}, {at(6, 0, 0), 1}, {at(7, 0, 0), 0}} {
			if got := r.day(t, evalID, series, "", want.day); got.N != want.n {
				t.Errorf("the row of %s on %s holds n %d, want %d", series, want.day.Format(time.DateOnly), got.N, want.n)
			}
		}
	}
}

// requireQueuedOnTheEvalLock fails the test unless a backend waits, not granted, on the advisory
// lock of evalID (the key lockEvals takes, hashtext('eval:' || id) as a bigint, which pg_locks
// shows as its high half in classid, its low half in objid and objsubid 1) behind a backend that
// holds it and itself waits behind holder: a race's second writer, what names, queued on the eval's
// lock behind the paused first. It reads pg_locks through holder's own connection, as
// storetest.WaitForLockWaiters does.
func requireQueuedOnTheEvalLock(t *testing.T, holder pgx.Tx, evalID, what string) {
	t.Helper()
	var queued bool
	if err := holder.QueryRow(context.Background(), `
		select exists (
			select 1
			from pg_locks waiter
			join pg_locks held on held.locktype = waiter.locktype and held.database = waiter.database
			  and held.classid = waiter.classid and held.objid = waiter.objid and held.objsubid = waiter.objsubid
			where waiter.locktype = 'advisory' and not waiter.granted and held.granted
			  and waiter.classid = ((hashtext('eval:' || $1)::bigint >> 32) & 4294967295)::oid
			  and waiter.objid = (hashtext('eval:' || $1)::bigint & 4294967295)::oid
			  and waiter.objsubid = 1
			  and held.pid = any(pg_blocking_pids(waiter.pid))
			  and pg_backend_pid() = any(pg_blocking_pids(held.pid)))
	`, evalID).Scan(&queued); err != nil {
		t.Fatalf("read the waits on the lock of %s: %v", evalID, err)
	}
	if !queued {
		t.Errorf("%s did not wait on the advisory lock of %s while the paused writer held it", what, evalID)
	}
}

// A register write that lists or unlists a logged eval's series writes the new listing into every
// day row the series holds (relistDays), and every other writer of those rows takes the eval's
// lock before it reads the listing it writes into them, so none writes the listing the register
// write replaced once that write has committed. The two tests below pause such a writer after its
// read of the listing while a register write unlists the series. Before, the paused writer held no
// lock of the eval: the register write unlisted the stored rows and committed beside it, and the
// writer, let on, wrote the old listing into the rows it wrote, which a window then folded.

// listDepth is a register write that lists or unlists the depth series of slug.
func listDepth(r rig, slug string, listed bool) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: slug, Caller: alice,
			Series: []SeriesChange{{Series: "depth", Listed: Set(listed)}}})
		return err
	}
}

// requireDepthRows fails the test unless the depth series of slug holds want day rows, every one
// of them unlisted.
func requireDepthRows(t *testing.T, r rig, slug string, want int) {
	t.Helper()
	evalID := EvalID(r.project, slug)
	rows := r.count(t, `select count(*) from eval_days where eval_id = $1 and series = 'depth'`, evalID)
	listed := r.count(t, `select count(*) from eval_days where eval_id = $1 and series = 'depth' and listed`, evalID)
	if rows != want || listed != 0 {
		t.Errorf("depth holds %d day rows, %d of them listed, after the register write unlisted it; want %d, none listed", rows, listed, want)
	}
}

// TestARegisterWriteThatUnlistsASeriesBesideAReadingsWriteUnlistsEveryRow: a readings write of a
// listed series, one reading on a day that holds rows of it and one on a day that holds none, each
// counted in the project's row and its repository's, is paused at its readings' insert, after it
// read the series, while a register write unlists the series. The register write now waits for
// the readings write and then unlists every row, the ones the readings write made among them.
func TestARegisterWriteThatUnlistsASeriesBesideAReadingsWriteUnlistsEveryRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	slug := "queue-depth"
	depth := func(key string, day int) json.RawMessage {
		return row(t, map[string]any{"series": "depth", "row_kind": "queue", "row_key": key, "value": float64(day), "measured_at": at(day, 9, 0),
			"attributes": map[string]any{"repo": "acme/web"}})
	}
	r.write(t, slug, alice, depth("a", 4), depth("b", 5))
	holder := pauseInserts(t, r, "eval_readings", "new.row_key = 'paused'")
	readingsErr, registerErr := race(t, holder,
		func(ctx context.Context) error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
				Rows: []json.RawMessage{depth("paused", 4), depth("c", 6)}})
			return err
		},
		listDepth(r, slug, false))
	requireNoDeadlock(t, "the readings write", readingsErr)
	requireNoDeadlock(t, "the register write", registerErr)
	if t.Failed() {
		return
	}
	requireDepthRows(t, r, slug, 6)
	if day := r.day(t, EvalID(r.project, slug), "depth", "", at(4, 0, 0)); day.N != 2 {
		t.Errorf("the 4th holds n %d, want both its readings", day.N)
	}
}

// TestARegisterWriteThatUnlistsASeriesBesideTheZeroFillUnlistsEveryRow: the refresher's fill of a
// listed series' zero rows through the 7th is paused at its first zero row's insert, after it read
// the listing of the series' newest row, while a register write unlists the series. The fill now
// holds the eval's lock from before that read, so the register write waits for it and then
// unlists the zero rows with the rest.
func TestARegisterWriteThatUnlistsASeriesBesideTheZeroFillUnlistsEveryRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	slug := "queue-depth"
	r.write(t, slug, alice, row(t, map[string]any{"series": "depth", "row_kind": "queue", "row_key": "a", "value": 4, "measured_at": at(4, 9, 0)}))
	holder := pauseInserts(t, r, "eval_days", "new.built_by = 'fill-race'")
	fillErr, registerErr := race(t, holder,
		func(ctx context.Context) error {
			return r.registry.newRefresh("fill-race", at(7, 12, 0), walkBatch).fillLogged(ctx, r.project, nil)
		},
		listDepth(r, slug, false))
	requireNoDeadlock(t, "the zero fill", fillErr)
	requireNoDeadlock(t, "the register write", registerErr)
	if t.Failed() {
		return
	}
	requireDepthRows(t, r, slug, 4)
}

// TestAFirstReadingAndARegisterWriteWithDocTextNeverDeadlock: a register write that replaces an
// eval's method and a first reading under another slug of the same project, at once. The register
// write is paused at its version's insert, after it locked the project's evals-md to write the
// method; in the old order the first reading had then taken the change log's lock with its own
// version 1 and waited for evals-md to write its section, and the register write, let on, waited
// for the change log's lock: a deadlock. Both now land, each section in evals-md.
func TestAFirstReadingAndARegisterWriteWithDocTextNeverDeadlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.registry.Register(t.Context(), Registration{Project: r.project, Slug: "review-time", Caller: alice, Name: Set("Review time")}); err != nil {
		t.Fatalf("register review-time: %v", err)
	}
	holder := pauseInserts(t, r, "eval_versions", "new.written_via = 'register'")
	registerErr, readingErr := race(t, holder,
		func(ctx context.Context) error {
			_, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: "review-time", Caller: alice, DocText: Set("Alice's method: time each review.")})
			return err
		},
		func(ctx context.Context) error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "deploy-wait", Caller: alice, BuiltBy: testBuild,
				Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 3, "measured_at": at(5, 9, 0)})}})
			return err
		})
	requireNoDeadlock(t, "the register write", registerErr)
	requireNoDeadlock(t, "the first reading", readingErr)
	if t.Failed() {
		return
	}
	text, _ := r.document(t)
	if headings(text, "review-time") != 1 || headings(text, "deploy-wait") != 1 || !strings.Contains(text, "Alice's method: time each review.") {
		t.Errorf("evals-md does not hold one section of each eval with alice's method:\n%s", text)
	}
}

// TestAFirstReadingAndTheSeedNeverDeadlock: the seed of a project whose evals-md lost one
// catalogue method block, and a first reading under a slug of the same project, at once. The seed
// is paused at its versions' insert, after it locked evals-md to add the missing section; in the
// old order the first reading had then taken the change log's lock with its version 1 and waited
// for evals-md, and the seed, let on, waited for the change log's lock: a deadlock. Both now land:
// deploy-frequency's newest version names the block the seed added, and deploy-wait has its
// section.
func TestAFirstReadingAndTheSeedNeverDeadlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.registry.Ensure(t.Context(), r.project); err != nil {
		t.Fatalf("seed %s: %v", r.project, err)
	}
	definition, _ := r.currentOf(t, SlugDeployFrequency)
	r.deleteBlock(t, r.documentID(t, r.project), definition.DocBlock)
	holder := pauseInserts(t, r, "eval_versions", "new.written_via = 'seed'")
	seedErr, readingErr := race(t, holder,
		func(ctx context.Context) error {
			_, err := r.registry.Ensure(ctx, r.project)
			return err
		},
		func(ctx context.Context) error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "deploy-wait", Caller: alice, BuiltBy: testBuild,
				Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 3, "measured_at": at(5, 9, 0)})}})
			return err
		})
	requireNoDeadlock(t, "the seed", seedErr)
	requireNoDeadlock(t, "the first reading", readingErr)
	if t.Failed() {
		return
	}
	text, blocks := r.document(t)
	restored, _ := r.currentOf(t, SlugDeployFrequency)
	if !hasBlock(blocks, restored.DocBlock) || restored.DocBlock == definition.DocBlock || headings(text, "deploy-wait") != 1 {
		t.Errorf("deploy-frequency's doc_block %s is no block of evals-md, or is the deleted one, or deploy-wait has no one section:\n%s", restored.DocBlock, text)
	}
}

// TestFirstRegistrationsBesideAnEditOfEvalsMdNeitherDeadlockNorLoseASection: five first
// registrations of one project, three by a reading and two by a register write, and a person's
// edit of the project's evals-md, at once. A registration takes its eval's lock, then the
// document's owner lock (lockDocument, store.LockOwner), does the section's document work and
// stores its rows, and its commit sends the document's queued writes; the edit takes the owner
// lock first, as the edit route does (requireOpenOwnerStatus), then the document's other locks in
// the docs package's order (the writer slot, the advisory lock). The first registration is paused
// in its commit, at the insert of the document's version, holding all of them; the edit then
// waits on the owner lock; and the four other registrations each wait on it too, holding their
// eval's lock and no transaction id, so none of them holds back the event horizon while it waits
// (requireQueuedWithNoTransactionID). Each writer runs on a context marked as a route marks its
// request (store.WithTransactionTracking), so a second connection of the pool asked for inside
// its transaction is refused, as the server refuses it. Let on, every write lands: evals-md holds
// the edit's paragraph and one section of each eval, which its doc_block names.
func TestFirstRegistrationsBesideAnEditOfEvalsMdNeitherDeadlockNorLoseASection(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "deploy-wait", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 1, "measured_at": at(4, 9, 0)}))
	documentID := r.documentID(t, r.project)
	firstRegistrations{r: r, documentID: documentID}.run(t, "review-time", "build-minutes", "merge-wait", "flaky-reruns")
}

// TestAFirstRegistrationBesideAnEarlierBuildNeitherDeadlocksNorLosesASection is the race above in
// the owner lock's rollout mode: a process of a build before the owner lock is in the fleet (a
// server_processes row at owner lock protocol 0, registerEarlierBuildProcess), which serialises
// the document's writers on its row alone, so every owner lock of this build takes the document's
// row too, right after the advisory lock (store.LockOwner). The paused registration holds the
// document's row from its read phase, which an earlier build's writer of the document waits on;
// the edit and two other first registrations, a register write and a reading, wait on the
// advisory lock before they take the row, still holding no transaction id. Let on, every write
// lands, none deadlocked, and evals-md holds the edit's paragraph and one section of each eval.
func TestAFirstRegistrationBesideAnEarlierBuildNeitherDeadlocksNorLosesASection(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "deploy-wait", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 1, "measured_at": at(4, 9, 0)}))
	documentID := r.documentID(t, r.project)
	registerEarlierBuildProcess(t, r)
	firstRegistrations{r: r, documentID: documentID, earlierBuild: true}.run(t, "review-time", "build-minutes")
}

// registerEarlierBuildProcess writes a server_processes row as a process of a build before the
// owner lock protocol leaves it, as the api package's rollout tests do: no owner_lock_protocol, so
// the column's default 0, which keeps that build in the fleet for store.OwnerLockLegacyWindow.
func registerEarlierBuildProcess(t *testing.T, r rig) {
	t.Helper()
	if _, err := r.database.Pool.Exec(t.Context(), `
		insert into server_processes (instance, commit, signing_keys)
		values (gen_random_uuid(), 'earlier0build', '{fingerprint}')`); err != nil {
		t.Fatalf("register a process of an earlier build: %v", err)
	}
}

// firstRegistrations is the race of first registrations beside an edit of the project's
// evals-md, documentID, which already holds deploy-wait's section: cache-hits' reading, paused at
// the insert of the document's version, the edit, and a first registration of each slug run is
// given, queued behind the two, alternately a register write and a reading. earlierBuild says a
// process of an earlier build is in the fleet, so the paused registration holds the document's row.
type firstRegistrations struct {
	r            rig
	documentID   string
	earlierBuild bool
}

func (f firstRegistrations) run(t *testing.T, queuedSlugs ...string) {
	t.Helper()
	r := f.r
	reading := func(slug string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			written, err := r.registry.WriteReadings(store.WithTransactionTracking(ctx), Batch{Project: r.project, Slug: slug, Caller: alice, BuiltBy: testBuild,
				Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 3, "measured_at": at(5, 9, 0)})}})
			if err == nil && !written.Registered {
				err = fmt.Errorf("the first reading under %s registered no eval", slug)
			}
			return err
		}
	}
	register := func(slug string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := r.registry.Register(store.WithTransactionTracking(ctx), Registration{Project: r.project, Slug: slug, Caller: alice, Name: Set("Registered " + slug)})
			return err
		}
	}
	documentID := f.documentID
	const note = "Alice's note: every eval of this project is measured on the main branch."
	edit := func(ctx context.Context) error {
		ctx = store.WithTransactionTracking(ctx)
		tx, err := r.database.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		// The document's owner lock, then a read of its row in the next statement, as the edit
		// route takes them (requireOpenOwnerStatus, store.LockOwnerAndRead).
		if err := store.LockOwnerAndRead(ctx, tx, store.ArtifactOwner(documentID), `
			select true from artifacts where id = $1 and issue_key is null
		`, []any{documentID}, new(bool)); err != nil {
			return fmt.Errorf("lock the document's owner: %w", err)
		}
		joined, ledger := r.documents.Join(ctx, tx)
		defer ledger.Discard()
		applied, err := r.documents.ApplyOps(joined, documentID, []model.EditOp{{Op: "insert", After: "end", Markdown: note}}, alice, nil)
		if err != nil {
			return err
		}
		if !applied.Changed {
			return errors.New("the edit changed nothing")
		}
		if _, err := r.documents.SnapshotVersion(joined, documentID, alice); err != nil {
			return err
		}
		return ledger.Commit(ctx)
	}
	type queuedRegistration struct {
		slug  string
		write func(ctx context.Context) error
	}
	queued := make([]queuedRegistration, len(queuedSlugs))
	for index, slug := range queuedSlugs {
		queued[index] = queuedRegistration{slug, register(slug)}
		if index%2 == 1 {
			queued[index].write = reading(slug)
		}
	}
	answers := make(chan error, len(queued))
	holder := pauseInserts(t, r, "artifact_versions", "true")
	firstErr, editErr := race(t, holder, reading("cache-hits"), edit, func() {
		ctx, cancel := context.WithTimeout(t.Context(), lockOrderBound)
		t.Cleanup(cancel)
		for _, registration := range queued {
			go func() { answers <- registration.write(ctx) }()
		}
		// The paused registration, the edit and every queued registration.
		storetest.WaitForLockWaiters(t, holder, 2+len(queued))
		requireQueuedWithNoTransactionID(t, holder, documentID, len(queued))
		if held := documentRowHeld(t, holder, documentID); held != f.earlierBuild {
			t.Errorf("the paused registration holds evals-md's row: %t; want %t, which an earlier build in the fleet makes every owner lock take (store.LockOwner)", held, f.earlierBuild)
		}
	})
	requireNoDeadlock(t, "the paused registration", firstErr)
	requireNoDeadlock(t, "the edit", editErr)
	for range queued {
		requireNoDeadlock(t, "a queued registration", <-answers)
	}
	if t.Failed() {
		return
	}
	text, blocks := r.document(t)
	if !strings.Contains(text, note) {
		t.Errorf("evals-md lost the edit's paragraph:\n%s", text)
	}
	for _, slug := range append([]string{"deploy-wait", "cache-hits"}, queuedSlugs...) {
		definition, _ := r.currentOf(t, slug)
		if headings(text, slug) != 1 || !hasBlock(blocks, definition.DocBlock) {
			t.Errorf("evals-md holds %d sections of %s, and its doc_block %q names a block of it: %t; want one, naming one",
				headings(text, slug), slug, definition.DocBlock, hasBlock(blocks, definition.DocBlock))
		}
	}
}

// requireQueuedWithNoTransactionID fails the test unless want backends that each hold an eval's
// lock (lockEval, a single-key advisory lock, which pg_locks shows with objsubid 1) wait, not
// granted, on the owner lock of the document documentID (store.LockOwner: the two-key advisory
// lock store.OwnerLockNamespace and the hashtext of the owner's lock key, which pg_locks shows as
// classid, objid and objsubid 2), and no backend waiting on it holds a transaction id: a backend
// that has one holds the lock on it, which pg_locks shows as a granted transactionid lock. The
// edit waits on the same lock holding no eval lock, and is not counted. It reads pg_locks through
// holder's own connection, as storetest.WaitForLockWaiters does, and only the test database's.
func requireQueuedWithNoTransactionID(t *testing.T, holder pgx.Tx, documentID string, want int) {
	t.Helper()
	var queued, withID int
	if err := holder.QueryRow(context.Background(), `
		select
			count(*) filter (where exists (
				select 1 from pg_locks own
				where own.pid = waiter.pid and own.locktype = 'advisory' and own.granted and own.objsubid = 1)),
			count(*) filter (where exists (
				select 1 from pg_locks own where own.pid = waiter.pid and own.locktype = 'transactionid' and own.granted))
		from pg_locks waiter
		where waiter.locktype = 'advisory' and not waiter.granted
		  and waiter.database = (select oid from pg_database where datname = current_database())
		  and waiter.classid = ($1::bigint & 4294967295)::oid
		  and waiter.objid = (hashtext($2)::bigint & 4294967295)::oid
		  and waiter.objsubid = 2
	`, int64(store.OwnerLockNamespace), store.ArtifactOwner(documentID).LockKey()).Scan(&queued, &withID); err != nil {
		t.Fatalf("read the waits on the owner lock of evals-md %s: %v", documentID, err)
	}
	if queued != want || withID != 0 {
		t.Errorf("%d writes holding an eval's lock wait on the owner lock of evals-md, and %d waiters on it hold a transaction id; want %d and none",
			queued, withID, want)
	}
}

// documentRowHeld reports whether another transaction holds the row of the document documentID
// locked: a `for no key update skip locked` of it from holder's own connection, which answers no
// row then, and does not wait.
func documentRowHeld(t *testing.T, holder pgx.Tx, documentID string) bool {
	t.Helper()
	rows, err := holder.Query(context.Background(), `select 1 from artifacts where id = $1 for no key update skip locked`, documentID)
	if err != nil {
		t.Fatalf("probe the row of evals-md %s: %v", documentID, err)
	}
	defer rows.Close()
	found := rows.Next()
	if err := rows.Err(); err != nil {
		t.Fatalf("probe the row of evals-md %s: %v", documentID, err)
	}
	return !found
}

// collidingIDs is how many eval ids of the shape P/s<n> the test below searches for two whose
// lock keys collide: the key is a 32-bit hash, so about ten pairs collide among 300,000 ids (12 on
// the server the test was written against, found in under a second).
const collidingIDs = 300_000

// TestTwoWritersLockingEvalsWhoseKeysCollideNeverDeadlock: lockEvals over ids whose lock keys
// (hashtext('eval:' || id)) collide. The test finds two ids, a and c, that share a key, and a
// third, b, between them in byte order under another key. While the test holds the shared key,
// one writer locks a and b and queues on it, and then a second locks b and c and queues too; the
// test then lets them on. Taken in the byte order of the ids, the first took the shared key and
// then b's and the second b's and then the shared key, c's: each waited for the other, and
// Postgres ended one of them with 40P01. Taken in key order, both take the two keys in one order,
// and both land.
func TestTwoWritersLockingEvalsWhoseKeysCollideNeverDeadlock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(t.Context(), lockOrderBound)
	defer cancel()
	searched := time.Now()
	var a, b, c string
	var shared, other int32
	if err := database.Pool.QueryRow(ctx, `
		with ids as materialized (
			select 'P/s' || n as id, hashtext('eval:P/s' || n) as key from generate_series(1, $1::integer) n),
		pairs as (
			select min(id collate "C") as a, max(id collate "C") as c, key
			from ids group by key having count(*) = 2)
		select pairs.a, middle.id, pairs.c, pairs.key, middle.key
		from pairs cross join lateral (
			select ids.id, ids.key from ids
			where ids.id collate "C" > pairs.a and ids.id collate "C" < pairs.c and ids.key <> pairs.key
			limit 1) middle
		order by pairs.a
		limit 1
	`, collidingIDs).Scan(&a, &b, &c, &shared, &other); err != nil {
		t.Fatalf("find two of %d eval ids whose lock keys collide, and one between them: %v", collidingIDs, err)
	}
	t.Logf("%s and %s share the key %d and %s between them holds %d, found among %d ids in %s",
		a, c, shared, b, other, collidingIDs, time.Since(searched).Round(time.Millisecond))
	if !(a < b && b < c) {
		t.Fatalf("%s, %s and %s are not in byte order", a, b, c)
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if err := lockEval(ctx, holder, a); err != nil {
		t.Fatalf("hold the key of %s: %v", a, err)
	}
	lock := func(ids ...string) <-chan error {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the writer of %v: %v", ids, err)
		}
		done := make(chan error, 1)
		go func() {
			if err := lockEvals(ctx, tx, ids); err != nil {
				_ = tx.Rollback(context.Background())
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		return done
	}
	first := lock(a, b)
	storetest.WaitForLockWaiters(t, holder, 1)
	second := lock(b, c)
	storetest.WaitForLockWaiters(t, holder, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the writers on: %v", err)
	}
	requireNoDeadlock(t, "the writer of "+a+" and "+b, <-first)
	requireNoDeadlock(t, "the writer of "+b+" and "+c, <-second)
}
