package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// compactHarness is the one daemon both doors are driven against, as freshSessionHarness is: the
// operator's bearer and a controller grant on the same server. backend is the deployment's
// `worker_backends` as the routes see it (Options.Backend); nil runs every role on Oh My Pi.
func compactHarness(t *testing.T, backend func(claim.Role) runtime.Backend) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{&phaseRestarter{}}, RoleFor: workflow.RoleFor, Backend: backend,
	}).Handler
	return h
}

// compactDoor is one of the two ways a compaction is asked for: `legion claims compact`, which
// names the claim by its token and carries the operator's bearer, and the controller's compact,
// which names it by its issue and role and carries a controller grant. One effect stands behind
// both (api's compact), so every case runs through both.
type compactDoor struct {
	name string
	ask  func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder
}

func compactDoors() []compactDoor {
	return []compactDoor{
		{"the operator's claims compact", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(freshSessionToken(t, issue, role))+"/compact", nil)
		}},
		{"the controller's compact", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			grant := controllerGrant(t, h)
			return h.request(http.MethodPost, "/legion/v1/claims/compact",
				ClaimRequest{GrantID: grant.GrantID, Issue: issue, Role: string(role)}, nil)
		}},
	}
}

// connected gives the claim a connection to Oh My Pi, which a compaction and a send both need;
// the harness registers none of its own (operator_test.go's forget-session case).
func connected(h *harness, token claim.Token) *fake.Conn {
	conn := fake.NewConn()
	h.conns.Register(token, conn)
	return conn
}

// settled waits for the send the compaction's end started, so what the connection was asked is
// all of what it was asked.
func settled(h *harness, token claim.Token) {
	m, _ := h.supervisor.Machine(token)
	m.Wait()
}

// decodeCompaction reads a 200 answer as the compaction it reports.
func decodeCompaction(t *testing.T, recorder *httptest.ResponseRecorder) CompactResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("the compaction = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer CompactResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// A claim idle on the provider-error ladder keeps its session, has it compacted, and is sent the
// task it owes again — once, and only after the compaction, rather than at the end of a ladder
// step counted against the transcript the compaction just shortened. The run of provider-error
// turns it carried goes back to zero, so the project's controller can be told a second time.
func TestACompactionKeepsTheSessionAndSendsTheOwedTaskAgain(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := idleOnTheLadder(t, h)
			conn := connected(h, token)
			before := h.stored(token)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if answer := decodeCompaction(t, recorder); !answer.Compacted || answer.Reason != "" {
				t.Fatalf("the compaction answered %+v, want it compacted with no reason", answer)
			}
			settled(h, token)
			after := h.stored(token)
			if after.Session != before.Session || after.SessionFile != before.SessionFile {
				t.Errorf("the compaction left session %q (%q), want the session it had, %q (%q)",
					after.Session, after.SessionFile, before.Session, before.SessionFile)
			}
			if after.ProviderErrorRun != 0 {
				t.Errorf("the claim's run of provider errors is %d, want 0: the compacted session has had no turn refused", after.ProviderErrorRun)
			}
			if after.State != supervise.StateIdle {
				t.Errorf("the compacted claim is %s, want idle again", after.State)
			}
			if got := len(conn.Compactions()); got != 1 {
				t.Fatalf("Oh My Pi was asked for %d compactions, want one", got)
			}
			prompts := conn.Prompts()
			if len(prompts) != 1 || prompts[0].DeliveryID != before.Pending.ID {
				t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, before.Pending.ID)
			}
		})
	}
}

// The compaction carries the instructions that make Oh My Pi's summary text: without them it
// archives the history onto snapcompact's images, which a worker resumed on them needs a vision
// model to read back (shimwire.Compact). The bound it is asked within is the operation's own
// ten minutes, not the RPC timeout.
func TestACompactionAsksOhMyPiForATextSummary(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if answer := decodeCompaction(t, recorder); !answer.Compacted {
		t.Fatalf("the compaction answered %+v, want it compacted", answer)
	}
	asked := conn.Compactions()
	if len(asked) != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want one", len(asked))
	}
	if !strings.Contains(asked[0].CustomInstructions, "Write the summary as text.") {
		t.Errorf("the compaction's instructions are %q, want them to ask for a text summary", asked[0].CustomInstructions)
	}
	if asked[0].Bound != runtime.CompactTimeout {
		t.Errorf("the compaction was bounded at %s, want the operation's own %s", asked[0].Bound, runtime.CompactTimeout)
	}
}

// A claim that owes no task is sent none: a root architect between turns has its session
// compacted and nothing else.
func TestACompactionSendsNoTaskToAClaimThatOwesNone(t *testing.T) {
	h := compactHarness(t, nil)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	token := freshSessionToken(t, "LEGION-208", claim.RoleArchitect)
	conn := connected(h, token)
	h.ready(token, "ses_architect", 1)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if answer := decodeCompaction(t, recorder); !answer.Compacted {
		t.Fatalf("the compaction answered %+v, want it compacted", answer)
	}
	settled(h, token)
	if got := len(conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want one", got)
	}
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the root architect was sent %#v, want nothing: it owed no task", prompts)
	}
}

