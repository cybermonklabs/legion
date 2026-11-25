package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// podRefusalRig is LEGION-208 in testing, holding its admission slot, its tester's claim suspended
// by the daemon with its session kept and its phases row on entry 2, over an outbox runner that
// drains on the test's goroutine. The slot makes an owed entry's start claimable, as it is for an
// issue the daemon started (record.SuperviseRequest.NeedsSlot). Every launch of the claim the
// runtime refuses is told to the workflow as the daemon tells it (workflowRuntime.podRefused,
// supervise's Deps.PodRefused), and the machines run on a clock whose timers fire only when the
// test fires them (heldClock).
type podRefusalRig struct {
	t       *testing.T
	runner  *outbox
	rt      *fake.Runtime
	pool    *store.Pool
	clock   *heldClock
	machine *supervise.Machine
	token   claim.Token
}

// newPodRefusalRig builds the rig. answers builds the workflow runtime the refusals are told to,
// over the rig's pool and records, whose handlers the outbox runner applies its own facts with, as
// the daemon's runner does; deps changes the machines' deps before the claim is created, since a
// machine takes its deps where it is built.
func newPodRefusalRig(t *testing.T, answers func(pool *store.Pool, records record.Store) *workflowRuntime, deps func(*supervise.Deps)) *podRefusalRig {
	t.Helper()
	return newPodRefusalRigOf(t, answers, deps, nil)
}

// newPodRefusalRigOf is newPodRefusalRig with the tester's claim changed by shape before it is
// created; a nil shape changes nothing.
func newPodRefusalRigOf(t *testing.T, answers func(pool *store.Pool, records record.Store) *workflowRuntime, deps func(*supervise.Deps),
	shape func(*supervise.Claim)) *podRefusalRig {
	t.Helper()
	runner, sup, pool := runnerIn(t, phase.Testing, "testing", &outboxPublisher{}, quietLogger())
	clock := microClock()
	sup.deps.Clock = clock
	told := answers(pool, runner.records)
	sup.deps.PodRefused = told.podRefused
	runner.handlers = told.handlers
	if deps != nil {
		deps(&sup.deps)
	}
	token := mustClaimToken(t, "LEGION-208", claim.RoleTester)
	putPhaseRow(t, pool, runner.records, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: token, Entry: 2, EntryTask: "Test it.",
		EntryRaisedAt: time.Now().UTC().Add(-time.Hour)})
	if _, err := pool.Exec(context.Background(), `insert into slots (issue, index, admitted_at) values ('LEGION-208', 0, now())`); err != nil {
		t.Fatalf("seed LEGION-208's admission slot: %v", err)
	}
	// A start that waits for the pod reads the claims through the supervisor's store
	// (podWaitPrefix), as the daemon's does.
	sup.store = openOutboxStore(t, pool)
	tester := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
		Session: "ses-tester", SessionFile: t.TempDir() + "/session.jsonl"}
	if shape != nil {
		shape(&tester)
	}
	machine, _, err := sup.Create(context.Background(), tester, "")
	if err != nil {
		t.Fatalf("create the tester's claim: %v", err)
	}
	return &podRefusalRig{t: t, runner: runner, rt: sup.deps.Runtime.(*fake.Runtime), pool: pool, clock: clock, machine: machine, token: token}
}

// engineAnswers is the workflow runtime that applies each refusal's fact through the workflow
// engine of LEGION.
func engineAnswers(pool *store.Pool, records record.Store) *workflowRuntime {
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	return &workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger(), failed: make(chan error, 1)}
}

// queueTheTestersStart writes the outbox start that brings the tester back for entry 2 of testing,
// as the transition that raised that entry queued it.
func (r *podRefusalRig) queueTheTestersStart() {
	r.t.Helper()
	enqueueOutbox(r.t, r.pool, r.runner.records, mustOutboxRow(r.t, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
		Role: claim.RoleTester, Phase: phase.Testing, Entry: 2, Task: "Test it.", Generation: 1}, time.Now().Add(-time.Minute)))
}

// refuseOnPod1 makes pod-1 LEGION-208's pod, and every later launch of the tester one that pod
// cannot run until its update round or its release replaces it.
func (r *podRefusalRig) refuseOnPod1() {
	r.rt.ScriptIssuePod("LEGION-208", "pod-1")
	r.rt.OnResume = func(context.Context) error {
		return runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(r.token) + ": the pod pod-1 of LEGION-208 was built without checker"}
	}
}

// starts is every supervise start the outbox holds, in order.
func (r *podRefusalRig) starts() []record.SuperviseRequest {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select payload from outbox where kind = 'supervise' and payload->>'op' = 'start' order by id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var got []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			r.t.Fatal(err)
		}
		var start record.SuperviseRequest
		if err := json.Unmarshal(payload, &start); err != nil {
			r.t.Fatal(err)
		}
		got = append(got, start)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return got
}

