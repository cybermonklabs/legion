package daemon

// The idle release's backstop of a merger the design gate holds (dispatch://INFRA-2040 R1, D13 of
// its plan): the wake tick reads the hold before it applies any fact and leaves such a backstop
// due, the fact reads it again for a tick that raced the gate's closing, and the first tick that
// reads the gate open takes the backstop. Each case drives the held-merger rig (newHeldMergerRig):
// the daemon's fact handlers, the outbox's executor and its fence, the claim's machine, a real
// idle release that arms the backstop, and the wake tick (fireDueBackstops), over a real
// PostgreSQL.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// heldBackstopLine and heldBackstopTakenLine are the wake tick's two lines of a held merger's
	// backstop (fireDueBackstops).
	heldBackstopLine      = `msg="wake: a held merger's backstop waits for the design gate"`
	heldBackstopTakenLine = `msg="wake: a held merger's backstop is taken"`
)

// backstopFacts counts the backstop facts the daemon's handlers are handed, placed before them: a
// fact the wake tick applies costs a transaction on the fact lock, which a held backstop must not.
// The tick runs on the test's goroutine, so the count needs no lock.
type backstopFacts struct{ applied int }

func (h *backstopFacts) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if _, ok := fact.(intake.PodBackstopDue); ok {
		h.applied++
	}
	return intake.Result{}, nil
}

// countsBackstopFacts places a backstopFacts before the rig's fact handlers, which the wake tick
// applies its facts through, and answers it.
func (r *waitDeliveryRig) countsBackstopFacts() *backstopFacts {
	counted := &backstopFacts{}
	r.w.handlers = append([]intake.Handler{counted}, r.w.handlers...)
	return counted
}

// gateClosesFirst is the design gate's closing committing between the wake tick's read of the hold
// and its backstop fact: placed before the daemon's handlers, it writes the closed gate's row in
// the first backstop fact's transaction before the engine reads it, as the gate's own fact would
// have written it before the backstop fact took the lock.
type gateClosesFirst struct {
	records record.Store
	gate    record.DesignGate
	facts   int
}

func (h *gateClosesFirst) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if _, ok := fact.(intake.PodBackstopDue); !ok {
		return intake.Result{}, nil
	}
	h.facts++
	if h.facts > 1 {
		return intake.Result{}, nil
	}
	return intake.Result{}, h.records.PutGate(ctx, tx, h.gate)
}

// bootsWith is the daemon stopping and a new one starting over the same Postgres under a
// deployment that gates steps: a new workflow engine, workflow runtime and outbox reading those
// steps and holding nothing of the first daemon's memory, the supervisor restored from the stored
// claims (restart), and the boot's fact, which resumes no released claim (workflowRuntime.booted).
func (r *waitDeliveryRig) bootsWith(steps []record.DesignStep) {
	r.t.Helper()
	podRecords, err := r.st.PodRecordsOf(r.issue.Project)
	if err != nil {
		r.t.Fatalf("read the project's pod records: %v", err)
	}
	engine := workflow.New(r.records, workflow.Config{Project: r.issue.Project, Repositories: testRepositories, Steps: steps,
		Pods: podRecords}, r.log)
	admission := admit.New(r.records, engine, 1, r.issue.Project, r.log)
	previous := r.w
	r.w = &workflowRuntime{pool: r.pool, records: r.records, engine: engine, admission: admission,
		handlers: factHandlers(r.records, r.issue.Project, engine, admission), gatedSteps: steps,
		dispatchProject: r.issue.Project, bootID: "test-boot-2", log: r.log, projectID: previous.projectID,
		idleStore: previous.idleStore, idleRuntime: previous.idleRuntime, idlePeriod: previous.idlePeriod}
	r.runner.gatedSteps, r.runner.handlers = steps, r.w.handlers
	r.restart()
	if err := r.w.booted(r.ctx, 2); err != nil {
		r.t.Fatalf("apply the boot: %v", err)
	}
}

// heldBackstopArmed puts the rig's merger to work, closes the gate over it unless open, and runs
// an idle release of its pod, which arms its backstop; it answers that backstop.
func heldBackstopArmed(t *testing.T, r *waitDeliveryRig, closes bool) armedBackstop {
	t.Helper()
	r.mergerWorked()
	if closes {
		r.closesTheGate()
	}
	r.releasesAt(time.Now().UTC().Add(time.Minute))
	armed := openBackstops(t, r.pool, r.issue.Key)
	if len(armed) != 1 || armed[0].role != claim.RoleMerger {
		t.Fatalf("the backstops after the release = %+v, want the merger's one", armed)
	}
	return armed[0]
}

