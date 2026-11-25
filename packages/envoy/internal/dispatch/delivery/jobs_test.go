package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newFakeJobClock is the JobIntake's clock in a test: a clocktest.Fake, whose Advance fires every
// timer due by the new time in due order and returns once each has run, so a read a timer starts
// has finished when Advance returns.
func newFakeJobClock() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
}

// jobsFake is a fake GitHub answering run 500's jobs: a build job and the production job, each
// finished as the test sets them, counting the reads.
type jobsFake struct {
	*fakeGitHub
	calls      atomic.Int32
	mu         sync.Mutex
	production map[string]any
	answer     func(w http.ResponseWriter, r *http.Request) bool
}

func newJobsFake(t *testing.T) *jobsFake {
	t.Helper()
	fake := &jobsFake{fakeGitHub: newFakeGitHub(t)}
	fake.production = map[string]any{
		"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z",
		"completed_at": "2026-09-03T12:09:00Z", "conclusion": "success",
	}
	fake.handle("GET /repos/acme/widgets/actions/runs/500/jobs", func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		fake.mu.Lock()
		answer, production := fake.answer, fake.production
		fake.mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		mustEncode(t, w, map[string]any{"total_count": 2, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
			production,
		}})
	})
	return fake
}

// seedRunningDeployRun stores run 500 of the deploy repository, started and not ended.
func seedRunningDeployRun(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
		t.Fatalf("seed run 500: %v", err)
	}
}

func jobPayload(repo string, runID int64) string {
	payload, _ := json.Marshal(map[string]string{
		"kind": "job", "repo": repo, "run_id": fmt.Sprint(runID), "job_id": "9001",
		"name": "widgets-release / widgets-release", "status": "completed", "conclusion": "success",
	})
	return string(payload)
}

func readJobsComplete(t *testing.T, ctx context.Context, pool *store.Pool, runID int64) bool {
	t.Helper()
	var complete bool
	if err := pool.QueryRow(ctx, `select jobs_complete from delivery_runs where repo = 'acme/widgets' and run_id = $1`, runID).Scan(&complete); err != nil {
		t.Fatalf("read jobs_complete of run %d: %v", runID, err)
	}
	return complete
}

// TestAJobOfATrackedDeployRunRecordsItsJobsWhileItRuns drives the consumer end to end: a job event
// for run 500, which Dispatch tracks and which has not ended, makes one read of its jobs once the
// burst goes quiet. The production job is stored finished, another job in that same listing is
// still running, so the run's jobs stay incomplete, and the change log holds a run_job change.
func TestAJobOfATrackedDeployRunRecordsItsJobsWhileItRuns(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	before := latestChange(t, ctx, pool)
	fake := newJobsFake(t)
	fake.mu.Lock()
	fake.answer = func(w http.ResponseWriter, _ *http.Request) bool {
		mustEncode(t, w, map[string]any{"total_count": 3, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
			{"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z", "completed_at": "2026-09-03T12:09:00Z", "conclusion": "success"},
			{"name": "post-release", "started_at": "2026-09-03T12:10:00Z", "completed_at": nil, "conclusion": nil},
		}})
		return true
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	natsClient := intakeTestClient(t)
	jobs := newJobIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); jobs.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the job intake's Run to return once its context ended", done)
	})
	waitFor(t, func() bool {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchJobDurable)
		return err == nil && info.PushBound
	}, func() string { return "the job intake never bound its durable" })

	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.job.500.completed", jobPayload("acme/widgets", 500))
	waitFor(t, func() bool { return len(clock.Armed()) == 1 }, func() string { return "the job event scheduled no read" })
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("read the jobs %d times before the burst went quiet", calls)
	}
	clock.Advance(jobQuiet)

	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("jobs reads = %d, want 1", calls)
	}
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	var production *DeliveryRunJob
	for i := range stored {
		if stored[i].Name == "widgets-release / widgets-release" {
			production = &stored[i]
		}
	}
	if production == nil || production.CompletedAt == nil || production.Conclusion == nil || *production.Conclusion != DeliveryJobConclusionSuccess {
		t.Fatalf("the production job = %+v, want it stored finished with success", production)
	}
	if readJobsComplete(t, ctx, pool, 500) {
		t.Fatal("jobs_complete = true for a run that has not ended")
	}
	changes := changesAfter(t, ctx, pool, before)
	if !slices.ContainsFunc(changes, func(c Change) bool { return c.Kind == "run_job" && c.Ref == "500" }) {
		t.Fatalf("changes = %+v, want a run_job change of run 500", changes)
	}
}