// A compaction Oh My Pi refuses or skips — a session too small, one already compacted — is
// reported to the caller in Oh My Pi's own words, the owed task goes again all the same, and the
// claim's run of provider-error turns stands: the session is as it was.
func TestACompactionThatIsRefusedOrSkippedTellsTheCallerAndStillSendsTheOwedTask(t *testing.T) {
	for _, reason := range []string{"Nothing to compact (session too small)", "Already compacted"} {
		for _, door := range compactDoors() {
			t.Run(reason+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := idleOnTheLadder(t, h)
				conn := connected(h, token)
				conn.RefuseCompact(reason)
				owed := h.stored(token).Pending.ID

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				answer := decodeCompaction(t, recorder)
				if answer.Compacted || answer.Reason != reason {
					t.Fatalf("the compaction answered %+v, want it not compacted because %q", answer, reason)
				}
				settled(h, token)
				after := h.stored(token)
				if after.ProviderErrorRun != 1 {
					t.Errorf("the claim's run of provider errors is %d, want the 1 it carried: nothing was compacted", after.ProviderErrorRun)
				}
				prompts := conn.Prompts()
				if len(prompts) != 1 || prompts[0].DeliveryID != owed {
					t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, owed)
				}
			})
		}
	}
}

// A compaction that takes its time answers its caller what Oh My Pi really said, however long
// that is, and the claim is busy meanwhile: a fresh session asked for during it is refused, as
// one asked for during a turn is, and the claim keeps its session.
func TestADelayedCompactionAnswersItsRealResultAndAFreshSessionDuringItIsRefused(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	refused := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", RetryRequest{FreshSession: true})

	wantFreshSessionRefusal(t, refused, http.StatusConflict, claimInATurn,
		string(token)+" is in a turn: a fresh session would cut that turn off, so ask again after the turn ends")
	if after := h.stored(token); after.Session != "ses_implementer" {
		t.Errorf("the refused claim records session %q, want the session it had", after.Session)
	}
	release()
	if answer := decodeCompaction(t, <-answered); !answer.Compacted {
		t.Fatalf("the delayed compaction answered %+v, want the real result, compacted", answer)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted, which is what
// makes it busy for every other request.
func waitForCompacting(t *testing.T, h *harness, token claim.Token) {
	t.Helper()
	m, _ := h.supervisor.Machine(token)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := m.View().Claim; c.Compacting && c.State == supervise.StateWorking {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never reported itself being compacted", token)
}

// A second compaction of a claim already being compacted is refused: the compaction in flight is
// what it waits for.
func TestASecondCompactionOfAClaimBeingCompactedIsRefused(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	second := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, second, http.StatusConflict, claimInATurn,
		string(token)+" is being compacted already: wait for that compaction to end")
	release()
	<-answered
}

// A claim in a turn is refused through either door, and the refusal names the compaction: a
// compaction aborts whatever turn its session is running, and that turn may be the one that
// ends the run of provider errors. A controller whose skill has it compact first must not read
// a refusal telling it to ask for a fresh session, which is the operation it is to try second
// (api's refuseInATurn).
func TestACompactionRefusedForAClaimInATurnNamesTheCompaction(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := readyImplementer(t, h)
			connected(h, token)
			m, _ := h.supervisor.Machine(token)
			if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("start the turn: %v", err)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
				string(token)+" is in a turn: a compaction would abort that turn, so ask again after the turn ends")
		})
	}
}

// parkedRelaunchCause is one of the three runtime verdicts that leave a claim with a relaunch
// the daemon's wake pacing parks over it: a process found gone, one the daemon's own
// replacement of its pod ended, and one still running at an address a process launched now is
// not handed (supervise's died, displaced, repoint). Every refusal of that window is asked for
// after each of them, because no door can tell them apart: the park writes nothing of the
// claim, and the process it holds over is kept until the launch's turn whether it is dead or
// alive (api's relaunchWaits).
type parkedRelaunchCause struct {
	name string
	// observation is the runtime's verdict as the daemon posts it, less the locator the window
	// below reads off the claim (supervise.RuntimeObservation, supervise's observeDisplaced).
	observation runtime.Observation
}

func parkedRelaunchCauses() []parkedRelaunchCause {
	return []parkedRelaunchCause{
		{"a death", runtime.Observation{Kind: runtime.Gone}},
		{"a displacement", runtime.Observation{Kind: runtime.Gone, Displaced: true}},
		{"a repoint over a live process", runtime.Observation{Kind: runtime.StaleAddress}},
	}
}

