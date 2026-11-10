package cireconcile

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The record each pass leaves in ci_reconcile_passes, and the reading of it the API answers
// (passes.go). The cases here are the ones the issue's verification document names: a pass held
// open, the record outliving the process that wrote it, the prune, a pass its own bound cut
// short, a pass a kill left open, and the liveness rule that decides the last of those when
// nothing else can.

// registerProcess writes the row processes.Register writes for a live server process, with a
// heartbeat of the given age, and answers the instance id. Written here rather than by calling
// Register because these tests need a heartbeat older than the live window, which a registration
// that keeps beating cannot have.
func registerProcess(t *testing.T, pool *store.Pool, heartbeatAge time.Duration) string {
	t.Helper()
	var instance string
	if err := pool.QueryRow(t.Context(), `
		insert into server_processes (instance, heartbeat_at)
		values (gen_random_uuid(), now() - make_interval(secs => $1))
		returning instance::text
	`, heartbeatAge.Seconds()).Scan(&instance); err != nil {
		t.Fatalf("register a server process: %v", err)
	}
	return instance
}

// deregisterProcess deletes a process's registry row, which is what Deregister does once a
// process's HTTP drain has ended, and what a restart leaves behind.
func deregisterProcess(t *testing.T, pool *store.Pool, instance string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from server_processes where instance = $1`, instance); err != nil {
		t.Fatalf("deregister %s: %v", instance, err)
	}
}

// seedPass writes a pass row that was started the given time ago and never ended: what a process
// killed in the middle of a pass leaves behind.
func seedPass(t *testing.T, pool *store.Pool, instance string, startedAgo time.Duration) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
		insert into ci_reconcile_passes (instance, started_at)
		values ($1, now() - make_interval(secs => $2))
		returning id
	`, instance, startedAgo.Seconds()).Scan(&id); err != nil {
		t.Fatalf("seed a pass of %s: %v", instance, err)
	}
	return id
}

// rowCount is how many rows the table holds, whatever their age: what the prune is measured by,
// since RecentPasses reads only the last day and the prune keeps two.
func rowCount(t *testing.T, pool *store.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `select count(*) from ci_reconcile_passes`).Scan(&count); err != nil {
		t.Fatalf("count the pass rows: %v", err)
	}
	return count
}

// recent is the passes of the last day as the API answers them.
func recent(t *testing.T, pool *store.Pool) []PassRecord {
	t.Helper()
	passes, err := RecentPasses(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the recent passes: %v", err)
	}
	return passes
}

// passOf is the row of one id among those, and fails when the answer does not hold it.
func passOf(t *testing.T, passes []PassRecord, id int64) PassRecord {
	t.Helper()
	for _, pass := range passes {
		if pass.ID == id {
			return pass
		}
	}
	t.Fatalf("pass %d is not among the %d the read answered: %+v", id, len(passes), passes)
	return PassRecord{}
}

// holdLease takes the `ci-reconcile` lease exactly as the loop takes it -- duty.Exclusive, on a
// connection of its own -- runs body under it, and holds it until the returned function is
// called or the test ends. taken is closed once the lease is held and before body starts, and
// finished once body has returned; a nil body holds the lease and does nothing, which is what a
// test standing for a session the database has not reaped wants.
func holdLease(t *testing.T, pool *store.Pool, body func(ctx context.Context)) (release func(), taken, finished <-chan struct{}) {
	t.Helper()
	leases, err := pool.Leases()
	if err != nil {
		t.Fatalf("the lease pool: %v", err)
	}
	held := make(chan struct{})
	ran := make(chan struct{})
	let := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.Exclusive(t.Context(), leases, Lease, time.Time{}, 0, func(ctx context.Context) {
			close(held)
			if body != nil {
				body(ctx)
			}
			close(ran)
			<-let
		})
	}()
	var once bool
	release = func() {
		if once {
			return
		}
		once = true
		close(let)
		<-done
	}
	t.Cleanup(release)
	return release, held, ran
}

