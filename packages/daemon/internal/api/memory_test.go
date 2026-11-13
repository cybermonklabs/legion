package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// memoryDrawnAt is the instant LEGION-208's tree was drawn into its memory group, which its root
// and every issue under it carry.
var memoryDrawnAt = time.Date(2026, 9, 20, 11, 4, 5, 0, time.UTC)

// The state the fixture builds carries one drawn tree (populatedState): the root LEGION-208 and
// LEGION-209, a child of it, both showing the root's draw as internal/projection fills them,
// beside the issues no draw has reached. Its claim views carry the pod's update round, the
// record of what each claim's agent last did, the six keys the decided claim record added and,
// on LEGION-210's failed implementer, the sentence a launch try was refused with because the
// shared public leak check could not be installed, and its issues the three that record added
// to an issue; its admission block names the live trees and the issues the record holds in the
// workflow that neither of its other lists names, its stranded claims hold a retired one, it
// carries the wakes the pacing holds and the count of claims the refresh lane has left, an
// implementer of it carries its issue's counts of the moves back, every root of it carries the
// deployment's gated step, the two issues of its admission line carry their place in it with the
// line's own fields beside that place, every issue of it that is not working now carries its
// answer under `why`, the two issues whose current phase names a phase worker role carry the
// entry that role stands on and the entry it was given, every issue of a live tree carries that
// tree's quiet time, and one issue carries the correction of its repository and one the change
// Dispatch has named that the daemon has not applied, so one state proves all seventeen
// downgrades and that they compose. What the sixteenth band is measured on in finer grain — both
// halves of the quiet time on each member of one tree, and neither on an issue whose tree has
// left the workflow — is quiet_time_test.go's state of its own.

// issueViews is every issue of a state body, decoded as the plugin's reader sees it, by key.
func issueViews(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var state struct {
		Issues map[string]map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	return state.Issues
}

// An issue's memory group is its tree's: the root that was drawn and every issue under it carry
// the group, the mode it was drawn under and the instant of the draw, and an issue of a tree no
// draw has reached carries none of the three, which `omitempty` leaves off the wire rather than
// writing as empty strings a strict reader would refuse.
func TestTheStateShowsEveryIssueItsTreesMemoryDraw(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion))
	issues := issueViews(t, body)

	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		view, shown := issues[key]
		if !shown {
			t.Fatalf("the state %s carries no issue %s", body, key)
		}
		want := map[string]any{
			"memoryGroup":    "memory",
			"memoryDrawMode": "random",
			"memoryDrawnAt":  "2026-09-20T11:04:05Z",
		}
		for field, value := range want {
			if view[field] != value {
				t.Errorf("%s's %s = %v, want its tree's %v", key, field, view[field], value)
			}
		}
	}
	// LEGION-210 is a tree of its own that no draw has reached.
	for _, field := range []string{"memoryGroup", "memoryDrawMode", "memoryDrawnAt"} {
		if value, carried := issues["LEGION-210"][field]; carried {
			t.Errorf("LEGION-210, which no draw reached, carries %s = %v", field, value)
		}
	}
}

// contract46Issue is every key the issue record of the last strict reader before memoryContract
// knows (packages/contracts/src/legion-api.ts's legionIssue, unchanged from 46 through 48): one
// key more and it refuses the whole state, which is every `read_record` of every worker in a pod
// one release behind.
var contract46Issue = []string{
	"key", "generation", "phase", "status", "repo", "defaultBranch", "holdReason", "architect",
	"workers", "pullRequest", "designGate", "slot", "waitingOn", "check",
}

