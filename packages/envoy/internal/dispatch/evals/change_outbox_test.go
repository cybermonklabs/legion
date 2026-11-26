package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The eval writes record their change rows in the change log's outbox (delivery_changes_pending,
// migration 20261117100001) and take no change log lock, and each write's own drain moves a batch
// of them into the log in a short transaction of its own after it commits
// (delivery.DrainPendingBatch). R19 holds every write to at most 100 ms of the lock at the size of
// a correction of 1,400 readings, and an intake statement behind eval writes to a median wait of at
// most 20 ms: before the outbox the correction held it a median 1.53 s and the intake waited a
// median 632 ms. These tests assert the structure that bounds both (no eval write's transaction
// holds the lock, no intake statement waits on one) and log the times, since the wall clock is the
// build farm's measure (k20) and a busy runner would fail it here.

// The two keys of the change log's lock, pg_advisory_xact_lock(6900, 0) (delivery's
// lockChangesStatement).
const (
	changeLockClass  = 6900
	changeLockObject = 0
)

// correctionSize is the size of the correction R19 is measured at.
const correctionSize = 1400

// readingsOverDays is a batch of n readings of the `minutes` series, one run each, spread over the
// 28 days of October 2026, the i-th replacing replaces[i] where replaces is given, with a value
// half a minute above the one it replaces, so the correction moves every day row it refolds.
func readingsOverDays(t *testing.T, n int, replaces []string) []json.RawMessage {
	t.Helper()
	rows := make([]json.RawMessage, n)
	for i := range n {
		fields := map[string]any{
			"series": "minutes", "row_kind": "run", "row_key": fmt.Sprintf("run-%d", i), "value": float64(i%97 + 1),
			"measured_at": time.Date(2026, time.October, 1+i%28, 9, i%60, 0, 0, time.UTC),
		}
		if replaces != nil {
			fields["replaces"], fields["value"] = replaces[i], float64(i%97+1)+0.5
		}
		rows[i] = row(t, fields)
	}
	return rows
}

// readingIDs is the ids of written's readings, in the batch's order.
func readingIDs(written Written) []string {
	ids := make([]string, len(written.Readings))
	for i, reading := range written.Readings {
		ids[i] = reading.ID
	}
	return ids
}

// recordCommitHolds makes every transaction that writes a day row or a version record, as it
// commits, whether it holds the change log's lock: a constraint trigger deferred to the commit
// reads pg_locks for its own backend, and a transaction advisory lock is held from where it is
// taken to the transaction's end, so a transaction holding it at its commit took it somewhere, and
// one not holding it never took it. The table and the triggers are the test database's alone.
// It answers how many transactions recorded since the last call, and how many of them held it.
func recordCommitHolds(t *testing.T, r rig) func() (int, int) {
	t.Helper()
	for _, statement := range []string{
		`create table evals_test_commit_holds (txid bigint primary key, holds boolean not null)`,
		fmt.Sprintf(`create function evals_test_record_hold() returns trigger language plpgsql as $$
		begin
			insert into evals_test_commit_holds (txid, holds)
			values (txid_current(), exists (
				select 1 from pg_locks where locktype = 'advisory' and classid = %d and objid = %d and objsubid = 2
				  and pid = pg_backend_pid() and granted))
			on conflict (txid) do nothing;
			return null;
		end $$`, changeLockClass, changeLockObject),
		`create constraint trigger evals_test_record_hold after insert or update on eval_days
			deferrable initially deferred for each row execute function evals_test_record_hold()`,
		`create constraint trigger evals_test_record_hold after insert on eval_versions
			deferrable initially deferred for each row execute function evals_test_record_hold()`,
	} {
		if _, err := r.database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("record the commits' holds: %v", err)
		}
	}
	return func() (int, int) {
		t.Helper()
		var recorded, holding int
		if err := r.database.Pool.QueryRow(t.Context(), `
			with taken as (delete from evals_test_commit_holds returning holds)
			select count(*), count(*) filter (where holds) from taken
		`).Scan(&recorded, &holding); err != nil {
			t.Fatalf("read the commits' holds: %v", err)
		}
		return recorded, holding
	}
}

