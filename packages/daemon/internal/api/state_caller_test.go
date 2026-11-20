package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podClaims, stateServer and stateFrom carry the claims the state's own snapshot holds: the route
// reads the caller's plugin contract from those, as the daemon's source answers them beside the
// state it projected from them (api.StateSource).

const (
	// statePodUID is the pod populatedState's LEGION-208 workers run in, and statePodAddr its
	// address, from which that pod's plugin asks for the state.
	statePodUID  = "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"
	statePodAddr = "10.1.24.6"
)

// contract45ClaimView is every key the claim view of contract 45's strict reader knows
// (packages/contracts/src/legion-api.ts's legionClaimView at pi-legion v17.1.2, the last release
// declaring 45): one key more and it refuses the whole state.
var contract45ClaimView = []string{"session", "state", "locator", "capabilities", "pluginContract"}

// stateServer serves state, one value for every request, as a daemon whose pod at statePodAddr is
// statePodUID and whose state snapshot holds claims.
func stateServer(t *testing.T, state State, claims []supervise.Claim) http.Handler {
	t.Helper()
	return NewServer("127.0.0.1", 8437, stateOptions(state, claims)).Handler
}

// stateOptions is what that daemon answers from, apart so a test that reads the daemon's own
// log takes them and adds its logger (narrowing_test.go's stateNarrowed).
func stateOptions(state State, claims []supervise.Claim) Options {
	return Options{
		Steps: config.DesignGateRootIssues.Steps(),
		State: fakeSource{state: state, claims: claims}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		CallerPod: func(addr netip.Addr) (string, bool) {
			if addr == netip.MustParseAddr(statePodAddr) {
				return statePodUID, true
			}
			return "", false
		},
	}
}

// stateAsked is GET /legion/v1/state on handler, asked from remote.
func stateAsked(t *testing.T, handler http.Handler, remote string) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
	request.RemoteAddr = remote
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// stateFrom is GET /legion/v1/state asked from remote of a server of its own.
func stateFrom(t *testing.T, state State, remote string, claims []supervise.Claim) []byte {
	t.Helper()
	return stateAsked(t, stateServer(t, state, claims), remote)
}

// podClaims is the claims of the pod at statePodAddr, each recording contract, beside a claim in
// another pod at this daemon's contract, which says nothing of the caller.
func podClaims(contract int) []supervise.Claim {
	inPod := func(token claim.Token, pod string, contract int) supervise.Claim {
		return supervise.Claim{Token: token, State: supervise.StateReady, PluginContract: contract, Locator: &runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: pod + "/1",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-208", PodUID: pod, Container: "roles", Generation: 1},
		}}
	}
	return []supervise.Claim{
		inPod("legion-legion-legion-208-planner", statePodUID, contract),
		inPod("legion-legion-legion-208-implementer", statePodUID, contract),
		inPod("legion-legion-legion-210-planner", "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34", DaemonAPIVersion),
	}
}

// wakesServed is the `wakesWaiting` of a state body — the wakes it names, and whether the key is
// on the wire at all, which an empty array is not. The two are read apart because the reader
// before wakesWaitingContract refuses the key whether it names a wake or nothing, so an emptied
// key is as bad as a filled one and withoutWakesWaiting leaves it off.
func wakesServed(t *testing.T, body []byte) (wakes []map[string]any, carried bool) {
	t.Helper()
	var state struct {
		WakesWaiting json.RawMessage `json:"wakesWaiting"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	if state.WakesWaiting == nil {
		return nil, false
	}
	if err := json.Unmarshal(state.WakesWaiting, &wakes); err != nil {
		t.Fatalf("decode wakesWaiting %s: %v", state.WakesWaiting, err)
	}
	return wakes, true
}

// claimsToRefreshServed is the `claimsToRefresh` of a state body — the count it names, and
// whether the key is on the wire at all, which 0 is not the same as. The two are read apart for
// the same reason wakesServed reads them apart: the reader before claimsToRefreshContract
// refuses the key when it is 0 as surely as when it names a number, so a zeroed key is as bad
// as a filled one and withoutClaimsToRefresh leaves it off.
func claimsToRefreshServed(t *testing.T, body []byte) (count int, carried bool) {
	t.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	raw, carried := state[ClaimsToRefreshField]
	if !carried {
		return 0, false
	}
	if err := json.Unmarshal(raw, &count); err != nil {
		t.Fatalf("decode %s %s: %v", ClaimsToRefreshField, raw, err)
	}
	return count, true
}

// claimViews is every claim view of a state body, decoded as the plugin's reader sees it.
func claimViews(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			Architect map[string]any `json:"architect"`
			Workers   map[string]struct {
				Claim map[string]any `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	var views []map[string]any
	for _, issue := range state.Issues {
		if issue.Architect != nil {
			views = append(views, issue.Architect)
		}
		for _, worker := range issue.Workers {
			views = append(views, worker.Claim)
		}
	}
	return views
}

// unmarked is a state body with the `contract` mark taken out and written again: what two
// bodies of two callers are compared by, since that mark is the one key of the body the
// caller's own recorded contract decides rather than the state (narrowing_test.go holds what
// each caller is told there).
func unmarked(t *testing.T, body []byte) string {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	delete(served, "contract")
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its mark: %v", body, err)
	}
	return string(encoded)
}