// workingUnderAParkedRelaunch is the window the refusals below are asked in: a worker idle on
// the provider-error ladder whose process the runtime gave its verdict on while the session was
// being compacted, with the relaunch that replaces it held by the daemon's wake pacing. What is
// left is a claim stored working, not being compacted, with a launch waiting its turn: that
// turn is what the claim waits for, and it comes by itself. Nothing is held over the claim,
// which is the window the fixture below adds (suspensionHeldOverTheParkedRelaunch).
//
// observation is the verdict, less the locator this reads off the claim
// (parkedRelaunchCauses). The two that lose the process drop the compaction in flight with it,
// so the request answers that loss and the claim is left stored working over the process it
// records (supervise's compactionLostProcess,
// TestACompactionWhoseRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn). The stale address
// replaces a process that is still running, so the compaction is the claim's own to the end and
// gives it back idle (supervise's
// TestACompactionUnderAParkedRepointGivesTheClaimBackAndLeavesTheTaskToThatTurn); what leaves
// that claim working under the park is the turn its live process takes of its own account,
// which is the half of this window no door can tell from the other (supervise's
// TestATurnTheReplacedProcessConfirmedIsTakenBackAtTheLaunchsTurn).
//
// The pacing is installed before the claim is created, because a machine takes its deps where
// it is built, and it holds nothing until the verdict below: the launches that make the claim
// ready run as they are raised. The turn it then gives is an hour away and this harness's clock
// fires no timer at all (stillClock), so the wake stays parked for the rest of the test.
func workingUnderAParkedRelaunch(t *testing.T, h *harness, observation runtime.Observation) claim.Token {
	t.Helper()
	var park atomic.Bool
	h.supervisor.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time {
		if park.Load() {
			return now.Add(time.Hour)
		}
		return now
	}}
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	park.Store(true)
	m, _ := h.supervisor.Machine(token)
	locator := m.Claim().Locator
	if locator == nil {
		t.Fatalf("%s records no process for the runtime to answer for", token)
	}
	observation.Locator = *locator
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: observation}); err != nil {
		t.Fatalf("post %s of %s's process: %v", observation.Kind, token, err)
	}
	release()

	recorder := <-answered
	settled(h, token)
	if observation.Kind == runtime.StaleAddress {
		if recorder.Code != http.StatusOK {
			t.Fatalf("the compaction over the process the repoint replaces = %d, want the 200 of a claim given back; body %s",
				recorder.Code, recorder.Body)
		}
		if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("the turn %s's live process takes under the park: %v", token, err)
		}
	} else if recorder.Code != http.StatusConflict {
		t.Fatalf("the compaction whose claim lost its process = %d, want the 409 of a claim not given back; body %s",
			recorder.Code, recorder.Body)
	}
	view := m.View()
	if view.State != supervise.StateWorking || view.Compacting {
		t.Fatalf("the claim is %s and reports Compacting %v, want the working claim the park kept over the process it records",
			view.State, view.Compacting)
	}
	if view.WakeWaiting == nil || view.WakeWaiting.Kind != supervise.WakeLaunch {
		t.Fatalf("the claim's waiting wake is %+v, want the relaunch the pacing holds", view.WakeWaiting)
	}
	if view.SuspensionHeld {
		t.Fatalf("the claim reports a suspension held by %s, want none: nothing was asked of it while it waited",
			view.SuspensionHeldBy)
	}
	return token
}

// A claim whose relaunch waits its paced turn is a claim between states with a launch in
// flight, and the compaction says so after each of the three verdicts that leave it there: the
// refusal names the relaunch's turn, which comes by itself, rather than a turn's end. Answered
// "in a turn", a controller would ask again after a turn nothing will end, in the one window
// the compaction exists to recover from.
func TestACompactionRefusedWhileTheRelaunchWaitsItsTurnSaysTheClaimIsUnsettled(t *testing.T) {
	for _, cause := range parkedRelaunchCauses() {
		for _, door := range compactDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingUnderAParkedRelaunch(t, h, cause.observation)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
					string(token)+" has a relaunch in flight: the daemon is starting its process again, "+
						"and that relaunch waits its turn in the wake pacing. The turn comes by itself; ask again once "+
						string(token)+" is running, which is when a compaction reaches it")
			})
		}
	}
}

// The other door answers the same window in the same code and names itself: a fresh session of
// such a claim is refused here too, so a caller sent to the other recovery would be sent in a
// circle. What it is told to wait for is the relaunch's turn, as the compaction's refusal says.
// The claim is left exactly as the park left it, its session included.
func TestAFreshSessionRefusedWhileTheRelaunchWaitsItsTurnSaysTheClaimIsUnsettled(t *testing.T) {
	for _, cause := range parkedRelaunchCauses() {
		for _, door := range freshSessionDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingUnderAParkedRelaunch(t, h, cause.observation)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
					string(token)+" has a relaunch in flight: the daemon is starting its process again, "+
						"and that relaunch waits its turn in the wake pacing. The turn comes by itself; ask again once "+
						string(token)+" is running, which is when a fresh session reaches it")
				if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
					t.Errorf("the refused claim is %s recording session %q, want the working claim the park left on the session it had",
						after.State, after.Session)
				}
			})
		}
	}
}