// toldTheController waits, bounded, for the pod-wait-held notice the refusal's fact writes in its
// own transaction: the fact is applied on a goroutine of its own, so its commit is what the test
// waits for before it reads what else that transaction wrote.
func (r *podRefusalRig) toldTheController() {
	r.t.Helper()
	r.toldTheControllerTimes(1)
}

// toldTheControllerTimes is toldTheController for the nth refusal's report: it waits, bounded, for
// n pod-wait-held notices.
func (r *podRefusalRig) toldTheControllerTimes(n int) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if r.podWaitNotices() >= n {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the refusal's fact never committed: the outbox holds %d pod-wait-held notices after 20 s, want %d", r.podWaitNotices(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// podWaitNotices is how many pod-wait-held notices the outbox holds: one for each refusal's report
// the workflow applied.
func (r *podRefusalRig) podWaitNotices() int {
	r.t.Helper()
	var told int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and payload->>'kind' = $2`,
		string(record.OutboxKindControllerNotice), string(record.PodWaitHeldNotice)).Scan(&told); err != nil {
		r.t.Fatal(err)
	}
	return told
}

// launchWriteRefused is the outbox's claim store, which refuses the first write of a claim
// launching, as a store that did not answer refuses it: the launch's own write, which arms its
// retry at the probe interval (supervise's relaunchLater).
type launchWriteRefused struct {
	*outboxClaimStore
	refused atomic.Bool
}

func (s *launchWriteRefused) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.State == supervise.StateLaunching && s.refused.CompareAndSwap(false, true) {
		return errors.New("the store did not answer the launch's write")
	}
	return s.outboxClaimStore.PutClaim(ctx, c)
}

// A launch its issue's pod refuses after the outbox start that raised it has finished leaves no
// row in the outbox to bring the claim back: the start delivered its task to a claim still
// launching and ended, and the refusal, which suspends the claim for the daemon and retires that
// task, comes later, on a timer of the machine's. Two launches come that way: the turn the wake
// pacing held a resume for, and the retry of a launch whose write the store did not take, which
// carries the start's work. Either way the workflow reads no start of the claim in the outbox in
// the refusal's own transaction, and queues one: the tester's owed entry, the task its phases row
// holds, marked as a resend of that entry, so whatever replaces the pod resumes the claim on that
// work.
func TestALaunchItsPodRefusesAfterItsStartRowFinishedQueuesAStartOfTheClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		deps func(*supervise.Deps)
		// launchingAfterItsRow drains the rig's start row to its finish, leaving the claim
		// launching with no process under the timer this case fires, and answers that timer's
		// wait.
		launchingAfterItsRow func(t *testing.T, r *podRefusalRig) time.Duration
	}{
		{
			name: "the turn of a resume the wake pacing held",
			deps: func(d *supervise.Deps) {
				d.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time { return now.Add(parkedTurn) }}
			},
			launchingAfterItsRow: func(t *testing.T, r *podRefusalRig) time.Duration {
				drainAt(t, r.runner, 0, "the start row's attempt")
				if held := r.machine.View(); held.State != supervise.StateLaunching || held.Locator != nil || held.WakeWaiting == nil {
					t.Fatalf("after the start row's attempt the tester is %s recording %+v with wake %+v, want it launching with no process under the resume the pacing parked",
						held.State, held.Locator, held.WakeWaiting)
				}
				return parkedTurn
			},
		},
		{
			name: "the retry of a launch whose write the store did not take",
			deps: func(d *supervise.Deps) {
				d.Store = &launchWriteRefused{outboxClaimStore: d.Store.(*outboxClaimStore)}
			},
			launchingAfterItsRow: func(t *testing.T, r *podRefusalRig) time.Duration {
				drainAt(t, r.runner, 0, "the start row's first attempt")
				if live, _ := r.clock.armed(time.Second); live != 1 {
					t.Fatalf("after the launch's write failed %d timers wait the probe interval, want the one retry of the launch", live)
				}
				drainAt(t, r.runner, parkFound, "the start row's second attempt")
				if held := r.machine.View(); held.State != supervise.StateLaunching || held.Locator != nil {
					t.Fatalf("after the start row's second attempt the tester is %s recording %+v, want it launching with no process under the retry", held.State, held.Locator)
				}
				return time.Second
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPodRefusalRig(t, engineAnswers, tc.deps)
			r.queueTheTestersStart()
			wait := tc.launchingAfterItsRow(t, r)
			if left := r.starts(); len(left) != 0 {
				t.Fatalf("the outbox still holds the starts %+v, want the tester's start finished once it delivered its task", left)
			}

			r.refuseOnPod1()
			if fired := r.clock.fire(wait); fired != 1 {
				t.Fatalf("fired %d timers waiting %s, want the one the launch waits on", fired, wait)
			}
			if held := r.machine.Claim(); held.State != supervise.StateSuspended || held.SuspendedBy != supervise.SuspendedByDaemon {
				t.Fatalf("after the refused launch the tester is %s by %q, want it suspended by the daemon", held.State, held.SuspendedBy)
			}
			r.toldTheController()
			got := r.starts()
			if len(got) != 1 {
				t.Fatalf("after the refusal the outbox holds the starts %+v, want one start of the tester: nothing else brings it back once another pod stands", got)
			}
			if s := got[0]; s.Role != claim.RoleTester || s.Phase != phase.Testing || s.Entry != 2 || !strings.HasPrefix(s.Task, "Test it.") || !s.ResendEntry {
				t.Fatalf("the queued start is %+v, want the tester's owed entry 2 of testing, its stored task, marked as a resend of that entry", s)
			}
			r.outboxSettles()
		})
	}
}

// refusalReportLine is the line the workflow logs at ERROR at each failed apply of a refusal's
// report (intake.PodRefusedLaunch).
const refusalReportLine = "apply a launch its issue's pod refused"

// refuseTheTestersResume refuses the tester's resume by rule on pod-1, as a boot's resume of a
// claim that owns its issue's step meets a pod built without its slot.
func (r *podRefusalRig) refuseTheTestersResume() {
	r.t.Helper()
	r.refuseOnPod1()
	if err := r.machine.Handle(context.Background(), supervise.RequestRuleResume{Claim: r.token, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		r.t.Fatalf("the resume answered %v, want the refusal, which is runtime.ErrAwaitsPodUpdate", err)
	}
}

// failingAnswers is engineAnswers with failing before the engine in its handlers, so a fact that
// failing refuses rolls the engine's writes back with it. The runtime is stored in *answers for the
// test to read, and logs to log where log is not nil.
func failingAnswers(answers **workflowRuntime, failing intake.Handler, log *slog.Logger) func(pool *store.Pool, records record.Store) *workflowRuntime {
	return func(pool *store.Pool, records record.Store) *workflowRuntime {
		*answers = engineAnswers(pool, records)
		(*answers).handlers = append([]intake.Handler{failing}, (*answers).handlers...)
		if log != nil {
			(*answers).log = log
		}
		return *answers
	}
}

// A refusal's report is how the workflow learns that a claim waits for its pod and is what queues
// the start that brings the claim back, so a report the store does not take is tried again on a
// backoff until it is applied, each failure logged at ERROR, as a RefusedStartEnded fact is; and
// neither stops the workflow, so a report that fails, the hourly one included, costs the daemon no
// restart. Failing once and then passing, the issue and the controller are told and the claim's
// owed entry is queued. Failing for ever, the line is logged at each try, nothing reaches the
// workflow's failure channel (failed), nothing is queued, and the daemon's stop ends the tries.
func TestARefusalsReportTheStoreDidNotTakeIsTriedAgainUntilApplied(t *testing.T) {
	t.Parallel()
	notStopped := func(t *testing.T, answers *workflowRuntime) {
		t.Helper()
		select {
		case err := <-answers.failed:
			t.Fatalf("the workflow stopped on %v, want the report tried again and the workflow going on", err)
		default:
		}
	}
	t.Run("failing once, then applied", func(t *testing.T) {
		var answers *workflowRuntime
		failing := failingRefusals(1)
		r := newPodRefusalRig(t, failingAnswers(&answers, failing, nil), nil)
		r.refuseTheTestersResume()
		r.toldTheController()
		r.owedEntryQueued()
		if n := failing.count(); n != 1 {
			t.Fatalf("the report failed %d times, want the one failure before it was applied", n)
		}
		notStopped(t, answers)
	})
	t.Run("failing for ever", func(t *testing.T) {
		var answers *workflowRuntime
		logs := &syncBuffer{}
		failing := failingRefusals(-1)
		r := newPodRefusalRig(t, failingAnswers(&answers, failing, slog.New(slog.NewJSONHandler(logs, nil))), nil)
		stopping, stop := context.WithCancel(context.Background())
		defer stop()
		answers.stopped = stopping
		r.refuseTheTestersResume()
		deadline := time.Now().Add(20 * time.Second)
		for len(loggedLines(t, logs.String(), refusalReportLine)) < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("20 s after the refusal the log holds %d lines %q, want at least three: the report is tried again on its backoff",
					len(loggedLines(t, logs.String(), refusalReportLine)), refusalReportLine)
			}
			time.Sleep(10 * time.Millisecond)
		}
		for _, line := range loggedLines(t, logs.String(), refusalReportLine) {
			if line["level"] != "ERROR" {
				t.Fatalf("the line %v is not at ERROR", line)
			}
		}
		notStopped(t, answers)
		if got := r.starts(); len(got) != 0 {
			t.Fatalf("with the report never applied the outbox holds the starts %+v, want none", got)
		}
		stop()
		ended := failing.count()
		time.Sleep(5 * time.Second)
		if n := failing.count(); n > ended+1 {
			t.Fatalf("after the daemon's stop the report was tried %d more times, want the tries ended", n-ended)
		}
	})
}

// The turn of a resume the wake pacing held can come, and its launch be refused, while the start
// row that the pacing parked it for is still finishing: the row has delivered its task and returned,
// and its finish has not yet deleted it. The refusal's fact then reads that row as a start of the
// claim and queues none, and the row's finish leaves the claim suspended with nothing to start it.
// So the finish of a start row whose claim's last launch its pod refused asks the workflow again,
// once the row is gone, and the workflow queues the claim's start where none stands (the tester's
// owed entry). The interleaving is driven here: the row's attempt runs (execute) and its finish
// (settle) waits until the wake's turn has been refused and its fact has committed.
func TestARefusalWhileItsParkedStartRowIsStillFinishingQueuesAStartOnceTheRowEnds(t *testing.T) {
	t.Parallel()
	r := newPodRefusalRig(t, engineAnswers, pacedTurn)
	row := r.refusedWhileItsRowFinishes()
	r.runner.settle(context.Background(), row, nil)
	r.owedEntryQueued()
	r.outboxSettles()
}

// A start a refusal queued that ends at a fence while the refusal stands asks the workflow nothing,
// and the workflow queues a refused claim no start that the outbox would finish without acting. The
// tester's launch is refused and its owed entry queued, marked with pod-1, whose refusal queued it
// (record.SuperviseRequest.RefusedPod); then its owner stops its task, and the outbox runs that
// start once: it serves a phase the issue has left and finishes at the phase fence. Nothing follows
// it: no start, no fact, since the machine still reports pod-1's refusal. Were the end of such a
// start to read the claim's starts again (intake.RefusedStartEnded), it would queue a start of no
// phase, which the outbox finishes at the stopped step's fence, whose end would ask again, at the
// database's speed, one row, one fact and one log line a turn, telling nobody.
func TestAStartRowThatEndedAtAFenceQueuesARefusedClaimNoStartOfItsStoppedTask(t *testing.T) {
	t.Parallel()
	r := newPodRefusalRig(t, engineAnswers, nil)
	r.refuseOnPod1()
	if err := r.machine.Handle(context.Background(), supervise.RequestRuleResume{Claim: r.token, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the resume answered %v, want the refusal, which is runtime.ErrAwaitsPodUpdate", err)
	}
	r.toldTheController()
	r.owedEntryQueued()
	r.stopTheTestersTask()
	facts := r.factsApplied()

	drainAt(t, r.runner, 0, "the outbox's run of the owed entry")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if left := r.starts(); len(left) != 0 {
			t.Fatalf("after the owed entry finished at the phase fence the outbox holds the starts %+v, want none: the outbox would finish each without acting", left)
		}
		if now := r.factsApplied(); now != facts {
			t.Fatalf("after the owed entry finished at the phase fence %d facts were applied, want none: the start was queued for pod-1's refusal, which still stands", now-facts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.outboxSettles()
}

// stopTheTestersTask records LEGION-208 as its owner's stop of its task at testing leaves it: held
// from testing, task-stopped, so the tester is the role of the stopped task's step
// (workflow.StoppedStepRole).
func (r *podRefusalRig) stopTheTestersTask() {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.runner.records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped},
		Generation: 1, Status: "testing", Rank: "U", LastDispatchSeq: 5})
}

// factsApplied is how many facts the database records applied (processed_events).
func (r *podRefusalRig) factsApplied() int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from processed_events`).Scan(&n); err != nil {
		r.t.Fatalf("count the applied facts: %v", err)
	}
	return n
}