// TestAJobIntakeRecordsTheConfiguredProductionApplyStep exercises the early job-event path: it
// keeps the configuration observed with the event through the delayed listing and stores that
// listed job's configured step.
func TestAJobIntakeRecordsTheConfiguredProductionApplyStep(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	const step = "Run ./.github/actions/apply"
	settings.ProductionApplyStepName = new(step)
	if _, err := PutSettings(ctx, pool, settings, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("set production apply step: %v", err)
	}
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	fake.mu.Lock()
	fake.production = map[string]any{
		"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z",
		"completed_at": "2026-09-03T12:09:00Z", "conclusion": "success",
		"steps": []map[string]any{{"name": step, "conclusion": "success"}},
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	jobs.receive(ctx, jobPayload("acme/widgets", 500))
	clock.Advance(jobQuiet)
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil || len(stored) != 2 {
		t.Fatalf("ListRunJobs = %+v, %v; want the two fetched jobs", stored, err)
	}
	for _, job := range stored {
		if job.Name != "widgets-release / widgets-release" {
			continue
		}
		if job.ApplyStep == nil || *job.ApplyStep != step || job.ApplyStepConclusion == nil || *job.ApplyStepConclusion != DeliveryJobConclusionSuccess {
			t.Fatalf("production job = %+v, want configured apply step success", job)
		}
		return
	}
	t.Fatal("the stored jobs have no production job")
}

// TestAJobOfAnUntrackedRunCostsNoGitHubCall: a job of a run Dispatch does not store schedules
// nothing and asks GitHub for nothing, not even a token.
func TestAJobOfAnUntrackedRunCostsNoGitHubCall(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	jobs.receive(ctx, jobPayload("acme/widgets", 999))
	clock.Advance(jobMaxWait)
	if armed, calls, mints := len(clock.Armed()), fake.calls.Load(), fake.tokenMints.Load(); armed != 0 || calls != 0 || mints != 0 {
		t.Fatalf("an untracked run's job: %d timers, %d jobs reads, %d token mints, want none", armed, calls, mints)
	}
}

// TestAJobOfAnotherRepositoryIsIgnored: a job of a repository other than the deploy repository
// schedules nothing, even when a run of that number is stored.
func TestAJobOfAnotherRepositoryIsIgnored(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	jobs.receive(ctx, jobPayload("acme/other", 500))
	clock.Advance(jobMaxWait)
	if armed, calls, mints := len(clock.Armed()), fake.calls.Load(), fake.tokenMints.Load(); armed != 0 || calls != 0 || mints != 0 {
		t.Fatalf("another repository's job: %d timers, %d jobs reads, %d token mints, want none", armed, calls, mints)
	}
}

// TestABurstOfJobsOfOneRunReadsItsJobsOnce: thirty job events of one run within a second are one
// read.
func TestABurstOfJobsOfOneRunReadsItsJobsOnce(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	for range 30 {
		jobs.receive(ctx, jobPayload("acme/widgets", 500))
		clock.Advance(time.Second / 30)
	}
	clock.Advance(jobQuiet)
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("thirty events within a second made %d reads, want 1", calls)
	}
}

