package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// newLeftPodRig is the idle rig (idleRig) over a child of a running tree rather than over a root:
// the root LEGION-208 stands in the workflow at implementing, and its child LEGION-209 holds one
// claim, its planner's, the role that worked the child's first phase. With inBacklog false the
// child stands at implementing and its planner is idle in the child's pod, holding a process, as a
// planner stays once it has handed its phase on. With inBacklog true the child stands where a move
// to backlog leaves it, at phase done with the status backlog, and its planner is suspended by the
// daemon with no process, as the move's own stop left it.
//
// Either way the pod has been quiet for half an hour, its session reported no background work
// after that, the Envoy listener reports itself ready and the runtime holds the issue's Sandbox,
// so nothing outside the issue's own standing keeps the pod. The rig's issue and token are the
// child's and its planner's, which is what idleRig's own readers (drain, queuedBy, rows, records)
// read.
func newLeftPodRig(t *testing.T, inBacklog bool) *idleRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	parent := root.Key
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "V", LastDispatchSeq: 1}
	if inBacklog {
		child.Phase, child.Status, child.DispatchStatus = phase.Done, "backlog", "backlog"
	}
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	st := outboxTreeStore(t, pool, root.Key)

	rt := &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = rt, st, api.DaemonAPIVersion()
	// The planner's machine reads its clock from here, so a test can stamp a turn at a moment of its
	// own (turnedBeforeQuiet). The clock is the wall clock until a test sets one.
	sup.deps.Clock = &heldClock{}
	sup.store = st

	token := mustClaimToken(t, child.Key, claim.RolePlanner)
	// The roles of an issue share one Sandbox, named as the Kubernetes runtime names it from a
	// role claim of the issue (sandbox.SandboxName).
	rt.sandboxName = sandbox.SandboxName(token)
	stored := supervise.Claim{Token: token, Project: "legion", Tree: root.Key, Issue: child.Key,
		Role: claim.RolePlanner, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Session: "ses-planner", SessionFile: "/tmp/planner.jsonl",
		PluginContract: api.DaemonAPIVersion()}
	if inBacklog {
		stored.State, stored.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByDaemon
	} else {
		locator, err := rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: claim.RolePlanner})
		if err != nil {
			t.Fatalf("mint a locator: %v", err)
		}
		stored.Locator = &locator
	}
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("seed the child's planner: %v", err)
	}
	if _, err := sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the child's planner: %v", err)
	}
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	// The idle rule decides no pod unless the Envoy listener has reported itself ready inside its
	// freshness window (workflowRuntime.listenerLive); a rig stands for a deployment whose
	// listener reports.
	if _, err := pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, "legion", time.Now().UTC()); err != nil {
		t.Fatalf("seed the listener's report: %v", err)
	}
	runner := &outbox{
		pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		notices: &outboxPublisher{}, dispatch: &roleHoldersDispatch{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
	w := &workflowRuntime{pool: pool, projectID: "legion", dispatchProject: "LEGION", log: quietLogger(),
		idleStore: st, idleRuntime: rt, idlePeriod: 10 * time.Minute}
	rig := &idleRig{pool: pool, st: st, issues: records, runner: runner, sup: sup, rt: rt, w: w,
		issue: child, token: token, machine: machine}
	quietPod(t, rig)
	return rig
}

// quietPod leaves the rig's pod quiet for half an hour: its claims last written then, and their
// sessions' last report, read now, naming no background work and no reminder.
// claims.updated_at is what the idle period is measured from (pods.IdleView.QuietSince). The
// report is read after the claim's last turn, as a session's report after a turn is: a claim
// whose report predates its last turn is held as unreported (claim.ReadSessionWork), and a
// relaunched claim that holds no task is sent, at its ready, the daemon's own delivery of what
// the relaunch ended (supervise's queueRelaunch), whose send is such a turn.
func quietPod(t *testing.T, rig *idleRig) {
	t.Helper()
	if _, err := rig.pool.Exec(context.Background(), `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now(), activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where issue = $1`, rig.issue.Key); err != nil {
		t.Fatalf("leave the pod quiet: %v", err)
	}
}

// moveChildToBacklog moves the rig's child to backlog as the workflow takes a child moved to
// backlog out of it (the workflow's leave): the record stands at phase done with the status
// backlog, and the outbox holds the move's stop — a `suspend` of every role a child can hold, at
// the child's generation, and the issue_suspend that deletes the child's Sandbox once every role
// has stopped. The departure's two other rows, the release of the issue's Dispatch claim and the
// notice to the architect above, reach no role of this pod, and are left out: the stop is what
// this file reads.
func moveChildToBacklog(t *testing.T, rig *idleRig) {
	t.Helper()
	ctx := context.Background()
	moved := rig.issue
	moved.Phase, moved.Status, moved.DispatchStatus = phase.Done, "backlog", "backlog"
	payloads := make([]record.OutboxPayload, 0, len(claim.Roles)+1)
	for _, role := range claim.Roles {
		payloads = append(payloads, record.SuperviseRequest{Op: "suspend", Tree: moved.Tree, Role: role,
			Generation: moved.Generation, Reason: moved.Key + " is backlog"})
	}
	payloads = append(payloads, record.IssueSuspend{Tree: moved.Tree, Generation: moved.Generation, TreeGeneration: 1})
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.issues.PutIssue(ctx, tx, moved); err != nil {
			return err
		}
		for _, payload := range payloads {
			if err := rig.issues.Enqueue(ctx, tx, mustOutboxRow(t, moved.Key, payload, time.Now())); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("move %s to backlog: %v", moved.Key, err)
	}
	rig.issue = moved
}

// resumeAsOperator is an operator's resume of the rig's stopped planner: the event the operator's
// resume route posts (the api's resumeEvent) and the controller's fresh session posts after its
// own suspension (the api's freshSession), then the launch's registration on the daemon's plugin
// contract and its ready. Neither route reads the standing of the claim's issue, so the planner of
// a child the workflow has taken out comes back, idle, in a pod built again for it.
func resumeAsOperator(t *testing.T, rig *idleRig) {
	t.Helper()
	ctx := context.Background()
	rig.sup.deps.Conns.(*fake.Conns).Register(rig.token, fake.NewConn())
	if err := rig.machine.Handle(ctx, supervise.RequestResume{Claim: rig.token}); err != nil {
		t.Fatalf("the operator's resume of %s: %v", rig.token, err)
	}
	rig.machine.Wait()
	generation := rig.machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: rig.token, Generation: generation},
		supervise.RequestRegister{Claim: rig.token, Generation: generation, Session: "ses-planner",
			SessionFile: "/tmp/planner.jsonl", PluginContract: api.DaemonAPIVersion()},
		supervise.RequestReady{Claim: rig.token, Generation: generation, Session: "ses-planner"},
	} {
		if err := rig.machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, rig.token, err)
		}
	}
	rig.machine.Wait()
	if got := rig.machine.Claim(); got.State != supervise.StateReady && got.State != supervise.StateIdle {
		t.Fatalf("the planner after the operator's resume = %s, want it ready in a pod of its own", got.State)
	}
	// The resume's launch built the issue's pod again, so the runtime holds a Sandbox for it.
	rig.rt.mu.Lock()
	rig.rt.sandboxGone = false
	rig.rt.mu.Unlock()
	quietPod(t, rig)
}