// rowsWritten is how many rows the outbox has ever been given: the last value its id sequence
// handed out, which a finished row, deleted, does not take back.
func (r *podRefusalRig) rowsWritten() int64 {
	r.t.Helper()
	var n int64
	if err := r.pool.QueryRow(context.Background(), `select coalesce(pg_sequence_last_value('outbox_id_seq'), 0)`).Scan(&n); err != nil {
		r.t.Fatalf("read the outbox's id sequence: %v", err)
	}
	return n
}

// outboxSettles is the loop control of the refused-launch tests: with nothing else changing, the
// outbox stops writing rows and the workflow stops applying facts. It drains the outbox ten
// times, each with the runner's clock past every backoff the drains before it set, and after the
// work a row's finish starts on a goroutine of its own has had time to land, and fails unless the
// last five drains wrote no row and applied no fact. A loop that queues one start a turn writes a
// row at every drain.
func (r *podRefusalRig) outboxSettles() {
	r.t.Helper()
	const drains, quiet = 10, 5
	var rows int64
	var facts int
	for i := range drains {
		if i == drains-quiet {
			rows, facts = r.rowsWritten(), r.factsApplied()
		}
		drainAt(r.t, r.runner, time.Duration(i+1)*parkFound, "a drain of the loop control")
		time.Sleep(200 * time.Millisecond)
	}
	if wrote, applied := r.rowsWritten()-rows, r.factsApplied()-facts; wrote != 0 || applied != 0 {
		r.t.Fatalf("the last %d of %d drains, with nothing else changing, wrote %d outbox rows and applied %d facts, want none: the outbox and the workflow feed each other",
			quiet, drains, wrote, applied)
	}
}

