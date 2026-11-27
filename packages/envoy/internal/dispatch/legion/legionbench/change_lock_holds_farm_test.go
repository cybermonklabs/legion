//go:build farm

package legionbench

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestFarmChangeLockHolds is R7's measurement of the change log's lock (k27; spec Design, "How
// R7's hold is measured"). It runs in the R7 Job only (r7.sh), built with the farm tag, against a
// database legionbench loaded at INFRA's size with -first-read: no issue_facts row and no build in
// issue_facts_computed. It starts the server built at this head on that database, which reads
// INFRA's facts whole, about 10,920 rows whose changes the triggers put in the outbox and the drain
// moves in batches of at most 250. Beside that read, and for LEGIONBENCH_AFTER (5 minutes) after
// it, it writes through the delivery intake at LEGIONBENCH_RATE times the Design's rates:
//
//   - one day's volume of runs at ten times the stated run rate, 200 deploy runs of 8 jobs and 600
//     pull-request runs of 40 jobs, spread evenly over 5 minutes, one deploy run in four: each
//     run's row (delivery.UpsertRun) and then its jobs in one statement (delivery.UpsertRunJobs);
//   - after each deploy run's jobs, the deployed write (delivery.RecordDeployedEvents), which marks
//     the pull request the run ships: a merged pull request of the deploy repository naming an
//     issue, written before the measurement with its merge between that deploy run's creation and
//     the one before it, so each deploy run ships one;
//   - one session-group write a second: a rename (delivery.UpdateSessionGroup, whose write is
//     writeSessionGroup's) and, in place of every fifth, a delete (delivery.DeleteSessionGroup's
//     batch), on groups created before the measurement.
//
// storetest.SampleAdvisoryHolds samples each hold of the lock (6900, 0) and sampleLockWaits each
// wait for it. Each writer writes through a pool of its own, whose connections' backends it
// records, so a hold or a wait is that writer's by its backend; a hold by another backend is the
// drain's where its statement is delivery_changes_drain. The run row's write, which a run's jobs
// need first, is recorded as a writer of its own (run), and its statements are intake statements
// whose waits count, but R7 bounds the hold of four writers and it is not one of them.
//
// Before the measurement it marks the deployed write of every pull request the loaded history's
// deploy runs shipped (RecordDeployedEvents, production's own state once each deploy was read), so
// the window's writes are the window's runs' alone. After it, it deletes the pull requests it
// wrote for deploy runs the window did not reach, so the next run of the Job finds none shipped by
// its first deploy run.
//
// LEGIONBENCH_GATE says what the run is held to: `stated` (k27's run at the stated load: each of
// the four writers' p99 hold at most 100 ms, the intake's median wait at most 20 ms, the drain at
// least 40 holds and each other writer at least 150), `recorded` (the run at one and a half times
// the load: recorded, not gated), or `sleep` (k27's negative control, a server whose drain sleeps
// 150 ms after it takes the lock and before its transaction commits). The sleep arm is judged as a
// control: its drain must miss R7's hold bound, a p99 hold above 100 ms, while it still takes the
// lock at least 40 times, so the miss is the hold's alone. That miss is a RESULT line and passes
// the test; a drain held within the bound, or taken fewer than 40 times, is a FAIL line, since the
// measurement then cannot see the hold it bounds. Every measurement prints a RESULT line and every
// miss a FAIL line, which fails the test.
func TestFarmChangeLockHolds(t *testing.T) {
	o := farmOptionsFromEnv(t)
	ctx := t.Context()
	setup, err := store.Open(ctx, o.database)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer setup.Pool.Close()
	r := &farmReport{t: t, prefix: "r7 arm=" + o.arm}
	r.result("gate=%s rate=%g after=%s server=%s", o.gate, o.rate, o.after, o.server)

	// The read's precondition: the server that boots reads INFRA whole.
	var rows, computed int64
	if err := setup.Pool.QueryRow(ctx, `select (select count(*) from issue_facts), (select count(*) from issue_facts_computed)`).
		Scan(&rows, &computed); err != nil {
		t.Fatalf("read the facts' precondition: %v", err)
	}
	if rows != 0 || computed != 0 {
		r.fail("issue_facts holds %d rows and issue_facts_computed %d: the server would make no first whole read", rows, computed)
		t.FailNow()
	}

	plan, err := planFarmDrive(ctx, r, setup.Pool, o)
	if err != nil {
		t.Fatalf("plan the intake drive: %v", err)
	}
	defer plan.removeUnshipped(r, setup.Pool)
	writers := map[string]*farmWriter{}
	for _, name := range []string{"run", "run-jobs", "deployed", "session-group"} {
		writers[name] = newFarmWriter(t, ctx, o.database, name)
	}
	var startSeq int64
	if err := setup.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from delivery_changes`).Scan(&startSeq); err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	stopHolds := storetest.SampleAdvisoryHolds(t, setup.Pool, changeLockClass, changeLockObject)
	stopWaits := sampleLockWaits(t, setup.Pool, changeLockClass, changeLockObject)

	driveCtx, stopDrive := context.WithCancel(ctx)
	var drivers sync.WaitGroup
	drivers.Go(func() { plan.driveRuns(driveCtx, r, writers) })
	drivers.Go(func() { plan.driveGroups(driveCtx, r, writers["session-group"]) })

	server, err := startFarmServer(o)
	if err != nil {
		stopDrive()
		drivers.Wait()
		t.Fatalf("start the server: %v", err)
	}
	defer server.stop()
	build, err := server.waitHealthy(ctx, 15*time.Minute)
	if err != nil {
		r.fail("the server did not serve: %v (its log: %s)", err, server.log)
	}
	readEnded, wrote, err := waitWholeRead(ctx, setup.Pool, build, o.readTimeout)
	if err != nil {
		r.fail("whole read: %v", err)
		readEnded = time.Now()
	} else {
		r.result("whole_read build=%s rows=%d seconds_from_server_start=%.1f", build, wrote, readEnded.Sub(server.started).Seconds())
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(readEnded.Add(o.after))):
	}
	stopDrive()
	drivers.Wait()
	holds := stopHolds()
	waits := stopWaits()
	server.stop()
	r.result("window seconds=%.1f (from the drive's start to %s after the read)", time.Since(plan.start).Seconds(), o.after)
	// The drain's takes: each transaction that moved outbox rows into the log wrote them, so the
	// moved rows' distinct inserting transactions since the measurement began are its takes.
	var drainTakes int64
	if err := setup.Pool.QueryRow(ctx, `
		select count(distinct xmin::text) from delivery_changes
		where seq > $1 and kind in ('issue_facts', 'issue_filings')`, startSeq).Scan(&drainTakes); err != nil {
		t.Fatalf("count the drain's transactions: %v", err)
	}
	judgeFarmHolds(r, o, writers, holds, waits, drainTakes)
}

// The change log's lock: the two-key transaction advisory lock (6900, 0) (delivery's
// lockChangesStatement).
const (
	changeLockClass  = 6900
	changeLockObject = 0
)

// farmOptions is the run's environment.
type farmOptions struct {
	database, server, work, arm, gate string
	port                              int
	rate                              float64
	after, readTimeout                time.Duration
}

// farmOptionsFromEnv reads the run's environment, failing the test where a variable it needs is
// unset rather than skipping it: a measurement that ran nothing is no evidence.
func farmOptionsFromEnv(t *testing.T) farmOptions {
	t.Helper()
	o := farmOptions{
		database: os.Getenv("LEGIONBENCH_DATABASE_URL"), server: os.Getenv("LEGIONBENCH_SERVER"),
		work: os.Getenv("LEGIONBENCH_WORK"), arm: os.Getenv("LEGIONBENCH_ARM"), gate: os.Getenv("LEGIONBENCH_GATE"),
		port: 58111, rate: 1, after: 5 * time.Minute, readTimeout: 30 * time.Minute,
	}
	if o.database == "" || o.server == "" || o.work == "" || o.arm == "" {
		t.Fatal("set LEGIONBENCH_DATABASE_URL, LEGIONBENCH_SERVER, LEGIONBENCH_WORK and LEGIONBENCH_ARM (r7.sh does)")
	}
	if !slices.Contains([]string{"stated", "recorded", "sleep"}, o.gate) {
		t.Fatalf("LEGIONBENCH_GATE is stated, recorded or sleep, not %q", o.gate)
	}
	if value := os.Getenv("LEGIONBENCH_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("LEGIONBENCH_PORT: %v", err)
		}
		o.port = port
	}
	if value := os.Getenv("LEGIONBENCH_RATE"); value != "" {
		rate, err := strconv.ParseFloat(value, 64)
		if err != nil || rate <= 0 {
			t.Fatalf("LEGIONBENCH_RATE is a multiple of the stated rates above 0, not %q", value)
		}
		o.rate = rate
	}
	for name, into := range map[string]*time.Duration{"LEGIONBENCH_AFTER": &o.after, "LEGIONBENCH_READ_TIMEOUT": &o.readTimeout} {
		if value := os.Getenv(name); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			*into = parsed
		}
	}
	return o
}

// farmReport prints the run's RESULT and FAIL lines; a FAIL line fails the test.
type farmReport struct {
	t      *testing.T
	prefix string
	mu     sync.Mutex
}

func (r *farmReport) result(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Printf("RESULT "+r.prefix+" "+format+"\n", args...)
}

func (r *farmReport) fail(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Printf("FAIL "+r.prefix+" "+format+"\n", args...)
	r.t.Fail()
}

// farmWriter is one intake writer: a pool of its own, the backends its connections run on, how
// many writes it made that take the change log's lock, and how many failed.
type farmWriter struct {
	name     string
	pool     *store.Pool
	mu       sync.Mutex
	backends map[uint32]bool
	writes   atomic.Int64
	failures atomic.Int64
}

// newFarmWriter opens a pool for one writer, named for it, which records each backend it connects.
func newFarmWriter(t *testing.T, ctx context.Context, url, name string) *farmWriter {
	t.Helper()
	writer := &farmWriter{name: name, backends: map[uint32]bool{}}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("read the database URL: %v", err)
	}
	config.MaxConns = 32
	config.ConnConfig.RuntimeParams["application_name"] = "legionbench-" + name
	config.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		writer.mu.Lock()
		writer.backends[conn.PgConn().PID()] = true
		writer.mu.Unlock()
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the %s writer's pool: %v", name, err)
	}
	writer.pool = store.NewPool(pool)
	t.Cleanup(writer.pool.Close)
	return writer
}

// owns is whether backend is one of the writer's connections.
func (w *farmWriter) owns(backend uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.backends[backend]
}

// The Design's intake rates: one day's runs at ten times the stated run rate, over 5 minutes, one
// deploy run in four; and one session-group write a second, every fifth a delete.
const (
	farmDeployRuns    = 200
	farmPRRuns        = 600
	farmSpread        = 5 * time.Minute
	farmDeployEvery   = 4
	farmGroupsASecond = 1.0
	farmDeleteEvery   = 5
	farmPRChecksJobs  = 40
	// farmHeadroom is how long the plan allows the whole read to take beside the window after it:
	// the session groups and the deploy runs' pull requests are written for that much more.
	farmHeadroom = 10 * time.Minute
	// farmRunAge is how long before its write a run was created: every job of it has finished by
	// the write, as a run's jobs listing that intake writes complete has.
	farmRunAge = 10 * time.Minute
)

// farmDeployJobs are a deploy run's eight jobs; planFarmDrive puts the settings' first production
// job name at farmProductionIndex.
var farmDeployJobs = []string{"build / image", "build / assets", "migrate / check", "staging-release / apply",
	"staging-e2e / run", "production-release / plan", "", "notify / release"}

const farmProductionIndex = 6

// farmPlan is the drive: the delivery settings, the moment it starts and the time between two runs,
// the next run id, the pull requests the deploy runs ship, and the session groups the group writes
// edit.
type farmPlan struct {
	settings        delivery.DeliverySettings
	start           time.Time
	interval        time.Duration
	firstRun        int64
	groupsPerSecond float64
	shipped         []int
	broker          *events.Broker
	groups          []string
	renamed         map[string]bool
}

// planFarmDrive writes what the drive needs before any sample is taken: the deployed writes of the
// pull requests the history's deploy runs shipped, the session groups, and a pull request for each
// deploy run the drive may write, merged between that run's creation and the deploy run's before
// it. It plans for the window after the read and farmHeadroom for the read.
func planFarmDrive(ctx context.Context, r *farmReport, pool *store.Pool, o farmOptions) (*farmPlan, error) {
	settings, err := delivery.GetSettings(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("read the delivery settings: %w", err)
	}
	if len(settings.ProductionJobNames) == 0 {
		return nil, errors.New("the delivery settings name no production job")
	}
	runsPerSecond := o.rate * float64(farmDeployRuns+farmPRRuns) / farmSpread.Seconds()
	plan := &farmPlan{
		settings: settings, interval: time.Duration(float64(time.Second) / runsPerSecond),
		groupsPerSecond: o.rate * farmGroupsASecond, broker: events.NewBroker(), renamed: map[string]bool{},
	}
	marked, err := delivery.RecordDeployedEvents(ctx, pool, settings, plan.broker)
	if err != nil {
		return nil, fmt.Errorf("mark the history's shipped pull requests: %w", err)
	}
	if err := pool.QueryRow(ctx, `select coalesce(max(run_id), 0) + 1 from delivery_runs where repo = $1`, settings.DeployRepo).
		Scan(&plan.firstRun); err != nil {
		return nil, fmt.Errorf("read the next run id: %w", err)
	}

	planned := o.after + farmHeadroom
	deletes := int(math.Ceil(plan.groupsPerSecond*planned.Seconds()/farmDeleteEvery)) + 10
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	for index := range deletes {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		group, err := delivery.CreateSessionGroup(ctx, tx, fmt.Sprintf("Bench crew %s %s %d", o.arm, stamp, index),
			[]string{fmt.Sprintf("bench-%s-%s-%d", o.arm, stamp, index)}, actor)
		if err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, fmt.Errorf("create session group %d: %w", index, err)
		}
		plan.groups = append(plan.groups, group.ID)
	}

	var issues []string
	keys, err := pool.Query(ctx, `select key from issues where project_key = $1 order by number`, Project)
	if err != nil {
		return nil, err
	}
	if issues, err = pgx.CollectRows(keys, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("%s holds no issue for the deploy runs' pull requests to name", Project)
	}
	var number int
	if err := pool.QueryRow(ctx, `select coalesce(max(number), 0) from delivery_pull_requests where repo = $1`, settings.DeployRepo).
		Scan(&number); err != nil {
		return nil, err
	}
	plan.start = time.Now().Add(2 * time.Second)
	deploys := int(math.Ceil(runsPerSecond/farmDeployEvery*planned.Seconds())) + 5
	var numbers []int32
	var merged []time.Time
	var titles, urls, linked []string
	for deploy := range deploys {
		number++
		plan.shipped = append(plan.shipped, number)
		key := issues[(deploy*7919)%len(issues)]
		numbers = append(numbers, int32(number))
		merged = append(merged, plan.created(int64(deploy*farmDeployEvery)).Add(-plan.interval/2))
		titles = append(titles, fmt.Sprintf("%s: ship %d", key, number))
		urls = append(urls, fmt.Sprintf("https://github.com/%s/pull/%d", settings.DeployRepo, number))
		linked = append(linked, key)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, merge_commit_sha,
			additions, deletions, rework, issue_key, sessions, partial, attribution_title_keys, attribution_cited_issues,
			attribution_branch_keys, attribution_commit_keys)
		select $1, pr.number, pr.title, pr.url, 'dev-ana', pr.merged - interval '1 hour', pr.merged, md5(pr.url),
			10, 2, false, pr.issue, '{}', false, array[pr.issue], '{}', '{}', '{}'
		from unnest($2::int[], $3::text[], $4::text[], $5::timestamptz[], $6::text[]) as pr(number, title, url, merged, issue)`,
		settings.DeployRepo, numbers, titles, urls, merged, linked); err != nil {
		return nil, fmt.Errorf("write the deploy runs' pull requests: %w", err)
	}
	r.result("setup deploy_repo=%s history_deployed_writes=%d groups=%d deploy_prs=%d first_run_id=%d runs_a_second=%.3f group_writes_a_second=%.3f",
		settings.DeployRepo, marked, len(plan.groups), len(plan.shipped), plan.firstRun, runsPerSecond, plan.groupsPerSecond)
	return plan, nil
}