// TestABurstThatKeepsComingStillReadsWithinTenSeconds: an event every second for fifteen seconds
// never goes quiet, yet the run is read at ten seconds, and again once the rest of the burst ends.
func TestABurstThatKeepsComingStillReadsWithinTenSeconds(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	for second := range 15 {
		jobs.receive(ctx, jobPayload("acme/widgets", 500))
		clock.Advance(time.Second)
		if second == 9 {
			if calls := fake.calls.Load(); calls != 1 {
				t.Fatalf("reads at ten seconds = %d, want 1", calls)
			}
		}
	}
	clock.Advance(jobQuiet)
	if calls := fake.calls.Load(); calls != 2 {
		t.Fatalf("reads once the burst ended = %d, want 2", calls)
	}
}

// TestAStaleJobsReadStaysForTheBackfill: the job listing can see the production job running while
// the run's completed workflow event reaches Dispatch between that listing and the jobs write. The
// stale listing is the first job-row write, so it must leave the run available for the completion
// listing rather than permanently mark it complete.
func TestAStaleJobsReadStaysForTheBackfill(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	ended := started.Add(15 * time.Minute)
	success := DeliveryRunConclusionSuccess
	run := changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)
	run.CompletedAt, run.Conclusion = &ended, &success
	completed := make(chan error, 1)
	fake.mu.Lock()
	fake.production = map[string]any{
		"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z",
		"completed_at": nil, "conclusion": nil,
	}
	fake.answer = func(_ http.ResponseWriter, _ *http.Request) bool {
		completed <- UpsertRun(ctx, pool, run)
		return false
	}
	fake.mu.Unlock()

	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), newFakeJobClock(), nil)
	jobs.read(ctx, runKey{repo: "acme/widgets", runID: 500}, Production{})
	if err := testwait.Receive(t, "the run completion the job read's answer stores", completed); err != nil {
		t.Fatalf("store the run completion: %v", err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{500})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs after stale read: %v", err)
	}
	if skip[500] {
		t.Fatal("a stale jobs read marked the run complete, so the backfill cannot finish its production job")
	}

	finished := started.Add(9 * time.Minute)
	jobSuccess := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 500, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
		{Repo: "acme/widgets", RunID: 500, Name: "widgets-release / widgets-release", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("store the completion listing: %v", err)
	}
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs after completion listing: %v", err)
	}
	for _, job := range stored {
		if job.CompletedAt == nil || !job.CompletedAt.Equal(finished) {
			t.Fatalf("job %q after the completion listing = %+v, want completed at %v", job.Name, job, finished)
		}
	}
}

// TestAReadThatLandsAfterTheRunEndedUndoesNothing: a read in flight while the run ends -- its end
// writes every job finished -- answers with the production job still running, and lands after:
// the stored jobs stay finished and the run's jobs stay complete.
func TestAReadThatLandsAfterTheRunEndedUndoesNothing(t *testing.T) {
	t.Parallel()
	// githubapp's own bound on a call runs on the wall clock, so this row's client ends a call only
	// at the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp): only the row's release may
	// end the read GitHub holds, which then lands after the run ended. At githubapp.RequestTimeout
	// (20 s), a slow run would end the held read first, and the client would ask again, so the row
	// would pass with no late read landing.
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	entered, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	fake.mu.Lock()
	fake.production = map[string]any{"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z", "completed_at": nil, "conclusion": nil}
	fake.answer = func(w http.ResponseWriter, r *http.Request) bool {
		close(entered)
		<-released
		return false
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t))), clock, loadedSettingsCache(t, ctx, pool))
	jobs.receive(ctx, jobPayload("acme/widgets", 500))
	// The move that fires the read returns once the read has, which waits at GitHub for the row's
	// release, so it is made off the test goroutine (clocktest.Fake.StartMove). The row joins it,
	// and so does a cleanup, for a row that failed before its own join; once the move has returned,
	// a join returns at once. A read that never returns fails the row at the join, naming the move
	// and the read's timer.
	joinRead := clock.StartMove("the move that ran the job intake's read", clock.Now().Add(jobQuiet))
	// After the fake's server, so they run before its Close, which waits for the handler held at
	// GitHub: a failure before the release releases the read and waits for the move that ran it,
	// and a give-up of that wait once the row has failed is logged rather than failed again.
	t.Cleanup(func() { joinRead(failOnce{t}) })
	t.Cleanup(release)
	testwait.Receive(t, "the job intake's read, which the move fires, to reach GitHub", entered)

	// The run's end, as Intake writes it: the run concluded and every job finished.
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	ended, finished := started.Add(15*time.Minute), started.Add(9*time.Minute)
	success := DeliveryRunConclusionSuccess
	run := changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)
	run.CompletedAt, run.Conclusion = &ended, &success
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("end the run: %v", err)
	}
	jobSuccess := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 500, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
		{Repo: "acme/widgets", RunID: 500, Name: "widgets-release / widgets-release", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("store the ended run's jobs: %v", err)
	}
	release()
	joinRead(t)

	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	for _, job := range stored {
		if job.CompletedAt == nil || job.Conclusion == nil || *job.Conclusion != jobSuccess {
			t.Fatalf("job %q after the late read = %+v, want it still finished", job.Name, job)
		}
	}
	if !readJobsComplete(t, ctx, pool, 500) {
		t.Fatal("jobs_complete went back to false after a late read")
	}
}

