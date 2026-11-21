package daemon

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releasedPodUID is the pod the rig's release takes its bundle off: the pod of the issue whose
// roles are suspended and whose disk goes with it. The store keeps it on the row and answers
// that pod no restore of its own work (store.TakeWorkspaceBundleForRestore).
const releasedPodUID = "pod-being-released"

// releaseRuntime is the fake runtime with the two things an idle release asks of a Kubernetes
// one: the bundle of the issue workspace's unpushed work, taken from a launcher of the pod, and
// the issue-level teardown that takes the pod down — the release's (ReleaseIssue), and the
// close's (SuspendIssue), which a test reaches through the same lane. It also answers what the
// daemon reads of the pod outside the transaction (issuePods): whether it runs the configured
// image, and whether the runtime still holds the issue's Sandbox. The rest of issuePods, which pod
// the Sandbox holds and whether the runtime keeps the issue's volume, is the embedded fake's.
type releaseRuntime struct {
	*fake.Runtime
	mu        sync.Mutex
	onImage   bool
	bundle    runtime.WorkspaceBundle
	bundleErr error
	bundled   []claim.Role
	suspended int
	refused   int
	// teardowns are the issue-level teardowns the daemon asked for, by method and in order,
	// whatever the store answered under the pod lock: ReleaseIssue for a release, SuspendIssue
	// for a close. A release that asked the close's teardown would delete a kept volume.
	teardowns []string
	// sandboxName is the name the runtime answers the issue's Sandbox under, as the rig derives
	// it from the issue's claim token (sandbox.SandboxName), and sandboxGone makes it answer that
	// it holds no Sandbox at all — the pod the runtime has already taken down, which a test sets
	// before the pass it wants to read that answer.
	sandboxName string
	sandboxGone bool
	// beforeAuthorize runs inside each teardown, before it asks the store: the pod lock is held
	// and the teardown's own read has already answered, which is where production's second
	// answer can differ from its first. A test sets it before the drain.
	beforeAuthorize func()
	// failsAfterAuthorize is how many teardowns the store authorized still fail after they took
	// the pod down, as production's runtime fails when a step after the suspend (the wait for the
	// pod, the delete of the Secrets, the record on the Sandbox) does not go through.
	failsAfterAuthorize int
}

func (r *releaseRuntime) IssuePodRunsConfiguredImage(string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onImage
}

func (r *releaseRuntime) IssueSandbox(string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxGone {
		return "", false
	}
	return r.sandboxName, true
}

func (r *releaseRuntime) BundleWorkspace(_ context.Context, _ string, role claim.Role) (runtime.WorkspaceBundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundled = append(r.bundled, role)
	return r.bundle, r.bundleErr
}

func (r *releaseRuntime) SuspendIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error)) error {
	return r.teardown(ctx, "SuspendIssue", authorize)
}

func (r *releaseRuntime) ReleaseIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error)) error {
	return r.teardown(ctx, "ReleaseIssue", authorize)
}

// teardown is both teardowns, method naming which: it records the call, runs beforeAuthorize,
// asks the store and counts the pod it took down, or the store's refusal.
func (r *releaseRuntime) teardown(ctx context.Context, method string, authorize func(context.Context) (bool, error)) error {
	r.mu.Lock()
	r.teardowns = append(r.teardowns, method)
	hook := r.beforeAuthorize
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	act, err := authorize(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !act {
		r.refused++
		return nil
	}
	r.suspended++
	if r.failsAfterAuthorize > 0 {
		r.failsAfterAuthorize--
		return errors.New("the Secrets of the issue's Sandbox could not be deleted")
	}
	return nil
}

// teardownsAsked is every issue-level teardown the daemon asked for, by method and in order.
func (r *releaseRuntime) teardownsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.teardowns)
}

func (r *releaseRuntime) reads() (bundled []claim.Role, suspended int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]claim.Role(nil), r.bundled...), r.suspended
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

	rt := &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true,
		// A bundle names the pod its work came off, as the Kubernetes runtime's own does
		// (sandbox's Runtime.BundleWorkspace): the store refuses that pod the restore and keeps
		// the attempt for the pod that replaces it (store.PutWorkspaceBundle).
		bundle: runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "abc", Tips: []string{"abc"},
			At: time.Now().UTC(), PodUID: releasedPodUID}}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = rt, st, api.DaemonAPIVersion
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
		SessionFile: "/tmp/implementer.jsonl", PluginContract: api.DaemonAPIVersion, Pending: &pending}
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

func (r *idleRig) bundled(t *testing.T) bool {
	t.Helper()
	_, held, err := r.st.WorkspaceBundleSize(context.Background(), r.issue.Key)
	if err != nil {
		t.Fatalf("read the saved bundle: %v", err)
	}
	return held
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
		Session: "ses-architect", SessionFile: "/tmp/architect.jsonl", PluginContract: api.DaemonAPIVersion}
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
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
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
	rig := newIdleRig(t)
	ctx := context.Background()
	architect := rig.releasedArchitect(t)

	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePod(ctx, rig.issue.Key) })
	if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
		t.Fatalf("queued rows = %v, want the save, the implementer's stop and the teardown: the architect is already released", queued)
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

