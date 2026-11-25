package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// stateKeys is what one contract's own state schema accepts, as testdata's
// state_keys_contract_<N>.json holds it: captured from the contract package of the commit that
// declared contract N (`git archive <commit> packages/contracts`, then a walk of its
// LegionStateResponse schema), by key path, "" for the root and "*" for a record's or an array's
// member. Each object says whether it is strict (a key it does not know refuses the whole state),
// its keys and its required keys; each enumerated field, the values it accepts.
type stateKeys struct {
	Contract int    `json:"contract"`
	Commit   string `json:"commit"`
	Objects  map[string]struct {
		Strict   bool     `json:"strict"`
		Keys     []string `json:"keys"`
		Required []string `json:"required"`
	} `json:"objects"`
	Enums map[string][]string `json:"enums"`
}

// fleetStateContracts is every contract the state's narrowing is held to here: the ones the
// fleet's plugins read on 2026-11-20, 144 and 152, and the oldest the state narrows for, the
// serving floor.
var fleetStateContracts = []int{ServingFloor, 144, 152}

func readStateKeys(t *testing.T, contract int) stateKeys {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("state_keys_contract_%d.json", contract)))
	if err != nil {
		t.Fatalf("read the state keys of contract %d: %v", contract, err)
	}
	var keys stateKeys
	if err := json.Unmarshal(raw, &keys); err != nil || keys.Contract != contract {
		t.Fatalf("decode the state keys of contract %d (%v): %+v", contract, err, keys)
	}
	return keys
}

// parsesWith is every way body breaks keys' schema: a key a strict object does not know, a
// required key missing, a value an enumerated field does not accept. An object or a record the
// schema does not describe is passed through unread, as a schema that strips it does.
func parsesWith(keys stateKeys, body []byte) []string {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return []string{"the body is no JSON: " + err.Error()}
	}
	var broken []string
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch value := value.(type) {
		case map[string]any:
			object, described := keys.Objects[path]
			if !described {
				if _, record := keys.Objects[keyPathJoin(path, "*")]; record {
					for _, member := range value {
						walk(member, keyPathJoin(path, "*"))
					}
				}
				return
			}
			for key, member := range value {
				if object.Strict && !slices.Contains(object.Keys, key) {
					broken = append(broken, fmt.Sprintf("%s carries %q, which its strict schema does not know", path, key))
				}
				walk(member, keyPathJoin(path, key))
			}
			for _, key := range object.Required {
				if _, carried := value[key]; !carried {
					broken = append(broken, fmt.Sprintf("%s carries no %q, which its schema requires", path, key))
				}
			}
		case []any:
			for _, member := range value {
				walk(member, keyPathJoin(path, "*"))
			}
		case string:
			if accepted, enumerated := keys.Enums[path]; enumerated && !slices.Contains(accepted, value) {
				broken = append(broken, fmt.Sprintf("%s = %q, which its schema does not accept (%v)", path, value, accepted))
			}
		}
	}
	walk(value, "")
	sort.Strings(broken)
	return broken
}

// stateWithTheHoldsThisChangeAdds is the populated state with two more held rows, one of each
// reason this change adds: an issue the daemon holds on a chain the copy cannot show to its top,
// and a run that waits for a person's todo, which no issue holds.
func stateWithTheHoldsThisChangeAdds(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	state.Admission.Held = append(state.Admission.Held,
		HeldIssue{Issue: "LEGION-299", Reason: record.HeldChainUnknown, HeldBy: []string{"LEGION-298"}},
		HeldIssue{Issue: "LEGION-300", Reason: record.HeldAwaitingTodo})
	return state
}

// For each daemon API contract a plugin in the fleet reads, and the oldest the state narrows for,
// the whole state the daemon serves a caller stating that contract parses with that contract's
// own schema, as testdata holds it (stateKeys): no key a strict object does not know, every
// required key, and every enumerated value one that contract knows, each held reason this change
// adds included. Where that contract reads `tree` on an issue and `admission.trees`, the state
// carries them (R22 of dispatch://INFRA-3320).
func TestEachOlderContractKeepsItsNarrowing(t *testing.T) {
	state := stateWithTheHoldsThisChangeAdds(t)
	handler := stateServer(t, state, nil)
	for _, contract := range fleetStateContracts {
		t.Run(fmt.Sprint(contract), func(t *testing.T) {
			keys := readStateKeys(t, contract)
			body := stateStating(t, handler, "192.0.2.77:41000", contract)
			if broken := parsesWith(keys, body); len(broken) != 0 {
				t.Fatalf("the state served at contract %d breaks that contract's schema (commit %s):\n%s", contract, keys.Commit, strings.Join(broken, "\n"))
			}
			var served struct {
				Admission map[string]json.RawMessage            `json:"admission"`
				Issues    map[string]map[string]json.RawMessage `json:"issues"`
			}
			if err := json.Unmarshal(body, &served); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(keys.Objects["admission"].Keys, "trees") {
				if _, carried := served.Admission["trees"]; !carried {
					t.Errorf("the state served at contract %d carries no admission.trees, which that contract reads", contract)
				}
			}
			if slices.Contains(keys.Objects["issues.*"].Keys, "tree") {
				for key, issue := range state.Issues {
					if _, carried := served.Issues[key]["tree"]; issue.Tree != "" && !carried {
						t.Errorf("%s is served at contract %d with no tree, which that contract reads", key, contract)
					}
				}
			}
		})
	}
}

