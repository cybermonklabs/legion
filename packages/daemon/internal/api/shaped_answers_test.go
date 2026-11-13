package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// shapedServer is stateServer with its log captured, so a test reads the one line each of the
// outer two edges writes and proves the quiet of every caller between them. placesCallers is
// whether the daemon can place a caller in a pod at all: under runtime tmux Options.CallerPod is
// nil, and no caller there is in any pod.
func shapedServer(t *testing.T, state State, claims []supervise.Claim, logs *bytes.Buffer, placesCallers bool) http.Handler {
	t.Helper()
	options := Options{
		Steps: config.DesignGateRootIssues.Steps(),
		State: fakeSource{state: state, claims: claims}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		// Warnings alone, which are the two edge lines servedContracts writes and what every
		// row below counts. The record of each band the shaping leaves out of a state is an
		// info line of its own, and narrowing_test.go is what reads those (recordShaping).
		Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if placesCallers {
		options.CallerPod = func(addr netip.Addr) (string, bool) {
			if addr == netip.MustParseAddr(statePodAddr) {
				return statePodUID, true
			}
			return "", false
		}
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
		{name: "a caller that states the floor", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion), stated: ServingFloor},
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
	handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion), logs, true)
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
// Both builds of this pod read the body the pod reading decides, which is what makes the union
// body the right one: neither states a contract of its own, so each is served what the pod
// says, and no one body serves them both unless it carries the newer's keys and the older's
// stand-ins. The pod of the next case differs in exactly that, and in nothing else.
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
	if stated := stateStating(t, handler, statePodAddr+":41000", DaemonAPIVersion); string(stated) != string(whole) {
		t.Errorf("a caller of the mixed pod stating this contract was served %s, want the body this contract writes %s", stated, whole)
	}
}

// The pod a roll makes, holding one build at the floor and one at this daemon's own contract,
// is served the floor's shape and not the union of the two. The build at 115 states its
// contract in the header, so contractsFor answers it there and it never reads the body the pod
// decides; the build at 46 does read it, and it is the only process that does. Counting the
// newer one would serve that reader `admission.trees`, the memory fields and the rest, which
// its own schema refuses the whole body for (podContracts).
//
// This narrows no rule: where both builds of a pod read the pod-decided body, the union body
// is still the one they can both read, which the `{46, 73}` case above holds. The two pods
// differ in who reads the answer and in nothing else.
func TestAPodWhoseNewestBuildStatesItsOwnContractIsServedTheOldests(t *testing.T) {
	logs := &bytes.Buffer{}
	handler := shapedServer(t, populatedState(t), mixedPodClaims(ServingFloor, DaemonAPIVersion), logs, true)
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

	// The 115 build of that same pod is served this contract's own body, by its header.
	wantThisContract(t, stateStating(t, handler, statePodAddr+":41000", DaemonAPIVersion))
}

// A value follows the pod's lowest contract where a key follows its highest, because no reader
// requires a value and a reader whose enumeration does not name one refuses the whole body for
// it. A pod recording 64 and 85 is served the wait of every root a person declined without its
// answered pair: `declined` arrived at version.go's 68, and the pod's 64 process parses
// `answeredBy` with an enumeration of the eight values before it. The `waitingOn` block itself
// stays, with the kind and the moment every contract reads.
func TestAMixedPodIsServedNoWaitAnswerItsOldestProcessRefuses(t *testing.T) {
	const declined, oldest = "declined", 64
	logs := &bytes.Buffer{}
	handler := shapedServer(t, stateCarryingTheRestOfTheShape(t), mixedPodClaims(oldest, whyAndLineFieldsContract), logs, true)
	answered := ""
	for key, wait := range waitsServed(t, stateStating(t, handler, "192.0.2.1:41000", DaemonAPIVersion)) {
		if wait.AnsweredBy == declined {
			answered = key
		}
	}
	if answered == "" {
		t.Fatalf("no issue of the fixture's state carries a wait answered %q; this proves nothing", declined)
	}

	mixed := stateStating(t, handler, statePodAddr+":41000", 0)

	served, carried := waitsServed(t, mixed)[answered]
	if !carried {
		t.Fatalf("the {%d, %d} pod is served %s with no wait at all: %s", oldest, whyAndLineFieldsContract, answered, mixed)
	}
	if served.AnsweredBy != "" || served.AnsweredAt != "" {
		t.Errorf("the {%d, %d} pod is served %s answered %q at %q, want neither key: the value arrived at 68 and the pod's %d process refuses the whole body for it",
			oldest, whyAndLineFieldsContract, answered, served.AnsweredBy, served.AnsweredAt, oldest)
	}
	if served.Kind == "" || served.Since == "" {
		t.Errorf("the {%d, %d} pod is served %s's wait as %+v, want the kind and the moment it began: both are true at every contract",
			oldest, whyAndLineFieldsContract, answered, served)
	}
	if logs.Len() != 0 {
		t.Errorf("the mixed pod logged %s, want no line: both of its contracts are ones this daemon describes", logs)
	}
}

