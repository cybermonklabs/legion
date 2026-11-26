package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releasedPodUID is the uid of the pod the rig's release takes down: the pod of the issue whose
// roles are suspended and whose disk goes with it.
const releasedPodUID = "pod-being-released"

// releaseRuntime is the fake runtime with the two things an idle release asks of a Kubernetes
// one: the read of the issue workspace for work origin does not hold, made through a role's shim
// (the embedded fake's WorkspaceWork, which ScriptWorkspaceWork and FailWorkspaceWork set and which
// answers no work for an issue nothing set), and the issue-level teardown that takes the pod down
// — the release's (ReleaseIssue), and the close's (SuspendIssue), which a test reaches through the
// same lane. It also answers what the daemon reads of the pod outside the transaction (issuePods):
// whether it runs the configured image, and whether the runtime still holds the issue's Sandbox.
// The rest of issuePods, which pod the Sandbox holds and whether the runtime keeps the issue's
// volume, is the embedded fake's.
type releaseRuntime struct {
	*fake.Runtime
	mu        sync.Mutex
	onImage   bool
	suspended int
	refused   int
	// teardowns are the issue-level teardowns the daemon asked for, by method and in order,
	// whatever the store answered under the pod lock: ReleaseIssue for a release, SuspendIssue
	// for a close. A release that asked the close's teardown would delete a kept volume.
	teardowns []string
	// sandboxName is the name the runtime answers the issue's Sandbox under, as the rig derives
	// it from the issue's claim token (sandbox.SandboxName), and sandboxGone makes it answer that
	// it holds no Sandbox at all — the pod the runtime has already taken down, which a test sets
	// before the pass it wants to read that answer, or before a teardown that should find no
	// Sandbox to release (ReleaseIssue's false), as the retried teardowns of
	// TestARetriedTeardownOfAPodNoClaimOwnsRecordsWhatTheReleaseDid do.
	sandboxName string
	sandboxGone bool
	// beforeAuthorize runs inside each teardown, before it asks the store: the pod lock is held
	// and the teardown's own read has already answered, which is where production's second
	// answer can differ from its first. A test sets it before the drain.
	beforeAuthorize func()
	// sandboxReached, when set, is sent each issue whose Sandbox a pass reads, and sandboxGate
	// holds that read until the issue's channel closes: a test holds a pass open between two of
	// its issues with them.
	sandboxReached chan string
	sandboxGate    map[string]chan struct{}
	// afterPodRead, when set, runs once each read through the pod's launcher has answered and
	// before its answer reaches the daemon: a test replaces the issue's pod there, as a launch can
	// while a read is in flight.
	afterPodRead func()
}

func (r *releaseRuntime) IssuePodRunsConfiguredImage(string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onImage
}

// PodWorkspaceWork is the embedded fake's read through the pod's launcher, with afterPodRead run
// between the read and its answer.
func (r *releaseRuntime) PodWorkspaceWork(ctx context.Context, issue string) (runtime.WorkspaceWork, error) {
	work, err := r.Runtime.PodWorkspaceWork(ctx, issue)
	r.mu.Lock()
	hook := r.afterPodRead
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return work, err
}

func (r *releaseRuntime) IssueSandbox(issue string) (string, bool) {
	r.mu.Lock()
	reached, gate := r.sandboxReached, r.sandboxGate[issue]
	r.mu.Unlock()
	if reached != nil {
		reached <- issue
	}
	if gate != nil {
		<-gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxGone {
		return "", false
	}
	return r.sandboxName, true
}

func (r *releaseRuntime) SuspendIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error)) error {
	_, err := r.teardown(ctx, "SuspendIssue", authorize, nil)
	return err
}

func (r *releaseRuntime) ReleaseIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	return r.teardown(ctx, "ReleaseIssue", authorize, writing)
}

