package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// lowestServed is the `lowestPluginContract` of a state body, and whether the body carries the
// key at all: the two are read apart because no number is reported where no claim holds a
// process, and an absent key is the report.
func lowestServed(t *testing.T, body []byte) (lowest int, carried bool) {
	t.Helper()
	var state struct {
		LowestPluginContract *int `json:"lowestPluginContract"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	if state.LowestPluginContract == nil {
		return 0, false
	}
	return *state.LowestPluginContract, true
}

// claimWithoutLocator is a claim in the state the supervisor stores it in, holding no process:
// suspending, failing or retiring a claim ends what the machine had with its process and the
// claim gives up its locator with it (supervise's letGo), so none of these records a pod.
func claimWithoutLocator(token claim.Token, state supervise.ClaimState, contract int) supervise.Claim {
	return supervise.Claim{Token: token, State: state, PluginContract: contract}
}

// The state reports the lowest contract among the claims that hold a process, over every pod of
// the project: that is the number which says which shapes the serving-down must still cover, and
// the one to read before the floor is raised.
//
// The fixture holds claims at several contracts in two pods, one running claim with no locator
// at all, one that records no contract, and a suspended, a failed and a retired claim at
// contracts below every running one. None of the last four lowers the number: a claim that holds
// no process calls nothing, so no shape is owed to it, and a claim that records no contract says
// nothing about any shape. The route reads the claims of its own snapshot, so this runs from the
// claim to the body it is served in.
func TestTheStateReportsTheLowestContractOfTheClaimsThatHoldAProcess(t *testing.T) {
	const (
		otherPod = "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34"
		lowest   = 48
	)
	claims := []supervise.Claim{
		claimInPod("legion-legion-legion-208-planner", statePodUID, 52),
		claimInPod("legion-legion-legion-208-implementer", statePodUID, DaemonAPIVersion),
		// The lowest of the fleet, and in another pod than the caller's: the number is the
		// project's, not one pod's.
		claimInPod("legion-legion-legion-210-planner", otherPod, lowest),
		// Records no contract, so it moves nothing.
		claimInPod("legion-legion-legion-210-tester", otherPod, 0),
		// Running and placed in no pod — a claim of the tmux runtime — so it names no pod and is
		// in no reckoning here.
		{Token: "legion-legion-legion-211-architect", State: supervise.StateReady, PluginContract: 46, Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: "legion-legion-legion-211-architect", Incarnation: "31881:9442101",
			Tmux: &runtime.TmuxLocator{Window: "@4", Pane: "%12"},
		}},
		claimWithoutLocator("legion-legion-legion-207-tester", supervise.StateSuspended, 36),
		claimWithoutLocator("legion-legion-legion-207-reviewer", supervise.StateFailed, 38),
		claimWithoutLocator("legion-legion-legion-207-merger", supervise.StateRetired, 41),
	}

	reported, carried := lowestServed(t, stateFrom(t, populatedState(t), "192.0.2.1:41000", claims))

	if !carried {
		t.Fatal("the state reports no lowest plugin contract, want the lowest of the claims that hold a process")
	}
	if reported != lowest {
		t.Errorf("the lowest plugin contract = %d, want %d: a suspended, failed or retired claim holds no process and lowers it by nothing, and a claim recording none moves it either way",
			reported, lowest)
	}
}

// A project no claim of which holds a process reports no number at all, rather than a zero that
// reads as a contract: the key is absent, and a reader learns that nothing runs rather than that
// something runs at contract 0. The claims here record contracts below every number this daemon
// shapes for, so a report would be loud.
func TestTheStateReportsNoLowestContractWhereNoClaimHoldsAProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []supervise.Claim
	}{
		{name: "no claim at all"},
		{name: "every claim stopped", claims: []supervise.Claim{
			claimWithoutLocator("legion-legion-legion-207-tester", supervise.StateSuspended, 36),
			claimWithoutLocator("legion-legion-legion-207-reviewer", supervise.StateFailed, 38),
			claimWithoutLocator("legion-legion-legion-207-merger", supervise.StateRetired, 41),
		}},
		{name: "a running claim recording no contract", claims: []supervise.Claim{
			claimInPod("legion-legion-legion-208-planner", statePodUID, 0),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), "192.0.2.1:41000", tc.claims)

			if reported, carried := lowestServed(t, body); carried {
				t.Errorf("the state reports a lowest plugin contract of %d, want no key: no claim of this project holds a process at a recorded contract", reported)
			}
		})
	}
}

// The number is the served body's, read from the claims of the snapshot the body was projected
// from, and not a value the source carried: the fixture's own value is the floor, and a project
// whose claims record something else is reported as its claims read.
func TestTheStateReportsTheSnapshotsOwnClaimsAndNotTheFixturesValue(t *testing.T) {
	fixture := populatedState(t)
	if fixture.LowestPluginContract == nil || *fixture.LowestPluginContract != ServingFloor {
		t.Fatalf("the fixture's lowest plugin contract is %v, want the floor; this proves nothing", fixture.LowestPluginContract)
	}
	server := NewServer("127.0.0.1", 8437, Options{
		Steps: config.DesignGateRootIssues.Steps(),
		State: fakeSource{state: fixture, claims: podClaims(DaemonAPIVersion)}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
	})
	recorder := httptest.NewRecorder()

	server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))

	reported, carried := lowestServed(t, recorder.Body.Bytes())
	if !carried || reported != DaemonAPIVersion {
		t.Errorf("the lowest plugin contract = %d (carried %v), want the %d every claim of the snapshot records", reported, carried, DaemonAPIVersion)
	}
}
