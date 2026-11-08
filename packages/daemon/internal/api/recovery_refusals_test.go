package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The windows both of the controller's claim recoveries are refused in, and the fixtures that
// build them. A claim stored working under a parked relaunch, one left over a process the
// runtime's last report found gone, and one a suspension is held over each read the same to
// the compaction and to the fresh session, and each is answered by one sentence both of them
// give (api's refuseWorkingClaim, refuseHeldSuspension). The tests here hold both routes to
// that one answer; each route's own refusals are in its own file (compact_test.go,
// freshsession_test.go).

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

// refusingCheckLaunchStore is the supervise store with the lifecycle recheck every launch
// makes before it starts anything (supervise.Store.CheckLaunch) failing while refusing is set,
// with a failure that is not the tree's — the store did not answer. That is the refusal which
// leaves a live claim exactly as it stands, the process it records kept and nothing of it
// written (supervise's relaunchRefused). It is set after the claim it refuses is running,
// since the launches that bring the claim up make the same check.
type refusingCheckLaunchStore struct {
	supervise.Store
	refusing atomic.Bool
}

func (s *refusingCheckLaunchStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	if s.refusing.Load() {
		return fmt.Errorf("check the launch of %s: the store did not answer", c.Token)
	}
	return s.Store.CheckLaunch(ctx, c)
}

// refusedRelaunchCause is one of the three runtime verdicts that raise a relaunch over a
// process the claim still records (parkedRelaunchCauses), with what the claim reports of that
// process once the relaunch has been refused: the two that find it gone leave a claim stored
// working over a process nothing is replacing, and the stale address leaves a process that is
// running and takes a turn of its own under the claim. Both doors read that one fact, and it
// is the whole of what tells the two apart (supervise.Claim.ProcessGone).
type refusedRelaunchCause struct {
	name        string
	observation runtime.Observation
	processGone bool
}

func refusedRelaunchCauses() []refusedRelaunchCause {
	return []refusedRelaunchCause{
		{"a death", runtime.Observation{Kind: runtime.Gone}, true},
		{"a displacement", runtime.Observation{Kind: runtime.Gone, Displaced: true}, true},
		{"a repoint over a live process", runtime.Observation{Kind: runtime.StaleAddress}, false},
	}
}

// processGoneRefusal is the sentence both recoveries give a claim left stored working over a
// process the runtime's last report of it found gone, with the asking operation's own name in
// it (api's refuseProcessGone, controllerOp).
func processGoneRefusal(token claim.Token, operation string) string {
	return string(token) + " is stored working over a process the daemon's last report of it found gone: the " +
		"relaunch that replaces that process has not run, so no turn of " + string(token) + " is running and " +
		"waiting for one to end waits for nothing. The daemon reads that process again at its runtime sweep, " +
		"which it makes by itself, and raises the relaunch again — or fails " + string(token) + ", where that " +
		"report is the death its budget runs out on. Ask again once " + string(token) + " is running, " +
		"which is when " + operation + " reaches it"
}