// isDrain reports whether hold is the change log drain's own (delivery.DrainPending).
func isDrain(hold storetest.AdvisoryHold) bool {
	return strings.Contains(hold.Query, "delivery_changes_drain")
}

// TestAnEvalWriteHoldsNoChangeLogLock: a first registration, the 1,400 readings of a second eval
// (its first registration too), a register write that rewrites that eval's method in evals-md,
// and a correction of all 1,400 readings each commit without holding the change log's lock at
// all, each transaction's own record says, and the one transaction the samples see hold it during
// each write is the write's drain, whose hold is logged. Before the outbox each write took the lock
// first and held it to its commit.
func TestAnEvalWriteHoldsNoChangeLogLock(t *testing.T) {
	// Not parallel: it samples who holds the change log's lock every millisecond
	// (storetest.SampleAdvisoryHolds), which other tests' writes on the same server would hold.
	r := newRig(t)
	ctx := t.Context()
	commits := recordCommitHolds(t, r)
	var readings []string
	writes := []struct {
		what  string
		write func() error
	}{
		{"a first registration", func() error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "first-reading", Caller: alice, BuiltBy: testBuild,
				Rows: []json.RawMessage{row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "a", "value": 3, "measured_at": at(5, 9, 0)})}})
			return err
		}},
		{"1,400 readings of a new eval", func() error {
			written, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "build-minutes", Caller: alice, BuiltBy: testBuild,
				Rows: readingsOverDays(t, correctionSize, nil)})
			readings = readingIDs(written)
			return err
		}},
		{"a register write of its method", func() error {
			_, err := r.registry.Register(ctx, Registration{Project: r.project, Slug: "build-minutes", Caller: alice,
				DocText: Set("Times each build from its first job's start to its last job's end.")})
			return err
		}},
		{"a correction of its 1,400 readings", func() error {
			_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "build-minutes", Caller: alice, BuiltBy: testBuild,
				Rows: readingsOverDays(t, correctionSize, readings)})
			return err
		}},
	}
	for _, write := range writes {
		stop := storetest.SampleAdvisoryHolds(t, r.database.Pool, changeLockClass, changeLockObject)
		began := time.Now()
		err := write.write()
		took := time.Since(began)
		holds := stop()
		if err != nil {
			t.Fatalf("%s: %v", write.what, err)
		}
		recorded, holding := commits()
		if recorded == 0 {
			t.Fatalf("%s recorded no commit, so this test examined nothing", write.what)
		}
		if holding > 0 {
			t.Errorf("%s: %d of its %d transactions held the change log's lock at their commit, want none", write.what, holding, recorded)
		}
		for _, hold := range holds {
			if !isDrain(hold) {
				t.Errorf("%s: a transaction other than the drain held the change log's lock: %s", write.what, hold)
			}
		}
		t.Logf("%s took %s; %d of its %d transactions held the change log's lock at their commit; the samples saw %d holds: %v",
			write.what, took.Round(time.Millisecond), holding, recorded, len(holds), holds)
	}
}

// intakeApplication is the application_name of the intake statements' connections, by which a
// sample of pg_locks tells them from the registry's.
const intakeApplication = "outbox-test-intake"

// intakePool is a pool of its own on r's database for the intake statements a test sends beside
// the registry's writes, its connections named intakeApplication and every one opened before it is
// used, so a statement's time is its own and not a pool's wait or a connection's dial.
func intakePool(t *testing.T, r rig, size int32) *store.Pool {
	t.Helper()
	config := r.database.Pool.Config().Copy()
	config.MaxConns, config.MinConns = size, size
	config.ConnConfig.RuntimeParams["application_name"] = intakeApplication
	pgxPool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open the intake's pool: %v", err)
	}
	t.Cleanup(pgxPool.Close)
	held := make([]*pgxpool.Conn, 0, size)
	for range size {
		conn, err := pgxPool.Acquire(t.Context())
		if err != nil {
			t.Fatalf("open the intake's connections: %v", err)
		}
		held = append(held, conn)
	}
	for _, conn := range held {
		conn.Release()
	}
	return store.NewPool(pgxPool)
}

// intakeBlocker is one backend a sample saw an intake statement wait behind on the change log's
// lock: its application_name and the statement it ran.
type intakeBlocker struct {
	application, query string
}