// leftPodGone is the runtime's informer after a teardown took the issue's pod: it holds no
// Sandbox of the issue any more (releaseRuntime.IssueSandbox).
func leftPodGone(rig *idleRig) {
	rig.rt.mu.Lock()
	defer rig.rt.mu.Unlock()
	rig.rt.sandboxGone = true
}

// idlePasses runs the tick's two passes once, in the tick's order (tickIdle): the pass over the
// pods no claim owns, then the idle release.
func idlePasses(rig *idleRig) {
	ctx := context.Background()
	rig.w.releaseUnownedPods(ctx)
	rig.w.releaseIdlePods(ctx)
}

// holdsOf is every hold record of the rig's project under reason.
func holdsOf(t *testing.T, rig *idleRig, reason workflow.IdleHold) []store.PodHold {
	t.Helper()
	var held []store.PodHold
	for _, hold := range rig.records(t).Holds {
		if hold.Reason == reason.String() {
			held = append(held, hold)
		}
	}
	return held
}

// queuedLeftPodCheck brings the rig's child to the close of a left pod with a live role, as
// TestALeftPodWithALiveRoleIsReadBeforeItsClose does: the child moved to backlog and its stop
// run, its pod built again and its planner brought back by an operator, and the idle passes queue
// the close's workspace check alone. It answers that check as the outbox lists it.
func queuedLeftPodCheck(t *testing.T, rig *idleRig) (record.OutboxRow, record.IssueWorkspaceCheck) {
	t.Helper()
	moveChildToBacklog(t, rig)
	rig.drain(t)
	leftPodGone(rig)
	resumeAsOperator(t, rig)
	if queued := rig.queuedBy(t, func() { idlePasses(rig) }); !slices.Equal(queued, []string{"issue_bundle"}) {
		t.Fatalf("the passes queued %v, want the close's workspace check alone", queued)
	}
	var row record.OutboxRow
	var kind string
	var payload []byte
	if err := rig.pool.QueryRow(context.Background(), `select id, kind, issue, payload, lease_token from outbox where issue = $1 and kind = $2`,
		rig.issue.Key, string(record.OutboxKindIssueBundle)).Scan(&row.ID, &kind, &row.Issue, &payload, &row.LeaseToken); err != nil {
		t.Fatalf("read the close's workspace check: %v", err)
	}
	row.Kind, row.Payload = record.OutboxKind(kind), payload
	decoded, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the close's workspace check: %v", err)
	}
	check, ok := decoded.(record.IssueWorkspaceCheck)
	if !ok || len(check.CloseRoles) == 0 {
		t.Fatalf("the queued check = %#v, want a close's workspace check", decoded)
	}
	return row, check
}

