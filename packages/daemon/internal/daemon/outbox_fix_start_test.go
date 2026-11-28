package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The fix a start serves, and what its start names: the head the fix's working copy is made at,
// the fix's own branch and the head the task's working copy moves to.
const (
	fixStartIssue  = "LEGION-208"
	fixStartEntry  = 2
	fixStartHead   = "c0de000000000000000000000000000000000001"
	fixStartMove   = "c0de000000000000000000000000000000000002"
	fixStartBranch = "legion-beside/LEGION-208/1/default/fix-1/implementer"
	fixStartTask   = "Fix the CI check k-ci, which failed at " + fixStartHead + "."

	// phaseLeftLine and fixEndedLine are the start fence's two lines: a start of no fix whose
	// phase the issue has left, and a start of a fix that no longer runs.
	phaseLeftLine = "outbox start serves a phase the issue has left; finished without acting"
	fixEndedLine  = "outbox start serves a fix that no longer runs; finished without acting"
)

// fixStartRig is an issue whose task's fix 1 runs beside the step the issue stands in: the record
// holds the issue, the implementer's phases row at the fix's entry and the fix itself, and the
// outbox runs over a supervisor of its own, whose machines ask the daemon's own answer whether a
// task is still its issue's work (workflowRuntime.phaseHolds) and adopt a phase task's working copy
// under the role's App as the daemon wires them (adoptingAsTheDaemonDoes). The implementer's agent
// answers on a fake connection of its own, and its log is kept.
type fixStartRig struct {
	t       *testing.T
	pool    *store.Pool
	records *record.Postgres
	issue   record.Issue
	runner  *outbox
	sup     *supervisor
	rt      *fake.Runtime
	conn    *fake.Conn
	token   claim.Token
	logs    *syncBuffer
}

// newFixStartRig records the issue as stands leaves it, beside fix 1 of its task default, running
// at entry fixStartEntry of the implementer.
func newFixStartRig(t *testing.T, stands func(*record.Issue)) *fixStartRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: fixStartIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: fixStartIssue, Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "in_progress"}
	stands(&issue)
	putOutboxIssue(t, pool, records, issue)
	token, err := claim.NewToken("legion", fixStartIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: fixStartIssue, Role: claim.RoleImplementer, Claim: token,
			Entry: fixStartEntry, EntryTask: fixStartTask, EntryRaisedAt: time.Now().UTC()}); err != nil {
			return err
		}
		fix := record.TaskFix{Issue: fixStartIssue, Generation: 1, Task: "default", Fix: 1, State: record.FixReading,
			Check: "k-ci", Head: fixStartHead, Place: 1}
		if err := records.PutFix(ctx, tx, fix); err != nil {
			return err
		}
		fix.State, fix.Entry = record.FixRunning, fixStartEntry
		return records.UpdateFix(ctx, tx, fix)
	}); err != nil {
		t.Fatalf("record the implementer's entry and the running fix: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).phaseHolds
	adoptingAsTheDaemonDoes(sup)
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	logs := &syncBuffer{}
	runner := &outbox{log: slog.New(slog.NewJSONHandler(logs, nil)), pool: pool, dispatchProject: "LEGION", records: records,
		supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + fixStartIssue}, nil
		},
	}
	if got := readIssue(t, pool, fixStartIssue); got.RunningFix != 1 {
		t.Fatalf("the record names running fix %d, want 1: the case's own premise", got.RunningFix)
	}
	return &fixStartRig{t: t, pool: pool, records: records, issue: issue, runner: runner, sup: sup, rt: rt, conn: conn, token: token, logs: logs}
}

// fixStartOf is the start of fix 1's task on the implementer: phase implementing, entry
// fixStartEntry, the fix's head and the move, as the workflow writes it (Store.EnqueueFixStart).
func fixStartOf() record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: fixStartIssue, Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Entry: fixStartEntry, Task: fixStartTask, Fix: 1, FixFrom: fixStartHead, FixBranch: fixStartBranch,
		MoveTo: fixStartMove}
}

// queue writes start as the issue's outbox row and answers the row with its id, as a lane reads it.
func (r *fixStartRig) queue(start record.SuperviseRequest) record.OutboxRow {
	r.t.Helper()
	row := mustOutboxRow(r.t, fixStartIssue, start, time.Now())
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row.ID, err = r.records.EnqueueID(ctx, tx, row)
		return err
	}); err != nil {
		r.t.Fatalf("queue the start: %v", err)
	}
	return row
}

// run runs row through the start executor, as its lane would.
func (r *fixStartRig) run(row record.OutboxRow) {
	r.t.Helper()
	if err := r.runner.execute(context.Background(), row); err != nil {
		r.t.Fatalf("run the start row %d: %v", row.ID, err)
	}
}