// created is when GitHub created run index of the drive: its place in the drive's schedule,
// farmRunAge before it.
func (p *farmPlan) created(index int64) time.Time {
	return p.start.Add(time.Duration(index)*p.interval - farmRunAge).UTC().Truncate(time.Millisecond)
}

// removeUnshipped deletes the pull requests planFarmDrive wrote that no deploy run of the window
// shipped, once the measurement is over.
func (p *farmPlan) removeUnshipped(r *farmReport, pool *store.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tag, err := pool.Exec(ctx, `delete from delivery_pull_requests where repo = $1 and number = any($2) and deployed_event_at is null`,
		p.settings.DeployRepo, p.shipped)
	if err != nil {
		r.fail("delete the pull requests no deploy run shipped: %v", err)
		return
	}
	r.result("teardown deploy_prs_unshipped_deleted=%d", tag.RowsAffected())
}

// driveRuns writes a run each interval from the plan's start until ctx ends, one in four a deploy
// run, each on a goroutine of its own, so a run that waits for the lock delays none after it.
func (p *farmPlan) driveRuns(ctx context.Context, r *farmReport, writers map[string]*farmWriter) {
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	for index := int64(0); ; index++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(p.start.Add(time.Duration(index) * p.interval))):
		}
		inFlight.Go(func() { p.writeRun(r, writers, index) })
	}
}

