package daemon

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dispatch://INFRA-3583 R6: "The claims that the release stops are those whose role process runs or
// starts at the release." A claim whose start the wake pacing parked is launching with no process,
// and its issue has no pod of the project yet. The release stops it as it stops the others
// (podlessStops): the claim is stored suspended by the project release, and its line says that the
// next boot starts it. A release that stopped only the claims of the pods it found would leave the
// claim launching, and its pod would start once the pacing let the launch go.
func TestProjectReleaseStopsAClaimWhoseStartHasNoPodYet(t *testing.T) {
	t.Parallel()
	// parkTheWake leaves the implementer's claim on LEGION-208 launching with no process under the
	// start the wake pacing parked, the outbox's own wake of its released pod, with no pod of the
	// project standing.
	p := parkTheWake(t, firedHook, claim.RoleImplementer)
	sup, machine := p.runner.supervisor, p.machine
	token := machine.Claim().Token
	records := &releaseRecord{claims: sup.deps.Store.(*outboxClaimStore), finished: make(chan json.RawMessage, 1),
		// The stop set as the release's own transaction reads it: the claim is live, launching.
		stop: []store.ProjectReleaseClaim{{Token: token, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer,
			Session: "ses-" + string(claim.RoleImplementer), State: supervise.StateLaunching}}}
	result := runRelease(t, records, sup, &releasePods{})
	if len(result.Claims) != 1 || result.Claims[0].Token != token ||
		result.Claims[0].Ended != api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease) || !result.Claims[0].BootStarts || result.Claims[0].Detail != "" {
		t.Fatalf("the claim lines are %+v, want the implementer's ended project_release, which the next boot starts, with no detail", result.Claims)
	}
	if c := machine.Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByProjectRelease {
		t.Fatalf("after the release the implementer is %s by %q, want it stored suspended by the project release", c.State, c.SuspendedBy)
	}
}