// workingAfterTheRelaunchWasRefused is the window the refusals below are asked in: a worker
// idle on the provider-error ladder, taken for a compaction, whose process the runtime then
// gave its verdict on, and whose relaunch the store would not answer the lifecycle recheck of
// (refusingCheckLaunchStore). That relaunch started nothing and wrote nothing, so the claim is
// left over the process it records with no launch coming for it (supervise's relaunchRefused).
//
// It is the window a parked relaunch refused at its turn leaves, reached without the pacing:
// one refusal serves every launch there is, the park's turn included, and it leaves the same
// claim either way (supervise's launchAfter and runWake, which share relaunchRefused). The
// parked turn itself is driven where the clock can be advanced (supervise's
// TestAParkedRelaunchRefusedAtItsTurnRecordsThatItsProcessIsGone); this harness's clock fires
// no timer at all (stillClock).
//
// The two verdicts that find the process gone drop the compaction with it, so the request
// answers that loss and the claim is left stored working over the dead process (supervise's
// compactionLostProcess). The stale address replaces a process that is still running: the
// compaction is the claim's own to the end and gives it back idle, and what leaves that claim
// working is the turn its live process then takes — which is a turn that is running and will
// end, and the claim both doors must go on answering "in a turn".
func workingAfterTheRelaunchWasRefused(t *testing.T, h *harness, cause refusedRelaunchCause) claim.Token {
	t.Helper()
	store := &refusingCheckLaunchStore{Store: h.supervisor.deps.Store}
	h.supervisor.deps.Store = store
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	store.refusing.Store(true)
	m, _ := h.supervisor.Machine(token)
	locator := m.Claim().Locator
	if locator == nil {
		t.Fatalf("%s records no process for the runtime to answer for", token)
	}
	observation := cause.observation
	observation.Locator = *locator
	// The refusal goes back to whoever raised the relaunch, which here is the verdict itself.
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: observation}); err == nil {
		t.Fatalf("the %s of %s's process was taken, want the lifecycle recheck's refusal of the relaunch it raised",
			observation.Kind, token)
	}
	release()

	recorder := <-answered
	settled(h, token)
	if cause.processGone {
		if recorder.Code != http.StatusConflict {
			t.Fatalf("the compaction whose claim lost its process = %d, want the 409 of a claim not given back; body %s",
				recorder.Code, recorder.Body)
		}
	} else {
		if recorder.Code != http.StatusOK {
			t.Fatalf("the compaction over the process the repoint replaces = %d, want the 200 of a claim given back; body %s",
				recorder.Code, recorder.Body)
		}
		if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("the turn %s's live process takes after the refusal: %v", token, err)
		}
	}
	view := m.View()
	if view.State != supervise.StateWorking || view.Compacting || view.SuspensionHeld || view.WakeWaiting != nil {
		t.Fatalf("the claim is %s reporting Compacting %v, a suspension held %v and wake %+v; want the working claim the refusal left with nothing waiting over it",
			view.State, view.Compacting, view.SuspensionHeld, view.WakeWaiting)
	}
	if view.ProcessGone != cause.processGone {
		t.Fatalf("the claim reports ProcessGone %v after %s, want %v", view.ProcessGone, observation.Kind, cause.processGone)
	}
	if got := view.Locator; got == nil || *got != *locator {
		t.Fatalf("the claim records %+v, want the process the refused relaunch kept, %+v", got, *locator)
	}
	return token
}

// A claim the lifecycle recheck's refusal left stored working over a process the runtime found
// gone is in no turn: the relaunch that was to replace that process never ran, nothing waits
// over the claim, and nothing will end a turn it is not in. Both recoveries say so, and name
// what does come for it — the relaunch the next report of that process raises. Answered "in a
// turn", a controller would ask again after an end that never comes, in the one window these
// recoveries exist for; at e9e7ae6f that is the answer every door gave, since nothing of such
// a claim told it from one in a turn.
//
// The claim whose live process the same refusal left is the control, and it keeps that answer:
// its turn is running and its end will come, so "in a turn" is what the doors must go on
// saying of it. The verdict the refused relaunch was raised by is the whole of what separates
// the two (supervise.Claim.ProcessGone).
func TestARecoveryRefusedAfterARefusedRelaunchNamesTheRelaunchRatherThanATurn(t *testing.T) {
	for _, cause := range refusedRelaunchCauses() {
		for _, door := range compactDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingAfterTheRelaunchWasRefused(t, h, cause)
				want := processGoneRefusal(token, "a compaction")
				if !cause.processGone {
					want = string(token) + " is in a turn: a compaction would abort that turn, so ask again after the turn ends"
				}

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn, want)
			})
		}
		for _, door := range freshSessionDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingAfterTheRelaunchWasRefused(t, h, cause)
				want := processGoneRefusal(token, "a fresh session")
				if !cause.processGone {
					want = string(token) + " is in a turn: a fresh session would cut that turn off, so ask again after the turn ends"
				}

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn, want)
				if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
					t.Errorf("the refused claim is %s recording session %q, want the working claim the refusal left on the session it had",
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
// the fresh session's refusal is its own door's (api's freshSession), and the compaction's is
// the one the machine answers in that state, which the same door then writes (supervise's
// compactStart, api's refuseState).
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