// teardown is both teardowns, method naming which: it records the call, runs beforeAuthorize,
// asks the store and counts the pod it took down, or the store's refusal. It answers whether it
// took a Sandbox down: an authorized teardown of a runtime that still holds one (sandboxGone
// false), which calls writing first, as the Kubernetes runtime calls it before its first write.
func (r *releaseRuntime) teardown(ctx context.Context, method string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	r.mu.Lock()
	r.teardowns = append(r.teardowns, method)
	hook := r.beforeAuthorize
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	act, err := authorize(ctx)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	if act {
		r.suspended++
	} else {
		r.refused++
	}
	takes := act && !r.sandboxGone
	r.mu.Unlock()
	if takes && writing != nil {
		if err := writing(ctx); err != nil {
			return false, err
		}
	}
	return takes, nil
}

// teardownsAsked is every issue-level teardown the daemon asked for, by method and in order.
func (r *releaseRuntime) teardownsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.teardowns)
}

// reads is the role of the claim each workspace read of the issue was made through, in order — the
// name its token ends in (claim.Token.Cut), which is the role for a role's own claim — and how many
// pods the runtime's teardown took down.
func (r *releaseRuntime) reads() (read []claim.Role, suspended int) {
	for _, call := range r.CallsOf("WorkspaceWork") {
		_, agent, _ := call.Claim.Cut()
		read = append(read, claim.Role(agent))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return read, r.suspended
}

// refusals is how many times the runtime asked the store under its pod lock and was refused.
func (r *releaseRuntime) refusals() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

// idleRig is an issue in the workflow whose pod has been quiet long enough to release: one live
// implementer in its own session, idle with the phase's task still pending, on the daemon's
// plugin contract and the configured image, with nothing queued for any role.
type idleRig struct {
	pool    *store.Pool
	st      *store.Store
	issues  record.Store
	runner  *outbox
	sup     *supervisor
	rt      *releaseRuntime
	w       *workflowRuntime
	issue   record.Issue
	token   claim.Token
	machine *supervise.Machine
}

func newIdleRig(t *testing.T) *idleRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	st := outboxTreeStore(t, pool, issue.Key)
	// The store of a daemon with a workflow, which an idle release needs: its claim writes queue
	// the role-holder publication (store.PublishRoleHolders), so the seeded idle implementer
	// leaves one queued row of that kind before any release is decided, and a test reads what a
	// pass queued with queuedBy.
	st.PublishRoleHolders(nil)

	// The workspace holds nothing that origin does not hold until a test scripts work into it
	// (ScriptWorkspaceWork), so the release's check lets it go on.
	rt := &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = rt, st, api.DaemonAPIVersion()
	// The teardown records the release through the supervisor's store, as the daemon's does.
	sup.store = st

	token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	locator, err := rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: claim.RoleImplementer})
	if err != nil {
		t.Fatalf("mint a locator: %v", err)
	}
	// The pod's Sandbox is named as the Kubernetes runtime names it, from a role claim of the
	// issue (sandbox.SandboxName, which the runtime's own IssueSandbox answers): the roles of an
	// issue share one Sandbox, so the implementer's token names the pod's.
	rt.sandboxName = sandbox.SandboxName(token)
	pending := supervise.Delivery{ID: "outbox:7", Task: "implement it", Phase: phase.Implementing,
		Generation: 1, QueuedAt: time.Now().UTC().Add(-time.Hour)}
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Locator: &locator, Session: "ses-implementer",
		SessionFile: "/tmp/implementer.jsonl", PluginContract: api.DaemonAPIVersion(), Pending: &pending}
	if err := st.PutClaimAndDelivery(ctx, stored, pending); err != nil {
		t.Fatalf("seed the idle implementer: %v", err)
	}
	if _, err := sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the idle implementer: %v", err)
	}
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if got := machine.Claim(); got.State != supervise.StateIdle || got.Pending == nil {
		t.Fatalf("the restored claim = %+v, want it idle holding its phase's task", got)
	}
	// The pod last did something half an hour ago, and its session reported five minutes later
	// that it runs no background work with no reminder armed.
	if _, err := pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now() - interval '25 minutes', activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where issue = $1`, issue.Key); err != nil {
		t.Fatalf("backdate the pod: %v", err)
	}
	// The idle rule releases nothing unless the Envoy listener has reported itself ready inside
	// the freshness window (R7, workflowRuntime.listenerLive). A rig stands for a deployment
	// whose listener is reporting.
	if _, err := pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, "legion", time.Now().UTC()); err != nil {
		t.Fatalf("seed the listener's report: %v", err)
	}

	// The drain runs a minute after whatever the release queued, so every row of it is due. The
	// Dispatch takes the role-holder publications the claims' writes queue, as a deployment's does,
	// and none of the release's rows calls Dispatch.
	runner := &outbox{
		pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		notices: &outboxPublisher{}, dispatch: &roleHoldersDispatch{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
	// The rig's issue records carry the Dispatch project key and its claims the claim token's
	// spelling, as a deployment's do, and the two passes read the two: the candidate answer of
	// the rule for a pod no claim owns reads issues.project (dispatchProject), everything else
	// reads the claims' (projectID).
	w := &workflowRuntime{pool: pool, projectID: "legion", dispatchProject: "LEGION", log: quietLogger(),
		idleStore: st, idleRuntime: rt, idlePeriod: 10 * time.Minute}
	return &idleRig{pool: pool, st: st, issues: records, runner: runner, sup: sup, rt: rt, w: w,
		issue: issue, token: token, machine: machine}
}

// drain runs the issue's lifecycle lane until it has nothing left to do.
func (r *idleRig) drain(t *testing.T) {
	t.Helper()
	for range 6 {
		if err := r.runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	}
}

// idleRigRow is one outbox row of the rig's issue: its id, the version of it the table holds
// (xmin, which each insert and each update of the row gives it anew), and its kind with the
// supervise op where it has one.
type idleRigRow struct {
	id      int64
	version string
	label   string
}

// outboxRows is every outbox row of the issue, in id order.
func (r *idleRig) outboxRows(t *testing.T) []idleRigRow {
	t.Helper()
	found, err := r.pool.Query(context.Background(), `select id, xmin::text, kind, coalesce(payload->>'op', '')
		from outbox where issue = $1 order by id`, r.issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	var rows []idleRigRow
	for found.Next() {
		var row idleRigRow
		var op string
		if err := found.Scan(&row.id, &row.version, &row.label, &op); err != nil {
			t.Fatal(err)
		}
		if op != "" {
			row.label += ":" + op
		}
		rows = append(rows, row)
	}
	if err := found.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// rows is every outbox row of the issue, by kind and the supervise op where it has one: what is
// left, as a drain reads it. What a pass queued is queuedBy's.
func (r *idleRig) rows(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, row := range r.outboxRows(t) {
		kinds = append(kinds, row.label)
	}
	return kinds
}

// queuedBy is what act queued: each outbox row of the issue that act inserted or wrote again, in id
// order and named as rows names them. A test that asserts what a pass queued reads this and not
// rows, because the seed queues a row before any pass: the rig's store publishes, so the seeded
// idle implementer's write queues its issue's role-holder publication (store.putClaim). That row
// is true of the state the rig builds, and it is no row of the pass. A row the act wrote again is
// the act's: a second queue of that publication makes the standing row due again rather than add
// one beside it (store.EnqueueRoleHolders), so it adds no id, and only the row's new version shows
// the write.
func (r *idleRig) queuedBy(t *testing.T, act func()) []string {
	t.Helper()
	seeded := map[int64]string{}
	for _, row := range r.outboxRows(t) {
		seeded[row.id] = row.version
	}
	act()
	var queued []string
	for _, row := range r.outboxRows(t) {
		if version, ok := seeded[row.id]; !ok || version != row.version {
			queued = append(queued, row.label)
		}
	}
	return queued
}

func (r *idleRig) boundary(t *testing.T) int64 {
	t.Helper()
	var boundary int64
	if err := r.pool.QueryRow(context.Background(), `select boundary_row from issue_releases where issue = $1`,
		r.issue.Key).Scan(&boundary); err != nil {
		t.Fatalf("read the release's boundary: %v", err)
	}
	return boundary
}

// outboxID is an id the outbox's own sequence hands out, as a row's insert takes it: no row holds
// it and the sequence never hands it out again. One taken before the release is below every row of
// the release, the id of a transaction already in flight when the mark was written, and one taken
// after is above them.
func (r *idleRig) outboxID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := r.pool.QueryRow(context.Background(), `select nextval(pg_get_serial_sequence('outbox', 'id'))`).Scan(&id); err != nil {
		t.Fatalf("take an outbox id: %v", err)
	}
	return id
}

func (r *idleRig) marked(t *testing.T) bool {
	t.Helper()
	var marked bool
	if err := r.pool.QueryRow(context.Background(), `select exists (select 1 from issue_releases where issue = $1)`,
		r.issue.Key).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	return marked
}

// declined is whether the store records a declined release of the issue (idle_release_declines),
// which only the release's workspace check writes: a release cancelled anywhere else records none.
func (r *idleRig) declined(t *testing.T) bool {
	t.Helper()
	var declined bool
	if err := r.pool.QueryRow(context.Background(), `select exists (select 1 from idle_release_declines where issue = $1)`,
		r.issue.Key).Scan(&declined); err != nil {
		t.Fatalf("read the declined release: %v", err)
	}
	return declined
}

// releasedArchitect leaves the pod an architect claim an earlier release suspended and no wake
// has brought back: suspended with the source `released`, as quiet as the rest of the pod, and
// restored into the supervisor the way the daemon's boot restores it.
func (r *idleRig) releasedArchitect(t *testing.T) *supervise.Machine {
	t.Helper()
	ctx := context.Background()
	token := mustClaimToken(t, r.issue.Key, claim.RoleArchitect)
	stored := supervise.Claim{Token: token, Project: "legion", Tree: r.issue.Tree, Issue: r.issue.Key,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-architect", SessionFile: "/tmp/architect.jsonl", PluginContract: api.DaemonAPIVersion()}
	if err := r.st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("seed the architect an earlier release left released: %v", err)
	}
	if _, err := r.sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the released architect: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes'
		where token = $1`, string(token)); err != nil {
		t.Fatalf("backdate the released architect: %v", err)
	}
	machine, ok := r.sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if got := machine.Claim(); !got.Released() {
		t.Fatalf("the restored architect = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	return machine
}

// The teardown acts on the answer the runtime took under its pod lock, not on the one this row
// read before it. The two differ: the release's rows run on the issue's lifecycle lane, and a
// notice or a hook prompt runs on a lane of its own, so the wake one of those makes can
// resume a role in between. A teardown that took the first answer and ignored the second
// recorded a release of a pod that still stands, armed a backstop over it and forgot its wake
// marks, and left the other roles released in a running pod with nothing to bring them back.
func TestATeardownTheRuntimeFindsUnauthorizedCancelsTheRelease(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// Under the pod lock, before the store is asked: a re-admission moved the issue's run on,
	// which is a close's business and not a release's.
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if refused := rig.rt.refusals(); refused != 1 {
		t.Fatalf("authorizations refused under the pod lock = %d, want the one the teardown asked for", refused)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the store refused the pod", suspended)
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the pod still stands", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldUnauthorized.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldUnauthorized)
	}
	var backstops int
	if err := rig.pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1`, rig.issue.Key).Scan(&backstops); err != nil {
		t.Fatal(err)
	}
	if backstops != 0 {
		t.Fatalf("backstops armed = %d, want none: a backstop belongs to a pod that is gone", backstops)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if rig.declined(t) {
		t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have resumed it into the pod it kept",
			got.State, got.SuspendedBy)
	}
}

// A cancel resumes the claims its own release suspended and no others, and the resume is a row
// of the issue's lane rather than a relaunch held in memory: a release that did not finish is
// undone by one transaction that queues those resumes and deletes the mark together.
//
// The pod here holds an architect an earlier release left released, which no wake has brought
// back. Resuming it would put a role into a pod that has no work for it, with no wake and no
// task saying why it is there.
func TestACancelResumesOnlyTheRolesItsOwnReleaseSuspended(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	architect := rig.releasedArchitect(t)

	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePod(ctx, rig.issue.Key) })
	if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
		t.Fatalf("queued rows = %v, want the workspace check, the implementer's stop and the teardown: the architect is already released", queued)
	}
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if got := architect.Claim(); !got.Released() {
		t.Fatalf("the architect = %s by %s, want the release that never touched it to have left it released",
			got.State, got.SuspendedBy)
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the implementer = %s by %s, want the claim this release suspended resumed", got.State, got.SuspendedBy)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
}

// The boundary's read belongs to the release and not to its claim: it is the one row that
// answers a task, notice, message or start the pod was given around the mark, so it runs before
// the row returns for a claim with nothing to stop. A boundary that returned first left that
// delivery unanswered, the release ran on and took the pod, and the delivery then cost a wake
// and a pod start to answer what the standing pod would have answered.
func TestABoundaryWhoseClaimRunsNothingStillAnswersADeliveryAroundTheMark(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	// A start for another role of the pod whose transaction was already in flight when the mark
	// was written: its id taken before the release's own rows, and not due, so it is still queued
	// when the boundary reads it.
	below := rig.outboxID(t)
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if boundary := rig.boundary(t); below >= boundary {
		t.Fatalf("control: the start's id %d is not below the release's boundary %d", below, boundary)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'supervise', $2, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now() + interval '1 hour', '')`,
		below, rig.issue.Key); err != nil {
		t.Fatal(err)
	}
	// The role whose stop is the boundary is retired before that stop runs: it has no process
	// left to stop, which is what used to take the row out before it read the mark.
	if err := rig.machine.Handle(ctx, supervise.RequestStop{Claim: rig.token}); err != nil {
		t.Fatalf("retire the claim the boundary stops: %v", err)
	}

	rig.drain(t)

	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the pod is the one that answers the queued start", suspended)
	}
	if rig.declined(t) {
		t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the release was cancelled at its boundary", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldOvertaken.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldOvertaken)
	}
}