// recordFieldClaimKeys are the six keys recordFieldsContract added to a claim view and
// recordFieldIssueKeys the three it added to an issue, the decided claim and issue records
// (dispatch://INFRA-3083). recordFieldKeys is the nine together: they arrive and leave as one
// (withoutRecordFields), so one flag of the table below answers for all nine and that list is
// what the flag is read over. The halves are named because the table strips each at its own
// site, and a key written out there instead would be one this test could stop checking without
// saying so.
var (
	recordFieldClaimKeys = []string{
		"servingGeneration", "updateReasons", "providerErrorRun", "pendingTask", "failure", "pendingCause",
	}
	recordFieldIssueKeys = []string{"heldFrom", "lingerUntil", "ancestry"}
	recordFieldKeys      = slices.Concat(recordFieldClaimKeys, recordFieldIssueKeys)
)

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below memoryContract is served issues with only the keys the
// last strict reader before it knows, and the rest of the body as it is. The seventeen downgrades
// compose, band by band: a pod at 46 to 48 is served the update rounds it reads and none of the
// memory draws, the trees, the wakes the pacing holds, the gated steps, the retired stranded
// entries, the step records, the places in the admission line, the counts of the moves back, the
// count of claims the refresh lane has left, the failure record, the issues' answers, the leak
// check's cause, the two entry numbers, the held list, the tree quiet time and the repository
// corrections; a pod at 49 or 50 the memory draws as well, and still none of the rest; a pod at
// 51 or 52 the trees too; a pod at 53 or 54 the wakes too; a pod at 55 the steps too, and still
// none of the retired entries, the step records, the places, the counts of the moves back, the
// refresh count, the failure record, the answers, the cause, the entry numbers, the held list,
// the quiet time and the repository corrections; a pod at 56 the retired entries too; a pod at
// 57 the step records too; a pod at 58 to 61 the places too, and neither count nor the record
// nor the answers nor the cause nor the entry numbers nor the held list nor the quiet time nor a
// repository correction; a pod from 62 to 72 the counts of the moves back too, and still no
// refresh count; a pod at 73 or 74 the refresh count too, and none of the nine keys the decided
// record added; a pod from 75 to 84 those nine too, and no issue's answer, no field of the line
// beside its place and its length, no claim view's `leakCheckCause`, neither entry number, no
// held list, no quiet time and nothing of a repository correction; a pod at 85 or 86 the answers
// and the line's own fields too, and still neither the cause, nor either entry number, nor the
// held list, nor the quiet time, nor a repository correction; a pod from 87 to 93 the claim
// views' `leakCheckCause` too, and still neither entry number, nor the held list, nor the quiet
// time, nor a repository correction; a pod from 94 to 97 the two entry numbers too, and still no
// held list, no quiet time and no repository correction; a pod from 98 to 110 the held list too,
// and still no tree quiet time and no repository correction; a pod from 111 to 118 the quiet
// time too, and still nothing of a repository correction; a pod below 46 none of the seventeen;
// and a pod at 119, this daemon's declared contract, a pod whose claims record no contract and a
// caller in no pod — the operator's `legion state`, whose memory line reads the draws, whose
// queue line reads the wakes and whose tool-call line reads the step records — are served all of
// them, the quiet time of every live tree with the rest.
func TestTheStateRouteServesAPodItsPluginContractsIssues(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(memoryContract-1))
	fullWakes, fullCarried := wakesServed(t, full)
	fields := []string{"memoryGroup", "updateRound", "gatedSteps", "waitingLine", "leakCheckCause", TreeMovedAtField, QuietHoursField}
	for _, field := range append(fields, recordFieldKeys...) {
		field := `"` + field + `"`
		if !strings.Contains(string(full), field) {
			t.Fatalf("the fixture's state %s carries no %s; this test proves nothing", full, field)
		}
	}
	fullCount, fullCounted := claimsToRefreshServed(t, full)
	if !fullCarried || len(fullWakes) == 0 {
		t.Fatalf("the fixture's state %s carries no waiting wake; this test proves nothing", full)
	}
	if !fullCounted || fullCount == 0 {
		t.Fatalf("the fixture's state %s says the refresh lane has nothing left; this test proves nothing", full)
	}
	for _, tc := range []struct {
		name    string
		remote  string
		claims  []supervise.Claim
		memory  bool
		trees   bool
		rounds  bool
		steps   bool
		wakes   bool
		retired bool
		// stall is whether the caller is served the claim views' `step` (stallRecordContract,
		// 57), so a pod at 57 or above is served the record and a pod still running an older
		// plugin is not.
		stall bool
		// line is whether the caller is served each waiting issue's place in the admission line
		// (waitingLineContract, 58), so a pod at 58 or above is served the places and a pod still
		// running an older plugin is not.
		line bool
		// moves is whether the caller is served each worker's counts of the moves back its issue
		// has made (backwardMovesContract, 62).
		moves bool
		// refresh is whether the caller is served `claimsToRefresh`, the count of claims the
		// stale-plugin lane has left (claimsToRefreshContract, 73), so a pod at 73 or above is
		// served the count and every older pod is not.
		refresh bool
		// failure is whether the caller is served the nine keys recordFieldsContract added
		// together: a claim view's `servingGeneration`, `updateReasons`, `providerErrorRun`,
		// `pendingTask`, `failure` and `pendingCause`, and an issue's `heldFrom`, `lingerUntil`
		// and `ancestry`. What the caller is served is decided by its own recorded claim
		// contract (podContract) and never by the declared constant, which is why these rows
		// name recordFieldsContract rather than DaemonAPIVersion: a pod recording less than it
		// is served none of the nine, whatever this daemon declares.
		failure bool
		// answers is whether the caller is served each issue's one rendered answer and the
		// waiting line's own fields beside each member's place (whyAndLineFieldsContract, 85):
		// one strip takes both, so a pod below it reads every member with its `place` and its
		// `length` alone and no issue with a `why`.
		answers bool
		// leak is whether the caller is served a claim view's `leakCheckCause`, the sentence a
		// launch try was refused with because the shared public leak check could not be
		// installed (leakCheckCauseContract, 87), so a pod at 87 or above is served it and every
		// older pod is not.
		leak bool
		// entries is whether the caller is served each issue's `phaseEntry` and `givenEntry`
		// (phaseEntriesContract, 94), so a pod at 94 or above is served them and every older
		// pod is not.
		entries bool
		// held is whether the caller is served `admission.held`, the issues the record holds in
		// the workflow that stand in neither `active` nor `waiting` (heldContract, 98), so a pod
		// at 98 or above is served the list and every older pod is not.
		held bool
		// quiet is whether the caller is served each issue's measure of its tree's quiet time,
		// `treeMovedAt` and `quietHours` (quietTimeContract, 111), so a pod at 111 or above is
		// served the measure and every older pod is not. The two arrive and leave together
		// (withoutQuietTime), so one column answers for both.
		quiet bool
		// correction is whether the caller is served what a correction of an issue's repository
		// left on its record (repositoryCorrectionContract, 119), this daemon's own contract.
		correction bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", claims: podClaims(memoryContract - 1), rounds: true},
		{name: "a pod below PodUpdateContract, below all seventeen", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(memoryContract - 1), rounds: true},
		// The band at 49 and 50, above memoryContract and below treeContract: the memory group
		// is served and the two tree keys, `wakesWaiting`, the gated steps, the retired stranded
		// entries, the step record, the places in the admission line, the counts of the moves
		// back, the refresh count, the nine keys of the decided record, the answers, the leak
		// check's cause, the two entry numbers, the held list, the tree quiet time and the four
		// repository keys are not.
		{name: "a pod at memoryContract", remote: statePodAddr + ":41000", claims: podClaims(memoryContract), memory: true, rounds: true},
		// The band at 51 and 52, from treeContract to below wakesWaitingContract: the trees are
		// served too, and none of `wakesWaiting`, the gated steps, the retired entries, the step
		// record, the places, the counts, the refresh count, the nine keys of the decided
		// record, the answers, the leak check's cause, the two entry numbers, the held list, the
		// tree quiet time and the four repository keys is. This row is at treeContract, its
		// lower edge.
		{name: "a pod at treeContract", remote: statePodAddr + ":41000", claims: podClaims(treeContract), memory: true, trees: true, rounds: true},
		// The band at 53 and 54, from wakesWaitingContract to below gatedStepsContract: none of
		// the gated steps, the retired entries, the step record, the places, the counts, the
		// refresh count, the nine keys of the decided record, the answers, the leak check's
		// cause, the two entry numbers, the held list, the tree quiet time and the four
		// repository keys is served.
		{name: "a pod at wakesWaitingContract", remote: statePodAddr + ":41000", claims: podClaims(wakesWaitingContract), memory: true, trees: true, rounds: true, wakes: true},
		// The band at 55 alone, from gatedStepsContract to below retiredStrandedContract: the
		// gated steps are served too, and none of the retired entries, the step record, the
		// places, the counts, the refresh count, the nine keys of the decided record, the
		// answers, the leak check's cause, the two entry numbers, the held list, the tree quiet
		// time and the four repository keys is.
		{name: "a pod at gatedStepsContract", remote: statePodAddr + ":41000", claims: podClaims(gatedStepsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true},
		// The band at 56 alone, from retiredStrandedContract to below stallRecordContract: the
		// retired entries are served too, and none of the step record, the places, the counts,
		// the refresh count, the nine keys of the decided record, the answers, the leak check's
		// cause, the two entry numbers, the held list, the tree quiet time and the four
		// repository keys is.
		{name: "a pod at retiredStrandedContract", remote: statePodAddr + ":41000", claims: podClaims(retiredStrandedContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true},
		// The band at 57 alone, from stallRecordContract to below waitingLineContract: the step
		// record is served too, and none of the places, the counts, the refresh count, the nine
		// keys of the decided record, the answers, the leak check's cause, the two entry numbers,
		// the held list, the tree quiet time and the four repository keys is.
		{name: "a pod at stallRecordContract", remote: statePodAddr + ":41000", claims: podClaims(stallRecordContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true},
		// The band from 58 to 61, from waitingLineContract to below backwardMovesContract: the
		// places in the admission line are served too, and none of the counts of the moves back,
		// the refresh count, the nine keys of the decided record, the answers, the leak check's
		// cause, the two entry numbers, the held list, the tree quiet time and the four
		// repository keys is.
		{name: "a pod at waitingLineContract", remote: statePodAddr + ":41000", claims: podClaims(waitingLineContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true},
		// The band from 62 to 72, from backwardMovesContract to below claimsToRefreshContract:
		// the counts of the moves back are served too, and `claimsToRefresh`, the nine keys of
		// the decided record, the answers, the leak check's cause, the two entry numbers, the
		// held list, the tree quiet time and the four repository keys are held back.
		{name: "a pod at backwardMovesContract", remote: statePodAddr + ":41000", claims: podClaims(backwardMovesContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true},
		// The band at 73 and 74, from claimsToRefreshContract to below recordFieldsContract: the
		// count of claims the refresh lane has left is served too, and the nine keys of the
		// decided record, the answers, the leak check's cause, the two entry numbers, the held
		// list, the tree quiet time and the four repository keys are held back.
		{name: "a pod at claimsToRefreshContract", remote: statePodAddr + ":41000", claims: podClaims(claimsToRefreshContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true},
		// The band from 75 to 84, from recordFieldsContract to below whyAndLineFieldsContract:
		// the nine keys of the decided record are served too, and each issue's answer with the
		// line's own fields, the leak check's cause, the two entry numbers, the held list, the
		// tree quiet time and the four repository keys are held back.
		{name: "a pod at recordFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(recordFieldsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true},
		// The band at 85 and 86, from whyAndLineFieldsContract to below leakCheckCauseContract:
		// each issue's answer and the line's own fields are served too, and the leak check's
		// cause, the two entry numbers, the held list, the tree quiet time and the four
		// repository keys are held back.
		{name: "a pod at whyAndLineFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(whyAndLineFieldsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true},
		// The band from 87 to 93, from leakCheckCauseContract to below phaseEntriesContract: the
		// claim views' `leakCheckCause` is served too, and the two entry numbers, the held list,
		// the tree quiet time and the four repository keys are held back.
		{name: "a pod at leakCheckCauseContract", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true},
		// The band from 94 to 97, from phaseEntriesContract to below heldContract: the two entry
		// numbers are served too, and the held list, the tree quiet time and the four repository
		// keys are held back.
		{name: "a pod at phaseEntriesContract", remote: statePodAddr + ":41000", claims: podClaims(phaseEntriesContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true},
		// The band from 98 to 110, from heldContract to below quietTimeContract: the held list
		// is served too, and the tree quiet time and the four repository keys are held back.
		{name: "a pod at heldContract", remote: statePodAddr + ":41000", claims: podClaims(heldContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true},
		// The band from 111 to 118, from quietTimeContract to below repositoryCorrectionContract,
		// the last band under the declared contract: the tree quiet time is served too, and the
		// four repository keys are the ones held back. Its two rows are its two edges.
		{name: "a pod at quietTimeContract", remote: statePodAddr + ":41000", claims: podClaims(quietTimeContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true},
		{name: "a pod a contract below repositoryCorrectionContract", remote: statePodAddr + ":41000", claims: podClaims(repositoryCorrectionContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, correction: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, correction: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(memoryContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, correction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)

			// Only LEGION-208's tree was drawn: its root and its child carry the draw when the
			// caller is served it, and no other issue carries one whoever asks. Every root of the
			// fixture carries the one step `root-issues` gates; LEGION-209, a child of
			// LEGION-208's tree, carries none whoever asks. The two issues `admission.waiting`
			// names carry their place in that line, and no other issue carries one: LEGION-208
			// and LEGION-210 hold the slots, and LEGION-207 closed. The four issues standing in
			// a tree the admission block names carry that tree's quiet time where the caller is
			// served it; LEGION-207's tree closed and LEGION-209 stands in no tree of this
			// fixture, so neither carries a measure whoever asks.
			drawn := map[string]bool{"LEGION-208": true, "LEGION-209": true}
			gated := map[string]bool{"LEGION-207": true, "LEGION-208": true, "LEGION-210": true, "LEGION-211": true, "LEGION-212": true}
			lined := map[string]bool{"LEGION-211": true, "LEGION-212": true}
			measured := map[string]bool{"LEGION-208": true, "LEGION-210": true, "LEGION-211": true, "LEGION-212": true}
			// The two issues whose current phase names a phase worker role carry the entry
			// numbers: LEGION-208, whose implementer holds the work its row names, and
			// LEGION-211, whose implementer is behind it. The held, awaiting_person and done
			// issues carry neither whoever asks.
			entried := map[string]bool{"LEGION-208": true, "LEGION-211": true}
			for key, view := range issueViews(t, body) {
				carries := view["memoryGroup"] != nil || view["memoryDrawMode"] != nil || view["memoryDrawnAt"] != nil
				if want := tc.memory && drawn[key]; carries != want {
					t.Errorf("issue %s of %s carries a memory draw = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["gatedSteps"] != nil, tc.steps && gated[key]; carries != want {
					t.Errorf("issue %s of %s carries gated steps = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["waitingLine"] != nil, tc.line && lined[key]; carries != want {
					t.Errorf("issue %s of %s carries a place in the waiting line = %v, want %v", key, body, carries, want)
				}
				carriesEntries := view["phaseEntry"] != nil || view["givenEntry"] != nil
				if want := tc.entries && entried[key]; carriesEntries != want {
					t.Errorf("issue %s of %s carries the two phase entry numbers = %v, want %v", key, body, carriesEntries, want)
				}
				carriesQuiet := view[TreeMovedAtField] != nil || view[QuietHoursField] != nil
				if want := tc.quiet && measured[key]; carriesQuiet != want {
					t.Errorf("issue %s of %s carries its tree's quiet time = %v, want %v", key, body, carriesQuiet, want)
				}
				if tc.memory {
					continue
				}
				for field := range view {
					if !slices.Contains(contract46Issue, field) {
						t.Errorf("issue view %v carries %q, which the strict reader before memoryContract refuses", view, field)
					}
				}
			}
			if rounds := strings.Contains(string(body), `"updateRound"`); rounds != tc.rounds {
				t.Errorf("the state %s carries an update round = %v, want %v", body, rounds, tc.rounds)
			}
			if moves := strings.Contains(string(body), `"`+backwardMovesField+`"`); moves != tc.moves {
				t.Errorf("the state %s carries a worker's counts of the moves back = %v, want %v", body, moves, tc.moves)
			}
			if leak := strings.Contains(string(body), `"leakCheckCause"`); leak != tc.leak {
				t.Errorf("the state %s carries a claim view's leakCheckCause = %v, want %v", body, leak, tc.leak)
			}
			for _, key := range recordFieldKeys {
				if carries := strings.Contains(string(body), `"`+key+`"`); carries != tc.failure {
					t.Errorf("the state %s carries %q, one of the nine keys of the decided record, = %v, want %v",
						body, key, carries, tc.failure)
				}
			}
			wakes, carried := wakesServed(t, body)
			if carried != tc.wakes {
				t.Errorf("the state %s carries the `wakesWaiting` key = %v, want %v; the reader before wakesWaitingContract refuses it, empty as much as filled", body, carried, tc.wakes)
			}
			if tc.wakes && len(wakes) != len(fullWakes) {
				t.Errorf("the state %s names %d of the wakes the pacing holds, want the fixture's %d", body, len(wakes), len(fullWakes))
			}
			count, counted := claimsToRefreshServed(t, body)
			if counted != tc.refresh {
				t.Errorf("the state %s carries the `%s` key = %v, want %v; the reader before claimsToRefreshContract refuses it, 0 as much as a number", body, ClaimsToRefreshField, counted, tc.refresh)
			}
			if tc.refresh && count != fullCount {
				t.Errorf("the state %s says the lane has %d claims left, want the fixture's %d", body, count, fullCount)
			}

			// Nothing else of the body differs: it is the full state with the stripped keys gone,
			// `wakesWaiting` among them where the caller is below contract 53, since the reader
			// before it refuses that key empty as surely as filled (withoutWakesWaiting). The
			// `contract` mark is read apart and taken out of both: it is the one key the
			// caller's own recorded contract decides rather than the state, and the baseline,
			// asked from no pod, carries the daemon's number alone where a caller at a contract
			// is told its own beside it (narrowing_test.go).
			var want, got map[string]any
			if err := json.Unmarshal(full, &want); err != nil {
				t.Fatal(err)
			}
			if !tc.wakes {
				delete(want, "wakesWaiting")
			}
			if !tc.refresh {
				delete(want, ClaimsToRefreshField)
			}
			delete(want, "contract")
			// A pod below retiredStrandedContract is served the report of claims nothing will
			// wake without its retired entries, the state contract 56 added (api's
			// withoutRetiredStranded), which is every pod of every band below this contract.
			if !tc.retired {
				want["strandedClaims"] = strandedWithoutRetired(t, want["strandedClaims"])
			}
			// A pod below treeContract is served neither `admission.trees` nor the issues'
			// `tree`, the two keys contract 51 added (api's withoutTrees), which is every pod
			// below memoryContract and every pod of the band above it as well.
			if !tc.trees {
				delete(want["admission"].(map[string]any), "trees")
			}
			// A pod below heldContract is served no `admission.held`, the one key that contract
			// added (api's withoutHeld), which is every pod of every band below this one.
			if !tc.held {
				delete(want["admission"].(map[string]any), "held")
			}
			for _, issue := range want["issues"].(map[string]any) {
				issue := issue.(map[string]any)
				if !tc.memory {
					delete(issue, "memoryGroup")
					delete(issue, "memoryDrawMode")
					delete(issue, "memoryDrawnAt")
				}
				if !tc.trees {
					delete(issue, "tree")
				}
				if !tc.steps {
					delete(issue, "gatedSteps")
				}
				if !tc.line {
					delete(issue, "waitingLine")
				}
				// The issue's three of the nine keys recordFieldsContract added; the claim
				// views' six are below.
				if !tc.failure {
					for _, key := range recordFieldIssueKeys {
						delete(issue, key)
					}
				}
				if !tc.correction {
					delete(issue, "repoBefore")
					delete(issue, "repoCorrectedAt")
					delete(issue, "repoChangePending")
					delete(issue, "repoChangeReason")
				}
				// The issues' answer and the line's own fields are one contract and one
				// strip, which state_caller_test's stripAnswerKeys states and this reads:
				// below the contract a member keeps the place and the length
				// waitingLineContract added and carries no other field, and no issue
				// carries a `why`.
				if !tc.answers {
					stripAnswerKeys(issue)
				}
				// The two keys quietTimeContract added, which ride on every issue of a live
				// tree and leave together (withoutQuietTime).
				if !tc.quiet {
					delete(issue, TreeMovedAtField)
					delete(issue, QuietHoursField)
				}
				if !tc.entries {
					delete(issue, "phaseEntry")
					delete(issue, "givenEntry")
				}
				views := []any{issue["architect"]}
				for _, worker := range issue["workers"].(map[string]any) {
					worker := worker.(map[string]any)
					if !tc.moves {
						delete(worker, backwardMovesField)
					}
					views = append(views, worker["claim"])
				}
				for _, view := range views {
					view, ok := view.(map[string]any)
					if !ok {
						continue
					}
					if !tc.rounds {
						delete(view, "updateRound")
						delete(view, "updateHead")
					}
					if !tc.stall {
						delete(view, "step")
					}
					if !tc.failure {
						for _, key := range recordFieldClaimKeys {
							delete(view, key)
						}
					}
					if !tc.leak {
						delete(view, "leakCheckCause")
					}
				}
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if mark, carried := got["contract"]; carried != tc.failure {
				t.Errorf("the state %s carries the mark %v = %v, want %v: it rides under the contract of the decided record",
					body, mark, carried, tc.failure)
			}
			delete(got, "contract")
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("state = %s, want the full state without the stripped keys, %s", gotJSON, wantJSON)
			}
		})
	}
}

// The body a pod a contract behind is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, memory draws and all.
func TestTheStateServedToAPodBeforeTheMemoryContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(memoryContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if strings.Contains(string(behind), `"memoryGroup"`) {
		t.Fatalf("the pod a contract behind was served %s, want no memoryGroup", behind)
	}
	if !strings.Contains(string(operator), `"memoryGroup"`) {
		t.Fatalf("the operator was served %s, want the memory draw the state carries", operator)
	}
}

// memoryContract is the contract whose entry in version.go added the three fields, and this
// daemon's own contract is never below it: a caller at this contract is served them.
func TestTheMemoryContractIsTheContractThatAddedTheFields(t *testing.T) {
	const addedMemory = 49
	if memoryContract != addedMemory {
		t.Fatalf("memoryContract = %d, want %d, the contract whose entry in version.go added the issues' memory group",
			memoryContract, addedMemory)
	}
	if memoryContract > DaemonAPIVersion {
		t.Fatalf("memoryContract = %d, above DaemonAPIVersion %d: no caller could be served the fields",
			memoryContract, DaemonAPIVersion)
	}
}

// wakesWaitingContract is the contract whose entry in version.go added the key, and this daemon's
// own contract is never below it: a caller at this contract is served it. The bands this constant
// cuts with supervise.PodUpdateContract, memoryContract and treeContract are what the table above
// walks, so a constant that moves is a band that moves with it.
func TestTheWakesWaitingContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedWakesWaiting = 53
	if wakesWaitingContract != addedWakesWaiting {
		t.Fatalf("wakesWaitingContract = %d, want %d, the contract whose entry in version.go added `wakesWaiting`",
			wakesWaitingContract, addedWakesWaiting)
	}
	if wakesWaitingContract > DaemonAPIVersion {
		t.Fatalf("wakesWaitingContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			wakesWaitingContract, DaemonAPIVersion)
	}
}