// A release can be cancelled after a wake it lets run: a CI settlement woke the implementer after
// the release recorded its claim as released, and then the issue's run moved on, so the teardown
// cancels the release. The pod never went away, so the wake's resume and its start go, and the
// cancel's own resume is the role's one start (store.CancelIssueRelease). A CI settlement reaches
// its woken role only through the wake's task, so that start carries the settlement's text under
// a preamble that says the release did not finish, built by the outbox's own builder
// (releaseResume), and nothing in it says that the pod was released.
func TestACancelAfterASettlementWakeTellsTheRoleTheSettlement(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}

	// The settlement's wake, as the workflow takes it (wakeReleased): the resume, and the start
	// that carries the wake's task and the wake's kind (workflow's wakeRow).
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	wake, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the wake's start: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rig.issue.Key, string(wake.Payload)); err != nil {
		t.Fatalf("queue the wake's start: %v", err)
	}

	// The issue's run moves on, so the teardown's store read cancels the release, and the
	// outbox carries the cancel out.
	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	rows, err := rig.pool.Query(ctx, `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`,
		rig.issue.Key)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		var start record.SuperviseRequest
		if err := rows.Scan(&payload); err != nil || json.Unmarshal(payload, &start) != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, start)
	}
	rows.Close()
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	const settled = "The wake is a CI settlement on your pull request's head: head1 failed build. Read the checks before you decide anything."
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != cancelled+settled ||
		strings.Contains(starts[0].Task, "pod had been released") {
		t.Fatalf("queued starts = %+v, want the cancel's one start of the implementer with the task %q: the settlement reaches the role only through it, and the pod never went away",
			starts, cancelled+settled)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes = %+v, want none: the release was cancelled, so no released pod came back", records.Resumes)
	}
}

// The same wake and cancel while the project is paused (decision 12 of dispatch://INFRA-3759): the
// CI settlement woke the implementer as a held wake of the pause, after the release recorded its
// claim, and then the issue's run moved on, so the teardown cancels the release. The cancel keeps
// that wake as the launch the pause owes the role, and the release took no pod. So the kept start's
// task is the task of a wake of a role the pause stopped (workflow.PausedWakeTask, which the
// outbox's cancel hands the store): the settlement's text under the pause's words, and no text
// that says the pod was released. The woken role's wake text is true (dispatch://INFRA-3589).
func TestACancelWhileThePausedProjectTellsTheKeptWakeThatThePauseStoppedTheRole(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	pause := openLegionPause(t, rig.pool, time.Now().UTC())

	// The settlement's wake, as the workflow takes it while the project is paused (wakeReleased):
	// a held wake of the pause, and its start, whose task WakeTask wrote for a session the release
	// stopped.
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	const detail = "head1 failed build"
	wake, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks, Pause: pause}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the wake's start: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rig.issue.Key, string(wake.Payload)); err != nil {
		t.Fatalf("queue the wake's start: %v", err)
	}

	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	rows, err := rig.pool.Query(ctx, `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`,
		rig.issue.Key)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		var start record.SuperviseRequest
		if err := rows.Scan(&payload); err != nil || json.Unmarshal(payload, &start) != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, start)
	}
	rows.Close()
	want := workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByPause)
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != want ||
		starts[0].WakeKind != pods.WakeChecks || starts[0].Pause != pause || strings.Contains(starts[0].Task, "pod had been released") {
		t.Fatalf("queued starts = %+v, want the kept wake's one start of the implementer, of the %s wake held by pause %d, with the task %q: the release took no pod, and the pause owes the role its launch",
			starts, pods.WakeChecks, pause, want)
	}
	if _, held, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || !open || held != pause {
		t.Fatalf("the implementer's open resume = pause %d, open %t, %v; want the kept wake, held by pause %d", held, open, err, pause)
	}
}

// A cancel after a CI settlement's wake, as in TestACancelAfterASettlementWakeTellsTheRoleTheSettlement,
// with a production check's look of the implementer queued beside the wake's start (the reviewer's
// read-ahead of dispatch://INFRA-3589, finding 2). The look names no phase and waits for no slot,
// as the wake's start does, but it carries no wake's kind (record.SuperviseRequest.WakeKind): it
// is work the pod was given, not a wake. So the cancel deletes the wake's resume and the wake's
// start and keeps the look as it was queued. The cancel's own start tells the role the
// settlement, and nothing of the look's task.
func TestACancelAfterASettlementWakeKeepsACheckLookQueuedForTheRole(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks}, time.Now().UTC()))
	const lookTask = "Production check of LEGION-208: the look the merge's deploy woke."
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: lookTask, CheckLook: 1, CheckOpenedAt: opened}, time.Now().UTC()))

	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	rows, err := rig.pool.Query(ctx, `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		order by id`, rig.issue.Key)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		var start record.SuperviseRequest
		if err := rows.Scan(&payload); err != nil || json.Unmarshal(payload, &start) != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, start)
	}
	rows.Close()
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	const settled = "The wake is a CI settlement on your pull request's head: head1 failed build. Read the checks before you decide anything."
	if len(starts) != 2 || starts[0].CheckLook != 1 || !starts[0].CheckOpenedAt.Equal(opened) || starts[0].Task != lookTask ||
		starts[1].Role != claim.RoleImplementer || starts[1].CheckLook != 0 || starts[1].Task != cancelled+settled {
		t.Fatalf("queued starts = %+v, want the check's look as it was queued and then the cancel's start of the implementer with the task %q: the look is no wake's start",
			starts, cancelled+settled)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes = %+v, want none: the release was cancelled, so no released pod came back", records.Resumes)
	}
}

