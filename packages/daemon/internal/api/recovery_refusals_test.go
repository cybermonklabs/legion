package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The windows both of the controller's claim recoveries are read in, and the fixtures that
// build them. A claim stored working under a parked relaunch and one a suspension is held over
// read the same to the compaction and to the fresh session, and each is answered by one
// sentence both of them give (api's refuseWorkingClaim, refuseInATurn, refuseHeldSuspension).
// A claim stored working that no process stands behind is where the two part: the fresh
// session stops it where the request arrives and starts it again, and the compaction, which
// has no agent to ask, names that door instead (deadProcessWindows). The tests here hold both
// routes to those answers; each route's own refusals are in its own file (compact_test.go,
// freshsession_test.go).
//
// What a door answers such a claim is read off its process at the moment of the request
// (processVerdict), never off the daemon's memory of an old report: the two windows below
// differ in that memory alone, and a door that read it would answer one of them a turn that is
// not running — which is the false word a controller with a dead architect was sent away with
// at e9e7ae6f.
//
// Every sentence says what the claim is doing when the refusal is given, which door reaches
// each state a caller can read of it afterwards, and nothing of what the next report, turn,
// hold or relaunch will bring (api's askAgainLater). The states are the sets the two
// operations act on, in the words `legion claims list` prints them (api's agentRunning,
// startsAgain, supervise's onCompact rows). The words below are spelled here rather
// than read off the routes, so a route that changes one of them fails a test. That a route's
// words are the states its predicates admit is a different question, and a copy like this one
// cannot answer it: the predicates decide it in reachstates_test.go.
const (
	wantCompactionReaches   = "A compaction reaches a claim whose agent is ready or idle"
	wantCompactionLive      = "ready or idle"
	wantFreshSessionReaches = "A fresh session reaches a claim whose agent is registered, ready or idle, one that is suspended, failed or retired, and one stored working that no process stands behind"
	wantFreshSessionLive    = "registered, ready or idle"
	wantAskAgainLater       = "Ask again later; `read_state` (`legion claims list`) shows its state"
)

// inATurnRefusal is the sentence both recoveries give a claim stored working that neither the
// parked relaunch nor the read of its process answers, with what the asking operation would do
// to a turn of that claim that is running and the states it reaches (api's refuseInATurn). It
// says the claim is stored working and no more, because that is the whole of what the record
// holds: the process is there, and whether a turn of it is running is the agent's own and not
// the daemon's to state.
func inATurnRefusal(token claim.Token, cutsTheTurn, reaches string) string {
	return string(token) + " is stored working: if a turn of it is running, " + cutsTheTurn + ". " +
		reaches + ". " + wantAskAgainLater
}

// parkedRelaunchRefusal is the sentence both recoveries give a claim stored working while a
// launch the daemon's wake pacing holds waits over it (api's refuseParkedRelaunch).
func parkedRelaunchRefusal(token claim.Token, reaches string) string {
	return string(token) + " has a relaunch in flight: the daemon is starting its process again, " +
		"and that relaunch waits its turn in the wake pacing. " + reaches + ". " + wantAskAgainLater
}

// unsettledRefusal is the sentence each recovery gives a claim between states that no window
// of its own answers: the claim's state, the states the operation reaches, and the list
// (api's refuseState, freshSession).
func unsettledRefusal(token claim.Token, state supervise.ClaimState, reaches string) string {
	return string(token) + " is " + string(state) + ". " + reaches + ". " + wantAskAgainLater
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
// refusal names that launch rather than a turn, and says nothing of what the launch's turn
// brings. Answered "in a turn", a controller would ask again after a turn nothing will end, in
// the one window the compaction exists to recover from.
func TestACompactionRefusedWhileTheRelaunchWaitsItsTurnSaysTheClaimIsUnsettled(t *testing.T) {
	for _, cause := range parkedRelaunchCauses() {
		for _, door := range compactDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingUnderAParkedRelaunch(t, h, cause.observation)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
					parkedRelaunchRefusal(token, wantCompactionReaches))
			})
		}
	}
}

// The other door answers the same window in the same code and names itself: a fresh session of
// such a claim is refused here too, so a caller sent to the other recovery would be sent in a
// circle. What it names is the launch in flight, as the compaction's refusal does.
// The claim is left exactly as the park left it, its session included.
func TestAFreshSessionRefusedWhileTheRelaunchWaitsItsTurnSaysTheClaimIsUnsettled(t *testing.T) {
	for _, cause := range parkedRelaunchCauses() {
		for _, door := range freshSessionDoors() {
			t.Run(cause.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := workingUnderAParkedRelaunch(t, h, cause.observation)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
					parkedRelaunchRefusal(token, wantFreshSessionReaches))
				if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
					t.Errorf("the refused claim is %s recording session %q, want the working claim the park left on the session it had",
						after.State, after.Session)
				}
			})
		}
	}
}