// intakeBlockers starts reading, every millisecond on a connection of its own to r's database,
// which backends each intake statement waiting on the change log's lock waits behind
// (pg_blocking_pids: the holder, and the waiters queued ahead of it), and answers stop, which ends
// the reading and answers every blocker each sample saw. It reads the locks of r's database alone,
// since an advisory lock is a lock of one database.
func intakeBlockers(t *testing.T, r rig) (stop func() []intakeBlocker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.ConnectConfig(ctx, r.database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		cancel()
		t.Fatalf("connect the intake's lock sampler: %v", err)
	}
	query := fmt.Sprintf(`
		select coalesce(b.application_name, ''), coalesce(b.query, '')
		from pg_locks l
		join pg_stat_activity w on w.pid = l.pid
		cross join lateral unnest(pg_blocking_pids(l.pid)) as blocking(pid)
		join pg_stat_activity b on b.pid = blocking.pid
		where l.locktype = 'advisory' and l.classid = %d and l.objid = %d and l.objsubid = 2 and not l.granted
		  and l.database = (select oid from pg_database where datname = current_database())
		  and w.application_name = $1
	`, changeLockClass, changeLockObject)
	done := make(chan struct{})
	var blockers []intakeBlocker
	var failure error
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			rows, err := conn.Query(ctx, query, intakeApplication)
			if err == nil {
				var seen []intakeBlocker
				seen, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (intakeBlocker, error) {
					var blocker intakeBlocker
					err := row.Scan(&blocker.application, &blocker.query)
					return blocker, err
				})
				blockers = append(blockers, seen...)
			}
			if err != nil {
				if ctx.Err() == nil {
					failure = err
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() []intakeBlocker {
		t.Helper()
		cancel()
		<-done
		_ = conn.Close(context.Background())
		if failure != nil {
			t.Fatalf("sample what the intake statements wait behind: %v", failure)
		}
		return blockers
	}
}

// median is the middle of durations, the upper middle of an even count.
func median(durations []time.Duration) time.Duration {
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// TestNoIntakeStatementWaitsOnAnEvalCorrection: while a correction of 1,400 readings runs, a run
// write of the delivery intake (delivery.UpsertRun, whose trigger takes the change log's lock) is
// sent every 20 ms, each on a connection of its own, and no sample of pg_locks finds one waiting on
// the lock behind a correction's transaction: whatever it waits behind is a drain or another
// intake statement. Corrections run one after another until at least minIntakeWrites run writes
// were sent inside one, up to maxCorrections of them, so how many land does not hang on how fast
// the machine runs a correction: a fast one runs more corrections. The median and the longest time
// a run write took are logged and asserted nowhere: R19's 20 ms median wait is the build farm's
// measure (k20), which a busy runner would fail here. Before the outbox every run write sent
// during a correction waited behind that correction's transaction until it committed.
func TestNoIntakeStatementWaitsOnAnEvalCorrection(t *testing.T) {
	// Not parallel: it times the intake's statements behind a correction (intakeBlockers), a
	// bound that other tests' load on the same server would move.
	r := newRig(t)
	ctx := t.Context()
	intake := intakePool(t, r, 48)
	readings := readingIDs(r.write(t, "build-minutes", alice, readingsOverDays(t, correctionSize, nil)...))
	const minIntakeWrites, maxCorrections, every = 10, 50, 20 * time.Millisecond
	var (
		mu    sync.Mutex
		waits []time.Duration
		next  = int64(9000)
		took  []time.Duration
		sent  int
	)
	stop := intakeBlockers(t, r)
	for len(took) < maxCorrections && sent < minIntakeWrites {
		done := make(chan error, 1)
		began := time.Now()
		go func() {
			written, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "build-minutes", Caller: alice, BuiltBy: testBuild,
				Rows: readingsOverDays(t, correctionSize, readings)})
			if err == nil {
				readings = readingIDs(written)
			}
			done <- err
		}()
		var writes sync.WaitGroup
		ticker := time.NewTicker(every)
		var correctionErr error
	send:
		for {
			select {
			case correctionErr = <-done:
				break send
			case <-ticker.C:
				next++
				sent++
				id := next
				writes.Go(func() {
					started := time.Now()
					err := delivery.UpsertRun(ctx, intake, delivery.DeliveryRun{
						Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("sha%d", id),
						HeadCommitAt: started, StartedAt: started, URL: fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
					})
					wait := time.Since(started)
					if err != nil {
						t.Errorf("the run write %d: %v", id, err)
						return
					}
					mu.Lock()
					waits = append(waits, wait)
					mu.Unlock()
				})
			}
		}
		ticker.Stop()
		took = append(took, time.Since(began))
		writes.Wait()
		if correctionErr != nil {
			t.Fatalf("the correction: %v", correctionErr)
		}
	}
	blockers := stop()
	if sent < minIntakeWrites {
		t.Fatalf("%d corrections of %d readings let %d run writes be sent inside them, want at least %d to examine",
			len(took), correctionSize, sent, minIntakeWrites)
	}
	if len(waits) == 0 {
		t.Fatalf("none of the %d run writes sent inside the corrections landed", sent)
	}
	behind := map[string]int{}
	for _, blocker := range blockers {
		switch {
		case blocker.application == intakeApplication:
			behind["another run write"]++
		case strings.Contains(blocker.query, "delivery_changes_drain"):
			behind["a drain"]++
		default:
			behind["a correction's transaction"]++
			if behind["a correction's transaction"] <= 3 {
				t.Errorf("a run write waited on the change log's lock behind a backend that is neither a drain nor a run write, running %q", blocker.query)
			}
		}
	}
	sorted := slices.Clone(waits)
	slices.Sort(sorted)
	t.Logf("%d run writes during %d corrections of %d readings (each took %v): median %s, longest %s; samples of a waiting run write saw it behind %v",
		len(waits), len(took), correctionSize, took, median(waits).Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond), behind)
}