// The other value strip follows the pod's lowest contract for the same reason: a pod recording
// 51 and 59 is served the report of claims nothing will wake without its retired entries, since
// the pod's 51 process parses `strandedClaims[].state` with an enum of `suspended` and `failed`
// and refuses the whole body for the `retired` of version.go's 56. Every entry whose state that
// process knows stays, in the order the report named it.
func TestAMixedPodIsServedNoStrandedStateItsOldestProcessRefuses(t *testing.T) {
	logs := &bytes.Buffer{}
	handler := shapedServer(t, populatedState(t), mixedPodClaims(treeContract, fixAttemptsContract), logs, true)
	every, carried := strandedServed(t, stateStating(t, handler, "192.0.2.1:41000", DaemonAPIVersion))
	var kept []map[string]string
	for _, entry := range every {
		if entry["state"] != string(supervise.StateRetired) {
			kept = append(kept, entry)
		}
	}
	if !carried || len(kept) == len(every) || len(kept) == 0 {
		t.Fatalf("the fixture's state names %v, want a retired claim nothing will wake beside claims in other states; this proves nothing", every)
	}

	mixed := stateStating(t, handler, statePodAddr+":41000", 0)

	stranded, carriedMixed := strandedServed(t, mixed)
	if !carriedMixed {
		t.Fatalf("the {%d, %d} pod is served no strandedClaims: the key is never absent and never null; %s",
			treeContract, fixAttemptsContract, mixed)
	}
	if !reflect.DeepEqual(stranded, kept) {
		t.Errorf("the {%d, %d} pod is served the claims nothing will wake %v, want %v: the pod's %d process refuses the whole body for a retired entry",
			treeContract, fixAttemptsContract, stranded, kept, treeContract)
	}
	if logs.Len() != 0 {
		t.Errorf("the mixed pod logged %s, want no line: both of its contracts are ones this daemon describes", logs)
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

// mixedPodClaims is the pod at statePodAddr holding one claim at low, one at high and one that
// records no contract at all, beside a claim of another pod.
func mixedPodClaims(low, high int) []supervise.Claim {
	claims := podClaims(low)
	claims[1] = claimInPod("legion-legion-legion-208-implementer", statePodUID, high)
	return append(claims, claimInPod("legion-legion-legion-208-tester", statePodUID, 0))
}

// keysAddedAfterTheFloor is the thirteen keys the servings-down of the seven bands between the
// floor and claimsToRefreshContract remove, as the wire names them: a caller served all of them
// is served everything this daemon writes above the floor and below its own contract, but each
// check's `role`, which shapeOf reads instead because an issue carries a check only while its
// production check is open.
var keysAddedAfterTheFloor = []string{
	`"updateRound"`, `"updateHead"`, `"memoryGroup"`, `"memoryDrawMode"`, `"memoryDrawnAt"`,
	`"trees"`, `"tree"`, `"wakesWaiting"`, `"gatedSteps"`, `"step"`, `"` + WaitingLineField + `"`,
	`"backwardMoves"`, `"claimsToRefresh"`,
}

// keysOfThisContract is the five keys this change's own contract adds, which shapedAnswersContract
// serves down: the state's own `lowestPluginContract` and the four of each claim view's round
// report.
var keysOfThisContract = []string{
	`"lowestPluginContract"`, `"updateState"`, `"updateReaskedForPushCheck"`,
	`"unpromptedTurnTakesAnswerBack"`, `"wake"`,
}

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
	lowest, highest, recorded := podContracts(podClaims(DaemonAPIVersion), statePodUID)

	if lowest != 0 || highest != 0 || recorded != "" {
		t.Errorf("a pod of claims at this daemon's own contract reports %d, %d and %q, want nothing on all three",
			lowest, highest, recorded)
	}
	mixedLowest, mixedHighest, _ := podContracts(mixedPodClaims(ServingFloor, DaemonAPIVersion), statePodUID)
	if mixedLowest != ServingFloor || mixedHighest != ServingFloor {
		t.Errorf("the rolled pod reports %d and %d, want the floor on both sides: its 115 build reads no body this decides",
			mixedLowest, mixedHighest)
	}
}

// The four edges of the contract a caller is served, and the one log line each of the outer two
// writes.
func TestTheEdgesOfTheContractACallerIsServed(t *testing.T) {
	logs := &bytes.Buffer{}
	full := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion), logs, true)
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

		// The rule holds over every answer, so the record route of the same pod is read too:
		// each issue's record is the issue the state above carries for that pod, with every key
		// added after the floor stripped, `pullRequest.fixAttempts` as its stand-in and no
		// `check.role` (shapeIssue). The records are read after the log is counted, since each
		// read is a below-floor caller of its own.
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
		handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion), logs, true)

		body := stateStating(t, handler, "192.0.2.1:41000", DaemonAPIVersion+1)

		if string(body) != string(whole) {
			t.Errorf("a caller above this contract was served %s, want the body this contract writes %s", body, whole)
		}
		logged := logs.String()
		if !strings.Contains(logged, fmt.Sprintf(`"pluginContract":%d`, DaemonAPIVersion+1)) ||
			!strings.Contains(logged, `"caller":"192.0.2.1:41000"`) {
			t.Errorf("logs %s do not name the contract and the caller's address", logged)
		}
		if lines := strings.Count(strings.TrimSpace(logged), "\n") + 1; lines != 1 {
			t.Errorf("logs %s carry %d lines, want the one line this edge writes", logged, lines)
		}
	})

	t.Run("a contract no change ever shipped under", func(t *testing.T) {
		logs := &bytes.Buffer{}
		handler := shapedServer(t, populatedState(t), podClaims(DaemonAPIVersion), logs, true)
		for _, tc := range []struct{ stated, nearest int }{{stated: 47, nearest: 46}, {stated: 50, nearest: 49}} {
			t.Run(strconv.Itoa(tc.stated), func(t *testing.T) {
				if slices.Contains(SupportedContracts, tc.stated) {
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
	if restore, strip := restoreContract(DaemonAPIVersion+1), stripContract(DaemonAPIVersion+1); restore != DaemonAPIVersion || strip != DaemonAPIVersion {
		t.Errorf("the contracts of a caller above this daemon's own = %d and %d, want %d on both sides", restore, strip, DaemonAPIVersion)
	}
}

// Every answer of the table has a row in shapeAnswer: a shape change that lands with no row of
// its own is an answer this names and that does not, which is a programming error rather than a
// body served unshaped.
//
// Each row is handed a body its own route could write, because the two rows whose shape has
// moved shape a State and an Issue and panic on anything else: a row handed the wrong type
// would fail here as an answer with no row does, and never serve that body unshaped.
func TestShapeAnswerShapesEveryAnswer(t *testing.T) {
	state := populatedState(t)
	for _, answer := range Answers {
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
			default:
				body := map[string]string{"shape": "unmoved"}
				if shaped, _ := shapeAnswer(answer.ID, body, ServingFloor, ServingFloor); !reflectEqualBody(shaped, body) {
					t.Errorf("the %s answer was shaped to %v, want the body it was given", answer.ID, shaped)
				}
			}
		})
	}
}

// The two rows that shape a body of their own panic on a body of another type, as a row that
// does not exist panics: a route that wrote a State under AnswerIssueRecord would otherwise
// have it served unshaped and in silence.
func TestShapeAnswerPanicsOnABodyItsRowCannotShape(t *testing.T) {
	for _, id := range []AnswerID{AnswerState, AnswerIssueRecord} {
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
	current := h.registerContract(boot, session, DaemonAPIVersion, true)
	recorded := DaemonAPIVersion - 1
	h.makeStale(t, token, session, current.Secret, recorded)
	stated := http.Header{pluginContractHeader: []string{strconv.Itoa(ServingFloor)}}

	wantRefusal(t,
		h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
			ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation,
		}, stated),
		http.StatusConflict, claim.StalePluginContract(recorded, DaemonAPIVersion).Message)
	wantFailure(t, h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
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
	recorded := DaemonAPIVersion - 1
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
			current := h.registerContract(boot, session, DaemonAPIVersion, relaunchable)
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