// A wake the role's open resume refuses resumes a claim that is still released all the same, and
// this is the case that makes it necessary: the delivery wake commits its take and then resumes
// the claim in process, so a resume the store refuses, or a daemon that restarts between the two,
// leaves the role released with its resume open. Only a release closes an open resume, and a pod
// that is fully released is never released again, so without this the role's every later wake —
// a delivery, a CI settlement, a role message, its own backstop — would read that resume as a pod
// already on its way and resume nothing, for as long as the issue lives.
//
// The take is made here without the resume behind it, which is what both shapes leave in the
// record: a resume that failed after its take committed, and a daemon that came back up between
// the two and reads the claim released with the resume open. The wake that meets it takes no wake
// of its own — the resume already recorded is still the claim's one open resume — and brings the
// claim back. No pause held that resume, so the wake says at Warn that it stands open with no
// process behind it (logRefusedByOpenResume).
func TestAWakeResumesAClaimLeftReleasedUnderAnOpenResume(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}

	taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, claim.RoleImplementer, pods.WakeDelivery, time.Now().UTC(), 0)
	if err != nil || !taken {
		t.Fatalf("take the wake whose resume never ran = %t, %v; want it taken", taken, err)
	}
	resume, pause, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer, rig.token)
	if err != nil || !open || pause != 0 {
		t.Fatalf("the open resume after that take = %d, pause %d, %t, %v; want the one it recorded, which no pause held", resume, pause, open, err)
	}
	logs := &testlog.Buffer{}
	rig.runner.log = slog.New(slog.NewJSONHandler(logs, nil))

	woken, _, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken {
		t.Fatalf("the wake meeting the open resume = %t, %v; want the claim woken", woken, err)
	}
	stuck := loggedLines(t, logs.String(), `"msg":"wake: a released pod's resume stands open with no process behind it; this wake resumes it"`)
	if len(stuck) != 1 || stuck[0]["level"] != "WARN" {
		t.Fatalf("the stuck resume's lines = %v, want one at WARN; log:\n%s", stuck, logs.String())
	}
	if held := loggedLines(t, logs.String(), "its pause's held wake"); len(held) != 0 {
		t.Fatalf("the wake logged %v, want no line of a pause's held wake: no pause held this resume", held)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the wake that met the open resume = %s by %s, want it resumed: no release will ever close that resume",
			got.State, got.SuspendedBy)
	}
	// The heal takes no wake of its own: the resume the first take recorded is still the one
	// open, so the records hold one resume of this release rather than two.
	again, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer, rig.token)
	if err != nil || !open || again != resume {
		t.Fatalf("the open resume after the heal = %d, %t, %v; want the first take's %d", again, open, err, resume)
	}
	// And a wake that meets the open resume of a role whose process is running resumes nothing:
	// that pod is back, which is what the refusal means when the claim is no longer released.
	before := len(rig.rt.CallsOf("Resume"))
	woken, _, err = rig.runner.wakeForDelivery(ctx, rig.token, "a second notice of "+rig.issue.Key, 0)
	if err != nil || woken {
		t.Fatalf("the wake of a running claim = %t, %v; want it to wake nothing", woken, err)
	}
	if after := len(rig.rt.CallsOf("Resume")); after != before {
		t.Fatalf("resumes after the wake of a running claim = %d, want the %d it already had", after, before)
	}
}