// workingUnderAParkedRefresh is the other wake a machine parks over a live process: the
// relaunch the stale-plugin refresh lane raises, which carries a kind of its own so that a
// reader of the daemon's state can tell a fleet's refresh from the work it holds up
// (supervise.WakePluginRefresh, dispatch://INFRA-2712 R4). Everything else about it is a
// launch, and the window it leaves is the one above: a claim stored working with a launch in
// flight over the process it still records.
//
// The claim is seeded stale as the API's own stale-caller fixture seeds one — written at an
// earlier contract and its machine rebuilt from the store (harness.makeStale) — but with the
// capability it registered with left as it is, since nothing here calls as the claim. The sweep
// then finds it, the pacing parks its relaunch over the process it still runs
// (supervise.refreshPlugin), and the live process takes a turn of its own, which is what leaves
// the claim stored working under the park (TestATurnTheReplacedProcessConfirmedIsTakenBackAt
// TheLaunchsTurn in supervise).
func workingUnderAParkedRefresh(t *testing.T, h *harness) claim.Token {
	t.Helper()
	var park atomic.Bool
	h.supervisor.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time {
		if park.Load() {
			return now.Add(time.Hour)
		}
		return now
	}}
	token := idleOnTheLadder(t, h)

	park.Store(true)
	m, _ := h.supervisor.Machine(token)
	stale := m.Claim()
	stale.PluginContract = DaemonAPIVersion - 1
	if err := h.store.PutClaim(h.ctx, stale); err != nil {
		t.Fatalf("seed %s at an earlier plugin contract: %v", token, err)
	}
	m = h.supervisor.restart(t, token)
	if err := m.Handle(h.ctx, supervise.StalePlugin{Claim: token, Trigger: "sweep"}); err != nil {
		t.Fatalf("the sweep's post of %s's stale plugin: %v", token, err)
	}
	m.Wait()
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("the turn %s's live process takes under the park: %v", token, err)
	}
	settled(h, token)

	view := m.View()
	if view.State != supervise.StateWorking {
		t.Fatalf("the claim is %s, want the working claim the park kept over the process it records", view.State)
	}
	if view.WakeWaiting == nil || view.WakeWaiting.Kind != supervise.WakePluginRefresh {
		t.Fatalf("the claim's waiting wake is %+v, want the refresh lane's relaunch the pacing holds", view.WakeWaiting)
	}
	return token
}

// Both recoveries answer that window exactly as they answer a parked launch: a relaunch is in
// flight, which is what the refresh's relaunch is. The kind tells a reader of the state body
// which lane holds the pacing and nothing else, so a door that read it as anything but a launch
// would answer "in a turn" and send a controller back to wait out a turn the launch is about to
// cut (api's relaunchWaits, supervise.StartsProcess).
func TestARecoveryRefusedWhileTheStalePluginRefreshWaitsItsTurnNamesTheLaunchInFlight(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run("the compaction: "+door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := workingUnderAParkedRefresh(t, h)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
				parkedRelaunchRefusal(token, wantCompactionReaches))
		})
	}
	for _, door := range freshSessionDoors() {
		t.Run("the fresh session: "+door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := workingUnderAParkedRefresh(t, h)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
				parkedRelaunchRefusal(token, wantFreshSessionReaches))
		})
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

// refusedRelaunchCause is the runtime verdict that raises a relaunch over a process the claim
// still records (parkedRelaunchCauses), with what the claim reports of that process once the
// relaunch has been refused: a verdict that found the process gone leaves a claim stored
// working over a process nothing is replacing, and the stale address leaves a process that is
// running and takes a turn of its own under the claim (supervise.Claim.ProcessGone). The two
// the windows below are built from are aDeath and aRepointOverALiveProcess.
type refusedRelaunchCause struct {
	observation runtime.Observation
	processGone bool
}

// aDeath is the verdict that leaves the claim stored working over a process that is not
// running: the runtime found the process gone. A displacement — the daemon's own replacement
// of the pod, which ends the process through no fault of its own — leaves the very same claim
// (supervise's died, displaced), and no door tells it from this one, because what a door reads
// of such a claim is the process itself rather than the verdict that left it there
// (deadProcessWindows).
func aDeath() refusedRelaunchCause {
	return refusedRelaunchCause{observation: runtime.Observation{Kind: runtime.Gone}, processGone: true}
}

// aRepointOverALiveProcess is the verdict that leaves a process still running under the claim:
// it holds an address a process launched now is not handed, and the refused relaunch keeps it
// (supervise's repoint). That process then takes a turn of its own, so the claim is working
// with a turn that is running — the claim both doors go on answering "in a turn".
func aRepointOverALiveProcess() refusedRelaunchCause {
	return refusedRelaunchCause{observation: runtime.Observation{Kind: runtime.StaleAddress}}
}

// notRunningRefusal is the sentence the compaction gives a claim stored working whose process
// the read at the request finds is not running: there is no Oh My Pi session to compact, since
// nothing runs one, and what starts such a claim again is the fresh session for every workflow
// role (api's startsAgainName, claimNotRunning). The compaction names that door rather than a
// wait, because nothing comes for this claim on its own: the relaunch that was to replace the
// process has not run, and the recovery the caller is sent to is the one that acts.
func notRunningRefusal(token claim.Token) string {
	return string(token) + " is stored working over a process that is no longer running, so it runs no " +
		"Oh My Pi session to compact; " + freshSessionName + " starts it again instead"
}