// Each held reason this change adds, chain_unknown and awaiting_todo, reads as unclassified to a
// caller below the contract it arrived at (chainContract), whose own enumeration of the held
// reasons does not hold it and would refuse the state, and as itself at and above that contract.
func TestANewHeldReasonReadsUnclassifiedToAnOlderContract(t *testing.T) {
	handler := stateServer(t, stateWithTheHoldsThisChangeAdds(t), nil)
	reasons := []struct {
		issue  string
		reason record.HeldReason
	}{
		{"LEGION-299", record.HeldChainUnknown},
		{"LEGION-300", record.HeldAwaitingTodo},
	}
	// itself says whether the contract reads each added reason as itself; a contract the row
	// before named already (DaemonAPIVersion() while the chain entry is the build's newest) is
	// served once.
	last := 0
	for _, tc := range []struct {
		contract int
		itself   bool
	}{
		{144, false},
		{152, false},
		{entryNumber(chainContract) - 1, false},
		{entryNumber(chainContract), true},
		{DaemonAPIVersion(), true},
	} {
		if tc.contract == last {
			continue
		}
		last = tc.contract
		var served struct {
			Admission struct {
				Held []HeldIssue `json:"held"`
			} `json:"admission"`
		}
		if err := json.Unmarshal(stateStating(t, handler, "192.0.2.77:41000", tc.contract), &served); err != nil {
			t.Fatal(err)
		}
		for _, added := range reasons {
			want := record.HeldUnclassified
			if tc.itself {
				want = added.reason
			}
			i := slices.IndexFunc(served.Admission.Held, func(held HeldIssue) bool { return held.Issue == added.issue })
			if i < 0 || served.Admission.Held[i].Reason != want {
				t.Errorf("the state at contract %d holds %+v, want %s held %s", tc.contract, served.Admission.Held, added.issue, want)
			}
		}
	}
}

// A registration names a tree (claim.RegisterRequest's tree, the plugin's LEGION_TREE), and that
// tree grants nothing: the claim acts only as R18 of dispatch://INFRA-3320 allows, on its own
// issue and the issues under it on the Dispatch chain. The architect of C registered naming its
// root R as its tree, and one registered naming a tree it has nothing to do with, are each refused
// ISSUE_NOT_OWNED on R and on R's other child S, and each acts on C and the issue under C.
func TestARegistrationWithATreeGrantsNothing(t *testing.T) {
	const root, own, sibling, below, unrelated = "LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211", "LEGION-999"
	for _, tree := range []string{root, unrelated} {
		t.Run("registered naming "+tree, func(t *testing.T) {
			h, facts, _ := newArchitectHarness(t, nil, nil)
			seedChain(t, h,
				chainRow{root, "", "in_progress", phase.Implementing},
				chainRow{own, root, "in_progress", phase.Implementing},
				chainRow{sibling, root, "in_progress", phase.Implementing},
				chainRow{below, own, "in_progress", phase.Implementing},
			)
			architect := newLiveClaimIn(t, h, tree, own, claim.RoleArchitect)
			signOff := func(issue string) int {
				return h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": issue}, nil).Code
			}
			for _, issue := range []string{root, sibling} {
				refused(t, h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": issue}, nil),
					http.StatusForbidden, "ISSUE_NOT_OWNED")
			}
			for _, issue := range []string{own, below} {
				if code := signOff(issue); code != http.StatusOK {
					t.Fatalf("sign_off of %s by the architect of %s registered naming %s = %d, want 200", issue, own, tree, code)
				}
			}
			if got := len(facts.recorded()); got != 2 {
				t.Fatalf("facts applied = %d, want the two sign-offs R18 allows", got)
			}
		})
	}
}