// A pod that has had no turn in progress and no turn due for the idle period is released while
// its issue is still in the workflow: the decision writes the check of the workspace for work
// origin does not hold (its row's kind keeps the name issue_bundle), one `release` row per live
// claim behind it, each naming its claim, and the teardown last. Each claim goes to suspended with
// the source `released` keeping its phase and the task it holds, the workspace is read through the
// claim's shim before any claim's process is stopped — only a shim can read it — and the Sandbox
// teardown runs last.
func TestAnIdleIssuesPodIsReleasedWithItsClaimsKeepingTheirPhaseAndTask(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()

	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(ctx) })
	if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
		t.Fatalf("queued rows = %v, want the workspace check, then the role stop, then the teardown", queued)
	}
	if !rig.marked(t) {
		t.Fatal("the release left no mark")
	}

	rig.drain(t)

	got := rig.machine.Claim()
	if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("the released claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
	}
	if got.Pending == nil || got.Pending.Phase != phase.Implementing || got.Pending.ID != "outbox:7" {
		t.Fatalf("the released claim's task = %+v, want its phase's task kept", got.Pending)
	}
	if got.Generation != 1 || got.ServingGeneration != 1 {
		t.Fatalf("the released claim = %+v, want its place in the phase kept", got)
	}
	var state, by string
	if err := rig.pool.QueryRow(ctx, `select state, suspended_by from claims where token = $1`, string(rig.token)).Scan(&state, &by); err != nil {
		t.Fatal(err)
	}
	if state != "suspended" || by != "released" {
		t.Fatalf("the stored claim = %s by %s, want suspended by released", state, by)
	}
	var tasks int
	if err := rig.pool.QueryRow(ctx, `select count(*) from pending_task_deliveries where claim_token = $1`, string(rig.token)).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Fatalf("stored pending tasks = %d, want the phase's task kept through the release", tasks)
	}

	read, suspended := rig.rt.reads()
	if len(read) != 1 || read[0] != claim.RoleImplementer || rig.rt.CallsOf("WorkspaceWork")[0].Claim != rig.token {
		t.Fatalf("the workspace was read through %v, want the pod's one live claim %s", read, rig.token)
	}
	if suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want one", suspended)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release's alone: the issue stays in the workflow", asked)
	}
	if rig.declined(t) {
		t.Fatal("the release of a clean workspace recorded a decline")
	}
	if rig.marked(t) {
		t.Fatal("the finished release left its mark behind")
	}
	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("rows left after the release = %v, want none", got)
	}
}

