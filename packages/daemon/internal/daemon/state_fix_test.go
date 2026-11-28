package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The state the daemon serves names a running fix's directory beside the workspace the runtime
// places the issue's roles in (engineWorkspaceDir, the directory every task the engine composes
// names), which is where the fix's working copy is made: the plugin reads the fix's completion
// there and nowhere else. The control is the same source with no workspace of its runtime, which
// serves the fix with no directory, so the directory is the runtime's and comes from nowhere else.
func TestTheStateNamesTheFixsDirectoryBesideTheRuntimesWorkspace(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	key := cfg.Project + "-1"
	records := record.NewStore()
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Project: cfg.Project, Tree: key, Title: key,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Phase: phase.Reviewing, Generation: 1, Status: "needs_review"}); err != nil {
			return err
		}
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
			GivenBy: "owner", GivenAt: time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)}
		tasks := []record.PlanTask{{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"}}
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, key, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Entry: 2}); err != nil {
			return err
		}
		fix := record.TaskFix{Issue: key, Generation: 1, Task: "a", Fix: 1, State: record.FixReading, Check: "unit",
			Head: "c0de0000000000000000000000000000000000ff", Place: 1}
		if err := records.PutFix(ctx, tx, fix); err != nil {
			return err
		}
		started := time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC)
		fix.State, fix.Entry, fix.StartedAt = record.FixRunning, 2, &started
		return records.UpdateFix(ctx, tx, fix)
	}); err != nil {
		t.Fatalf("seed %s beside its fix: %v", key, err)
	}
	located, err := tmux.WorkspaceDir(cfg.StateDir, ghrepo.MustParse("acme/widgets"), key)
	if err != nil {
		t.Fatalf("the tmux runtime's workspace of %s: %v", key, err)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	served := func(t *testing.T, workspaceDir func(record.Issue) string) string {
		t.Helper()
		view := &source{
			store: st, records: records, project: cfg.Project, projectToken: project, runtime: "tmux", admissionCap: cfg.AdmissionCap,
			startedAt: time.Now(), deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
			reports:      func() map[claim.Token]capabilities.Report { return nil },
			workspaceDir: workspaceDir,
		}
		var fix *api.FixView
		if err := store.TransactWith(ctx, st.Pool(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			state, _, err := view.State(ctx, tx)
			if err != nil {
				return err
			}
			fix = state.Issues[key].Fix
			return nil
		}); err != nil {
			t.Fatalf("State: %v", err)
		}
		if fix == nil || fix.Task != "a" || fix.Check != "unit" || fix.Entry != 2 {
			t.Fatalf("the state serves %s the fix %+v, want fix 1 of task a, for check unit, at entry 2", key, fix)
		}
		return fix.Dir
	}

	if got, want := served(t, engineWorkspaceDir(cfg)), workspace.FixDir(located); got != want {
		t.Errorf("the state names the fix's directory %q, want %q, beside the tmux runtime's workspace of %s", got, want, key)
	}
	if got := served(t, nil); got != "" {
		t.Errorf("a source with no workspace of its runtime names the fix's directory %q, want none", got)
	}
}