// A child moved out of the workflow to backlog, icebox or triage while its tree runs on had its
// one close at that move: the move suspended its claims and deleted its Sandbox, and nothing of
// its running tree closes it again. A role that runs there after that move runs in a pod built
// again for it — an operator's resume, an operator's task to the suspended claim, an operator's
// retry and the controller's fresh session each start a claim without reading its issue's
// standing — and once its turn ends it stands idle. A deployment's child in backlog stood so:
// phase done, status backlog, its one claim, the planner, idle, and its pod running, every idle
// pass holding that pod as tree_not_live, a close's to stop, with no close left to stop it.
//
// The test moves a finished child whose planner is idle in its running pod to backlog, runs the
// move's stop, and reads what it did: the planner suspended and the Sandbox deleted. The
// operator's resume then brings the planner back, idle, into a pod built again, which is the
// state that deployment's child stood in. The idle passes then run the move's stop again
// themselves (workflow.LeftPodClose): the planner reads suspended, by the daemon and with its
// session kept, the Sandbox goes a second time, and no tree_not_live hold is recorded, after that
// stop or before it.
func TestABacklogIssuesIdleClaimIsStoppedSoItsPodIsNotHeldTreeNotLive(t *testing.T) {
	t.Parallel()
	rig := newLeftPodRig(t, false)

	moveChildToBacklog(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the planner after the move's stop = %s by %s, want it suspended by the daemon", got.State, got.SuspendedBy)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue"}) {
		t.Fatalf("teardowns asked by the move = %v, want the move's own Sandbox suspension", asked)
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns the store authorized after the move = %d, want the move's one", suspended)
	}
	leftPodGone(rig)

	resumeAsOperator(t, rig)
	if left := rig.rows(t); len(left) != 0 {
		t.Fatalf("rows of the child in backlog = %v, want none: the move's stop has finished and nothing else is queued", left)
	}
	if record.InWorkflow(rig.issue) || rig.issue.Phase != phase.Done || rig.issue.Status != "backlog" {
		t.Fatalf("the child = %s at %s, want it in backlog: phase done, status backlog", rig.issue.Phase, rig.issue.Status)
	}

	idlePasses(rig)
	rig.drain(t)
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the planner of the child in backlog after the idle passes = %s by %s, want it suspended by the daemon; tree_not_live holds recorded: %+v",
			got.State, got.SuspendedBy, holdsOf(t, rig, workflow.IdleHoldTreeNotLive))
	}
	if got := rig.storedClaim(t, rig.token); got.State != supervise.StateSuspended || got.Session != "ses-planner" || got.Locator != nil {
		t.Fatalf("the stored planner = %s, session %q, locator %+v; want it suspended with its session kept and no process",
			got.State, got.Session, got.Locator)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue", "SuspendIssue"}) {
		t.Fatalf("teardowns asked = %v, want the move's and then the close the idle pass finished", asked)
	}
	if _, suspended := rig.rt.reads(); suspended != 2 {
		t.Fatalf("Sandbox teardowns the store authorized = %d, want the move's and the idle pass's close", suspended)
	}
	if left := rig.rows(t); len(left) != 0 {
		t.Fatalf("rows left after the idle pass's close = %v, want none", left)
	}
	if rig.marked(t) {
		t.Fatal("the close left a release mark: a close is no release")
	}

	leftPodGone(rig)
	for range 3 {
		idlePasses(rig)
		rig.drain(t)
	}
	if held := holdsOf(t, rig, workflow.IdleHoldTreeNotLive); len(held) != 0 {
		t.Fatalf("tree_not_live holds recorded = %+v, want none: the pod's close has run, and nothing is left of the pod to hold", held)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended {
		t.Fatalf("the planner after the later passes = %s, want it left suspended", got.State)
	}
}

// A pod can stand after its issue left the workflow with no claim left to stop: the issue's
// close stopped every claim and its Sandbox suspension never took the pod, or an operator's
// start built the pod again after that close and the role it started has stopped since, by its
// own exit or a failure. The release of a pod no claim owns decides only issues still in the
// workflow (store.UnownedPods), and its rule would hold this one as unowned_tree_not_live; the idle
// rule decides only pods with a live claim (store.IdlePods). So such a pod stood, with nobody in
// it, for as long as its issue did.
//
// The test stages that pod — a child in backlog under a running tree, its one claim suspended by
// the daemon and recording no process, the runtime still holding its Sandbox, quiet for half an
// hour — and reads that a pass takes it: the passes queue the close's Sandbox suspension alone,
// which deletes the Sandbox, the pod and the disk, and leave the claim as it was.
func TestAPodHeldTreeNotLiveIsTakenOnceItsCloseHasNothingLeftToStop(t *testing.T) {
	t.Parallel()
	rig := newLeftPodRig(t, true)
	if record.InWorkflow(rig.issue) {
		t.Fatalf("the staged child = %s at %s, want it out of the workflow", rig.issue.Phase, rig.issue.Status)
	}
	if _, present := rig.rt.IssueSandbox(rig.issue.Key); !present {
		t.Fatal("the runtime holds no Sandbox of the staged child, want its pod standing")
	}

	queued := rig.queuedBy(t, func() { idlePasses(rig) })
	if !slices.Equal(queued, []string{"issue_suspend"}) {
		t.Fatalf("the passes queued %v, want the close's Sandbox suspension alone: no claim is left to stop, and nothing else takes the pod", queued)
	}
	rig.drain(t)

	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue"}) {
		t.Fatalf("teardowns asked = %v, want the close's Sandbox suspension, which deletes the Sandbox of an issue out of the workflow", asked)
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns the store authorized = %d, want the one that takes the pod", suspended)
	}
	if got := rig.storedClaim(t, rig.token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon || got.Session != "ses-planner" {
		t.Fatalf("the planner = %s by %s with session %q, want it left as its close left it", got.State, got.SuspendedBy, got.Session)
	}
	if left := rig.rows(t); len(left) != 0 {
		t.Fatalf("rows left after the close = %v, want none", left)
	}
	if rig.marked(t) {
		t.Fatal("the close left a release mark: a close is no release")
	}

	// The pod is gone, and the passes after it decide nothing more of the issue.
	leftPodGone(rig)
	if queued := rig.queuedBy(t, func() { idlePasses(rig) }); len(queued) != 0 {
		t.Fatalf("the passes after the close queued %v, want nothing: the runtime holds no pod of the issue", queued)
	}
}

