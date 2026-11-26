package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pod can hold two live claims of one stored role: a keyed agent on its tester step, whose claim
// is the one it started on the implementer's step, and the fresh tester an earlier version of the
// plan started. The release suspends each live claim by its token, so the decision names both
// claims, in the order the view read them, each beside its role; a role names neither of them alone.
func TestAReleaseSuspendsEachLiveClaimByItsToken(t *testing.T) {
	const keyed claim.Token = "legion-legion-legion-209-implementer"
	const fresh claim.Token = "legion-legion-legion-209-tester"
	view := idleChild()
	view.Claims[0].Token, view.Claims[0].Role = keyed, claim.RoleTester
	second := view.Claims[0]
	second.Token = fresh
	view.Claims[1] = second
	view.Claims = append(view.Claims, view.Claims[1])
	view.Claims[2].Token, view.Claims[2].State = "legion-legion-legion-209-reviewer", supervise.StateSuspended
	view.Claims[2].Role, view.Claims[2].UpdatedAt = claim.RoleReviewer, idleNow.Add(-40*time.Minute)

	decision := IdleRelease(view, idleNow, idlePeriod, true, idleContract)
	if !decision.Release {
		t.Fatalf("the pod of two idle claims = %+v, want it released", decision)
	}
	want := []store.ReleasedClaim{{Token: keyed, Role: claim.RoleTester}, {Token: fresh, Role: claim.RoleTester}}
	if !slices.Equal(decision.Claims, want) {
		t.Fatalf("the claims the release suspends = %v, want %v: each live claim by its token beside its role, the suspended one left out", decision.Claims, want)
	}
}