// pacedTurn is the wake pacing that gives every launch its turn parkedTurn on.
func pacedTurn(d *supervise.Deps) {
	d.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time { return now.Add(parkedTurn) }}
}

// refusedWhileItsRowFinishes drives the rig, under pacedTurn, to the moment a start row's attempt
// has run and its finish has not: the row's resume is parked by the pacing, the wake's turn is
// refused by pod-1, and the refusal's fact has committed while the row still stands, reading it as
// the claim's start. It answers the row, for the test to settle.
func (r *podRefusalRig) refusedWhileItsRowFinishes() record.OutboxRow {
	t := r.t
	t.Helper()
	r.queueTheTestersStart()
	var id int64
	if err := r.pool.QueryRow(context.Background(), `select id from outbox where kind = 'supervise'`).Scan(&id); err != nil {
		t.Fatalf("read the start row's id: %v", err)
	}
	row := mustOutboxRow(t, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Phase: phase.Testing,
		Entry: 2, Task: "Test it.", Generation: 1}, time.Now().Add(-time.Minute))
	row.ID = id
	if err := r.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the start row's attempt: %v", err)
	}
	if held := r.machine.View(); held.State != supervise.StateLaunching || held.WakeWaiting == nil {
		t.Fatalf("after the start row's attempt the tester is %s with wake %+v, want it launching under the resume the pacing parked", held.State, held.WakeWaiting)
	}
	r.refuseOnPod1()
	if fired := r.clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	r.toldTheController()
	if got := r.starts(); len(got) != 1 {
		t.Fatalf("while the row still stands the outbox holds the starts %+v, want the row's own alone: the refusal read it as the claim's start", got)
	}
	return row
}

