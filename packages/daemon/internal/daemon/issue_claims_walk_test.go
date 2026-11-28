package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// agentClaimToken is the claim token of the agent named agent on issue, in the project legion.
func agentClaimToken(t *testing.T, issue string, agent claim.Agent) claim.Token {
	t.Helper()
	token, err := claim.AgentToken("legion", issue, agent)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A child carried into a tree of its own waits, before its start in the new tree, for every claim
// of it that still runs in the old tree: the claim of an agent whose name is no role,
// `…-implementer-2`, as much as the claim `…-implementer`. A walk that reads the issue's claims by
// the six roles' tokens finds `…-implementer` alone, and the start would go on while
// `…-implementer-2` still works in the old tree's workspace.
func TestAStartWaitsForAnAgentWhoseNameIsNoRoleStillRunningInTheOldTree(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	parent := "LEGION-207"
	child := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
		Parent: &parent, Title: "child", Phase: phase.Step, Generation: 2, Status: "in_progress"}
	putOutboxIssue(t, pool, records, child)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	tokens := []claim.Token{agentClaimToken(t, child.Key, "implementer"), agentClaimToken(t, child.Key, "implementer-2")}
	for _, token := range tokens {
		_, agent, _ := token.Cut()
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: parent, Issue: child.Key, Role: claim.RoleImplementer, Generation: 2,
			State: supervise.StateWorking, Session: "ses_" + string(agent), SessionFile: "/sessions/" + string(agent) + ".jsonl",
			Pending: &supervise.Delivery{ID: "outbox:" + string(agent), Task: "Implement it.", Generation: 2, Phase: phase.Step, ConfirmedAt: time.Now()}}, ""); err != nil {
			t.Fatalf("create the claim %s in the old tree: %v", token, err)
		}
	}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now}

	err := runner.stopElsewhere(ctx, child)

	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("stopElsewhere = %v, want a wait for the claims still in tree %s", err, parent)
	}
	for _, token := range tokens {
		if !strings.Contains(err.Error(), string(token)+":") {
			t.Errorf("stopElsewhere = %v, want the wait to name the claim %s, still in tree %s", err, token, parent)
		}
	}
}

// A workspace removal waits for the tree's close to retire each claim of the issue, the claim of
// an agent whose name is no role among them: with `…-implementer` retired and `…-implementer-2`
// still suspended, the removal waits, naming `…-implementer-2`, and keeps the workspace. A walk
// that reads the issue's claims by the six roles' tokens finds only the retired `…-implementer`
// and removes the workspace under `…-implementer-2`, which a re-admission resumes in it.
func TestTheWorkspaceRemovalWaitsForEachClaim(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	until := time.Now().Add(-time.Minute)
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
		Title: "Workflow", Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	implementer, second := agentClaimToken(t, issue.Key, "implementer"), agentClaimToken(t, issue.Key, "implementer-2")
	for _, held := range []supervise.Claim{
		{Token: implementer, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, Generation: 2, State: supervise.StateRetired},
		{Token: second, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, Generation: 2,
			State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon},
	} {
		if _, _, err := sup.Create(ctx, held, ""); err != nil {
			t.Fatalf("create the claim %s: %v", held.Token, err)
		}
	}
	removed := false
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, project: "legion", log: quietLogger(), stateDir: t.TempDir(),
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { removed = true; return nil }}

	err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.WorkspaceRemove{Linger: issue.Generation}, time.Now()))

	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(second)+" ") || removed {
		t.Fatalf("the workspace removal = %v (removed %t), want a wait naming the claim %s and the workspace kept", err, removed, second)
	}
}