// unreadableProcessRefusal is the sentence both recoveries give a claim stored working whose
// process the runtime would not answer for: `Uncertain`, which is a verdict and never a
// statement that the process is gone, and a Probe that fails, which says nothing about the
// process at all (runtime's Probe, Uncertain). Neither door may act on a claim it cannot read
// — a fresh session that took an unread process for a dead one would cut off the turn of a
// process that is running — so the answer is the one every claim stored working takes, with
// the read that did not happen named in it, and the claim is left exactly as the request found
// it (api's claimInATurn).
func unreadableProcessRefusal(token claim.Token, reaches string) string {
	return string(token) + " is stored working and the daemon could not read whether its process is running: " +
		"the runtime did not answer. " + reaches + ". " + wantAskAgainLater
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

// launchesAnswerAgain puts the lifecycle recheck workingAfterTheRelaunchWasRefused left
// refusing back to answering (refusingCheckLaunchStore), so a start run over the window that
// fixture built is a start that can run. The store is the supervisor's own deps, which every
// machine built from them holds, so the claim a restart rebuilds takes it as well
// (restoredWorkingOverADeadProcess). A row that drives a recovery through to its start calls
// this after the fixture; a row about the start the recheck refuses, or about a step before the
// start, does not (TestAFreshSessionThatStoppedTheClaimAndCouldNotStartItSaysNotRestartedInItsAnswer).
func launchesAnswerAgain(t *testing.T, h *harness) {
	t.Helper()
	store, ok := h.supervisor.deps.Store.(*refusingCheckLaunchStore)
	if !ok {
		t.Fatalf("the supervisor's store is %T, want the %T the window installed over it", h.supervisor.deps.Store, store)
	}
	store.refusing.Store(false)
}

// restoredWorkingOverADeadProcess is the window a daemon restart leaves: the claim above,
// stored working over a process the runtime's last report found gone, read back from the store
// by a machine built again. That report is the old machine's memory and the store never held
// it, so the claim comes back working with nothing that tells it from a claim in a turn — and
// no turn is running, since the process that would run one is dead and the relaunch that was
// to replace it never ran (supervise.ClaimView.ProcessGone, supervise's NewMachine).
func restoredWorkingOverADeadProcess(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := workingAfterTheRelaunchWasRefused(t, h, aDeath())
	m := h.supervisor.restart(t, token)
	view := m.View()
	if view.State != supervise.StateWorking || view.ProcessGone || view.Compacting || view.SuspensionHeld || view.WakeWaiting != nil {
		t.Fatalf("the restored claim is %s reporting ProcessGone %v, compacting %v, a suspension held %v and wake %+v; want the working claim the restart rebuilds with no memory of the dead process",
			view.State, view.ProcessGone, view.Compacting, view.SuspensionHeld, view.WakeWaiting)
	}
	return token
}

// deadProcessWindow is one of the claims stored working that no process stands behind, by what
// left it there: a relaunch the lifecycle recheck refused, where the daemon still holds the
// report that found the process gone, and the same claim after a daemon restart, where it
// holds nothing of that report at all (workingAfterTheRelaunchWasRefused,
// restoredWorkingOverADeadProcess, supervise.ClaimView.ProcessGone). The record reads working
// in both and no process runs under either, so every row below is asked in both: an answer
// read off the daemon's memory of an old report differs between the two, and an answer read
// off the process itself is the same in both.
type deadProcessWindow struct {
	name  string
	build func(t *testing.T, h *harness) claim.Token
}

func deadProcessWindows() []deadProcessWindow {
	return []deadProcessWindow{
		{"a relaunch the store refused", func(t *testing.T, h *harness) claim.Token {
			t.Helper()
			return workingAfterTheRelaunchWasRefused(t, h, aDeath())
		}},
		{"a daemon restart over that claim", restoredWorkingOverADeadProcess},
	}
}

// processVerdict is one answer the runtime gives about the claim's process when a door reads
// that process at the moment of the request, in the words the row that stages it reads it by.
// Every row scripts its own (fake.Runtime.ScriptProbe), because an unscripted probe answers
// `Alive`: a row that scripted none would be a row about a live process whatever window it
// meant to stage.
type processVerdict struct {
	name   string
	result fake.ProbeResult
}

// processGoneVerdicts are the two answers that say the claim's process is not running: nothing
// is there at all, and something that is not the process the claim records is — a reissued pid,
// a pane id handed out again (runtime's Gone, NotRecordedProcess). The daemon reads the two the
// same (supervise's reportedProcess), and they are the window both recoveries exist for.
func processGoneVerdicts() []processVerdict {
	return []processVerdict{
		{"a process that is gone", fake.ProbeResult{Kind: runtime.Gone}},
		{"another process in its place", fake.ProbeResult{Kind: runtime.NotRecordedProcess}},
	}
}

// processRunningVerdicts are the two answers that say the process the claim records is running:
// alive, and alive while holding an address a process launched now is not handed, which is a
// live process with a turn of its own under the claim (runtime's Alive, StaleAddress). A door
// that acted on either would stop an agent that is working.
func processRunningVerdicts() []processVerdict {
	return []processVerdict{
		{"a process that is alive", fake.ProbeResult{Kind: runtime.Alive}},
		{"a live process at a stale address", fake.ProbeResult{Kind: runtime.StaleAddress}},
	}
}

// unreadableProcessVerdicts are the two ways the runtime says neither: `Uncertain`, which is a
// verdict of its own precisely so that it is never read as gone, and a Probe that fails, which
// is the runtime not being askable and no statement about the process (runtime's Probe,
// Uncertain).
func unreadableProcessVerdicts() []processVerdict {
	return []processVerdict{
		{"a verdict of uncertain", fake.ProbeResult{Kind: runtime.Uncertain}},
		{"a runtime that would not answer", fake.ProbeResult{Err: errors.New("the runtime did not answer")}},
	}
}

// processReads is how many times the runtime has been asked about a process (fake's Probe).
// Every row counts it across its own request, because the answers below turn on that one read:
// a row that made none was answered from the record, and a row that made two read one claim
// twice in one request.
func processReads(h *harness) int {
	return len(h.runtime.CallsOf("Probe"))
}

// recoveryDoor is one of the four doors the two recoveries have, with the words its own
// operation writes into the refusals both of its doors give (freshSessionDoors, compactDoors,
// controllerOp). The rows whose answer is the same at every door run through all four.
type recoveryDoor struct {
	name        string
	ask         func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder
	cutsTheTurn string
	reaches     string
}

func recoveryDoors() []recoveryDoor {
	doors := make([]recoveryDoor, 0, 4)
	for _, door := range freshSessionDoors() {
		doors = append(doors, recoveryDoor{door.name, door.ask, "a fresh session would cut that turn off", wantFreshSessionReaches})
	}
	for _, door := range compactDoors() {
		doors = append(doors, recoveryDoor{door.name, door.ask, "a compaction would abort that turn", wantCompactionReaches})
	}
	return doors
}

// A claim stored working that no process stands behind is the one window the fresh session
// exists for, and it starts the claim again: the process the claim records is read at the
// request and found gone, so there is no turn to cut off and no agent to stop, and the
// suspension this route runs as the operator lands where the request arrives. The claim then
// leaves its session behind and is started again, which is the 200 a project controller has
// for a dead architect — the one recovery it holds for a role nothing else will bring back.
//
// Both windows answer the same because the read is of the process rather than of the daemon's
// memory: the claim a restart rebuilt holds no report at all, and at e9e7ae6f that is what
// made it unreachable, since the record alone reads the same as a claim in a turn.
func TestAFreshSessionReachesAClaimOverADeadProcess(t *testing.T) {
	for _, window := range deadProcessWindows() {
		for _, verdict := range processGoneVerdicts() {
			for _, door := range freshSessionDoors() {
				t.Run(window.name+": "+verdict.name+": "+door.name, func(t *testing.T) {
					h := compactHarness(t, nil)
					token := window.build(t, h)
					launchesAnswerAgain(t, h)
					h.runtime.ScriptProbe(verdict.result)
					reads := processReads(h)

					recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

					if recorder.Code != http.StatusOK {
						t.Fatalf("the fresh session over the dead process = %d, want the 200 of a claim started again; body %s",
							recorder.Code, recorder.Body)
					}
					if got := processReads(h) - reads; got != 1 {
						t.Errorf("the request read the claim's process %d times, want the one read its answer turns on", got)
					}
					after := h.stored(token)
					if after.State == supervise.StateWorking {
						t.Errorf("the claim is %s, want it out of the working the dead process left it stored in", after.State)
					}
					if after.Session == "ses_implementer" {
						t.Errorf("the claim records session %q, want the session it had left behind", after.Session)
					}
				})
			}
		}
	}
}

// A claim whose process the read finds running is a claim in a turn to every door, and both
// recoveries go on refusing it in today's words: the record says working, the process is there,
// and what either operation would do to a turn of it is the fact the sentence gives as the
// condition it is. The two dead-process windows are asked here with the runtime answering that
// the process runs, which is the whole of what separates these rows from the 200 above: a door
// that answered from the daemon's own report would refuse the claim its read found alive, or
// stop it, and both answers are read here from the request alone.
//
// The third window is the claim no read has to correct: the refused relaunch left a live
// process under it and that process took a turn of its own, so the turn the sentence names is
// running (aRepointOverALiveProcess). It is the row both doors answered this way before any of
// them read a process, and the fix leaves it where it stands.
func TestARecoveryRefusedOverALiveProcessNamesTheTurn(t *testing.T) {
	windows := append(deadProcessWindows(), deadProcessWindow{
		name: "a relaunch the store refused over a live process",
		build: func(t *testing.T, h *harness) claim.Token {
			t.Helper()
			return workingAfterTheRelaunchWasRefused(t, h, aRepointOverALiveProcess())
		},
	})
	for _, window := range windows {
		for _, verdict := range processRunningVerdicts() {
			for _, door := range recoveryDoors() {
				t.Run(window.name+": "+verdict.name+": "+door.name, func(t *testing.T) {
					h := compactHarness(t, nil)
					token := window.build(t, h)
					h.runtime.ScriptProbe(verdict.result)
					reads := processReads(h)

					recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

					wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
						inATurnRefusal(token, door.cutsTheTurn, door.reaches))
					if got := processReads(h) - reads; got != 1 {
						t.Errorf("the request read the claim's process %d times, want the one read its answer turns on", got)
					}
					if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
						t.Errorf("the refused claim is %s recording session %q, want the working claim the refusal left on the session it had",
							after.State, after.Session)
					}
				})
			}
		}
	}
}