// writeRun writes run index of the drive: its row, its jobs in one statement and, for a deploy run,
// its deployed write, each counted as its writer's. It writes on a context of its own, so the
// drive's end lets a run in flight land.
func (p *farmPlan) writeRun(r *farmReport, writers map[string]*farmWriter, index int64) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, runID, deploy := p.settings.DeployRepo, p.firstRun+index, index%farmDeployEvery == 0
	created := p.created(index)
	started := created.Add(10 * time.Second)
	run := delivery.DeliveryRun{
		Repo: repo, RunID: runID, HeadSHA: fmt.Sprintf("%040x", runID), HeadCommitAt: created.Add(-time.Minute),
		StartedAt: started, CreatedAt: &created, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, runID),
	}
	var names []string
	if deploy {
		run.Kind, run.HeadBranch, run.Event = delivery.DeliveryRunKindDeploy, new("main"), new("push")
		names = slices.Clone(farmDeployJobs)
		names[farmProductionIndex] = p.settings.ProductionJobNames[0]
	} else {
		number := p.shipped[int(index)%len(p.shipped)]
		run.Kind, run.PRNumber, run.HeadBranch, run.Event = delivery.DeliveryRunKindPRChecks, &number,
			new(fmt.Sprintf("feature/%d", number)), new("pull_request")
		for check := range farmPRChecksJobs - 1 {
			names = append(names, fmt.Sprintf("test / shard-%02d", check+1))
		}
		names = append(names, "checks-complete")
	}
	success := delivery.DeliveryJobConclusionSuccess
	jobs := make([]delivery.DeliveryRunJob, len(names))
	for i, name := range names {
		jobStarted := started.Add(time.Duration(i) * 30 * time.Second)
		jobDone := jobStarted.Add(20 * time.Second)
		jobs[i] = delivery.DeliveryRunJob{Name: name, StartedAt: &jobStarted, CompletedAt: &jobDone, Conclusion: &success}
	}
	completed := started.Add(time.Duration(len(names))*30*time.Second + 5*time.Second)
	runSuccess := delivery.DeliveryRunConclusionSuccess
	run.CompletedAt, run.Conclusion = &completed, &runSuccess
	if err := delivery.UpsertRun(ctx, writers["run"].pool, run); err != nil {
		writers["run"].failures.Add(1)
		r.fail("run %d: %v", runID, err)
		return
	}
	writers["run"].writes.Add(1)
	if err := delivery.UpsertRunJobs(ctx, writers["run-jobs"].pool, repo, runID, jobs, true); err != nil {
		writers["run-jobs"].failures.Add(1)
		r.fail("the jobs of run %d: %v", runID, err)
		return
	}
	writers["run-jobs"].writes.Add(1)
	if !deploy {
		return
	}
	written, err := delivery.RecordDeployedEvents(ctx, writers["deployed"].pool, p.settings, p.broker)
	if err != nil {
		writers["deployed"].failures.Add(1)
		r.fail("the deployed write after run %d: %v", runID, err)
	}
	writers["deployed"].writes.Add(int64(written))
}