// TestACorrectionCommitsWhileTheChangeLogLockIsHeldElsewhere: a correction of 1,400 readings
// commits while another transaction holds the change log's lock, since the write never takes it,
// and its change rows wait in the outbox for its drain, which waits for the lock. Let go, the drain
// moves every one of them, and the only transaction the samples see hold the lock from then on is
// the drain. The samples start before the release, so a sample can see the gate's own hold; that
// hold is left out by its identity, its backend and its transaction's start. The time from the
// release to the write's answer, which bounds the drain's hold from above, is logged. Before the
// outbox the write waited for the lock before it wrote anything, and held it for the whole write.
func TestACorrectionCommitsWhileTheChangeLogLockIsHeldElsewhere(t *testing.T) {
	// Not parallel: it samples who holds the change log's lock every millisecond
	// (storetest.SampleAdvisoryHolds), which other tests' writes on the same server would hold.
	r := newRig(t)
	ctx := t.Context()
	readings := readingIDs(r.write(t, "build-minutes", alice, readingsOverDays(t, correctionSize, nil)...))
	head, err := delivery.ChangesHead(ctx, r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	before := head.Seq

	gate, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock($1, $2)`, changeLockClass, changeLockObject); err != nil {
		t.Fatalf("take the change log's lock: %v", err)
	}
	// The gate's identity, by which a sample of its own hold is told from the drain's: its backend
	// and its transaction's start, which now() answers in a transaction and a sample reads as
	// xact_start. The backend alone would not do: the drain may run on the connection the gate
	// gives back, the same backend in a transaction of its own.
	var gatePID uint32
	var gateStart time.Time
	if err := gate.QueryRow(ctx, `select pg_backend_pid(), now()`).Scan(&gatePID, &gateStart); err != nil {
		t.Fatalf("read the gate's backend and transaction start: %v", err)
	}
	done := make(chan error, 1)
	answered := make(chan time.Time, 1)
	go func() {
		_, err := r.registry.WriteReadings(ctx, Batch{Project: r.project, Slug: "build-minutes", Caller: alice, BuiltBy: testBuild,
			Rows: readingsOverDays(t, correctionSize, readings)})
		answered <- time.Now()
		done <- err
	}()
	storetest.WaitForLockWaiters(t, gate, 1)
	pending := r.count(t, `select count(*) from delivery_changes_pending`)
	if pending == 0 {
		t.Fatal("the correction waits for the change log's lock with no change of its own committed: it takes the lock before it writes")
	}
	stop := storetest.SampleAdvisoryHolds(t, r.database.Pool, changeLockClass, changeLockObject)
	released := time.Now()
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the lock go: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the correction: %v", err)
	}
	drained := (<-answered).Sub(released)
	holds := stop()
	if left := r.count(t, `select count(*) from delivery_changes_pending`); left != 0 {
		t.Errorf("%d change rows are still pending after the correction's drain", left)
	}
	changes, err := delivery.ChangesSince(ctx, r.database.Pool, before, 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, change := range changes {
		kinds = append(kinds, change.Kind)
	}
	if len(changes) != pending || !slices.Contains(kinds, delivery.ChangeKindEvalReadings) || !slices.Contains(kinds, delivery.ChangeKindEvalDays) {
		t.Errorf("the drain moved %v into the log, want the %d pending changes, its readings' and its day rows'", kinds, pending)
	}
	for _, hold := range holds {
		if hold.PID == gatePID && hold.XactStart.Equal(gateStart) {
			continue
		}
		if !isDrain(hold) {
			t.Errorf("a transaction other than the correction's drain held the change log's lock after its release: %s", hold)
		}
	}
	t.Logf("the correction committed %d change rows while the lock was held elsewhere; once let go its drain answered in %s (the samples saw %v)",
		pending, drained.Round(time.Microsecond), holds)
}

// TestAReadingsWriteDrainsOneBatchOfAnotherWritersBacklog: with more change rows pending than two
// batches of the drain, left by another writer (a whole-project Legion facts pass records
// thousands, which wait for its drain), a readings write answers having moved one batch of them
// into the log, not the whole backlog, and leaves the rest pending, its own change rows behind
// them; the next drain moves every one of them, so nothing is lost and nothing is moved twice.
// Before, the write's drain moved batch after batch until the outbox was empty, and the write
// answered only then.
func TestAReadingsWriteDrainsOneBatchOfAnotherWritersBacklog(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	minutes := func(key string, day int) json.RawMessage {
		return row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": key, "value": 3, "measured_at": at(day, 9, 0)})
	}
	evalID := r.write(t, "build-minutes", alice, minutes("a", 4)).Definition.EvalID
	// The contract the test states, written out as the delivery package's own test writes it
	// rather than read from its drainBatch: a batch moves at most 250 rows.
	const batchRows, backlog = 250, 700
	if _, err := r.database.Pool.Exec(ctx, `
		select delivery_change_log_pending('issue_facts', null, $1, $1 || '-' || n, null, null)
		from generate_series(1, $2) n
	`, r.project, backlog); err != nil {
		t.Fatalf("record %d pending changes of another writer: %v", backlog, err)
	}
	head, err := delivery.ChangesHead(ctx, r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	before := head.Seq
	r.write(t, "build-minutes", alice, minutes("b", 5))
	moved := r.count(t, `select count(*) from delivery_changes where seq > $1`, before)
	pending := r.count(t, `select count(*) from delivery_changes_pending`)
	t.Logf("the readings write's drain moved %d change rows and left %d pending", moved, pending)
	if moved == 0 || moved > batchRows || moved+pending <= backlog {
		t.Errorf("the readings write's drain moved %d of the %d pending change rows another writer left and its own, and left %d; want one batch of at most %d moved and the rest pending",
			moved, backlog, pending, batchRows)
	}
	rest, err := delivery.DrainPending(ctx, r.database.Pool)
	if err != nil {
		t.Fatalf("the next drain: %v", err)
	}
	if left := r.count(t, `select count(*) from delivery_changes_pending`); left != 0 || rest != int64(pending) {
		t.Errorf("the next drain moved %d rows and left %d pending, want the %d the write left and none", rest, left, pending)
	}
	changes, err := delivery.ChangesSince(ctx, r.database.Pool, before, 10*backlog)
	if err != nil {
		t.Fatal(err)
	}
	own := slices.ContainsFunc(changes, func(change delivery.Change) bool {
		return change.Kind == delivery.ChangeKindEvalReadings && change.Ref == evalID
	})
	if len(changes) != moved+pending || !own {
		t.Errorf("the log holds %d changes after the write, want the %d it moved and the %d the next drain moved, the write's readings change among them (%t)",
			len(changes), moved, pending, own)
	}
}

// TestACancelledDrainAfterCommitStillAnswersSuccessAndLeavesChangesForTheNextDrain: a readings
// write whose context is cancelled while its post-commit drain waits on the change log's lock
// still answers success with the reading it committed, and leaves its change rows pending in the
// outbox rather than losing them; once the lock is let go, the next drain moves every one of them
// into the change log. The backend of the cancelled drain can stay queued behind the lock and run
// the drain when it is let go, so the test ends every backend still queued behind it first, and the
// drain that moves the rows is the next one.
func TestACancelledDrainAfterCommitStillAnswersSuccessAndLeavesChangesForTheNextDrain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	minutes := func(key string, day int) json.RawMessage {
		return row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": key, "value": 3, "measured_at": at(day, 9, 0)})
	}
	evalID := r.write(t, "build-minutes", alice, minutes("a", 4)).Definition.EvalID
	head, err := delivery.ChangesHead(ctx, r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	before := head.Seq

	gate, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock($1, $2)`, changeLockClass, changeLockObject); err != nil {
		t.Fatalf("take the change log's lock: %v", err)
	}

	writeCtx, cancelWrite := context.WithCancel(ctx)
	defer cancelWrite()
	type answer struct {
		written Written
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		written, err := r.registry.WriteReadings(writeCtx, Batch{Project: r.project, Slug: "build-minutes", Caller: alice, BuiltBy: testBuild,
			Rows: []json.RawMessage{minutes("b", 5)}})
		done <- answer{written, err}
	}()
	storetest.WaitForLockWaiters(t, gate, 1)
	pending := r.count(t, `select count(*) from delivery_changes_pending`)
	if pending == 0 {
		t.Fatal("the write waits on the change log's lock with no change row pending: it took the lock before its commit")
	}
	cancelWrite()
	got := testwait.Receive(t, "the write's answer once its drain is cancelled", done)
	if got.err != nil || len(got.written.Readings) != 1 {
		t.Fatalf("the write whose post-commit drain was cancelled answered %v (%d readings), want success with the committed reading", got.err, len(got.written.Readings))
	}
	if still := r.count(t, `select count(*) from delivery_changes_pending`); still != pending {
		t.Errorf("%d change rows remain pending after the cancelled drain, want all %d", still, pending)
	}
	if _, err := r.database.Pool.Exec(ctx, `
		select pg_terminate_backend(pid) from pg_stat_activity
		where datname = current_database() and $1::integer = any(pg_blocking_pids(pid))
	`, int32(gate.Conn().PgConn().PID())); err != nil {
		t.Fatalf("end the backends queued behind the gate: %v", err)
	}
	testwait.Until(t, "no backend to stay queued behind the gate (the backends queued, last read)", func() (int, bool) {
		queued, err := storetest.LockWaiters(ctx, gate)
		if err != nil {
			t.Fatal(err)
		}
		return queued, queued == 0
	})
	if still := r.count(t, `select count(*) from delivery_changes_pending`); still != pending {
		t.Errorf("%d change rows remain pending once the backends behind the gate are ended, want all %d", still, pending)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the change log's lock go: %v", err)
	}
	moved, err := delivery.DrainPending(ctx, r.database.Pool)
	if err != nil {
		t.Fatalf("the next drain: %v", err)
	}
	if left := r.count(t, `select count(*) from delivery_changes_pending`); left != 0 || moved != int64(pending) {
		t.Errorf("the next drain moved %d rows and left %d pending, want %d and none", moved, left, pending)
	}
	changes, err := delivery.ChangesSince(ctx, r.database.Pool, before, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, change := range changes {
		if change.Ref == evalID {
			kinds = append(kinds, change.Kind)
		}
	}
	if len(changes) != pending || !slices.Contains(kinds, delivery.ChangeKindEvalReadings) || !slices.Contains(kinds, delivery.ChangeKindEvalDays) {
		t.Errorf("the next drain moved %v into the log (%d changes), want the %d pending changes, its readings' and its day rows'", kinds, len(changes), pending)
	}
	t.Logf("the write answered success with its drain cancelled, left %d change rows pending, and the next drain moved %v", pending, kinds)
}
