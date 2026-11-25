package daemon

// Every way a phase holder's claim comes back to ready in a session that has not read its task
// ends in its phase's task, within two minutes of that ready: dispatch://INFRA-714 R7, acceptance
// A9. An operator's resume on 2026-10-30 brought about eight phase holders back ready with no task,
// and each sat idle for two to five hours. A phase holder that comes back in the session it
// finished its last task in is given nothing instead (dispatch://INFRA-2811 R4): it waits, and its
// session's own resume reminder wakes it while its phase is open. The three come-backs here are the
// ones a claim has — an operator's suspend and resume, a relaunch after a death, and a resume after
// the daemon replaced the pod (the displaced path) — each driven through the supervisor that
// observes it, the ready fact it fires (workflowRuntime.terminal), the workflow's readyWorkerTask
// and the outbox row it queues, to the task the agent's claim holds for its worker to take.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// taskWithinOfReady is how long a phase holder may stand ready with no task before the daemon has
// failed it (dispatch://INFRA-714 acceptance A9). It is measured where an operator reads it, over
// the two times the claim view carries: the claim's last ready (supervise.Claim.LastReady) and the
// moment its task was queued (supervise.Delivery.QueuedAt).
const taskWithinOfReady = 2 * time.Minute

// readyWorkerRig is a tester at work in its issue's testing phase, as the daemon leaves one: the
// start row that moved the issue into testing created and launched its claim, its agent said it
// was ready, and that row's task was delivered to it. Its supervisor fires the ready fact the
// daemon fires (workflowRuntime.terminal) and keeps its claims where the workflow reads them, in
// Postgres, so readyWorkerTask sees the claim and its pending task as the daemon's does. It reads
// the session's activity report from that store as the daemon does (supervise.Deps.ClaimActivity),
// which is what a relaunch records the work it ended from. Started with readyResume
// (newStartedTesterRig), its ready delivers the resume row the ready's fact queued as the
// daemon's does (supervise.Deps.ReadyResume, the outbox's readyResume). It writes each task's
// value to a bucket of its own (values), which is what the worker reads after its ready.
type readyWorkerRig struct {
	t       *testing.T
	pool    *store.Pool
	st      *store.Store
	sup     *supervisor
	rt      *fake.Runtime
	runner  *outbox
	machine *supervise.Machine
	conn    *fake.Conn
	token   claim.Token
	issue   record.Issue
	// engine is the workflow engine the ready's fact is applied through, which composes the task
	// the phase holder is given again (resumeTaskOf).
	engine *workflow.Engine
	// values is the task bucket the claim's value is written to (supervise.Deps.TaskValues).
	values *supervisetest.TaskValues
}

// newReadyWorkerRig starts the tester of a testing issue and queues it the start's task.
func newReadyWorkerRig(t *testing.T) *readyWorkerRig {
	t.Helper()
	rig := newStartedTesterRig(t, "Test it.", true)
	if held := rig.claim().Pending; held == nil || held.Task != "Test it." {
		t.Fatalf("the started tester holds %+v, want the start's task", held)
	}
	return rig
}

// newStartedTesterRig starts the tester of a testing issue from a start carrying task, which ""
// leaves carrying none, and brings its agent to ready in testerSession. With readyResume, its
// ready delivers the resume row the ready's fact queued, as the daemon's does
// (supervise.Deps.ReadyResume); without, it leaves that row to the outbox's own run. The start
// creates the machine, which copies the supervisor's deps then, so the choice is made here.
func newStartedTesterRig(t *testing.T, task string, readyResume bool) *readyWorkerRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}
	putOutboxIssue(t, pool, records, issue)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.ClaimActivity = st.ClaimActivity
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: issue.Project, Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	rig := &readyWorkerRig{t: t, pool: pool, st: st, sup: sup, rt: rt, issue: issue, engine: engine,
		runner: &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
			trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), handlers: []intake.Handler{engine},
			// The outbox reads the clock to lease rows; a minute on runs every row the ready
			// queued in the same pass. The two times the bound is read from are the supervisor's.
			now: func() time.Time { return time.Now().Add(time.Minute) },
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
			},
		},
	}
	rig.token = mustClaimToken(t, issue.Key, claim.RoleTester)
	rig.conn = fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(rig.token, rig.conn)
	rig.values = supervisetest.NewTaskValues()
	sup.deps.TaskValues = rig.values
	if readyResume {
		sup.deps.ReadyResume = rig.runner.readyResume
	}
	// The start that moved the issue into testing: it creates the claim, launches it and hands it
	// task where it carries one, which waits for the agent to be ready.
	if err := rig.runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree,
		Role: claim.RoleTester, Generation: issue.Generation, Phase: phase.Testing, Task: task}, time.Now())); err != nil {
		t.Fatalf("start the tester: %v", err)
	}
	machine, found := sup.Machine(rig.token)
	if !found {
		t.Fatalf("the start left no claim for the tester of %s", issue.Key)
	}
	rig.machine = machine
	rig.boot(testerSession)
	return rig
}