// driveGroups writes a session group each 1/groupsPerSecond seconds from the plan's start until ctx
// ends, one after another as the route's transactions take the session groups' lock: a rename, and
// in place of every fifth a delete of the oldest group left.
func (p *farmPlan) driveGroups(ctx context.Context, r *farmReport, writer *farmWriter) {
	interval := time.Duration(float64(time.Second) / p.groupsPerSecond)
	for index := 0; ; index++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(p.start.Add(time.Duration(index) * interval))):
		}
		if len(p.groups) == 0 {
			writer.failures.Add(1)
			r.fail("no session group is left to write")
			return
		}
		write := func(tx pgx.Tx) error {
			if index%farmDeleteEvery == farmDeleteEvery-1 {
				id := p.groups[0]
				p.groups = p.groups[1:]
				return delivery.DeleteSessionGroup(ctx, tx, id)
			}
			id := p.groups[index%len(p.groups)]
			name := "Bench crew " + id
			if p.renamed[id] {
				name += " renamed"
			}
			p.renamed[id] = !p.renamed[id]
			_, err := delivery.UpdateSessionGroup(ctx, tx, id, delivery.SessionGroupUpdate{Name: &name}, actor)
			return err
		}
		tx, err := writer.pool.Begin(ctx)
		if err == nil {
			err = write(tx)
			if err == nil {
				err = tx.Commit(ctx)
			}
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			writer.failures.Add(1)
			r.fail("session group write %d: %v", index, err)
			continue
		}
		writer.writes.Add(1)
	}
}