// The close of a left pod with a live role deletes a disk that role worked on, so it reads that
// disk first, as the idle release does (workflow.LeftPodClose). The test stands the child in
// backlog whose planner an operator's resume brought back, live and idle, in a pod built again,
// with one commit origin does not hold in its workspace. The passes queue the close's workspace
// check alone, which reads the workspace through the planner, finds the commit and declines: the
// planner stays live in its pod, no stop and no Sandbox suspension is queued or run, the pod is
// held as unpushed_work, and the planner is told once which commit keeps it. The passes after the
// decline ask nothing more.
func TestALeftPodWithALiveRoleIsReadBeforeItsClose(t *testing.T) {
	t.Parallel()
	rig := newLeftPodRig(t, false)
	moveChildToBacklog(t, rig)
	rig.drain(t)
	leftPodGone(rig)
	resumeAsOperator(t, rig)
	publisher := &outboxPublisher{}
	rig.runner.notices = publisher
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	moveTeardowns := rig.rt.teardownsAsked()

	if queued := rig.queuedBy(t, func() { idlePasses(rig) }); !slices.Equal(queued, []string{"issue_bundle"}) {
		t.Fatalf("the passes queued %v, want the close's workspace check alone: a live role's disk is read before anything stops it", queued)
	}
	rig.drain(t)

	if read, suspended := rig.rt.reads(); !slices.Equal(read, []claim.Role{claim.RolePlanner}) || suspended != 1 {
		t.Fatalf("the workspace was read through %v and %d Sandbox teardowns were authorized, want one read through the planner and the move's teardown alone",
			read, suspended)
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the planner = %s by %s, want it live in the pod its work is in", got.State, got.SuspendedBy)
	}
	if got := rig.storedClaim(t, rig.token); got.State == supervise.StateSuspended || got.Locator == nil {
		t.Fatalf("the stored planner = %s with process %+v, want it live with the process it had", got.State, got.Locator)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, moveTeardowns) {
		t.Fatalf("teardowns asked = %v, want the move's %v alone", asked, moveTeardowns)
	}
	for _, left := range rig.rows(t) {
		if left == "issue_suspend" || left == "supervise:suspend" {
			t.Fatalf("rows left after the check = %v, want no stop and no Sandbox suspension queued", rig.rows(t))
		}
	}
	if rig.marked(t) {
		t.Fatal("the close's check left a release mark: a close is no release")
	}
	if !rig.declined(t) {
		t.Fatal("the check that found the commit recorded no decline")
	}
	if held := holdsOf(t, rig, workflow.IdleHoldUnpushedWork); len(held) != 1 {
		t.Fatalf("holds = %+v, want one %s", rig.records(t).Holds, workflow.IdleHoldUnpushedWork)
	}
	messages := roleMessages(publisher, string(rig.token))
	if len(messages) != 1 || !strings.Contains(messages[0], idleRigUnpushed.Commits[0]) {
		t.Fatalf("messages to %s = %q, want one naming the commit %s", rig.token, messages, idleRigUnpushed.Commits[0])
	}

	rows := rig.sequenceValue(t, "outbox")
	for range idleRigPasses {
		idlePasses(rig)
		rig.drain(t)
	}
	if read, _ := rig.rt.reads(); len(read) != 1 {
		t.Fatalf("the workspace was read %d times in all, want only the first check's read", len(read))
	}
	if after := rig.sequenceValue(t, "outbox"); after != rows {
		t.Fatalf("the passes after the decline queued outbox rows (the sequence moved from %d to %d), want none", rows, after)
	}
	if messages := roleMessages(publisher, string(rig.token)); len(messages) != 1 {
		t.Fatalf("messages to %s after the passes = %d, want the decline's one", rig.token, len(messages))
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the planner after the passes = %s, want it still live", got.State)
	}
}

