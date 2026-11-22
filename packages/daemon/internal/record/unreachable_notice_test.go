package record

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The notices R5 and R3 add are the controller's alone, and each is keyed as its rule says: the
// list of claims that have newly gone out of reach and the one that says Envoy lists nobody speak
// for the project, so one wake carries whatever a pass finds, while every other kind speaks for one
// claim and is one wake per claim.
func TestTheUnreachableNoticesAreTheControllersAndKeyedPerClaim(t *testing.T) {
	for kind, perClaim := range map[NoticeKind]bool{
		UnreachableClaimsNotice:  false,
		EnvoyNoLiveSessionNotice: false,
		RelaunchReadyNotice:      true,
		RelaunchGuardHeldNotice:  true,
		RelaunchNotBackNotice:    true,
		UnreachableExitNotice:    true,
		WorkerLockedOutNotice:    true,
	} {
		if !controllerOnlyNoticeKind(kind) {
			t.Errorf("%s is not a controller-only notice kind", kind)
		}
		if validNoticeKind(kind) {
			t.Errorf("%s is an architect's notice kind as well; these go to the controller alone", kind)
		}
		if NoticePerClaim(kind) != perClaim {
			t.Errorf("NoticePerClaim(%s) = %v, want %v", kind, NoticePerClaim(kind), perClaim)
		}
	}
}

// The tree's architect is told of each unreachable claim of its tree but its own, so that kind is
// an architect's notice and not the controller's.
func TestTheArchitectsUnreachableNoticeIsAnArchitectsKind(t *testing.T) {
	if !validNoticeKind(ClaimOutOfReachNotice) {
		t.Errorf("%s is not an architect's notice kind", ClaimOutOfReachNotice)
	}
	if controllerOnlyNoticeKind(ClaimOutOfReachNotice) {
		t.Errorf("%s is the controller's alone; the tree's architect is told of its own tree's claims", ClaimOutOfReachNotice)
	}
}

func TestAnUnreachableNoticeIsAValidOutboxPayload(t *testing.T) {
	at := time.Date(2026, 11, 12, 15, 51, 1, 0, time.UTC)
	for _, payload := range []OutboxPayload{
		ControllerNotice{Kind: UnreachableClaimsNotice, Unreachable: []UnreachableClaim{
			{Claim: "legion-LEGION-836-implementer", Issue: "LEGION-836", Role: claim.RoleImplementer, State: "working", LastSeen: at},
		}},
		ControllerNotice{Kind: EnvoyNoLiveSessionNotice, Reason: "Envoy has listed no live session for 20 minutes"},
		ControllerNotice{Kind: RelaunchReadyNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "its background job has ended, so relaunch would now reach it"},
		ControllerNotice{Kind: RelaunchGuardHeldNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "the guard has refused its relaunch for 60 minutes; this is a case for a person"},
		ControllerNotice{Kind: RelaunchNotBackNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "it is still unreachable 20 minutes after its relaunch; this is a case for a person"},
		ControllerNotice{Kind: UnreachableExitNotice, Role: claim.RolePlanner, Claim: "legion-LEGION-836-planner",
			Reason: "its process exits at its Envoy role claim; this is a case for a person"},
		ControllerNotice{Kind: WorkerLockedOutNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "generation 7 has been refused the worker stream for 5 minutes; generation 6 is still connected"},
		Notice{Kind: ClaimOutOfReachNotice, Role: claim.RoleImplementer, Claim: "legion-LEGION-836-implementer",
			Reason: "its session's last registration with Envoy is more than 20 minutes old"},
	} {
		if _, err := NewOutboxRow("LEGION-836", payload, at); err != nil {
			t.Errorf("NewOutboxRow(%#v): %v", payload, err)
		}
	}
}

// The claim token and the list of unreachable claims ride on the kinds that carry them and on no
// other, as every other field of a notice does.
func TestOnlyTheUnreachableNoticesCarryAClaimOrAList(t *testing.T) {
	at := time.Date(2026, 11, 12, 15, 51, 1, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		payload OutboxPayload
		reason  string
	}{
		{name: "a claim token on a tick", payload: ControllerNotice{Kind: TickNotice, Claim: "legion-LEGION-836-implementer"},
			reason: "names a claim"},
		{name: "a claim token on a phase-finished notice", payload: Notice{Kind: "phase-finished", Claim: "legion-LEGION-836-implementer"},
			reason: "names a claim"},
		{name: "an unreachable list on a tick", payload: ControllerNotice{Kind: TickNotice, Unreachable: []UnreachableClaim{{Claim: "c"}}},
			reason: "lists unreachable claims"},
		{name: "an unreachable-claims notice with no list", payload: ControllerNotice{Kind: UnreachableClaimsNotice},
			reason: "lists at least one claim"},
		{name: "an unreachable claim with no claim token", payload: ControllerNotice{Kind: UnreachableClaimsNotice,
			Unreachable: []UnreachableClaim{{Issue: "LEGION-836", Role: claim.RoleImplementer}}}, reason: "names its claim"},
		{name: "a per-claim unreachable notice with no claim token", payload: ControllerNotice{Kind: RelaunchReadyNotice,
			Role: claim.RoleImplementer, Reason: "its job ended"}, reason: "names the claim it speaks for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOutboxRow("LEGION-836", tc.payload, at)
			if err == nil {
				t.Fatalf("NewOutboxRow(%#v) was accepted, want a refusal naming %q", tc.payload, tc.reason)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow(%#v) refused with %q, want it to name %q", tc.payload, err, tc.reason)
			}
		})
	}
}