// TestAJobsReadThatFailsIsLoggedAndTheRunIsReadAtItsEnd: GitHub answering 500 to the jobs read is
// a WARN and changes no row; the run stays on the backfill's list, and its end reads its jobs.
func TestAJobsReadThatFailsIsLoggedAndTheRunIsReadAtItsEnd(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	before := latestChange(t, ctx, pool)
	fake := newJobsFake(t)
	fake.mu.Lock()
	fake.answer = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
		return true
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	jobs.receive(ctx, jobPayload("acme/widgets", 500))
	clock.Advance(jobQuiet)

	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("jobs reads = %d, want 1", calls)
	}
	warned := slices.ContainsFunc(logs(), func(record map[string]any) bool {
		return record["level"] == "WARN" && record["run_id"] == float64(500)
	})
	if !warned {
		t.Fatalf("no WARN naming run 500 among %v", logs())
	}
	if changes := changesAfter(t, ctx, pool, before); len(changes) != 0 {
		t.Fatalf("a failed read logged %+v, want nothing", changes)
	}
	if stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500); err != nil || len(stored) != 0 {
		t.Fatalf("jobs after a failed read = %+v, %v, want none", stored, err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{500})
	if err != nil || skip[500] {
		t.Fatalf("the backfill skips run 500 after a failed read (%v), want it listed", err)
	}
}

// concurrentJobClock is a clocktest.Fake whose timers' functions all run at once, each on a
// goroutine of its own beside the others, when the test calls fireAll: the way a backlog that
// drains faster than jobQuiet ends every run's burst together. The Fake's own move runs one
// function after another and waits for each; this one's functions only start theirs in fired.
type concurrentJobClock struct {
	*clocktest.Fake
	fired sync.WaitGroup
}

func newConcurrentJobClock() *concurrentJobClock {
	return &concurrentJobClock{Fake: clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))}
}

func (c *concurrentJobClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return c.Fake.AfterFunc(d, func() { c.fired.Go(f) })
}

// fireAll moves the fake past every timer armed, starting each one's function on a goroutine of
// its own, and returns join, which waits on the test goroutine for every function it started to
// return (testwait.Returns), naming that wait. Each call of join waits again, so a row may join in
// its body and again in a cleanup that runs when the body ended before its join.
func (c *concurrentJobClock) fireAll() (join func(t testing.TB)) {
	if armed := c.Armed(); len(armed) > 0 {
		c.Advance(max(armed[len(armed)-1].Sub(c.Now()), 0))
	}
	return func(t testing.TB) {
		t.Helper()
		testwait.Returns(t, "every run's jobs read the fake clock fired to return once its listing was released", c.fired.Wait)
	}
}