// A backstop that comes due for a held merger whose pod was released is not taken while the gate is
// closed, and a CI wake that follows it still passes the fence and reaches the merger (R1). Two
// wake ticks past the backstop's time apply no fact for it, since the tick's own read of the hold
// (heldMergerBackstops) passes it over: the backstop stays armed and unfired, no resume is
// recorded, the merger stays released, and the hold is logged once for the backstop, not once
// per tick. A settlement of the merger's head then wakes it: no resume of the backstop stands open
// to refuse the wake at pod_resumes_one_open_per_role, the wake cancels the backstop, and its start,
// which names no phase, passes the fence and gives the relaunched merger the settlement's task.
// What the ticks leave is checked without stopping the case, so a backstop taken during the hold
// also shows what it does to the CI wake after it.
func TestAHeldMergersBackstopWaitsForTheGateAndACIWakeAfterItReachesTheMerger(t *testing.T) {
	r := newHeldMergerRig(t)
	backstop := heldBackstopArmed(t, r, true)
	counted := r.countsBackstopFacts()

	for _, at := range []time.Time{backstop.dueAt, backstop.dueAt.Add(time.Minute)} {
		if resumes := r.backstopsFired(at); len(resumes) != 0 {
			t.Errorf("the pod resumes after the wake tick at %s = %v, want none: the gate holds the merger", at, resumes)
		}
	}
	if applied := counted.applied; applied != 0 {
		t.Errorf("the wake ticks applied %d backstop facts, want none: a held backstop costs no fact", applied)
	}
	if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 1 || !armed[0].dueAt.Equal(backstop.dueAt) {
		t.Errorf("the backstops after the held ticks = %+v, want the merger's one still armed and unfired", armed)
	}
	if got := r.machine.Claim(); !got.Released() {
		t.Errorf("the merger after the held ticks = %s by %s, want it still released: its backstop gives it no turn", got.State, got.SuspendedBy)
	}
	if lines := r.lines(heldBackstopLine, "issue="+r.issue.Key, "role=merger", "version=2"); len(lines) != 1 {
		t.Errorf("the hold of the backstop was logged %d times, want once", len(lines))
	}

	r.applies("settled", heldMergerSettlement())
	if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeChecks {
		t.Fatalf("the pod resumes after the settlement = %v, want its one wake of the merger: no resume of the backstop stands open for pod_resumes_one_open_per_role to refuse it by",
			resumes)
	}
	if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
		t.Fatalf("the backstops after the CI wake = %+v, want none: the wake cancels the merger's backstop", armed)
	}
	starts := entryStartRows(t, r.pool, r.issue.Key)
	if len(starts) != 1 || starts[0].request.Phase != "" {
		t.Fatalf("the start rows after the settlement = %+v, want the wake's one start, naming no phase", starts)
	}
	r.drainAt(backstop.dueAt.Add(2*time.Minute), record.OutboxKindSupervise)
	if _, queued := r.outboxRow(starts[0].row.ID); queued {
		t.Fatal("the CI wake's start is still queued after the outbox ran it, want it passed through the fence")
	}
	r.boot()
	if woken := r.takeTask(); !strings.Contains(woken.Task, "The wake is a CI settlement on your pull request's head: "+heldMergerHead) {
		t.Fatalf("the merger was sent %q, want the CI wake's task", woken.Task)
	}
}

// A wake tick that races the gate's closing still leaves the held merger's backstop unfired (R1):
// the tick reads the gate open and applies the backstop's fact, and the closing commits before the
// fact takes the lock. The fact reads the hold in its own transaction (workflow's podBackstopDue):
// it fires nothing, records no resume and is left unspent, and the tick logs the hold. The next
// tick reads the hold itself and applies no fact. Once the gate stands open again, the first tick
// takes the backstop, logs that it took it, and wakes the merger.
func TestAHeldMergersBackstopThatRacesTheGatesClosingIsLeftUnfired(t *testing.T) {
	r := newHeldMergerRig(t)
	backstop := heldBackstopArmed(t, r, false)
	approved := 1
	racing := &gateClosesFirst{records: r.records, gate: record.DesignGate{Issue: r.issue.Key, Step: record.DesignStepSpec,
		ArtifactID: heldMergerSpec, LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true}}
	r.w.handlers = append([]intake.Handler{racing}, r.w.handlers...)

	if resumes := r.backstopsFired(backstop.dueAt); len(resumes) != 0 {
		t.Fatalf("the pod resumes after the tick that raced the closing = %v, want none: the fact read the gate closed", resumes)
	}
	if racing.facts != 1 {
		t.Fatalf("the tick applied %d backstop facts, want the one its read of the open gate let through", racing.facts)
	}
	if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 1 {
		t.Fatalf("the backstops after the race = %+v, want the merger's one armed and unfired", armed)
	}
	event := backstopEvent(r, backstop)
	if applied, err := intake.Applied(r.ctx, r.pool, "wake", []string{event}); err != nil || applied[event] {
		t.Fatalf("the backstop's fact %s is marked applied (%v, %v), want it unspent so a later tick applies it again", event, applied, err)
	}
	r.wantReleased("the backstop that raced the closing gives the held merger no turn")
	if lines := r.lines(heldBackstopLine, "issue="+r.issue.Key, "role=merger"); len(lines) != 1 {
		t.Fatalf("the hold of the backstop was logged %d times, want once; the log is:\n%s", len(lines), r.logs)
	}

	if resumes := r.backstopsFired(backstop.dueAt.Add(time.Minute)); len(resumes) != 0 || racing.facts != 1 {
		t.Fatalf("the next tick woke %v and applied %d backstop facts in all, want no wake and no fact past the race's one", resumes, racing.facts)
	}

	approvedNow := 2
	putOutboxGate(t, r.pool, r.records, record.DesignGate{Issue: r.issue.Key, Step: record.DesignStepSpec, ArtifactID: heldMergerSpec,
		LatestVersion: 2, ApprovedVersion: &approvedNow, EverOpened: true})
	if resumes := r.backstopsFired(backstop.dueAt.Add(2 * time.Minute)); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeBackstop {
		t.Fatalf("the pod resumes once the gate stands open = %v, want the backstop taken", resumes)
	}
	if lines := r.lines(heldBackstopTakenLine, "issue="+r.issue.Key, "role=merger"); len(lines) != 1 {
		t.Fatalf("the taking of the held backstop was logged %d times, want once; the log is:\n%s", len(lines), r.logs)
	}
}