// withoutQuietKeys is a state body with no issue carrying `treeMovedAt` or `quietHours`, which
// is what a caller below quietTimeContract is served of a state that carries them (api's
// withoutQuietTime). A row of the table below is compared against the whole body, so the
// baseline it is read against is the full state with exactly the keys that row's own contract
// holds back taken out of it.
func withoutQuietKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		issue := issue.(map[string]any)
		delete(issue, TreeMovedAtField)
		delete(issue, QuietHoursField)
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its quiet times: %v", body, err)
	}
	return encoded
}

// withoutAssignmentKeys is a state body with no issue carrying `assignments`, which is what a
// caller below assignmentsContract is served of a state that carries them (api's
// withoutAssignments). It serves the table below as withoutQuietKeys does: a row's baseline is
// the full state with exactly the keys that row's own contract holds back taken out of it.
func withoutAssignmentKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		delete(issue.(map[string]any), "assignments")
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its assignments: %v", body, err)
	}
	return encoded
}

// withoutMergeWaitKey is a state body with no issue carrying `mergeWait`, which is what a caller
// below mergeWaitContract is served of a state (api's withoutMergeWait). It serves the table
// below as withoutQuietKeys does: a row's baseline is the full state with exactly the keys that
// row's own contract holds back taken out of it.
func withoutMergeWaitKey(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		delete(issue.(map[string]any), MergeWaitField)
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its merge waits: %v", body, err)
	}
	return encoded
}

// withoutEntryTimeKeys is a state body with no issue carrying `entryQueuedAt`, `entryConfirmedAt`
// or `architectStalls`, which is what a caller below entryTimesContract is served of a state that
// carries them (api's withoutEntryTimes). It serves the table below as withoutQuietKeys does: a
// row's baseline is the full state with exactly the keys that row's own contract holds back taken
// out of it.
func withoutEntryTimeKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		issue := issue.(map[string]any)
		delete(issue, "entryQueuedAt")
		delete(issue, "entryConfirmedAt")
		delete(issue, "architectStalls")
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its entry times: %v", body, err)
	}
	return encoded
}

// withoutPausedKey is a state body with no `paused`, which is what a caller below pauseContract is
// served of a state (api's withoutPause). It serves the table below as withoutQuietKeys does.
func withoutPausedKey(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	delete(served, "paused")
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its pause: %v", body, err)
	}
	return encoded
}

// withoutHeldKey is a state body whose admission block carries no `held`, which is what a caller
// below heldContract is served of a state (api's withoutHeld). It serves the table below as
// withoutQuietKeys does: a row's baseline is the full state with exactly the keys that row's own
// contract holds back taken out of it.
func withoutHeldKey(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	delete(served["admission"].(map[string]any), "held")
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its held list: %v", body, err)
	}
	return encoded
}

// withoutEntryKeys is a state body with no issue carrying `phaseEntry` or `givenEntry`, which is
// what a caller below phaseEntriesContract is served of a state that carries them (api's
// withoutPhaseEntries). It serves the table below as withoutQuietKeys does: a row's baseline is
// the full state with exactly the keys that row's own contract holds back taken out of it.
func withoutEntryKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		issue := issue.(map[string]any)
		delete(issue, "phaseEntry")
		delete(issue, "givenEntry")
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its entry numbers: %v", body, err)
	}
	return encoded
}

// withoutLeakCheckCauses is a state body with no claim view carrying `leakCheckCause`, which is
// what a caller below leakCheckCauseContract is served of a state that carries one (api's
// withoutLeakCheckCause). It serves the table below as withoutQuietKeys does: a row's baseline
// is the full state with exactly the keys that row's own contract holds back taken out of it.
func withoutLeakCheckCauses(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		issue := issue.(map[string]any)
		views := []any{issue["architect"]}
		for _, worker := range issue["workers"].(map[string]any) {
			views = append(views, worker.(map[string]any)["claim"])
		}
		for _, view := range views {
			if view, ok := view.(map[string]any); ok {
				delete(view, "leakCheckCause")
			}
		}
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its leak-check causes: %v", body, err)
	}
	return encoded
}

// stripAnswerKeys takes the answer keys off one issue of a decoded state body: its `why`, and
// every field of its waiting-line view beside the `place` and the `length` waitingLineContract
// added before it. The two go together because they are one contract and one strip on the
// route itself (api's withoutWhyAndLineFields, whose doc says why they are one): a test that
// took one and left the other would hold the route to a body no caller is ever served.
//
// It is the one statement of that strip over a decoded body, and the two tests that must agree
// about it read it here: withoutAnswerKeys below, which takes the keys off a whole body, and
// the contract table of memory_test.go, which takes them off the issues of the body it expects.
func stripAnswerKeys(issue map[string]any) {
	delete(issue, "why")
	if line, carried := issue["waitingLine"].(map[string]any); carried {
		for name := range line {
			if name != "place" && name != "length" {
				delete(line, name)
			}
		}
	}
}