// A runtime that could not say whether the process is running leaves every door where it was:
// an unread process is not a dead one, and a fresh session that took it for one would cut off
// the turn of an agent that is working. So both recoveries give the answer a claim stored
// working takes, with the read that did not happen named in it, and neither suspends, forgets
// nor starts anything — the claim is left exactly as the request found it, for the caller to
// ask again when the runtime answers.
func TestARecoveryRefusedWhenTheRuntimeCouldNotReadTheProcess(t *testing.T) {
	for _, window := range deadProcessWindows() {
		for _, verdict := range unreadableProcessVerdicts() {
			for _, door := range recoveryDoors() {
				t.Run(window.name+": "+verdict.name+": "+door.name, func(t *testing.T) {
					h := compactHarness(t, nil)
					token := window.build(t, h)
					h.runtime.ScriptProbe(verdict.result)
					reads := processReads(h)

					recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

					wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
						unreadableProcessRefusal(token, door.reaches))
					if got := processReads(h) - reads; got != 1 {
						t.Errorf("the request read the claim's process %d times, want the one read it was refused for", got)
					}
					if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
						t.Errorf("the refused claim is %s recording session %q, want the working claim the request found, untouched",
							after.State, after.Session)
					}
				})
			}
		}
	}
}

// The compaction reads the same process and still refuses: it has no agent to ask, since
// nothing runs the session it would compact, and compacting is all it does. What it can do is
// name the door that acts — the fresh session, which starts such a claim again — so the
// caller is sent to the recovery that works rather than back to a wait that never ends. The
// code is the one every claim with no session to compact takes (api's claimNotRunning), and the
// claim is left stored working on the session it recorded, because the compaction changed
// nothing about it.
func TestACompactionOverADeadProcessNamesTheFreshSession(t *testing.T) {
	for _, window := range deadProcessWindows() {
		for _, verdict := range processGoneVerdicts() {
			for _, door := range compactDoors() {
				t.Run(window.name+": "+verdict.name+": "+door.name, func(t *testing.T) {
					h := compactHarness(t, nil)
					token := window.build(t, h)
					h.runtime.ScriptProbe(verdict.result)
					reads := processReads(h)

					recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

					wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimNotRunning, notRunningRefusal(token))
					if got := processReads(h) - reads; got != 1 {
						t.Errorf("the request read the claim's process %d times, want the one read its answer turns on", got)
					}
					if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
						t.Errorf("the refused claim is %s recording session %q, want the working claim on the session it had: the compaction changes nothing",
							after.State, after.Session)
					}
				})
			}
		}
	}
}

