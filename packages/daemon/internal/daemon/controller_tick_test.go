package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The runtime's tick is what wakes a controller when no event does: on its period it applies a
// ControllerTick, and with a controller registered admission queues one `tick` controller notice
// named by the project, however many ticks pass before it is published. Here a slot is free; the
// tick's indifference to the slots is admission's to prove (controller_wake_test.go).
func TestTheControllerTickQueuesOneWakeWhileASlotIsFree(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The daemon's own path: `legion controller start` mints the capability and its session
	// registers with it, both under the project token (api/controller.go).
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(context.Background(), token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(context.Background(), token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log, controllerWake: 10 * time.Millisecond,
	}
	ticks := func() (rows int, issue string) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `select count(*), coalesce(min(issue), '') from outbox
			where kind = $1 and payload->>'kind' = 'tick'`, string(record.OutboxKindControllerNotice)).Scan(&rows, &issue); err != nil {
			t.Fatalf("read the tick notices: %v", err)
		}
		return rows, issue
	}
	processed := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where event_id like 'controller-tick:CAPTURE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the processed ticks: %v", err)
		}
		return n
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.tickController(ctx); close(done) }()
	testwait.Eventually(t, "three ticks applied", func() bool { return processed() >= 3 })
	cancel()
	<-done
	if rows, issue := ticks(); rows != 1 || issue != "CAPTURE" {
		t.Fatalf("tick notices = %d on %q; want one, named by the project", rows, issue)
	}
}

// The runtime ticks once shortly after it starts, not a full interval later, so a daemon restarted
// more often than controller_wake_interval_seconds still wakes its controller: with an hour's
// interval, the first tick is the early one.
func TestTheControllerTickComesShortlyAfterTheRuntimeStarts(t *testing.T) {
	pool := isolatedOutboxPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(record.NewStore(), engine, 1, "CAPTURE", log)
	w := &workflowRuntime{
		pool: pool, records: tickRecords(), admission: admission, handlers: []intake.Handler{engine, admission},
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log,
		controllerWake: time.Hour, controllerFirstWake: 10 * time.Millisecond,
	}
	processed := func() bool {
		var n int
		if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where event_id like 'controller-tick:CAPTURE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the processed ticks: %v", err)
		}
		return n == 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.tickController(ctx); close(done) }()
	testwait.Eventually(t, "the first tick, an hour before the interval's", processed)
	cancel()
	<-done
}

// The tick of a daemon that booted carries the silent list, because the boot registers the
// deployment's `worker_silence_minutes` with admission (openWorkflow, admit's
// UseWorkerSilence). The runtime here is the boot's own rather than one this test assembled, so
// a boot that registered no bound would leave the rule off and the tick below would name
// nothing at all. That is the whole of what this test holds: what the notice names of each
// silent worker is admission's own (internal/admit's silent_tick_test.go), and the architect of
// the issue is told the same fact in a `worker-silent` notice.
func TestControllerTickOfABootedWorkflowNamesTheSilentWorker(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	w := bootedTickWorkflow(t, pool)
	seedSilentPhaseWorker(t, pool, "CAPTURE-700", time.Now().Add(-2*time.Hour).UTC())

	w.applyControllerTick(ctx, time.Now())

	if silent := queuedTickNotice(t, pool).Silent; len(silent) != 1 {
		t.Fatalf("the tick names %#v as silent, want the one worker whose last step passed the bound the boot registered", silent)
	}
}

// The boot registers `worker_background_hours` beside the silence bound (openWorkflow, admit's
// UseWorkerSilence), so the tick names a worker whose own report of background work has stood
// past it. The worker here reports background work, which the silence rule reads as explaining
// its quiet, so a boot that registered only the silence bound would name nothing.
func TestControllerTickOfABootedWorkflowNamesTheBackgroundStalledWorker(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	w := bootedTickWorkflow(t, pool)
	seedSilentPhaseWorker(t, pool, "CAPTURE-700", time.Now().Add(-6*time.Hour).UTC())
	reported := time.Now().Add(-5 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `update claims set activity_at = $2, activity_background = true, activity_kinds = 'async-job'
		where issue = $1`, "CAPTURE-700", reported); err != nil {
		t.Fatalf("record the background report of CAPTURE-700's implementer: %v", err)
	}

	w.applyControllerTick(ctx, time.Now())

	if silent := queuedTickNotice(t, pool).Silent; len(silent) != 1 || !silent[0].BackgroundSince.Equal(reported) {
		t.Fatalf("the tick names %#v as silent, want CAPTURE-700's implementer with its report of %s: it has stood past the bound the boot registered",
			silent, reported)
	}
}

