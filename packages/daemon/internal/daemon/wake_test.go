package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// wakeRuntime is the smallest workflowRuntime the wake pass and the release gate read: the
// project, the store and a log a test can read back.
func wakeRuntime(t *testing.T) (*workflowRuntime, *store.Pool, *bytes.Buffer) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	logged := &bytes.Buffer{}
	w := &workflowRuntime{pool: pool, projectID: "legion", idleStore: st, idlePeriod: 10 * time.Minute,
		log: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return w, pool, logged
}

// recordPodWake writes one resume row as a wake's own transaction does (store.RecordPodWakeTx),
// which is what these tests seed: the whole wake (store.TakePodWake) also marks the wake and
// cancels a backstop, and neither is what the pass under test reads.
func recordPodWake(t *testing.T, st *store.Store, res store.PodResume) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, _, err = store.RecordPodWakeTx(ctx, tx, res)
		return err
	}); err != nil {
		t.Fatalf("record the wake of %s: %v", res.Issue, err)
	}
	return id
}

// recordListenerReady stamps the listener's liveness report at at, as the wake fact does.
func recordListenerReady(t *testing.T, pool *store.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into listener_reports (project, ready_at) values ('legion', $1)
		on conflict (project) do update set ready_at = excluded.ready_at`, at); err != nil {
		t.Fatalf("record the listener's report: %v", err)
	}
}

// R7: the daemon releases nothing unless the Envoy listener has reported itself ready within
// fifteen minutes. The gate runs before the pass reads a single pod, so a daemon that has heard
// nothing releases nothing at all rather than deciding each pod and then declining.
func TestTheDaemonReleasesNothingWithoutAFreshListenerReport(t *testing.T) {
	t.Parallel()
	t.Run("no report at all", func(t *testing.T) {
		w, _, logged := wakeRuntime(t)
		w.releaseIdlePods(context.Background())
		if !strings.Contains(logged.String(), "has not once reported itself ready") {
			t.Fatalf("the daemon's log = %q, want it to say the listener has never reported", logged)
		}
	})
	t.Run("a report older than the window", func(t *testing.T) {
		w, pool, logged := wakeRuntime(t)
		recordListenerReady(t, pool, time.Now().UTC().Add(-listenerReadyWindow-time.Minute))
		if w.listenerLive(context.Background()) {
			t.Fatalf("the listener reads live on a report older than %s", listenerReadyWindow)
		}
		w.releaseIdlePods(context.Background())
		if !strings.Contains(logged.String(), "too old to release a pod on") {
			t.Fatalf("the daemon's log = %q, want it to say the report is too old", logged)
		}
	})
	t.Run("a report inside the window", func(t *testing.T) {
		w, pool, _ := wakeRuntime(t)
		recordListenerReady(t, pool, time.Now().UTC().Add(-listenerReadyWindow+time.Minute))
		if !w.listenerLive(context.Background()) {
			t.Fatalf("the listener does not read live on a report inside %s", listenerReadyWindow)
		}
	})
}

// R5: a resume with no first turn ten minutes after its wake writes one warning naming the
// issue, the role and the step it waits on, and is marked, so it warns once however many passes
// read it. The clock is the test's: every time comes from the pass's own now.
func TestAStuckResumeIsWarnedAboutOnceAtTenMinutes(t *testing.T) {
	t.Parallel()
	w, pool, logged := wakeRuntime(t)
	ctx := context.Background()
	woke := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	id := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleImplementer), Claim: "legion-legion-legion-208-implementer", Kind: pods.WakeBackstop, At: woke})

	// Nine minutes after the wake nothing is stuck yet.
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake-time.Minute))
	if strings.Contains(logged.String(), "has had no first turn") {
		t.Fatalf("the daemon's log = %q, want no warning before ten minutes", logged)
	}
	if stuck := stuckMark(t, pool, id); stuck {
		t.Fatalf("the resume is marked stuck before ten minutes")
	}

	// Ten minutes after it, one warning naming the issue, the role and the step.
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake))
	line := logged.String()
	for _, want := range []string{"has had no first turn", "LEGION-208", string(claim.RoleImplementer), "the replacement pod to be ready"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the warning = %q, want it to name %q", line, want)
		}
	}
	if !stuckMark(t, pool, id) {
		t.Fatalf("the resume is not marked stuck at ten minutes")
	}

	// A later pass warns about it no second time: the mark is what takes it out of the read.
	logged.Reset()
	w.warnStuckResumes(ctx, woke.Add(time.Hour))
	if strings.Contains(logged.String(), "has had no first turn") {
		t.Fatalf("the daemon's log = %q, want the stuck resume warned about once", logged)
	}

	// A resume whose pod came up but whose role has taken no turn waits on the first turn.
	later := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(claim.RoleTester), Claim: "legion-legion-legion-208-tester", Kind: pods.WakeChecks, At: woke})
	if err := w.idleStore.MarkPodResumeReady(ctx, later, woke.Add(time.Minute)); err != nil {
		t.Fatalf("stamp the pod ready: %v", err)
	}
	logged.Reset()
	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake))
	if !strings.Contains(logged.String(), "the woken role's first turn") {
		t.Fatalf("the warning = %q, want it to say the pod is up and the first turn is what waits", logged)
	}
}

func stuckMark(t *testing.T, pool *store.Pool, id int64) bool {
	t.Helper()
	var stuck bool
	if err := pool.QueryRow(context.Background(), `select stuck from pod_resumes where id = $1`, id).Scan(&stuck); err != nil {
		t.Fatalf("read the resume's stuck mark: %v", err)
	}
	return stuck
}

// R5 and R9: a backstop that came due while the daemon was down fires at the first pass after it
// starts. The row is read from the database the restarted daemon opens, and the pass applies one
// fact per row under the row's own id, so the backstop fires once.
func TestABackstopThatCameDueWhileTheDaemonWasDownFiresAfterItStarts(t *testing.T) {
	t.Parallel()
	w, pool, _ := wakeRuntime(t)
	ctx := context.Background()
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := w.idleStore.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: "legion", Issue: "LEGION-208",
		Claim: mustClaimToken(t, "LEGION-208", claim.RoleImplementer), Role: claim.RoleImplementer, DueAt: due}); err != nil {
		t.Fatalf("arm the backstop: %v", err)
	}

	// The daemon stops and starts again: a new runtime over the same database, holding nothing
	// of the first one. The armed backstop is still due.
	restarted := &workflowRuntime{pool: pool, projectID: "legion", idleStore: outboxTreeStore(t, pool),
		idlePeriod: 10 * time.Minute, log: quietLogger()}
	records, err := restarted.idleStore.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the pod records: %v", err)
	}
	applied := &backstopHandler{records: records}
	restarted.handlers = []intake.Handler{applied}
	restarted.fireDueBackstops(ctx, time.Now().UTC())
	if applied.facts != 1 {
		t.Fatalf("facts applied after the restart = %d, want the one backstop", applied.facts)
	}
	restarted.fireDueBackstops(ctx, time.Now().UTC())
	if applied.facts != 1 {
		t.Fatalf("facts applied by a second pass = %d, want the backstop fired once", applied.facts)
	}
}

// The release of an issue's pod arms the backstop of the worker of the issue's open phase, and at
// step that worker is the role of the running step, read with the issue (workflow.OpenRoleOf):
// the released checker of an issue at step 2 of `implementer checker` gets the backstop, and the
// released implementer of the step before it gets none. The checker's claim must serve the
// issue's current run, as a phase worker's must: one serving an earlier run gets none. The issue
// is read as the release's teardown reads it before it arms (outbox.issue, releaseIssue), so its
// running role comes from its plan rows.
func TestTheReleaseOfAPodAtStepArmsTheDeploymentRolesBackstop(t *testing.T) {
	t.Parallel()
	const key, checker = "LEGION-208", claim.Role("checker")
	for _, c := range []struct {
		name    string
		serving uint64
		want    []string
	}{
		{"a checker serving the current run", 2, []string{string(checker)}},
		{"a checker serving an earlier run", 1, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: key, Title: "root", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				v := record.PlanVersion{Issue: key, Generation: 2, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
					GivenBy: mustClaimToken(t, key, claim.RoleArchitect), GivenAt: time.Now().UTC()}
				task := record.PlanTask{Issue: key, Generation: 2, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
				steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
					{Issue: key, Generation: 2, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
					{Issue: key, Generation: 2, Task: "a", Position: 2, Role: string(checker), Type: "testing"},
				})
				if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
					return err
				}
				return records.MoveTask(ctx, tx, key, 2, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
			}); err != nil {
				t.Fatalf("seed the plan of %s at the checker's step: %v", key, err)
			}
			st := outboxTreeStore(t, pool, key)
			for _, released := range []struct {
				role    claim.Role
				serving uint64
			}{{claim.RoleImplementer, 2}, {checker, c.serving}} {
				if err := st.PutClaim(ctx, supervise.Claim{Token: mustClaimToken(t, key, released.role), Project: "legion", Tree: key,
					Issue: key, Role: released.role, TreeEpoch: 1, Generation: 2, ServingGeneration: released.serving,
					State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
					Session: "ses-" + string(released.role), SessionFile: "/tmp/" + string(released.role) + ".jsonl",
					PluginContract: api.DaemonAPIVersion}); err != nil {
					t.Fatalf("seed the released %s: %v", released.role, err)
				}
			}
			runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", trees: st,
				backstop: time.Hour, log: quietLogger(), now: time.Now}

			issue, err := runner.issue(ctx, key)
			if err != nil {
				t.Fatalf("read %s as the teardown does: %v", key, err)
			}
			if err := runner.armBackstops(ctx, issue); err != nil {
				t.Fatalf("arm the backstops of %s's release: %v", key, err)
			}
			rows, err := pool.Query(ctx, `select role from pod_wake_backstops where issue = $1 and not fired order by role`, key)
			if err != nil {
				t.Fatalf("read the backstops of %s: %v", key, err)
			}
			armed, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the backstops of %s: %v", key, err)
			}
			if !slices.Equal(armed, c.want) {
				t.Fatalf("the release of %s's pod armed the backstops of %v, want %v", key, armed, c.want)
			}
		})
	}
}

// A backstop is the claim's it wakes. The release of a pod at step arms the backstop of the claim
// the record gives the running step (record.WorkerClaim): here the agent of a key on the checker's
// step, whose token names the implementer, beside the checker's own claim of an earlier version of
// the plan, both released. A wake of that other claim, and a `suspend` row that stops it, each take
// that claim over and leave the worker's backstop due: nothing else brings the worker back while it
// waits on a deploy run.
func TestAWakeOrAStopOfAnotherClaimOfTheRoleLeavesTheWorkersBackstop(t *testing.T) {
	t.Parallel()
	const key, checker = "LEGION-208", claim.Role("checker")
	ctx := context.Background()
	runner, sup, pool := runnerIn(t, phase.Step, "in_progress", &outboxPublisher{}, quietLogger())
	runner.backstop = time.Hour
	records := record.NewStore()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, key, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: string(checker), Type: "testing"},
		})
		if err := records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("seed the plan of %s at the checker's step: %v", key, err)
	}
	worker, other := mustClaimToken(t, key, claim.RoleImplementer), mustClaimToken(t, key, checker)
	if _, err := pool.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, pass, verdict, summary,
		step_task, step_place) values ($1, $2, $3, '', 0, 0, '', '', 'a', 2)`, key, string(checker), string(worker)); err != nil {
		t.Fatalf("name the keyed agent on the checker's step: %v", err)
	}
	// Each claim is the supervisor's, which the suspend row reaches, and the store's, which the
	// release's arming reads (ReleasedClaimsIn).
	st := runner.trees.(*store.Store)
	for _, token := range []claim.Token{worker, other} {
		released := supervise.Claim{Token: token, Project: "legion", Tree: key, Issue: key, Role: checker,
			Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
			Session: "ses-" + string(token), SessionFile: t.TempDir() + "/session.jsonl"}
		if _, _, err := sup.Create(ctx, released, ""); err != nil {
			t.Fatalf("create the released claim %s: %v", token, err)
		}
		if err := st.PutClaim(ctx, released); err != nil {
			t.Fatalf("store the released claim %s: %v", token, err)
		}
	}
	due := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1 and not fired`, key).Scan(&n); err != nil {
			t.Fatalf("count the backstops of %s: %v", key, err)
		}
		return n
	}
	issue, err := runner.issue(ctx, key)
	if err != nil {
		t.Fatalf("read %s as the teardown does: %v", key, err)
	}
	if err := runner.armBackstops(ctx, issue); err != nil {
		t.Fatalf("arm the backstops of %s's release: %v", key, err)
	}
	if got := due(); got != 1 {
		t.Fatalf("backstops armed by the release = %d, want the step's worker's one", got)
	}

	if taken, err := runner.trees.TakePodWake(ctx, "legion", key, other, checker, pods.WakeAgentMessage, time.Now(), 0); err != nil || !taken {
		t.Fatalf("the wake of the checker's own claim = %t, %v; want it taken", taken, err)
	}
	if got := due(); got != 1 {
		t.Fatalf("backstops due after the wake of the checker's own claim = %d, want the worker's still due", got)
	}
	if err := runner.execute(ctx, mustOutboxRow(t, key, record.SuperviseRequest{Op: "suspend", Tree: key, Role: checker,
		Claim: other, Generation: 1, Reason: "the tree was parked"}, time.Now())); err != nil {
		t.Fatalf("the suspend of the checker's own claim: %v", err)
	}
	if got := due(); got != 1 {
		t.Fatalf("backstops due after the suspend of the checker's own claim = %d, want the worker's still due", got)
	}
	var armedFor string
	if err := pool.QueryRow(ctx, `select claim from pod_wake_backstops where issue = $1 and not fired`, key).Scan(&armedFor); err != nil || armedFor != string(worker) {
		t.Fatalf("the open backstop is armed for %q, %v; want the step's worker %s", armedFor, err, worker)
	}
}

// A backstop that comes due wakes its pod's lifecycle lane once, whether its fact's commit reply
// came back or was lost and the store said it committed: the fact's wake queues the role's start
// row through the project's records, which wake that row's lane once the fact commits
// (projectRecords.wakeOnCommit), so the pass wakes nothing beside it. The wake's two lines — the
// close of the resume a pod that went away left open, and the pod coming back — say what the
// fact's transaction wrote, so each is a hook of its commit: a fact whose commit never landed
// writes neither, and the pass's retry, which fires the backstop, writes each once.
func TestADueBackstopWakesItsPodsLifecycleLaneOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cut  testdb.Cut
	}{{name: "committed"}, {name: "reply_lost", cut: testdb.CutAfterCommit}, {name: "rollback", cut: testdb.CutBeforeCommit}} {
		t.Run(tc.name, func(t *testing.T) {
			w, pool, _ := wakeRuntime(t)
			ctx := context.Background()
			faulted, fault := faultedPoolOf(t, pool)
			w.pool = faulted
			putOutboxIssue(t, pool, record.NewStore(), record.Issue{Key: pauseIssue, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: "LEGION", Tree: pauseIssue, Title: "Workflow", Phase: phase.Implementing,
				Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5})
			implementer := mustClaimToken(t, pauseIssue, claim.RoleImplementer)
			if err := w.idleStore.PutClaim(ctx, supervise.Claim{Token: implementer,
				Project: "legion", Tree: pauseIssue, Issue: pauseIssue, Role: claim.RoleImplementer, Generation: 1, ServingGeneration: 1,
				State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased, Session: "ses-implementer"}); err != nil {
				t.Fatalf("store the released implementer: %v", err)
			}
			// An earlier wake's resume, open when the pod went away again: the release recorded
			// after it stands above it, so this wake closes it before it takes its own.
			woke := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: pauseIssue,
				Role: string(claim.RoleImplementer), Claim: string(implementer), Kind: pods.WakeBackstop, At: woke})
			if _, _, err := w.idleStore.RecordPodRelease(ctx, store.PodRelease{Project: "legion", Issue: pauseIssue, Generation: 1,
				Roles: []string{string(claim.RoleImplementer)}, At: woke.Add(time.Minute)}); err != nil {
				t.Fatalf("record the pod's release: %v", err)
			}
			if err := w.idleStore.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: "legion", Issue: pauseIssue,
				Claim: implementer, Role: claim.RoleImplementer, DueAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
				t.Fatalf("arm the backstop: %v", err)
			}
			podRecords, err := w.idleStore.PodRecordsOf("LEGION")
			if err != nil {
				t.Fatal(err)
			}
			logs := &syncBuffer{}
			records := projectRecords{Store: record.NewStore(), project: "LEGION", wake: w.wakeOutbox, log: quietLogger()}
			w.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Pods: podRecords, Linger: time.Hour},
				slog.New(slog.NewTextHandler(logs, nil)))}
			wakes := recordLaneWakes(w, pauseIssue)
			starts := func() int {
				t.Helper()
				var n int
				if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and issue = $1
					and payload->>'op' = 'start' and payload->>'wakeKind' = $2`, pauseIssue, pods.WakeBackstop).Scan(&n); err != nil {
					t.Fatalf("count the backstop's start rows: %v", err)
				}
				return n
			}
			want := func(n int, when string) {
				t.Helper()
				if got := starts(); got != n {
					t.Fatalf("%s: start rows of the backstop's wake = %d, want %d", when, got, n)
				}
				if got := wakes(); got != n {
					t.Errorf("%s: wakes of %s's lifecycle lane = %d, want %d: the start row's own wake", when, pauseIssue, got, n)
				}
				for _, line := range []string{`msg="wake: a released pod is coming back"`, `msg="wake: a pod went away under an open resume`} {
					if got := count(logs, line); got != n {
						t.Errorf("%s: lines %s = %d, want %d", when, line, got, n)
					}
				}
			}
			if tc.cut != 0 {
				fired := fault.Cut(faultMatch, tc.cut)
				w.fireDueBackstops(ctx, time.Now().UTC())
				testdb.Met(t, fired)
			} else {
				w.fireDueBackstops(ctx, time.Now().UTC())
			}
			if tc.cut == testdb.CutBeforeCommit {
				want(0, "after a fact whose commit never landed")
				w.fireDueBackstops(ctx, time.Now().UTC())
				want(1, "after the pass fired the backstop again")
				return
			}
			want(1, "after the backstop fired")
		})
	}
}