// R4's boundary, through the whole daemon path. The boundary is the stop of the first role
// process of the pod, and until it has run a delivery for a role of the pod cancels the
// release: one queued below the release's own rows was in flight when the mark was written,
// one above them arrived while every role still ran. Either way the roles go on in the same
// pod, nothing is suspended, no Sandbox is torn down and no decline is recorded, since the
// workspace check found nothing to decline for.
func TestADeliveryThePodIsGivenAroundTheMarkCancelsTheRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		before bool
	}{
		{name: "queued below the release's rows", before: true},
		{name: "queued above them", before: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			below := rig.outboxID(t)
			rig.w.releaseIdlePods(ctx)
			boundary := rig.boundary(t)

			// Below the boundary means below the release's own first row, the check: the
			// delivery's transaction took its id before the release's and committed after. Above
			// it, an id taken after its last row. Both come from the outbox's own sequence, so no
			// row of the pod holds them. It is not due yet, so the drain below runs the release's
			// own rows and this row stays queued, which is what the boundary reads.
			id := below
			if !tc.before {
				id = rig.outboxID(t)
			}
			if (id < boundary) != tc.before {
				t.Fatalf("control: the delivery's id %d against the release's boundary %d is not where the row says", id, boundary)
			}
			if _, err := rig.pool.Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
				values ($1, 'supervise', $2, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now() + interval '1 hour', '')`,
				id, rig.issue.Key); err != nil {
				t.Fatal(err)
			}

			rig.drain(t)

			got := rig.machine.Claim()
			if got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want the cancelled release to have left it running", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none: the pod goes on", suspended)
			}
			if rig.declined(t) {
				t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
			}
			if rig.marked(t) {
				t.Fatal("the release's mark outlived it")
			}
			// The pod it kept is recorded under the hold's own word, so a deployment reading
			// the records sees a release that stopped short and why.
			holds := rig.records(t).Holds
			if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldOvertaken.String() {
				t.Fatalf("the holds are %+v, want one %s", holds, workflow.IdleHoldOvertaken)
			}
			// The delivery itself is untouched: it is still queued for its role.
			var left int
			if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
				and payload->>'op' = 'start' and payload->>'role' = 'tester'`, rig.issue.Key).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 1 {
				t.Fatalf("the queued start = %d rows, want it left for its own turn", left)
			}
		})
	}
}