// bootedTickWorkflow is the boot's own workflow runtime over pool, with a controller registered,
// a board that answers the boot's calls, and the shipped bounds on a worker's silence and on its
// background work (config.Config.WorkerSilence, config.Config.WorkerBackground).
func bootedTickWorkflow(t *testing.T, pool *store.Pool) *workflowRuntime {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	registerTickController(t, pool)
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servedBootCall(t, w, r, "CAPTURE") {
			return
		}
		// Nothing waits on anything here, so the turn reads no issue from Dispatch at all
		// (observeWaitedOn); a read that does arrive is answered as a board that serves the
		// issue no longer answers, which costs the tick nothing.
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(board.Close)
	cfg := config.Config{
		Project: "CAPTURE", DispatchURL: board.URL, AdmissionCap: 1, WorkerSilence: 30 * time.Minute,
		WorkerBackground: 4 * time.Hour, Projects: map[string]config.Project{"CAPTURE": {}},
	}
	tokens := loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	w, err := openWorkflow(ctx, cfg, st, "project-id", "dispatch-token", nil,
		newRepositories(cfg.Project, cfg.RepositoryRenames), log, tokens, repositoriesGitHub(t), nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}
	w.bootID = "test-boot"
	return w
}

// seedSilentPhaseWorker records key as a running root of this project in implementing, holding
// an admission slot with its tree lifecycle open under the workflow, as promotion leaves a root
// it admitted, with its implementer's claim up and between turns (supervise.StateReady) holding
// no task, and the moment its agent last stepped. That is every term of the rule the tick reads
// (workflow.SilentPhaseWorker) with a step old enough to answer it.
//
// The turn the step was taken in started ten minutes before the step itself, so the claim holds
// both the columns the rule reads one moment from (record.RoleClaim.Step,
// supervise.StepRecord.LastStep).
func seedSilentPhaseWorker(t *testing.T, pool *store.Pool, key string, lastStep time.Time) {
	t.Helper()
	ctx := context.Background()
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1,
	})
	project, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.OpenTreeLifecycle(ctx, tx, project, key, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("record the admission slot and the tree lifecycle of %s: %v", key, err)
	}
	token, err := claim.NewToken(project, key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token of the implementer of %s: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, serving_generation, last_start_row, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'CAPTURE', $2, $2, 'implementer', 1, 'ses-implementer', '', $3, 1, 0, 0, 0, 0, 0)`,
		string(token), key, string(supervise.StateReady)); err != nil {
		t.Fatalf("record the implementer claim of %s: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `update claims set step_at = $2, turn_started_at = $3 where token = $1`,
		string(token), lastStep, lastStep.Add(-10*time.Minute)); err != nil {
		t.Fatalf("record the last step of the claim %s: %v", token, err)
	}
}

// queuedTickNotice is the one tick wake the pass queued for the controller, and fails the test
// where the pass queued another controller notice beside it: this case is about what one tick
// carries, so a second notice in the outbox would make the assertions read the wrong row.
func queuedTickNotice(t *testing.T, pool *store.Pool) record.ControllerNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 order by id`, string(record.OutboxKindControllerNotice))
	if err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var queued []record.ControllerNotice
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a controller notice: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode a controller notice: %v", err)
		}
		queued = append(queued, payload.(record.ControllerNotice))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	if len(queued) != 1 || queued[0].Kind != record.TickNotice {
		t.Fatalf("the controller notices = %#v, want the one tick this pass queued", queued)
	}
	return queued[0]
}