// backstopHandler is the workflow engine's backstop arm as this test needs it: it counts the
// facts it is handed and fires the row conditionally, as the engine does, so a backstop two
// passes read is seen to fire once.
type backstopHandler struct {
	records *store.PodRecords
	facts   int
}

func (h *backstopHandler) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	due, ok := fact.(intake.PodBackstopDue)
	if !ok {
		return intake.Result{}, nil
	}
	fired, err := h.records.FirePodBackstop(ctx, tx, due.ID)
	if err != nil || !fired {
		return intake.Result{}, err
	}
	h.facts++
	return intake.Result{}, nil
}

// terminalHandler counts the workflow facts a claim's durable states applied, by their kind.
type terminalHandler struct {
	mu    sync.Mutex
	facts []string
}

func (h *terminalHandler) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch fact.(type) {
	case intake.ClaimReady:
		h.facts = append(h.facts, "ready")
	case intake.ClaimFailed:
		h.facts = append(h.facts, "failed")
	}
	return intake.Result{}, nil
}

func (h *terminalHandler) applied() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.facts...)
}

// A supervisor keeps one durable-state callback (supervisor.OnDurableState assigns it), so the
// daemon's two readers of those states are one callback: the workflow facts a claim's ready and
// failure are, and the stamps a released pod's resume is measured by. Registering them as two
// callbacks left only the second, which dropped every ClaimReady and ClaimFailed the workflow
// runs on.
//
// The callback is taken from the supervisor the boot's attach installed it on, never called as a
// method: a second registration in attach would be the same defect again, and only the hook the
// supervisor ends up holding shows it.
//
// R5: the first turn is stamped from the woken claim reaching working, not from a delivery's
// confirmation, because a wake for a notice or a hook prompt carries no delivery.
func TestTheDaemonsOneDurableStateCallbackCarriesTheFactsAndTheResumeStamps(t *testing.T) {
	t.Parallel()
	w, pool, _ := wakeRuntime(t)
	facts := &terminalHandler{}
	w.handlers = []intake.Handler{facts}
	woke := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	held := supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}
	id := recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: "LEGION-208",
		Role: string(held.Role), Claim: string(held.Token), Kind: pods.WakeRoleMessage, At: woke})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := newSupervisor(ctx, w.idleStore, "legion", t.TempDir(), quietLogger())
	w.attach(&supervision{supervisor: sup, log: quietLogger(), refreshGate: supervise.NewRefreshGate(0)})
	hook := sup.durableState
	if hook == nil {
		t.Fatal("attach left the supervisor no durable-state callback")
	}

	hook(held, supervise.StateReady)
	awaitResume(t, pool, id, "pod_ready_at")
	hook(held, supervise.StateWorking)
	awaitResume(t, pool, id, "first_turn_at")
	hook(held, supervise.StateFailed)

	deadline := time.Now().Add(10 * time.Second)
	for len(facts.applied()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := facts.applied(); len(got) != 2 || got[0] != "ready" || got[1] != "failed" {
		t.Fatalf("workflow facts = %v, want the claim's ready and its failure", got)
	}
}

// awaitResume waits for the resume's column to be stamped: the stamps are written off the
// machine's goroutine, so the callback returns before the write lands.
func awaitResume(t *testing.T, pool *store.Pool, id int64, column string) {
	t.Helper()
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool {
		var stamped bool
		if err := pool.QueryRow(context.Background(),
			`select `+column+` is not null from pod_resumes where id = $1`, id).Scan(&stamped); err != nil {
			t.Fatalf("read the resume's %s: %v", column, err)
		}
		return stamped
	}) {
		t.Fatalf("the resume's %s was never stamped", column)
	}
}
