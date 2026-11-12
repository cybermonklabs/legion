package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (populatedState) carries one leak-check cause: LEGION-210's
// failed implementer, whose launch tries the runtime refused because the shared public leak
// check could not be installed in its pod.

// leakCheckCausesServed is the `leakCheckCause` of every claim view of a state body that carries
// one, and how many views the body holds: the two are read apart so a body that carries no claim
// view at all cannot pass for one whose causes were stripped.
func leakCheckCausesServed(t *testing.T, body []byte) (causes []string, views int) {
	t.Helper()
	for _, view := range claimViews(t, body) {
		views++
		cause, carried := view["leakCheckCause"]
		if !carried {
			continue
		}
		sentence, ok := cause.(string)
		if !ok {
			t.Fatalf("a claim view of %s carries a leakCheckCause that is no string: %v", body, cause)
		}
		causes = append(causes, sentence)
	}
	return causes, views
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below leakCheckCauseContract is served claim views with no
// `leakCheckCause`, since that pod's plugin did not ask for it, and one built before
// sjawhar/legion#2788 reads the state with a schema that knows no such key and refuses the whole
// body for it. A pod at that contract, a pod whose claims record no contract at all, and a caller
// the daemon cannot place in a pod — the operator's `legion state` — are served it.
func TestTheStateRouteServesAPodItsPluginContractsLeakCheckCause(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(leakCheckCauseContract))
	wantCauses, wantViews := leakCheckCausesServed(t, full)
	if len(wantCauses) == 0 || wantViews == 0 {
		t.Fatalf("the fixture's state %s carries no claim view with a leak-check cause; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		cause  bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "a pod at leakCheckCauseContract", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract), cause: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), cause: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(leakCheckCauseContract - 1), cause: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)
			causes, views := leakCheckCausesServed(t, body)
			if views != wantViews {
				t.Fatalf("the state %s carries %d claim views, want the fixture's %d", body, views, wantViews)
			}
			if !tc.cause {
				if len(causes) > 0 {
					t.Errorf("the state %s carries the leak-check causes %q; the reader before leakCheckCauseContract did not ask for the key", body, causes)
				}
				return
			}
			if strings.Join(causes, "\n") != strings.Join(wantCauses, "\n") {
				t.Errorf("the state %s carries the leak-check causes %q, want the fixture's %q", body, causes, wantCauses)
			}
		})
	}
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// that contract, is served the state as the source answered it, its cause and all.
func TestTheStateServedToAPodBeforeTheLeakCheckCauseContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(leakCheckCauseContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if causes, _ := leakCheckCausesServed(t, behind); len(causes) > 0 {
		t.Fatalf("the pod before the contract was served the causes %q, want none: %s", causes, behind)
	}
	if causes, _ := leakCheckCausesServed(t, operator); len(causes) == 0 {
		t.Fatalf("the operator after it was served %s, want the cause the source answered", operator)
	}
}

// leakCheckCauseContract is the contract whose entry in version.go added the key, and it is this
// daemon's own: a caller at this contract is served it. A constant above DaemonAPIVersion would be
// a key no pod is ever served; one below it would serve the key to a plugin that never asked.
func TestTheLeakCheckCauseContractIsThisDaemonsContract(t *testing.T) {
	const addedLeakCheckCause = 87
	if leakCheckCauseContract != addedLeakCheckCause {
		t.Fatalf("leakCheckCauseContract = %d, want %d, the contract whose entry in version.go adds the claim views' `leakCheckCause`",
			leakCheckCauseContract, addedLeakCheckCause)
	}
	if leakCheckCauseContract != DaemonAPIVersion {
		t.Fatalf("leakCheckCauseContract = %d, DaemonAPIVersion %d: the key is this release's own",
			leakCheckCauseContract, DaemonAPIVersion)
	}
}

// The cause reaches both shapes a claim is served in. The controller's claim is on no claim view
// of the state — internal/projection puts it on no issue — so the operator routes, which
// `legion claims list` reads, are where its cause is read; every other claim carries it on its
// claim view as well.
func TestBothClaimShapesCarryTheLeakCheckCause(t *testing.T) {
	const sentence = "the shared public leak check could not be installed: the pod's launcher could not unpack the check the daemon served: unexpected EOF"
	controller := supervise.Claim{
		Token: claim.ControllerToken("legion"), Role: claim.RoleController,
		State: supervise.StateLaunching, Budgets: supervise.Budgets{LaunchFailures: 1},
		LeakCheckCause: sentence,
	}

	encoded, err := json.Marshal(OperatorClaims{Claims: []OperatorClaim{operatorView(controller, supervise.StepRecord{})}})
	if err != nil {
		t.Fatalf("marshal the operator claim list: %v", err)
	}
	var list struct {
		Claims []map[string]any `json:"claims"`
	}
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatalf("read the operator claim list back: %v", err)
	}
	if len(list.Claims) != 1 || list.Claims[0]["leakCheckCause"] != sentence {
		t.Errorf("the operator claim list %s carries no leakCheckCause %q", encoded, sentence)
	}

	worker := controller
	worker.Token, worker.Role = "legion-legion-legion-209-implementer", claim.RoleImplementer
	if view := ClaimViewOf(worker); view.LeakCheckCause != sentence {
		t.Errorf("ClaimViewOf(the refused claim).LeakCheckCause = %q, want %q", view.LeakCheckCause, sentence)
	}
}

// A claim no leak check refused carries no key at all, on either surface: the key is absent
// rather than an empty string, which is what the schemas on the other side of this wire read it
// as and what they refuse.
func TestAClaimTheLeakCheckDidNotRefuseCarriesNoCause(t *testing.T) {
	c := supervise.Claim{Token: "legion-legion-legion-209-implementer", Role: claim.RoleImplementer, State: supervise.StateReady}
	for name, value := range map[string]any{
		"the claim view":     ClaimViewOf(c),
		"the operator claim": operatorView(c, supervise.StepRecord{}),
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if strings.Contains(string(encoded), "leakCheckCause") {
			t.Errorf("%s of a claim the leak check did not refuse = %s, want no leakCheckCause key", name, encoded)
		}
	}
}
