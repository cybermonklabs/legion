package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The claim of a role the deployment defines is one of the issue's claims wherever the daemon
// walks them (claimsOfIssue, every claim of the issue it supervises): a carried child whose checker still runs in its old tree waits for that claim to stop
// before its new tree starts (stopElsewhere), and a tmux workspace removal waits for the tree's
// close to retire the checker's claim before it removes the workspace (removeWorkspace).
func TestTheOldTreeAndTheWorkspaceTakeADeploymentRolesClaim(t *testing.T) {
	t.Parallel()
	t.Run("a carried child's old tree", func(t *testing.T) {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		parent := "LEGION-207"
		child := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
			Parent: &parent, Title: "child", Phase: phase.Step, Generation: 2, Status: "in_progress"}
		putOutboxIssue(t, pool, records, child)
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		checker := mustClaimToken(t, child.Key, "checker")
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: checker, Project: "legion", Tree: parent, Issue: child.Key, Role: "checker", Generation: 2,
			State: supervise.StateWorking, Session: "ses_checker", SessionFile: "/sessions/checker.jsonl",
			Pending: &supervise.Delivery{ID: "outbox:1", Task: "Check it.", Generation: 2, Phase: phase.Step, ConfirmedAt: time.Now()}}, ""); err != nil {
			t.Fatalf("create the checker's claim in the old tree: %v", err)
		}
		runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now}

		err := runner.stopElsewhere(ctx, child)

		if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(checker)) {
			t.Fatalf("stopElsewhere = %v, want a wait naming the checker's claim %s, still in tree %s", err, checker, parent)
		}
	})

	t.Run("a tmux workspace removal", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		until := time.Now().Add(-time.Minute)
		issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
			Title: "Workflow", Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until}
		putOutboxIssue(t, pool, records, issue)
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		checker := mustClaimToken(t, issue.Key, "checker")
		if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: checker, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: "checker",
			Generation: 2, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon}, ""); err != nil {
			t.Fatalf("create the checker's claim: %v", err)
		}
		removed := false
		runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, project: "legion", log: quietLogger(), stateDir: t.TempDir(),
			remove: func(context.Context, workspace.Workspace, sync.Locker) error { removed = true; return nil }}

		err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.WorkspaceRemove{Linger: issue.Generation}, time.Now()))

		if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(checker)) || removed {
			t.Fatalf("the workspace removal = %v (removed %t), want a wait naming the checker's claim %s and the workspace kept", err, removed, checker)
		}
	})
}