// A release's record tells a held wake the release's words (dispatch://INFRA-3589, R3). A release
// that saved its workspace before the project's pause runs to its end. The CI settlement woke the
// implementer while the project is paused, after the release recorded the claim, so the wake is a
// held wake of the pause. Where the pause's stop of the role ran before the release's stop, the
// wake reads the pause as what stopped the role, and its start carries the pause's words, which
// say that the pod was kept (workflow.WakeTask, StoppedByPause). The test writes that start. The
// teardown then takes the pod and records the release, and the record is the moment the release's
// words become true. So the start's task becomes the settlement's text under the release's words
// (workflow.ReleasedWakeTask, which the teardown hands the store). The wake stays held by its
// pause, and its resume pairs with no release.
func TestARecordedReleaseTellsAHeldWakeThatTheReleaseTookItsPod(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	pause := openLegionPause(t, rig.pool, time.Now().UTC())

	// The settlement's wake, as the workflow takes it while the project is paused (wakeReleased):
	// a held wake of the pause, and its start in the pause's words.
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	const detail = "head1 failed build"
	wake, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByPause),
		WakeKind: pods.WakeChecks, Pause: pause}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the wake's start: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rig.issue.Key, string(wake.Payload)); err != nil {
		t.Fatalf("queue the wake's start: %v", err)
	}

	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if records := rig.records(t); rig.marked(t) || len(records.Releases) != 1 {
		t.Fatalf("the mark stands = %t and the releases = %+v, want the release finished and recorded once", rig.marked(t), records.Releases)
	}

	rows, err := rig.pool.Query(ctx, `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`,
		rig.issue.Key)
	if err != nil {
		t.Fatalf("read the queued starts: %v", err)
	}
	var starts []record.SuperviseRequest
	for rows.Next() {
		var payload []byte
		var start record.SuperviseRequest
		if err := rows.Scan(&payload); err != nil || json.Unmarshal(payload, &start) != nil {
			t.Fatalf("read a queued start: %v", err)
		}
		starts = append(starts, start)
	}
	rows.Close()
	want := workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByRelease)
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != want ||
		starts[0].WakeKind != pods.WakeChecks || starts[0].Pause != pause || strings.Contains(starts[0].Task, "Your project was paused") {
		t.Fatalf("queued starts = %+v, want the held wake's one start of the implementer, of the %s wake held by pause %d, with the task %q: the release took the pod",
			starts, pods.WakeChecks, pause, want)
	}
	if _, held, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || !open || held != pause {
		t.Fatalf("the implementer's open resume = pause %d, open %t, %v; want the held wake, held by pause %d", held, open, err, pause)
	}
	if resumes := rig.records(t).Resumes; len(resumes) != 1 || resumes[0].Release != 0 || resumes[0].Pause != pause {
		t.Fatalf("resumes = %+v, want the held wake's one resume, paired with no release and held by pause %d", resumes, pause)
	}
}

// The boundary's read belongs to the release and not to its claim: it is the one row that
// answers a task, notice, message or start the pod was given around the mark, so it runs before
// the row returns for a claim with nothing to stop. A boundary that returned first left that
// delivery unanswered, the release ran on and took the pod, and the delivery then cost a wake
// and a pod start to answer what the standing pod would have answered.
func TestABoundaryWhoseClaimRunsNothingStillAnswersADeliveryAroundTheMark(t *testing.T) {
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
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
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
// of its own — the resume already recorded is still the role's one open resume — and brings the
// claim back. No pause held that resume, so the wake says at Warn that it stands open with no
// process behind it (logRefusedByOpenResume).
func TestAWakeResumesAClaimLeftReleasedUnderAnOpenResume(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}

	taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeDelivery, time.Now().UTC(), 0)
	if err != nil || !taken {
		t.Fatalf("take the wake whose resume never ran = %t, %v; want it taken", taken, err)
	}
	resume, pause, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer)
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
	again, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer)
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