// suspensionHeldOverTheParkedRelaunch is that window with an operator's suspension held over
// it: the suspend arrives while the claim is stored working, so the machine holds it for the
// turn's end rather than stopping anything, and the park keeps its launch. The launch's turn
// ends the held suspension instead of launching (supervise's wakeNow, endHeld), so this claim
// comes out suspended rather than running.
func suspensionHeldOverTheParkedRelaunch(t *testing.T, h *harness, observation runtime.Observation) claim.Token {
	t.Helper()
	token := workingUnderAParkedRelaunch(t, h, observation)
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the parked claim = %d, want the 202 of one held for the turn's end; body %s",
			recorder.Code, recorder.Body)
	}
	m, _ := h.supervisor.Machine(token)
	view := m.View()
	if view.State != supervise.StateWorking || !view.SuspensionHeld {
		t.Fatalf("the claim is %s holding %v, want the working claim with the operator's suspension held",
			view.State, view.SuspensionHeld)
	}
	if view.WakeWaiting == nil || view.WakeWaiting.Kind != supervise.WakeLaunch {
		t.Fatalf("the claim's waiting wake is %+v, want the relaunch the pacing still holds", view.WakeWaiting)
	}
	return token
}

// suspensionHeldOverATurn is the commonest held suspension and the one nothing is parked over:
// an ordinary worker in a turn, which the operator suspends, so the machine holds the stop for
// the turn's end rather than cutting the turn off (supervise's holdSuspension). That end lands
// the suspension, as the operator's stop deadline does when the turn outlasts it, and the claim
// it leaves is suspended — the landing the parked window has, brought by a turn's end rather
// than by a launch's turn. It is the operator's hold that lands so; the daemon's own is the
// row below (suspensionHeldOverATurnByTheDaemon).
func suspensionHeldOverATurn(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start %s's turn: %v", token, err)
	}
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the claim in a turn = %d, want the 202 of one held for the turn's end; body %s",
			recorder.Code, recorder.Body)
	}
	view := m.View()
	if view.State != supervise.StateWorking || !view.SuspensionHeld {
		t.Fatalf("the claim is %s holding %v, want the working claim with the operator's suspension held",
			view.State, view.SuspensionHeld)
	}
	if view.WakeWaiting != nil {
		t.Fatalf("the pacing holds %+v over the claim, want nothing held: this window is the suspension on its own",
			*view.WakeWaiting)
	}
	return token
}

// suspensionHeldOverATurnByTheDaemon is the same turn under the hold whose end is not a
// suspension: the daemon's own suspend — its issue left the workflow, or its tree lingers —
// reaches the claim in a turn, and the machine holds it exactly as it holds the operator's
// (supervise's holdSuspension). What follows differs. A start run against the claim drops a
// hold of the daemon's own rather than waiting it out, so the turn ends as any other turn does
// and leaves the claim idle, with nothing held and no Suspend asked of the runtime (supervise's
// StartedBy, dropHeld, TestAStartRunAgainstTheClaimDropsTheSuspensionWaitingForItsTurn), which
// is what TestAStartDropsTheDaemonsHoldAndTheIdleClaimItLeavesIsCompacted drives. A claim told
// to ask again once it is suspended would wait for a state that end never brings, so the
// refusal names what every hold's end shares instead (api's refuseHeldSuspension).
func suspensionHeldOverATurnByTheDaemon(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start %s's turn: %v", token, err)
	}
	err := m.Handle(h.ctx, supervise.RequestSuspend{
		Claim: token, Reason: "the daemon's suspension reached the claim in a turn", By: supervise.SuspendedByDaemon})
	if !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("the daemon's suspend of the claim in a turn = %v, want the ErrSuspendHeld of one held", err)
	}
	view := m.View()
	if view.State != supervise.StateWorking || !view.SuspensionHeld || view.SuspensionHeldBy != supervise.SuspendedByDaemon {
		t.Fatalf("the claim is %s holding %v by %q, want the working claim with the daemon's suspension held",
			view.State, view.SuspensionHeld, view.SuspensionHeldBy)
	}
	return token
}

// suspensionHeldOverACompaction is the third thing a held claim can be stored working under:
// its session is being compacted, and the operator's suspend arrives while it is, so the
// machine holds the stop as it holds one that arrives in a turn (supervise's holdSuspension).
// The compaction's end runs that stop rather than giving the claim back (supervise's
// compactEnded), so this claim comes out suspended as well, and the hold is read ahead of
// "being compacted already": a caller told to wait for the compaction in flight would ask again
// for a claim the compaction then refuses.
func suspensionHeldOverACompaction(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)
	// The compaction in flight is let go once the refusal has been read, so no request of this
	// fixture's is left outstanding; its end is what lands the suspension held here.
	t.Cleanup(func() {
		release()
		<-answered
	})
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the claim being compacted = %d, want the 202 of one held for the compaction's end; body %s",
			recorder.Code, recorder.Body)
	}
	m, _ := h.supervisor.Machine(token)
	view := m.View()
	if view.State != supervise.StateWorking || !view.SuspensionHeld || !view.Compacting {
		t.Fatalf("the claim is %s holding %v, compacting %v; want the claim still being compacted with the operator's suspension held",
			view.State, view.SuspensionHeld, view.Compacting)
	}
	return token
}