// TestTheJobIntakeReadsAtMostABoundedNumberOfRunsAtOnce: when many tracked runs' bursts end
// together, as on the first start after an outage drains its backlog, the job intake asks GitHub for
// at most reconcilePartialConcurrency runs' jobs at a time, and reads every run once. Unbounded, it
// opened one listing per run at once, which past GitHub's 100 concurrent requests rate-limits the
// App. The held listings do not watch the request's context, so the client's own bound on a call is
// the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp): at githubapp.RequestTimeout (20 s)
// it would give each held listing up and ask again, and GitHub would see more listings at once than
// there are slots.
func TestTheJobIntakeReadsAtMostABoundedNumberOfRunsAtOnce(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	const runs = 3 * reconcilePartialConcurrency
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for id := int64(600); id < 600+runs; id++ {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
			t.Fatalf("seed run %d: %v", id, err)
		}
	}
	var inFlight, most, reads atomic.Int32
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}/jobs", func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		<-release
		inFlight.Add(-1)
		reads.Add(1)
		mustEncode(t, w, map[string]any{"total_count": 1, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
		}})
	})
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handlers it holds.
	t.Cleanup(releaseOnce)

	clock := newConcurrentJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, github, clock, nil)
	for id := int64(600); id < 600+runs; id++ {
		jobs.schedule(runKey{repo: "acme/widgets", runID: id}, Production{})
	}
	joinReads := clock.fireAll()
	// Runs before the pool's and the fake's cleanups, so no read outlives the test into them.
	t.Cleanup(func() {
		releaseOnce()
		joinReads(t)
	})
	waitFor(t, func() bool { return inFlight.Load() >= reconcilePartialConcurrency }, func() string {
		return fmt.Sprintf("%d jobs reads in flight, want %d", inFlight.Load(), reconcilePartialConcurrency)
	})
	// Every run's burst has ended. A read past the slots would start on a goroutine that shows
	// nothing until its request reaches GitHub, so the span gives such a read the time to arrive.
	testwait.Span(t, 500*time.Millisecond, "a run waiting for a slot is a goroutine parked on the slot channel, which neither the intake's state (its pending runs are gone as each fires) nor the fake GitHub, which sees only reads that started, shows, and the code does network I/O, so no synctest bubble reaches it")
	if got := most.Load(); got != reconcilePartialConcurrency {
		t.Fatalf("%d of %d runs' jobs read at once, want at most %d", got, runs, reconcilePartialConcurrency)
	}

	releaseOnce()
	joinReads(t)
	if got := reads.Load(); got != runs {
		t.Fatalf("jobs reads = %d, want one for each of the %d runs", got, runs)
	}
	if got := most.Load(); got != reconcilePartialConcurrency {
		t.Fatalf("%d runs' jobs read at once by the end, want at most %d", got, reconcilePartialConcurrency)
	}
}