// backstopEvent is the event id the wake tick applies the rig's backstop under.
func backstopEvent(r *waitDeliveryRig, backstop armedBackstop) string {
	r.t.Helper()
	var id int64
	if err := r.pool.QueryRow(r.ctx, `select id from pod_wake_backstops where issue = $1 and role = $2 and due_at = $3`,
		r.issue.Key, string(backstop.role), backstop.dueAt).Scan(&id); err != nil {
		r.t.Fatalf("read the backstop's id: %v", err)
	}
	return fmt.Sprintf("pod-backstop:%d", id)
}

// After `gates.design` is emptied across a daemon restart, which writes no gate row, the first
// wake tick takes the backstop the hold left due and wakes the merger (R1; the restart case
// c-hold-quiet and c-gate-hold share). The gate closes over the merger at work, the idle release
// takes its pod, and a tick past the backstop's time leaves it due. The daemon then boots on a
// deployment that gates no step: the boot resumes no released claim, the merger is held by
// nothing, and the first tick fires the backstop; its start passes the fence and the relaunched
// merger takes the backstop's task.
func TestAHeldMergersBackstopIsTakenAtTheFirstTickAfterABootThatEmptiesTheGatedSteps(t *testing.T) {
	r := newHeldMergerRig(t)
	backstop := heldBackstopArmed(t, r, true)
	if resumes := r.backstopsFired(backstop.dueAt); len(resumes) != 0 {
		t.Fatalf("the pod resumes after the tick during the hold = %v, want none", resumes)
	}

	r.bootsWith(nil)
	if held, holds := workflow.HeldByDesignGate(r.issue, claim.RoleMerger, r.treeGate()); holds {
		t.Fatalf("the merger after the boot is held on %+v, want no hold: no step is gated", held)
	}
	r.wantReleased("the boot resumes no released claim")

	if resumes := r.backstopsFired(backstop.dueAt.Add(time.Minute)); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeBackstop {
		t.Fatalf("the pod resumes after the boot's first tick = %v, want the merger's backstop taken", resumes)
	}
	if armed := openBackstops(t, r.pool, r.issue.Key); len(armed) != 0 {
		t.Fatalf("the backstops after the boot's first tick = %+v, want none left unfired", armed)
	}
	r.drainAt(backstop.dueAt.Add(2*time.Minute), record.OutboxKindSupervise)
	r.boot()
	if woken := r.takeTask(); !strings.Contains(woken.Task, "The wake is the release's backstop running out") {
		t.Fatalf("the merger was sent %q, want the backstop's wake task", woken.Task)
	}
}

// A backstop wake taken while the gate was open, whose start has not run when the gate closes,
// reaches the merger (R1, R7): the wake tick takes the backstop of the merger released under the
// open gate and queues its start, the gate then closes, and that start, which names no phase,
// passes the fence and gives the relaunched merger the backstop's task.
func TestABackstopWakeTakenBeforeTheGateClosedReachesTheHeldMerger(t *testing.T) {
	r := newHeldMergerRig(t)
	backstop := heldBackstopArmed(t, r, false)
	if resumes := r.backstopsFired(backstop.dueAt); len(resumes) != 1 || resumes[0] != "merger:"+pods.WakeBackstop {
		t.Fatalf("the pod resumes after the tick under the open gate = %v, want the backstop taken", resumes)
	}
	starts := entryStartRows(t, r.pool, r.issue.Key)
	if len(starts) != 1 || starts[0].request.Phase != "" {
		t.Fatalf("the start rows after the backstop = %+v, want its one wake start, naming no phase", starts)
	}

	r.closesTheGate()
	r.drainAt(backstop.dueAt.Add(time.Minute), record.OutboxKindSupervise)
	if _, queued := r.outboxRow(starts[0].row.ID); queued {
		t.Fatal("the backstop's wake start is still queued after the outbox ran it under the closed gate, want it passed through the fence")
	}
	r.boot()
	if woken := r.takeTask(); !strings.Contains(woken.Task, "The wake is the release's backstop running out") {
		t.Fatalf("the merger was sent %q, want the backstop's wake task", woken.Task)
	}
}