// suspensionHeldOverALaunchThatHasNotRegistered is the hold that is live before the agent is:
// the operator's suspend reaches a claim whose launch is in flight, and the machine holds it
// for the agent's ready rather than stopping a process that is still coming up (supervise's
// holdSuspendUntilReady). Such a claim is launching rather than working, and the hold lands at
// its ready before the claim is ever published ready (supervise's ready), so a caller told to
// ask again when the claim is ready or idle would wait for a state the hold itself keeps it
// from.
func suspensionHeldOverALaunchThatHasNotRegistered(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the launching claim = %d, want the 202 of one held for the agent's ready; body %s",
			recorder.Code, recorder.Body)
	}
	m, _ := h.supervisor.Machine(token)
	view := m.View()
	if view.State != supervise.StateLaunching || !view.SuspensionHeld {
		t.Fatalf("the claim is %s holding %v, want the launching claim with the operator's suspension held",
			view.State, view.SuspensionHeld)
	}
	return token
}

// heldSuspensionCase is one claim a suspension is held over, named by what the claim was doing
// when the hold arrived and by who asked for the hold. Every door answers all of them with one
// sentence, which is what these rows are for: neither door reads what the hold's end will
// bring, and the ends differ — the operator's hold lands as a suspension at every one of them,
// and a start the daemon runs against the claim drops the daemon's own and leaves the claim
// running (api's refuseHeldSuspension). A refusal true of one of those rows alone sends the
// caller back for a state the others never reach.
//
// code is the refusal's own, which follows the claim rather than the hold: a claim stored
// working takes the turn's, and one still coming up under a hold held for its launch takes the
// code of a claim between states, which is the code that claim was refused with before either
// door read the hold (api's refuseHeldSuspension).
type heldSuspensionCase struct {
	name  string
	code  string
	start func(t *testing.T, h *harness) claim.Token
}

func heldSuspensionCases() []heldSuspensionCase {
	cases := []heldSuspensionCase{
		{"a turn of the claim's own", claimInATurn, suspensionHeldOverATurn},
		{"a turn under the daemon's own hold", claimInATurn, suspensionHeldOverATurnByTheDaemon},
		{"a compaction in flight", claimInATurn, suspensionHeldOverACompaction},
		{"a launch that has not registered", claimUnsettled, suspensionHeldOverALaunchThatHasNotRegistered},
	}
	for _, cause := range parkedRelaunchCauses() {
		cases = append(cases, heldSuspensionCase{
			name: "a relaunch parked by " + cause.name,
			code: claimInATurn,
			start: func(t *testing.T, h *harness) claim.Token {
				return suspensionHeldOverTheParkedRelaunch(t, h, cause.observation)
			},
		})
	}
	return cases
}

// heldSuspensionRefusal is the sentence both recoveries give a claim a suspension is held over,
// with the asking operation's own name in it (api's refuseHeldSuspension, controllerOp).
func heldSuspensionRefusal(token claim.Token, operation string) string {
	return string(token) + " has a suspension held over it, and " + operation + " reaches no claim a " +
		"suspension waits over. The hold ends by itself, and not always in a suspension: a start the daemon " +
		"runs against the claim drops a hold of the daemon's own, and the end of the claim's process takes " +
		"any hold with it. Ask again once the hold has ended: fresh_session (`legion claims retry " +
		"--fresh-session`) reaches " + string(token) + " if it is suspended, stopped or failed by then, and " +
		operation + " if it is ready or idle"
}

// A suspension held over the claim is read ahead of every other window the claim can be in,
// because what such a claim waits for is the hold's end rather than a turn's, a compaction's
// or a launch's: neither "ask again once it is running" nor "ask again after the turn ends"
// names a wait that comes for it. What the refusal may say of that end is only that the hold
// ends, since the end itself differs — an operator's hold lands as a suspension at a turn's
// end, at a compaction's, at a parked relaunch's turn, at the agent's ready and at the
// operator's deadline, and a start the daemon runs against the claim drops the daemon's own
// and leaves it running. So the refusal names both operations by the state each reaches, and
// both recoveries say so of every row (api's refuseHeldSuspension). The code each row takes
// is the claim's own, so the answer a state was refused with before either door read the hold
// is the answer it is refused with now.
func TestARecoveryRefusedUnderAHeldSuspensionSaysTheClaimStopsWorking(t *testing.T) {
	for _, held := range heldSuspensionCases() {
		for _, door := range compactDoors() {
			t.Run(held.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := held.start(t, h)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, held.code,
					heldSuspensionRefusal(token, "a compaction"))
			})
		}
		for _, door := range freshSessionDoors() {
			t.Run(held.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := held.start(t, h)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, held.code,
					heldSuspensionRefusal(token, "a fresh session"))
				if m, _ := h.supervisor.Machine(token); !m.View().SuspensionHeld {
					t.Errorf("the refused claim holds no suspension, want the one the refusal left held")
				}
			})
		}
	}
}

