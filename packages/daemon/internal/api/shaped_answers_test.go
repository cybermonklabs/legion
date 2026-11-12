package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
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
		Log: slog.New(slog.NewJSONHandler(logs, nil)),
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

// The mixed pod: a caller that states no contract, in a pod whose claims record 46 and 81 at
// once, is served the keys the newer process requires — `check.role` and every other key added
// after 46 — and the stand-ins for the keys removed after 46, which the older process requires.
// The pod's third claim records no contract and moves neither half. A caller of that same pod
// that states its own contract is served that contract's shape, whichever of the two it states.
func TestAMixedPodIsServedTheHighestsKeysAndTheLowestsStandIns(t *testing.T) {
	claims := mixedPodClaims(ServingFloor, DaemonAPIVersion)
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

// mixedPodClaims is the pod at statePodAddr holding one claim at low, one at high and one that
// records no contract at all, beside a claim of another pod.
func mixedPodClaims(low, high int) []supervise.Claim {
	claims := podClaims(low)
	claims[1] = claimInPod("legion-legion-legion-208-implementer", statePodUID, high)
	return append(claims, claimInPod("legion-legion-legion-208-tester", statePodUID, 0))
}

// keysAddedAfterTheFloor is the sixteen keys the ten servings-down below remove, as the wire
// names them: the two of the update round, the three of the memory draw, the two of the trees,
// `wakesWaiting`, `gatedSteps`, the claim views' `step`, each waiting issue's place in the line,
// and the five this contract's own entry adds, which shapedAnswersContract serves down. Three
// keys a serving-down removes are left out of it, each read another way: each check's `role`,
// which shapeOf reads because an issue carries a check only while its production check is open,
// and `backwardMoves` and `claimsToRefresh`, whose own cases walk the bands they arrived at
// (backward_moves_test.go, memory_test.go).
var keysAddedAfterTheFloor = []string{
	`"updateRound"`, `"updateHead"`, `"memoryGroup"`, `"memoryDrawMode"`, `"memoryDrawnAt"`,
	`"trees"`, `"tree"`, `"wakesWaiting"`, `"gatedSteps"`, `"step"`, `"` + WaitingLineField + `"`,
	`"lowestPluginContract"`, `"updateState"`, `"updateReaskedForPushCheck"`,
	`"unpromptedTurnTakesAnswerBack"`, `"wake"`,
}

// A claim that holds no process is in no pod's reckoning: suspending, failing or retiring a claim
// ends what the machine had with its process, and the claim gives up its locator with it
// (supervise's letGo), so no such claim records a pod. That is why no fixture here holds a
// retired claim of a pod: there is no such claim to hold.
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

	lowest, highest, recorded := podContracts(append(podClaims(DaemonAPIVersion), stopped...), statePodUID)

	if lowest != DaemonAPIVersion || highest != DaemonAPIVersion {
		t.Errorf("the pod's contracts = %d and %d, want %d on both sides: a claim with no process is in no pod",
			lowest, highest, DaemonAPIVersion)
	}
	if recorded == "" {
		t.Error("the lowest contract names no claim, want the claim of the pod it was read from")
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
		// the caller's own number, which is below every one of the eight contracts those
		// sixteen keys arrived at: it loses every key it loses today and no more.
		wantStandIns(t, body)
		for _, key := range keysAddedAfterTheFloor {
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
				shaped, isState := shapeAnswer(AnswerState, state, ServingFloor, ServingFloor).(State)
				if !isState {
					t.Fatalf("the state answer was shaped to %T, want a State", shaped)
				}
				if !shaped.Settings.maxFixAttemptsShown {
					t.Error("the state served at the floor carries no `max_fix_attempts` stand-in")
				}
			case AnswerIssueRecord:
				shaped, isIssue := shapeAnswer(AnswerIssueRecord, state.Issues[recordGoldenIssue], ServingFloor, ServingFloor).(Issue)
				if !isIssue {
					t.Fatalf("the record answer was shaped to %T, want an Issue", shaped)
				}
				if shaped.PullRequest == nil || !shaped.PullRequest.fixAttemptsShown {
					t.Error("the record served at the floor carries no `fixAttempts` stand-in: the record's row shapes the issue as the state's row shapes it (shapeIssue)")
				}
				if shaped.MemoryGroup != "" {
					t.Error("the record served at the floor carries its tree's memory group, which the reader before memoryContract refuses")
				}
			default:
				body := map[string]string{"shape": "unmoved"}
				if shaped := shapeAnswer(answer.ID, body, ServingFloor, ServingFloor); !reflectEqualBody(shaped, body) {
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
