package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (startFailureState) carries one failed start with its moment,
// LEGION-215's implementer's, and one whose moment no release kept, its tester's.

// startFailureMomentsServed is the `lastStartFailureAt` of every claim view of a state body that
// carries one, and how many views the body holds: the two are read apart so a body that carries
// no claim view at all cannot pass for one whose moments were stripped.
func startFailureMomentsServed(t *testing.T, body []byte) (moments []string, views int) {
	t.Helper()
	for _, view := range claimViews(t, body) {
		views++
		moment, carried := view["lastStartFailureAt"]
		if !carried {
			continue
		}
		text, ok := moment.(string)
		if !ok {
			t.Fatalf("a claim view of %s carries a lastStartFailureAt that is no string: %v", body, moment)
		}
		moments = append(moments, text)
	}
	return moments, views
}

// A failed start reaches both shapes a claim is served in with the moment it failed, so a reader
// of either reads how old the failure is: a sentence with no moment read as current days after it
// was fixed. The claim view and the operator's claim carry the same instant.
func TestAFailedStartIsServedWithTheMomentItFailed(t *testing.T) {
	failedAt := time.Date(2026, 11, 16, 23, 12, 0, 0, time.UTC)
	c := supervise.Claim{
		Token: "legion-legion-legion-209-implementer", Role: claim.RoleImplementer, State: supervise.StateWorking,
		LastStartFailure:   "launch legion-legion-legion-209-implementer: start through its authenticated launcher: the pod never became ready",
		LastStartFailureAt: failedAt,
	}
	for name, value := range map[string]any{
		"the claim view":     ClaimViewOf(c),
		"the operator claim": operatorView(c, supervise.StepRecord{}),
	} {
		document := wireOf(t, value)
		if document["lastStartFailure"] != c.LastStartFailure || document["lastStartFailureAt"] != "2026-11-16T23:12:00Z" {
			t.Errorf("%s = %s, want the failed start %q with its moment 2026-11-16T23:12:00Z", name, canonicalOf(t, document), c.LastStartFailure)
		}
	}
}

// A failed start whose moment no release kept — every one recorded before the daemon kept it —
// is served with its sentence and no moment at all: the key is absent, which every reader takes
// as a moment unknown, and the daemon makes none up from the claim's ready or its last write.
func TestAFailedStartWithNoMomentIsServedWithoutOne(t *testing.T) {
	c := supervise.Claim{
		Token: "legion-legion-legion-209-implementer", Role: claim.RoleImplementer, State: supervise.StateWorking,
		LastStartFailure: "launch legion-legion-legion-209-implementer: start through its authenticated launcher: the pod never became ready",
		LastReady:        time.Date(2026, 11, 19, 17, 41, 54, 0, time.UTC),
	}
	for name, value := range map[string]any{
		"the claim view":     ClaimViewOf(c),
		"the operator claim": operatorView(c, supervise.StepRecord{}),
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if !strings.Contains(string(encoded), `"lastStartFailure"`) || strings.Contains(string(encoded), "lastStartFailureAt") {
			t.Errorf("%s of a failed start with no moment = %s, want the sentence and no lastStartFailureAt key", name, encoded)
		}
	}
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below lastStartFailureAtContract is served claim views with no
// `lastStartFailureAt`, since that pod's plugin reads the claim view with a schema that knows no
// such key. A pod at that contract, a pod whose claims record no contract at all, and a caller
// the daemon cannot place in a pod — the operator's `legion state` — are served it.
func TestTheStateRouteServesAPodItsPluginContractsStartFailureMoment(t *testing.T) {
	full := stateFrom(t, startFailureState(t), "192.0.2.1:41000", podClaims(lastStartFailureAtContract))
	wantMoments, wantViews := startFailureMomentsServed(t, full)
	if len(wantMoments) == 0 || wantViews == 0 {
		t.Fatalf("the fixture's state %s carries no claim view with a failed start's moment; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		moment bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(lastStartFailureAtContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "a pod at lastStartFailureAtContract", remote: statePodAddr + ":41000", claims: podClaims(lastStartFailureAtContract), moment: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), moment: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(lastStartFailureAtContract - 1), moment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, startFailureState(t), tc.remote, tc.claims)
			moments, views := startFailureMomentsServed(t, body)
			if views != wantViews {
				t.Fatalf("the state %s carries %d claim views, want the fixture's %d", body, views, wantViews)
			}
			// The strip takes the moment and nothing else: both failed starts' sentences stay on
			// every body, as the contracts before this one serve them.
			sentences := 0
			for _, view := range claimViews(t, body) {
				if _, carried := view["lastStartFailure"]; carried {
					sentences++
				}
			}
			if sentences != 2 {
				t.Errorf("the state %s carries %d failed starts' sentences, want the fixture's 2", body, sentences)
			}
			if !tc.moment {
				if len(moments) > 0 {
					t.Errorf("the state %s carries the failed starts' moments %q; the reader before lastStartFailureAtContract did not ask for the key", body, moments)
				}
				return
			}
			if strings.Join(moments, "\n") != strings.Join(wantMoments, "\n") {
				t.Errorf("the state %s carries the failed starts' moments %q, want the fixture's %q", body, moments, wantMoments)
			}
		})
	}
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// that contract, is served the state as the source answered it, the moment and all.
func TestTheStateServedToAPodBeforeTheStartFailureMomentContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, startFailureState(t), podClaims(lastStartFailureAtContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if moments, _ := startFailureMomentsServed(t, behind); len(moments) > 0 {
		t.Fatalf("the pod before the contract was served the moments %q, want none: %s", moments, behind)
	}
	if moments, _ := startFailureMomentsServed(t, operator); len(moments) == 0 {
		t.Fatalf("the operator after it was served %s, want the moment the source answered", operator)
	}
}

// lastStartFailureAtContract is the contract whose entry in version.go added the key, and this
// daemon's own contract is never below it: a caller at this contract is served it.
func TestTheStartFailureMomentContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedStartFailureMoment = 190
	if lastStartFailureAtContract != addedStartFailureMoment {
		t.Fatalf("lastStartFailureAtContract = %d, want %d, the contract whose entry in version.go adds the claim views' `lastStartFailureAt`",
			lastStartFailureAtContract, addedStartFailureMoment)
	}
	if lastStartFailureAtContract > DaemonAPIVersion {
		t.Fatalf("lastStartFailureAtContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			lastStartFailureAtContract, DaemonAPIVersion)
	}
}