// A turn that began after the release was marked — a direct message, a notice or a person's
// answer reaching a live session — leaves no outbox row at all: what says it happened is the
// claim, working rather than idle. The check read the workspace from before that turn's edits, so
// the boundary cancels the release and the pod keeps whatever the turn does in its own workspace.
func TestATurnStartedAfterTheMarkCancelsTheRelease(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePods(ctx)
	if _, err := rig.pool.Exec(ctx, `update claims set state = 'working' where issue = $1`, rig.issue.Key); err != nil {
		t.Fatal(err)
	}

	rig.drain(t)

	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have left it in its turn", got.State, got.SuspendedBy)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the pod is in a turn", suspended)
	}
	if rig.declined(t) {
		t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
	}
	if rig.marked(t) {
		t.Fatal("the release's mark outlived it")
	}
}

// A check that cannot show the workspace holds nothing origin lacks stops the release before any
// role stops: the pod stays, no claim is suspended, no Sandbox is torn down, and the decline is
// recorded with its reason. A read that found work tells the role it read through, once; a read
// that failed tells nobody, since there is nothing the role can push on its word. The work a role
// has not pushed is kept at the cost of a pod, never dropped to save one.
func TestAWorkspaceCheckThatCannotShowACleanWorkspaceDeclinesTheRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		work     runtime.WorkspaceWork
		err      error
		reason   string
		hold     workflow.IdleHold
		messages int
	}{
		{name: "the read failed", err: errors.New("the launcher answered nothing"),
			reason: pods.DeclineWorkspaceUnread, hold: workflow.IdleHoldWorkspaceUnread},
		{name: "the working copy holds a change", work: runtime.WorkspaceWork{Head: "abc", Changed: true},
			reason: pods.DeclineUnpushedWork, hold: workflow.IdleHoldUnpushedWork, messages: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.rt.ScriptWorkspaceWork(rig.issue.Key, tc.work)
			rig.rt.FailWorkspaceWork(rig.issue.Key, tc.err)

			rig.w.releaseIdlePods(ctx)
			rig.drain(t)

			if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want it left running in the pod the release kept", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none", suspended)
			}
			if rig.marked(t) {
				t.Fatal("the declined release left its mark behind")
			}
			var reason string
			if err := rig.pool.QueryRow(ctx, `select reason from idle_release_declines where issue = $1`, rig.issue.Key).Scan(&reason); err != nil || reason != tc.reason {
				t.Fatalf("the recorded decline = %q, %v; want %q", reason, err, tc.reason)
			}
			if holds := rig.records(t).Holds; len(holds) != 1 || holds[0].Reason != tc.hold.String() {
				t.Fatalf("holds = %+v, want the one %s", holds, tc.hold)
			}
			// The drain runs the message's row, so what it told the role is what the publisher
			// carried to the role's topic.
			publisher, ok := rig.runner.notices.(*outboxPublisher)
			if !ok {
				t.Fatalf("the rig's publisher is a %T, which records no message", rig.runner.notices)
			}
			if messages := roleMessages(publisher, string(rig.token)); len(messages) != tc.messages {
				t.Fatalf("messages to %s = %q, want %d", rig.token, messages, tc.messages)
			}
		})
	}
}