// waitForPass polls the recent passes until one answers want for id, and fails when it does not
// within the bound. The pass it is waiting on runs in a goroutine of its own, so there is nothing
// to wait on but the row.
func waitForPass(t *testing.T, pool *store.Pool, id int64, want string) PassRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []PassRecord
	for time.Now().Before(deadline) {
		last = recent(t, pool)
		for _, pass := range last {
			if pass.ID == id && pass.State == want {
				return pass
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pass %d never read as %q: %+v", id, want, last)
	return PassRecord{}
}

// waitForRow polls until the table holds a pass of this instance with no end time, which is the
// row a pass writes when it starts, and answers its id.
func waitForRow(t *testing.T, pool *store.Pool, instance string) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var id int64
		err := pool.QueryRow(t.Context(), `
			select id from ci_reconcile_passes where instance = $1 and ended_at is null
			order by started_at desc, id desc limit 1
		`, instance).Scan(&id)
		if err == nil {
			return id
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no open pass row of %s was written", instance)
	return 0
}

// A pass that is still reading GitHub reads as running: its row has no end time, its process is
// live, the lease its loop holds is held, and it is the newest row. This is the whole point of
// writing the row at the start of the pass rather than at its end -- a person who asks why
// nothing has settled wants to know that a pass is running now, not only what the last one did.
//
// The pass here is held open by a GitHub that does not answer the second page of the repository,
// and it is run under its lease exactly as the loop runs it (duty.Exclusive), so the lease the
// read asks about is held by the pass itself.
func TestAPassHeldOpenReadsAsRunning(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(heads(45)...)
	instance := registerProcess(t, h.pool, 0)
	h.reconciler.Instance = func() string { return instance }
	// Long enough that the pass is still reading while the test reads the row, and short enough
	// that the test does not wait on it: GitHub never answers the second page.
	h.reconciler.PassTimeout = 3 * time.Second
	h.github.stallQuery(gitHubRepository, 1, time.Minute)

	_, _, finished := holdLease(t, h.pool, h.reconciler.Pass)
	id := waitForRow(t, h.pool, instance)

	running := waitForPass(t, h.pool, id, PassStateRunning)
	if running.EndedAt != nil {
		t.Errorf("a running pass has an end time of %v", running.EndedAt)
	}
	if running.Counts != nil || running.Cursor != nil {
		t.Errorf("a running pass carries counts %+v and a cursor %+v, want neither until it ends", running.Counts, running.Cursor)
	}
	if running.Instance != instance {
		t.Errorf("the row names instance %q, want this process's %q", running.Instance, instance)
	}

	// The pass ends on its own bound, and the same row then says so.
	<-finished
	ended := passOf(t, recent(t, h.pool), id)
	if ended.State != outcomeDeadline {
		t.Fatalf("the pass ended %q, want %q: %+v", ended.State, outcomeDeadline, ended)
	}
}

// A pass its own bound cut short records its end and says the bound did it. The end write runs
// on a context of its own for exactly this case: the pass's own context is the thing that has
// just expired, and a write made on it would land nowhere, leaving a row that reads as a pass
// that vanished -- the one state that must mean a process died.
func TestAPassCutShortByItsDeadlineRecordsThatItsBoundEndedIt(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(heads(45)...)
	instance := registerProcess(t, h.pool, 0)
	h.reconciler.Instance = func() string { return instance }
	h.reconciler.PassTimeout = 2 * time.Second
	h.github.stallQuery(gitHubRepository, 1, time.Minute)

	h.pass()

	passes := recent(t, h.pool)
	if len(passes) != 1 {
		t.Fatalf("the read answered %d passes, want the one this test ran: %+v", len(passes), passes)
	}
	pass := passes[0]
	if pass.State != outcomeDeadline {
		t.Fatalf("the pass ended %q, want %q: %+v", pass.State, outcomeDeadline, pass)
	}
	if pass.EndedAt == nil {
		t.Fatal("a pass its bound cut short has no end time, so nothing tells it from a pass whose process died")
	}
	if pass.Counts == nil {
		t.Fatal("a pass its bound cut short recorded no counts")
	}
	// The one page it did read, which the log line reports as the same number.
	if pass.Counts.Heads != pullsPerQuery {
		t.Errorf("the row records %d heads, want the %d of the page the pass finished", pass.Counts.Heads, pullsPerQuery)
	}
	if pass.Counts.GraphQLQueries == 0 {
		t.Errorf("the row records no GraphQL queries, and the pass asked for two pages: %+v", pass.Counts)
	}
	if line := h.passLine(); line["heads"] != float64(pass.Counts.Heads) {
		t.Errorf("the row records %d heads and the line %v: the row is the line made durable", pass.Counts.Heads, line["heads"])
	}
	// The cursor the next pass takes up: the page this one did not finish.
	if pass.Cursor == nil || pass.Cursor.Repository != rowRepository {
		t.Errorf("the row's cursor is %+v, want the repository the pass was cut short in", pass.Cursor)
	}
}

// The record outlives the process that wrote it. A restart deletes the process's registry row
// (Deregister) and the next process registers under an id of its own, and the day of passes the
// route answers still holds both -- which is the whole reason the counts are in Postgres rather
// than only in a container log the restart rotated away.
func TestTheRecordSurvivesARestartOfTheProcessThatWroteIt(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	before := registerProcess(t, h.pool, 0)
	h.reconciler.Instance = func() string { return before }

	h.pass()

	// The process stops: its drain ends, it deregisters, and the pod is replaced.
	deregisterProcess(t, h.pool, before)
	after := registerProcess(t, h.pool, 0)
	h.reconciler.Instance = func() string { return after }

	h.pass()

	passes := recent(t, h.pool)
	if len(passes) != 2 {
		t.Fatalf("the read answered %d passes, want the two this test ran: %+v", len(passes), passes)
	}
	// Newest first.
	if passes[0].Instance != after || passes[1].Instance != before {
		t.Fatalf("the passes read %q then %q, want the newest first: %q then %q", passes[0].Instance, passes[1].Instance, after, before)
	}
	for _, pass := range passes {
		if pass.State != outcomeComplete {
			t.Errorf("the pass of %s ended %q, want %q", pass.Instance, pass.State, outcomeComplete)
		}
		if pass.Counts == nil {
			t.Errorf("the pass of %s records no counts", pass.Instance)
		}
	}
}

// Rows past the retention go with the next pass that ends. The prune runs with a pass and
// nowhere else: the passes are the only writer of this table, so there is nothing to tidy
// between two of them.
func TestRowsPastTheRetentionArePrunedByThePassThatEnds(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	instance := registerProcess(t, h.pool, 0)
	h.reconciler.Instance = func() string { return instance }
	old := seedPass(t, h.pool, instance, PassRetention+time.Hour)
	kept := seedPass(t, h.pool, instance, PassRetention-time.Hour)
	if rowCount(t, h.pool) != 2 {
		t.Fatalf("the seed left %d rows, want 2", rowCount(t, h.pool))
	}

	h.pass()

	if got := rowCount(t, h.pool); got != 2 {
		t.Fatalf("the table holds %d rows, want the row within the retention and this pass's own", got)
	}
	var stillThere bool
	if err := h.pool.QueryRow(t.Context(), `select exists (select 1 from ci_reconcile_passes where id = $1)`, old).Scan(&stillThere); err != nil {
		t.Fatalf("read whether the old row is still there: %v", err)
	}
	if stillThere {
		t.Errorf("the row %d, older than the %s retention, was not pruned", old, PassRetention)
	}
	if err := h.pool.QueryRow(t.Context(), `select exists (select 1 from ci_reconcile_passes where id = $1)`, kept).Scan(&stillThere); err != nil {
		t.Fatalf("read whether the kept row is still there: %v", err)
	}
	if !stillThere {
		t.Errorf("the row %d, within the %s retention, was pruned", kept, PassRetention)
	}
}

// A pass whose process was killed between its start and its end reads as ended without
// completing, and no later pass repairs it: nothing knows what it had done. Here the killed
// process's registry row is still there and still live -- a killed process stays live for up to
// processes.LiveWindow after its last heartbeat -- and another process holds the lease and is
// running a pass of its own, so the only thing that can decide the open row is that it is no
// longer the newest.
func TestAKilledPassReadsAsEndedWithoutCompletingWhileAnotherProcessHoldsTheLease(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	killed := registerProcess(t, h.pool, 0)
	open := seedPass(t, h.pool, killed, time.Minute)

	// The other process of the deployment, with a reconciler and a registry row of its own.
	other := registerProcess(t, h.pool, 0)
	replica := h.replica()
	replica.Instance = func() string { return other }
	release, _, finished := holdLease(t, h.pool, replica.Pass)
	<-finished
	defer release()

	passes := recent(t, h.pool)
	if len(passes) != 2 {
		t.Fatalf("the read answered %d passes, want the killed one and the other process's: %+v", len(passes), passes)
	}
	if got := passOf(t, passes, open).State; got != PassStateAbandoned {
		t.Errorf("the killed process's pass reads %q, want %q", got, PassStateAbandoned)
	}
	newest := passes[0]
	if newest.Instance != other || newest.State != outcomeComplete {
		t.Errorf("the newest pass is %q %q, want the other process's completed one", newest.Instance, newest.State)
	}
	// The killed process's registry row is still there: the row was not decided by its absence.
	var live int
	if err := h.pool.QueryRow(t.Context(), `select count(*) from server_processes where instance = $1`, killed).Scan(&live); err != nil {
		t.Fatalf("read the killed process's registry row: %v", err)
	}
	if live != 1 {
		t.Fatalf("the killed process's registry row is gone, so this case proves nothing")
	}
}

// The liveness rule on its own. The killed process's row is the newest there is, and the
// `ci-reconcile` lease is held by a session of its own -- which is what a dead process whose
// backend Postgres has not yet reaped leaves behind -- so neither of the other two rules can
// decide this row. Only the heartbeat can: the killed process's registry row is older than
// processes.LiveWindow, so the pass is over whatever the lease says.
//
// LiveWindow is a variable so this test can shorten it rather than wait a real window out; the
// heartbeat is written older than the shortened window, so the test waits for nothing at all.
//
// The mutation this case exists to catch is "live while its row exists": read that way, the
// killed process is live, the lease is held and the row is the newest, and the pass reads as
// running for ever.
func TestTheLivenessRuleEndsTheNewestPassOfAProcessThatStoppedHeartbeating(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	was := processes.LiveWindow
	processes.LiveWindow = time.Second
	t.Cleanup(func() { processes.LiveWindow = was })

	killed := registerProcess(t, h.pool, 5*time.Second)
	open := seedPass(t, h.pool, killed, time.Minute)
	release, taken, _ := holdLease(t, h.pool, nil)
	<-taken
	defer release()

	held, err := duty.LeaseHeld(t.Context(), h.pool, Lease)
	if err != nil {
		t.Fatalf("read whether the lease is held: %v", err)
	}
	if !held {
		t.Fatal("the test does not hold the lease, so this case proves nothing")
	}
	passes := recent(t, h.pool)
	if len(passes) != 1 {
		t.Fatalf("the read answered %d passes, want the killed one alone: %+v", len(passes), passes)
	}
	if passes[0].ID != open {
		t.Fatalf("the newest pass is %d, want the killed process's %d", passes[0].ID, open)
	}
	if passes[0].State != PassStateAbandoned {
		t.Fatalf("the killed process's newest pass reads %q, want %q: its heartbeat is %s old and the live window is %s",
			passes[0].State, PassStateAbandoned, 5*time.Second, processes.LiveWindow)
	}
}

// A pass of a process the registry does not hold yet writes no row. cmd/dispatch starts the
// loops before it registers the process, because registration waits out a live peer whose
// signing keys do not match, so the first pass of a process can run before its instance id
// exists -- and a row naming no process could be read neither as running nor as ended.
func TestAPassBeforeItsProcessIsRegisteredRecordsNothing(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.reconciler.Instance = func() string { return "" }

	h.pass()

	if got := rowCount(t, h.pool); got != 0 {
		t.Fatalf("the table holds %d rows, want none: the process holds no registry row", got)
	}
	if line := h.passLine(); line["records"] == nil {
		t.Fatalf("the pass logged no line, so it did not run at all: %v", line)
	}
}