// A delivery for a role whose claim a release still running has recorded as `released` takes no
// wake. That release runs to its end whatever wakes the role, and the delivery's wake resumes the
// claim in process, so a resume now would launch the role in the pod the teardown is about to
// take. The row waits 30 seconds instead, as a routine wait, and reads the role again. Once the
// teardown has recorded the release and cleared its mark, the next attempt takes the wake against
// that release and resumes the role, in the new pod.
func TestADeliveryWakeWaitsForTheTeardownOfAReleaseThatRecordedItsRole(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// The teardown is not due yet, so the lane runs the save and the implementer's stop and the
	// release stands with the claim recorded.
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}

	resumes := len(rig.rt.CallsOf("Resume"))
	earliest := rig.runner.now().Add(30 * time.Second)
	woken, turn, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	latest := rig.runner.now().Add(30 * time.Second)
	at, until := wait.UntilAt(err)
	var routine interface{ Routine() bool }
	if woken || !turn.IsZero() || !until || at.Before(earliest) || at.After(latest) || !errors.As(err, &routine) || !routine.Routine() {
		t.Fatalf("the wake while the release runs = %t, %s, %v; want no wake and a routine wait until 30 seconds on: the release recorded the role",
			woken, turn, err)
	}
	if _, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || open {
		t.Fatalf("an open resume after the wake that waited = %t, %v; want none: the wake took nothing", open, err)
	}
	if got := rig.machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
		t.Fatalf("the claim after the wake that waited = %s by %s, resumes %d; want it released and not resumed in the old pod",
			got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
	}

	// The teardown runs: it records the release and clears the mark.
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("let the teardown run: %v", err)
	}
	rig.drain(t)
	records := rig.records(t)
	if rig.marked(t) || len(records.Releases) != 1 {
		t.Fatalf("the mark stands = %t and the releases = %+v, want the release finished and recorded once", rig.marked(t), records.Releases)
	}

	woken, turn, err = rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken || !turn.IsZero() {
		t.Fatalf("the wake once the teardown finished = %t, %s, %v; want the claim woken", woken, turn, err)
	}
	records = rig.records(t)
	if len(records.Resumes) != 1 || records.Resumes[0].Release != records.Releases[0].ID || records.Resumes[0].Kind != pods.WakeDelivery {
		t.Fatalf("resumes = %+v, want the one delivery wake of release %d", records.Resumes, records.Releases[0].ID)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the wake = %s by %s, want it resumed in the new pod", got.State, got.SuspendedBy)
	}
}

// The class search of dispatch://INFRA-3589 at 45fc14a3, entries 33 and 42. A cancel of a release
// queues a start for each claim it resumes and resumes none itself, so the claim reads `released`
// until that start runs, and its pod never went away (store.CancelIssueRelease). A delivery for
// that role in that time takes no wake: a wake would record a resume of a pod that no release
// took, and the records would count that pod's return. The row waits 30 seconds, as a routine
// wait, while the start is queued. The start resumes the role in the same pod, and the row's next
// attempt finds the claim live and publishes, so the role still gets its delivery.
func TestADeliveryWakeAfterACancelWaitsForTheCancelsStartAndRecordsNoResume(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	// The issue's run moves on, so the teardown cancels the release and queues the claim's resume.
	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	// The lane deletes a row it finished, and the cancel's start waits for the teardown row until
	// then (record's outboxInOrder).
	if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, teardown.ID); err != nil {
		t.Fatalf("finish the teardown row: %v", err)
	}
	if got := rig.machine.Claim(); rig.marked(t) || !got.Released() {
		t.Fatalf("after the teardown the mark stands = %t and the claim = %s by %s; want the release cancelled and the claim released until the cancel's start runs",
			rig.marked(t), got.State, got.SuspendedBy)
	}

	resumes := len(rig.rt.CallsOf("Resume"))
	earliest := rig.runner.now().Add(30 * time.Second)
	woken, turn, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	latest := rig.runner.now().Add(30 * time.Second)
	at, until := wait.UntilAt(err)
	var routine interface{ Routine() bool }
	if woken || !turn.IsZero() || !until || at.Before(earliest) || at.After(latest) || !errors.As(err, &routine) || !routine.Routine() ||
		!strings.Contains(err.Error(), "waits for the start queued for "+rig.issue.Key+"'s implementer") {
		t.Fatalf("the wake while the cancel's start is queued = %t, %s, %v; want no wake and a routine wait of 30 seconds for that start",
			woken, turn, err)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes after the wake that waited = %+v, want none: the release took no pod", records.Resumes)
	}
	if got := rig.machine.Claim(); !got.Released() || len(rig.rt.CallsOf("Resume")) != resumes {
		t.Fatalf("the claim after the wake that waited = %s by %s, resumes %d; want it released, for the cancel's start to resume",
			got.State, got.SuspendedBy, len(rig.rt.CallsOf("Resume"))-resumes)
	}

	// The cancel's start runs and resumes the role in the pod that stayed. The row's next attempt
	// finds the claim live: it wakes nothing and hands its delivery over.
	rig.drain(t)
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the claim after the cancel's start = %s by %s with the rows %v queued, want it resumed", got.State, got.SuspendedBy, rig.rows(t))
	}
	woken, turn, err = rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || woken || !turn.IsZero() {
		t.Fatalf("the wake once the cancel's start ran = %t, %s, %v; want no wait: the claim is live, and the delivery goes to it", woken, turn, err)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes after the cancel's start = %+v, want none: the release took no pod", records.Resumes)
	}
}