// suspensionHeldOverAReadyAgent is the one held claim the machine's own take reaches: the
// operator's suspend was held for the agent's ready (supervise's holdSuspendUntilReady), and
// the stop that ready ran was refused by the runtime, which leaves the claim ready with the
// hold intact and the stop retried at the probe interval (supervise's ready, suspendHeld). The
// claim is not working, so neither door's switch over the published view is reached for it:
// the compaction's refusal is the machine's (supervise's compactStart), and the fresh
// session's is its own door's (api's freshSession).
func suspensionHeldOverAReadyAgent(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the launching claim = %d, want the 202 of one held for the agent's ready; body %s",
			recorder.Code, recorder.Body)
	}
	h.runtime.FailSuspend(errors.New("the runtime refused the stop"))
	registered := h.registered(h.bootToken(token), "ses_implementer")
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret, Generation: 1,
	}, nil); recorder.Code == http.StatusNoContent {
		t.Fatal("the ready whose held stop the runtime refused answered 204, want the runtime's failure")
	}
	// The runtime answers again from here, so the hold is one that lands: its retry at the
	// probe interval stops the process, which is what every request below is asked over.
	h.runtime.FailSuspend(nil)
	m, _ := h.supervisor.Machine(token)
	view := m.View()
	if view.State != supervise.StateReady || !view.SuspensionHeld {
		t.Fatalf("the claim is %s holding %v, want the ready claim with the operator's suspension held",
			view.State, view.SuspensionHeld)
	}
	return token
}

// A compaction of a ready claim a suspension is held over is taken for nothing, and the answer
// is the machine's: the state takes a compaction, so no refusal of the state is made, and what
// stops this one is the hold (supervise's compactStart). Taken, the compaction would spend ten
// minutes of a model on a session the hold is about to stop, and its own end would hand the
// claim to that hold rather than back to its agent — answered 200 all the while, which tells
// an operator the worker is back and its owed task has gone.
func TestACompactionOfAReadyClaimUnderAHeldSuspensionIsNotTaken(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := suspensionHeldOverAReadyAgent(t, h)
			conn := connected(h, token)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body)
			}
			var failure Failure
			decodeInto(t, recorder, &failure)
			// The sentence is the machine's own, which Machine.Compact answers joined to
			// supervise.ErrCompactNotTaken, as it answers every take that did not happen.
			held := "a suspension is held over the claim, so nothing is asked of a session that hold may stop; " +
				"the hold ends by itself, and the claim takes a compaction again if it is ready or idle then"
			if failure.Code != compactionFailed || !strings.Contains(failure.Error, held) {
				t.Fatalf("the refusal is %+v, want %s carrying %q", failure, compactionFailed, held)
			}
			if asked := conn.Compactions(); len(asked) != 0 {
				t.Fatalf("Oh My Pi was asked %+v, want nothing: the claim was never taken", asked)
			}
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateReady || !view.SuspensionHeld {
				t.Errorf("the refused claim is %s holding %v, want the ready claim with its hold, as the request found it",
					view.State, view.SuspensionHeld)
			}
			if after := h.stored(token); after.Session != "ses_implementer" {
				t.Errorf("the refused claim records session %q, want the session it had", after.Session)
			}
		})
	}
}

// The other door answers the same claim in its own words rather than starting it: a fresh
// session suspends a ready claim as the operator and then resumes it, and the suspension it
// runs lets the held request go with the process (supervise's letGo), so the operator's own
// suspend — answered 202 and still waiting on its retry — would be dropped unrun and the
// worker started again in a new session.
func TestAFreshSessionOfAReadyClaimUnderAHeldSuspensionIsRefused(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := suspensionHeldOverAReadyAgent(t, h)
			launches := len(h.runtime.CallsOf("Resume")) + len(h.runtime.CallsOf("Spawn"))

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
				heldSuspensionRefusal(token, "a fresh session"))
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateReady || !view.SuspensionHeld {
				t.Errorf("the refused claim is %s holding %v, want the ready claim with the operator's own hold, still waiting on its retry",
					view.State, view.SuspensionHeld)
			}
			if after := h.stored(token); after.Session != "ses_implementer" {
				t.Errorf("the refused claim records session %q, want the session it had", after.Session)
			}
			if got := len(h.runtime.CallsOf("Resume")) + len(h.runtime.CallsOf("Spawn")); got != launches {
				t.Errorf("the runtime was asked for %d launches, want the %d made before the refusal", got, launches)
			}
		})
	}
}

// A suspension that arrives while the session is being compacted is held for the compaction's
// end, and that end runs it: the claim is suspended with its process stopped and the task it
// owes unsent, which only the operator's resume starts again. The compaction says so rather
// than answering 200 — the answer that promises the claim is back and whatever task it owed
// has gone again (CompactResponse) — so a controller reads a recovery that did not land and
// an operator is not told a stopped worker is running.
func TestACompactionASuspensionHeldOverItEndedSaysTheClaimWasNotGivenBack(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := idleOnTheLadder(t, h)
			conn := connected(h, token)
			owed := h.stored(token).Pending.ID
			release := conn.HoldCompact()
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- door.ask(t, h, "LEGION-209", claim.RoleImplementer) }()
			waitForCompacting(t, h, token)

			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
				t.Fatalf("the suspend of the claim being compacted = %d, want the 202 of one held for the compaction's end; body %s",
					recorder.Code, recorder.Body)
			}
			release()

			wantFreshSessionRefusal(t, <-answered, http.StatusConflict, compactionFailed,
				string(token)+"'s session was compacted, and a suspension held over the claim ended that "+
					"compaction rather than the claim coming back: it was not given back, and the task it owes "+
					"goes when its agent runs again")
			settled(h, token)
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByOperator {
				t.Errorf("the claim is %s suspended by %q, want it suspended by the operator whose hold ended the compaction",
					after.State, after.SuspendedBy)
			}
			if after.Pending == nil || after.Pending.ID != owed {
				t.Errorf("the claim holds %+v, want the task %s it still owes", after.Pending, owed)
			}
			if prompts := conn.Prompts(); len(prompts) != 0 {
				t.Errorf("the connection was sent %#v, want nothing: the claim was not given back", prompts)
			}
		})
	}
}