// A close's check whose lease another lane holds now (this run's lapsed, and the row was claimed
// again) writes nothing of what its read found: what the check records commits with the finish of
// its own row, which fences on the lease (record's FinishLeasedOutbox), so the lane that holds the
// row now decides the close. A read that finds work records no decline, no hold and no message to
// the role; a read that finds nothing forgets no decline on record and queues no stop.
func TestALeftPodCloseCheckWhoseLeaseWasLostWritesNothing(t *testing.T) {
	t.Parallel()
	// run hands the check's lease to another lane and runs the check under the lease it had.
	run := func(t *testing.T, rig *idleRig, row record.OutboxRow, check record.IssueWorkspaceCheck) {
		t.Helper()
		row.LeaseToken = "this-run"
		if _, err := rig.pool.Exec(context.Background(), `update outbox set lease_token = 'another-lane',
			lease_until = now() + interval '5 minutes' where id = $1`, row.ID); err != nil {
			t.Fatalf("hand the check's lease to another lane: %v", err)
		}
		before := rig.rows(t)
		if err := rig.runner.checkIssueWorkspace(context.Background(), row, check); !errors.Is(err, record.ErrOutboxLeaseLost) {
			t.Errorf("the check whose lease was lost = %v, want %v", err, record.ErrOutboxLeaseLost)
		}
		if after := rig.rows(t); !slices.Equal(after, before) {
			t.Errorf("rows after the check = %v, want %v as before it", after, before)
		}
	}
	t.Run("a read that finds work", func(t *testing.T) {
		rig := newLeftPodRig(t, false)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
		row, check := queuedLeftPodCheck(t, rig)
		run(t, rig, row, check)
		if rig.declined(t) {
			t.Error("the check whose lease was lost recorded a decline")
		}
		if held := holdsOf(t, rig, workflow.IdleHoldUnpushedWork); len(held) != 0 {
			t.Errorf("holds = %+v, want no %s", held, workflow.IdleHoldUnpushedWork)
		}
	})
	t.Run("a read that finds nothing", func(t *testing.T) {
		rig := newLeftPodRig(t, false)
		row, check := queuedLeftPodCheck(t, rig)
		// A decline on record, which this clean read would forget if it held the lease.
		if _, err := rig.st.DeclineLeftPodClose(context.Background(), "legion", rig.issue.Key, store.IdleReleaseDecline{
			At: time.Now().UTC(), Role: claim.RolePlanner, Reason: pods.DeclineWorkspaceUnread}); err != nil {
			t.Fatalf("record a decline: %v", err)
		}
		run(t, rig, row, check)
		if !rig.declined(t) {
			t.Error("the check whose lease was lost forgot the decline on record")
		}
	})
}

// Two runs of a close's check overlap after a lease lapsed: this run, whose lease another lane
// took, reads nothing, while the lane that holds the row now writes the decline row under the
// lifecycle serializer, as every writer of the issue's lifecycle does (store.DeclineLeftPodClose).
// The check takes the serializer before it touches a row of the issue: one that deleted the decline
// row first would hold that row while waiting for the serializer the lane holds, and the lane would
// wait for that row, a deadlock PostgreSQL ends by failing one of the two (40P01). Here the lane's
// transaction takes the serializer, the run starts and waits on a lock, and the lane writes the
// decline row and commits; then the run goes on, finds its lease lost and writes nothing.
func TestTwoRunsOfALeftPodCloseCheckAfterALostLeaseDoNotDeadlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newLeftPodRig(t, false)
	row, check := queuedLeftPodCheck(t, rig)
	if _, err := rig.st.DeclineLeftPodClose(ctx, "legion", rig.issue.Key, store.IdleReleaseDecline{
		At: time.Now().UTC(), Role: claim.RolePlanner, Reason: pods.DeclineWorkspaceUnread}); err != nil {
		t.Fatalf("record the decline the clean read forgets: %v", err)
	}
	row.LeaseToken = "this-run"
	if _, err := rig.pool.Exec(ctx, `update outbox set lease_token = 'another-lane',
		lease_until = now() + interval '5 minutes' where id = $1`, row.ID); err != nil {
		t.Fatalf("hand the check's lease to another lane: %v", err)
	}

	lane, err := rig.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the lane's transaction: %v", err)
	}
	defer func() { _ = lane.Rollback(context.Background()) }()
	if err := treelifecycle.Serialize(ctx, lane); err != nil {
		t.Fatalf("take the lifecycle serializer: %v", err)
	}
	ran := make(chan error, 1)
	go func() { ran <- rig.runner.checkIssueWorkspace(context.Background(), row, check) }()
	awaitOneLockWaiter(t, rig.pool, "the run to wait on a lock the lane holds")
	if _, err := lane.Exec(ctx, `update idle_release_declines set checked_at = now() where project = $1 and issue = $2`,
		"legion", rig.issue.Key); err != nil {
		t.Errorf("the lane's write of the decline row = %v, want it written beside the waiting run", err)
	}
	if err := lane.Commit(ctx); err != nil {
		t.Errorf("the lane's commit = %v, want it committed", err)
	}
	select {
	case err := <-ran:
		if !errors.Is(err, record.ErrOutboxLeaseLost) {
			t.Errorf("the run whose lease was lost = %v, want %v", err, record.ErrOutboxLeaseLost)
		}
	case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
		t.Fatal("the run whose lease was lost did not end once the lane committed")
	}
	if !rig.declined(t) {
		t.Error("the run whose lease was lost forgot the decline the lane wrote")
	}
}