// TestAQueuedJobReadsDeadlineStartsOnceItHoldsItsSlot: a run whose burst ends while every read slot
// is taken waits for one, and its read's deadline (intakeMessageTimeout, on the intake's clock)
// starts only once it holds the slot. Here every slot is held by a read whose listing waits on a
// gate, one more run waits for a slot, and the clock moves past the deadlines the slot holders
// armed. A deadline started before the wait would have expired with theirs: the waiting run's read
// would log that it could not read its run's jobs and store none. Its deadline is armed once it
// holds a slot instead, a whole intakeMessageTimeout past the moment it took it, and its read stores
// the run's jobs.
func TestAQueuedJobReadsDeadlineStartsOnceItHoldsItsSlot(t *testing.T) {
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	// Every read slot taken, and one run waiting for a slot.
	const runs = reconcilePartialConcurrency + 1
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for id := int64(700); id < 700+runs; id++ {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
			t.Fatalf("seed run %d: %v", id, err)
		}
	}
	// The slot holders' listings wait on held, and the listing of the run that waited, the first
	// to reach GitHub once every slot holder has, on waited. The client's own bound on a call is
	// the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp), so only the row's release or the
	// read's own deadline ends a held listing: at githubapp.RequestTimeout (20 s) the slot holders'
	// reads would end while the row stages its next step and hand their slots to the waiting run early.
	held, waited := make(chan struct{}), make(chan struct{})
	releaseHeld, releaseWaited := sync.OnceFunc(func() { close(held) }), sync.OnceFunc(func() { close(waited) })
	var mu sync.Mutex
	reached := map[string]bool{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gate := held
		if len(reached) == reconcilePartialConcurrency {
			gate = waited
		}
		reached[r.PathValue("run_id")] = true
		mu.Unlock()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		mustEncode(t, w, map[string]any{"total_count": 1, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
		}})
	})
	reachedGitHub := func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(reached)
	}

	clock := newConcurrentJobClock()
	jobs := newJobIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t))), clock, nil)
	for id := int64(700); id < 700+runs; id++ {
		jobs.schedule(runKey{repo: "acme/widgets", runID: id}, Production{})
	}
	joinReads := clock.fireAll()
	t.Cleanup(func() {
		releaseHeld()
		releaseWaited()
		joinReads(t)
	})

	// Every slot is held by a read whose listing waits at GitHub; the run left over waits for a slot.
	testwait.Eventually(t, "every slot holder's listing to reach GitHub", func() bool {
		return len(reachedGitHub()) == reconcilePartialConcurrency
	})
	var waiter string
	for id := int64(700); id < 700+runs; id++ {
		if !reachedGitHub()[fmt.Sprint(id)] {
			waiter = fmt.Sprint(id)
		}
	}
	failedRead := func(runID string) map[string]any {
		for _, record := range logs() {
			switch record["msg"] {
			case "dispatch delivery: read the jobs of a running deploy run; its end reads them again", "dispatch delivery: store the jobs of a running deploy run":
				if fmt.Sprint(record["run_id"]) == runID {
					return record
				}
			}
		}
		return nil
	}

	// The clock moves past every deadline the slot holders armed while run waiter waits for a slot,
	// and the slots are let go.
	clock.Advance(intakeMessageTimeout)
	tookSlot := clock.Now()
	releaseHeld()

	// The run that waited takes a slot and reads, unless its deadline had already passed. A try
	// that finds neither reads the runs whose listings reached GitHub.
	var failure map[string]any
	testwait.Until(t, fmt.Sprintf("run %s's listing to reach GitHub, or its read to fail", waiter), func() (string, bool) {
		reached := reachedGitHub()
		if failure = failedRead(waiter); failure != nil || reached[waiter] {
			return fmt.Sprintf("run %s's listing reached GitHub (%t) or its read failed (%v)", waiter, reached[waiter], failure), true
		}
		return fmt.Sprintf("run %s's read logged no failure; the listings that reached GitHub are runs %v", waiter, slices.Sorted(maps.Keys(reached))), false
	})
	if failure != nil {
		t.Fatalf("run %s's read failed once it held a slot, the clock having moved %s past the moment its burst ended while it waited for one: %v", waiter, intakeMessageTimeout, failure)
	}
	armed := clock.AwaitArmed(t, fmt.Sprintf("run %s's read deadline, armed as it took a slot", waiter), 1)
	if want := tookSlot.Add(intakeMessageTimeout); len(armed) != 1 || !armed[0].Equal(want) {
		t.Fatalf("with run %s reading, the clock holds the deadlines %v, want only %v: %s past %v, when it took its slot", waiter, armed, want, intakeMessageTimeout, tookSlot)
	}
	releaseWaited()
	// A try that finds neither reads whether the run's jobs are stored and the runs whose listings
	// reached GitHub.
	testwait.Until(t, fmt.Sprintf("run %s's jobs to be stored, or its read to fail", waiter), func() (string, bool) {
		var stored bool
		if err := pool.QueryRow(ctx, `select exists (select from delivery_run_jobs where repo = 'acme/widgets' and run_id = $1)`, waiter).Scan(&stored); err != nil {
			t.Fatalf("read run %s's stored jobs: %v", waiter, err)
		}
		if failure = failedRead(waiter); stored || failure != nil {
			return fmt.Sprintf("run %s's jobs stored (%t) or its read failed (%v)", waiter, stored, failure), true
		}
		return fmt.Sprintf("run %s's jobs stored: %t; its read logged no failure; the listings that reached GitHub are runs %v", waiter, stored, slices.Sorted(maps.Keys(reachedGitHub()))), false
	})
	if failure != nil {
		t.Fatalf("run %s's read failed once it was released: %v", waiter, failure)
	}
	if failure := failedRead(waiter); failure != nil {
		t.Fatalf("run %s's read stored its jobs and logged a failure: %v", waiter, failure)
	}
}