// A finished release arms the backstop of its open phase's worker, unless a wake of that worker
// came after the release recorded it: that wake's start already brings the worker back in the
// new pod, and a backstop for it could never act (armBackstops).
func TestAReleaseArmsNoBackstopForARoleAWakeIsBringingBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		woken     bool
		backstops int
	}{
		{name: "no wake during the release", backstops: 1},
		{name: "a wake after the release recorded the role", woken: true, backstops: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.runner.backstop = time.Hour
			rig.w.releaseIdlePod(ctx, rig.issue.Key)
			if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key); err != nil {
				t.Fatalf("hold the teardown back: %v", err)
			}
			rig.drain(t)
			if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
				t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
					got.State, got.SuspendedBy, rig.marked(t))
			}
			if tc.woken {
				if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token,
					pods.WakeAgentMessage, time.Now().UTC(), 0); err != nil || !taken {
					t.Fatalf("take the wake after the record = %t, %v; want it taken", taken, err)
				}
			}
			if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key); err != nil {
				t.Fatalf("let the teardown run: %v", err)
			}
			rig.drain(t)
			if records := rig.records(t); rig.marked(t) || len(records.Releases) != 1 {
				t.Fatalf("the mark stands = %t and the releases = %+v, want the release finished and recorded once", rig.marked(t), records.Releases)
			}
			var open int
			if err := rig.pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer' and not fired`,
				rig.issue.Key).Scan(&open); err != nil {
				t.Fatalf("count the implementer's open backstops: %v", err)
			}
			if open != tc.backstops {
				t.Fatalf("the implementer's open backstops = %d, want %d", open, tc.backstops)
			}
		})
	}
}

