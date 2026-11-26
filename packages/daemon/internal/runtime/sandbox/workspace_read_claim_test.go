package sandbox

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A workspace read uses the named claim's connection, not a claim derived from its current role.
// The keyed agent runs the tester's step in the claim it started as implementer; no tester token
// has a process or a connection.
func TestAKeyedClaimsWorkspaceReadUsesItsOwnConnection(t *testing.T) {
	g := newRig(t, nil)
	keyed := claim.Token("legion-legion-legion-208-implementer")
	conn := fake.NewConn()
	conn.SetWorkspaceWork(runtime.WorkspaceWork{Head: "abc", Count: 1})
	g.conns.Register(keyed, conn)
	loc := g.spawn(testSpec(t, keyed, claim.RoleTester, testTree))
	g.store.mu.Lock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: keyed, Project: "legion", Tree: testTree, Issue: testTree,
		Role: claim.RoleTester, Locator: &loc})
	g.store.mu.Unlock()

	work, err := g.r.WorkspaceWork(g.ctx, testTree, keyed)
	if err != nil {
		t.Fatalf("read the workspace of the claim that runs: %v", err)
	}
	if work.Head != "abc" || work.Count != 1 {
		t.Fatalf("the workspace reading = %+v, want the shim's abc and one commit", work)
	}
	if asked := conn.WorkspaceWorkReads(); len(asked) != 1 || asked[0].Issue != testTree {
		t.Fatalf("the claim's shim was asked %+v, want once for %s", asked, testTree)
	}
}
