package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A step of a task whose branch is not the issue's own waits while the issue's pod runs an image
// other than the one the daemon launches: a shim of an earlier release ignores the branch an
// adoption names and adopts in the issue's own working copy, so the second task's agent would work
// in the first task's copy. The start of such a step waits on its row: it makes no claim, asks the
// runtime for nothing and adopts nothing, and its text begins with pods.StartWaitsForPodPrefix, so
// the idle view, the release's boundary and the served state read it as every other wait for the
// pod. The start of a step on the issue's own branch starts in that pod. Once the pod's update
// round replaces it, the waiting row starts, and the delivery its claim's adoption reads names the
// task's branch.
func TestAStepOfASecondTaskWaitsForAPodOfAnEarlierImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.store = outboxTreeStore(t, pool, issue.Key)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	rt.ScriptIssuePod(issue.Key, "pod-1")
	rt.ScriptPodImage("pod-1", true)
	agent := func(name claim.Agent) claim.Token {
		t.Helper()
		token, err := claim.AgentToken("legion", issue.Key, name)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	first, second := agent("k1"), agent("k2")
	const branch = "legion/LEGION-208.b"
	start := func(id int64, token claim.Token, branch string) record.OutboxRow {
		row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Claim: token,
			Task: "Implement it.", Generation: issue.Generation, Phase: phase.Implementing, Branch: branch}, time.Now())
		row.ID = id
		return row
	}

	err := runner.execute(ctx, start(41, second, branch))
	if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the start of the second task's step = %v, want a wait that is runtime.ErrAwaitsPodUpdate", err)
	}
	if !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) || !strings.Contains(err.Error(), "pod-1") {
		t.Errorf("the wait reads %q, want it to begin %q and name the pod", err, pods.StartWaitsForPodPrefix)
	}
	if _, found := sup.Machine(second); found {
		t.Errorf("the start made the claim of k2, want none while it waits")
	}
	if calls := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")) + len(rt.CallsOf("AdoptWorkingCopy")); calls > 0 {
		t.Errorf("the runtime was asked %d spawns, resumes and adoptions, want none while the start waits", calls)
	}

	if err := runner.execute(ctx, start(42, first, "")); err != nil {
		t.Fatalf("the start of the step on the issue's own branch = %v, want it to start in the pod", err)
	}
	if _, found := sup.Machine(first); !found {
		t.Fatalf("the start of the step on the issue's own branch made no claim")
	}

	if err := rt.ReplaceIssuePod(ctx, issue.Key, "pod-1", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("replace the pod as its update round does: %v", err)
	}
	if err := runner.execute(ctx, start(41, second, branch)); err != nil {
		t.Fatalf("the waiting start once the pod is replaced = %v, want it to start", err)
	}
	machine, found := sup.Machine(second)
	if !found {
		t.Fatalf("the waiting start made no claim once the pod was replaced")
	}
	testwait.Eventually(t, "the delivery of k2's task", func() bool {
		pending := machine.Claim().Pending
		return pending != nil && pending.Branch == branch
	})
}