// failingClaimWriteStore is the supervise store with the claim writes refuse picks failing: the
// store did not answer them. It sits under refusingCheckLaunchStore where both are installed,
// and it is installed before the claim is created, because a machine takes its deps where it is
// built. refuse is set once the claim stands where the row needs it, so the writes that bring
// it up all land.
type failingClaimWriteStore struct {
	supervise.Store
	refuse atomic.Pointer[func(supervise.Claim) bool]
}

func (s *failingClaimWriteStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	if refuse := s.refuse.Load(); refuse != nil && (*refuse)(c) {
		return fmt.Errorf("write the claim %s: the store did not answer", c.Token)
	}
	return s.Store.PutClaim(ctx, c)
}

// refusing has the store refuse every claim write refuse picks from now on.
func (s *failingClaimWriteStore) refusing(refuse func(supervise.Claim) bool) {
	s.refuse.Store(&refuse)
}

// failingClaimWrites installs failingClaimWriteStore over the supervisor's store.
func failingClaimWrites(h *harness) *failingClaimWriteStore {
	writes := &failingClaimWriteStore{Store: h.supervisor.deps.Store}
	h.supervisor.deps.Store = writes
	return writes
}

// wantSuspendedClaimStarts is the sentence a fresh session that stopped the claim ends its
// answer with where the step it could not take left the claim suspended (api's
// notStartedAgain): nothing starts such a claim by itself, so the caller is told what does.
const wantSuspendedClaimStarts = "It is suspended, and nothing starts a suspended claim by itself, so a fresh session asked again is what starts it"