// boot drives the launched process through its shim's hello, its agent's registration and its
// ready, at the generation the claim is launching, as session, and waits for what the ready set
// going.
func (r *readyWorkerRig) boot(session string) {
	r.t.Helper()
	ctx := context.Background()
	generation := r.claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: generation},
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: session, SessionFile: "/tmp/" + session + ".jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: session},
	} {
		if err := r.machine.Handle(ctx, ev); err != nil {
			r.t.Fatalf("boot the tester: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
}

// workTheTask has the worker take the delivered task, runs the turn the take answers and ends it,
// then sends the turn-outcome report naming the task, which is what finishes it: with background
// work on, an agent_end names no task — a background result wakes a settled session and starts
// turns the daemon never gave a task — so a task's life ends at the report of its number rather
// than at any turn end. The session then reports that nothing of its own runs, as Oh My Pi's does
// at every turn's end, so a come-back ends no work of it. The tester is then the phase holder the
// issue waits on, idle with nothing pending, where CI, a review or an operator's resume leaves one.
func (r *readyWorkerRig) workTheTask() {
	r.t.Helper()
	ctx := context.Background()
	pending := r.claim().Pending
	if pending == nil {
		r.t.Fatal("the tester holds no task to work")
	}
	acceptTake(ctx, r.t, r.machine)
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: r.token},
		supervise.StreamTurnEnd{Claim: r.token},
		supervise.RequestTurnOutcome{Claim: r.token, Generation: r.claim().Generation, Sequence: turnSequence.Add(1), Task: pending.Number},
	} {
		if err := r.machine.Handle(ctx, ev); err != nil {
			r.t.Fatalf("work the task: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
	if held := r.claim().Pending; held != nil {
		r.t.Fatalf("after its turn the tester still holds %+v, want the task retired with the turn", held)
	}
	r.report()
}

// report is the tester's session reporting what of its own runs (putActivity): the kinds named,
// or nothing running when it names none.
func (r *readyWorkerRig) report(kinds ...string) {
	r.t.Helper()
	putActivity(r.t, r.st, r.token, r.claim().Generation, kinds...)
}

// putActivity is token's session reporting what of its own runs, through the store's own write
// onto launch, the launch it runs now (the activity route's PutClaimActivity): background work of
// the kinds named, or nothing running when it names none.
func putActivity(t *testing.T, st *store.Store, token claim.Token, launch uint64, kinds ...string) {
	t.Helper()
	known, err := st.PutClaimActivity(context.Background(), token, launch, claim.SessionActivity{
		At: time.Now(), BackgroundWork: claim.BackgroundWork{Running: len(kinds) > 0, Kinds: kinds}})
	if err != nil || !known {
		t.Fatalf("report the activity of %s: known=%v, %v", token, known, err)
	}
}

// run runs the outbox once and waits for every machine it drove.
func (r *readyWorkerRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("run the outbox: %v", err)
	}
	r.machine.Wait()
}

func (r *readyWorkerRig) claim() supervise.Claim { return r.machine.Claim() }

// relaunched is how many times the runtime was asked to resume the claim's session, which every
// come-back here does once: the process the come-back ended is replaced by one on the session the
// claim keeps.
func (r *readyWorkerRig) relaunched() int { return len(resumesOf(r.rt, r.token)) }

// observe hands the machine what the runtime now says about the claim's process.
func (r *readyWorkerRig) observe(kind runtime.ObservationKind, displaced bool) {
	r.t.Helper()
	if err := r.machine.Handle(context.Background(), supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *r.claim().Locator, Kind: kind, Displaced: displaced, At: time.Now()}}); err != nil {
		r.t.Fatalf("observe the tester's process as %s: %v", kind, err)
	}
	r.machine.Wait()
}