// The daemon's hold is the one that ends in no suspension at all: a newer workflow start drops
// it, the turn then ends as any other does, and what the caller is left with is an idle claim
// the compaction reaches. A caller told to ask again once the claim is suspended waits for that
// state for as long as the claim runs, in the one window the compaction exists to recover from,
// so the refusal tells it to ask again once the claim is no longer working.
func TestAStartDropsTheDaemonsHoldAndTheIdleClaimItLeavesIsCompacted(t *testing.T) {
	h := compactHarness(t, nil)
	token := suspensionHeldOverATurnByTheDaemon(t, h)
	conn := connected(h, token)
	m, _ := h.supervisor.Machine(token)

	refused := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, refused, http.StatusConflict, claimInATurn,
		heldSuspensionRefusal(token, "a compaction"))
	if err := m.StartedBy(h.ctx, 42); err != nil {
		t.Fatalf("the start run against the held claim: %v", err)
	}
	if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end %s's turn: %v", token, err)
	}
	if view := m.View(); view.State != supervise.StateIdle || view.SuspensionHeld {
		t.Fatalf("the turn's end left the claim %s holding %v, want it idle with the daemon's hold dropped",
			view.State, view.SuspensionHeld)
	}

	second := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if answer := decodeCompaction(t, second); !answer.Compacted {
		t.Fatalf("the compaction of the claim the turn left idle answered %+v, want it compacted", answer)
	}
	settled(h, token)
	if got := len(conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want the one the idle claim took", got)
	}
}

// A claim whose agent is not running has no session to compact, and the refusal names the
// operation that does reach it: a suspended claim, one an operator stopped, and a failed one.
func TestACompactionIsRefusedForASuspendedAStoppedAndAFailedClaimNamingFreshSession(t *testing.T) {
	for _, tc := range freshSessionCases() {
		if tc.name == "a phase worker idle on the provider-error ladder" || tc.name == "an idle root architect" {
			continue
		}
		for _, door := range compactDoors() {
			t.Run(tc.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := tc.start(t, h)
				connected(h, token)
				state := h.stored(token).State

				recorder := door.ask(t, h, tc.issue, tc.role)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimNotRunning,
					string(token)+" is "+string(state)+", so it runs no Oh My Pi session to compact; "+
						freshSessionName+" starts it again instead")
			})
		}
	}
}

// A claim whose launch has not registered yet has no agent to ask at all.
func TestACompactionIsRefusedForAClaimWhoseLaunchIsInFlight(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	if got := h.stored(token).State; got != supervise.StateLaunching {
		t.Fatalf("the launched worker is %s, want launching", got)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	assertFailure(t, recorder, http.StatusConflict, claimUnsettled)
}

// A role the deployment runs on Claude Code has no Oh My Pi session to compact, and the daemon
// knows which role runs on which harness from its own configuration (Options.Backend).
func TestACompactionIsRefusedForAClaudeCodeClaim(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, func(role claim.Role) runtime.Backend {
				if role == claim.RoleImplementer {
					return runtime.BackendClaude
				}
				return runtime.BackendOmp
			})
			token := readyImplementer(t, h)
			conn := connected(h, token)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimNotOnOmp,
				string(token)+" runs on Claude Code, which has no Oh My Pi session to compact; "+
					freshSessionName+" is what starts it again")
			if got := len(conn.Compactions()); got != 0 {
				t.Fatalf("the refused claim was asked for %d compactions, want none", got)
			}
		})
	}
}

// `controller` is no role the controller's own tool may name, as it is none for a fresh session:
// the roles are the six a claim can be on, and the project's controller is none of them.
func TestTheControllersCompactRefusesTheRoleController(t *testing.T) {
	h := compactHarness(t, nil)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: grant.GrantID, Issue: "LEGION-209", Role: "controller"}, nil)

	wantFreshSessionRefusal(t, recorder, http.StatusBadRequest, "INVALID_ROLE",
		"role must be one of architect, planner, implementer, tester, reviewer, merger; "+
			"the project's controller is none of them, and the controller does not compact its own session")
}

// The controller's door refuses a grant that is not the controller's, as every controller route
// does, and names a claim the daemon does not supervise with a 404 of its own.
func TestTheControllersCompactNeedsAControllerGrantAndAClaim(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	registered := h.registered(h.bootToken(token), "ses_implementer")
	var worker GrantResponse
	minted := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: "ses_implementer", Secret: registered.Secret, Tree: "LEGION-208", Issue: "LEGION-209",
	}, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("mint the worker's grant = %d; body %s", minted.Code, minted.Body)
	}
	decodeInto(t, minted, &worker)

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: worker.GrantID, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil),
		http.StatusForbidden, "CONTROLLER_REQUIRED")

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-777", Role: string(claim.RoleTester)}, nil),
		http.StatusNotFound, "NO_CLAIM")
}