// lockWait is one wait for the change log's lock the samples saw: the backend, when its wait
// began, and the longest a sample saw it wait.
type lockWait struct {
	backend uint32
	since   time.Time
	waited  time.Duration
}

// sampleLockWaits starts reading, every millisecond on a connection of its own, each backend that
// waits for the two-key transaction advisory lock (key1, key2) in pool's database, with when its
// wait began (pg_locks.waitstart) and how long it has waited by the database's clock, and answers
// stop, which ends the reading and answers each wait once, with the longest wait a sample saw. A
// wait shorter than a sample's interval may be seen by none; the caller counts such a statement's
// wait as none.
func sampleLockWaits(t *testing.T, pool *store.Pool, key1, key2 int32) (stop func() []lockWait) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		cancel()
		t.Fatalf("connect the wait sampler: %v", err)
	}
	query := fmt.Sprintf(`
		select l.pid, l.waitstart, extract(epoch from clock_timestamp() - l.waitstart)::float8
		from pg_locks l
		where l.locktype = 'advisory' and l.classid = %d and l.objid = %d and l.objsubid = 2 and not l.granted
		  and l.waitstart is not null
		  and l.database = (select oid from pg_database where datname = current_database())
	`, key1, key2)
	type key struct {
		backend uint32
		since   time.Time
	}
	done := make(chan struct{})
	seen := map[key]time.Duration{}
	var failure error
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			rows, err := conn.Query(ctx, query)
			if err == nil {
				var backend uint32
				var since time.Time
				var seconds float64
				_, err = pgx.ForEachRow(rows, []any{&backend, &since, &seconds}, func() error {
					waited := time.Duration(seconds * float64(time.Second))
					if waited > seen[key{backend, since}] {
						seen[key{backend, since}] = waited
					}
					return nil
				})
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
	return func() []lockWait {
		t.Helper()
		cancel()
		<-done
		_ = conn.Close(context.Background())
		if failure != nil {
			t.Fatalf("sample the waiters of the advisory lock (%d, %d): %v", key1, key2, failure)
		}
		waits := make([]lockWait, 0, len(seen))
		for one, waited := range seen {
			waits = append(waits, lockWait{backend: one.backend, since: one.since, waited: waited})
		}
		return waits
	}
}