// The fresh session over a dead process stops the claim, forgets the session it recorded and
// then starts it, and either of the last two can be refused on its own. The claim is stopped by
// then, so a refusal would be false — the request did happen, and what did not is a step after
// the stop. The answer says so in the member that carries what the claim's own state does not
// record (api.FreshSessionResponse.PhaseNotRestarted, freshSessionNote, notStartedAgain): it
// names the claim, gives the store's own words for the refusal, and says what state that left
// the claim in, read from the machine rather than assumed, since the steps leave different ones.
//
//   - The start the store would not answer the lifecycle recheck of, which starts nothing and
//     writes nothing (refusingCheckLaunchStore, supervise's relaunchRefused): the claim reads
//     suspended with no session, and the answer says a fresh session asked again starts it.
//   - The forget the store would not write (supervise's forgetSession): the machine has
//     forgotten the session and owes the write, the stored claim is suspended still recording
//     it, and the answer says the same of the suspended claim.
//   - The start whose launch the store would not write (supervise's launchNow, relaunchLater):
//     the claim is launching, with that launch tried again at the probe interval, which starts
//     it by itself — and a fresh session asked of it then is refused as a claim between states.
//     The answer names the state it stands in and does not send the caller to ask again.
//
// The store's refusal is not spelled here, as it is the store's own wording; the sentence about
// the state is the route's own, and is spelled.
//
// The last row is the negative control, and is the same request over a process the read finds
// alive: it is refused, and the claim is left working on the session it had. So the arms are
// told apart from the answer alone — a 200 that says a step after the stop did not run, and a
// 409 that says nothing ran at all.
func TestAFreshSessionThatStoppedTheClaimAndCouldNotStartItSaysNotRestartedInItsAnswer(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name+": the start the store would not answer", func(t *testing.T) {
			h := compactHarness(t, nil)
			token := workingAfterTheRelaunchWasRefused(t, h, aDeath())
			h.runtime.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			reads := processReads(h)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session whose start was refused = %d, want the 200 of a claim that was stopped; body %s",
					recorder.Code, recorder.Body)
			}
			note := freshSessionNote(t, recorder)
			if !strings.Contains(note, string(token)) {
				t.Errorf("the answer says %q, want it to name %s, the claim whose start did not run", note, token)
			}
			if refusal := fmt.Sprintf("check the launch of %s: the store did not answer", token); !strings.Contains(note, refusal) {
				t.Errorf("the answer says %q, want the store's own refusal of the start in it, %q", note, refusal)
			}
			if !strings.HasSuffix(note, wantSuspendedClaimStarts) {
				t.Errorf("the answer says %q, want it to end %q", note, wantSuspendedClaimStarts)
			}
			if got := processReads(h) - reads; got != 1 {
				t.Errorf("the request read the claim's process %d times, want the one read its answer turns on", got)
			}
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.Session != "" {
				t.Errorf("the claim is %s recording session %q, want the suspended claim with no session the refused start left",
					after.State, after.Session)
			}
		})
		t.Run(door.name+": the forget the store would not write", func(t *testing.T) {
			h := compactHarness(t, nil)
			writes := failingClaimWrites(h)
			token := workingAfterTheRelaunchWasRefused(t, h, aDeath())
			h.runtime.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			writes.refusing(func(c supervise.Claim) bool { return c.Session == "" })

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session whose forget was refused = %d, want the 200 of a claim that was stopped; body %s",
					recorder.Code, recorder.Body)
			}
			note := freshSessionNote(t, recorder)
			for _, want := range []string{
				string(token) + " was stopped for a fresh session, and the forget of the session it recorded was refused: ",
				fmt.Sprintf("write the claim %s: the store did not answer", token),
			} {
				if !strings.Contains(note, want) {
					t.Errorf("the answer says %q, want %q in it", note, want)
				}
			}
			if !strings.HasSuffix(note, wantSuspendedClaimStarts) {
				t.Errorf("the answer says %q, want it to end %q", note, wantSuspendedClaimStarts)
			}
			m, _ := h.supervisor.Machine(token)
			if c := m.Claim(); c.State != supervise.StateSuspended || c.Session != "" {
				t.Errorf("the machine holds the claim %s recording session %q, want it suspended with the session forgotten and the write owed",
					c.State, c.Session)
			}
			if after := h.stored(token); after.State != supervise.StateSuspended || after.Session != "ses_implementer" {
				t.Errorf("the stored claim is %s recording session %q, want the suspension written and the forget not",
					after.State, after.Session)
			}
		})
		t.Run(door.name+": the launch the store would not write", func(t *testing.T) {
			h := compactHarness(t, nil)
			writes := failingClaimWrites(h)
			token := workingAfterTheRelaunchWasRefused(t, h, aDeath())
			launchesAnswerAgain(t, h)
			h.runtime.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			writes.refusing(func(c supervise.Claim) bool { return c.State == supervise.StateLaunching })

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if recorder.Code != http.StatusOK {
				t.Fatalf("the fresh session whose launch was not written = %d, want the 200 of a claim that was stopped; body %s",
					recorder.Code, recorder.Body)
			}
			note := freshSessionNote(t, recorder)
			for _, want := range []string{
				string(token) + " was stopped for a fresh session, and the start of it in a new session was refused: ",
				fmt.Sprintf("write the claim %s: the store did not answer", token),
			} {
				if !strings.Contains(note, want) {
					t.Errorf("the answer says %q, want %q in it", note, want)
				}
			}
			if want := ". It is launching now; `read_state` (`legion claims list`) shows its state"; !strings.HasSuffix(note, want) {
				t.Errorf("the answer says %q, want it to end %q", note, want)
			}
			if strings.Contains(note, "asked again") {
				t.Errorf("the answer says %q, want no word sending the caller back for a claim that starts by itself", note)
			}
			m, _ := h.supervisor.Machine(token)
			if c := m.Claim(); c.State != supervise.StateLaunching || c.Session != "" {
				t.Errorf("the machine holds the claim %s recording session %q, want it launching in no session, its launch tried again",
					c.State, c.Session)
			}
			if after := h.stored(token); after.State != supervise.StateSuspended || after.Session != "" {
				t.Errorf("the stored claim is %s recording session %q, want the suspension and the forget written and the launch not",
					after.State, after.Session)
			}
		})
		t.Run(door.name+": the live process the read finds instead", func(t *testing.T) {
			h := compactHarness(t, nil)
			token := workingAfterTheRelaunchWasRefused(t, h, aDeath())
			h.runtime.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})
			reads := processReads(h)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
				inATurnRefusal(token, "a fresh session would cut that turn off", wantFreshSessionReaches))
			if got := processReads(h) - reads; got != 1 {
				t.Errorf("the request read the claim's process %d times, want the one read its answer turns on", got)
			}
			if after := h.stored(token); after.State != supervise.StateWorking || after.Session != "ses_implementer" {
				t.Errorf("the refused claim is %s recording session %q, want the working claim the refusal left on the session it had",
					after.State, after.Session)
			}
		})
	}
}