// fenced is each line the start fence logged.
func (r *fixStartRig) fenced() []map[string]any {
	r.t.Helper()
	logs := r.logs.String()
	return append(loggedLines(r.t, logs, phaseLeftLine), loggedLines(r.t, logs, fixEndedLine)...)
}

// tookTheTask holds that row passed the fence and that the implementer's claim took its task: the
// claim records the row and the fix's entry as given, and holds the row's own delivery, of phase
// implementing and that entry.
func (r *fixStartRig) tookTheTask(row record.OutboxRow) {
	t := r.t
	t.Helper()
	if fenced := r.fenced(); len(fenced) != 0 {
		t.Fatalf("the start fence finished the fix's start: %v", fenced)
	}
	machine, found := r.sup.Machine(r.token)
	if !found {
		t.Fatal("the fix's start created no claim for the implementer")
	}
	machine.Wait()
	held := machine.Claim()
	if !slices.Contains(held.DeliveredRows, row.ID) || held.GivenEntry != fixStartEntry {
		t.Fatalf("the implementer's claim records rows %v and was given entry %d, want row %d and entry %d: the claim took the fix's task",
			held.DeliveredRows, held.GivenEntry, row.ID, fixStartEntry)
	}
	want := fmt.Sprintf("%s%d", outboxDeliveryPrefix, row.ID)
	if held.Pending == nil || held.Pending.ID != want || held.Pending.Task != fixStartTask ||
		held.Pending.Phase != phase.Implementing || held.Pending.Entry != fixStartEntry {
		t.Fatalf("the implementer's pending delivery = %+v, want the row's own delivery %s of the fix's task, phase %s, entry %d",
			held.Pending, want, phase.Implementing, fixStartEntry)
	}
}

// sentTheFixsTask holds that the fix's task the implementer's claim took reaches its agent once
// the claim is ready, whatever phase the issue stands in. The machine's own fence of the send asks
// the daemon's answer with the task's fix (workflowRuntime.phaseHolds), so the task is not retired
// for the phase the issue stands in; and the send adopts first, naming the fix's head and the move,
// under the implementer's App, before the agent is prompted with the fix's task.
func (r *fixStartRig) sentTheFixsTask() {
	t := r.t
	t.Helper()
	machine, found := r.sup.Machine(r.token)
	if !found {
		t.Fatal("the fix's start created no claim for the implementer")
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: generation},
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: "ses-impl", SessionFile: "/sessions/impl.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: "ses-impl"},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T for the implementer: %v", ev, err)
		}
	}
	machine.Wait()
	held := machine.Claim().Pending
	if held == nil {
		t.Fatalf("the implementer's ready claim holds no task, want the fix's task sent: the send's fence retired it (issue %s in %s)",
			fixStartIssue, readIssue(t, r.pool, fixStartIssue).Phase)
	}
	if held.Fix != 1 || held.FixFrom != fixStartHead || held.MoveTo != fixStartMove {
		t.Fatalf("the implementer's pending delivery carries fix %d from %q moving to %q, want fix 1 from %s moving to %s",
			held.Fix, held.FixFrom, held.MoveTo, fixStartHead, fixStartMove)
	}
	testwait.Eventually(t, "the fix's task to reach the implementer's agent", func() bool { return len(r.conn.Prompts()) == 1 })
	if sent := r.conn.Prompts()[0]; sent.DeliveryID != held.ID || !strings.Contains(sent.Message, fixStartTask) {
		t.Fatalf("the implementer's agent was sent %+v, want the fix's task, delivery %s", sent, held.ID)
	}
	want := runtime.Adoption{FixFrom: fixStartHead, MoveTo: fixStartMove, FixBranch: fixStartBranch}
	made := r.rt.CallsOf("AdoptWorkingCopy")
	if len(made) != 1 || made[0].Adoption != want || made[0].Identity.Name != "legion-implementer[bot]" {
		t.Fatalf("adoptions = %+v, want one naming %+v under the implementer's App", made, want)
	}
}

// finishedByTheFence holds that row was finished without acting by the start fence, on line, which
// names the fix the row serves where it serves one (fix above 0) and no fix otherwise, and that no
// claim was made for the implementer.
func (r *fixStartRig) finishedByTheFence(line string, fix int) {
	t := r.t
	t.Helper()
	logged := loggedLines(t, r.logs.String(), line)
	if len(logged) != 1 {
		t.Fatalf("logged %q %d times, want once: %s", line, len(logged), r.logs.String())
	}
	named, ok := logged[0]["fix"]
	if fix > 0 && named != float64(fix) || fix == 0 && ok {
		t.Fatalf("%q names fix %v (named: %t), want %d", line, named, ok, fix)
	}
	if _, found := r.sup.Machine(r.token); found {
		t.Fatal("the fenced start made a claim for the implementer")
	}
}