// farmServer is the server the run boots on the loaded database.
type farmServer struct {
	cmd     *exec.Cmd
	url     string
	log     string
	started time.Time
	done    chan struct{}
	once    sync.Once
}

// startFarmServer starts the server built at this head with the environment the bench's servers
// run under: no NATS, no GitHub App, the header identity and an agent token, its output in a log
// under the work directory.
func startFarmServer(o farmOptions) (*farmServer, error) {
	home := filepath.Join(o.work, "home-"+o.arm)
	web := filepath.Join(o.work, "web")
	for _, dir := range []string{home, web} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	logPath := filepath.Join(o.work, "server-"+o.arm+".log")
	log, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(o.server)
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		// The server refuses to start without an agent token, and nothing here sends one: a value
		// drawn for this run is enough, and no fixed one is written down.
		"DATABASE_URL=" + o.database, "DISPATCH_AGENT_TOKEN=" + rand.Text(), "DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1", "DISPATCH_LISTEN_HOST=127.0.0.1", "DISPATCH_PORT=" + strconv.Itoa(o.port),
		"DISPATCH_WEB_DIST=" + web, "DISPATCH_NATS_DISABLED=1",
	}
	cmd.Stdout, cmd.Stderr = log, log
	server := &farmServer{cmd: cmd, url: fmt.Sprintf("http://127.0.0.1:%d", o.port), log: logPath, done: make(chan struct{})}
	server.started = time.Now()
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(server.done)
	}()
	return server, nil
}

