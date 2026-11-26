package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// shapedServer is stateServer with its log captured, so a test reads the one line each of the
// outer two edges writes and proves the quiet of every caller between them. placesCallers is
// whether the daemon can place a caller in a pod at all: under runtime tmux Options.CallerPod is
// nil, and no caller there is in any pod.
func shapedServer(t *testing.T, state State, claims []supervise.Claim, logs *bytes.Buffer, placesCallers bool) http.Handler {
	t.Helper()
	options := stateOptions(state, claims)
	// Warnings alone, which are the two edge lines servedContracts writes and what every row
	// below counts. The record of each band the shaping leaves out of a state is an info line of
	// its own, and narrowing_test.go is what reads those (recordShaping).
	options.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if !placesCallers {
		options.CallerPod = nil
	}
	return NewServer("127.0.0.1", 8437, options).Handler
}

// stateStating is GET /legion/v1/state asked from remote, stating contract in
// pluginContractHeader where it is positive and stating none where it is zero (asked).
func stateStating(t *testing.T, handler http.Handler, remote string, contract int) []byte {
	t.Helper()
	recorder := asked(t, handler, remote, "/legion/v1/state", contract)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// servedShapeOf is what a state body carries of the three keys this change moves: the
// `fixAttempts` of each pull request, the settings block's `max_fix_attempts` and the `role` of
// each check, each absent where the body leaves the key off. The counts of the pull requests and
// the checks come with them, so a test tells a body that left a key off from one that had
// nothing to carry it.
type servedShapeOf struct {
	pullRequests, checks int
	fixAttempts          []int
	checkRoles           []string
	maxFixAttempts       *int
}

func shapeOf(t *testing.T, body []byte) servedShapeOf {
	t.Helper()
	var state struct {
		Settings struct {
			MaxFixAttempts *int `json:"max_fix_attempts"`
		} `json:"settings"`
		Issues map[string]struct {
			PullRequest *struct {
				FixAttempts *int `json:"fixAttempts"`
			} `json:"pullRequest"`
			Check *struct {
				Role *string `json:"role"`
			} `json:"check"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	served := servedShapeOf{maxFixAttempts: state.Settings.MaxFixAttempts}
	for _, issue := range state.Issues {
		if issue.PullRequest != nil {
			served.pullRequests++
			if issue.PullRequest.FixAttempts != nil {
				served.fixAttempts = append(served.fixAttempts, *issue.PullRequest.FixAttempts)
			}
		}
		if issue.Check != nil {
			served.checks++
			if issue.Check.Role != nil {
				served.checkRoles = append(served.checkRoles, *issue.Check.Role)
			}
		}
	}
	return served
}

// wantStandIns holds body to what a caller below both contracts reads: `fixAttempts` of 0 on
// every pull request, `max_fix_attempts` of 3, and no check carrying `role`.
func wantStandIns(t *testing.T, body []byte) {
	t.Helper()
	served := shapeOf(t, body)
	if served.pullRequests == 0 || served.checks == 0 {
		t.Fatalf("the fixture's state %s carries no pull request or no check; this proves nothing", body)
	}
	if len(served.fixAttempts) != served.pullRequests {
		t.Errorf("%d of the %d pull requests of %s carry `fixAttempts`, want every one", len(served.fixAttempts), served.pullRequests, body)
	}
	for _, attempts := range served.fixAttempts {
		if attempts != 0 {
			t.Errorf("`fixAttempts` = %d in %s, want the 0 the stopped column takes on an insert", attempts, body)
		}
	}
	if served.maxFixAttempts == nil || *served.maxFixAttempts != 3 {
		t.Errorf("`max_fix_attempts` = %v in %s, want the 3 the setting defaulted to", served.maxFixAttempts, body)
	}
	if len(served.checkRoles) != 0 {
		t.Errorf("%d checks of %s carry `role`, which the reader before checkRoleContract refuses", len(served.checkRoles), body)
	}
}

// wantThisContract holds body to what this contract writes: no `fixAttempts`, no
// `max_fix_attempts`, and `role` on every check.
func wantThisContract(t *testing.T, body []byte) {
	t.Helper()
	served := shapeOf(t, body)
	if served.pullRequests == 0 || served.checks == 0 {
		t.Fatalf("the fixture's state %s carries no pull request or no check; this proves nothing", body)
	}
	if len(served.fixAttempts) != 0 {
		t.Errorf("%d pull requests of %s carry `fixAttempts`, which this contract no longer writes", len(served.fixAttempts), body)
	}
	if served.maxFixAttempts != nil {
		t.Errorf("`max_fix_attempts` = %d in %s, which this contract no longer writes", *served.maxFixAttempts, body)
	}
	if len(served.checkRoles) != served.checks {
		t.Errorf("%d of the %d checks of %s carry `role`, want every one", len(served.checkRoles), served.checks, body)
	}
}

// A caller at ServingFloor — one that states it, and one in a pod whose claims all record it —
// is served the two keys contract 59 removed, as the stand-ins its own reader requires, and no
// check's `role`, the key contract 54 added. A caller at this daemon's own contract is served
// the body this contract writes: neither stand-in, and every check's role.
func TestACallerAtTheFloorIsServedTheStandInsAndNoCheckRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
	}{
		{name: "a caller that states the floor", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion()), stated: ServingFloor},
		{name: "a pod whose claims record the floor", remote: statePodAddr + ":41000", claims: podClaims(ServingFloor)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			handler := shapedServer(t, populatedState(t), tc.claims, logs, true)

			wantStandIns(t, stateStating(t, handler, tc.remote, tc.stated))

			if logs.Len() != 0 {
				t.Errorf("a caller at the floor logged %s, want no line: the floor is a shape this daemon describes", logs)
			}
		})
	}

	logs := &bytes.Buffer{}
	handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion()), logs, true)
	wantThisContract(t, stateStating(t, handler, statePodAddr+":41000", 0))
	if logs.Len() != 0 {
		t.Errorf("a pod at this contract logged %s, want no line", logs)
	}
}

// The mixed pod: a caller that states no contract, in a pod whose claims record 46 and 73 at
// once, is served `check.role`, the one key added between those two contracts that the newer
// process requires, every other key added between them, which that process reads as optional,
// and the stand-ins for the keys removed after 46, which the older process requires. The pod's
// third claim records no contract and moves neither half. A caller of that same pod that states
// its own contract is served that contract's shape, whichever of the two it states.
//
// Both builds of this pod read the body the pod reading decides: neither states a contract of
// its own, so each is served what the pod says. No one body serves them both. A 46 reader
// refuses `check.role`, a key it does not know, and every reader from 54 on requires it. So the
// union body carries the 73 reader's required keys and the 46 reader's stand-ins, and the 46
// process keeps failing until it is replaced. The pod of the next case differs in which of its
// builds reads that body, and in nothing else.
func TestAMixedPodIsServedTheHighestsKeysAndTheLowestsStandIns(t *testing.T) {
	claims := mixedPodClaims(ServingFloor, claimsToRefreshContract)
	logs := &bytes.Buffer{}
	handler := shapedServer(t, populatedState(t), claims, logs, true)
	whole := stateStating(t, handler, "192.0.2.1:41000", 0)

	mixed := stateStating(t, handler, statePodAddr+":41000", 0)

	served := shapeOf(t, mixed)
	if len(served.checkRoles) != served.checks {
		t.Errorf("%d of the %d checks of the mixed pod's state %s carry `role`, want every one: the pod's newer process requires it",
			len(served.checkRoles), served.checks, mixed)
	}
	for _, key := range keysAddedAfterTheFloor {
		if !strings.Contains(string(mixed), key) {
			t.Errorf("the mixed pod's state %s carries no %s, which the pod's newer process requires", mixed, key)
		}
	}
	for _, key := range keysOfThisContract {
		if strings.Contains(string(mixed), key) {
			t.Errorf("the mixed pod's state %s carries %s, which neither of its contracts reads", mixed, key)
		}
	}
	if len(served.fixAttempts) != served.pullRequests || served.maxFixAttempts == nil {
		t.Errorf("the mixed pod's state %s carries %d of %d `fixAttempts` and max_fix_attempts %v, want both stand-ins: the pod's older process requires them",
			mixed, len(served.fixAttempts), served.pullRequests, served.maxFixAttempts)
	}
	if logs.Len() != 0 {
		t.Errorf("the mixed pod logged %s, want no line: both of its contracts are ones this daemon describes", logs)
	}

	// The header beats the pod, whichever of the pod's two contracts the caller states.
	wantStandIns(t, stateStating(t, handler, statePodAddr+":41000", ServingFloor))
	if stated := stateStating(t, handler, statePodAddr+":41000", DaemonAPIVersion()); string(stated) != string(whole) {
		t.Errorf("a caller of the mixed pod stating this contract was served %s, want the body this contract writes %s", stated, whole)
	}
}

// The pod a roll makes, holding one build at the floor and one at this daemon's own contract,
// is served the floor's shape and not the union of the two. The build at 193 states its
// contract in the header, so contractsFor answers it there and it never reads the body the pod
// decides; the build at 46 does read it, and it is the only process that does. Counting the
// newer one would serve that reader `admission.trees`, the memory fields and the rest, which
// its own schema refuses the whole body for (podContracts).
//
// This narrows no rule: where both builds of a pod read the pod-decided body, that body is still
// the union, which the `{46, 73}` case above holds. The union carries the 73 reader's required
// keys, and the 46 process keeps failing on it until it is replaced: a 46 reader refuses
// `check.role`, and every reader from 54 on requires it. The two pods differ in who reads the
// answer and in nothing else.
func TestAPodWhoseNewestBuildStatesItsOwnContractIsServedTheOldests(t *testing.T) {
	logs := &bytes.Buffer{}
	handler := shapedServer(t, populatedState(t), mixedPodClaims(ServingFloor, DaemonAPIVersion()), logs, true)
	floor := stateStating(t, handler, "192.0.2.1:41000", ServingFloor)

	rolled := stateStating(t, handler, statePodAddr+":41000", 0)

	if string(rolled) != string(floor) {
		t.Errorf("the rolled pod was served %s, want the very body a caller stating the floor reads %s", rolled, floor)
	}
	wantStandIns(t, rolled)
	for _, key := range keysOfThisContract {
		if strings.Contains(string(rolled), key) {
			t.Errorf("the rolled pod's state %s carries %s, which the 46 process it is written for refuses", rolled, key)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("the rolled pod logged %s, want no line: the floor is a shape this daemon describes", logs)
	}

	// The 193 build of that same pod is served this contract's own body, by its header.
	wantThisContract(t, stateStating(t, handler, statePodAddr+":41000", DaemonAPIVersion()))
}

// A caller at 111 reads the settings block as this daemon writes it, whether it states 111 in
// the header or the claims of its pod record 111: `pod_update_grace_minutes` with the value this
// daemon holds, and neither `pod_update_retask_hours` nor `pod_update_notice_hours`. main's #3982
// took the two hour keys out of the block and put the grace key in, and merged when main declared
// 111, so a daemon at 111 may send either block. No reader at any contract types any of the
// three (answers.go's typedSettings), so no stand-in and no band is owed for them, and no record
// holds them (contract_shapes_test.go's withoutUntypedSettings). The shape gate therefore does
// not see these keys, and this case is what holds that they are served as written.
func TestACallerAt111ReadsTheSettingsBlockThisDaemonWrites(t *testing.T) {
	settingsOf := func(body []byte) map[string]json.RawMessage {
		t.Helper()
		var served struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(body, &served); err != nil {
			t.Fatalf("decode the state %s: %v", body, err)
		}
		return served.Settings
	}
	// quietTimeContract is 111, the contract main declared when #3982 merged.
	const at = quietTimeContract
	state := populatedState(t)
	written := settingsOf(stateStating(t, shapedServer(t, state, podClaims(DaemonAPIVersion()), &bytes.Buffer{}, true), "192.0.2.1:41000", 0))
	grace, carried := written["pod_update_grace_minutes"]
	if !carried {
		t.Fatalf("the settings this contract writes carry no pod_update_grace_minutes; this proves nothing: %v", written)
	}
	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
	}{
		{name: "a caller that states 111", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion()), stated: at},
		{name: "a pod whose claims record 111", remote: statePodAddr + ":41000", claims: podClaims(at)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			served := settingsOf(stateStating(t, shapedServer(t, state, tc.claims, logs, true), tc.remote, tc.stated))
			for _, key := range []string{"pod_update_retask_hours", "pod_update_notice_hours"} {
				if value, carried := served[key]; carried {
					t.Errorf("a caller at %d is served settings.%s %s, want none: this daemon writes no such key, and no reader types it", at, key, value)
				}
			}
			if got, carried := served["pod_update_grace_minutes"]; !carried || string(got) != string(grace) {
				t.Errorf("a caller at %d is served settings.pod_update_grace_minutes %s (carried %v), want %s, as this daemon writes it: no reader types the key, so no band takes it off",
					at, got, carried, grace)
			}
			if logs.Len() != 0 {
				t.Errorf("a caller at %d logged %s, want no line: %d is a contract this daemon describes", at, logs, at)
			}
		})
	}
}

// undeclaredValue is a word no list of the declared table names (answers.go's EnumeratedFields).
// It is made up for the bodies the fallback of fieldArrivals.heldBack is read on, and
// TestEveryValueStripHoldsBackAValueNoRowDeclares holds the table to not naming it.
const undeclaredValue = "a_word_no_row_declares"

// valueStripCase is what one row of valueStrips is read on: a state and the two contracts of a
// mixed pod (TestEveryValueStripFollowsThePodsLowestContract), and a body carrying a value no
// row of the declared table names (TestEveryValueStripHoldsBackAValueNoRowDeclares).
type valueStripCase struct {
	// state carries a value the row takes; low is below that value's arrival, and high at or
	// above it.
	state     func(t *testing.T) State
	low, high int
	// carries is whether a served body carries the value.
	carries func(t *testing.T, body []byte) bool
	// stays holds the mixed pod's body to what the strip leaves of it, read beside the body
	// served at this daemon's own contract.
	stays func(t *testing.T, whole, mixed []byte)
	// beforeField is a contract below the arrival of the row's field, and withField the values a
	// served body carries that arrived with that field, each under the issue that carries it. A
	// case gives both where its row's field arrived above the floor. Where the field is older
	// than the floor it gives neither: no contract this daemon serves is below such a field's
	// arrival, so no pod records one beside a contract that reads the field.
	beforeField int
	withField   func(t *testing.T, body []byte) map[string]string
	// undeclaredState and undeclaredIssue carry undeclaredValue in the field the row strips: the
	// first is the body the row's half of the state's own values is handed, and the second the
	// body its half of an issue's values is. A case gives one for each half its row has.
	undeclaredState State
	undeclaredIssue Issue
}

// valueStripCases is the case of each row of valueStrips, under the row's `dropped`
// (eachValueStrip). The comment on each case says what its mixed pod is served and why.
func valueStripCases() map[string]valueStripCase {
	declined := string(record.AnsweredByDecline)
	waitingOnIssue := strconv.Quote(waitingOnIssueKind)
	answeredAt := time.Date(2026, 9, 22, 8, 45, 0, 0, time.UTC)
	return map[string]valueStripCase{
		// A pod recording 51 and 59 is served the report of claims nothing will wake without its
		// retired entries, since the pod's 51 process parses `strandedClaims[].state` with an
		// enum of `suspended` and `failed` and refuses the whole body for the `retired` of
		// version.go's 56. Every entry of Legion's roles whose state that process knows stays, in
		// the order the report named it. A pod this far back is below deploymentRolesContract too,
		// so an entry of a role of the deployment goes. The field is older than the floor, so the
		// case names no value that arrived with it.
		droppedStrandedState: {
			state: populatedState, low: treeContract, high: fixAttemptsContract,
			carries: func(t *testing.T, body []byte) bool {
				stranded, _ := strandedServed(t, body)
				return slices.ContainsFunc(stranded, func(entry map[string]string) bool {
					return entry["state"] == string(supervise.StateRetired)
				})
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				every, _ := strandedServed(t, whole)
				var kept []map[string]string
				for _, entry := range every {
					// The pod is below deploymentRolesContract too, so a claim of a role outside
					// Legion's six is no entry of its body either (shaping.go's
					// withoutDeploymentRoleClaims), and below strandedTokenContract, so no entry
					// carries its `token` (withoutStrandedTokens).
					if entry["state"] != string(supervise.StateRetired) && claim.IsBuiltin(claim.Role(entry["role"])) {
						delete(entry, "token")
						kept = append(kept, entry)
					}
				}
				if len(kept) == 0 {
					t.Fatalf("the fixture's state names %v, want a retired claim nothing will wake beside claims in other states; this proves nothing", every)
				}
				stranded, carried := strandedServed(t, mixed)
				if !carried {
					t.Fatalf("the {%d, %d} pod is served no strandedClaims: the key is never absent and never null; %s",
						treeContract, fixAttemptsContract, mixed)
				}
				if !reflect.DeepEqual(stranded, kept) {
					t.Errorf("the {%d, %d} pod is served the claims nothing will wake %v, want %v: the pod's %d process refuses the whole body for a retired entry",
						treeContract, fixAttemptsContract, stranded, kept, treeContract)
				}
			},
			// One claim nothing will wake, in a state no row declares.
			undeclaredState: State{StrandedClaims: []StrandedClaim{
				{Issue: "LEGION-212", Role: claim.RoleTester, State: undeclaredValue},
			}},
		},
		// A pod recording 64 and 85 is served the wait of every root a person declined without
		// its answered pair: `declined` arrived at version.go's 68, and the pod's 64 process
		// parses `answeredBy` with an enumeration of the eight values before it. The `waitingOn`
		// block itself stays, with the kind and the moment every contract reads. The field is
		// older than the floor, so the case names no value that arrived with it.
		droppedWaitAnswer: {
			state: stateCarryingTheRestOfTheShape, low: 64, high: whyAndLineFieldsContract,
			carries: func(t *testing.T, body []byte) bool {
				for _, wait := range waitsServed(t, body) {
					if wait.AnsweredBy == declined {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := waitsServed(t, mixed)
				for key, wait := range waitsServed(t, whole) {
					if wait.AnsweredBy != declined {
						continue
					}
					got, carried := served[key]
					if !carried {
						t.Fatalf("the {64, %d} pod is served %s with no wait at all: %s", whyAndLineFieldsContract, key, mixed)
					}
					if got.AnsweredBy != "" || got.AnsweredAt != "" {
						t.Errorf("the {64, %d} pod is served %s answered %q at %q, want neither key: the value arrived at 68 and the pod's 64 process refuses the whole body for it",
							whyAndLineFieldsContract, key, got.AnsweredBy, got.AnsweredAt)
					}
					if got.Kind == "" || got.Since == "" {
						t.Errorf("the {64, %d} pod is served %s's wait as %+v, want the kind and the moment it began: both are true at every contract",
							whyAndLineFieldsContract, key, got)
					}
				}
			},
			// A root whose wait was answered by a value no row declares.
			undeclaredIssue: Issue{Key: "SHAPES-2", WaitingOn: &WaitingOnView{
				Kind: "ask", Ask: "7f4b2e60-9c18-4a3d-8e52-6b0d1f93a7c4",
				AnsweredAt: &answeredAt, AnsweredBy: undeclaredValue,
			}},
		},
		// A pod recording 111 and 133 reads `kind` through its 111 process too, which parses it
		// with a `z.enum` of the four words before 132 and refuses the whole body for
		// `waiting_on_issue`, so a member of that kind is served without its `kind`. The band of
		// mergeWaitContract follows the pod's highest contract and serves the same body
		// `mergeWait`, which is what tells a strip keyed to the lowest from one keyed to the
		// highest on one body. Every other field of the member stays, each a field of
		// whyAndLineFieldsContract, and so does every member of another kind.
		//
		// `kind` arrived at 85 with those four words, and they ride the band of that contract
		// and not this strip (fieldArrivals.heldBack says why). So a pod recording 46 and 133 is
		// served each of them, and still no `waiting_on_issue`, which arrived after the field.
		// The fixture stands a member of the kind `queued_root` and one of the kind `answered`
		// beside the member that waits on another issue.
		droppedLineKind: {
			state: narrowingState, low: quietTimeContract, high: mergeWaitContract,
			carries: func(t *testing.T, body []byte) bool {
				for _, member := range waitingLineFieldsServed(t, body) {
					if member["kind"] == waitingOnIssue {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				waits := mergeWaitsServed(t, whole)
				if len(waits) == 0 {
					t.Fatalf("no issue of the fixture's state carries a %s; this proves nothing: %s", MergeWaitField, whole)
				}
				if served := mergeWaitsServed(t, mixed); !reflect.DeepEqual(served, waits) {
					t.Errorf("the {%d, %d} pod is served the merge waits %+v, want %+v: the band follows the pod's highest contract, which reads them",
						quietTimeContract, mergeWaitContract, served, waits)
				}
				served := waitingLineFieldsServed(t, mixed)
				for key, member := range waitingLineFieldsServed(t, whole) {
					// The band of priorityContract follows the pod's highest contract too, and
					// 133 is below it, so no member is served the priority that placed it.
					want := maps.Clone(member)
					delete(want, "priority")
					if member["kind"] == waitingOnIssue {
						delete(want, "kind")
					}
					if !reflect.DeepEqual(served[key], want) {
						t.Errorf("the {%d, %d} pod is served %s's %s as %v, want %v: the strip takes the word the pod's %d process refuses and nothing else",
							quietTimeContract, mergeWaitContract, key, WaitingLineField, served[key], want, quietTimeContract)
					}
				}
			},
			beforeField: ServingFloor,
			withField: func(t *testing.T, body []byte) map[string]string {
				kinds := map[string]string{}
				for key, member := range waitingLineFieldsServed(t, body) {
					if kind, carried := member["kind"]; carried && kind != waitingOnIssue {
						kinds[key] = kind
					}
				}
				return kinds
			},
			// A member of the waiting line whose `kind` is a word no row declares.
			undeclaredIssue: Issue{Key: "LEGION-213", WaitingLine: &WaitingLineView{
				Place: 1, Length: 1, Part: "returned", Kind: undeclaredValue, PartPlace: 1, PartLength: 1,
			}},
		},
		// A pod recording pauseContract and cachesSourceContract is served the repository-cache-room
		// row without its `source`: `caches` arrived at cachesSourceContract, and the pod's 178
		// process parses `source` with an enum of `configuration` and `boot`. The pod's process at
		// cachesSourceContract states its contract in its header and is in no pod's reckoning
		// (podContracts), so this pod is read by its 178 process alone, the reader the strip is for,
		// and a pod whose every claim records cachesSourceContract is served this daemon's own body.
		// Every other field of that row stays, and so does every row of another source, whose word
		// the 178 process admits. The field is older than the floor, so the case names no value
		// that arrived with it.
		droppedCapabilitySource: {
			state: populatedState, low: pauseContract, high: cachesSourceContract,
			carries: func(t *testing.T, body []byte) bool {
				return slices.ContainsFunc(capabilityRowsServed(t, body), func(row map[string]any) bool {
					return row["source"] == cachesSource
				})
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				want := capabilityRowsServed(t, withoutCachesSources(t, whole))
				if served := capabilityRowsServed(t, mixed); !reflect.DeepEqual(served, want) {
					t.Errorf("the {%d, %d} pod is served the capability rows %v, want %v: the strip takes the word the pod's %d process refuses and nothing else",
						pauseContract, cachesSourceContract, served, want, pauseContract)
				}
			},
			// One capability row whose source is a word no row declares.
			undeclaredState: State{Capabilities: []CapabilityState{{
				Name: string(capabilities.RepositoryCacheRoom), Status: capabilities.StatusOpen,
				Detail: "a cache ran out of room", Source: undeclaredValue,
			}}},
		},
		// The two strips of gateBasisContract's values read a mixed pod recording 178 and
		// gateBasisContract, and no pod reading counts the gateBasisContract claim: podContracts
		// leaves out a claim at shapedAnswersContract or above, since every build from there states
		// its contract in the header. So the pod is served as its 178 process alone, on both sides,
		// and these cases hold each strip to the body that process reads; a strip keyed to the pod's
		// highest contract is held by the stated callers of awaiting_children_narrow_test.go and
		// gate_basis_wire_test.go instead, and the strip's own half by the value no row declares
		// below. Each value arrived after its field, whose arrival is no contract a pod reading can
		// place above, so no case names a value that arrived with the field.
		//
		// A gated step on an ancestor's basis reads `open_on_basis` or `pending_on_basis`, which a
		// reader from 55 up to the contract below gateBasisContract refuses the whole body for, so
		// the pod is served neither issue's `gatedSteps`, and every other issue keeps its gated steps
		// as the body at gateBasisContract serves them. The band of the same contract takes those
		// lists for the same caller first (shapeIssue runs the bands before the strips), so the strip
		// finds nothing left to take on this pod.
		droppedStepStatus: {
			state: populatedState, low: pauseContract, high: entryNumber(gateBasisContract),
			carries: func(t *testing.T, body []byte) bool {
				for _, statuses := range stepStatusesServed(t, body) {
					if slices.ContainsFunc(statuses, onBasisStatus) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := stepStatusesServed(t, mixed)
				for key, statuses := range stepStatusesServed(t, whole) {
					var want []string
					if !slices.ContainsFunc(statuses, onBasisStatus) {
						want = statuses
					}
					if !reflect.DeepEqual(served[key], want) {
						t.Errorf("the {%d, %d} pod is served %s's gated steps as %v, want %v: an issue a step of which stands on a basis is served none, and every other issue its own",
							pauseContract, entryNumber(gateBasisContract), key, served[key], want)
					}
				}
			},
			// A gated step whose status is a word no row declares.
			undeclaredIssue: Issue{Key: "LEGION-214", GatedSteps: []GatedStepView{
				{Step: record.DesignStepSpec, Status: GatedStepStatus(undeclaredValue)},
			}},
		},
		// An issue in `awaiting_children`, and the word at `heldFrom` and at a held task's `phase`,
		// which no reader below gateBasisContract admits: the pod is served the issue's `phase` as
		// `admitted` and neither other key where it names the word, and every other phase word as
		// the body at gateBasisContract serves it.
		droppedPhase: {
			state: stateCarryingEveryDeclaredValue, low: pauseContract, high: entryNumber(gateBasisContract),
			carries: func(t *testing.T, body []byte) bool {
				for _, word := range phaseWordsServed(t, body) {
					if word == string(phase.AwaitingChildren) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := phaseWordsServed(t, mixed)
				for place, word := range phaseWordsServed(t, whole) {
					got, carried := served[place]
					switch {
					case word != string(phase.AwaitingChildren) && got != word:
						t.Errorf("the {%d, %d} pod is served %s as %q, want %q: the strip takes only the word its process refuses",
							pauseContract, entryNumber(gateBasisContract), place, got, word)
					case word == string(phase.AwaitingChildren) && strings.HasSuffix(place, " phase") && got != string(phase.Admitted):
						t.Errorf("the {%d, %d} pod is served %s as %q, want admitted: the issue's own phase is a key every reader requires",
							pauseContract, entryNumber(gateBasisContract), place, got)
					case word == string(phase.AwaitingChildren) && !strings.HasSuffix(place, " phase") && carried:
						t.Errorf("the {%d, %d} pod is served %s as %q, want the key left off",
							pauseContract, entryNumber(gateBasisContract), place, got)
					}
				}
			},
			// An issue whose phase, `heldFrom` and architect's held task each name a word no row
			// declares.
			undeclaredIssue: Issue{Key: "LEGION-215", Phase: phase.Phase(undeclaredValue), HeldFrom: undeclaredValue,
				Architect: &ClaimView{Session: "ses_architect_215", PendingTask: &PendingTaskView{Phase: phase.Phase(undeclaredValue), QueuedAt: answeredAt}}},
		},
		// A pod recording the contract before shapedAnswersContract is served every issue held for
		// `task-stopped` or `children` without its `holdReason`: the two arrived at
		// OwnerPlanContract, and the pod's process reads the key with `z.literal("escalated")`.
		// The hold's `heldFrom` stays, served as implementing where it is step, since such a pod is
		// below deploymentRolesContract too, and so does every escalated hold. No pod's reckoning holds
		// a claim at or above shapedAnswersContract, since a build from that contract on states
		// its own (podContracts), so the pod's claim at OwnerPlanContract moves neither half and
		// the pod alone at it is served this daemon's own body, as production serves it. The
		// field is older than the floor, so the case names no value that arrived with it.
		droppedHoldReason: {
			state: heldIssues, low: shapedAnswersContract - 1, high: OwnerPlanContract,
			carries: func(t *testing.T, body []byte) bool {
				for _, hold := range holdsServed(t, body) {
					if hold[0] == string(record.HoldTaskStopped) || hold[0] == string(record.HoldForChildren) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := holdsServed(t, mixed)
				for key, hold := range holdsServed(t, whole) {
					want := hold
					if hold[0] == string(record.HoldTaskStopped) || hold[0] == string(record.HoldForChildren) {
						want[0] = ""
					}
					// The pod is below deploymentRolesContract too, so a hold from step is served as
					// one from implementing (shaping.go's withoutDeploymentRoles).
					if hold[1] == string(phase.Step) {
						want[1] = string(phase.Implementing)
					}
					if served[key] != want {
						t.Errorf("the {%d, %d} pod is served %s held %q, want %q: the strip takes the reason the pod's %d process refuses and nothing else",
							shapedAnswersContract-1, OwnerPlanContract, key, served[key], want, shapedAnswersContract-1)
					}
				}
			},
			// A held issue whose reason is a word no row declares.
			undeclaredIssue: Issue{Key: "SHAPES-3", Phase: "held", HoldReason: undeclaredValue, HeldFrom: "testing"},
		},
		// A pod recording the contract before shapedAnswersContract is served each row of
		// `admission.held` under `children_before_retro` or `task_stopped` as `unclassified` with
		// no `heldBy`: the two arrived at OwnerPlanContract, after the field, and the pod's process
		// reads `reason` with a `z.enum` of the eleven words before them, and a row of the
		// `awaiting_children` gateBasisContract added as `children_first`, the word that wait took
		// over, with its `heldBy`, which names what that word names (heldReasonStandIn). Every
		// other row stays as it stands. The pod's claim at OwnerPlanContract moves neither half,
		// as above. The eleven words arrived with the field at heldContract and ride its band,
		// which follows the pod's highest contract; no pod whose highest reads the new words is a
		// pod at all, so that half is read on a pod recording the floor and the contract before
		// shapedAnswersContract (TestAnOlderCallerReadsTheNewHeldWordsAsUnclassified) rather than
		// here.
		droppedHeldWord: {
			state: heldWordAndWaitIssues, low: shapedAnswersContract - 1, high: OwnerPlanContract,
			carries: func(t *testing.T, body []byte) bool {
				for _, row := range heldRowsServed(t, body) {
					if arrivalsAt("admission.held.*.reason").heldBack(row["reason"].(string), shapedAnswersContract-1) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := heldRowsServed(t, mixed)
				for key, row := range heldRowsServed(t, whole) {
					want := row
					switch {
					case row["reason"] == string(record.HeldAwaitingChildren):
						want = map[string]any{"issue": key, "reason": string(record.HeldChildrenFirst), "heldBy": row["heldBy"]}
					case arrivalsAt("admission.held.*.reason").heldBack(row["reason"].(string), shapedAnswersContract-1):
						want = map[string]any{"issue": key, "reason": string(record.HeldUnclassified)}
					}
					if !reflect.DeepEqual(served[key], want) {
						t.Errorf("the {%d, %d} pod is served %s's held row as %v, want %v: the strip rewrites the word the pod's %d process refuses and nothing else",
							shapedAnswersContract-1, OwnerPlanContract, key, served[key], want, shapedAnswersContract-1)
					}
				}
			},
			// A row of the held list whose word no row of the table declares.
			undeclaredState: State{Admission: Admission{Held: []HeldIssue{{Issue: "SHAPES-4", Reason: record.HeldReason(undeclaredValue)}}}},
		},
		// A pod recording the contract before shapedAnswersContract and the cyber-policy entry's
		// number is served every claim view whose failure cause or pending cause is `cyber_policy`
		// with `provider_error` in its place: the word arrived at that entry, after the field, and
		// the pod's older process reads both with a `z.enum` of the seven words before it. The
		// failure record's other keys stay, its `detail` naming cyber_policy among them, and so
		// does every view naming another word. The pod's claim at the entry's number states its
		// own contract and is in no pod's reckoning (podContracts), so the pod alone at it is
		// served this daemon's own body. The field is older than the floor's records of it only
		// through recordFieldsContract's band, which the low contract is above, so the case names
		// no value that arrived with it.
		droppedFailureCause: {
			state: stateCarryingEveryDeclaredValue, low: shapedAnswersContract - 1, high: entryNumber(cyberPolicyContract),
			carries: func(t *testing.T, body []byte) bool {
				for _, cause := range failureCausesServed(t, body) {
					if cause == string(supervise.CauseCyberPolicy) {
						return true
					}
				}
				return false
			},
			stays: func(t *testing.T, whole, mixed []byte) {
				served := failureCausesServed(t, mixed)
				for at, cause := range failureCausesServed(t, whole) {
					want := cause
					if cause == string(supervise.CauseCyberPolicy) {
						want = string(supervise.CauseProviderError)
					}
					if served[at] != want {
						t.Errorf("the {%d, %d} pod is served %s as %q, want %q: the strip rewrites the word the pod's %d process refuses and nothing else",
							shapedAnswersContract-1, entryNumber(cyberPolicyContract), at, served[at], want, shapedAnswersContract-1)
					}
				}
			},
			// A failed architect whose failure cause and pending cause are a word no row declares.
			undeclaredIssue: Issue{Key: "SHAPES-5", Architect: &ClaimView{
				Session: "ses_architect_shapes_5", State: "failed", PendingCause: undeclaredValue,
				Failure: &ClaimFailureView{Cause: undeclaredValue, Detail: "a cause no row declares"},
			}},
		},
	}
}

// failureCausesServed is every failure cause and pending cause a served body's claim views carry,
// keyed by the issue, the view (the architect's or a worker's role) and the field.
func failureCausesServed(t *testing.T, body []byte) map[string]string {
	t.Helper()
	type view struct {
		Failure *struct {
			Cause string `json:"cause"`
		} `json:"failure"`
		PendingCause string `json:"pendingCause"`
	}
	var state struct {
		Issues map[string]struct {
			Architect *view `json:"architect"`
			Workers   map[string]struct {
				Claim view `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	causes := map[string]string{}
	add := func(at string, v view) {
		if v.Failure != nil {
			causes[at+" failure.cause"] = v.Failure.Cause
		}
		if v.PendingCause != "" {
			causes[at+" pendingCause"] = v.PendingCause
		}
	}
	for key, issue := range state.Issues {
		if issue.Architect != nil {
			add(key+" architect", *issue.Architect)
		}
		for role, worker := range issue.Workers {
			add(key+" "+role, worker.Claim)
		}
	}
	return causes
}

// onBasisStatus is whether status is one of the two words a step on an ancestor's basis reads.
func onBasisStatus(status string) bool {
	return status == string(GatedStepOpenOnBasis) || status == string(GatedStepPendingOnBasis)
}

// stepStatusesServed is the `status` of each gated step a served body carries, by the issue that
// carries them, in their order; an issue served no `gatedSteps` is in no entry.
func stepStatusesServed(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			GatedSteps []struct {
				Status string `json:"status"`
			} `json:"gatedSteps"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	served := map[string][]string{}
	for key, issue := range state.Issues {
		for _, step := range issue.GatedSteps {
			served[key] = append(served[key], step.Status)
		}
	}
	return served
}

// phaseWordsServed is every phase word a served body carries at the three fields that take one:
// each issue's `phase`, its `heldFrom`, and the `phase` of the task its architect and each of its
// workers holds, by `<issue> <field>`; a key the body leaves off is in no entry.
func phaseWordsServed(t *testing.T, body []byte) map[string]string {
	t.Helper()
	type task struct {
		PendingTask *struct {
			Phase string `json:"phase"`
		} `json:"pendingTask"`
	}
	var state struct {
		Issues map[string]struct {
			Phase     string `json:"phase"`
			HeldFrom  string `json:"heldFrom"`
			Architect *task  `json:"architect"`
			Workers   map[string]struct {
				Claim task `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	served := map[string]string{}
	carry := func(place, word string) {
		if word != "" {
			served[place] = word
		}
	}
	for key, issue := range state.Issues {
		carry(key+" phase", issue.Phase)
		carry(key+" heldFrom", issue.HeldFrom)
		if issue.Architect != nil && issue.Architect.PendingTask != nil {
			carry(key+" architect.pendingTask.phase", issue.Architect.PendingTask.Phase)
		}
		for role, worker := range issue.Workers {
			if worker.Claim.PendingTask != nil {
				carry(key+" workers."+role+".pendingTask.phase", worker.Claim.PendingTask.Phase)
			}
		}
	}
	return served
}

// eachValueStrip runs read on each row of valueStrips with that row's case, in a subtest named
// by the row's `dropped`. A case is looked up by its row's `dropped`, and a row with no case
// fails, so a strip is added to the table with the bodies it is read on. A case no row reads
// fails too: the row it was written for is gone or drops something else.
func eachValueStrip(t *testing.T, read func(t *testing.T, row valueStrip, tc valueStripCase)) {
	t.Helper()
	cases := valueStripCases()
	rows := map[string]bool{}
	for _, row := range valueStrips() {
		tc, cased := cases[row.dropped]
		if !cased {
			t.Errorf("the value strip that drops %q has no case here: a strip is added with the bodies it is read on", row.dropped)
			continue
		}
		rows[row.dropped] = true
		t.Run(row.dropped, func(t *testing.T) { read(t, row, tc) })
	}
	for dropped := range cases {
		if !rows[dropped] {
			t.Errorf("the case for %q reads no row of valueStrips: the row it was written for is gone or drops something else", dropped)
		}
	}
}

// A value follows the pod's lowest contract where a key follows its highest
// (fieldArrivals.heldBack says why). Every row of valueStrips is read here on a mixed pod, a
// caller that states no contract in a pod whose claims record low and high at once, low below
// the arrival of a value the row takes and high at or above it. The body served at this daemon's
// own contract carries the value, which is what makes the fixture prove anything; a pod whose
// claims record high alone is served it; and the mixed pod is not. A strip keyed to the pod's
// highest contract serves the mixed pod the value and fails here. The pod's third claim records
// no contract and moves neither half, and every contract of each pod is one this daemon
// describes, so neither edge line is written.
//
// That is the rule of a value newer than its field, which is what a row takes. A value that
// arrived with its field rides the band of the field's key, which follows the pod's highest
// contract (fieldArrivals.heldBack says why). So a row whose field arrived above the floor is
// read on a second mixed pod, one whose claims record a contract below the field's arrival
// beside high. That pod is served every value that arrived with the field, and still none the
// row takes. A strip that took every value above the pod's lowest contract would take those
// values too and fail here.
//
// A row with no case fails, so a strip is added to the table with the mixed pod it is read on
// (eachValueStrip).
func TestEveryValueStripFollowsThePodsLowestContract(t *testing.T) {
	eachValueStrip(t, func(t *testing.T, _ valueStrip, tc valueStripCase) {
		logs := &bytes.Buffer{}
		state := tc.state(t)
		handler := shapedServer(t, state, mixedPodClaims(tc.low, tc.high), logs, true)
		whole := stateStating(t, handler, "192.0.2.1:41000", DaemonAPIVersion())
		if !tc.carries(t, whole) {
			t.Fatalf("the body served at %d carries no value this strip takes; this proves nothing: %s", DaemonAPIVersion(), whole)
		}

		alone := stateStating(t, shapedServer(t, state, podClaims(tc.high), logs, true), statePodAddr+":41000", 0)
		if !tc.carries(t, alone) {
			t.Errorf("a pod at %d is served no value this strip takes, want it: the value arrived at or below %d; %s",
				tc.high, tc.high, alone)
		}

		mixed := stateStating(t, handler, statePodAddr+":41000", 0)

		if tc.carries(t, mixed) {
			t.Errorf("the {%d, %d} pod is served a value this strip takes, want none: the value arrived above %d and the pod's %d process refuses the whole body for it; %s",
				tc.low, tc.high, tc.low, tc.low, mixed)
		}
		tc.stays(t, whole, mixed)

		if tc.withField != nil {
			arrived := tc.withField(t, whole)
			if len(arrived) == 0 {
				t.Fatalf("the body served at %d carries no value that arrived with this strip's field; this proves nothing: %s", DaemonAPIVersion(), whole)
			}
			spanning := stateStating(t, shapedServer(t, state, mixedPodClaims(tc.beforeField, tc.high), logs, true), statePodAddr+":41000", 0)
			if served := tc.withField(t, spanning); !reflect.DeepEqual(served, arrived) {
				t.Errorf("the {%d, %d} pod is served the values that arrived with their field as %v, want %v: they ride the band of the field's key, which follows the pod's %d process; %s",
					tc.beforeField, tc.high, served, arrived, tc.high, spanning)
			}
			if tc.carries(t, spanning) {
				t.Errorf("the {%d, %d} pod is served a value this strip takes, want none: the value arrived after its field and above %d; %s",
					tc.beforeField, tc.high, tc.beforeField, spanning)
			}
		}
		if logs.Len() != 0 {
			t.Errorf("the pods logged %s, want no line: every contract they record is one this daemon describes", logs)
		}
	})
}

// A value no row of the declared table names is read as arriving at this daemon's own contract
// (fieldArrivals.heldBack): no older reader is known to admit it, so every row of valueStrips
// holds it back from a caller below DaemonAPIVersion and serves it to a caller at
// DaemonAPIVersion. Each half a row has is handed a body carrying such a value in the field it
// strips, at both contracts (wantUndeclaredValueHeldBack). A strip that takes the words it
// names, and not every word the table does not admit, serves the caller below a value its
// reader refuses the whole body for, and fails here.
//
// The value is a made-up word, and the table is held to naming it in no list before any row is
// read: a list that declared it at this daemon's own contract would pass a row by that arrival
// and prove nothing of a value no row declares. A row with no case fails, and so does a case
// whose body does not carry the word, so a strip is added to the table with the body it is read
// on (eachValueStrip).
func TestEveryValueStripHoldsBackAValueNoRowDeclares(t *testing.T) {
	for _, field := range EnumeratedFields() {
		for _, value := range field.Values {
			if value.Value == undeclaredValue {
				t.Fatalf("the declared table names %q, at `%s` of the %s answer since %d; this proves nothing of a value no row declares",
					undeclaredValue, field.KeyPath, field.Answer, value.Since)
			}
		}
	}
	eachValueStrip(t, func(t *testing.T, row valueStrip, tc valueStripCase) {
		if row.state != nil {
			wantUndeclaredValueHeldBack(t, row.state, tc.undeclaredState)
		}
		if row.issue != nil {
			wantUndeclaredValueHeldBack(t, row.issue, tc.undeclaredIssue)
		}
	})
}

// wantUndeclaredValueHeldBack holds take, one half of a row of valueStrips, to the fallback of
// fieldArrivals.heldBack on body, which carries undeclaredValue in the field that half strips.
// At the contract below this daemon's own the word goes and the half reports that it took it;
// at this daemon's own the body comes back as it was and the half reports nothing. The word is
// looked for in the marshalled body, which is what a caller is served. The body is marshalled
// before either reading, so a half that wrote through to the body it was handed fails the
// second one.
func wantUndeclaredValueHeldBack[T any](t *testing.T, take func(T, int) (T, bool), body T) {
	t.Helper()
	word := []byte(strconv.Quote(undeclaredValue))
	marshalled := func(body T) []byte {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal the body %+v: %v", body, err)
		}
		return encoded
	}
	whole := marshalled(body)
	if !bytes.Contains(whole, word) {
		t.Fatalf("the case's body %s carries no %s; this proves nothing", whole, word)
	}

	below, took := take(body, DaemonAPIVersion()-1)
	if served := marshalled(below); !took || bytes.Contains(served, word) {
		t.Errorf("at %d the strip reports %v and serves %s, want it to report true and serve no %s: a value no row declares arrives at %d, above that caller",
			DaemonAPIVersion()-1, took, served, word, DaemonAPIVersion())
	}
	at, took := take(body, DaemonAPIVersion())
	if served := marshalled(at); took || !bytes.Equal(served, whole) {
		t.Errorf("at %d the strip reports %v and serves %s, want it to report false and serve the body as it was, %s: a caller at this daemon's own contract admits the value",
			DaemonAPIVersion(), took, served, whole)
	}
}

// waitServed is one issue's `waitingOn` as a served body carries it: the two keys of the
// answered pair, and two the block carries at every contract this daemon serves.
type waitServed struct {
	Kind       string `json:"kind"`
	Since      string `json:"since"`
	AnsweredAt string `json:"answeredAt"`
	AnsweredBy string `json:"answeredBy"`
}

// waitsServed is the wait of every issue of body that carries one, by issue key.
func waitsServed(t *testing.T, body []byte) map[string]waitServed {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			WaitingOn *waitServed `json:"waitingOn"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	waits := map[string]waitServed{}
	for key, issue := range state.Issues {
		if issue.WaitingOn != nil {
			waits[key] = *issue.WaitingOn
		}
	}
	return waits
}

// capabilityRowsServed is every row of a served body's capability report, in its order, decoded
// as a plugin's reader sees it.
func capabilityRowsServed(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var state struct {
		Capabilities []map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	return state.Capabilities
}

// mixedPodClaims is the pod at statePodAddr holding one claim at low, one at high and one that
// records no contract at all, beside a claim of another pod.
func mixedPodClaims(low, high int) []supervise.Claim {
	claims := podClaims(low)
	claims[1] = claimInPod("legion-legion-legion-208-implementer", statePodUID, high)
	return append(claims, claimInPod("legion-legion-legion-208-tester", statePodUID, 0))
}

// keysAddedAfterTheFloor is the keys the servings-down of the bands between the floor and
// claimsToRefreshContract remove, as the wire names them: a caller served all of them
// is served everything this daemon writes above the floor and below its own contract, but each
// check's `role`, which shapeOf reads instead because an issue carries a check only while its
// production check is open.
var keysAddedAfterTheFloor = []string{
	`"updateRound"`, `"updateHead"`, `"memoryGroup"`, `"memoryDrawMode"`, `"memoryDrawnAt"`,
	`"trees"`, `"tree"`, `"wakesWaiting"`, `"gatedSteps"`, `"step"`, `"` + WaitingLineField + `"`,
	`"backwardMoves"`, `"claimsToRefresh"`,
}

// keysOfThisContract is the three keys shapedAnswersContract added and serves down: the state's
// own `lowestPluginContract` and the two of each claim view's round report.
var keysOfThisContract = []string{`"lowestPluginContract"`, `"updateState"`, `"wake"`}

// A claim that holds no process is in no pod's reckoning: suspending, failing or retiring a claim
// ends what the machine had with its process, and the claim gives up its locator with it
// (supervise's letGo), so no such claim records a pod. That is why no fixture here holds a
// retired claim of a pod: there is no such claim to hold.
//
// The pod's live claims record claimsToRefreshContract rather than this daemon's own, because a
// claim at shapedAnswersContract or above is in no pod's reckoning either: it states its own
// contract and reads no pod-decided body (podContracts).
func TestAClaimThatHoldsNoProcessIsInNoPodsReckoning(t *testing.T) {
	stopped := []supervise.Claim{
		{Token: "legion-legion-legion-207-tester", State: supervise.StateRetired, PluginContract: 30},
		{Token: "legion-legion-legion-207-reviewer", State: supervise.StateFailed, PluginContract: 31},
		{Token: "legion-legion-legion-207-merger", State: supervise.StateSuspended, PluginContract: 32},
	}
	for _, c := range stopped {
		if c.Locator != nil {
			t.Fatalf("the %s claim %s records a process; this proves nothing", c.State, c.Token)
		}
	}

	lowest, highest, recorded := podContracts(append(podClaims(claimsToRefreshContract), stopped...), statePodUID)

	if lowest != claimsToRefreshContract || highest != claimsToRefreshContract {
		t.Errorf("the pod's contracts = %d and %d, want %d on both sides: a claim with no process is in no pod",
			lowest, highest, claimsToRefreshContract)
	}
	if recorded == "" {
		t.Error("the lowest contract names no claim, want the claim of the pod it was read from")
	}
}

// A claim that states its own contract is in no pod's reckoning either: every build at
// shapedAnswersContract or later writes its number in pluginContractHeader, and contractsFor
// answers on the header before it reads the pod, so counting such a claim would shape the
// pod-decided body for a process that never reads it (podContracts). A pod of those alone
// reports nothing, and its callers that state no contract are served this daemon's own body.
func TestAClaimThatStatesItsOwnContractIsInNoPodsReckoning(t *testing.T) {
	lowest, highest, recorded := podContracts(podClaims(DaemonAPIVersion()), statePodUID)

	if lowest != 0 || highest != 0 || recorded != "" {
		t.Errorf("a pod of claims at this daemon's own contract reports %d, %d and %q, want nothing on all three",
			lowest, highest, recorded)
	}
	mixedLowest, mixedHighest, _ := podContracts(mixedPodClaims(ServingFloor, DaemonAPIVersion()), statePodUID)
	if mixedLowest != ServingFloor || mixedHighest != ServingFloor {
		t.Errorf("the rolled pod reports %d and %d, want the floor on both sides: its 193 build reads no body this decides",
			mixedLowest, mixedHighest)
	}
}

// The four edges of the contract a caller is served, and the one log line each of the outer two
// writes.
func TestTheEdgesOfTheContractACallerIsServed(t *testing.T) {
	logs := &bytes.Buffer{}
	full := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion()), logs, true)
	whole := stateStating(t, full, statePodAddr+":41000", 0)
	logs.Reset()

	t.Run("below the floor", func(t *testing.T) {
		const recorded = 42
		logs := &bytes.Buffer{}
		handler := shapedServer(t, populatedState(t), podClaims(recorded), logs, true)

		body := stateStating(t, handler, statePodAddr+":41000", 0)

		// The restore side is the floor's, which carries both stand-ins, and the strip side is
		// the caller's own number, which is below every contract the eighteen keys of the two
		// lists arrived at: it loses every key it loses today and no more.
		wantStandIns(t, body)
		for _, key := range append(append([]string{}, keysAddedAfterTheFloor...), keysOfThisContract...) {
			if strings.Contains(string(body), key) {
				t.Errorf("a pod at %d was served %s, which carries %s: it loses no key it loses today", recorded, body, key)
			}
		}
		stranded, carried := strandedServed(t, body)
		if !carried {
			t.Fatalf("the state %s carries no `strandedClaims`, which is never absent", body)
		}
		for _, entry := range stranded {
			if entry["state"] == string(supervise.StateRetired) {
				t.Errorf("a pod at %d was served the retired stranded entry %v", recorded, entry)
			}
		}
		logged := logs.String()
		if !strings.Contains(logged, `"pluginContract":42`) || !strings.Contains(logged, `"claim":"legion-legion-legion-208-planner"`) {
			t.Errorf("logs %s do not name the claim and the contract it recorded", logged)
		}
		if lines := strings.Count(strings.TrimSpace(logged), "\n") + 1; lines != 1 {
			t.Errorf("logs %s carry %d lines, want the one line this edge writes", logged, lines)
		}

		// For a caller that states no contract, the state and the per-issue record are the only
		// answers whose shape the pod decides, so the record route of the same pod is read too:
		// each issue's record is the issue the state above carries for that pod, with every key
		// added after the floor stripped, `pullRequest.fixAttempts` as its stand-in and no
		// `check.role` (shapeIssue). Every other body is this daemon's own. The pod places a fresh
		// session's caller too, and there it decides what a stop that could not start the claim
		// again means: the 200 with `phaseNotRestarted` or the failure contract 103 gave
		// (readsTheStopAsAFailure), and not the shape of either body. The records are read after
		// the log is counted, since each read is a below-floor caller of its own.
		var pullRequests, checks int
		issues := issuesOf(t, body)
		for _, key := range slices.Sorted(maps.Keys(issues)) {
			route := "GET /legion/v1/issues/" + key + "/record"
			record := recordOf(t, handler, statePodAddr+":41000", key, 0)
			if string(record) != string(issues[key]) {
				t.Errorf("%s served a pod at %d a record other than the issue its state holds: %s,\nwant %s", route, recorded, record, issues[key])
			}
			var carried []string
			for _, added := range append(append([]string{}, keysAddedAfterTheFloor...), keysOfThisContract...) {
				if strings.Contains(string(record), added) {
					carried = append(carried, added)
				}
			}
			if len(carried) != 0 {
				t.Errorf("%s served a pod at %d %s, each added after the floor: such a pod loses every key it loses today; record %s", route, recorded, strings.Join(carried, ", "), record)
			}
			var issue struct {
				PullRequest *struct {
					FixAttempts *int `json:"fixAttempts"`
				} `json:"pullRequest"`
				Check *struct {
					Role *string `json:"role"`
				} `json:"check"`
			}
			if err := json.Unmarshal(record, &issue); err != nil {
				t.Fatalf("decode %s's record %s: %v", route, record, err)
			}
			if issue.PullRequest != nil {
				pullRequests++
				if attempts := issue.PullRequest.FixAttempts; attempts == nil || *attempts != 0 {
					t.Errorf("%s served a pod at %d a pull request with no `fixAttempts` of 0, the stand-in its reader requires; record %s", route, recorded, record)
				}
			}
			if issue.Check != nil {
				checks++
				if issue.Check.Role != nil {
					t.Errorf("%s served a pod at %d a check carrying `role`, which the reader before checkRoleContract refuses; record %s", route, recorded, record)
				}
			}
		}
		if pullRequests == 0 || checks == 0 {
			t.Fatalf("no record a pod at %d was served carries a pull request or a check; this proves nothing", recorded)
		}
	})

	t.Run("above this daemon's own", func(t *testing.T) {
		logs := &bytes.Buffer{}
		handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion()), logs, true)

		body := stateStating(t, handler, "192.0.2.1:41000", DaemonAPIVersion()+1)

		if string(body) != string(whole) {
			t.Errorf("a caller above this contract was served %s, want the body this contract writes %s", body, whole)
		}
		logged := logs.String()
		if !strings.Contains(logged, fmt.Sprintf(`"pluginContract":%d`, DaemonAPIVersion()+1)) ||
			!strings.Contains(logged, `"caller":"192.0.2.1:41000"`) {
			t.Errorf("logs %s do not name the contract and the caller's address", logged)
		}
		if lines := strings.Count(strings.TrimSpace(logged), "\n") + 1; lines != 1 {
			t.Errorf("logs %s carry %d lines, want the one line this edge writes", logged, lines)
		}
	})

	t.Run("a contract no change ever shipped under", func(t *testing.T) {
		logs := &bytes.Buffer{}
		handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion()), logs, true)
		for _, tc := range []struct{ stated, nearest int }{{stated: 47, nearest: 46}, {stated: 50, nearest: 49}} {
			t.Run(strconv.Itoa(tc.stated), func(t *testing.T) {
				if slices.Contains(SupportedContracts(), tc.stated) {
					t.Fatalf("SupportedContracts records %d; this case proves nothing", tc.stated)
				}
				body := stateStating(t, handler, "192.0.2.1:41000", tc.stated)
				nearest := stateStating(t, handler, "192.0.2.1:41000", tc.nearest)
				if string(body) != string(nearest) {
					t.Errorf("a caller stating %d was served %s, want the nearest recorded contract %d's body %s",
						tc.stated, body, tc.nearest, nearest)
				}
			})
		}
		if logs.Len() != 0 {
			t.Errorf("a caller between the floor and this contract logged %s, want no line", logs)
		}
	})

	t.Run("a caller in no pod", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			remote        string
			claims        []supervise.Claim
			placesCallers bool
		}{
			{name: "a daemon that places no caller", remote: statePodAddr + ":41000", claims: podClaims(ServingFloor)},
			{name: "an address of no pod", remote: "192.0.2.1:41000", claims: podClaims(ServingFloor), placesCallers: true},
			{name: "a pod whose claims hold no process", remote: statePodAddr + ":41000", claims: processlessClaims(ServingFloor), placesCallers: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs := &bytes.Buffer{}
				handler := shapedServer(t, populatedState(t), tc.claims, logs, tc.placesCallers)

				// Each row's claims are its own, so each reports its own `lowestPluginContract`,
				// which no caller's contract decides: the shape is what is compared.
				if body := stateStating(t, handler, tc.remote, 0); withoutLowestContract(t, body) != withoutLowestContract(t, whole) {
					t.Errorf("state = %s, want the body this contract writes %s", body, whole)
				}
				if logs.Len() != 0 {
					t.Errorf("logs = %s, want no line: no claim places this caller", logs)
				}
			})
		}
	})
}

// processlessClaims is a project whose claims record contract and hold no process, as every
// suspended, failed and retired claim does (supervise's letGo): no claim of it is in any pod.
func processlessClaims(contract int) []supervise.Claim {
	return []supervise.Claim{
		{Token: "legion-legion-legion-208-planner", State: supervise.StateSuspended, PluginContract: contract},
		{Token: "legion-legion-legion-208-implementer", State: supervise.StateRetired, PluginContract: contract},
	}
}

// The floor is read from the constant, and a caller below it is served the floor's own restore
// shape: the two numbers of a below-floor caller are the floor on the restore side and the
// caller's own on the strip side.
func TestTheServedContractsOfACallerBelowTheFloor(t *testing.T) {
	below := ServingFloor - 1

	if restore := restoreContract(below); restore != ServingFloor {
		t.Errorf("the restore contract of %d = %d, want the floor %d", below, restore, ServingFloor)
	}
	if strip := stripContract(below); strip != below {
		t.Errorf("the strip contract of %d = %d, want its own number: such a caller loses no key it loses today", below, strip)
	}
	if restore, strip := restoreContract(DaemonAPIVersion()+1), stripContract(DaemonAPIVersion()+1); restore != DaemonAPIVersion() || strip != DaemonAPIVersion() {
		t.Errorf("the contracts of a caller above this daemon's own = %d and %d, want %d on both sides", restore, strip, DaemonAPIVersion())
	}
}

// Every answer of the table has a row in shapeAnswer: a shape change that lands with no row of
// its own is an answer this names and that does not, which is a programming error rather than a
// body served unshaped.
//
// Each row is handed a body its own route could write, because the five rows whose shape has
// moved shape a State, an Issue, the release records, a refusal and register_plan's answer and
// panic on anything else: a row handed the wrong type would fail here as an answer with no row
// does, and never serve that body unshaped.
func TestShapeAnswerShapesEveryAnswer(t *testing.T) {
	state := populatedState(t)
	for _, answer := range Answers() {
		t.Run(string(answer.ID), func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("shapeAnswer has no row for %s: %v", answer.ID, recovered)
				}
			}()
			switch answer.ID {
			case AnswerState:
				body, _ := shapeAnswer(AnswerState, state, ServingFloor, ServingFloor)
				shaped, isState := body.(State)
				if !isState {
					t.Fatalf("the state answer was shaped to %T, want a State", body)
				}
				if !shaped.Settings.maxFixAttemptsShown {
					t.Error("the state served at the floor carries no `max_fix_attempts` stand-in")
				}
			case AnswerIssueRecord:
				body, _ := shapeAnswer(AnswerIssueRecord, state.Issues[recordGoldenIssue], ServingFloor, ServingFloor)
				shaped, isIssue := body.(Issue)
				if !isIssue {
					t.Fatalf("the record answer was shaped to %T, want an Issue", body)
				}
				if shaped.PullRequest == nil || !shaped.PullRequest.fixAttemptsShown {
					t.Error("the record served at the floor carries no `fixAttempts` stand-in: the record's row shapes the issue as the state's row shapes it (shapeIssue)")
				}
				if shaped.MemoryGroup != "" {
					t.Error("the record served at the floor carries its tree's memory group, which the reader before memoryContract refuses")
				}
			case AnswerReleaseRecords:
				records := ReleaseRecordsResponse{Releases: []ReleaseRecord{}, Holds: []HoldRecord{},
					Resumes: []ResumeRecord{{ID: 203, Issue: "LEGION-210", Kind: "pause", Pause: 7}}}
				body, _ := shapeAnswer(AnswerReleaseRecords, records, ServingFloor, ServingFloor)
				shaped, isRecords := body.(ReleaseRecordsResponse)
				if !isRecords {
					t.Fatalf("the release records were shaped to %T, want a ReleaseRecordsResponse", body)
				}
				if len(shaped.Resumes) != 1 || shaped.Resumes[0].Pause != 0 {
					t.Errorf("the release records served at the floor carry a resume's `pause`, which pauseContract added: %+v", shaped.Resumes)
				}
				if records.Resumes[0].Pause != 7 {
					t.Error("the shaping wrote the resume of the body the route built: the resumes are copied before one is written")
				}
			case AnswerRefusal:
				failure := Failure{Code: "GRANT_NOT_FOUND", Error: "no grant for this command"}
				if shaped, _ := shapeAnswer(AnswerRefusal, failure, ServingFloor, ServingFloor); shaped != any(failure) {
					t.Errorf("a plain refusal was shaped to %v, want the body it was given", shaped)
				}
				refused := PauseFailure{Failure: Failure{Code: projectPaused, Error: "the project is paused"}, Setting: "runtime: tmux"}
				if shaped, _ := shapeAnswer(AnswerRefusal, refused, ServingFloor, ServingFloor); shaped != any(refused.Failure) {
					t.Errorf("a PauseFailure was shaped to %+v at the floor, want its Failure alone", shaped)
				}
			case AnswerPlanRegister:
				needs := []string{"a"}
				registered := PlanRegisterResponse{Plan: RegisteredPlan{Version: 2, Tasks: []RegisteredTask{{ID: "b", Steps: []string{}, Needs: &needs}}}}
				body, _ := shapeAnswer(AnswerPlanRegister, registered, OwnerPlanContract, OwnerPlanContract)
				shaped, isRegistered := body.(PlanRegisterResponse)
				if !isRegistered {
					t.Fatalf("register_plan's answer was shaped to %T, want a PlanRegisterResponse", body)
				}
				if shaped.Plan.Tasks[0].Needs != nil {
					t.Errorf("register_plan's answer served at %d carries a task's `needs`, which tasksContract added", OwnerPlanContract)
				}
				if registered.Plan.Tasks[0].Needs == nil {
					t.Error("the shaping wrote the task of the body the route built: the tasks are copied before one is written")
				}
				for _, contract := range []int{entryNumber(tasksContract), DaemonAPIVersion()} {
					body, _ := shapeAnswer(AnswerPlanRegister, registered, contract, contract)
					if kept, ok := body.(PlanRegisterResponse); !ok || kept.Plan.Tasks[0].Needs == nil || !slices.Equal(*kept.Plan.Tasks[0].Needs, needs) {
						t.Errorf("register_plan's answer served at %d = %+v, want each task's `needs` kept", contract, body)
					}
				}
			default:
				body := map[string]string{"shape": "unmoved"}
				if shaped, _ := shapeAnswer(answer.ID, body, ServingFloor, ServingFloor); !reflectEqualBody(shaped, body) {
					t.Errorf("the %s answer was shaped to %v, want the body it was given", answer.ID, shaped)
				}
			}
		})
	}
}

// The five rows that shape a body of their own panic on a body of another type, as a row that
// does not exist panics: a route that wrote a State under AnswerIssueRecord would otherwise
// have it served unshaped and in silence.
func TestShapeAnswerPanicsOnABodyItsRowCannotShape(t *testing.T) {
	for _, id := range []AnswerID{AnswerState, AnswerIssueRecord, AnswerReleaseRecords, AnswerRefusal, AnswerPlanRegister} {
		t.Run(string(id), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("shapeAnswer served a body of the wrong type under %s without a sound", id)
				}
			}()
			shapeAnswer(id, map[string]string{"shape": "wrong"}, ServingFloor, ServingFloor)
		})
	}
}

// reflectEqualBody is whether a shaped answer is the very body it was given, for the rows whose
// rule is the identity.
func reflectEqualBody(shaped any, body map[string]string) bool {
	given, ok := shaped.(map[string]string)
	return ok && len(given) == len(body) && given["shape"] == body["shape"]
}

// The header decides the shape and nothing else: a caller that states a contract other than this
// daemon's own is answered on a claim route and on a grant-bearing route exactly as it is today,
// the stale-caller refusal reading the contract the claim's registration recorded rather than the
// header's number.
func TestTheHeaderRefusesNoCallerAndMovesNoStaleRefusal(t *testing.T) {
	h, logs := newStaleHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	current := h.registerContract(boot, session, DaemonAPIVersion(), true)
	recorded := DaemonAPIVersion() - 1
	h.makeStale(t, token, session, current.Secret, recorded)
	stated := http.Header{pluginContractHeader: []string{strconv.Itoa(ServingFloor)}}

	wantRefusal(t,
		h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
			ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation,
		}, stated),
		http.StatusConflict, claim.StalePluginContract(recorded, DaemonAPIVersion()).Message)
	refused(t, h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: session, Secret: current.Secret, Tree: "LEGION-208", Issue: "LEGION-208",
	}, stated), http.StatusConflict, "STALE_PLUGIN_CONTRACT")

	logged := logs.String()
	if !strings.Contains(logged, fmt.Sprintf(`"pluginContract":%d`, recorded)) {
		t.Errorf("logs %s do not name the contract the claim's registration recorded", logged)
	}
	if strings.Contains(logged, fmt.Sprintf(`"pluginContract":%d`, ServingFloor)) {
		t.Errorf("logs %s name the header's contract, which decides the shape and nothing else", logged)
	}
}

// The four exemptions staleClaim carries are the four it carried before a header existed, and a
// caller that states a contract is exempt by none of them the less: a claim whose launch installs
// no plugin, a claim whose update round is open, a claim stored working, and every claim of a
// daemon that can relaunch nothing is served on the contract its registration recorded.
func TestTheExemptionsFromTheStaleRefusalStandWithAHeader(t *testing.T) {
	recorded := DaemonAPIVersion() - 1
	stated := http.Header{pluginContractHeader: []string{strconv.Itoa(ServingFloor)}}

	for _, tc := range []struct {
		name    string
		harness func(t *testing.T) *harness
		seed    func(t *testing.T, h *harness, token claim.Token)
	}{
		{
			name: "a claim whose launch installs no plugin",
			harness: func(t *testing.T) *harness {
				return newNoInstallHarness(t, func(c supervise.Claim) bool { return c.Role == claim.RoleImplementer })
			},
		},
		{
			name:    "a claim whose update round is open",
			harness: newHarness,
			seed: func(t *testing.T, h *harness, token claim.Token) {
				staleWith(t, h, token, func(c *supervise.Claim) { c.UpdateRound = "round-1" })
			},
		},
		{
			name:    "a claim stored working",
			harness: newHarness,
			seed: func(t *testing.T, h *harness, token claim.Token) {
				staleWith(t, h, token, func(c *supervise.Claim) { c.State = supervise.StateWorking })
			},
		},
		{
			name:    "a daemon that can relaunch nothing",
			harness: newHarnessUnrelaunchable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.harness(t)
			relaunchable := tc.name != "a daemon that can relaunch nothing"
			token, boot := h.launch("LEGION-208", claim.RoleImplementer)
			session := "ses_implementer"
			current := h.registerContract(boot, session, DaemonAPIVersion(), relaunchable)
			h.makeStale(t, token, session, current.Secret, recorded)
			if tc.seed != nil {
				tc.seed(t, h, token)
			}

			ready := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
				ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation,
			}, stated)
			grants := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
				SessionID: session, Secret: current.Secret, Tree: "LEGION-208", Issue: "LEGION-208",
			}, stated)

			if ready.Code != http.StatusNoContent {
				t.Errorf("ready from an exempt claim = %d, want 204; body %s", ready.Code, ready.Body)
			}
			if grants.Code != http.StatusOK {
				t.Errorf("grants for an exempt claim = %d, want 200; body %s", grants.Code, grants.Body)
			}
		})
	}
}

// staleWith is the claim as makeStale left it, with one more field of the stale window set on it
// — the open update round, or the working state a turn leaves — written to the store and the
// machine rebuilt from it, as makeStale writes its own.
func staleWith(t *testing.T, h *harness, token claim.Token, with func(*supervise.Claim)) {
	t.Helper()
	c := h.stored(token)
	with(&c)
	if err := h.store.PutClaim(t.Context(), c); err != nil {
		t.Fatalf("seed the stale window of %s: %v", token, err)
	}
	h.supervisor.restart(t, token)
}