// routeServer is the server the recoveries run on, built bare so a row can call one of its
// steps with a view of its own choosing: the races below are a view taken, then the claim
// moving, then the step, and no request through the routes can hold that order still.
func routeServer(h *harness) *server {
	return &server{
		log:        slog.Default(),
		supervisor: h.supervisor,
		project:    testProject,
		stopping:   context.Background(),
		drained:    context.Background(),
		decisions:  NewRouteDecisions(),
	}
}

// A launch the wake pacing parks can be raised between the route's view of the claim and the
// machine's lock, and both steps that meet such a claim there answer it as the parked relaunch
// it is (api's refuseParkedRelaunch, claimUnsettled), never as a claim in a turn.
//
//   - The read (refuseWorkingClaim): the view was taken of a claim in a turn, and the sweep's
//     death of its process parked the relaunch before the read; the machine asks nothing of a
//     process a parked launch replaces (supervise.ProcessNotRead), and the step reads the park
//     again from the machine. Both doors make this read, so both are asked.
//   - The suspension (suspendForFreshSession): the read found the process gone, and the same
//     death parked the relaunch before the suspension arrived; the machine leaves that claim to
//     the launch and refuses the suspension, which may not be held.
//
// Neither changes the claim: it stays working under the launch the pacing holds. The negative
// control is the step's own default put back, which answers both CLAIM_IN_A_TURN.
func TestARecoveryStepThatMeetsAParkRaisedAfterItsViewNamesTheLaunchInFlight(t *testing.T) {
	for _, door := range []struct {
		name    string
		op      controllerOp
		reaches string
	}{
		{"the fresh session", freshSessionOp, wantFreshSessionReaches},
		{"the compaction", compactOp, wantCompactionReaches},
	} {
		t.Run("the read: "+door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			var park atomic.Bool
			h.supervisor.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time {
				if park.Load() {
					return now.Add(time.Hour)
				}
				return now
			}}
			token := readyImplementer(t, h)
			m, _ := h.supervisor.Machine(token)
			if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("start %s's turn: %v", token, err)
			}
			before := m.View()
			if before.State != supervise.StateWorking || before.WakeWaiting != nil {
				t.Fatalf("the view is of a claim %s with wake %+v, want one in a turn with nothing parked", before.State, before.WakeWaiting)
			}
			park.Store(true)
			if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
				Locator: *before.Locator, Kind: runtime.Gone, At: stillClock{}.Now(),
			}}); err != nil {
				t.Fatalf("post the death of %s's process: %v", token, err)
			}
			if now := m.View(); now.State != supervise.StateWorking || !relaunchWaits(now) {
				t.Fatalf("the claim is %s with wake %+v, want it working under the relaunch the pacing parked", now.State, now.WakeWaiting)
			}
			reads := processReads(h)
			recorder := httptest.NewRecorder()

			answered := routeServer(h).refuseWorkingClaim(recorder, httptest.NewRequest(http.MethodPost, "/", nil), m, token, before, door.op)

			if !answered {
				t.Fatal("the read answered nothing, want the refusal of the parked relaunch")
			}
			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled, parkedRelaunchRefusal(token, door.reaches))
			if got := processReads(h) - reads; got != 0 {
				t.Errorf("the runtime was asked %d times, want nothing asked of a process a parked launch replaces", got)
			}
			if now := m.View(); now.State != supervise.StateWorking || !relaunchWaits(now) {
				t.Errorf("the claim is %s with wake %+v, want it left working under the parked relaunch", now.State, now.WakeWaiting)
			}
		})
	}
	t.Run("the suspension", func(t *testing.T) {
		h := compactHarness(t, nil)
		token := workingUnderAParkedRelaunch(t, h, runtime.Observation{Kind: runtime.Gone})
		m, _ := h.supervisor.Machine(token)
		recorder := httptest.NewRecorder()

		suspended := routeServer(h).suspendForFreshSession(recorder, httptest.NewRequest(http.MethodPost, "/", nil), m, token)

		if suspended {
			t.Fatal("the suspension landed, want it refused for the parked relaunch")
		}
		wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled, parkedRelaunchRefusal(token, wantFreshSessionReaches))
		if now := m.View(); now.State != supervise.StateWorking || !relaunchWaits(now) || now.SuspensionHeld {
			t.Errorf("the claim is %s with wake %+v and a suspension held %v, want it left working under the parked relaunch with nothing held",
				now.State, now.WakeWaiting, now.SuspensionHeld)
		}
	})
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
// refusal names no end of the hold at all (api's refuseHeldSuspension).
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
// "being compacted already": what such a claim waits for is the hold rather than the
// compaction, and only the hold's sentence names the door for each state it can be read in.
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
// its ready before the claim is ever published ready (supervise's ready), so the refusal it
// takes is the hold's own, which names the door for a suspended claim as well as for a running
// one.
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
// bring, and the ends differ — the operator's hold lands as a suspension, and a start the
// daemon runs against the claim drops the daemon's own and leaves the claim running (api's
// refuseHeldSuspension). The sentence names no end at all, so one answer is true of every row.
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
// with the asking operation's own name in it and the door that reaches each state the caller
// can read of the claim: what starts a suspended, failed or retired claim is the claim's
// role's — the fresh session for a workflow role, and the daemon's controller keeper for the
// project's own controller — and the asking operation is what reaches a live one, in the
// states that operation's own set holds (api's refuseHeldSuspension, startsAgainName,
// controllerOp.liveStates).
//
// The second sentence is of the claims nothing is held over, because the claim in hand is
// held: a hold over a ready agent is reached by neither door while it stands, and a sentence
// that said the asking operation reaches this claim while it is ready would deny the one
// before it. What the words themselves have to be is checked from the predicates rather than
// from this copy of them (TestTheHeldSuspensionRefusalNamesEachDoorsOwnLiveStates).
func heldSuspensionRefusal(token claim.Token, startsAgain, operation, liveStates string) string {
	return string(token) + " has a suspension held over it, and " + operation + " reaches no claim a " +
		"suspension waits over, whatever state it is in. Of the claims with no suspension held over them, " +
		startsAgain + " reaches those that are suspended, failed or retired, and " + operation +
		" reaches those whose agent is " + liveStates + ". " + wantAskAgainLater
}