// testerSession is the session the tester's agent registers at its launch, and resumes at every
// come-back that keeps it; replacedSession is the one a come-back registers whose recorded
// session no store held any more (supervise's startFresh), which has read nothing of the tester's
// task.
const (
	testerSession   = "ses-tester"
	replacedSession = "ses-tester-replaced"
)

// comeBacks are the ways a phase holder's claim comes back to ready, each ending in the relaunched
// process booting its agent as the session given.
var comeBacks = map[string]func(r *readyWorkerRig, session string){
	"an operator's suspend and resume": func(r *readyWorkerRig, session string) {
		r.t.Helper()
		ctx := context.Background()
		operatorSuspend(r.t, r.sup, r.token)
		if err := r.machine.Handle(ctx, supervise.RequestResume{Claim: r.token}); err != nil {
			r.t.Fatalf("the operator's resume: %v", err)
		}
		r.machine.Wait()
		r.boot(session)
	},
	"a relaunch after a death": func(r *readyWorkerRig, session string) {
		r.t.Helper()
		r.observe(runtime.Gone, false)
		r.boot(session)
	},
	"a resume after a pod replacement": func(r *readyWorkerRig, session string) {
		r.t.Helper()
		r.observe(runtime.Gone, true)
		r.boot(session)
	},
}

// A phase holder that comes back ready with nothing pending in a session that replaced the one it
// finished its task in — the one it recorded was in no session store, so the come-back started a
// fresh agent — is given its phase's task, whichever way it came back, and within
// taskWithinOfReady of that ready: the resume relaunches the claim, the ready fires the workflow
// fact, and the ready's own decision queues the task of the row that fact queued
// (supervise.Deps.ReadyResume), which the row's run in the outbox pass that follows leaves as it
// is. Before, nothing was sent at all — the start row that moved the issue into testing had run
// long before the restart — and the issue waited for as long as the worker did; a new session has
// no reminder of its own to wake it.
func TestEveryComeBackInANewSessionGivesAPhaseHolderItsTaskWithinTwoMinutes(t *testing.T) {
	t.Parallel()
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			rig.workTheTask()
			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the tester that worked its task was queued %d outbox rows, want none before it comes back", rows)
			}
			rig.rt.OnResume = func(context.Context) error { return runtime.ErrSessionFileMissing }

			comeBack(rig, replacedSession)

			if rows, relaunched := outboxRows(t, rig.pool), rig.relaunched(); rows != 1 || relaunched != 1 {
				t.Fatalf("the come-back relaunched the session %d times and left %d outbox rows, want one relaunch and the one start that carries the phase's task",
					relaunched, rows)
			}
			rig.run()
			held, last := rig.claim().Pending, rig.claim().LastReady
			if held == nil || held.Task != resumeTaskOf(t, rig.pool, rig.engine, rig.issue.Key) || held.Phase != phase.Testing ||
				held.Origin != supervise.OriginResume {
				t.Fatalf("after the come-back the tester holds %+v, want the testing phase's resume task", held)
			}
			if held.QueuedAt.IsZero() || held.QueuedAt.Before(last) || held.QueuedAt.Sub(last) > taskWithinOfReady {
				t.Fatalf("the task was queued at %s, %s after the claim became ready at %s; want it within %s of that ready",
					held.QueuedAt, held.QueuedAt.Sub(last), last, taskWithinOfReady)
			}
		})
	}
}

