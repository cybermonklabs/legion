package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The state route of a daemon as its boot builds it names a running fix's directory beside the
// workspace the runtime places the issue's roles in: the boot hands the state's source the
// engine's own reading of that place (engineWorkspaceDir), so the directory a fix's task names
// and the one the plugin reads the fix's completion in are one. The source's own test
// (state_fix_test.go) builds a source with a reading of its choosing; this reads the one the
// boot hands it.
func TestABootedDaemonsStateNamesTheFixsDirectory(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
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

	fix := d.state().Issues[key].Fix
	if fix == nil || fix.Task != "a" || fix.Check != "unit" {
		t.Fatalf("the booted daemon serves %s the fix %+v, want fix 1 of task a, for check unit", key, fix)
	}
	if want := workspace.FixDir(located); fix.Dir != want {
		t.Errorf("the booted daemon names the fix's directory %q, want %q, beside the tmux runtime's workspace of %s", fix.Dir, want, key)
	}
}
