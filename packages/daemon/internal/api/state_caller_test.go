package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
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

// asked is a GET of target on handler, asked from remote with no credential of any kind, and
// stating contract in pluginContractHeader where it is positive. It is the one request these
// tests make: stateAsked and stateStating ask the state route with it, recordAsked the record
// route, and callerKind.askedBody either of the two as a caller of its own kind.
func asked(t *testing.T, handler http.Handler, remote, target string, contract int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = remote
	if contract > 0 {
		request.Header.Set(pluginContractHeader, strconv.Itoa(contract))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// stateAsked is GET /legion/v1/state on handler, asked from remote and stating no contract.
func stateAsked(t *testing.T, handler http.Handler, remote string) []byte {
	t.Helper()
	return stateStating(t, handler, remote, 0)
}

// stateFrom is GET /legion/v1/state asked from remote of a server of its own.
func stateFrom(t *testing.T, state State, remote string, claims []supervise.Claim) []byte {
	t.Helper()
	return stateAsked(t, stateServer(t, state, claims), remote)
}

// podClaims is the claims of the pod at statePodAddr, each recording contract, beside a claim in
// another pod at this daemon's contract, which says nothing of the caller.
func podClaims(contract int) []supervise.Claim {
	return []supervise.Claim{
		claimInPod("legion-legion-legion-208-planner", statePodUID, contract),
		claimInPod("legion-legion-legion-208-implementer", statePodUID, contract),
		claimInPod("legion-legion-legion-210-planner", "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34", DaemonAPIVersion),
	}
}

// claimInPod is one claim of pod whose process runs there, recording contract; a contract of 0 is
// a claim that records none. The locator is what puts it in the pod, which is why a claim with
// none is in no pod's reckoning (api's podContracts).
func claimInPod(token claim.Token, pod string, contract int) supervise.Claim {
	return supervise.Claim{Token: token, State: supervise.StateReady, PluginContract: contract, Locator: &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: pod + "/1",
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-208", PodUID: pod, Container: "roles", Generation: 1},
	}}
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

// servedAt is a state body with the shapes applied that a caller at contract is served beside the
// keys a row of the table below names column by column: each claim view without the four keys
// shapedAnswersContract added (api's withoutClaimRoundReport) where contract is below it, and
// what servedShape applies. It serves the table as withoutQuietKeys does: a row's baseline is the
// full state with exactly the keys that row's own contract holds back taken out of it.
func servedAt(t *testing.T, body []byte, contract int) []byte {
	t.Helper()
	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	if contract < shapedAnswersContract {
		for _, issue := range served["issues"].(map[string]any) {
			issue := issue.(map[string]any)
			views := []any{issue["architect"]}
			for _, worker := range issue["workers"].(map[string]any) {
				views = append(views, worker.(map[string]any)["claim"])
			}
			for _, view := range views {
				if view, ok := view.(map[string]any); ok {
					for _, key := range roundReportKeys {
						delete(view, key)
					}
				}
			}
		}
	}
	servedShape(served, contract)
	encoded, err := json.Marshal(served)
	if err != nil {
		t.Fatalf("encode the state %s as contract %d is served it: %v", body, contract, err)
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
// each issue's two entry numbers, the issues the record holds in the workflow and each issue's
// tree quiet time among the keys it is served without, since such a pod is below every row of
// stateNarrowings. The measure of a tree that has stopped is read in finer grain by
// quiet_time_test.go. A pod at this contract, and a caller in no pod — the operator's
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
		// `treeMovedAt` and `quietHours` (quietTimeContract, the contract 111 added), so a pod at
		// 111 or above is served the measure and a pod still running an older plugin is not. The
		// two leave together (withoutQuietTime), so one column answers for both.
		quiet bool
		// contract is the contract a row that reaches the whole-body comparison is shaped at:
		// the one the row's claims record, and this daemon's own where no claim of the caller's
		// pod records one and where the caller is in no pod. It decides what servedAt applies,
		// which no column of its own carries.
		contract int
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		// The band from recordFieldsContract to below whyAndLineFieldsContract. What the caller
		// is served is decided by its own recorded claim contract (podContracts) and never by the
		// declared constant, which is why these rows name the contracts rather than the
		// declaration. Such a pod stands below whyAndLineFieldsContract, leakCheckCauseContract,
		// phaseEntriesContract, heldContract, quietTimeContract and shapedAnswersContract, the
		// six bands above it, so the issues it is served carry no answer, no entry number and no
		// tree quiet time, its claim views no leak check's cause and none of the four keys
		// contract 118 added, and its admission block no held list.
		{name: "a pod at recordFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(recordFieldsContract), rounds: true, wakes: true, stall: true, refresh: true, contract: recordFieldsContract},
		// The band at whyAndLineFieldsContract: the answers and the line's own fields are served
		// too, and the leak check's cause, the entry numbers, the held list, the tree quiet time
		// and the claim views' keys of contract 118 are held back.
		{name: "a pod at whyAndLineFieldsContract", remote: statePodAddr + ":41000", claims: podClaims(whyAndLineFieldsContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, contract: whyAndLineFieldsContract},
		// The band at leakCheckCauseContract: the cause is served too, and the entry numbers,
		// the held list, the tree quiet time and the claim views' keys of contract 118 are the
		// four bands left above it.
		{name: "a pod at leakCheckCauseContract", remote: statePodAddr + ":41000", claims: podClaims(leakCheckCauseContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, contract: leakCheckCauseContract},
		// The band from phaseEntriesContract to below heldContract: the entry numbers are
		// served too, and the held list, the tree quiet time and the claim views' keys of
		// contract 118 are the three bands left above it.
		{name: "a pod at phaseEntriesContract", remote: statePodAddr + ":41000", claims: podClaims(phaseEntriesContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, contract: phaseEntriesContract},
		// The band from heldContract to below quietTimeContract: the held list is served too,
		// and the tree quiet time and the claim views' keys of contract 118 are the two bands
		// left above it.
		{name: "a pod at heldContract", remote: statePodAddr + ":41000", claims: podClaims(heldContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, contract: heldContract},
		// The band from quietTimeContract to below shapedAnswersContract: the tree quiet time is
		// served too, and the claim views' keys of contract 118 are the one band left above it.
		{name: "a pod at quietTimeContract", remote: statePodAddr + ":41000", claims: podClaims(quietTimeContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: quietTimeContract},
		// The newest contract the route serves a body down by, which is what a pod must record
		// to be served every key. What the caller is served is decided by its own recorded claim
		// contract (podContracts) and never by the declared constant, which is why this row
		// names shapedAnswersContract rather than the declaration.
		{name: "a pod at the newest contract the route narrows by", remote: statePodAddr + ":41000", claims: podClaims(shapedAnswersContract), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: shapedAnswersContract},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: DaemonAPIVersion},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(supervise.PodUpdateContract - 1), rounds: true, wakes: true, stall: true, refresh: true, answers: true, leak: true, entries: true, held: true, quiet: true, contract: DaemonAPIVersion},
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
			// The mark of contract 75 and the fleet's lowest plugin contract are the two keys a
			// full body and the baseline part on (withoutLowestContract): the baseline is asked
			// from no pod and carries the daemon's number alone, where a caller at a contract is
			// told its own beside it (TestANarrowedBodySaysSo), and the lowest contract reports
			// the snapshot's claims, which are this row's own. The rest of the body is compared
			// whole, against the baseline without the keys this row's own contract holds back: a
			// pod at recordFieldsContract is served every claim view key the decided record added
			// and neither the issues' answers, nor the leak check's cause, nor the entry numbers,
			// nor the held list, nor the tree quiet time, nor the claim views' keys of contract
			// 118, the six bands between it and this daemon's own; a pod at
			// whyAndLineFieldsContract is served the answers and none of the other five; a pod at
			// leakCheckCauseContract is served the cause and none of the four above it; a pod at
			// phaseEntriesContract is served the entry numbers and none of the three above it; a
			// pod at heldContract is served the held list and neither the measure nor the claim
			// views' keys of contract 118; and a pod at quietTimeContract is served the measure
			// and not those keys.
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
				want = servedAt(t, want, tc.contract)
				if withoutLowestContract(t, body) != withoutLowestContract(t, want) {
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
			// that half), and with the two keys contract 111 added gone, each issue of a live
			// tree's `treeMovedAt` and `quietHours` (withoutQuietTime). Every row that reaches
			// here is below all of them, so those strips take their keys whatever the row
			// is.
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
				// entry numbers go whatever the row is, as the decided record's keys above do.
				delete(issue, "phaseEntry")
				delete(issue, "givenEntry")
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
					// Every row that reaches here is below shapedAnswersContract, below
					// leakCheckCauseContract and below recordFieldsContract too, so none of its
					// callers is served the four keys contract 118 added to a claim view, the
					// `leakCheckCause` contract 87 added or the six the decided record added.
					for _, key := range roundReportKeys {
						delete(view, key)
					}
					delete(view, "leakCheckCause")
					for _, key := range recordFieldClaimKeys {
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
				delete(issue, "why")
			}
			// Every row that reaches here is below supervise.PodUpdateContract, which is below
			// both of the contracts servedShape reads: such a caller is served each check
			// without `role` and both fix-attempt stand-ins.
			servedShape(want, supervise.PodUpdateContract-1)
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			// The fleet's lowest plugin contract is the snapshot's claims, which this row's own
			// claims are (withoutLowestContract): it is no key the caller's contract decides.
			delete(want, "lowestPluginContract")
			delete(got, "lowestPluginContract")
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

// withoutLowestContract is a state body as a string with `lowestPluginContract` and the
// `contract` mark left out, which is how two bodies of different claims are compared for their
// shape. `lowestPluginContract` reports the claims of the snapshot the body was projected from
// (State.LowestPluginContract) and nothing the caller's own contract decides, and the mark
// reports the contract the answer was shaped to (ContractView), so two bodies served from
// different claims differ in both by design and a comparison about the shaping reads neither.
// What each caller is told in the mark is held by narrowing_test.go.
func withoutLowestContract(t *testing.T, body []byte) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	delete(decoded, "lowestPluginContract")
	delete(decoded, "contract")
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode the state: %v", err)
	}
	return string(encoded)
}

// servedShape applies to want, a body decoded from the state this contract writes, the shapes
// that are no strip of a key this contract writes, and the one band whose keys this contract
// writes and an older caller is served without. A caller below checkRoleContract is served each
// issue's check without `role` (api's withoutCheckRole); a caller below fixAttemptsContract is
// served `fixAttempts` of 0 on each pull request and `max_fix_attempts` of 3 in the settings
// block (withFixAttempts, withMaxFixAttempts); and a caller below stateMachineContract is served
// the issue without the seven keys that contract added (withoutStateMachineFields). One contract
// decides the stand-ins, because the two sides of a caller's shaping part only below
// ServingFloor, which is below fixAttemptsContract as every number this is called with is.
func servedShape(want map[string]any, contract int) {
	if contract < fixAttemptsContract {
		want["settings"].(map[string]any)["max_fix_attempts"] = float64(3)
	}
	for _, issue := range want["issues"].(map[string]any) {
		issue := issue.(map[string]any)
		if pull, carries := issue["pullRequest"].(map[string]any); carries {
			if contract < fixAttemptsContract {
				pull["fixAttempts"] = float64(0)
			}
			if contract < stateMachineContract {
				delete(pull, "mergeState")
			}
		}
		if check, carries := issue["check"].(map[string]any); carries && contract < checkRoleContract {
			delete(check, "role")
		}
		if contract >= stateMachineContract {
			continue
		}
		if architect, carries := issue["architect"].(map[string]any); carries {
			deleteTurnRecord(architect)
		}
		workers, carries := issue["workers"].(map[string]any)
		if !carries {
			continue
		}
		for _, worker := range workers {
			worker := worker.(map[string]any)
			delete(worker, "pass")
			if view, held := worker["claim"].(map[string]any); held {
				deleteTurnRecord(view)
			}
		}
	}
}

// deleteTurnRecord takes off one decoded claim view the five keys stateMachineContract added to
// it, which a caller below that contract is served without (api's stripTurnRecord).
func deleteTurnRecord(view map[string]any) {
	for _, key := range []string{"lastTurnStart", "lastTurnEnd", "lastDeathCause", "lastStartFailure", "lastReady"} {
		delete(view, key)
	}
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