// turnedBeforeQuiet runs a turn of the rig's planner through its machine, started and ended forty
// minutes ago, before the pod went quiet half an hour ago (quietPod, run again after the turn's
// writes), and answers its start as the database holds it. The machine stamps the turn with its
// clock's nanoseconds, below the microsecond the database keeps (claims.last_turn_start), so the
// machine's own stamp of this one turn reads later than the database's: a rule that weighed the one
// against the other would take this old turn for a newer one. The pass carries the database's
// moment, which is not zero, into its close's check (record.IssueWorkspaceCheck.TurnStart), so a
// close that dropped that moment on its way would read this old turn as one since the pass and
// hold too.
func turnedBeforeQuiet(t *testing.T, rig *idleRig) time.Time {
	t.Helper()
	clock, ok := rig.sup.deps.Clock.(*heldClock)
	if !ok {
		t.Fatalf("the rig's supervisor clock is %T, want newLeftPodRig's heldClock", rig.sup.deps.Clock)
	}
	at := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Microsecond).Add(321 * time.Nanosecond)
	clock.mu.Lock()
	clock.at = at
	clock.mu.Unlock()
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: rig.token}, supervise.StreamTurnEnd{Claim: rig.token}} {
		if err := rig.machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, rig.token, err)
		}
	}
	rig.machine.Wait()
	clock.mu.Lock()
	clock.at = time.Time{}
	clock.mu.Unlock()
	if got := rig.machine.Claim().LastTurnStart; !got.Equal(at) {
		t.Fatalf("the planner's machine stamped its turn at %v, want %v", got, at)
	}
	stored := at.Truncate(time.Microsecond)
	var held time.Time
	if err := rig.pool.QueryRow(context.Background(), `select last_turn_start from claims where token = $1`,
		string(rig.token)).Scan(&held); err != nil || !held.Equal(stored) {
		t.Fatalf("the stored planner's last turn start = %v (%v), want %v, the machine's stamp to the microsecond", held, err, stored)
	}
	quietPod(t, rig)
	return stored
}

// rowTurnStart is the turnStart the rig's queued row of kind carries, with op the row's supervise op
// ("" for a row of another kind), and false where that row names none.
func rowTurnStart(t *testing.T, rig *idleRig, kind, op string) (time.Time, bool) {
	t.Helper()
	var raw *string
	if err := rig.pool.QueryRow(context.Background(), `select payload->>'turnStart' from outbox
		where issue = $1 and kind = $2 and coalesce(payload->>'op', '') = $3 order by id limit 1`,
		rig.issue.Key, kind, op).Scan(&raw); err != nil {
		t.Fatalf("read the turn start of the %s %s row: %v", kind, op, err)
	}
	if raw == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		t.Fatalf("the %s %s row's turn start %q: %v", kind, op, *raw, err)
	}
	return at, true
}

// checkedLeftPodClose stands the child in backlog whose planner an operator's resume brought back,
// live and idle in a pod built again, its last turn started before the pod went quiet
// (turnedBeforeQuiet), and runs one idle pass and the workspace check of the close that pass
// queues. The check carries that turn's moment, the pass's newest turn start, and reads a
// workspace that holds nothing origin does not hold; reading, where not nil, runs as the read
// begins, at readAt. The rows the check queues wait: the runner's clock stands at readAt, before
// they were queued. It answers the rig and the planner's turn start.
func checkedLeftPodClose(t *testing.T, reading func(rig *idleRig, readAt time.Time)) (*idleRig, time.Time) {
	t.Helper()
	rig := newLeftPodRig(t, false)
	moveChildToBacklog(t, rig)
	rig.drain(t)
	leftPodGone(rig)
	resumeAsOperator(t, rig)
	turnAt := turnedBeforeQuiet(t, rig)
	if queued := rig.queuedBy(t, func() { idlePasses(rig) }); !slices.Equal(queued, []string{"issue_bundle"}) {
		t.Fatalf("the passes queued %v, want the close's workspace check alone", queued)
	}
	if carried, ok := rowTurnStart(t, rig, "issue_bundle", ""); !ok || !carried.Equal(turnAt) {
		t.Fatalf("the close's check carries the turn start %v (named %t), want the planner's turn at %v: the pass's newest turn start",
			carried, ok, turnAt)
	}
	readAt := time.Now().UTC().Truncate(time.Microsecond)
	rig.runner.now = func() time.Time { return readAt }
	rig.sup.deps.Runtime = &readClockRuntime{releaseRuntime: rig.rt, reading: func() {
		if reading != nil {
			reading(rig, readAt)
		}
	}}
	rig.drain(t)
	if read, _ := rig.rt.reads(); !slices.Equal(read, []claim.Role{claim.RolePlanner}) {
		t.Fatalf("the workspace was read through %v, want one read through the planner", read)
	}
	if rig.declined(t) {
		t.Fatal("the clean read recorded a decline")
	}
	return rig, turnAt
}

