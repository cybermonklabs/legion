package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roundReportKeys is the two keys shapedAnswersContract added to each claim view, as the wire
// names them.
var roundReportKeys = []string{"updateState", "wake"}

// roundReportServed is how many claim views of a state body carry each of those two keys, and
// how many views the body holds: the two are read apart so a body that carries no claim view at
// all cannot pass for one whose keys were stripped.
func roundReportServed(t *testing.T, body []byte) (carried map[string]int, views int) {
	t.Helper()
	carried = map[string]int{}
	for _, view := range claimViews(t, body) {
		views++
		for _, key := range roundReportKeys {
			if _, holds := view[key]; holds {
				carried[key]++
			}
		}
	}
	return carried, views
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from, and a
// caller that states its own contract is placed by what it states. A caller below
// shapedAnswersContract is served neither part this contract added: no `lowestPluginContract` on
// the body, and no claim view carrying `updateState` or `wake`. That caller's plugin reads the
// state with a schema that names none of the three keys and refuses the whole body for one of
// them. A caller at this daemon's own contract, a pod whose claims record no contract, and a
// caller the daemon cannot place in a pod are served both.
func TestTheStateRouteServesAPodItsPluginContractsRoundReport(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion))
	wantCarried, wantViews := roundReportServed(t, full)
	if _, carried := lowestServed(t, full); !carried {
		t.Fatalf("the fixture's state %s reports no lowest plugin contract; this test proves nothing", full)
	}
	for _, key := range roundReportKeys {
		if wantCarried[key] == 0 {
			t.Fatalf("no claim view of the fixture's state %s carries %q; this test proves nothing", full, key)
		}
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		served bool
	}{
		{name: "a pod at the floor", remote: statePodAddr + ":41000", claims: podClaims(ServingFloor)},
		{name: "a caller that states the floor", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion), stated: ServingFloor},
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(shapedAnswersContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(shapedAnswersContract - 1)},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion), served: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), served: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(shapedAnswersContract - 1), served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := stateServer(t, populatedState(t), tc.claims)
			body := stateStating(t, handler, tc.remote, tc.stated)
			carried, views := roundReportServed(t, body)
			if views != wantViews {
				t.Fatalf("the state %s carries %d claim views, want the fixture's %d", body, views, wantViews)
			}
			if _, reports := lowestServed(t, body); reports != tc.served {
				t.Errorf("the state %s reports a lowest plugin contract = %v, want %v", body, reports, tc.served)
			}
			for _, key := range roundReportKeys {
				want := 0
				if tc.served {
					want = wantCarried[key]
				}
				if carried[key] != want {
					t.Errorf("%d claim views of %s carry %q, want %d", carried[key], body, key, want)
				}
			}
		})
	}
}

// The body a pod below the contract is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, the round report and the fleet's
// lowest contract and all.
func TestTheStateServedBelowThisContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(shapedAnswersContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if carried, _ := roundReportServed(t, behind); len(carried) > 0 {
		t.Fatalf("the pod below the contract was served %v, want no claim view carrying one: %s", carried, behind)
	}
	if _, reports := lowestServed(t, behind); reports {
		t.Fatalf("the pod below the contract was served a lowest plugin contract: %s", behind)
	}
	carried, _ := roundReportServed(t, operator)
	for _, key := range roundReportKeys {
		if carried[key] == 0 {
			t.Errorf("the caller after it was served no claim view carrying %q: %s", key, operator)
		}
	}
	if _, reports := lowestServed(t, operator); !reports {
		t.Fatalf("the caller after it was served no lowest plugin contract: %s", operator)
	}
}

// shapedAnswersContract is at or below this daemon's own contract, which is what makes the three
// keys of its entry reach a caller at all: a constant above DaemonAPIVersion would be keys no
// caller is ever served. Which number it is is answers.go's `Since` of this change's two new
// answers and version.go's own entry, each of them the constant itself, so nothing here pins it
// to a literal: a renumber moves one constant.
func TestTheShapedAnswersContractIsAContractThisDaemonServes(t *testing.T) {
	if shapedAnswersContract > DaemonAPIVersion {
		t.Fatalf("shapedAnswersContract = %d, above DaemonAPIVersion %d: no caller could be served the keys",
			shapedAnswersContract, DaemonAPIVersion)
	}
	if !slices.Contains(SupportedContracts, shapedAnswersContract) {
		t.Fatalf("shapedAnswersContract = %d, which SupportedContracts does not record: the contract that adds a key is a contract this daemon describes a shape for",
			shapedAnswersContract)
	}
}