// withoutAnswerKeys is a state body with no issue carrying `why` and no member of the waiting
// line carrying a field beside its `place` and its `length`, which is what a caller below
// whyAndLineFieldsContract is served of a state that carries them (api's
// withoutWhyAndLineFields). It stands beside withoutQuietKeys for the one reason that helper
// stands here: a row of the table below is compared against the whole body, so the baseline it
// is read against is the full state with exactly the keys that row's own contract holds back
// taken out of it.
func withoutAnswerKeys(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	for _, issue := range served["issues"].(map[string]any) {
		stripAnswerKeys(issue.(map[string]any))
	}
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s without its answers: %v", body, err)
	}
	return encoded
}

// withRepositoriesAt152 is a state body whose `repositories` block is written in the shape contract
// 152 gave it from the body's own `live` entries and `caches`, which is what a caller below
// repositoriesLiveContract is served of a state (api's withoutRepositoriesLive). It serves the
// table below as withoutQuietKeys does: a row's baseline is the full state with exactly what that
// row's own contract holds back changed in it. Where each entry goes in that shape is read by
// TestTheRepositoriesBlockIsNarrowedBelowItsContract against a block written out by hand, and not
// here, which reads the shape from the same writer the route reads it from.
func withRepositoriesAt152(t *testing.T, body []byte) []byte {
	t.Helper()
	var served map[string]json.RawMessage
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	var block RepositoriesView
	if err := json.Unmarshal(served["repositories"], &block); err != nil {
		t.Fatalf("decode the state's repositories %s: %v", served["repositories"], err)
	}
	block.narrowed = true
	narrowed, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encode the repositories %+v in contract 152's shape: %v", block, err)
	}
	served["repositories"] = narrowed
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s with its repositories in contract 152's shape: %v", body, err)
	}
	return encoded
}