// A close's check reads the workspace on the lifecycle lane while other lanes run, so a role can be
// given work after the pass queued the check: a message starts a turn before the read begins, the
// read snapshots while the turn runs, and the turn commits after that snapshot and ends before the
// check decides. That turn started before the read, so the read's moment does not show it; the
// pass's own newest turn start, carried in the check (record.IssueWorkspaceCheck.TurnStart, as
// issue_releases.turn_start is a release's), does. The close stops no role and suspends no Sandbox
// after a turn that started since the pass, and the next pass reads the workspace again.
//
// The planner's last turn started before the pod went quiet, so the pass's turn start is not zero
// and the check carries it through its row. The control runs the same check with no turn since
// the pass, and the close stops the planner: a check that lost the pass's turn start on the way
// would read the planner's old turn as one since the pass and hold.
func TestALeftPodCloseHoldsOnATurnThatEndedDuringItsRead(t *testing.T) {
	t.Parallel()
	t.Run("a_turn_that_ended_during_the_read", func(t *testing.T) {
		rig, _ := checkedLeftPodClose(t, func(rig *idleRig, readAt time.Time) {
			// The turn started a second before the read began, after the pass, and it has
			// ended: the claim stands idle again by the time the check decides.
			if _, err := rig.pool.Exec(context.Background(), `update claims set last_turn_start = $2 where issue = $1`,
				rig.issue.Key, readAt.Add(-time.Second)); err != nil {
				t.Errorf("record the turn: %v", err)
			}
		})
		if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
			t.Fatalf("the planner = %s by %s, want it live: a turn that started after the pass may have made work the read did not see",
				got.State, got.SuspendedBy)
		}
		for _, left := range rig.rows(t) {
			if left == "issue_suspend" || left == "supervise:suspend" {
				t.Fatalf("rows after the check = %v, want no stop and no Sandbox suspension queued", rig.rows(t))
			}
		}
	})

	t.Run("control:_no_turn_since_the_pass", func(t *testing.T) {
		rig, _ := checkedLeftPodClose(t, nil)
		if rows := rig.rows(t); !slices.Contains(rows, "supervise:suspend") || !slices.Contains(rows, "issue_suspend") {
			t.Fatalf("rows after the check = %v, want the close's stop and its Sandbox suspension queued after a clean read with no turn since the pass", rows)
		}
	})
}

// The check's transaction queues the close's stops and commits, and the lane runs them after it, so
// a role can be given work in between: a message starts a turn after the check decided and before
// the lane runs the role's `suspend`. Such a turn may make work the check's read did not see, and
// the suspend would end the turn and the Sandbox suspension after it would delete the disk that
// work is on. So each `suspend` the check queues carries the pass's newest turn start
// (record.SuperviseRequest.TurnStart), and the suspend of a claim that started a turn after it
// finishes without acting, as a release's role stop is cancelled by a turn after its mark. The
// close's Sandbox suspension carries the same turn start (record.IssueSuspend.TurnStart) and
// finishes without acting too (store.IssueSuspension), so nothing of the close outlives the hold:
// the planner stays live in its pod, and once an operator suspends it later, no row of the close
// is left to delete the pod and its disk without a read. The next pass decides the close again.
//
// The control runs the same stop with no turn since the check: the planner is suspended by the
// daemon and the Sandbox goes, the turn start of the planner's old turn notwithstanding.
func TestALeftPodCloseStopHoldsOnATurnThatStartedAfterItsCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stopQueued := func(t *testing.T) (*idleRig, time.Time) {
		t.Helper()
		rig, turnAt := checkedLeftPodClose(t, nil)
		if rows := rig.rows(t); !slices.Contains(rows, "supervise:suspend") || !slices.Contains(rows, "issue_suspend") {
			t.Fatalf("rows after the check = %v, want the close's stop and its Sandbox suspension queued", rows)
		}
		if fence, ok := rowTurnStart(t, rig, "supervise", "suspend"); !ok || !fence.Equal(turnAt) {
			t.Fatalf("the close's stop carries the turn start %v (named %t), want the check's, the planner's turn at %v", fence, ok, turnAt)
		}
		// From here every row the check queued is due.
		rig.runner.now = func() time.Time { return time.Now().Add(time.Minute) }
		return rig, turnAt
	}

	t.Run("a_turn_that_started_after_the_check", func(t *testing.T) {
		rig, turnAt := stopQueued(t)
		// A message reached the planner after the check's transaction committed: its turn starts,
		// and here it has ended too, before the lane runs the stop.
		for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: rig.token}, supervise.StreamTurnEnd{Claim: rig.token}} {
			if err := rig.machine.Handle(ctx, ev); err != nil {
				t.Fatalf("handle %T for %s: %v", ev, rig.token, err)
			}
		}
		rig.machine.Wait()
		if got := rig.machine.Claim(); !got.LastTurnStart.After(turnAt) {
			t.Fatalf("the planner's last turn started at %v, want after the check's turn start %v", got.LastTurnStart, turnAt)
		}
		rig.drain(t)
		if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
			t.Fatalf("the planner = %s by %s, want it live: its turn after the check may have made work the read did not see",
				got.State, got.SuspendedBy)
		}
		if got := rig.storedClaim(t, rig.token); got.State == supervise.StateSuspended || got.Locator == nil {
			t.Fatalf("the stored planner = %s with process %+v, want it live with the process it had", got.State, got.Locator)
		}
		// The runtime asks the store under its pod lock before every teardown, and records the ask
		// (releaseRuntime.teardown): the close's Sandbox suspension asks once, the store refuses it
		// for the turn after the check, and it is not asked again.
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue", "SuspendIssue"}) {
			t.Fatalf("teardowns asked = %v, want the move's and the close's one ask", asked)
		}
		if _, suspended := rig.rt.reads(); suspended != 1 {
			t.Fatalf("Sandbox teardowns the store authorized = %d, want the move's one", suspended)
		}
		if refused := rig.rt.refusals(); refused != 1 {
			t.Fatalf("Sandbox teardowns the store refused = %d, want the close's one: its suspension finishes with its held stop", refused)
		}
		if rows := rig.rows(t); slices.Contains(rows, "supervise:suspend") || slices.Contains(rows, "issue_suspend") {
			t.Fatalf("rows after the drain = %v, want the close's stop and its Sandbox suspension finished without acting", rows)
		}

		// An operator suspends the planner later (the api's suspendEvent), so no claim of the issue
		// may run any more. Nothing of the held close may delete the pod and its disk now: no read
		// has been made since the turn, and the next pass decides the close again.
		if err := rig.machine.Handle(ctx, supervise.RequestSuspend{Claim: rig.token, Reason: "the operator suspended it",
			By: supervise.SuspendedByOperator}); err != nil {
			t.Fatalf("the operator's suspension of %s: %v", rig.token, err)
		}
		rig.machine.Wait()
		if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByOperator {
			t.Fatalf("the planner after the operator's suspension = %s by %s, want it suspended by the operator", got.State, got.SuspendedBy)
		}
		rig.drain(t)
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue", "SuspendIssue"}) {
			t.Fatalf("teardowns asked after the operator's suspension = %v, want the move's and the close's one ask, and no other", asked)
		}
		if _, suspended := rig.rt.reads(); suspended != 1 {
			t.Fatalf("Sandbox teardowns the store authorized after the operator's suspension = %d, want the move's one: the held close deleted the pod with no read",
				suspended)
		}
		if refused := rig.rt.refusals(); refused != 1 {
			t.Fatalf("Sandbox teardowns the store refused after the operator's suspension = %d, want the close's one and no other ask", refused)
		}
	})

	t.Run("control:_no_turn_since_the_check", func(t *testing.T) {
		rig, _ := stopQueued(t)
		rig.drain(t)
		if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
			t.Fatalf("the planner = %s by %s, want it suspended by the daemon: no turn started after the check", got.State, got.SuspendedBy)
		}
		if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"SuspendIssue", "SuspendIssue"}) {
			t.Fatalf("teardowns asked = %v, want the move's and then the close's", asked)
		}
		if left := rig.rows(t); len(left) != 0 {
			t.Fatalf("rows left after the close = %v, want none", left)
		}
	})
}