// Release is off unless the deployment asks for it: `idle_minutes: 0`, a runtime that is not
// Kubernetes, and a runtime that cannot read the issue's pod each leave the tick with nothing to
// run, so it returns rather than ticking over a pod it must not decide.
func TestTheIdleTickDoesNothingWhenReleaseIsOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		w    *workflowRuntime
	}{
		{name: "idle_minutes is 0", w: &workflowRuntime{idleStore: &store.Store{}, idleRuntime: &releaseRuntime{}}},
		{name: "no store", w: &workflowRuntime{idlePeriod: time.Minute, idleRuntime: &releaseRuntime{}}},
		{name: "a runtime that reads no issue pod", w: &workflowRuntime{idlePeriod: time.Minute, idleStore: &store.Store{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() { defer close(done); tc.w.tickIdle(context.Background()) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("tickIdle kept running with release off")
			}
		})
	}
}

// A release cancelled because its issue left the workflow queues no resume, and the issue's
// close then deletes its Sandbox, which is what takes the pod's role Secrets — its launcher
// tokens and its `gh` credentials — with it.
//
// The sequence is the one the lane runs. The release's save and role stop have run, so the
// implementer is suspended as `released`; the root is parked under the teardown's pod lock, so
// the linger's own rows — the role suspends and the issue_suspend that deletes the Sandbox —
// are queued above the release's; the teardown then reads the issue as no longer the workflow's
// and cancels. A resume queued for the claim the release suspended would land above the close's
// rows and supersede them: store.IssueSuspension would read a start newer than its own row,
// finish the close without acting, and leave the roles of a parked issue running in a Sandbox
// nothing deletes.
func TestACancelOfAnIssueThatLeftTheWorkflowLetsItsCloseDeleteTheSandbox(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour}, quietLogger())
	// The cancel names the claims it queued a resume for, by token, which is the one place that
	// set is read after the fact: a resume the lane has already run leaves no row behind.
	logs := &syncBuffer{}
	rig.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rig.runner.handlers = []intake.Handler{engine}
	rig.w.releaseIdlePod(ctx, rig.issue.Key)

	// Under the teardown's pod lock, before the store is asked: a person parks the root, which
	// lingers its tree and queues the close's own rows. The close's SuspendIssue runs through
	// the same hook, and parks nothing a second time.
	var parked sync.Once
	rig.rt.beforeAuthorize = func() {
		parked.Do(func() {
			if _, err := intake.ApplyFact(ctx, rig.pool, "dispatch", "park", intake.DispatchIssue{
				Key: rig.issue.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: rig.issue.Title, Rank: rig.issue.Rank}, engine); err != nil {
				t.Errorf("park the root: %v", err)
			}
		})
	}

	rig.drain(t)

	if !strings.Contains(logs.String(), "resumed=[]") {
		t.Fatalf("the cancel queued a resume; want none, since the claims of a parked issue are its close's and a start above the close's rows supersedes them: %s",
			logs.String())
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the close's one: its role Secrets go with the Sandbox", suspended)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue", "SuspendIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release's, which the store refused, and then the close's", asked)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if rig.declined(t) {
		t.Fatal("the cancelled release recorded a decline: only its workspace check declines")
	}
	var left int
	if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
		and kind in ('supervise', 'issue_suspend', 'issue_release')`, rig.issue.Key).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d stop rows of the pod remain, want the release's and the close's every one finished", left)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the implementer = %s by %s, want it suspended by the close that took its pod",
			got.State, got.SuspendedBy)
	}
}
