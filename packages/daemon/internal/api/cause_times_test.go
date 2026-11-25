package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A claim keeps its death's cause through every later ready, and a cause carried no time, so a
// reader of the state took a death of days before for the reason a worker is not running now
// (#5349). Each cause is served beside the time the daemon recorded it.

// causeRecordedAt is the moment the tests below record a cause at, and causeReadyAt the agent's
// ready an hour later.
var (
	causeRecordedAt = time.Date(2026, 11, 21, 4, 3, 34, 0, time.UTC)
	causeReadyAt    = causeRecordedAt.Add(time.Hour)
)

// steppedClock is the route harness's still clock at a time a test moves: every timer stays armed
// and never due, as stillClock leaves it, and Now answers the time the test last set, which the
// machine records each cause and each ready at.
type steppedClock struct {
	stillClock
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// newSteppedHarness is the route harness whose machines read clock, set at causeRecordedAt. It is
// set before any claim is created, since a machine takes its dependencies when it is built.
func newSteppedHarness(t *testing.T) (*harness, *steppedClock) {
	t.Helper()
	h := newHarness(t)
	clock := &steppedClock{now: causeRecordedAt}
	h.supervisor.deps.Clock = clock
	return h, clock
}

// servedClaimView is the claim view of c as GET /legion/v1/state serves it, decoded as the
// plugin's reader sees it: c is the implementer of LEGION-209 in a state of that one issue, asked
// from no pod, as the operator's `legion state --json` asks.
func servedClaimView(t *testing.T, c supervise.Claim) map[string]any {
	t.Helper()
	state := State{Issues: map[string]Issue{"LEGION-209": {
		Key: "LEGION-209", Tree: "LEGION-209",
		Workers: map[claim.Role]PhaseView{claim.RoleImplementer: {Claim: ClaimViewOf(c)}},
	}}}
	views := claimViews(t, stateFrom(t, state, "192.0.2.1:41000", []supervise.Claim{c}))
	if len(views) != 1 {
		t.Fatalf("the state of one claim carries %d claim views, want 1: %v", len(views), views)
	}
	return views[0]
}

// listedClaimKeys is token's claim as GET /legion/v1/operator/claims writes it, the body
// `legion claims list --json` prints, decoded key by key.
func listedClaimKeys(t *testing.T, h *harness, token claim.Token) map[string]any {
	t.Helper()
	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list the claims = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var list struct {
		Claims []map[string]any `json:"claims"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode the claim listing %s: %v", recorder.Body, err)
	}
	for _, c := range list.Claims {
		if c["token"] == string(token) {
			return c
		}
	}
	t.Fatalf("the listing carries no %s: %s", token, recorder.Body)
	return nil
}

// wantTimeKey fails unless view carries key as the RFC 3339 time want.
func wantTimeKey(t *testing.T, what string, view map[string]any, key string, want time.Time) {
	t.Helper()
	raw, carried := view[key]
	if !carried {
		t.Errorf("%s carries no %s, want %s: %v", what, key, want.Format(time.RFC3339), view)
		return
	}
	text, _ := raw.(string)
	got, err := time.Parse(time.RFC3339, text)
	if err != nil || !got.Equal(want) {
		t.Errorf("%s carries %s %v, want the RFC 3339 time %s", what, key, raw, want.Format(time.RFC3339))
	}
}

// wantNoKeys fails where view carries any of keys.
func wantNoKeys(t *testing.T, what string, view map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if value, carried := view[key]; carried {
			t.Errorf("%s carries %s %v, want no such key", what, key, value)
		}
	}
}

// A claim's process found gone at T and its agent ready again at T+1h: the claim view of the
// state and the operator's claim view both carry the death's cause beside `lastDeathAt` T, so a
// reader tells the death the running agent survived from one that is why it is not running, and
// `lastReady` T+1h after it. The claim was never refused a start, so neither view carries a start
// failure or its time.
func TestALastDeathCarriesItsTime(t *testing.T) {
	h, clock := newSteppedHarness(t)
	token, _ := h.launch("LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	m, _ := h.supervisor.Machine(token)
	const detail = "pod legion-legion-209 (uid 8fdd52cc) ended: init container workspace-init exited 1"
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *m.Claim().Locator, Kind: runtime.Gone, At: causeRecordedAt.Add(-time.Minute), Detail: detail}}); err != nil {
		t.Fatalf("the death of %s: %v", token, err)
	}
	relaunched := m.Claim().Generation
	if relaunched < 2 {
		t.Fatalf("the dead claim is at generation %d, want the relaunch the death raised", relaunched)
	}

	clock.set(causeReadyAt)
	h.ready(token, "ses_implementer", relaunched)

	stored := h.stored(token)
	if stored.LastDeathCause == "" {
		t.Fatalf("the stored claim records no death cause after the death: %+v", stored)
	}
	for what, view := range map[string]map[string]any{
		"the state's claim view":    servedClaimView(t, stored),
		"the operator's claim view": listedClaimKeys(t, h, token),
	} {
		if view["lastDeathCause"] != stored.LastDeathCause {
			t.Errorf("%s carries lastDeathCause %v, want %q", what, view["lastDeathCause"], stored.LastDeathCause)
		}
		wantTimeKey(t, what, view, "lastDeathAt", causeRecordedAt)
		wantTimeKey(t, what, view, "lastReady", causeReadyAt)
		wantNoKeys(t, what, view, "lastStartFailure", "lastStartFailureAt")
	}
}

// A start the runtime refused at T, and the next try of the same launch started: the claim waits
// on an agent that has not said it is ready, so both claim views carry the start failure beside
// `lastStartFailureAt` T. Once the agent says it is ready, at T+1h, the claim drops the start
// failure with its time, and neither view carries either key.
func TestALastStartFailureCarriesItsTimeUntilReady(t *testing.T) {
	h, clock := newSteppedHarness(t)
	token, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, "LEGION-209", treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of LEGION-209: %v", err)
	}
	m, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: "LEGION-209", Issue: "LEGION-209", Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "role prompt")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	h.runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("the launcher refused the start: the pod was not ready within the worker boot timeout")})
	if err := m.Handle(h.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn %s: %v", token, err)
	}

	waiting := h.stored(token)
	if waiting.State != supervise.StateLaunching || waiting.LastStartFailure == "" {
		t.Fatalf("%s is %s with start failure %q, want it launching after the refused try, the refusal kept", token, waiting.State, waiting.LastStartFailure)
	}
	for what, view := range map[string]map[string]any{
		"the state's claim view":    servedClaimView(t, waiting),
		"the operator's claim view": listedClaimKeys(t, h, token),
	} {
		if view["lastStartFailure"] != waiting.LastStartFailure {
			t.Errorf("%s carries lastStartFailure %v, want %q", what, view["lastStartFailure"], waiting.LastStartFailure)
		}
		wantTimeKey(t, what, view, "lastStartFailureAt", causeRecordedAt)
		wantNoKeys(t, what, view, "lastDeathCause", "lastDeathAt")
	}

	clock.set(causeReadyAt)
	h.ready(token, "ses_implementer", waiting.Generation)

	up := h.stored(token)
	for what, view := range map[string]map[string]any{
		"the state's claim view":    servedClaimView(t, up),
		"the operator's claim view": listedClaimKeys(t, h, token),
	} {
		wantTimeKey(t, what+" after the ready", view, "lastReady", causeReadyAt)
		wantNoKeys(t, what+" after the ready", view, "lastStartFailure", "lastStartFailureAt")
	}
}

// A plugin below causeTimesContract did not ask for the two times, so a caller of such a contract
// is served no claim view carrying either: one that states the contract below in its header, on
// the state route and on the record route, and one whose pod's claims record a contract a pod
// still places (pauseContract). The causes themselves stay as their own contract serves them.
// A caller at causeTimesContract, and one in no pod, are served both times of the fixture's
// architect, whose view carries every key, and the death time this test gives the issue's
// implementer, so the band is read on a worker's view as well as the architect's (it narrows
// each through withEachClaimView). The worker is given it here rather than in the golden state:
// a worker's lastDeathCause there would be a key no record of a contract below this one holds.
// The daemon's log names what the narrowed body lost. Such a caller stands below the band of
// each entry too, stateSeqContract's among them, so the log holds each such band's line after the
// cause times' line and the Dispatch credential's.
func TestAnOlderContractIsServedNoCauseTimes(t *testing.T) {
	state := populatedState(t)
	architect := state.Issues[recordGoldenIssue].Architect
	if architect == nil || architect.LastDeathAt == nil || architect.LastStartFailureAt == nil {
		t.Fatalf("the fixture's architect of %s carries no cause times; this test proves nothing", recordGoldenIssue)
	}
	implementer, held := state.Issues[recordGoldenIssue].Workers[claim.RoleImplementer]
	if !held || implementer.Claim.LastDeathCause != "" {
		t.Fatalf("the fixture's %s holds no implementer with no death of its own (%+v); this test gives it one", recordGoldenIssue, implementer.Claim)
	}
	workerDied := time.Date(2026, 9, 22, 9, 14, 5, 0, time.UTC)
	implementer.Claim.LastDeathCause, implementer.Claim.LastDeathAt = "gone: the roles container's process was OOMKilled", &workerDied
	state.Issues[recordGoldenIssue].Workers[claim.RoleImplementer] = implementer
	causeKeys := []string{"lastDeathCause", "lastStartFailure"}
	timeKeys := []string{"lastDeathAt", "lastStartFailureAt"}
	handler := stateServer(t, state, podClaims(pauseContract))
	recordViews := func(contract int) []map[string]any {
		return claimViews(t, append(append([]byte(`{"issues":{"`+recordGoldenIssue+`":`),
			recordOf(t, handler, "192.0.2.1:41000", recordGoldenIssue, contract)...), "}}"...))
	}

	for _, tc := range []struct {
		name  string
		views []map[string]any
		times bool
	}{
		{"the state stating the contract below", claimViews(t, stateStating(t, handler, "192.0.2.1:41000", causeTimesContract-1)), false},
		{"the record stating the contract below", recordViews(causeTimesContract - 1), false},
		{"the state asked from a pod at pauseContract", claimViews(t, stateAsked(t, handler, statePodAddr+":41000")), false},
		{"the state stating causeTimesContract", claimViews(t, stateStating(t, handler, "192.0.2.1:41000", causeTimesContract)), true},
		{"the record stating causeTimesContract", recordViews(causeTimesContract), true},
		{"the state asked from no pod", claimViews(t, stateAsked(t, handler, "192.0.2.1:41000")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dated, workers := 0, 0
			for _, view := range tc.views {
				if view["lastDeathCause"] == implementer.Claim.LastDeathCause {
					workers++
					if tc.times {
						wantTimeKey(t, "the implementer's view", view, "lastDeathAt", workerDied)
						continue
					}
				}
				if view["lastDeathCause"] == architect.LastDeathCause {
					dated++
					for _, key := range causeKeys {
						if _, carried := view[key]; !carried {
							t.Errorf("the architect's view carries no %s, which its own contract serves: %v", key, view)
						}
					}
					if tc.times {
						wantTimeKey(t, "the architect's view", view, "lastDeathAt", *architect.LastDeathAt)
						wantTimeKey(t, "the architect's view", view, "lastStartFailureAt", *architect.LastStartFailureAt)
						continue
					}
				}
				if !tc.times {
					wantNoKeys(t, "a claim view served below causeTimesContract", view, timeKeys...)
				}
			}
			if dated != 1 || workers != 1 {
				t.Fatalf("%d claim views carry the architect's death cause %q and %d the implementer's %q, want one each: %v",
					dated, architect.LastDeathCause, workers, implementer.Claim.LastDeathCause, tc.views)
			}
		})
	}

	// A caller below causeTimesContract is below dispatchCredentialContract too, the one frozen row
	// above it, and below the band of each entry, above every frozen row, so it is told of each of
	// those rows in the table's order: the cause times' first, then the Dispatch credential's, then
	// each entry's band's (narrowingsExpected). It reads carriedState, so an entry's band finds its
	// key to take, and it holds no entry to be the newest (R4 of dispatch://INFRA-1569).
	carried := carriedState(t)
	_, narrowings := stateNarrowedStating(t, carried, statePodAddr+":41000", podClaims(0), causeTimesContract-1)
	const dropped = "the claim views' lastDeathAt and lastStartFailureAt"
	want := narrowingsExpected(carried, causeTimesContract-1)
	if len(want) < 2 || want[0] != dropped || want[1] != "dispatchCredential" {
		t.Fatalf("a caller at %d expects the lines %v, want one that dropped %q and then one that dropped %q first",
			causeTimesContract-1, want, dropped, "dispatchCredential")
	}
	wantNarrowings(t, narrowings, causeTimesContract-1, want)
}