// A left pod with no live claim runs no role to read its disk through, so the decline on record is
// the one word the daemon has of that disk. A planner whose relaunches ran out after a check found
// one commit origin does not hold left that commit on the disk of the pod the check read, and the
// close would delete it with the pod: the passes hold the pod as unowned_unpushed_work, as the
// release of a pod no claim owns does (pods.IdleDecline.HoldsUnpushedWork), queue nothing, ask for
// no teardown and record one streak of that hold. The control stands the same pod with no decline,
// and the passes close it as before: the Sandbox suspension alone.
func TestALeftPodWithNoLiveClaimKeepsADiskADeclineSaysHoldsWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// failedLeftPod is the child in backlog of newLeftPodRig whose planner failed rather than being
	// suspended, recording no process, in a pod the runtime names pod-a.
	failedLeftPod := func(t *testing.T) *idleRig {
		t.Helper()
		rig := newLeftPodRig(t, true)
		rig.rt.ScriptIssuePod(rig.issue.Key, "pod-a")
		if _, err := rig.pool.Exec(ctx, `update claims set state = 'failed', suspended_by = '', locator = null,
			updated_at = now() - interval '30 minutes' where token = $1`, string(rig.token)); err != nil {
			t.Fatalf("fail the planner: %v", err)
		}
		return rig
	}

	t.Run("a decline says the disk holds work", func(t *testing.T) {
		rig := failedLeftPod(t)
		if _, err := rig.pool.Exec(ctx, `insert into idle_release_declines
			(project, issue, declined_at, checked_at, role, reason, head, commits, changed, started_row, pod_uid)
			values ('legion', $1, now() - interval '1 hour', now() - interval '1 hour', 'planner', $2, $3, 1, false, 0, 'pod-a')`,
			rig.issue.Key, pods.DeclineUnpushedWork, idleRigUnpushed.Head); err != nil {
			t.Fatalf("record the decline: %v", err)
		}
		// The outbox's sequence counts a row the drain ran and deleted as much as one left queued.
		rows := rig.sequenceValue(t, "outbox")
		for range idleRigPasses {
			idlePasses(rig)
			rig.drain(t)
		}
		if after := rig.sequenceValue(t, "outbox"); after != rows {
			t.Fatalf("the passes queued outbox rows (the sequence moved from %d to %d), want none: the pod's disk holds work origin does not", rows, after)
		}
		if asked := rig.rt.teardownsAsked(); len(asked) != 0 {
			t.Fatalf("teardowns asked = %v, want none", asked)
		}
		if _, present := rig.rt.IssueSandbox(rig.issue.Key); !present {
			t.Fatal("the runtime no longer holds the child's Sandbox")
		}
		if held := holdsOf(t, rig, workflow.IdleHoldUnownedUnpushedWork); len(held) != 1 || held[0].Passes < 2 {
			t.Fatalf("holds = %+v, want one %s streak the passes counted on", rig.records(t).Holds, workflow.IdleHoldUnownedUnpushedWork)
		}
	})

	t.Run("control: no decline", func(t *testing.T) {
		rig := failedLeftPod(t)
		if queued := rig.queuedBy(t, func() { idlePasses(rig) }); !slices.Equal(queued, []string{"issue_suspend"}) {
			t.Fatalf("the passes queued %v, want the close's Sandbox suspension alone", queued)
		}
	})
}