// owedEntryQueued waits, bounded, for the outbox to hold one start, the tester's owed entry 2 of
// testing, its stored task, marked as a resend of that entry, and fails if it holds anything else.
func (r *podRefusalRig) owedEntryQueued() {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := r.starts()
		if len(got) == 1 && got[0].ResendEntry {
			if s := got[0]; s.Role != claim.RoleTester || s.Phase != phase.Testing || s.Entry != 2 || !strings.HasPrefix(s.Task, "Test it.") {
				r.t.Fatalf("the queued start is %+v, want the tester's owed entry 2 of testing, its stored task, marked as a resend of that entry", s)
			}
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("20 s after the row's finish the outbox holds the starts %+v, want the tester's owed entry queued: nothing else brings it back", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// failingKind fails every apply of a fact of one kind (of) while fails is above zero, counting
// down, and every one for ever where fails is negative; it passes every other fact. It stands
// before the engine in a runner's handlers, so a failure rolls the engine's writes back with it.
type failingKind struct {
	of     func(intake.Fact) bool
	mu     sync.Mutex
	fails  int
	failed int
}

// failingEnded fails the first fails applies of a RefusedStartEnded fact, or every one where fails
// is negative.
func failingEnded(fails int) *failingKind {
	return &failingKind{fails: fails, of: func(fact intake.Fact) bool { _, ok := fact.(intake.RefusedStartEnded); return ok }}
}

// failingRefusals fails the first fails applies of a refusal's report (intake.PodRefusedLaunch),
// or every one where fails is negative.
func failingRefusals(fails int) *failingKind {
	return &failingKind{fails: fails, of: func(fact intake.Fact) bool { _, ok := fact.(intake.PodRefusedLaunch); return ok }}
}

func (f *failingKind) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if !f.of(fact) {
		return intake.Result{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails == 0 {
		return intake.Result{}, nil
	}
	f.fails--
	f.failed++
	return intake.Result{}, errors.New("the store did not take the fact")
}

func (f *failingKind) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// refusedStartEndedLine is the line the outbox logs at ERROR at each failed apply of a
// RefusedStartEnded fact.
const refusedStartEndedLine = "apply the end of a start row of a claim its pod refused"

// A RefusedStartEnded fact the store does not take is tried again on a backoff until it is
// applied, each failure logged at ERROR, so a claim refused while its start row was finishing
// gets its start once the store answers, with no boot. Failing once and then passing, the claim
// gets exactly one start. Failing for ever, the line is logged at each try; the backoff doubles up
// to an hour (refusalRetryMost), so an operator sees it hourly. The stop ends the tries.
func TestARefusedStartEndedTheStoreDidNotTakeIsTriedAgainUntilApplied(t *testing.T) {
	t.Parallel()
	t.Run("failing once, then applied", func(t *testing.T) {
		r := newPodRefusalRig(t, engineAnswers, pacedTurn)
		failing := failingEnded(1)
		r.runner.handlers = append([]intake.Handler{failing}, r.runner.handlers...)
		row := r.refusedWhileItsRowFinishes()
		r.runner.settle(context.Background(), row, nil)
		r.owedEntryQueued()
		if n := failing.count(); n != 1 {
			t.Fatalf("the fact failed %d times, want the one failure before it was applied", n)
		}
	})
	t.Run("failing for ever", func(t *testing.T) {
		r := newPodRefusalRig(t, engineAnswers, pacedTurn)
		logs := &syncBuffer{}
		r.runner.log = slog.New(slog.NewJSONHandler(logs, nil))
		failing := failingEnded(-1)
		r.runner.handlers = append([]intake.Handler{failing}, r.runner.handlers...)
		row := r.refusedWhileItsRowFinishes()
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		r.runner.settle(ctx, row, nil)
		deadline := time.Now().Add(20 * time.Second)
		for len(loggedLines(t, logs.String(), refusedStartEndedLine)) < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("20 s after the row's finish the log holds %d lines %q, want at least three: the fact is tried again on its backoff",
					len(loggedLines(t, logs.String(), refusedStartEndedLine)), refusedStartEndedLine)
			}
			time.Sleep(10 * time.Millisecond)
		}
		for _, line := range loggedLines(t, logs.String(), refusedStartEndedLine) {
			if line["level"] != "ERROR" {
				t.Fatalf("the line %v is not at ERROR", line)
			}
		}
		if got := r.starts(); len(got) != 0 {
			t.Fatalf("with the fact never applied the outbox holds the starts %+v, want none", got)
		}
		stop()
		ended := failing.count()
		time.Sleep(5 * time.Second)
		if n := failing.count(); n > ended+1 {
			t.Fatalf("after the stop the fact was tried %d more times, want the tries ended", n-ended)
		}
	})
}

// moveToReviewing records LEGION-208 as its tester's pass leaves it: in reviewing, where the tester
// works no step.
func (r *podRefusalRig) moveToReviewing() {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.runner.records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U", LastDispatchSeq: 5})
}

// noPhaseStartQueued waits, bounded, for the outbox to hold one start, the tester's claim's start
// of no phase, which carries no task and no entry, and fails if it holds anything else.
func (r *podRefusalRig) noPhaseStartQueued() {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := r.starts()
		if len(got) == 1 && got[0].Phase == "" {
			if s := got[0]; s.Role != claim.RoleTester || s.Claim != r.token || s.Task != "" || s.Entry != 0 {
				r.t.Fatalf("the queued start is %+v, want the tester's claim's start of no phase, with no task and no entry", s)
			}
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("20 s on the outbox holds the starts %+v, want the tester's start of no phase: nothing else brings the claim back once another pod stands", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startRow is the one start the outbox holds, as a lane would lease it: its id, and its payload as
// stored.
func (r *podRefusalRig) startRow() record.OutboxRow {
	r.t.Helper()
	var id int64
	var payload []byte
	if err := r.pool.QueryRow(context.Background(), `select id, payload from outbox where kind = 'supervise' and payload->>'op' = 'start'`).Scan(&id, &payload); err != nil {
		r.t.Fatalf("read the start row: %v", err)
	}
	var start record.SuperviseRequest
	if err := json.Unmarshal(payload, &start); err != nil {
		r.t.Fatal(err)
	}
	row := mustOutboxRow(r.t, "LEGION-208", start, time.Now().Add(-time.Minute))
	row.ID = id
	return row
}

// podRefusedSpecs is the spec builder of a test that needs a claim refused by its pod while it
// holds no name of the session it records. It answers every launch with pod-1's refusal
// (runtime.AwaitsPodUpdate), which no real spec builder returns: it stands in for a state that a
// daemon may not reach in one run, since a real launch holds the session's names before the
// runtime can refuse it and the machine keeps the names it holds. A launch builds its spec before
// it holds those names (supervise's start), so this refusal leaves the claim holding nothing, and
// another claim can then hold the session the claim records.
type podRefusedSpecs struct{}

func (podRefusedSpecs) SpawnSpec(_ context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{}, runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(c.Token) + ": the pod pod-1 of " + c.Issue + " was built without checker"}
}

// A claim whose launch its issue's pod refused, and which records a session it does not hold —
// another claim recorded it first — is given a start that waits for the pod, not one that ends
// while the pod stands. Here the issue has moved on to reviewing, so the start the refusal queues
// is of no phase and carries no task, and no delivery refuses it. The rule's resume is refused by
// the pod that still stands before the claim's session is asked about, as the operator's resume is
// (supervise's resumeByRule and resume), so the start waits for the pod. Were it answered for the
// session first and ended, its end would ask the workflow again (intake.RefusedStartEnded), whose
// start would end the same way, at the database's speed: one row, one fact and one log line a turn,
// telling nobody.
func TestARefusedClaimThatRecordsASessionItDoesNotHoldWaitsForItsPod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newPodRefusalRigOf(t, engineAnswers, func(d *supervise.Deps) { d.Specs = podRefusedSpecs{} }, func(c *supervise.Claim) {
		c.State, c.SuspendedBy = supervise.StateQueued, ""
	})
	r.moveToReviewing()
	r.rt.ScriptIssuePod("LEGION-208", "pod-1")
	if err := r.machine.Handle(ctx, supervise.RequestSpawn{Claim: r.token}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the spawn answered %v, want pod-1's refusal, which is runtime.ErrAwaitsPodUpdate", err)
	}
	r.toldTheController()
	r.noPhaseStartQueued()

	recorded := r.machine.Claim()
	other := mustClaimToken(t, "LEGION-208", claim.RoleReviewer)
	claims := r.runner.supervisor.deps.Store.(*outboxClaimStore)
	if holders, err := claims.HoldSessions(ctx, "legion", other, supervise.RecordedHolds(recorded.Session, recorded.SessionFile)); err != nil ||
		slices.ContainsFunc(holders, func(c claim.Token) bool { return c != other }) {
		t.Fatalf("another claim's hold of the tester's session = %v, %v; want every name its own", holders, err)
	}

	r.outboxSettles()
	r.noPhaseStartQueued()
	if held := r.machine.Claim(); held.State != supervise.StateSuspended || held.SuspendedBy != supervise.SuspendedByDaemon || r.machine.RefusedPod() != "pod-1" {
		t.Fatalf("the tester is %s by %q with the refusal of %q, want it suspended by the daemon, still refused by pod-1", held.State, held.SuspendedBy, r.machine.RefusedPod())
	}
}

// A refusal no start row raised — the rule's resume of the tester, refused at once — reads the
// tester's start that stands in the outbox as the start that brings the claim back, and queues
// none. That start then ends without reaching the claim: the issue moves on to reviewing, and the
// outbox finishes the start at the phase fence. Its end asks the workflow again
// (intake.RefusedStartEnded), and the workflow queues the tester a start of no phase, which waits
// for the pod: nothing else would bring the claim back once another pod stands. The end of a start
// row asks whether or not the row reached the claim.
func TestAStartThatEndsAtAFenceAsksAgainForARefusalNoStartRaised(t *testing.T) {
	t.Parallel()
	r := newPodRefusalRig(t, engineAnswers, nil)
	r.queueTheTestersStart()
	r.refuseTheTestersResume()
	r.toldTheController()
	if got := r.starts(); len(got) != 1 || got[0].Phase != phase.Testing || got[0].ResendEntry {
		t.Fatalf("after the refusal the outbox holds the starts %+v, want the tester's own start alone: the report read it as the start that brings the claim back", got)
	}

	r.moveToReviewing()
	drainAt(t, r.runner, 0, "the run of the tester's start, which serves a phase the issue has left")
	r.noPhaseStartQueued()
	r.outboxSettles()
	r.noPhaseStartQueued()
}

// refusalGoneLine is the line the daemon logs where it drops a fact of a refused launch whose
// claim's machine no longer reports that refusal.
const refusalGoneLine = "a fact of a refused launch is not applied: the claim's machine no longer reports that refusal"

// A refusal's report is judged when it is applied, and a report the store did not take is tried
// again, up to an hour later: by then the claim may have launched on the pod that replaced the one
// that refused it. Such a report tells the issue nothing, since "cannot be started" is no longer
// true, tells the controller nothing, and queues no start, since the owed entry resent to a claim
// that runs would hand its worker the same task twice. So each try first reads the claim's
// machine, and where the machine no longer reports the refusal the report names, the fact is not
// applied and a line says so. Here the report fails once, the tester launches on pod-2, and the
// try after the backoff applies nothing.
func TestARefusalsReportTriedAgainAfterItsClaimLaunchedTellsNothing(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	var answers *workflowRuntime
	r := newPodRefusalRig(t, failingAnswers(&answers, failingRefusals(1), slog.New(slog.NewJSONHandler(logs, nil))), nil)
	r.refuseTheTestersResume()
	r.rt.ScriptIssuePod("LEGION-208", "pod-2")
	r.rt.OnResume = nil
	if err := r.machine.Handle(context.Background(), supervise.RequestRuleResume{Claim: r.token, Reason: "another pod stands"}); err != nil {
		t.Fatalf("the resume once pod-2 stands = %v, want the tester launched", err)
	}
	if held := r.machine.Claim(); held.Locator == nil || r.machine.RefusedPod() != "" {
		t.Fatalf("the tester records the process %+v and the refusal of %q, want it launched, refused by no pod", held.Locator, r.machine.RefusedPod())
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(loggedLines(t, logs.String(), refusalGoneLine)) == 0 {
		if told, starts := r.podWaitNotices(), r.starts(); told != 0 || len(starts) != 0 {
			t.Fatalf("the report applied after the tester launched told the controller %d times and queued the starts %+v, want neither", told, starts)
		}
		if time.Now().After(deadline) {
			t.Fatalf("20 s on the log holds no line %q, want the report's try after the launch to apply nothing and say so", refusalGoneLine)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if told, starts := r.podWaitNotices(), r.starts(); told != 0 || len(starts) != 0 {
		t.Fatalf("the report dropped after the tester launched told the controller %d times and queued the starts %+v, want neither", told, starts)
	}
}

// A refusal's report the store keeps refusing is tried again for as long as it fails, each try
// logged at ERROR, and nothing else says so: the daemon does not stop for it, and the issue and the
// controller are told only once it is taken. So the claim's machine carries the refusal the store
// has not recorded (supervise.Claim.UnrecordedRefusal) — when the first failed try failed, how many
// failed and the last error — which the operator's listing reads off the live machine and
// `legion claims list` prints as the cause word `refusal-unrecorded`. Once a try is taken, it is
// gone. Here the report fails twice and is taken at the third try.
func TestARefusalTheStoreHasNotRecordedIsShownOnItsClaimUntilItIs(t *testing.T) {
	t.Parallel()
	failing := failingRefusals(2)
	var answers *workflowRuntime
	r := newPodRefusalRig(t, failingAnswers(&answers, failing, nil), nil)
	r.refuseTheTestersResume()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if shown := r.machine.View().UnrecordedRefusal; shown != nil && shown.Attempts >= 2 {
			if shown.Since.IsZero() || !strings.Contains(shown.Error, "the store did not take the fact") {
				t.Fatalf("the claim shows the refusal not recorded as %+v, want when its first try failed and the last try's error", *shown)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("20 s on the claim shows the refusal not recorded as %+v after %d failed tries, want it shown with two failed tries", r.machine.View().UnrecordedRefusal, failing.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.toldTheController()
	r.owedEntryQueued()
	deadline = time.Now().Add(20 * time.Second)
	for r.machine.View().UnrecordedRefusal != nil {
		if time.Now().After(deadline) {
			t.Fatalf("20 s after the report was taken the claim still shows %+v, want no refusal not recorded", *r.machine.View().UnrecordedRefusal)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := failing.count(); n != 2 {
		t.Fatalf("the report failed %d times, want the two failures before it was taken", n)
	}
}

// A start a refusal queued is marked with the pod that refused (record.SuperviseRequest.RefusedPod),
// and its end asks the workflow nothing while that pod still refuses the claim. A start of an
// earlier refusal, whose pod has gone, asks again where a new pod refuses the launch its own resume
// raised: here the owed entry pod-1's refusal queued resumes the tester once pod-2 stands, the
// wake pacing parks that launch, and pod-2 refuses it at the wake's turn while the row is still
// finishing, so pod-2's report reads the row as the claim's start and queues nothing. Once the row
// ends, the workflow queues the owed entry again.
func TestAStartAnEarlierRefusalQueuedAsksAgainWhenAnotherPodRefusesItsLaunch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newPodRefusalRig(t, engineAnswers, pacedTurn)
	r.refuseOnPod1()
	if err := r.machine.Handle(ctx, supervise.RequestRuleResume{Claim: r.token, Reason: "it owns its issue's step"}); err != nil {
		t.Fatalf("the resume answered %v, want its launch parked by the pacing", err)
	}
	if fired := r.clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	r.toldTheController()
	r.owedEntryQueued()
	row := r.startRow()

	r.rt.ScriptIssuePod("LEGION-208", "pod-2")
	r.rt.OnResume = func(context.Context) error {
		return runtime.AwaitsPodUpdate{Pod: "pod-2", Reason: "launch " + string(r.token) + ": the pod pod-2 of LEGION-208 was built without checker"}
	}
	if err := r.runner.execute(ctx, row); err != nil {
		t.Fatalf("the owed entry's attempt once pod-2 stands: %v", err)
	}
	if held := r.machine.View(); held.State != supervise.StateLaunching || held.WakeWaiting == nil {
		t.Fatalf("after the owed entry's attempt the tester is %s with wake %+v, want it launching under the resume the pacing parked", held.State, held.WakeWaiting)
	}
	if fired := r.clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	r.toldTheControllerTimes(2)
	if got := r.starts(); len(got) != 1 {
		t.Fatalf("while the owed entry still stands the outbox holds the starts %+v, want it alone: pod-2's report read it as the claim's start", got)
	}
	r.runner.settle(ctx, row, nil)
	r.owedEntryQueued()
	r.outboxSettles()
}

// While a refusal's report is not yet taken, the machine's next report of the same refusal — the
// hourly one while the pod stands (supervise's tellPodRefused) — starts no second report: the
// first is still tried, and two would each tell the issue and the controller once the store took
// them. Here the store refuses every report; the second report of pod-1's refusal starts no tries of
// its own, so exactly one first try is logged.
func TestASecondReportOfARefusalNotYetTakenStartsNoTries(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	var answers *workflowRuntime
	r := newPodRefusalRig(t, failingAnswers(&answers, failingRefusals(-1), slog.New(slog.NewJSONHandler(logs, nil))), nil)
	r.refuseTheTestersResume()
	answers.podRefused(r.machine, r.machine.Claim(), "pod-1", "the pod pod-1 of LEGION-208 still stands")
	const line = "apply a launch its issue's pod refused"
	deadline := time.Now().Add(20 * time.Second)
	for {
		var firsts, seconds int
		for _, logged := range loggedLines(t, logs.String(), line) {
			switch logged["attempt"] {
			case float64(1):
				firsts++
			case float64(2):
				seconds++
			}
		}
		if firsts > 1 {
			t.Fatalf("the log holds %d first tries of pod-1's report, want one: the second report started tries of its own", firsts)
		}
		if seconds > 0 {
			if firsts != 1 {
				t.Fatalf("the log holds a second try and %d first tries, want one first try", firsts)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("20 s on the log holds %d first tries and no second, want the report tried again", firsts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