// A plugin below supervise.PodUpdateContract reads the state with a schema that knows no
// `updateRound` or `updateHead` and refuses the whole body for either, which is every legion
// tool call of a worker the update round is still serving (read_record, handoff_complete). The
// route authenticates nobody, so it knows that plugin by the pod the request came from: a pod
// whose claims record a contract below supervise.PodUpdateContract is served claim views with
// only the keys contract 45 knows, and the rest of the body as it is — the trees, the wakes the
// pacing holds, the count of claims the refresh lane has left, the gated steps, the claim
// views' step record, the places in the admission line, the workers' counts of the moves back,
// each issue's two entry numbers, the issues the record holds in the workflow, each issue's
// tree quiet time, what each reviewing issue's round waits on from GitHub, each issue's
// experiment assignments and the two times of each issue's entry task among the keys it is
// served without, and its repositories block in the shape contract 152 gave it, since such a pod
// is below every row of stateNarrowings. The measure of a tree
// that has stopped is read in finer grain by quiet_time_test.go, and the two times by
// architect_handshake_test.go. A pod at this contract, and a caller in no pod — the operator's
// `legion state`, whose open-rounds line reads both fields, whose queue line reads those wakes
// and whose tool-call line reads the step records — are served everything. The claims are the
// state snapshot's own, so there is no readable state whose caller's contract is unknown: a
// snapshot without them is no state, and the route refuses it.
func TestTheStateRouteServesAPodItsPluginContractsClaimViews(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(supervise.PodUpdateContract-1))
	fullWakes, fullCarried := wakesServed(t, full)
	fullCalls, _ := stepsServed(t, full)
	fullCount, fullCounted := claimsToRefreshServed(t, full)
	if !strings.Contains(string(full), `"updateRound"`) || !strings.Contains(string(full), `"updateHead"`) ||
		len(fullCalls) == 0 || !fullCarried || len(fullWakes) == 0 || !fullCounted || fullCount == 0 {
		t.Fatalf("the fixture's state %s carries no update round, no step record, no waiting wake or no claim left to refresh; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		rounds bool
		wakes  bool
		// stall is whether the caller is served the claim views' `step`, the record
		// stallRecordContract added at 57, so a pod at 57 or above is served the record and a pod
		// still running an older plugin is not.
		stall bool
		// refresh is whether the caller is served `claimsToRefresh`, the count
		// claimsToRefreshContract added at 73, so a pod at 73 or above is served the count and a
		// pod still running an older plugin is not.
		refresh bool
		// answers is whether the caller is served each issue's one rendered answer and the
		// waiting line's own fields beside each member's place (whyAndLineFieldsContract, the
		// contract 85 added). One strip takes both (withoutWhyAndLineFields), so one column
		// answers for the whole of that contract.
		answers bool
		// leak is whether the caller is served a claim view's `leakCheckCause`, the sentence a
		// launch try was refused with because the shared public leak check could not be
		// installed (leakCheckCauseContract, the contract 87 added).
		leak bool
		// entries is whether the caller is served each issue's `phaseEntry` and `givenEntry`
		// (phaseEntriesContract, the contract 94 added). The two leave together
		// (withoutPhaseEntries), so one column answers for both.
		entries bool
		// held is whether the caller is served `admission.held`, the issues the record holds in
		// the workflow that stand in neither `active` nor `waiting`, the list heldContract added
		// at 98, so a pod at 98 or above is served the key and a pod still running an older
		// plugin is not.
		held bool
		// quiet is whether the caller is served each issue's measure of its tree's quiet time,
		// `treeMovedAt` and `quietHours` (quietTimeContract, the contract 111 added), so a pod
		// at it or above is served the measure and a pod still running an older plugin is not.
		// The two leave together (withoutQuietTime), so one column answers for both.
		quiet bool
		// wait is whether the caller is served what each reviewing issue's round waits on from
		// GitHub, `mergeWait` (mergeWaitContract, the contract that added it), so a pod at it or
		// above is served the object and every older pod is not.
		wait bool
		// assigned is whether the caller is served each issue's experiment assignments
		// (assignmentsContract, the contract that added them), so a pod at it or above is served
		// them and every older pod is not.
		assigned bool
		// times is whether the caller is served each issue's `entryQueuedAt` and
		// `entryConfirmedAt`, and each root's `architectStalls` (entryTimesContract, the contract
		// that added them), so a pod at it or above is served them and every older pod is not.
		// The three leave together (withoutEntryTimes), so one column answers for all of them.
		times bool
		// pause is whether the caller is served the project's pause, `paused` here
		// (pauseContract, the contract that added it beside `lastPause`), so a pod at it or above
		// is served the key and every older pod is not.
		pause bool
		// repos is whether the caller is served the repositories block this contract writes,
		// `live` beside `caches` and `withoutWebhookApp` (repositoriesLiveContract), so a pod at
		// it or above is served that block and every older pod the block in contract 152's shape,
		// from the same entries.
		repos bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		// The band from recordFieldsContract to below whyAndLineFieldsContract. What the caller
		// is served is decided by its own recorded claim contract (podContract) and never by the
		// declared constant, which is why these rows name the contracts rather than the
		// declaration. Such a pod stands below whyAndLineFieldsContract, leakCheckCauseContract,
		// phaseEntriesContract and every later row of stateNarrowings, so it is served none of the
		// keys those rows add.
		{name: "a pod at recordFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(recordFieldsContract), rounds: true, wakes: true, stall: true, refresh: true},
		// The band at whyAndLineFieldsContract: the answers and the line's own fields are served
		// too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at whyAndLineFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(whyAndLineFieldsContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true},
		// The newest contract that strips a key of the claim views, which is what a pod must
		// record to be served every one of them; every key a later row of stateNarrowings adds is
		// held back from it.
		{name: "a pod at leakCheckCauseContract, the newest contract that strips a claim view's key", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true},
		// The band from phaseEntriesContract to below heldContract: the entry numbers are
		// served too, and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at phaseEntriesContract", remote: statePodAddr + ":41000", claims: podClaims(phaseEntriesContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true},
		// The band from heldContract to below quietTimeContract: the held list is served too,
		// and every key a later row of stateNarrowings adds is held back.
		{name: "a pod at heldContract", remote: statePodAddr + ":41000", claims: podClaims(heldContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true},
		// The band from quietTimeContract to below waitingOnIssueContract: the tree quiet time is
		// served too, and every key a later row of stateNarrowings adds is held back. 123,
		// INFRA-3146's contract, moves no shape of the state.
		{name: "a pod at quietTimeContract", remote: statePodAddr + ":41000", claims: podClaims(quietTimeContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true},
		// The band from waitingOnIssueContract to below mergeWaitContract: that contract's strip
		// takes a word of the line's `kind` and no key, so this row and the one before it read
		// alike here (waiting_on_issue_test.go reads that word), and `mergeWait` and every key a
		// later row adds are still held back.
		{name: "a pod at waitingOnIssueContract", remote: statePodAddr + ":41000", claims: podClaims(waitingOnIssueContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true},
		// The band from mergeWaitContract to below assignmentsContract: `mergeWait` is served
		// too, and the assignments and every key a later row adds are still held back.
		{name: "a pod at mergeWaitContract", remote: statePodAddr + ":41000", claims: podClaims(mergeWaitContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true},
		// The band from assignmentsContract to below entryTimesContract: the assignments are
		// served too, the entry times, the architect-stalled arms and the pause are held back,
		// and the repositories block is served in contract 152's shape, at the band's foot and
		// at its top alike.
		{name: "a pod at assignmentsContract", remote: statePodAddr + ":41000", claims: podClaims(assignmentsContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true},
		{name: "a pod just below entryTimesContract", remote: statePodAddr + ":41000", claims: podClaims(entryTimesContract - 1), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true},
		// The band from entryTimesContract to below pauseContract: the entry times and the arms
		// are served too, the pause is held back, and the repositories block is served in
		// contract 152's shape, at the band's foot and at its top alike.
		{name: "a pod at entryTimesContract", remote: statePodAddr + ":41000", claims: podClaims(entryTimesContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true},
		{name: "a pod just below pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract - 1), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true},
		// The band from pauseContract to below repositoriesLiveContract, the last band under the
		// declared contract: the pause is served too, and the repositories block is served in
		// contract 152's shape, at the band's foot and at its top alike.
		{name: "a pod at pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, pause: true},
		{name: "a pod just below repositoriesLiveContract", remote: statePodAddr + ":41000", claims: podClaims(repositoriesLiveContract - 1), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, pause: true},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, pause: true, repos: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, pause: true, repos: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(supervise.PodUpdateContract - 1), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, wait: true, assigned: true, times: true, pause: true, repos: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)

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
			// The mark of contract 75 is the one key a full body and the baseline part on: the
			// baseline is asked from no pod and carries the daemon's number alone, where a
			// caller at a contract is told its own beside it (TestANarrowedBodySaysSo). The
			// rest of the body is compared whole, against the baseline without the keys this
			// row's own contract holds back: a pod at recordFieldsContract is served every
			// claim view key the decided record added and none of the keys a later row of
			// stateNarrowings adds, and each pod above it is served the keys of its own row and of
			// every row below it, and none of a later row's.
			if tc.rounds && tc.stall {
				want := full
				if !tc.answers {
					want = withoutAnswerKeys(t, want)
				}
				if !tc.leak {
					want = withoutLeakCheckCauses(t, want)
				}
				if !tc.entries {
					want = withoutEntryKeys(t, want)
				}
				if !tc.held {
					want = withoutHeldKey(t, want)
				}
				if !tc.quiet {
					want = withoutQuietKeys(t, want)
				}
				if !tc.wait {
					want = withoutMergeWaitKey(t, want)
				}
				if !tc.assigned {
					want = withoutAssignmentKeys(t, want)
				}
				if !tc.times {
					want = withoutEntryTimeKeys(t, want)
				}
				if !tc.pause {
					want = withoutPausedKey(t, want)
				}
				if !tc.repos {
					want = withRepositoriesAt152(t, want)
				}
				if unmarked(t, body) != unmarked(t, want) {
					t.Fatalf("state = %s, want the state as this row's contract is served it, %s", body, want)
				}
				return
			}
			if !tc.rounds {
				for _, view := range claimViews(t, body) {
					for key := range view {
						if !slices.Contains(contract45ClaimView, key) {
							t.Errorf("claim view %v carries %q, which contract 45's strict reader refuses", view, key)
						}
					}
				}
			}
			// Nothing else of the body differs: it is the full state with the two claim keys gone,
			// with the three memory keys gone too, since a pod below this contract's own reader
			// refuses those as well (memory_test.go holds that half), with contract 51's two tree
			// keys — `admission.trees` and each issue's `tree` — gone (tree_test.go holds that
			// half), with `wakesWaiting` gone, the key a reader before contract 53 refuses
			// empty as surely as filled (withoutWakesWaiting), with the gated steps gone, the
			// field contract 55 added, with contract 56's retired entries under
			// `strandedClaims` gone (strandedWithoutRetired below), with each claim view's
			// `step` gone, the record contract 57 added (withoutStallRecord), with the
			// places in the admission line gone, the field contract 58 added, with the
			// workers' counts of the moves back gone, the field contract 62 added
			// (withoutBackwardMoves), with `claimsToRefresh` gone, the count a reader before
			// contract 73 refuses as surely when it is 0 (withoutClaimsToRefresh), with the
			// nine keys contract 75 added gone — a claim view's `servingGeneration`,
			// `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and `pendingCause`,
			// and an issue's `heldFrom`, `lingerUntil` and `ancestry` (withoutRecordFields), with
			// the mark of that same contract gone with them, with each issue's one rendered
			// answer gone, the `why` contract 85 added beside the line's own fields, which go
			// with the `waitingLine` key itself for a caller this far back
			// (withoutWhyAndLineFields), with each claim view's `leakCheckCause` gone, the
			// sentence contract 87 added (withoutLeakCheckCause), with each issue's `phaseEntry`
			// and `givenEntry` gone, the two keys contract 94 added (withoutPhaseEntries), with
			// `admission.held` gone, the list contract 98 added (withoutHeld, held_test.go holds
			// that half), with the two keys contract 111 added gone, each issue of a live tree's
			// `treeMovedAt` and `quietHours` (withoutQuietTime), with what each reviewing issue's
			// round waits on from GitHub gone, the object contract 133 added (withoutMergeWait),
			// with each issue's `assignments` gone, the experiment answers contract 141 added
			// (withoutAssignments), with each issue's `entryQueuedAt`, `entryConfirmedAt` and
			// `architectStalls` gone, the three keys entryTimesContract added (withoutEntryTimes),
			// with the project's pause gone, the `paused` contract 178 added (withoutPause), and
			// with the repositories block in contract 152's shape, the block
			// repositoriesLiveContract replaced (withoutRepositoriesLive).
			// Every row that reaches here is below all of them, so every one of them goes whatever
			// the row is; contract 132's strip takes a word of the line's `kind` and no key, and
			// this fixture stands no member of that kind.
			var want, got map[string]any
			if err := json.Unmarshal(withRepositoriesAt152(t, full), &want); err != nil {
				t.Fatal(err)
			}
			if !tc.wakes {
				delete(want, "wakesWaiting")
			}
			if !tc.refresh {
				delete(want, ClaimsToRefreshField)
			}
			delete(want, "contract")
			delete(want, "paused")
			if !tc.rounds {
				delete(want["admission"].(map[string]any), "trees")
				want["strandedClaims"] = strandedWithoutRetired(t, want["strandedClaims"])
			}
			if !tc.held {
				delete(want["admission"].(map[string]any), "held")
			}
			for _, issue := range want["issues"].(map[string]any) {
				issue := issue.(map[string]any)
				for _, key := range []string{"heldFrom", "lingerUntil", "ancestry"} {
					delete(issue, key)
				}
				// Every row that reaches here is below phaseEntriesContract too, so the two
				// entry numbers, the two times of their task and the architect-stalled arms go
				// whatever the row is, as the decided record's keys above do.
				delete(issue, "phaseEntry")
				delete(issue, "givenEntry")
				delete(issue, "entryQueuedAt")
				delete(issue, "entryConfirmedAt")
				delete(issue, "architectStalls")
				if !tc.quiet {
					delete(issue, TreeMovedAtField)
					delete(issue, QuietHoursField)
				}
				views := []any{issue["architect"]}
				for _, worker := range issue["workers"].(map[string]any) {
					worker := worker.(map[string]any)
					if !tc.rounds {
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
					for _, key := range []string{"servingGeneration", "updateReasons", "providerErrorRun", "pendingTask", "failure", "pendingCause", "leakCheckCause"} {
						delete(view, key)
					}
				}
				if tc.rounds {
					continue
				}
				delete(issue, "memoryGroup")
				delete(issue, "memoryDrawMode")
				delete(issue, "memoryDrawnAt")
				delete(issue, "tree")
				delete(issue, "gatedSteps")
				delete(issue, "waitingLine")
				delete(issue, "assignments")
				delete(issue, "why")
				delete(issue, MergeWaitField)
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("state = %s, want the full state without those keys, %s", gotJSON, wantJSON)
			}
		})
	}
}

// strandedWithoutRetired is the `strandedClaims` of a baseline body with its retired entries gone,
// the report a pod below retiredStrandedContract is served (api's withoutRetiredStranded). The key
// is never null, so a body every entry of which is retired keeps it as an empty array.
func strandedWithoutRetired(t *testing.T, stranded any) []any {
	t.Helper()
	rows, ok := stranded.([]any)
	if !ok {
		t.Fatalf("the state's strandedClaims is %T, want the array the route always writes", stranded)
	}
	kept := []any{}
	for _, row := range rows {
		if row.(map[string]any)["state"] == string(supervise.StateRetired) {
			continue
		}
		kept = append(kept, row)
	}
	return kept
}

// strandedServed is the report of claims nothing will wake of a state body, entry for entry as
// the plugin's reader sees it, and whether `strandedClaims` is on the wire at all: the key is
// never absent and never null, an emptied report included.
func strandedServed(t *testing.T, body []byte) (stranded []map[string]string, carried bool) {
	t.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	raw, carried := state["strandedClaims"]
	if !carried {
		return nil, false
	}
	if err := json.Unmarshal(raw, &stranded); err != nil {
		t.Fatalf("decode strandedClaims %s: %v", raw, err)
	}
	return stranded, stranded != nil
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below retiredStrandedContract is served the report of claims
// nothing will wake without its retired entries, since that pod's plugin parses
// `strandedClaims[].state` with a strict enum of `suspended` and `failed` and refuses the whole
// state body for a third value; every entry whose state it knows stays, in the order the report
// named it. A pod at this contract, a pod whose claims record no contract at all, and a caller
// the daemon cannot place in a pod —
// the operator's `legion state`, which names a retired claim under the workflow start it needs —
// are served every entry.
func TestTheStateRouteServesAPodItsPluginContractsStrandedClaims(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(retiredStrandedContract-1))
	every, carried := strandedServed(t, full)
	var kept []map[string]string
	for _, entry := range every {
		if entry["state"] != string(supervise.StateRetired) {
			kept = append(kept, entry)
		}
	}
	if !carried || len(kept) == len(every) || len(kept) == 0 {
		t.Fatalf("the fixture's state %s names no retired claim nothing will wake beside claims in other states; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name    string
		remote  string
		claims  []supervise.Claim
		retired bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(retiredStrandedContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(retiredStrandedContract - 1)},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion), retired: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), retired: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(retiredStrandedContract - 1), retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)
			stranded, carried := strandedServed(t, body)

			want := kept
			if tc.retired {
				want = every
			}
			if !carried {
				t.Fatalf("the state %s carries no strandedClaims; the key is never absent and never null", body)
			}
			if !reflect.DeepEqual(stranded, want) {
				t.Fatalf("the claims nothing will wake = %v, want %v", stranded, want)
			}
		})
	}
}

// retiredStrandedContract is the contract whose entry in version.go added `retired` to the states
// a stranded claim may be in, and this daemon's own contract is never below it: a caller at this
// contract is served the retired entries.
func TestTheRetiredStrandedContractIsTheContractThatAddedTheState(t *testing.T) {
	const addedRetiredStranded = 56
	if retiredStrandedContract != addedRetiredStranded {
		t.Fatalf("retiredStrandedContract = %d, want %d, the contract whose entry in version.go added `retired` to a stranded claim's state",
			retiredStrandedContract, addedRetiredStranded)
	}
	if retiredStrandedContract > DaemonAPIVersion {
		t.Fatalf("retiredStrandedContract = %d, above DaemonAPIVersion %d: no caller could be served a retired claim",
			retiredStrandedContract, DaemonAPIVersion)
	}
}

// The body a pod a contract behind is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, update rounds and all.
func TestTheStateServedToAPodAContractBehindLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(supervise.PodUpdateContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if strings.Contains(string(behind), `"updateRound"`) {
		t.Fatalf("the pod a contract behind was served %s, want no updateRound", behind)
	}
	if !strings.Contains(string(operator), `"updateRound"`) || !strings.Contains(string(operator), `"updateHead"`) {
		t.Fatalf("the operator after it was served %s, want the update rounds the source answered", operator)
	}
}

// repositoriesServed is the `repositories` block of a state body, and its keys, sorted. The keys
// are read apart because they alone say which shape a caller was served: the reader of contract
// 152 requires `owners`, `mapped`, `refused` and `withoutWebhookApp` beside `caches` and knows no
// `live`, and this contract's block holds `live` beside `caches` and `withoutWebhookApp`, and none
// of the other three.
func repositoriesServed(t *testing.T, body []byte) ([]string, json.RawMessage) {
	t.Helper()
	block := keysOf(t, body)["repositories"]
	return slices.Sorted(maps.Keys(keysOf(t, block))), block
}

// sameJSON is whether got and want encode one JSON value, whatever the spacing and the key order
// of either.
func sameJSON(t *testing.T, got []byte, want any) bool {
	t.Helper()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode %+v: %v", want, err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal(encoded, &wantValue); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	return reflect.DeepEqual(gotValue, wantValue)
}

// populatedRepositoriesAt152 is populatedState's repositories block as a caller below
// repositoriesLiveContract is served it, written out by hand from the fixture's five entries
// rather than by the writer under test: owners and mapped are the three both role Apps are
// installed on, sjawhar/legion under the name its pair serves it as, cybermonklabs/legion, and
// every `default` false; refused is acme/gadgets, for the review App GitHub answered is not
// installed on it, and zeta/archive, for its read GitHub did not answer, each under its own name;
// withoutWebhookApp and caches are the fixture's own.
func populatedRepositoriesAt152(t *testing.T) repositoriesAt152 {
	t.Helper()
	block := populatedState(t).Repositories
	entry := func(repo string) LiveRepositoryView {
		for _, live := range block.Live {
			if live.Repo == repo {
				return live
			}
		}
		t.Fatalf("the fixture's repositories %+v name no %s; this test proves nothing", block.Live, repo)
		return LiveRepositoryView{}
	}
	both := AppInstallationsView{Implement: true, Review: true}
	return repositoriesAt152{
		Owners: []string{"acme", "cybermonklabs", "zeta"},
		Mapped: []servedRepositoryAt152{
			{Repo: "acme/widgets", Apps: both, Cache: entry("acme/widgets").Cache},
			{Repo: "cybermonklabs/legion", Apps: both},
			{Repo: "zeta/tools", Apps: both, Cache: entry("zeta/tools").Cache},
		},
		Refused: []refusedRepositoryAt152{
			{Repo: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets", Apps: &AppInstallationsView{Implement: true}},
			{Repo: "zeta/archive", Reason: entry("zeta/archive").ReadError},
		},
		WithoutWebhookApp: block.WithoutWebhookApp,
		Caches:            block.Caches,
	}
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below repositoriesLiveContract is served the repositories block
// in the shape contract 152 gave it, since that pod's plugin reads the block with a schema that
// requires `owners`, `mapped`, `refused` and `withoutWebhookApp` and knows no `live`, and would
// refuse the whole state for the block this contract writes. That block is written from the same
// entries (populatedRepositoriesAt152 says where each goes). A pod at this contract, a pod whose
// claims record no contract at all, and a caller the daemon cannot place in a pod — the
// operator's `legion state` and `legion controller start` — are served `live`, `caches` and
// `withoutWebhookApp`, the entries as the source answered them, the last in contract 152's shape
// and the same list a pod below the contract is served, since a `legion state` of 152 reads its
// webhook App lines from it and the daemon cannot narrow the body for that caller.
//
// The control is the property put back: with repositoriesLiveContract's row taken out of
// stateNarrowings, each row of a pod below the contract fails here, served `live` and none of
// `owners`, `mapped` and `refused`, which its reader requires; and with `withoutWebhookApp` not
// written on this contract's block, each row of a caller served it fails here.
func TestTheRepositoriesBlockIsNarrowedBelowItsContract(t *testing.T) {
	full := populatedState(t).Repositories
	if len(full.Live) == 0 || len(full.Caches) == 0 {
		t.Fatalf("the fixture's repositories %+v name no live repository or no cache; this test proves nothing", full)
	}
	narrowed := populatedRepositoriesAt152(t)
	if len(narrowed.Mapped) == 0 || len(narrowed.Refused) == 0 || len(narrowed.WithoutWebhookApp) == 0 {
		t.Fatalf("the fixture's repositories %+v leave one of contract 152's lists empty; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		live   bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(repositoriesLiveContract - 1)},
		{name: "a pod at contract 152, whose schema of the block this one replaced", remote: statePodAddr + ":41000", claims: podClaims(152)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(repositoriesLiveContract - 1)},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(repositoriesLiveContract), live: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), live: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(repositoriesLiveContract - 1), live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, block := repositoriesServed(t, stateFrom(t, populatedState(t), tc.remote, tc.claims))
			if !tc.live {
				if want := []string{"caches", "mapped", "owners", "refused", "withoutWebhookApp"}; !slices.Equal(keys, want) {
					t.Fatalf("the repositories block %s carries the keys %q, want contract 152's %q", block, keys, want)
				}
				if !sameJSON(t, block, narrowed) {
					t.Fatalf("the repositories block %s, want contract 152's shape of the fixture's entries, %+v", block, narrowed)
				}
				return
			}
			if want := []string{"caches", "live", "withoutWebhookApp"}; !slices.Equal(keys, want) {
				t.Fatalf("the repositories block %s carries the keys %q, want %q", block, keys, want)
			}
			if !sameJSON(t, block, full) {
				t.Fatalf("the repositories block %s, want the fixture's %+v", block, full)
			}
		})
	}

	// Each reason contract 152's `refused` gives, on entries the fixture does not hold: GitHub's
	// answer not come yet, the Apps not installed on the name the entry is served under (both of
	// them, here, on a pair's other name), and the entry's read error, which comes before the
	// missing App; and an entry both Apps are installed on is mapped whatever read error it
	// carries, as the narrowed block's own rule has it.
	t.Run("each refused entry says why, and an entry both Apps are installed on is mapped", func(t *testing.T) {
		state := populatedState(t)
		state.Repositories = RepositoriesView{Live: []LiveRepositoryView{
			{Repo: "acme/fresh"},
			{Repo: "acme/stale", ReadError: "the name and default branch of acme/stale could not be read: GitHub answered 502", Apps: &AppInstallationsView{Implement: true, Review: true}},
			{Repo: "sjawhar/moved", ServedAs: "cybermonklabs/moved", Apps: &AppInstallationsView{}},
			{Repo: "zeta/gone", ReadError: "GitHub answered 404 for zeta/gone", Apps: &AppInstallationsView{Review: true}},
		}}
		_, block := repositoriesServed(t, stateFrom(t, state, statePodAddr+":41000", podClaims(repositoriesLiveContract-1)))
		want := repositoriesAt152{
			Owners: []string{"acme"},
			Mapped: []servedRepositoryAt152{{Repo: "acme/stale", Apps: AppInstallationsView{Implement: true, Review: true}}},
			Refused: []refusedRepositoryAt152{
				{Repo: "acme/fresh", Reason: "GitHub has not answered for it yet"},
				{Repo: "sjawhar/moved", Reason: "the implement and review Apps are not installed on cybermonklabs/moved", Apps: &AppInstallationsView{}},
				{Repo: "zeta/gone", Reason: "GitHub answered 404 for zeta/gone", Apps: &AppInstallationsView{Review: true}},
			},
			WithoutWebhookApp: []WebhookAppGapView{},
			Caches:            []CacheView{},
		}
		if !sameJSON(t, block, want) {
			t.Fatalf("the repositories block %s, want %+v", block, want)
		}
	})
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// this contract, is served the repositories block as the source answered it, `live` and all.
//
// The control is a narrowing that marks the source's own block rather than the copy it serves (the
// block held by pointer, say): the operator after the pod is then served contract 152's shape, and
// this fails.
func TestTheStateServedToAPodBeforeTheRepositoriesContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(repositoriesLiveContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if keys, block := repositoriesServed(t, behind); slices.Contains(keys, "live") {
		t.Fatalf("the pod before the contract was served the repositories %s, want contract 152's shape", block)
	}
	if keys, block := repositoriesServed(t, operator); !slices.Equal(keys, []string{"caches", "live", "withoutWebhookApp"}) || !sameJSON(t, block, populatedState(t).Repositories) {
		t.Fatalf("the operator after it was served the repositories %s, want the entries the source answered", block)
	}
}

// repositoriesLiveContract is the contract whose entry in version.go changed the block, and this
// daemon's own contract is never below it: a caller at this contract is served `live`. The control
// is either number moved: a constant left at 152, or renumbered without its entry, fails the
// first check, and a DaemonAPIVersion renumbered below it fails the second.
func TestTheRepositoriesContractIsTheContractThatChangedTheBlock(t *testing.T) {
	const changedTheBlock = 196
	if repositoriesLiveContract != changedTheBlock {
		t.Fatalf("repositoriesLiveContract = %d, want %d, the contract whose entry in version.go made the block `live`, `caches` and `withoutWebhookApp`",
			repositoriesLiveContract, changedTheBlock)
	}
	if repositoriesLiveContract > DaemonAPIVersion {
		t.Fatalf("repositoriesLiveContract = %d, above DaemonAPIVersion %d: no caller could be served `live`",
			repositoriesLiveContract, DaemonAPIVersion)
	}
}
