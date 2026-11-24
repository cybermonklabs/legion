package sandbox

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A release asks the shim of a live claim for the workspace's unpushed work, and names the claim
// by its token. Key a's agent works the tester's step in the claim it started as implementer, so
// the claim that runs in the pod is `…-implementer` with the stored role tester, and no claim
// `…-tester` runs at all: the bundle is asked of the claim named, over its own connection.
func TestABundleIsAskedOfTheClaimThatRuns(t *testing.T) {
	g := newRig(t, nil)
	keyed := claim.Token("legion-legion-legion-208-implementer")
	conn := fake.NewConn()
	conn.SetWorkspaceBundle(runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "abc"})
	g.conns.Register(keyed, conn)
	loc := g.spawn(testSpec(t, keyed, claim.RoleTester, testTree))
	g.store.mu.Lock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: keyed, Project: "legion", Tree: testTree, Issue: testTree,
		Role: claim.RoleTester, Locator: &loc})
	g.store.mu.Unlock()

	bundle, err := g.r.BundleWorkspace(g.ctx, testTree, keyed)
	if err != nil {
		t.Fatalf("bundle the workspace of the claim that runs: %v", err)
	}
	if string(bundle.Bundle) != "PACK" || bundle.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("the bundle = %+v, want the shim's PACK off pod %s", bundle, loc.Sandbox.PodUID)
	}
	if asked := conn.Bundlings(); len(asked) != 1 || asked[0].Issue != testTree {
		t.Fatalf("the claim's shim was asked %+v, want once for %s", asked, testTree)
	}
}
