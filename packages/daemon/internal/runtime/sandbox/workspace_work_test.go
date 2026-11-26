package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The workspace read is asked of the shim of the named claim, over that claim's connection, and
// only where the claim's recorded process runs in the issue's own running pod: a claim with no
// process, one whose recorded pod is not the running one, and one with no connection are each a
// refusal naming why, never an answer of no work, and no shim is asked. The answer the shim gives
// is the runtime's, asked with the read's own budget.
func TestWorkspaceWorkAsksTheClaimOfTheRunningPod(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	stale := sandboxLocator(workerToken, "uid-pod-stale")
	stored := func(locator *runtime.Locator) {
		g.store.mu.Lock()
		defer g.store.mu.Unlock()
		g.store.claims = []supervise.Claim{{
			Token: workerToken, Project: testProject, Tree: testTree, Issue: testTree, Role: claim.RoleTester, Locator: locator,
		}}
	}
	conn := fake.NewConn()
	want := runtime.WorkspaceWork{Head: strings.Repeat("a", 40), Changed: true, Commits: []string{strings.Repeat("b", 40)}, Count: 1}
	conn.SetWorkspaceWork(want)

	for _, refusal := range []struct {
		name     string
		locator  *runtime.Locator
		register bool
		says     string
	}{
		{name: "a claim with no process", locator: nil, register: true, says: "holds no role process to read it"},
		{name: "a claim whose recorded pod is not the running one", locator: &stale, register: true, says: "is not in its running issue pod"},
		{name: "a claim with no connection", locator: &loc, register: false, says: "has no connection to ask over"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			stored(refusal.locator)
			g.conns.Remove(workerToken)
			if refusal.register {
				g.conns.Register(workerToken, conn)
			}
			got, err := g.r.WorkspaceWork(g.ctx, testTree, workerToken)
			if err == nil || !strings.Contains(err.Error(), refusal.says) {
				t.Fatalf("WorkspaceWork = %+v, %v; want a refusal saying %q", got, err, refusal.says)
			}
			if reads := conn.WorkspaceWorkReads(); len(reads) != 0 {
				t.Fatalf("the shim was asked %+v for a claim the runtime refuses", reads)
			}
		})
	}

	stored(&loc)
	g.conns.Register(workerToken, conn)
	got, err := g.r.WorkspaceWork(g.ctx, testTree, workerToken)
	if err != nil {
		t.Fatal(err)
	}
	if got.Head != want.Head || got.Changed != want.Changed || !slices.Equal(got.Commits, want.Commits) || got.Count != want.Count {
		t.Fatalf("WorkspaceWork = %+v, want the shim's reading %+v", got, want)
	}
	reads := conn.WorkspaceWorkReads()
	if len(reads) != 1 || reads[0].Issue != testTree || reads[0].Timeout != runtime.WorkspaceWorkTimeout {
		t.Fatalf("the shim was asked %+v, want one read of %s within %v", reads, testTree, runtime.WorkspaceWorkTimeout)
	}
}