// A suspension held over the claim is read ahead of every other window the claim can be in,
// because what such a claim waits for is the hold's end rather than a turn's, a compaction's
// or a launch's: neither "ask again once it is running" nor "ask again after the turn ends"
// names a wait that comes for it. The refusal names no end of the hold either, since the end
// itself differs: an operator's hold lands as a suspension, and a start the daemon runs
// against the claim drops the daemon's own and leaves it running. It names the states each
// operation reaches instead, of the claims nothing is held over, and both recoveries say so
// of every row (api's refuseHeldSuspension). The code each row takes is the claim's own, so
// the answer a state was refused with before either door read the hold is the answer it is
// refused with now.
func TestARecoveryRefusedUnderAHeldSuspensionSaysTheClaimStopsWorking(t *testing.T) {
	for _, held := range heldSuspensionCases() {
		for _, door := range compactDoors() {
			t.Run(held.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := held.start(t, h)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, held.code,
					heldSuspensionRefusal(token, freshSessionName, "a compaction", wantCompactionLive))
			})
		}
		for _, door := range freshSessionDoors() {
			t.Run(held.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := held.start(t, h)

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, held.code,
					heldSuspensionRefusal(token, freshSessionName, "a fresh session", wantFreshSessionLive))
				if m, _ := h.supervisor.Machine(token); !m.View().SuspensionHeld {
					t.Errorf("the refused claim holds no suspension, want the one the refusal left held")
				}
			})
		}
	}
}

// The project's own controller is the one claim of these that no fresh session reaches. The
// compaction reaches the controller in every state, since it stops nothing (api's compact),
// and the fresh session refuses it in every state, since starting the controller's process
// again is the daemon's controller keeper's alone (api's freshSession).
// So this refusal names that keeper as what reaches the controller while it is suspended,
// failed or retired: fresh_session would send the operator to a door that answers
// NOT_THE_CONTROLLER, which is the circle the rows above exist to keep a caller out of. Only
// the operator's door asks it, since `controller` is no role the controller's own compact may
// name (api's claimCompact).
func TestACompactionRefusedUnderAHeldSuspensionOverTheControllerNamesItsKeeper(t *testing.T) {
	h := compactHarness(t, nil)
	// The hold that is live before the agent is: the operator's suspend reaches the controller
	// its keeper launched before anything registered on it, so the machine holds the stop for
	// the agent's ready (supervise's holdSuspendUntilReady).
	token, _ := h.launchController()
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("the suspend of the launching controller = %d, want the 202 of one held for the agent's ready; body %s",
			recorder.Code, recorder.Body)
	}
	m, _ := h.supervisor.Machine(token)
	if view := m.View(); view.State != supervise.StateLaunching || !view.SuspensionHeld {
		t.Fatalf("the controller is %s holding %v, want the launching claim with the operator's suspension held",
			view.State, view.SuspensionHeld)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
		heldSuspensionRefusal(token, controllerKeeperName, "a compaction", wantCompactionLive))
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
