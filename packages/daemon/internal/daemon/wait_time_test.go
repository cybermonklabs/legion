package daemon

// The wait's time (dispatch://INFRA-2040 R3's first end, D5 and D7 of its plan), through the
// daemon's own pieces over a real PostgreSQL, as waitDeliveryRig drives them: the declaration's
// wait_due row, run by the outbox's executor as its lanes run a row (claimed, run, then finished or
// put back on its backoff), the workflow's end of the wait and the wait's own task, and that
// task's start reaching the claim's machine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// waitTimeKinds are the outbox kinds a wait's time runs through: its own row, and the start that
// carries its task.
var waitTimeKinds = []record.OutboxKind{record.OutboxKindWaitDue, record.OutboxKindSupervise}

// idleRelease puts the rig under a configuration with an idle release, as `runtime: kubernetes`
// runs one with idle_minutes set: the workflow's idle period and the store its release reads, and
// a runtime that bundles an issue's workspace and takes its pod down (releaseRuntime). It is
// called before the tester first launches, so every process of the tester runs in that runtime.
func (r *waitDeliveryRig) idleRelease() *releaseRuntime {
	r.t.Helper()
	rt := &releaseRuntime{Runtime: r.sup.deps.Runtime.(*fake.Runtime), onImage: true}
	r.sup.deps.Runtime = rt
	r.w.projectID, r.w.idleStore, r.w.idleRuntime, r.w.idlePeriod = "legion", r.st, rt, 10*time.Minute
	return rt
}

// dueAt is every row of the tester's issue of kinds the outbox lists due at at, in the order a lane
// runs them (record.Store.DueRows).
func (r *waitDeliveryRig) dueAt(at time.Time, kinds ...record.OutboxKind) []record.OutboxRow {
	r.t.Helper()
	var due []record.OutboxRow
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		due, err = r.records.DueRows(r.ctx, tx, r.issue.Project, r.issue.Key, kinds, at)
		return err
	}); err != nil {
		r.t.Fatalf("list the rows due at %s: %v", at, err)
	}
	return due
}

// drainAt runs the rows of the tester's issue of kinds that are due at at, as the issue's lanes
// run them (outbox.runRow: claimed, run, then finished, or put back on their backoff when they
// wait), pass after pass until a pass runs nothing, and answers every row it ran. A row that a row
// it ran wrote is listed by the next pass, as a lane's next drain lists it; a row put back on its
// backoff is due after at, so it waits for a later drain.
func (r *waitDeliveryRig) drainAt(at time.Time, kinds ...record.OutboxKind) []record.OutboxRow {
	r.t.Helper()
	r.runner.now = func() time.Time { return at }
	var ran []record.OutboxRow
	for range 6 {
		due := r.dueAt(at, kinds...)
		if len(due) == 0 {
			return ran
		}
		for _, row := range due {
			if err := r.runner.runRow(r.ctx, row); err != nil {
				r.t.Fatalf("run row %d: %v", row.ID, err)
			}
			r.settled()
		}
		ran = append(ran, due...)
	}
	r.t.Fatalf("rows were still due at %s after six passes", at)
	return nil
}

// settled waits for whatever the rows the outbox ran set going in the tester's machine, which a
// start that created the claim gave it.
func (r *waitDeliveryRig) settled() {
	if machine, found := r.sup.Machine(r.token); found {
		r.machine = machine
		machine.Wait()
	}
}

// outboxRow is the outbox row id as the outbox holds it now, and whether it does.
func (r *waitDeliveryRig) outboxRow(id int64) (record.OutboxRow, bool) {
	r.t.Helper()
	row := record.OutboxRow{ID: id}
	var kind string
	err := r.pool.QueryRow(r.ctx, `select kind, issue, payload, attempts, next_at, last_error from outbox where id = $1`, id).
		Scan(&kind, &row.Issue, &row.Payload, &row.Attempts, &row.NextAt, &row.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return record.OutboxRow{}, false
	}
	if err != nil {
		r.t.Fatalf("read outbox row %d: %v", id, err)
	}
	row.Kind = record.OutboxKind(kind)
	return row, true
}

