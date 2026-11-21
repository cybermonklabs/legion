package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// TestAPRIVAndAPUBCopyOfOneRoleKeepSeparateLanes: a role route no holder takes keeps its copy on
// the lane of its claim key, (project, role): a PRIV copy and a PUB copy of one free-form role keep
// separate lanes, PRIV.reviewer and PUB.reviewer, as the listener keys its own keep by the role
// topic; a bare role keeps its bare key, and a Legion role its own. A role no claim can name is
// skipped. Control (a mutant, run apart): the lane keyed by the role's name alone gives both copies
// the lane "reviewer", and the separate-lanes check below fails naming it.
func TestAPRIVAndAPUBCopyOfOneRoleKeepSeparateLanes(t *testing.T) {
	unclaimed := roles.Resolution{State: roles.Unclaimed}
	keptFor := func(role string) string {
		return forwardOf(role, contracts.Envelope{}, unclaimed, nil, time.Time{}).keptFor
	}
	priv := keptFor(contracts.RoleClaimKey("PRIV", "reviewer"))
	pub := keptFor(contracts.RoleClaimKey("PUB", "reviewer"))
	if priv != "PRIV.reviewer" || pub != "PUB.reviewer" {
		t.Errorf("a PRIV and a PUB copy of reviewer keep lanes %q and %q, want PRIV.reviewer and PUB.reviewer", priv, pub)
	}
	if bare := keptFor("reviewer"); bare != "reviewer" {
		t.Errorf("a bare role's copy keeps lane %q, want reviewer, as today", bare)
	}
	if legion := keptFor("legion-infra-infra-12-architect"); legion != "legion-infra-infra-12-architect" {
		t.Errorf("a Legion role's copy keeps lane %q, want its own key", legion)
	}
	if skipped := forwardOf("Not A Role", contracts.Envelope{}, unclaimed, nil, time.Time{}); skipped.keptFor != "" || skipped.skip == "" {
		t.Errorf("a role no claim can name was kept for %q, want it skipped", skipped.keptFor)
	}
	// The two copies keep two lanes, each naming its own project and the role: keyed by the role's
	// name alone, both would be "reviewer", one lane for both copies.
	if priv == pub {
		t.Fatalf("the PRIV and PUB copies of reviewer share the lane %q", priv)
	}
	for lane, want := range map[string]string{priv: "PRIV", pub: "PUB"} {
		if project, name := contracts.ParseRoleClaimKey(lane); project != want || name != "reviewer" {
			t.Errorf("lane %q reads as project %q and role %q, want %s and reviewer", lane, project, name, want)
		}
	}
}
