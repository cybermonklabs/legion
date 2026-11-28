package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The branch-refused entry's rows of this package's test tables (entryTestRows): a state body
// holding an issue whose branch row GitHub refused for good, carrying every key of the
// `branchRefused` it adds, and that key carried into narrowingState, so the entry's band finds it
// there and writes its own line.
func init() {
	registerEntryTestRows(entryTestRows{
		entry:  branchRefusedContract,
		states: func(*testing.T) []State { return []State{branchRefusedState()} },
		carry: func(state State) State {
			// The first issue of the golden, in key order, carries the refusal, so the band of this
			// entry finds a branchRefused to take there.
			issues := maps.Clone(state.Issues)
			keys := slices.Sorted(maps.Keys(issues))
			if len(keys) > 0 {
				issue := issues[keys[0]]
				issue.BranchRefused = branchRefusedState().Issues["LEGION-208"].BranchRefused
				issues[keys[0]] = issue
			}
			state.Issues = issues
			return state
		},
	})
}

// branchRefusedState is a state holding an issue whose branch row GitHub refused for good, with
// every key of its branchRefused, and an issue at work that holds no refusal.
func branchRefusedState() State {
	at := time.Date(2026, 11, 27, 21, 30, 0, 0, time.UTC)
	return State{
		Daemon: DaemonInfo{Project: "legion", SchemaVersion: 1, Boots: 1, FirstBootAt: at, StartedAt: at},
		Issues: map[string]Issue{
			"LEGION-208": {Key: "LEGION-208", Generation: 2, Phase: phase.Admitted, Status: "in_progress", Tree: "LEGION-208",
				Repo: "acme/widgets", DefaultBranch: "main", Workers: map[claim.Role]PhaseView{},
				BranchRefused: &BranchRefusedView{Row: 121, Write: "move", Kind: "no-node-id",
					Reason: "move the branch of LEGION-208 to main for its next change: read the node id of acme/widgets: GitHub named none", At: at}},
			"LEGION-209": {Key: "LEGION-209", Generation: 1, Phase: phase.Implementing, Status: "in_progress", Tree: "LEGION-209",
				Repo: "acme/widgets", DefaultBranch: "main", Workers: map[claim.Role]PhaseView{}},
		},
	}
}

// A caller below the branch-refused entry did not ask for `branchRefused`: its band serves every
// issue without it (withoutBranchRefused), to a caller stating a contract below the entry and to
// one from a pod whose claims record one, on the record route as on the state. A caller at the
// entry's contract, and one stating nothing from no pod, reads it as the source holds it, and the
// source is left as it was.
func TestAnOlderContractSeesNoBranchRefused(t *testing.T) {
	state := branchRefusedState()
	const operator = "192.0.2.1:41000"
	for _, tc := range []struct {
		name    string
		remote  string
		claims  []supervise.Claim
		stated  int
		carried bool
	}{
		{name: "a caller stating the contract just below", remote: operator, claims: podClaims(DaemonAPIVersion()),
			stated: entryNumber(branchRefusedContract) - 1},
		{name: "a pod at pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract)},
		{name: "a caller stating the contract", remote: operator, claims: podClaims(pauseContract),
			stated: entryNumber(branchRefusedContract), carried: true},
		{name: "a caller in no pod stating nothing", remote: operator, claims: podClaims(pauseContract), carried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := stateServer(t, state, tc.claims)
			var served struct {
				Issues map[string]map[string]json.RawMessage `json:"issues"`
			}
			if err := json.Unmarshal(stateStating(t, handler, tc.remote, tc.stated), &served); err != nil {
				t.Fatalf("decode the state: %v", err)
			}
			if _, carried := served.Issues["LEGION-208"]["branchRefused"]; carried != tc.carried {
				t.Errorf("LEGION-208 carries branchRefused = %v, want %v: %s", carried, tc.carried, served.Issues["LEGION-208"]["branchRefused"])
			}
			if refused, carried := served.Issues["LEGION-209"]["branchRefused"]; carried {
				t.Errorf("LEGION-209 carries branchRefused %s, want none", refused)
			}

			recorded := asked(t, handler, tc.remote, "/legion/v1/issues/LEGION-208/record", tc.stated)
			if recorded.Code != http.StatusOK {
				t.Fatalf("the record of LEGION-208 = %d, want 200; body %s", recorded.Code, recorded.Body)
			}
			var issue map[string]json.RawMessage
			if err := json.Unmarshal(recorded.Body.Bytes(), &issue); err != nil {
				t.Fatalf("decode the record %s: %v", recorded.Body, err)
			}
			if _, carried := issue["branchRefused"]; carried != tc.carried {
				t.Errorf("the record of LEGION-208 carries branchRefused = %v, want %v", carried, tc.carried)
			}
		})
	}
	if got := state.Issues["LEGION-208"].BranchRefused; got == nil {
		t.Error("the source issue lost its branchRefused, want it left as it was")
	}
}