// A pod that has had no turn in progress and no turn due for the idle period is released while
// its issue is still in the workflow: the decision writes the save of the workspace's unpushed
// work, one `release` row per live role behind it, and the teardown last. Each claim goes to
// suspended with the source `released` keeping its phase and the task it holds, the bundle is
// saved before any role process is stopped — only a shim can build it — and the Sandbox teardown
// runs last.
func TestAnIdleIssuesPodIsReleasedWithItsClaimsKeepingTheirPhaseAndTask(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()

	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(ctx) })
	if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
		t.Fatalf("queued rows = %v, want the save, then the role stop, then the teardown", queued)
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

	bundled, suspended := rig.rt.reads()
	if len(bundled) != 1 || bundled[0] != claim.RoleImplementer {
		t.Fatalf("bundle asked of %v, want the pod's one live role", bundled)
	}
	if suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want one", suspended)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release's alone: the issue stays in the workflow", asked)
	}
	if !rig.bundled(t) {
		t.Fatal("the release deleted the pod with no bundle saved")
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
// pod, nothing is suspended, no Sandbox is torn down and the bundle the save stored goes with
// the release, since the pod keeps that work in its own workspace.
func TestADeliveryThePodIsGivenAroundTheMarkCancelsTheRelease(t *testing.T) {
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

			// Below the boundary means below the release's own first row, the save: the
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
			if rig.bundled(t) {
				t.Fatal("the cancelled release left its bundle behind")
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

// bundleDeleteRefused is the daemon's tree store with the bundle delete it once ran on its own
// before a cancel taken away: a store that answers every DeleteWorkspaceBundle with an error.
// Nothing of a cancel calls it any more, which is what this stands for.
type bundleDeleteRefused struct {
	outboxTrees
	err error
}

func (t bundleDeleteRefused) DeleteWorkspaceBundle(context.Context, string) error { return t.err }

// A cancel whose bundle delete fails leaves no bundle for a later pod, because the delete is
// the cancel's own transaction and no call of its own.
//
// The sequence it stops ran like this. A release saved the bundle of pod P and a delivery
// around the mark cancelled it; the cancel's separate delete failed, it logged a warning and
// went on, so P stood with its roles resumed and the bundle stayed. P is the pod the work came
// off, so it is answered nothing and spends nothing, and the row held the issue's pod
// (workflow.IdleHoldBundleUnrestored) rather than being replaced by another release. P's roles
// worked and pushed for hours. Then some later pod of the issue — an eviction's, an update
// round's replacement, a park and unpark's — asked for the bundle, was created after the save,
// and was sent it: `jj bookmark set --allow-backwards` and `jj edit` over a workspace
// provisioned from the branch those hours had been pushed to.
//
// So this drives that same cancel with the delete refused, and reads what the later pod is
// answered: nothing, and none of the saved bookmarks a restore would move back.
func TestACancelWhoseBundleDeleteFailsStillLeavesNoBundleForALaterPod(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.rt.bundle.Bookmarks = map[string]string{"legion/" + rig.issue.Key: "dffe88ae2f793e13436cef3921adc988433fbd4e"}
	rig.runner.trees = bundleDeleteRefused{outboxTrees: rig.st, err: errors.New("the pool is gone")}

	rig.w.releaseIdlePods(ctx)
	boundary := rig.boundary(t)
	// A start the pod is given above the release's own rows, which cancels at the boundary. It
	// is not due yet, so the drain runs the release's rows and leaves it queued.
	if _, err := rig.pool.Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'supervise', $2, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now() + interval '1 hour', '')`,
		boundary+10, rig.issue.Key); err != nil {
		t.Fatal(err)
	}

	rig.drain(t)

	// The pod stands with its roles back in it, which is the state the bundle must not outlive.
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have left it running", got.State, got.SuspendedBy)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the pod goes on", suspended)
	}
	if rig.marked(t) {
		t.Fatal("the release's mark outlived it, so the cancel did not run")
	}
	if rig.bundled(t) {
		t.Fatal("the cancel left its bundle behind when the store refused a delete of its own")
	}

	// The pod an eviction or an update round makes hours later, with the resumed roles' work
	// pushed in between: it is sent nothing at all.
	later, take, err := rig.st.TakeWorkspaceBundleForRestore(ctx, "legion", rig.issue.Key, "pod-after-an-eviction", time.Now().UTC().Add(time.Hour))
	if err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a pod created after the cancelled release's save was answered %q (%v, %v), want nothing", later.Bundle, take, err)
	}
	if len(later.Bookmarks) != 0 {
		t.Fatalf("that pod was answered the bookmarks %v, which a restore would move back over everything pushed since the cancel", later.Bookmarks)
	}
}

// clearRefusedOnce is the daemon's tree store with the teardown's last step refused once: the
// first ClearIssueRelease answers an error, as a store that lost its connection would, and every
// later one runs.
type clearRefusedOnce struct {
	outboxTrees
	refused *bool
}

func (t clearRefusedOnce) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if !*t.refused {
		*t.refused = true
		return errors.New("the pool is gone")
	}
	return t.outboxTrees.ClearIssueRelease(ctx, project, issue)
}

// A release's teardown is one-way once the runtime has taken the pod (the reviewer's second
// read-ahead of dispatch://INFRA-3589, D1). The teardown's record writes the release on the mark
// before any step that can fail. Here the teardown's last step fails, so its row runs again. That
// attempt finishes the release whatever happened meanwhile: it asks the runtime nothing, cancels
// nothing, keeps the bundle of unpushed work for the pod that replaces this one, and records no
// second release. Before, the attempt read the mark again: a claim an operator took over in
// between made it cancel a release whose pod was gone, which deleted the bundle, and an attempt
// with nothing new took the pod a second time and recorded a second release.
func TestATeardownThatFailsAfterItsPodIsGoneFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		takeOver bool
	}{
		{name: "an operator took a claim over between the attempts", takeOver: true},
		{name: "nothing changed between the attempts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			refused := false
			rig.runner.trees = clearRefusedOnce{outboxTrees: rig.st, refused: &refused}
			rig.w.releaseIdlePod(ctx, rig.issue.Key)
			if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key); err != nil {
				t.Fatalf("hold the teardown back: %v", err)
			}
			rig.drain(t)
			if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) || !rig.bundled(t) {
				t.Fatalf("the claim = %s by %s, the mark stands = %t and the bundle is saved = %t; want the release saved and stopped, its teardown to come",
					got.State, got.SuspendedBy, rig.marked(t), rig.bundled(t))
			}
			var teardown record.OutboxRow
			if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
				rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
				t.Fatalf("read the teardown row: %v", err)
			}
			if err := rig.runner.execute(ctx, teardown); err == nil || !refused {
				t.Fatalf("the teardown's first attempt = %v, want the refused last step's error", err)
			}
			if _, taken := rig.rt.reads(); taken != 1 || !rig.marked(t) {
				t.Fatalf("after the first attempt the runtime took %d pods and the mark stands = %t; want the pod taken once and the mark left for the next attempt",
					taken, rig.marked(t))
			}
			if tc.takeOver {
				if _, err := rig.pool.Exec(ctx, `update claims set suspended_by = 'operator' where token = $1`, string(rig.token)); err != nil {
					t.Fatalf("take the implementer's claim over as an operator: %v", err)
				}
			}

			if err := rig.runner.execute(ctx, teardown); err != nil {
				t.Fatalf("the teardown's next attempt: %v", err)
			}
			records := rig.records(t)
			if _, taken := rig.rt.reads(); rig.marked(t) || !rig.bundled(t) || len(records.Releases) != 1 || taken != 1 {
				t.Fatalf("after the next attempt the mark stands = %t, the bundle is saved = %t, the releases = %+v and the runtime took %d pods; want the release finished once: no mark, the bundle kept for the next pod, one release and one pod taken",
					rig.marked(t), rig.bundled(t), records.Releases, taken)
			}
		})
	}
}

// The runtime's read under the pod's launch turn is the teardown's point of no return: the runtime
// takes the pod on it. So that read commits the release where it acts (store's
// CommitIssueRelease), before the runtime's later steps. Here a later step of the runtime fails
// after the pod is gone, which leaves no record of the release, and the row runs again after a
// claim of the pod was taken over or the pod was given work. That attempt finishes the release: it
// asks the runtime again, records the release once, keeps the bundle of unpushed work for the next
// pod, and cancels nothing. Before, it read the mark again and cancelled a release whose pod was
// gone, which deleted the bundle.
func TestATeardownWhoseRuntimeFailsAfterItTookThePodFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range teardownInterleavings {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.rt.failsAfterAuthorize = 1
			teardown := rig.heldTeardown(t)
			if err := rig.runner.execute(ctx, teardown); err == nil {
				t.Fatal("the teardown's first attempt succeeded, want the runtime's error after it took the pod")
			}
			if records := rig.records(t); len(records.Releases) != 0 || !rig.marked(t) {
				t.Fatalf("after the first attempt the releases = %+v and the mark stands = %t; want no record and the mark left", records.Releases, rig.marked(t))
			}
			tc.between(t, rig)

			if err := rig.runner.execute(ctx, teardown); err != nil {
				t.Fatalf("the teardown's next attempt: %v", err)
			}
			records := rig.records(t)
			asked := rig.rt.teardownsAsked()
			if rig.marked(t) || !rig.bundled(t) || len(records.Releases) != 1 || !slices.Equal(asked, []string{"ReleaseIssue", "ReleaseIssue"}) {
				t.Fatalf("after the next attempt the mark stands = %t, the bundle is saved = %t, the releases = %+v and the runtime was asked %v; want the release finished: no mark, the bundle kept, one release, and the runtime asked again",
					rig.marked(t), rig.bundled(t), records.Releases, asked)
			}
		})
	}
}

// The record of a release (store.Store.RecordPodRelease) is the teardown's first step once the
// runtime has answered, and it can fail too: the runtime has taken the pod, and the mark names no
// release yet. A daemon that stops between the runtime's answer and the record leaves the store
// in the same state, and the daemon that replaces it runs the row again. Here the record fails
// once, and the row runs again after a claim of the pod was taken over or the pod was given work:
// in the same daemon, and in a daemon restarted from the store alone (idleRig.restarted). The
// runtime's read committed the release (store's CommitIssueRelease), so that attempt finishes
// it: it asks the runtime again, records the release once, keeps the bundle of unpushed work for
// the next pod, and cancels nothing. Before, it read the mark again and cancelled a release whose
// pod was gone, which deleted the bundle.
func TestATeardownWhoseRecordFailsOnceFinishesOnItsNextAttempt(t *testing.T) {
	for _, tc := range teardownInterleavings {
		for _, restart := range []bool{false, true} {
			name := tc.name
			if restart {
				name += ", and the daemon restarted"
			}
			t.Run(name, func(t *testing.T) {
				rig := newIdleRig(t)
				ctx := context.Background()
				teardown := rig.heldTeardown(t)
				refusePodReleaseRecordOnce(t, rig.pool)
				if err := rig.runner.execute(ctx, teardown); err == nil {
					t.Fatal("the teardown's first attempt succeeded, want the error of its record")
				}
				if _, taken := rig.rt.reads(); taken != 1 || len(rig.records(t).Releases) != 0 || !rig.marked(t) {
					t.Fatalf("after the first attempt the runtime took %d pods, the releases = %+v and the mark stands = %t; want the pod taken once, no record and the mark left",
						taken, rig.records(t).Releases, rig.marked(t))
				}
				tc.between(t, rig)
				runner := rig.runner
				if restart {
					runner = rig.restarted(t)
				}

				if err := runner.execute(ctx, teardown); err != nil {
					t.Fatalf("the teardown's next attempt: %v", err)
				}
				records := rig.records(t)
				asked := rig.rt.teardownsAsked()
				if rig.marked(t) || !rig.bundled(t) || len(records.Releases) != 1 || len(records.Holds) != 0 ||
					!slices.Equal(asked, []string{"ReleaseIssue", "ReleaseIssue"}) {
					t.Fatalf("after the next attempt the mark stands = %t, the bundle is saved = %t, the releases = %+v, the holds = %+v and the runtime was asked %v; want the release finished: no mark, the bundle kept, one release, no cancel's hold, and the runtime asked again",
						rig.marked(t), rig.bundled(t), records.Releases, records.Holds, asked)
				}
			})
		}
	}
}

// teardownInterleavings are what can come between two attempts of a release's teardown, each of
// which cancels a release that no read of the runtime has committed: an operator's takeover of a
// claim of the pod, and a start of another role queued, which gives the pod work.
var teardownInterleavings = []struct {
	name string
	// between is what happens between the two attempts of the teardown.
	between func(t *testing.T, rig *idleRig)
}{
	{name: "an operator took a claim over", between: func(t *testing.T, rig *idleRig) {
		if _, err := rig.pool.Exec(context.Background(), `update claims set suspended_by = 'operator' where token = $1`,
			string(rig.token)); err != nil {
			t.Fatalf("take the implementer's claim over as an operator: %v", err)
		}
	}},
	{name: "a start of another role was queued", between: func(t *testing.T, rig *idleRig) {
		rig.startQueuedAt(t, rig.outboxID(t)+100, claim.RoleTester)
	}},
}

// heldTeardown runs the rig's idle release up to its teardown and answers the teardown's row. The
// row is held back an hour, so the drain leaves it, and the test runs each of its attempts itself.
func (r *idleRig) heldTeardown(t *testing.T) record.OutboxRow {
	t.Helper()
	ctx := context.Background()
	r.w.releaseIdlePod(ctx, r.issue.Key)
	if _, err := r.pool.Exec(ctx, `update outbox set next_at = now() + interval '1 hour' where issue = $1 and kind = 'issue_release'`,
		r.issue.Key); err != nil {
		t.Fatalf("hold the teardown back: %v", err)
	}
	r.drain(t)
	if got := r.machine.Claim(); !got.Released() || !r.marked(t) || !r.bundled(t) {
		t.Fatalf("the claim = %s by %s, the mark stands = %t and the bundle is saved = %t; want the release saved and stopped, its teardown to come",
			got.State, got.SuspendedBy, r.marked(t), r.bundled(t))
	}
	var teardown record.OutboxRow
	if err := r.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		r.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	return teardown
}

// refusePodReleaseRecordOnce makes the database refuse the next record of a release: a trigger
// on pod_releases raises an error at the first insert, so the record's transaction
// (store.Store.RecordPodRelease) rolls back. The trigger counts on a sequence, which no rollback
// takes back, so every later insert goes through.
func refusePodReleaseRecordOnce(t *testing.T, pool *store.Pool) {
	t.Helper()
	for _, statement := range []string{
		`create sequence refused_pod_release_records`,
		`create function refuse_pod_release_record_once() returns trigger language plpgsql as $$
		begin
			if nextval('refused_pod_release_records') = 1 then
				raise exception 'the record of the release could not be written';
			end if;
			return new;
		end $$`,
		`create trigger refuse_pod_release_record_once before insert on pod_releases
			for each row execute function refuse_pod_release_record_once()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("refuse the next record of a release: %v", err)
		}
	}
}