// holdingJobClock is a fake job clock whose first timer holds the handling that armed it until the
// row releases it. It is the one place a job event's handling can be held: deliver acknowledges
// on receipt, reads one indexed row and arms a timer, and calls GitHub not at all.
type holdingJobClock struct {
	*clocktest.Fake
	// armedFirst is closed as the first handling reaches the timer it arms.
	armedFirst chan struct{}
	release    chan struct{}
	first      sync.Once
}

func newHoldingJobClock() *holdingJobClock {
	return &holdingJobClock{
		Fake:       newFakeJobClock(),
		armedFirst: make(chan struct{}), release: make(chan struct{}),
	}
}

func (c *holdingJobClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	timer := c.Fake.AfterFunc(d, f)
	holding := false
	c.first.Do(func() { holding = true })
	if holding {
		close(c.armedFirst)
		<-c.release
	}
	return timer
}

// TestAJobEventHandledAfterTheIntakeStoppedIsHandedBackToTheNextHolderAtOnce drives the moment
// the shutdown leaves open: Run's context is done, so run has returned and nothing it reads
// under that context can succeed, and the stop has not yet set the delivery's flag
// (intakeDelivery.stopping). In that moment the keyed runner can still start a job event that
// queued behind the one in hand. Such an event must not be acknowledged: its read would fail
// under the cancelled context, with the ERROR "read whether a job's run is tracked", and the
// early signal it carries would be spent on nothing.
//
// What the row decides is where that event goes, not which NATS call sent it there: the next
// holder of the durable is handed it. The durable here waits an hour for an acknowledgement, not
// jobAckWait's 30 s, and that is what makes the row a proof rather than a restatement of what NATS
// does anyway: an event the stopped intake acknowledged reaches nobody at all, and one it left
// unanswered would reach the next holder only when that hour ran out, past any run of the row, so
// the wait for it fails for both; the hand-back reaches the next holder rather than this stopped
// delivery because the stop drains the subscription before it answers anything
// (intakeDelivery.stop).
func TestAJobEventHandledAfterTheIntakeStoppedIsHandedBackToTheNextHolderAtOnce(t *testing.T) {
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(501, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
		t.Fatalf("seed run 501: %v", err)
	}
	fake := newJobsFake(t)
	clock := newHoldingJobClock()
	natsClient := intakeTestClient(t)
	jobs := newJobIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), clock, loadedSettingsCache(t, ctx, pool))
	// j.ctx is Run's context: this row sets the field Run sets and ends it where Run's caller
	// would, so the moment below is the one Run leaves open rather than one the row invented.
	runCtx, endRun := context.WithCancel(ctx)
	jobs.ctx = runCtx

	// The durable as a take-over leaves it, created here because Run is not what binds it, but for
	// its ack wait, a day, so that only a hand-back reaches the next holder within the row: staged
	// state for the server's clock, longer than any -timeout CI gives this package, not a wait. It
	// starts at new messages (jobConsumerConfig), so a durable an earlier row left on the
	// package's shared stream is recreated and handed this row's events alone.
	wanted := jobConsumerConfig(tuningFor(t))
	wanted.AckWait = 24 * time.Hour
	if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("delete the job durable: %v", err)
	}
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
		t.Fatalf("create the job durable: %v", err)
	}
	// The intake's own subscription, so each event takes the path a holder's takes: the
	// callback routes it (receive) and the goroutine of its key handles it (deliver). Every job
	// message keys alike (jobKey), so the event under test queues behind the one in hand.
	sub, err := jobs.subscribe(wanted.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe to the job durable: %v", err)
	}
	t.Cleanup(func() { _, _ = natsClient.DrainSubscription(sub) })
	jobs.mu.Lock()
	delivery := jobs.delivering
	jobs.mu.Unlock()
	consumer := func() *natsgo.ConsumerInfo {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, wanted.Durable)
		if err != nil {
			t.Errorf("read the job durable: %v", err)
			return &natsgo.ConsumerInfo{}
		}
		return info
	}
	// True once no handler of the delivery is running and none is queued: a key keeps its entry
	// while its goroutine has anything left to handle (keyedRunner).
	runnerIdle := func() bool {
		delivery.keys.mu.Lock()
		defer delivery.keys.mu.Unlock()
		return len(delivery.keys.queued) == 0
	}

	// The event in hand when the context ends: a tracked run's, which reaches the read it
	// schedules, where the clock holds it.
	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.job.500.completed", jobPayload("acme/widgets", 500))
	testwait.Receive(t, "the first job event to reach the read it schedules", clock.armedFirst)

	// The event under test, waiting behind it on the one key.
	event := jobPayload("acme/widgets", 501)
	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.job.501.completed", event)
	waitFor(t, func() bool { return consumer().Delivered.Consumer >= 2 }, func() string {
		return fmt.Sprintf("the job event under test was never delivered to the intake: %+v", consumer())
	})

	// The next holder: another process bound to the durable this one is about to give up. It
	// subscribes before the stop, so what the stop hands back finds a subscriber at once.
	nextHolder := intakeTestClient(t)
	handedOver := make(chan *natsgo.Msg, 1)
	next, err := subscribeDurable(nextHolder, wanted, wanted.DeliverSubject, func(msg *natsgo.Msg) {
		if err := msg.Ack(); err != nil {
			t.Errorf("acknowledge the job event handed to the next holder: %v", err)
		}
		select {
		case handedOver <- msg:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe the next holder to the job durable: %v", err)
	}
	t.Cleanup(func() { _, _ = nextHolder.DrainSubscription(next) })

	// The moment under test: Run's context is done, and nothing has stopped the subscription.
	endRun()
	if delivery.stopping.Load() {
		t.Fatal("the delivery was already stopping, so the row would measure the stop's own guard rather than the moment before it")
	}
	close(clock.release)
	waitFor(t, runnerIdle, func() string {
		return fmt.Sprintf("the job event under test was never handled: %+v", consumer())
	})

	// The stop, which hands back what the delivery held rather than what it acknowledged.
	jobs.StopDelivering(testwait.Context(t))
	msg := testwait.Receive(t, "the next holder to be handed the job event the stopped intake hands back", handedOver)
	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		t.Fatalf("decode the envelope the next holder was handed: %v", err)
	}
	if envelope.Payload != event {
		t.Fatalf("the next holder was handed payload %q, want the event the stopped intake handed back %q", envelope.Payload, event)
	}

	for _, record := range logs() {
		if record["msg"] == "dispatch delivery: read whether a job's run is tracked" {
			t.Errorf("the stopped intake read the event's run under the cancelled context: %v", record)
		}
	}
	if armed, calls, mints := len(clock.Armed()), fake.calls.Load(), fake.tokenMints.Load(); armed != 1 || calls != 0 || mints != 0 {
		t.Fatalf("after the stop: %d timers armed, %d jobs reads, %d token mints; want the first event's one timer and nothing of the event handled after the stop",
			armed, calls, mints)
	}
}