// A fix's start names phase implementing while the issue stands in reviewing, the step beside it.
// The start fence holds it to the fix instead of the phase (record.PhaseHolds): with fix 1 running,
// the start passes, and the implementer's claim takes the fix's task, with the row and the fix's
// entry; once the claim is ready, the machine's fence of the send holds it to the fix too, and the
// task reaches the agent after an adoption that names the fix's head. The controls are the same
// start of no fix, which the fence finishes as it finishes any start of a phase the issue has left,
// and a start of fix 1 once fix 1 ended, which it finishes too, with a line that names the fix.
func TestAFixStartRunsWhileTheIssueIsInReviewing(t *testing.T) {
	t.Parallel()

	t.Run("fix 1's start while fix 1 runs", func(t *testing.T) {
		t.Parallel()
		rig := newFixStartRig(t, func(*record.Issue) {})
		row := rig.queue(fixStartOf())
		rig.run(row)
		rig.tookTheTask(row)
		rig.sentTheFixsTask()
	})

	t.Run("control: the same start of no fix", func(t *testing.T) {
		t.Parallel()
		rig := newFixStartRig(t, func(*record.Issue) {})
		start := fixStartOf()
		start.Fix, start.FixFrom, start.FixBranch = 0, "", ""
		rig.run(rig.queue(start))
		rig.finishedByTheFence(phaseLeftLine, 0)
	})

	t.Run("control: fix 1's start once fix 1 ended", func(t *testing.T) {
		t.Parallel()
		rig := newFixStartRig(t, func(*record.Issue) {})
		if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			fix, err := rig.records.RunningFix(ctx, tx, fixStartIssue, 1, "default")
			if err != nil || fix == nil {
				return fmt.Errorf("read the running fix: %+v, %w", fix, err)
			}
			fix.State, fix.Result = record.FixEnded, "passed"
			return rig.records.UpdateFix(ctx, tx, *fix)
		}); err != nil {
			t.Fatalf("end fix 1: %v", err)
		}
		rig.run(rig.queue(fixStartOf()))
		rig.finishedByTheFence(fixEndedLine, 1)
	})
}

// A fix's start the lane runs only after the issue moved to held for the fix (record.HoldForFix: a
// step beside the fix failed and its return waits for the fix, or the last step passed and the
// retro waits) still passes the start fence, since the fix still runs, and the implementer's claim
// takes its task; the machine's fence of the send holds it to the fix too, and the task reaches the
// agent after an adoption that names the fix's head. The control is the same start of no fix, which
// the fence finishes: the issue stands in no phase of implementing.
func TestAFixStartRunsAfterTheIssueIsHeldForTheFix(t *testing.T) {
	t.Parallel()
	held := func(issue *record.Issue) {
		issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Reviewing, Reason: record.HoldForFix}
	}

	t.Run("fix 1's start while the issue is held for it", func(t *testing.T) {
		t.Parallel()
		rig := newFixStartRig(t, held)
		row := rig.queue(fixStartOf())
		rig.run(row)
		rig.tookTheTask(row)
		rig.sentTheFixsTask()
	})

	t.Run("control: the same start of no fix", func(t *testing.T) {
		t.Parallel()
		rig := newFixStartRig(t, held)
		start := fixStartOf()
		start.Fix, start.FixFrom, start.FixBranch = 0, "", ""
		rig.run(rig.queue(start))
		rig.finishedByTheFence(phaseLeftLine, 0)
	})
}

// The task a fix's start gives its claim (supervise.RequestDeliver, which the machine builds the
// delivery from) carries the fix, the head the fix's working copy is made at, the fix's own branch
// and the head the task's working copy moves to, as the row names them, beside the row's phase,
// run, entry and id. The control is the same start of no fix and no move: its task carries none
// of the four.
func TestAFixStartGivesItsTaskTheFixItsCopyAndItsMove(t *testing.T) {
	t.Parallel()
	token := claim.Token("legion-legion-legion-208-implementer")
	row := record.OutboxRow{ID: 41, Issue: fixStartIssue, Kind: record.OutboxKindSupervise}
	task := deliverRequestOf(token, row, fixStartOf(), startNews{task: fixStartTask}, nil)
	if task.Claim != token || task.Task != fixStartTask || task.Phase != phase.Implementing || task.Generation != 1 ||
		task.Entry != fixStartEntry || task.Row != row.ID || task.ID != fmt.Sprintf("%s%d", outboxDeliveryPrefix, row.ID) {
		t.Fatalf("the fix's task = %+v, want the row's task of phase %s, run 1, entry %d, row %d", task, phase.Implementing, fixStartEntry, row.ID)
	}
	if task.Fix != 1 || task.FixFrom != fixStartHead || task.FixBranch != fixStartBranch || task.MoveTo != fixStartMove {
		t.Fatalf("the fix's task carries fix %d from %q on branch %q moving to %q, want fix 1 from %s on %s moving to %s",
			task.Fix, task.FixFrom, task.FixBranch, task.MoveTo, fixStartHead, fixStartBranch, fixStartMove)
	}

	plain := fixStartOf()
	plain.Fix, plain.FixFrom, plain.FixBranch, plain.MoveTo = 0, "", "", ""
	if task := deliverRequestOf(token, row, plain, startNews{task: fixStartTask}, nil); task.Fix != 0 || task.FixFrom != "" ||
		task.FixBranch != "" || task.MoveTo != "" {
		t.Fatalf("control: the task of a start of no fix carries fix %d from %q on branch %q moving to %q, want none of them",
			task.Fix, task.FixFrom, task.FixBranch, task.MoveTo)
	}
}