// restarted is the rig's daemon after a restart: a supervisor that restores every claim the store
// holds, as the boot does (supervisor.restore), on the same store and runtime, and an outbox
// runner over it that holds nothing of what the daemon before it did.
func (r *idleRig) restarted(t *testing.T) *outbox {
	t.Helper()
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = r.rt, r.st, api.DaemonAPIVersion
	sup.store = r.st
	claims, err := r.st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims the restarted daemon restores: %v", err)
	}
	if _, err := sup.restore(ctx, claims); err != nil {
		t.Fatalf("restore the claims: %v", err)
	}
	return &outbox{
		pool: r.pool, records: r.issues, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: r.st,
		notices: &outboxPublisher{}, dispatch: &roleHoldersDispatch{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
}

// A turn that began after the release was marked — a direct message, a notice or a person's
// answer reaching a live session — leaves no outbox row at all: what says it happened is the
// claim, working rather than idle. The save stored the workspace from before that turn's edits,
// so the boundary cancels the release and the pod keeps the work in its own workspace.
func TestATurnStartedAfterTheMarkCancelsTheRelease(t *testing.T) {
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
	if rig.bundled(t) {
		t.Fatal("the cancelled release left behind a bundle taken before the turn's edits")
	}
	if rig.marked(t) {
		t.Fatal("the release's mark outlived it")
	}
}

// A bundle the release cannot save stops it: the pod stays, every claim the release suspended is
// resumed into it, and no Sandbox is torn down. The work a role has not pushed is kept at the
// cost of a pod, never dropped to save one.
func TestABundleTheReleaseCannotSaveStopsItAndResumesTheClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bundle runtime.WorkspaceBundle
		err    error
	}{
		{name: "the bundle could not be built", err: errors.New("the launcher answered nothing")},
		{name: "the bundle is over the bound", bundle: runtime.WorkspaceBundle{Bundle: make([]byte, runtime.MaxWorkspaceBundleBytes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.rt.bundle, rig.rt.bundleErr = tc.bundle, tc.err

			rig.w.releaseIdlePods(ctx)
			rig.drain(t)

			got := rig.machine.Claim()
			if got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want it resumed into the pod the release kept", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none", suspended)
			}
			if rig.bundled(t) {
				t.Fatal("a release that could not save its bundle left one behind")
			}
			if rig.marked(t) {
				t.Fatal("the stopped release left its mark behind")
			}
		})
	}
}

// Release is off unless the deployment asks for it: `idle_minutes: 0`, a runtime that is not
// Kubernetes, and a runtime that cannot read the issue's pod each leave the tick with nothing to
// run, so it returns rather than ticking over a pod it must not decide.
func TestTheIdleTickDoesNothingWhenReleaseIsOff(t *testing.T) {
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
	rig := newIdleRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour}, quietLogger())
	// The cancel names the roles it queued a resume for, which is the one place that set is
	// read after the fact: a resume the lane has already run leaves no row behind.
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
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
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