// superviseOf is the request a supervise row carries.
func superviseOf(t *testing.T, row record.OutboxRow) record.SuperviseRequest {
	t.Helper()
	var request record.SuperviseRequest
	if err := json.Unmarshal(row.Payload, &request); err != nil {
		t.Fatalf("decode supervise row %d: %v", row.ID, err)
	}
	return request
}

// resumes is every pod_resumes row of the tester's issue, oldest first, as role:kind.
func (r *waitDeliveryRig) resumes() []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select role || ':' || kind from pod_resumes where issue = $1 order by id`, r.issue.Key)
	if err != nil {
		r.t.Fatalf("read the pod resumes: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var resume string
		if err := rows.Scan(&resume); err != nil {
			r.t.Fatalf("read a pod resume: %v", err)
		}
		found = append(found, resume)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the pod resumes: %v", err)
	}
	return found
}

// markRelease marks an idle release of the tester's pod in progress, as the daemon's idle pass
// marks one (store.ReleaseIdleIssue, idleReleaseRows): the save of the workspace, the stop of the
// tester, which is the release's boundary, and the teardown, queued on the issue's lifecycle lane
// with the release's mark, none of them run yet.
func (r *waitDeliveryRig) markRelease() {
	r.t.Helper()
	marked, err := r.st.ReleaseIdleIssue(r.ctx, "legion", r.issue.Key, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		return idleReleaseRows(view, workflow.IdleDecision{Release: true, Roles: []claim.Role{claim.RoleTester}}, time.Now().UTC())
	})
	if err != nil || !marked {
		r.t.Fatalf("mark the release of the tester's pod = %t, %v; want it marked", marked, err)
	}
}

// releaseMarked is whether a release of the tester's pod is still in progress.
func (r *waitDeliveryRig) releaseMarked() bool {
	r.t.Helper()
	var marked bool
	if err := r.pool.QueryRow(r.ctx, `select exists (select 1 from issue_releases where issue = $1)`, r.issue.Key).
		Scan(&marked); err != nil {
		r.t.Fatalf("read the release mark: %v", err)
	}
	return marked
}

// wantTimeCame asserts wait ended because its time came, ended by its wait_due row's fact and
// logged once, and that the time was logged once naming the claim, the issue, the role and the
// wait, with wake when given.
func (r *waitDeliveryRig) wantTimeCame(wait record.ClaimWait, more ...string) {
	r.t.Helper()
	r.wantEnded(wait, workflow.WaitEndTimeCame, "outbox/wait-due:"+wait.ID)
	fragments := append([]string{`msg="wait: its time came"`, "claim=" + string(r.token), "issue=" + r.issue.Key,
		"role=tester", "wait=" + wait.ID}, more...)
	if lines := r.lines(fragments...); len(lines) != 1 {
		r.t.Fatalf("the time of wait %s was logged %d times with %q, want once; the log is:\n%s", wait.ID, len(lines), more, r.logs)
	}
}

// wantWaitTask asserts sent is the task of wait's time: it gives the wait's id and its text, says
// that its time came, and says what to do now.
func wantWaitTask(t *testing.T, sent fake.Prompt, wait record.ClaimWait) {
	t.Helper()
	for _, words := range []string{"The time of your wait " + wait.ID + " came.", wait.WaitingOn,
		"Continue your phase", "declare a new wait"} {
		if !strings.Contains(sent.Message, words) {
			t.Fatalf("the tester was sent %q, want the task of wait %s's time, holding %q", sent.Message, wait.ID, words)
		}
	}
}

// At its time the daemon writes the start whose task gives the wait's text, and the task is
// delivered, under both runtimes (R3: "the wait's time fires under both runtimes"). The tester
// takes its task, declares a wait in that turn and ends the turn. The wait's row is not due a
// second before the wait's time and is due at it; the outbox runs it, which ends the wait because
// its time came and writes the wait's own start, a start with no phase and no entry that names
// the wait; the outbox runs that start, and the tester, which runs, is sent its task.
//
// The two configurations are an idle release, and the `runtime: tmux` shape with an idle period
// of 0, under which the release's own timers never run (tickWakes returns at once): the wait's
// time is an outbox row, which the lanes run under either.
func TestAtItsTimeTheWaitsOwnTaskIsDeliveredWithOrWithoutAnIdleRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*waitDeliveryRig)
	}{
		{name: "a configuration with an idle release", configure: func(r *waitDeliveryRig) { r.idleRelease() }},
		{name: "the runtime: tmux shape, idle period 0", configure: func(r *waitDeliveryRig) {
			ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
			defer cancel()
			r.w.tickWakes(ctx)
			if ctx.Err() != nil {
				r.t.Fatal("the release's timers ran under an idle period of 0, want tickWakes to return at once")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitDeliveryRig(t)
			tc.configure(r)
			taken := r.working(1, "Test it.")
			wait := r.declare()
			r.endTurn(taken.ID)
			r.wantPrompts(1, "the task the turn took is the one prompt before the wait's time")

			if due := r.dueAt(wait.WakeAt.Add(-time.Second), record.OutboxKindWaitDue); len(due) != 0 {
				t.Fatalf("rows %+v are due a second before the wait's time, want none", due)
			}
			if due := r.dueAt(wait.WakeAt, record.OutboxKindWaitDue); len(due) != 1 {
				t.Fatalf("at the wait's time %d wait_due rows are due, want its one", len(due))
			}
			ran := r.drainAt(wait.WakeAt, waitTimeKinds...)
			if len(ran) != 2 || ran[0].Kind != record.OutboxKindWaitDue || ran[1].Kind != record.OutboxKindSupervise {
				t.Fatalf("the outbox ran %+v at the wait's time, want the wait's row and then its own start", ran)
			}
			if start := superviseOf(t, ran[1]); start.Op != "start" || start.Phase != "" || start.Entry != 0 || start.WaitID != wait.ID {
				t.Fatalf("the wait's own start = %+v, want a start with no phase and no entry naming %s", start, wait.ID)
			}

			r.wantTimeCame(wait, fmt.Sprintf("row=%d", ran[1].ID), "wake=false")
			sent := r.wantPrompts(2, "the wait's own task is delivered at its time")[1]
			wantWaitTask(t, sent, wait)
			if held := r.machine.Claim().Pending; held == nil || held.ID != sent.DeliveryID ||
				held.ID != fmt.Sprintf("%s%d", outboxDeliveryPrefix, ran[1].ID) {
				t.Fatalf("the tester holds %+v after the send %+v, want the delivery of start row %d", held, sent, ran[1].ID)
			}
			if left := r.dueAt(wait.WakeAt.Add(time.Hour), waitTimeKinds...); len(left) != 0 {
				t.Fatalf("rows %+v are left after the wait's time, want none", left)
			}
		})
	}
}

// The time of a wait whose pod an idle release took wakes the pod (pods.WakeWaitTime): the
// release suspends the waiting tester as released and leaves its wait standing; at the wait's
// time the outbox runs the wait's row, which ends the wait, records one resume of the tester under
// the wake's kind and writes the wake's start; the start brings the tester back, and once its
// relaunched process is ready the tester is sent the wake's task, which says the pod was released
// and gives the wait's own task.
func TestTheTimeOfAReleasedWorkersWaitBringsItsPodBack(t *testing.T) {
	r := newWaitDeliveryRig(t)
	r.idleRelease()
	taken := r.working(1, "Test it.")
	wait := r.declare()
	r.endTurn(taken.ID)
	r.handle(supervise.RequestSuspend{Claim: r.token, Reason: "the idle release stopped the role", By: supervise.SuspendedByReleased})
	if got := r.machine.Claim(); !got.Released() {
		t.Fatalf("the tester = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	r.wantStanding()

	ran := r.drainAt(wait.WakeAt, waitTimeKinds...)
	if len(ran) != 2 || ran[0].Kind != record.OutboxKindWaitDue || ran[1].Kind != record.OutboxKindSupervise {
		t.Fatalf("the outbox ran %+v at the wait's time, want the wait's row and then the wake's start", ran)
	}
	if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "tester:"+pods.WakeWaitTime {
		t.Fatalf("the pod resumes = %v, want the one wake of the tester for its wait's time", resumes)
	}
	r.wantTimeCame(wait, fmt.Sprintf("row=%d", ran[1].ID), "wake=true")
	if got := r.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the tester = %s by %s after the wake's start, want it coming back", got.State, got.SuspendedBy)
	}
	r.wantPrompts(1, "nothing is sent before the relaunched process is ready")

	r.boot()
	sent := r.wantPrompts(2, "the wake's task is sent once the pod is back")[1]
	if !strings.Contains(sent.Message, "released") {
		t.Fatalf("the woken tester was sent %q, want the wake's word that its pod had been released", sent.Message)
	}
	wantWaitTask(t, sent, wait)
}

// The time of a replaced wait sends nothing (R3). The tester declares a wait and, in the same turn,
// a second one, which replaces the first. At the first wait's time the outbox runs its row, which
// writes no start: the first wait is over. The second wait stands, its own row still queued for its
// time, and the tester is sent nothing.
func TestTheTimeOfAReplacedWaitSendsTheWorkerNothing(t *testing.T) {
	r := newWaitDeliveryRig(t)
	taken := r.working(1, "Test it.")
	first := r.declareUntil("the review of the head", time.Now().Add(2*time.Hour))
	second := r.declareUntil("the second review of the head", time.Now().Add(3*time.Hour))
	r.endTurn(taken.ID)

	ran := r.drainAt(first.WakeAt, waitTimeKinds...)
	if len(ran) != 1 || ran[0].Kind != record.OutboxKindWaitDue {
		t.Fatalf("the outbox ran %+v at the first wait's time, want its row alone", ran)
	}
	if starts := entryStartRows(t, r.pool, r.issue.Key); len(starts) != 0 {
		t.Fatalf("the time of the replaced wait wrote the starts %+v, want none", starts)
	}
	r.wantPrompts(1, "the time of a replaced wait sends nothing")
	if standing := r.wantStanding(); standing.ID != second.ID {
		t.Fatalf("the standing wait is %s, want the second, %s", standing.ID, second.ID)
	}
	if due := r.dueAt(second.WakeAt, record.OutboxKindWaitDue); len(due) != 1 {
		t.Fatalf("at the second wait's time %d wait_due rows are due, want its own", len(due))
	}
	if lines := r.lines("wait: its time came to a wait that is over; nothing is sent", "wait="+first.ID,
		"standing="+second.ID); len(lines) != 1 {
		t.Fatalf("the time of the replaced wait was logged %d times, want once; the log is:\n%s", len(lines), r.logs)
	}
}

// heldAtItsTime brings the tester to the moment D7 orders: it declares a wait in the first turn of
// its task of entry 1, its process dies in that turn, and the relaunched agent holds that task,
// taken back unconfirmed and interrupted, while the wait stands. Then the wait's time comes: the
// outbox runs the wait's row, which ends the wait and writes the wait's own start, and then that
// start, which reaches the claim's machine (Machine.queue). The claim holds the task it took back,
// so the start's delivery sends that task first — no wait holds it any more — and is refused
// behind it (supervise.ErrDeliveryPending): the start waits on its backoff, its task not queued.
// It answers the wait, the held task and the start row.
func heldAtItsTime(t *testing.T, r *waitDeliveryRig) (record.ClaimWait, supervise.Delivery, record.OutboxRow) {
	t.Helper()
	r.working(1, "Test it.")
	wait := r.declare()
	r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
		Kind: runtime.Gone, At: time.Now()}})
	r.boot()
	held := r.machine.Claim().Pending
	if held == nil || !held.Interrupted || !held.ConfirmedAt.IsZero() || held.Entry != 1 {
		t.Fatalf("the relaunched tester holds %+v, want the task of entry 1 taken back unconfirmed and interrupted", held)
	}
	r.wantPrompts(1, "the held task goes to no process while the wait stands")

	ran := r.drainAt(wait.WakeAt, waitTimeKinds...)
	if len(ran) != 2 || ran[0].Kind != record.OutboxKindWaitDue || ran[1].Kind != record.OutboxKindSupervise {
		t.Fatalf("the outbox ran %+v at the wait's time, want the wait's row and then its own start", ran)
	}
	r.wantTimeCame(wait, fmt.Sprintf("row=%d", ran[1].ID))
	resent := r.wantPrompts(2, "the held task goes first")[1]
	if resent.DeliveryID != held.ID || !strings.HasSuffix(resent.Message, "Test it.") || resent.Message == "Test it." {
		t.Fatalf("at the wait's time the tester was sent %+v, want the held task %s behind the note that its turn was interrupted", resent, held.ID)
	}
	start, queued := r.outboxRow(ran[1].ID)
	if !queued || start.Attempts < 1 || !strings.Contains(start.LastError, supervise.ErrDeliveryPending.Error()) ||
		!start.NextAt.After(wait.WakeAt) {
		t.Fatalf("the wait's own start after its first run = %+v (queued %t), want it waiting on its backoff behind the held task", start, queued)
	}
	if request := superviseOf(t, start); request.WaitID != wait.ID {
		t.Fatalf("the waiting start = %+v, want the one naming %s", request, wait.ID)
	}
	return wait, *r.machine.Claim().Pending, start
}

// A held task after a death goes first, and the wait's own task after it (D7). Past heldAtItsTime,
// the tester takes the held task in a turn; the start, run again on its backoff, queues the wait's
// task behind that turn and finishes, and sends nothing into it. Once the turn is over, the tester
// is sent the wait's own task.
func TestAHeldTaskGoesFirstAndTheWaitsOwnTaskAfterIt(t *testing.T) {
	r := newWaitDeliveryRig(t)
	wait, held, start := heldAtItsTime(t, r)

	working := r.takeTask()
	if working.ID != held.ID {
		t.Fatalf("the turn took %s, want the held task %s", working.ID, held.ID)
	}
	ran := r.drainAt(start.NextAt, waitTimeKinds...)
	if len(ran) != 1 || ran[0].ID != start.ID {
		t.Fatalf("the outbox ran %+v on the start's backoff, want the wait's own start", ran)
	}
	if _, queued := r.outboxRow(start.ID); queued {
		t.Fatalf("the wait's own start is still queued once the held task's turn took it, want it finished")
	}
	r.wantPrompts(2, "the wait's own task goes into no turn of the held task")

	r.endTurn(held.ID)
	sent := r.wantPrompts(3, "the wait's own task follows the held task's turn")[2]
	wantWaitTask(t, sent, wait)
	if delivered := r.machine.Claim().Pending; delivered == nil || delivered.ID != fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.ID) {
		t.Fatalf("the tester holds %+v, want the delivery of the wait's own start %d", delivered, start.ID)
	}
}

// A second declaration in the held task's turn makes the old wait's start finish with no delivery
// (D5). Past heldAtItsTime, the tester takes the held task in a turn and declares a new wait in it.
// The old wait's start, run again on its backoff, finds the newer wait standing: it finishes, it
// queues nothing, and it is logged. The turn ends, the tester is sent nothing more, and the newer
// wait still stands, since no task of the old wait's time reached the claim as new work.
func TestASecondWaitInTheHeldTasksTurnFinishesTheOldWaitsStartWithNoDelivery(t *testing.T) {
	r := newWaitDeliveryRig(t)
	wait, held, start := heldAtItsTime(t, r)

	r.takeTask()
	second := r.declareUntil("the second review of the head", time.Now().Add(3*time.Hour))
	ran := r.drainAt(start.NextAt, waitTimeKinds...)
	if len(ran) != 1 || ran[0].ID != start.ID {
		t.Fatalf("the outbox ran %+v on the start's backoff, want the old wait's start", ran)
	}
	if _, queued := r.outboxRow(start.ID); queued {
		t.Fatalf("the old wait's start is still queued, want it finished")
	}
	if lines := r.lines("outbox start of a wait's time finished without acting: a newer wait of the claim stands",
		fmt.Sprintf("row=%d", start.ID), "claim="+string(r.token), "wait="+wait.ID, "standing="+second.ID); len(lines) != 1 {
		t.Fatalf("the old wait's start was logged %d times as finished for the newer wait, want once; the log is:\n%s", len(lines), r.logs)
	}

	r.endTurn(held.ID)
	r.wantPrompts(2, "the old wait's task reaches no one once a newer wait stands")
	if standing := r.wantStanding(); standing.ID != second.ID {
		t.Fatalf("the standing wait is %s, want the newer one, %s", standing.ID, second.ID)
	}
	if lines := r.lines(`msg="wait: ended"`, "wait="+second.ID); len(lines) != 0 {
		t.Fatalf("the newer wait was ended: %q", lines)
	}
	if got := r.machine.Claim(); got.Pending != nil {
		t.Fatalf("the tester holds %+v after the turn, want nothing", got.Pending)
	}
}

// A wait whose time comes while its pod's release is still in progress has its task reach the
// worker once the pod is back. An idle release of the waiting tester's pod is marked, its rows
// queued; then the wait's time comes.
//
//   - Before the release has stopped the tester: no release has taken the claim, so its time is a
//     start that names the wait, not a wake. That start stands above the release's own rows, so
//     the release's boundary cancels the release (store's IssueReleaseStop): the pod is never
//     taken down, and the start delivers the wait's own task to the tester in it.
//   - After the release has stopped the tester and before its teardown: the claim is released, so
//     its time wakes it as any wake of a released claim does. The resume that wake records is newer
//     than the release's mark, so the teardown cancels the release (store's IssueRelease), the
//     pod is never taken down, and the wake's start brings the tester back in it with the wake's
//     task.
func TestAWaitsTimeDuringItsPodsReleaseReachesTheWorkerOnceThePodIsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stopped runs the release's save and its stop of the tester before the wait's time.
		stopped bool
	}{
		{name: "before the release stopped the tester"},
		{name: "after the release stopped the tester, before its teardown", stopped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitDeliveryRig(t)
			rt := r.idleRelease()
			taken := r.working(1, "Test it.")
			wait := r.declare()
			r.endTurn(taken.ID)
			r.markRelease()
			if tc.stopped {
				r.drainAt(time.Now(), record.OutboxKindIssueBundle, record.OutboxKindSupervise)
				if got := r.machine.Claim(); !got.Released() {
					t.Fatalf("the tester = %s by %s after the release's stop, want it released", got.State, got.SuspendedBy)
				}
				if !r.releaseMarked() {
					t.Fatal("the release ended before its teardown, want it still in progress")
				}
			}

			// The wait's time comes now, with the release in progress.
			due := r.dueAt(wait.WakeAt, record.OutboxKindWaitDue)
			if len(due) != 1 {
				t.Fatalf("at the wait's time %d wait_due rows are due, want its one", len(due))
			}
			r.runner.now = func() time.Time { return wait.WakeAt }
			if err := r.runner.runRow(r.ctx, due[0]); err != nil {
				t.Fatalf("run the wait's row: %v", err)
			}
			r.settled()
			r.wantTimeCame(wait, fmt.Sprintf("wake=%t", tc.stopped))
			starts := entryStartRows(t, r.pool, r.issue.Key)
			if len(starts) != 1 || starts[0].request.Phase != "" || starts[0].request.Entry != 0 {
				t.Fatalf("the wait's time wrote the starts %+v, want its one start with no phase and no entry", starts)
			}
			if tc.stopped {
				if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "tester:"+pods.WakeWaitTime {
					t.Fatalf("the pod resumes = %v, want the one wake of the released tester for its wait's time", resumes)
				}
			} else {
				if starts[0].request.WaitID != wait.ID {
					t.Fatalf("the wait's own start = %+v, want the start that names %s", starts[0].request, wait.ID)
				}
				if resumes := r.resumes(); len(resumes) != 0 {
					t.Fatalf("the pod resumes = %v, want none: no release has taken the claim, so nothing wakes it", resumes)
				}
			}

			r.drainAt(wait.WakeAt, record.OutboxKindIssueBundle, record.OutboxKindSupervise, record.OutboxKindIssueRelease)
			if r.releaseMarked() {
				t.Fatal("the release is still in progress after its rows ran, want it cancelled")
			}
			if _, suspended := rt.reads(); suspended != 0 {
				t.Fatalf("the runtime took the pod down %d times, want never: the wait's time cancelled the release", suspended)
			}
			if tc.stopped {
				r.wantPrompts(1, "nothing is sent before the tester's process is back")
				r.boot()
			}
			sent := r.wantPrompts(2, "the wait's own task reaches the tester once its pod is back")[1]
			wantWaitTask(t, sent, wait)
			if got := r.machine.Claim(); got.State == supervise.StateSuspended {
				t.Fatalf("the tester = %s by %s, want it back", got.State, got.SuspendedBy)
			}
		})
	}
}