// waitHealthy waits until /healthz answers, and answers the commit it names, the build the
// consumer records against the project it reads whole.
func (s *farmServer) waitHealthy(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-s.done:
			return "", errors.New("the server exited before it served")
		default:
		}
		build, err := serverCommit(ctx, s.url)
		if err == nil {
			return build, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no answer within %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// serverCommit is /healthz's commit.
func serverCommit(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var health struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return "", err
	}
	if health.Commit == "" {
		return "", errors.New("the server answers no commit: build it stamped (-ldflags -X main.buildCommit=<commit>)")
	}
	return health.Commit, nil
}

// stop stops the server with SIGTERM and waits for it to exit.
func (s *farmServer) stop() {
	s.once.Do(func() {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(time.Minute):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
}

// waitWholeRead waits until the server has read INFRA whole under build and drained every change
// the read put in the outbox, and answers when it saw that and how many rows the read wrote.
func waitWholeRead(ctx context.Context, pool *store.Pool, build string, timeout time.Duration) (time.Time, int64, error) {
	deadline := time.Now().Add(timeout)
	for {
		var computed string
		var rows, pending int64
		err := pool.QueryRow(ctx, `
			select coalesce((select computed_build from issue_facts_computed where project_key = $1), ''),
			       coalesce((select computed_rows from issue_facts_computed where project_key = $1), 0),
			       (select count(*) from delivery_changes_pending)`, Project).Scan(&computed, &rows, &pending)
		if err != nil {
			return time.Time{}, 0, err
		}
		if build != "" && computed == build && pending == 0 {
			return time.Now(), rows, nil
		}
		if time.Now().After(deadline) {
			return time.Time{}, 0, fmt.Errorf("after %s %s's record names build %q, not %q, and the outbox holds %d rows",
				timeout, Project, computed, build, pending)
		}
		select {
		case <-ctx.Done():
			return time.Time{}, 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// R7's bounds: each gated writer's p99 hold and the intake's median wait; and the fewest holds the
// drain and each other gated writer must take for its p99 to be read.
const (
	farmHoldBound   = 100 * time.Millisecond
	farmWaitBound   = 20 * time.Millisecond
	farmDrainFloor  = 40
	farmWriterFloor = 150
)

// gatedWriters are the four writers R7 bounds; intakeWriters the writers whose statements wait.
var (
	gatedWriters  = []string{"drain", "session-group", "run-jobs", "deployed"}
	intakeWriters = []string{"run", "run-jobs", "deployed", "session-group"}
)

// judgeFarmHolds classifies each hold and each wait by its writer, and prints and judges them by
// the run's gate.
func judgeFarmHolds(r *farmReport, o farmOptions, writers map[string]*farmWriter, holds []storetest.AdvisoryHold, waits []lockWait, drainTakes int64) {
	writerOf := func(backend uint32) string {
		for name, writer := range writers {
			if writer.owns(backend) {
				return name
			}
		}
		return ""
	}
	byWriter := map[string][]time.Duration{}
	others := map[string]int{}
	for _, hold := range holds {
		name := writerOf(hold.PID)
		switch {
		case name != "":
		case strings.Contains(hold.Query, "delivery_changes_drain"):
			name = "drain"
		default:
			name = "other"
			others[oneLineQuery(hold.Query)]++
		}
		byWriter[name] = append(byWriter[name], hold.Held())
	}
	// How many times each writer took the lock: an intake writer once for each write it committed,
	// and the drain once for each transaction that moved rows (drainTakes). A take no sample saw held
	// the lock less than a sample's interval; the p99 judged is the sampled holds' alone, which those
	// would only lower.
	takes := map[string]int64{"drain": drainTakes}
	for _, name := range intakeWriters {
		takes[name] = writers[name].writes.Load()
	}
	for _, name := range append(slices.Clone(gatedWriters), "run", "other") {
		held := byWriter[name]
		failed := "-"
		if writer := writers[name]; writer != nil {
			failed = strconv.FormatInt(writer.failures.Load(), 10)
		}
		r.result("writer=%s takes=%d failed_writes=%s holds_seen=%d p99_ms=%.1f longest_ms=%.1f median_ms=%.1f", name, takes[name], failed,
			len(held), ms(durationPercentile(held, 0.99)), ms(slices.Max(append(held, 0))), ms(durationPercentile(held, 0.5)))
	}
	for _, query := range slices.Sorted(maps.Keys(others)) {
		r.result("other_holds count=%d query=%q", others[query], query)
	}
	deployRuns := (writers["run-jobs"].writes.Load() + farmDeployEvery - 1) / farmDeployEvery
	r.result("deploy_runs_about=%d deployed_writes=%d", deployRuns, writers["deployed"].writes.Load())

	// Each intake write takes the lock once; a write no sample saw wait waited less than a sample's
	// interval, and counts as no wait.
	var waited []time.Duration
	seen := map[string]int{}
	for _, wait := range waits {
		name := writerOf(wait.backend)
		if !slices.Contains(intakeWriters, name) {
			continue
		}
		seen[name]++
		waited = append(waited, wait.waited)
	}
	statements := 0
	for _, name := range intakeWriters {
		statements += int(writers[name].writes.Load())
	}
	all := slices.Clone(waited)
	for range max(statements-len(waited), 0) {
		all = append(all, 0)
	}
	waitMedian, waitP99 := durationPercentile(all, 0.5), durationPercentile(all, 0.99)
	r.result("intake statements=%d waits_seen=%d by_writer=%v wait_median_ms=%.1f wait_p99_ms=%.1f longest_wait_ms=%.1f",
		statements, len(waited), seen, ms(waitMedian), ms(waitP99), ms(slices.Max(append(waited, 0))))

	switch o.gate {
	case "stated":
		for _, name := range gatedWriters {
			held := byWriter[name]
			if p99 := durationPercentile(held, 0.99); p99 > farmHoldBound {
				r.fail("writer=%s p99 hold %.1f ms is above %s", name, ms(p99), farmHoldBound)
			}
			floor := farmWriterFloor
			if name == "drain" {
				floor = farmDrainFloor
			}
			if takes[name] < int64(floor) {
				r.fail("writer=%s took the lock %d times, fewer than %d", name, takes[name], floor)
			}
		}
		if statements == 0 {
			r.fail("no intake statement was written, so no wait was measured")
		} else if waitMedian > farmWaitBound {
			r.fail("the intake's median wait %.1f ms is above %s", ms(waitMedian), farmWaitBound)
		}
	case "sleep":
		// The control works when the drain, which sleeps under the lock, misses the bound the stated
		// run is held to, and takes the lock as often as that run must for its p99 to be read.
		held := byWriter["drain"]
		p99 := durationPercentile(held, 0.99)
		r.result("control drain takes=%d holds_seen=%d p99_ms=%.1f bound_ms=%.0f floor=%d", takes["drain"], len(held), ms(p99),
			ms(farmHoldBound), farmDrainFloor)
		failedAsItMust := true
		if takes["drain"] < farmDrainFloor {
			failedAsItMust = false
			r.fail("control did not fail on the hold alone: the drain took the lock %d times, fewer than %d", takes["drain"], farmDrainFloor)
		}
		if p99 <= farmHoldBound {
			failedAsItMust = false
			r.fail("control did not fail: the drain's p99 hold %.1f ms is not above %s, so the measurement cannot see the hold it bounds",
				ms(p99), farmHoldBound)
		}
		if failedAsItMust {
			r.result("control failed on the hold as it must: the drain's p99 hold %.1f ms is above %s over %d takes", ms(p99),
				farmHoldBound, takes["drain"])
		}
	}
}

// durationPercentile is the nearest-rank p-th percentile of values, 0 for none.
func durationPercentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(values))
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// ms is d in milliseconds.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// oneLineQuery is a statement on one line, cut to 160 bytes.
func oneLineQuery(query string) string {
	text := strings.Join(strings.Fields(query), " ")
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return text
}