// A phase holder that comes back ready with nothing pending in the session it finished its last
// task in is given nothing, whichever way it came back (dispatch://INFRA-2811 R4): the ready
// queues no start, the claim holds no task, and the record writes it no task number, so no value
// is written for it. It waits, and its session's resume reminder wakes it while its phase is open.
func TestEveryComeBackInTheSessionThatFinishedItsTaskGivesThePhaseHolderNothing(t *testing.T) {
	t.Parallel()
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			rig.workTheTask()
			finished := rig.claim()
			if finished.TaskFinished != finished.TaskWritten || finished.TaskFinishedSession != testerSession {
				t.Fatalf("the tester that worked its task records task %d of %d finished in %q, want its last task finished in %s",
					finished.TaskFinished, finished.TaskWritten, finished.TaskFinishedSession, testerSession)
			}

			comeBack(rig, testerSession)

			if rows, relaunched := outboxRows(t, rig.pool), rig.relaunched(); rows != 0 || relaunched != 1 {
				t.Fatalf("the come-back relaunched the session %d times and left %d outbox rows, want one relaunch and no row",
					relaunched, rows)
			}
			rig.run()
			if held := rig.claim(); held.Pending != nil || held.TaskWritten != finished.TaskWritten {
				t.Fatalf("after the come-back the tester holds %+v with task %d written, want nothing held and no task written past %d",
					held.Pending, held.TaskWritten, finished.TaskWritten)
			}
		})
	}
}

// A phase holder that comes back still holding its task is given no second copy: the task it holds
// is the work under way, and a second task for it would be a second turn of the same work. The one
// it holds is the one its worker takes, under the same delivery, which is the relaunch's own
// resend, not a task of its own.
func TestAComeBackWithATaskStillPendingGivesNoSecondCopy(t *testing.T) {
	t.Parallel()
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			delivered := rig.claim().Pending.ID
			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the first ready of a tester that holds its task left %d outbox rows, want none", rows)
			}

			comeBack(rig, testerSession)

			if rows, relaunched := outboxRows(t, rig.pool), rig.relaunched(); rows != 0 || relaunched != 1 {
				t.Fatalf("the come-back of a tester that holds its task relaunched the session %d times and left %d outbox rows, want one relaunch and no row",
					relaunched, rows)
			}
			if held := rig.claim().Pending; held == nil || held.ID != delivered || held.Task != "Test it." {
				t.Fatalf("after the come-back the tester holds %+v, want the one task it held, %s", held, delivered)
			}
			if take, accepted := takeOffered(context.Background(), t, rig.machine); !accepted || take.DeliveryID != delivered {
				t.Fatalf("the tester's take after the come-back = %+v (accepted %v), want the one task it holds, %s", take, accepted, delivered)
			}
		})
	}
}

// resumeRow is the id of the one start row the come-back's ready queued, which the come-back left
// as the outbox's only row.
func (r *readyWorkerRig) resumeRow() int64 {
	r.t.Helper()
	if rows := outboxRows(r.t, r.pool); rows != 1 {
		r.t.Fatalf("the come-back left %d outbox rows, want the one start that carries the phase's task", rows)
	}
	return theOneStartRow(r.t, r.pool, r.issue.Key).row.ID
}

// holdsResume fails the test unless the tester holds the testing phase's resume task delivered
// from row (supervise.Deps.ReadyResume, the outbox's readyResume), with that task's value written
// under its number, and answers the delivery held.
func (r *readyWorkerRig) holdsResume(row int64) supervise.Delivery {
	r.t.Helper()
	held := r.claim()
	id := fmt.Sprintf("%s%d", outboxDeliveryPrefix, row)
	p := held.Pending
	if p == nil || p.Task != resumeTaskOf(r.t, r.pool, r.engine, r.issue.Key) || p.Phase != phase.Testing ||
		p.Origin != supervise.OriginResume || p.ID != id || !slices.Contains(held.DeliveredRows, row) {
		r.t.Fatalf("at its ready the tester holds %+v from rows %v, want the testing phase's resume task delivered as %s from row %d",
			p, held.DeliveredRows, id, row)
	}
	if value, ok := r.values.Value(r.token); !ok || value.Number != p.Number {
		r.t.Fatalf("at its ready the tester's value is %+v (written %v), want task %d written", value, ok, p.Number)
	}
	return *p
}