// fixStarting writes the start of a fix's task inside the fact's transaction, as the workflow's
// startFix does (through record.Store.EnqueueFixStart), and keeps the id the row took.
type fixStarting struct {
	records record.Store
	row     record.OutboxRow
	id      *int64
}

func (h fixStarting) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	if err := h.records.EnqueueFixStart(ctx, tx, h.row); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise'`, h.row.Issue).Scan(h.id)
}

// A fix's start runs on its lane as soon as the fact that wrote it commits, as every other row
// does. The workflow writes it through record.Store.EnqueueFixStart, beside the line of starts that
// wait for a slot, so the workflow's store (projectRecords) wakes the issue's lifecycle lane there
// as it does at EnqueueStart: without that wake the fix whose failed job was just read waits for
// the next sweep, up to one outbox lease after the read. The lanes run on a held clock, so no
// sweep fires: the commit wakes the lane, and the implementer's claim takes the fix's task on that
// wake alone.
//
// The control, a kept subtest: the same start written past the workflow's store wakes no lane and
// stays queued, with no claim made, until the test fires the sweep, which then runs it.
func TestAFixStartRunsOnItsLaneWithoutASweep(t *testing.T) {
	t.Parallel()
	play := func(t *testing.T, workflowStore bool) (*fixStartRig, *heldClock, record.OutboxRow, []string) {
		t.Helper()
		r := newFixStartRig(t, func(*record.Issue) {})
		clock := microClock()
		var mu sync.Mutex
		var woken []string
		var records record.Store = r.records
		if workflowStore {
			records = projectRecords{Store: r.records, project: "LEGION", log: quietLogger(), wake: func(issue string, kind record.OutboxKind) {
				mu.Lock()
				woken = append(woken, issue+":"+string(kind))
				mu.Unlock()
				r.runner.wake(issue, kind)
			}}
		}
		runLanes(t, r.runner, clock)
		row := mustOutboxRow(t, fixStartIssue, fixStartOf(), clock.Now())
		if _, err := intake.ApplyFact(context.Background(), r.pool, "lanes-test", "fix-start", intake.DaemonBooted{Boot: 1},
			fixStarting{records: records, row: row, id: &row.ID}); err != nil {
			t.Fatalf("commit the fix's start: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return r, clock, row, slices.Clone(woken)
	}
	took := func(t *testing.T, r *fixStartRig, row record.OutboxRow) {
		t.Helper()
		testwait.Eventually(t, "the implementer's claim took the fix's task and the start row finished", func() bool {
			_, found := r.sup.Machine(r.token)
			return found && outboxRowsOfKind(t, r.pool, fixStartIssue, record.OutboxKindSupervise) == 0
		})
		r.tookTheTask(row)
	}
	t.Run("the commit wakes the lane, which runs the start", func(t *testing.T) {
		t.Parallel()
		r, clock, row, woken := play(t, true)
		if want := fixStartIssue + ":" + string(record.OutboxKindSupervise); !slices.Equal(woken, []string{want}) {
			t.Fatalf("the commit of the fix's start woke the lanes %v, want %s's: the start waits for the sweep", woken, want)
		}
		took(t, r, row)
		if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
			t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired", sweeps)
		}
	})
	t.Run("control: written past the workflow's store, the start waits for the sweep", func(t *testing.T) {
		t.Parallel()
		r, clock, row, woken := play(t, false)
		if len(woken) != 0 {
			t.Fatalf("the bare store woke %v, want nothing", woken)
		}
		if _, found := r.sup.Machine(r.token); found || outboxRowsOfKind(t, r.pool, fixStartIssue, record.OutboxKindSupervise) != 1 {
			t.Fatalf("before the sweep: claim made %t, %d start rows; want no claim and the start still queued",
				found, outboxRowsOfKind(t, r.pool, fixStartIssue, record.OutboxKindSupervise))
		}
		fireSweep(t, clock)
		took(t, r, row)
	})
}
