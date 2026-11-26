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

// planDrawnAt is the instant each issue of that tree was drawn for `plan-length`, the fixture's
// issue experiment: a draw of its own, made when the issue was opened rather than when its tree
// was, so the two instants differ as the units do.
var planDrawnAt = time.Date(2026, 9, 21, 18, 42, 11, 0, time.UTC)

// The state the fixture builds carries one drawn tree (populatedState): the root LEGION-208 and
// LEGION-209, a child of it, both showing the root's draw as internal/projection fills them, beside
// the issues no draw has reached. Its claim views carry the pod's update round, the record of what
// each claim's agent last did, the six keys the decided claim record added and, on LEGION-210's
// failed implementer, the sentence a launch try was refused with because the shared public leak
// check could not be installed, and its issues the three that record added to an issue; its
// admission block names the live trees and the issues the record holds in the workflow that neither
// of its other lists names, its stranded claims hold a retired one, it carries the wakes the pacing
// holds and the count of claims the refresh lane has left, an implementer of it carries its issue's
// counts of the moves back, every root of it carries the deployment's gated step, two of them on an
// ancestor's basis, open and pending, the two issues of its admission line carry their place in it
// with the line's own fields beside that place, every issue of it that is not working now carries
// its answer under `why`, each issue whose current phase names a role carries the entry that role
// stands on and the entry it was given, two of them the times of the task they hold of that entry,
// a root no architect claim stands for carries the architect-stalled arm the daemon reads for it,
// each reviewing root of it carries what its round waits on from GitHub, every issue of a live tree
// carries that tree's quiet time, the issues of its drawn tree carry the experiment answers they
// hold, the project stands paused (`paused`), every root carries whether its tree runs, its
// capability report holds the repository caches' row with that source's own word, and the body
// carries the daemon's Dispatch credential under dispatch_token_broker, so one state
// proves every band of stateNarrowings, that they compose, and the value strip of a capability
// row's `source` beside them; it stands no member of the line of the kind 132 added, whose value
// strip waiting_on_issue_test.go proves, and the pause's other key, `lastPause`, is pause_test.go's
// state of its own. What the quiet-time band is measured on in finer grain — both halves on each
// member of one tree, and neither on an issue whose tree has left the workflow — is
// quiet_time_test.go's state of its own, and what the gate-basis band is, gate_basis_wire_test.go's.

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
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion()))
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
// whose claims record a contract below memoryContract is served issues with only the keys the last
// strict reader before it knows, and the rest of the body as it is. The downgrades this case walks
// compose, band by band, as the rows of the table below say, the line-kind value strip
// (withoutWaitingOnIssue) finding nothing to take here: each pod of this case records one
// contract, so a word of 85 is admitted or has left with its band, and the fixture stands no
// member of the kind 132 added (waiting_on_issue_test.go reads that word). A pod below 46 is served
// none of the keys a row of stateNarrowings strips, a pod in a band is served the keys of every row
// at or below that band's contract and none of a later row's — so a pod from 55 up is served the
// gated steps of every root but the two whose step stands on an ancestor's basis, which
// gateBasisContract's row takes — and a pod at this daemon's declared contract (DaemonAPIVersion),
// a pod whose claims record no contract and a caller in no pod — the operator's `legion state`,
// whose memory line reads the draws, whose experiment lines read the assignments, whose queue line
// reads the wakes, whose tool-call line reads the step records and whose merge-wait line reads the
// waits — are served all of them, the basis and every root's `treeRuns` with the rest.
func TestTheStateRouteServesAPodItsPluginContractsIssues(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(memoryContract-1))
	fullWakes, fullCarried := wakesServed(t, full)
	fields := []string{"memoryGroup", "updateRound", "gatedSteps", "waitingLine", "leakCheckCause", TreeMovedAtField, QuietHoursField,
		"assignments", "apps", "treeRuns", "basis", string(GatedStepOpenOnBasis), string(GatedStepPendingOnBasis), "dispatchCredential"}
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
		// has made (backwardMovesContract, 62), so a pod at 62 or above is served the counts and
		// a pod still running an older plugin is not.
		moves bool
		// refresh is whether the caller is served `claimsToRefresh`, the count of claims the
		// stale-plugin lane has left (claimsToRefreshContract, 73), so a pod at 73 or above is
		// served the count and every older pod is not.
		refresh bool
		// contract is the contract the body is shaped at: the one the row's claims record, and
		// this daemon's own where no claim of the caller's pod records one and where the caller
		// is in no pod. It decides the two shapes servedShape applies, which no column of its
		// own carries, because the bands they cut are the ones this table already walks.
		contract int
		// failure is whether the caller is served the nine keys recordFieldsContract added
		// together: a claim view's `servingGeneration`, `updateReasons`, `providerErrorRun`,
		// `pendingTask`, `failure` and `pendingCause`, and an issue's `heldFrom`, `lingerUntil`
		// and `ancestry`. What the caller is served is decided by its own recorded claim
		// contract (podContracts) and never by the declared constant, which is why these rows
		// name recordFieldsContract rather than DaemonAPIVersion: a pod recording less than it
		// is served none of the nine, whatever this daemon declares.
		failure bool
		// assigned is whether the caller is served each issue's experiment assignments
		// (assignmentsContract), the contract that added them, so a pod at it or above is served
		// them and every pod below it is not.
		assigned bool
		// answers is whether the caller is served each issue's one rendered answer and the
		// waiting line's own fields beside each member's place (whyAndLineFieldsContract, 85):
		// one serving-down takes both, so a pod below it reads every member with its `place`
		// and its `length` alone and no issue with a `why`.
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
		// `treeMovedAt` and `quietHours` (quietTimeContract), so a pod at it or above is served
		// the measure and every older pod is not. The two arrive and leave together
		// (withoutQuietTime), so one column answers for both.
		quiet bool
		// wait is whether the caller is served what each reviewing issue's round waits on from
		// GitHub (mergeWaitContract), the contract that added it, so a pod at it is served the
		// object and every pod below it is not, whatever else the contracts it does reach give it.
		wait bool
		// times is whether the caller is served each issue's `entryQueuedAt` and
		// `entryConfirmedAt`, and each root's `architectStalls` (entryTimesContract), the
		// contract that added the three, so a pod at it or above is served them and every pod
		// below it is not.
		times bool
		// apps is whether the caller is served `apps` on each entry of `repositories.mapped` and
		// `repositories.refused` (repositoryAppsContract), the contract that added it, so a pod at
		// it is served the key and every pod below it is not.
		apps bool
		// credential is whether the caller is served `dispatchCredential`, the daemon's Dispatch
		// credential under dispatch_token_broker (dispatchCredentialContract), the contract that
		// added it. No pod is placed at it: a plugin at shapedAnswersContract or later states its
		// contract in the header (podContracts), so every pod this table places is below it and is
		// served no object, and a caller served this daemon's own body is served it.
		credential bool
		// pause is whether the caller is served the project's pause, `paused` here, which
		// pauseContract added beside `lastPause`, so a pod at it or above is served the key and
		// every pod below it is not.
		pause bool
		// repos is whether the caller is served the repositories block repositoriesLiveContract
		// writes, `live` beside `caches` and `withoutWebhookApp`, so a caller at it or above is
		// served that block and every caller below it the block in contract 152's shape, from the
		// same entries. No pod's claims place a caller at it (callerKind.reaches), so the rows
		// served it are the three this daemon serves its own body.
		repos bool
		// plans is whether the caller is served each assignment's `plan` (assignmentPlanContract),
		// the contract that added it, so a pod at it is served the key and every pod below it is
		// not.
		plans bool
		// priority is whether the caller is served each issue's `priority` and its waitingLine's
		// `priority` (priorityContract), the contract that added both, so a pod at it is served
		// the keys and every pod below it is not.
		priority bool
		// caches is whether the caller is served the repository-cache-room row's `source`,
		// `caches` (cachesSourceContract), the contract that added the word. The word is a value,
		// held back from every caller whose pod's lowest contract is below that contract
		// (withoutUnadmittedCapabilitySources), and every pod this table places is below it: a
		// claim at shapedAnswersContract or above is in no pod's reckoning (podContracts). So the
		// rows served this daemon's own body are served the word, and every other row the row
		// without it.
		caches bool
		// giveUps is whether the caller is served `intakeGiveUps`, the intake's open give-ups
		// (intakeGiveUpsContract, the contract that added the key): a pod whose claims record a
		// contract below shapedAnswersContract is not served the key and a pod at or above it is
		// (podContracts counts no such claim), and a caller that states a contract below
		// intakeGiveUpsContract is not (TestAnOlderPluginContractGetsTheStateWithoutIntakeGiveUps).
		giveUps bool
		// basis is whether the caller is served what gateBasisContract added, this daemon's own
		// contract: every root's `treeRuns`, and the gated steps of a root one of whose steps
		// stands open or pending on an ancestor's basis (withoutGateBasis, and the strip of the
		// two status words beside it, withoutUnadmittedStepStatuses). They leave together for
		// every row here, so one column answers for both. No pod reading places a caller at that
		// contract (podContracts), so only the rows served this daemon's own body are served it.
		basis bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", claims: podClaims(memoryContract - 1), rounds: true, contract: memoryContract - 1},
		{name: "a pod below PodUpdateContract, below every narrowing", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1), contract: supervise.PodUpdateContract - 1},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(memoryContract - 1), rounds: true, contract: memoryContract - 1},
		// The band at 49 and 50, above memoryContract and below treeContract: the memory group is
		// served and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at memoryContract", remote: statePodAddr + ":41000", claims: podClaims(memoryContract), memory: true, rounds: true, contract: memoryContract},
		// The band at 51 and 52, from treeContract to below wakesWaitingContract: the trees are
		// served too, and every key a later row of stateNarrowings adds is held back. This row is
		// at treeContract, its lower edge.
		{name: "a pod at treeContract", remote: statePodAddr + ":41000", claims: podClaims(treeContract), memory: true, trees: true, rounds: true, contract: treeContract},
		// The band at 53 and 54, from wakesWaitingContract to below gatedStepsContract:
		// `wakesWaiting` is served too, and every key a later row of stateNarrowings adds is held
		// back.
		{name: "a pod at wakesWaitingContract", remote: statePodAddr + ":41000", claims: podClaims(wakesWaitingContract), memory: true, trees: true, rounds: true, wakes: true, contract: wakesWaitingContract},
		// The band at 55 alone, from gatedStepsContract to below retiredStrandedContract: the gated
		// steps are served too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at gatedStepsContract", remote: statePodAddr + ":41000", claims: podClaims(gatedStepsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, contract: gatedStepsContract},
		// The band at 56 alone, from retiredStrandedContract to below stallRecordContract: the
		// retired entries are served too, and every key a later row of stateNarrowings adds is held
		// back.
		{name: "a pod at retiredStrandedContract", remote: statePodAddr + ":41000", claims: podClaims(retiredStrandedContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, contract: retiredStrandedContract},
		// The band at 57 alone, from stallRecordContract to below waitingLineContract: the step
		// record is served too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at stallRecordContract", remote: statePodAddr + ":41000", claims: podClaims(stallRecordContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, contract: stallRecordContract},
		// The band from 58 to 61, from waitingLineContract to below backwardMovesContract: the
		// places in the admission line are served too, and every key a later row of
		// stateNarrowings adds is held back.
		{name: "a pod at waitingLineContract", remote: statePodAddr + ":41000", claims: podClaims(waitingLineContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, contract: waitingLineContract},
		// The band from 62 to 72, from backwardMovesContract to below claimsToRefreshContract:
		// the counts of the moves back are served too, and every key a later row of
		// stateNarrowings adds is held back.
		{name: "a pod at backwardMovesContract", remote: statePodAddr + ":41000", claims: podClaims(backwardMovesContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, contract: backwardMovesContract},
		// The band at 73 and 74, from claimsToRefreshContract to below recordFieldsContract: the
		// count of claims the refresh lane has left is served too, and every key a later row of
		// stateNarrowings adds is held back.
		{name: "a pod at claimsToRefreshContract", remote: statePodAddr + ":41000", claims: podClaims(claimsToRefreshContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, contract: claimsToRefreshContract},
		// The band from 75 to 84, from recordFieldsContract to below whyAndLineFieldsContract: the
		// nine keys of the decided record are served too, and every key a later row of
		// stateNarrowings adds is held back.
		{name: "a pod at recordFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(recordFieldsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, contract: recordFieldsContract},
		// The band at 85 and 86, from whyAndLineFieldsContract to below leakCheckCauseContract:
		// each issue's answer and the line's own fields are served too, and every key a later row
		// of stateNarrowings adds is held back.
		{name: "a pod at whyAndLineFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(whyAndLineFieldsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, contract: whyAndLineFieldsContract},
		// The band from 87 to 93, from leakCheckCauseContract to below phaseEntriesContract: the
		// claim views' `leakCheckCause` is served too, and every key a later row of stateNarrowings
		// adds is held back.
		{name: "a pod at leakCheckCauseContract", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, contract: leakCheckCauseContract},
		// The band from 94 to 97, from phaseEntriesContract to below heldContract: the two entry
		// numbers are served too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at phaseEntriesContract", remote: statePodAddr + ":41000", claims: podClaims(phaseEntriesContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, contract: phaseEntriesContract},
		// The band from 98 to 110, from heldContract to below quietTimeContract: the held list is
		// served too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at heldContract", remote: statePodAddr + ":41000", claims: podClaims(heldContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, contract: heldContract},
		// The band from 111 to 132, from quietTimeContract to below mergeWaitContract: the tree
		// quiet time is served too, and every key a later row of stateNarrowings adds is held back.
		// This fixture stands nothing the strip of 132 holds back, so a pod at
		// waitingOnIssueContract reads as a pod at quietTimeContract.
		{name: "a pod at quietTimeContract", remote: statePodAddr + ":41000", claims: podClaims(quietTimeContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: quietTimeContract},
		{name: "a pod at waitingOnIssueContract", remote: statePodAddr + ":41000", claims: podClaims(waitingOnIssueContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: waitingOnIssueContract},
		// The band from 133 to 140, from mergeWaitContract to below assignmentsContract: what each
		// reviewing root's round waits on from GitHub is served too, and every key a later row of
		// stateNarrowings adds is held back.
		{name: "a pod at mergeWaitContract", remote: statePodAddr + ":41000", claims: podClaims(mergeWaitContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, contract: mergeWaitContract},
		// The band from 141 to 145, from assignmentsContract to below entryTimesContract: each
		// issue's experiment assignments are served too, and the two times of the entry's task,
		// the architect-stalled arms and every key a later row of stateNarrowings adds are held
		// back, at the band's foot and at its top alike.
		{name: "a pod at assignmentsContract", remote: statePodAddr + ":41000", claims: podClaims(assignmentsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, contract: assignmentsContract},
		{name: "a pod just below entryTimesContract", remote: statePodAddr + ":41000", claims: podClaims(entryTimesContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, contract: entryTimesContract - 1},
		// The band from entryTimesContract to below repositoryAppsContract: the two times of the
		// entry's task and the architect-stalled arms are served too, and the repositories'
		// `apps` and every key a later row of stateNarrowings adds are held back, at the band's
		// foot and at its top alike.
		{name: "a pod at entryTimesContract", remote: statePodAddr + ":41000", claims: podClaims(entryTimesContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, contract: entryTimesContract},
		{name: "a pod just below repositoryAppsContract", remote: statePodAddr + ":41000", claims: podClaims(repositoryAppsContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, contract: repositoryAppsContract - 1},
		// The band from repositoryAppsContract to below pauseContract: the repositories' `apps`
		// are served too, and the project's pause and every key a later row of stateNarrowings adds
		// are held back, at the band's foot and at its top alike.
		{name: "a pod at repositoryAppsContract", remote: statePodAddr + ":41000", claims: podClaims(repositoryAppsContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, contract: repositoryAppsContract},
		{name: "a pod just below pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, contract: pauseContract - 1},
		// The band from pauseContract to below shapedAnswersContract, the last a pod's claims
		// place a caller in (callerKind.reaches): the project's pause is served too, and every key
		// a later row of stateNarrowings adds is held back, `intakeGiveUps` among them, the
		// repositories block written in the shape contract 152 gave it, as is the repository
		// caches' row's `source`, the value cachesSourceContract added, at the band's foot and at
		// its top alike.
		{name: "a pod at pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, pause: true, contract: pauseContract},
		{name: "a pod just below shapedAnswersContract", remote: statePodAddr + ":41000", claims: podClaims(shapedAnswersContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, pause: true, contract: shapedAnswersContract - 1},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion()), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, pause: true, repos: true, plans: true, priority: true, caches: true, giveUps: true, contract: DaemonAPIVersion(), basis: true, credential: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, pause: true, repos: true, plans: true, priority: true, caches: true, giveUps: true, contract: DaemonAPIVersion(), basis: true, credential: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(memoryContract - 1), memory: true, trees: true, rounds: true, wakes: true, steps: true, retired: true, stall: true, line: true, moves: true, refresh: true, failure: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, apps: true, pause: true, repos: true, plans: true, priority: true, caches: true, giveUps: true, contract: DaemonAPIVersion(), basis: true, credential: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)

			// Only LEGION-208's tree was drawn: its root and its child carry the draw when the
			// caller is served it, and no other issue carries one whoever asks. Every root of the
			// fixture carries the one step `root-issues` gates; LEGION-209, a child of LEGION-208's
			// tree, carries none whoever asks. LEGION-207's step stands open on an ancestor's basis
			// and LEGION-214's waits on one, so those two carry their steps only where the caller is
			// served the basis, and every root carries `treeRuns` there and nowhere else. The two
			// issues `admission.waiting` names carry their place in that line, and no other issue
			// carries one: the issues `admission.active` names hold the slots, and LEGION-207
			// closed. The reviewing roots (waits below) are the issues whose round waits on GitHub's
			// merge state for its head, and no other issue carries that object. The issues standing
			// in a tree the admission block names (measured below) carry that tree's quiet time
			// where the caller is served it; LEGION-207's tree closed and LEGION-209 stands in no
			// tree of this fixture, so neither carries a measure whoever asks.
			drawn := map[string]bool{"LEGION-208": true, "LEGION-209": true}
			gated := map[string]bool{"LEGION-207": true, "LEGION-208": true, "LEGION-210": true, "LEGION-211": true, "LEGION-212": true, "LEGION-214": true, "LEGION-218": true, "LEGION-219": true}
			onBasis := map[string]bool{"LEGION-207": true, "LEGION-214": true}
			lined := map[string]bool{"LEGION-211": true, "LEGION-212": true}
			measured := map[string]bool{"LEGION-208": true, "LEGION-210": true, "LEGION-211": true, "LEGION-212": true, "LEGION-218": true, "LEGION-219": true}
			// The issues the fixture holds an experiment answer for: the drawn tree's root and
			// its child, each under two experiments, and LEGION-210, whose hand-over settled it
			// at no arm, which is an answer and so a row.
			assigned := map[string]bool{"LEGION-208": true, "LEGION-209": true, "LEGION-210": true}
			// The issues whose current phase names a role carry the entry numbers (entried
			// below): LEGION-208, whose implementer holds the work its row names, LEGION-211,
			// whose implementer is behind it, and the reviewing roots LEGION-218 and LEGION-219,
			// whose reviewers hold theirs. The held, awaiting_person and done issues carry neither
			// whoever asks. LEGION-208 alone carries the two times: every other issue would carry
			// an architect's handshake too, a root its own architect's in every phase and
			// LEGION-209 its architect's at admitted, and no other issue of the fixture holds an
			// architect claim. LEGION-210, a root no architect claim stands for, is the one the
			// daemon names an architect-stalled arm for, which leaves with the two times.
			entried := map[string]bool{"LEGION-208": true, "LEGION-211": true, "LEGION-218": true, "LEGION-219": true}
			timed := map[string]bool{"LEGION-208": true}
			stalled := map[string]bool{"LEGION-210": true}
			waits := map[string]bool{"LEGION-218": true, "LEGION-219": true}
			for key, view := range issueViews(t, body) {
				carries := view["memoryGroup"] != nil || view["memoryDrawMode"] != nil || view["memoryDrawnAt"] != nil
				if want := tc.memory && drawn[key]; carries != want {
					t.Errorf("issue %s of %s carries a memory draw = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["gatedSteps"] != nil, tc.steps && gated[key] && (tc.basis || !onBasis[key]); carries != want {
					t.Errorf("issue %s of %s carries gated steps = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["treeRuns"] != nil, tc.basis && gated[key]; carries != want {
					t.Errorf("issue %s of %s carries treeRuns = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["waitingLine"] != nil, tc.line && lined[key]; carries != want {
					t.Errorf("issue %s of %s carries a place in the waiting line = %v, want %v", key, body, carries, want)
				}
				if carries, want := view["assignments"] != nil, tc.assigned && assigned[key]; carries != want {
					t.Errorf("issue %s of %s carries its experiment assignments = %v, want %v", key, body, carries, want)
				}
				carriesEntries := view["phaseEntry"] != nil || view["givenEntry"] != nil
				if want := tc.entries && entried[key]; carriesEntries != want {
					t.Errorf("issue %s of %s carries the two phase entry numbers = %v, want %v", key, body, carriesEntries, want)
				}
				if carries, want := view[MergeWaitField] != nil, tc.wait && waits[key]; carries != want {
					t.Errorf("issue %s of %s carries what its round waits on from GitHub = %v, want %v", key, body, carries, want)
				}
				carriesQuiet := view[TreeMovedAtField] != nil || view[QuietHoursField] != nil
				if want := tc.quiet && measured[key]; carriesQuiet != want {
					t.Errorf("issue %s of %s carries its tree's quiet time = %v, want %v", key, body, carriesQuiet, want)
				}
				carriesTimes := view["entryQueuedAt"] != nil || view["entryConfirmedAt"] != nil
				if want := tc.times && timed[key]; carriesTimes != want {
					t.Errorf("issue %s of %s carries the two entry times = %v, want %v", key, body, carriesTimes, want)
				}
				if carries, want := view["architectStalls"] != nil, tc.times && stalled[key]; carries != want {
					t.Errorf("issue %s of %s carries the architect-stalled arms = %v, want %v", key, body, carries, want)
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
			// asked from no pod, is told the daemon's number as its caller's where a caller at
			// a contract is told its own (narrowing_test.go).
			// The repositories block of a caller below repositoriesLiveContract is written in
			// contract 152's shape from the same entries (withoutRepositoriesLive), which
			// state_caller_test's withRepositoriesAt152 states and this reads.
			base := full
			if !tc.repos {
				base = withRepositoriesAt152(t, full)
			}
			var want, got map[string]any
			if err := json.Unmarshal(base, &want); err != nil {
				t.Fatal(err)
			}
			if !tc.wakes {
				delete(want, "wakesWaiting")
			}
			if !tc.refresh {
				delete(want, ClaimsToRefreshField)
			}
			// A caller below dispatchCredentialContract is served no `dispatchCredential`, the one
			// key that contract added (api's withoutDispatchCredential), which is every pod this
			// table places, since a plugin at shapedAnswersContract or later states its contract.
			if !tc.credential {
				delete(want, "dispatchCredential")
			}
			if !tc.giveUps {
				delete(want, "intakeGiveUps")
			}
			delete(want, "contract")
			// A pod below p0BoundContract is served the admission without `p0Holders` and
			// `p0Bound`, and every issue without `resolvedPriority` and `priorityFrom`, the keys
			// that entry added (api's withoutP0Start and withoutResolvedPriority).
			if tc.contract < entryNumber(p0BoundContract) {
				admission := want["admission"].(map[string]any)
				delete(admission, "p0Holders")
				delete(admission, "p0Bound")
				if issues, carries := want["issues"].(map[string]any); carries {
					for _, issue := range issues {
						delete(issue.(map[string]any), "resolvedPriority")
						delete(issue.(map[string]any), "priorityFrom")
					}
				}
			}
			// A pod below pauseContract is served no `paused`, the key that contract added beside
			// `lastPause` (api's withoutPause), which is every pod of every band below it.
			if !tc.pause {
				delete(want, "paused")
			}
			// A pod below cachesSourceContract is served the repository caches' capability row
			// without its `source` (api's withoutUnadmittedCapabilitySources), which is every pod
			// this table places.
			if !tc.caches {
				stripCachesSources(want)
			}
			// A pod below slotsReadingContract is served no slots reading, the five keys that
			// contract added (api's withoutSlotsReading, withoutRanksAndSlotStates), which is every
			// pod of every band here.
			if tc.contract < entryNumber(slotsReadingContract) {
				withoutSlotsReadingKeys(want)
			}
			if tc.contract < entryNumber(admissionSignalsContract) {
				withoutAdmissionSignalsKeys(want)
			}
			// A pod below retiredStrandedContract is served the report of claims nothing will
			// wake without its retired entries, the state contract 56 added (api's
			// withoutUnadmittedStrandedStates), which is every pod of every band below this
			// contract.
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
			// added (api's withoutHeld), which is every pod of every band below heldContract's.
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
				if !tc.assigned {
					delete(issue, "assignments")
				}
				if rows, carried := issue["assignments"].([]any); carried && !tc.plans {
					for _, row := range rows {
						delete(row.(map[string]any), "plan")
					}
				}
				// The one key podHoldContract added, which rides only on an issue whose pod a
				// hold stands on (withoutPodHold): every row below that contract loses it.
				if tc.contract < podHoldContract {
					delete(issue, "podHold")
				}
				if !tc.priority {
					stripPriority(issue)
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
				// What gateBasisContract added, which state_caller_test's stripBasisKeys
				// states and this reads: every root's `treeRuns`, and the gated steps of a
				// root one of whose steps stands on an ancestor's basis.
				if !tc.basis {
					stripBasisKeys(issue)
				}
				if !tc.entries {
					delete(issue, "phaseEntry")
					delete(issue, "givenEntry")
				}
				if !tc.wait {
					delete(issue, MergeWaitField)
				}
				if !tc.times {
					delete(issue, "entryQueuedAt")
					delete(issue, "entryConfirmedAt")
					delete(issue, "architectStalls")
				}
				for _, worker := range issue["workers"].(map[string]any) {
					if !tc.moves {
						delete(worker.(map[string]any), backwardMovesField)
					}
				}
				for _, view := range decodedClaimViews(issue) {
					if !tc.rounds {
						delete(view, "updateRound")
						delete(view, "updateHead")
					}
					if !tc.stall {
						delete(view, "step")
					}
					// A caller below shapedAnswersContract is served no claim view carrying the
					// two keys contract 193 added to one, which is every band of this table but
					// the last three rows.
					if tc.contract < shapedAnswersContract {
						for _, key := range roundReportKeys {
							delete(view, key)
						}
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
			servedShape(want, tc.contract)
			// The one key repositoryAppsContract added is on the body's own repositories block
			// and on no issue (withoutRepositoryApps).
			if !tc.apps {
				stripRepositoryApps(want)
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			// The fleet's lowest plugin contract is the snapshot's claims, which this row's own
			// claims are: it is no key the caller's contract decides (withoutLowestPluginContract).
			delete(want, "lowestPluginContract")
			delete(got, "lowestPluginContract")
			if mark, carried := got["contract"]; carried != tc.failure {
				t.Errorf("the state %s carries the mark %v = %v, want %v: it rides under the contract of the decided record",
					body, mark, carried, tc.failure)
			}
			if credential, carried := got["dispatchCredential"]; carried != tc.credential {
				t.Errorf("the state %s carries the daemon's Dispatch credential %v = %v, want %v", body, credential, carried, tc.credential)
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
	if memoryContract > DaemonAPIVersion() {
		t.Fatalf("memoryContract = %d, above DaemonAPIVersion %d: no caller could be served the fields",
			memoryContract, DaemonAPIVersion())
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
	if wakesWaitingContract > DaemonAPIVersion() {
		t.Fatalf("wakesWaitingContract = %d, above DaemonAPIVersion %d: no caller could be served the key",
			wakesWaitingContract, DaemonAPIVersion())
	}
}