// A compaction the daemon could not get an answer to — the wait ran out, or the transport failed
// — is reported as a compaction that did not happen, and the owed task goes again: the claim is
// never left working on a compaction nobody will answer for.
func TestACompactionWhoseAnswerNeverCameGivesTheClaimBack(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	conn.FailCompact(errors.New("worker-stream: compact: no answer came"))
	owed := h.stored(token).Pending.ID

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	answer := decodeCompaction(t, recorder)
	if answer.Compacted || !strings.Contains(answer.Reason, "no answer came") {
		t.Fatalf("the compaction answered %+v, want it not compacted, naming why", answer)
	}
	settled(h, token)
	if after := h.stored(token); after.State != supervise.StateIdle {
		t.Errorf("the claim is %s, want idle again", after.State)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != owed {
		t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, owed)
	}
}

// A claim the machine takes no compaction in is refused in the words of its own state, not as a
// claim in a turn: a claim whose agent registered and is not ready yet has no session to ask,
// and the machine is what says so.
func TestACompactionRefusedForAClaimThatIsNotReadyNamesWhatIsWrong(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	h.registered(h.bootToken(token), "ses_implementer")
	connected(h, token)
	if got := h.stored(token).State; got != supervise.StateRegistered {
		t.Fatalf("the launched worker is %s, want registered", got)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
		string(token)+" is registered: a compaction reaches a claim whose agent is ready or idle; ask again when "+
			string(token)+" is one of those")
}

// A ready claim whose shim is not connected is the one refusal the take writes itself: the
// daemon has nothing to ask through, so Oh My Pi was asked nothing and the claim was sent
// nothing. The caller is told the compaction failed rather than told a compaction that did not
// run, whose answer carries the claim's task with it.
func TestACompactionOfAClaimWhoseShimIsNotConnectedSaysNothingWasAsked(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != compactionFailed || !strings.Contains(failure.Error, "shim is not connected") {
		t.Fatalf("the refusal is %+v, want %s naming the shim that is not connected", failure, compactionFailed)
	}
	if after := h.stored(token); after.State != supervise.StateReady {
		t.Errorf("the refused claim is %s, want the ready it was", after.State)
	}
}

// A compaction whose end the store would not write is answered as the store failure it is. Oh My
// Pi compacted the session and the write that gives the claim back did not happen, so the claim
// is left working and the task it owes is unsent: answered 200 it would tell the caller a
// compaction that did not run, whose answer promises the claim is back and its task has gone
// again.
func TestACompactionWhoseEndTheStoreWouldNotWriteAnswersTheStoreFailure(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	// The store is gone from here on, so the write the compaction's end makes fails.
	h.store.Pool().Close()
	release()

	recorder := <-answered
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != compactionFailed {
		t.Fatalf("the failure is %+v, want %s: the compaction ended and the claim could not be given back", failure, compactionFailed)
	}
	if got := len(conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want the one that ran", got)
	}
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the connection was sent %#v, want nothing: the claim was never given back", prompts)
	}
}

// A claim that loses its process while its session is being compacted is sent nothing by the
// compaction's end: that end reaches a claim the relaunch has moved past, so it decides nothing.
// The answer says the claim was not given back and leaves the owed task to the moment its agent
// runs again, where a compaction reported as run would have the CLI say the task had gone again
// when nothing had sent it.
func TestACompactionOfAClaimThatLosesItsProcessSaysItWasNotGivenBack(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	owed := h.stored(token).Pending.ID
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	h.relaunch(token)
	release()

	wantFreshSessionRefusal(t, <-answered, http.StatusConflict, compactionFailed,
		string(token)+" lost its process while its session was being compacted; it was not given back, and the task it owes goes when its agent runs again")
	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the connection was sent %#v, want nothing: the compaction's end reached a claim its relaunch had moved past", prompts)
	}

	h.ready(token, "ses_implementer", 2)

	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != owed {
		t.Fatalf("the relaunched claim was sent %#v, want the one task it owed (%s)", prompts, owed)
	}
}

// The reason a compaction did not run is words the daemon did not write — Oh My Pi's, and
// through it a summarizer model's own explanation — and it reaches the controller's tool result
// and so its next prompt. The answer carries what the operation needs of it and no more.
func TestACompactionsReasonIsBoundedBeforeItReachesTheCaller(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	conn.RefuseCompact("Already compacted. " + strings.Repeat("and here is why at length. ", 200))

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	answer := decodeCompaction(t, recorder)
	if len(answer.Reason) > reasonBound+len("…") {
		t.Fatalf("the answer carried %d bytes of reason, want at most %d", len(answer.Reason), reasonBound+len("…"))
	}
	if !strings.HasPrefix(answer.Reason, "Already compacted.") {
		t.Fatalf("the answer's reason is %q, want it to start with what Oh My Pi said", answer.Reason)
	}
	if !strings.HasSuffix(answer.Reason, "…") {
		t.Fatalf("the answer's reason is %q, want the cut marked so a reader sees the sentence did not end", answer.Reason)
	}
}