// A phase holder that comes back ready with nothing pending in a new session holds its phase's
// resume task, its value written, by the time its ready is answered, whichever way it came back
// (R5 of dispatch://INFRA-4130). The worker reads its claim's value right after claims/ready
// answers, and an Envoy card waiting at the ready starts the session's first turn when that read
// finds no task: queued by the outbox, the resume came after the card's turn had started. So the
// ready's own decision delivers the row its fact queued (supervise.Deps.ReadyResume), and the
// row's own run in the outbox pass that follows finishes it with no second copy: the claim holds
// the same delivery, and no task number is written past it. The ready here is boot's
// RequestReady, handled through the machine as claims/ready hands it on, and the claim is read
// once that handling has returned, which is when claims/ready answers. With Deps.ReadyResume nil,
// as at the base, the claim holds nothing at the ready, and its task waits for the outbox pass.
func TestAReadyThatQueuesItsResumeHoldsItWithItsValueWrittenWhenTheReadyAnswers(t *testing.T) {
	t.Parallel()
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			rig.workTheTask()
			rig.rt.OnResume = func(context.Context) error { return runtime.ErrSessionFileMissing }

			comeBack(rig, replacedSession)

			row := rig.resumeRow()
			delivered := rig.holdsResume(row)
			written := rig.claim().TaskWritten

			rig.run()

			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the outbox pass after the ready left %d outbox rows, want the resume row finished", rows)
			}
			if held := rig.claim(); held.Pending == nil || held.Pending.ID != delivered.ID || held.TaskWritten != written {
				t.Fatalf("after the outbox pass the tester holds %+v with task %d written, want the one resume %s and no task written past %d",
					held.Pending, held.TaskWritten, delivered.ID, written)
			}
		})
	}
}

// An outbox pass that runs the resume row after the worker took the resume its ready delivered,
// and finished that turn, delivers no second resume, whichever way the phase holder came back in
// a new session: the row finishes, the claim holds nothing, and no task number is written past
// the resume's. The delivery the ready queued carried the row (supervise.RequestDeliver.Row), and
// the delivery's own write stored that row on the claim (supervise.Claim.DeliveredRows, the claims
// row's delivered_rows). So queue's given check reads the request of the row's own run as a row
// the claim was given already, and queues nothing, though the claim holds no task by then.
func TestAnOutboxPassAfterTheResumesTurnDeliversNoSecondResume(t *testing.T) {
	t.Parallel()
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			rig.workTheTask()
			rig.rt.OnResume = func(context.Context) error { return runtime.ErrSessionFileMissing }

			comeBack(rig, replacedSession)

			rig.holdsResume(rig.resumeRow())
			rig.workTheTask()
			written := rig.claim().TaskWritten

			rig.run()

			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the outbox pass after the resume's turn left %d outbox rows, want the resume row finished", rows)
			}
			if held := rig.claim(); held.Pending != nil || held.TaskWritten != written {
				t.Fatalf("after the outbox pass the tester holds %+v with task %d written, want nothing held and no task written past %d",
					held.Pending, held.TaskWritten, written)
			}
		})
	}
}

// R2: no ready is delivered a resume while its issue stands at admitted (outbox.readyResume), the
// architect's or a phase worker's. Admitted is the architect's own phase (phase.RoleFor), and an
// architect's ready never takes a resume here: readyWorkerTask queues it none, and readyResume
// answers an architect's claim nil before it reads anything. A planner's or an implementer's ready
// finds a phase its role does not work, and is answered nil before its run is read. Each role's run
// holds a resume row of exactly the shape readyWorkerTask writes, so each nil is the guard's, and
// not that of a run with no such row.
func TestArchitectUnchangedByAReadyResumeOfAnIssueAtAdmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-209", Title: "a root writing its design", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U",
		HandedOver: true}
	putOutboxIssue(t, pool, records, issue)
	roles := []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer}
	for _, role := range roles {
		enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role,
			Generation: issue.Generation, Phase: issue.Phase, Task: "Resume it.", Origin: supervise.OriginResume, ResumeTask: true},
			time.Now()))
	}
	runner := &outbox{log: quietLogger(), pool: pool, records: records}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			c := supervise.Claim{Token: mustClaimToken(t, issue.Key, role), Issue: issue.Key, Role: role}
			if delivery, err := runner.readyResume(ctx, c); delivery != nil || err != nil {
				t.Fatalf("the ready of the %s of an issue at admitted is delivered %+v (%v), want no resume", role, delivery, err)
			}
		})
	}
}
