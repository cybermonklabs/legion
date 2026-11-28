package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// observedContracts is every contract the observed view is asked at below: each contract of
// SupportedContracts, which the shape gate reads too (contract_shapes_test.go), and one below
// ServingFloor, which contractsFor serves at the floor's restore and its own strip.
func observedContracts() []int {
	return append([]int{ServingFloor - 1}, SupportedContracts()...)
}

// observedMembers is the part of a whole state body the observed view carries, read from the
// whole body's own JSON: the daemon block, the settings and the admission block whole but for
// `admission.slots`, the slots reading, which only the whole state serves, and each issue's `key`,
// `phase`, `tree`, `slot.index`, `waitingOn`, `architect.state` and each worker's `claim.state`,
// each absent where the whole body's is absent, and `workers` absent where the whole body's is
// empty (api.ObservedIssue).
func observedMembers(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var whole map[string]any
	if err := json.Unmarshal(body, &whole); err != nil {
		t.Fatalf("decode the whole state %s: %v", body, err)
	}
	issues := map[string]any{}
	for key, value := range whole["issues"].(map[string]any) {
		issue := value.(map[string]any)
		member := map[string]any{}
		for _, name := range []string{"key", "phase", "tree", "waitingOn"} {
			if field, carried := issue[name]; carried {
				member[name] = field
			}
		}
		if slot, carried := issue["slot"].(map[string]any); carried {
			member["slot"] = map[string]any{"index": slot["index"]}
		}
		if architect, carried := issue["architect"].(map[string]any); carried {
			member["architect"] = map[string]any{"state": architect["state"]}
		}
		if workers, carried := issue["workers"].(map[string]any); carried && len(workers) > 0 {
			observed := map[string]any{}
			for role, worker := range workers {
				claim := worker.(map[string]any)["claim"].(map[string]any)
				observed[role] = map[string]any{"claim": map[string]any{"state": claim["state"]}}
			}
			member["workers"] = observed
		}
		issues[key] = member
	}
	if admission, carried := whole["admission"].(map[string]any); carried {
		delete(admission, "slots")
	}
	return map[string]any{
		"daemon": whole["daemon"], "settings": whole["settings"], "admission": whole["admission"], "issues": issues,
	}
}

// observedPart is state with only the members the observed view carries: its daemon block, its
// settings, its admission block but for its slots reading (Admission.Slots, which only the whole
// state serves), and each issue's key, phase, tree, wait, slot index, architect's claim state and
// each worker's claim state. It is the narrowest State an observed read can answer
// (StateSource.ObservedState), so a view served from it is read from those members and nothing else.
func observedPart(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, whole := range state.Issues {
		issue := Issue{Key: whole.Key, Phase: whole.Phase, Tree: whole.Tree, WaitingOn: whole.WaitingOn}
		if whole.Slot != nil {
			issue.Slot = &SlotView{Index: whole.Slot.Index}
		}
		if whole.Architect != nil {
			issue.Architect = &ClaimView{State: whole.Architect.State}
		}
		if len(whole.Workers) > 0 {
			issue.Workers = make(map[claim.Role]PhaseView, len(whole.Workers))
			for role, worker := range whole.Workers {
				issue.Workers[role] = PhaseView{Claim: ClaimView{State: worker.Claim.State}}
			}
		}
		issues[key] = issue
	}
	admission := state.Admission
	admission.Slots = nil
	return State{Daemon: state.Daemon, Settings: state.Settings, Admission: admission, Issues: issues}
}

// The observed view follows the whole state's rule (stateSeqContract's entry, shaping.go's
// observedFor): at every contract this daemon answers, the view's daemon block, settings,
// admission block and each issue's observed members are the whole state's at that same contract,
// whether the caller states its contract or the claims of its pod record it. So an older contract
// keeps its narrowing in both bodies: a caller below entryNumber(stateSeqContract) reads no
// `stateSeq`, `stateId` or `stateRun` in either, a caller below treeContract no `admission.trees`
// and no issue's `tree`, and a caller below heldContract no `admission.held`.
//
// Each state fixture of the shape gate (stateFixtures) is served by one daemon whose observed read
// answers the part of that state the view carries (observedPart), and both routes are asked by the
// same caller at the same contract. The whole body is the reference: its members are read from its
// own JSON (observedMembers), not from the view's Go value.
func TestTheObservedViewIsTheWholeStatesMembersAtEveryContract(t *testing.T) {
	// The narrowings the comparison passes through, so a run that met none of them proves nothing
	// of the rule: a body below entryNumber(stateSeqContract) without `stateSeq` and one at it with
	// the key, and a body below treeContract without `admission.trees`.
	var seqHeld, seqServed, treesHeld int
	seqAdded := entryNumber(stateSeqContract)
	for i, state := range stateFixtures(t) {
		for _, caller := range callerKinds {
			for _, contract := range observedContracts() {
				if !caller.reaches(contract) {
					// No pod reading places a caller of this kind at the contract
					// (callerKind.reaches).
					continue
				}
				claims := caller.claims(contract)
				options := stateOptions(state, claims)
				options.State = fakeSource{state: state, observed: observedPart(state), claims: claims}
				handler := NewServer("127.0.0.1", 8437, options).Handler

				want := observedMembers(t, caller.askedBody(t, handler, "/legion/v1/state", contract))
				body := caller.askedBody(t, handler, "/legion/v1/state?view=observed", contract)
				var got map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode the observed view %s: %v", body, err)
				}
				if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []string{"admission", "daemon", "issues", "settings"}) {
					t.Errorf("state fixture %d, %s at contract %d: the observed view carries %v, want daemon, settings, admission and issues alone",
						i, caller.name, contract, keys)
				}
				for _, block := range []string{"daemon", "settings", "admission"} {
					if !reflect.DeepEqual(got[block], want[block]) {
						t.Errorf("state fixture %d, %s at contract %d: the observed view's %s is %v, and the whole state's at that contract is %v",
							i, caller.name, contract, block, got[block], want[block])
					}
				}
				gotIssues, wantIssues := got["issues"].(map[string]any), want["issues"].(map[string]any)
				if gotKeys, wantKeys := slices.Sorted(maps.Keys(gotIssues)), slices.Sorted(maps.Keys(wantIssues)); !slices.Equal(gotKeys, wantKeys) {
					t.Errorf("state fixture %d, %s at contract %d: the observed view lists %v, the whole state %v",
						i, caller.name, contract, gotKeys, wantKeys)
				}
				for key, wantIssue := range wantIssues {
					if gotIssue := gotIssues[key]; !reflect.DeepEqual(gotIssue, wantIssue) {
						t.Errorf("state fixture %d, %s at contract %d: the observed view's %s is %v, and the whole state's observed members at that contract are %v",
							i, caller.name, contract, key, gotIssue, wantIssue)
					}
				}

				daemon := want["daemon"].(map[string]any)
				_, carriesSeq := daemon["stateSeq"]
				switch {
				case state.Daemon.StateSeq != nil && contract < seqAdded && !carriesSeq:
					seqHeld++
				case state.Daemon.StateSeq != nil && contract >= seqAdded && carriesSeq:
					seqServed++
				}
				if _, carriesTrees := want["admission"].(map[string]any)["trees"]; contract < treeContract && !carriesTrees {
					treesHeld++
				}
			}
		}
	}
	if seqHeld == 0 || seqServed == 0 || treesHeld == 0 {
		t.Fatalf("the comparison met %d bodies holding `stateSeq` back, %d serving it and %d holding `admission.trees` back, want each above 0: it proves nothing of the narrowing",
			seqHeld, seqServed, treesHeld)
	}
}
