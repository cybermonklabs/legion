package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A caller below FixBesideReviewContract is served no `fix`, on the state route and on the
// record route alike, since one function shapes the issue for both (shapeIssue): no plugin before
// the contract completes a fix, because the daemon relaunches each claim of an older plugin on
// this contract at its boot (refreshStalePlugins), and the records of its contract's shape name no
// such key. A caller at the contract is served the fix whole, so the plugin of this release finds
// the fix's directory and its two entries; that half is the control, which fails where the band
// takes the key from every caller.
func TestACallerBelowTheFixContractIsServedNoFix(t *testing.T) {
	const key = "LEGION-208"
	state := populatedState(t)
	issue := state.Issues[key]
	issue.Fix = &FixView{Task: "a", Step: 1, Check: "unit", Head: "c0de0000000000000000000000000000000000ff",
		Dir: "/var/lib/legion/workspaces/acme/widgets/LEGION-208-fix", Entry: 4, GivenEntry: 3}
	state.Issues[key] = issue
	want := map[string]any{
		"task": "a", "step": float64(1), "check": "unit", "head": "c0de0000000000000000000000000000000000ff",
		"dir": "/var/lib/legion/workspaces/acme/widgets/LEGION-208-fix", "entry": float64(4), "givenEntry": float64(3),
	}
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))
	fix := entryNumber(FixBesideReviewContract)

	for _, tc := range []struct {
		name     string
		contract int
		served   bool
	}{
		{name: "a caller below the contract", contract: fix - 1},
		{name: "a caller at the contract", contract: fix, served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var record map[string]any
			if err := json.Unmarshal(recordOf(t, handler, "192.0.2.1:41000", key, tc.contract), &record); err != nil {
				t.Fatalf("decode the record of %s: %v", key, err)
			}
			for route, view := range map[string]map[string]any{
				"state":  issueViews(t, stateStating(t, handler, "192.0.2.1:41000", tc.contract))[key],
				"record": record,
			} {
				fix, carried := view["fix"]
				if carried != tc.served {
					t.Fatalf("the %s route serves a caller at %d %s with fix %v (carried %t), want carried %t",
						route, tc.contract, key, fix, carried, tc.served)
				}
				if tc.served && !reflect.DeepEqual(fix, want) {
					t.Errorf("the %s route serves a caller at %d the fix %v, want %v", route, tc.contract, fix, want)
				}
			}
		})
	}
	if state.Issues[key].Fix == nil {
		t.Errorf("shaping the answer for a caller below the contract took the fix off the state the source answered")
	}
}
