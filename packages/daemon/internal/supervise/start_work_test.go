package supervise

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// startOfStep is the work of a workflow start of a step of run 1, which the outbox's start row
// names and its request carries (StartWork).
var startOfStep = StartWork{Phase: phase.Step, Generation: 1}

// phaseHoldsAnswering sets a machine's PhaseHolds to answer *holds for every delivery, and counts its calls.
func phaseHoldsAnswering(holds *bool, asked *int) func(*Deps) {
	return func(d *Deps) {
		d.PhaseHolds = func(context.Context, Claim, Delivery) (bool, error) {
			*asked++
			return *holds, nil
		}
	}
}

// A workflow start brings its claim back for the work its row was queued for (StartWork), and the
// machine judges that work again under its own lock before it starts a process: a spawn of work
// the issue has left starts nothing, answers ErrStartLeft and leaves the claim queued, as it was.
// The controls: the same spawn while the work holds starts the process, and a spawn that names no
// work — the operator's — is judged by nothing.
func TestASpawnOfWorkTheIssueLeftStartsNoProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		start  StartWork
		holds  bool
		starts bool
	}{
		{"a start of work the issue has left", startOfStep, false, false},
		{"the control: a start of work that holds", startOfStep, true, true},
		{"the control: a spawn that names no work", StartWork{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := tc.holds, 0
			h := newBareHarness(t)
			phaseHoldsAnswering(&holds, &asked)(&h.deps)
			if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
				t.Fatal(err)
			}
			h.start(queuedClaim())

			err := h.handle(RequestSpawn{Claim: testToken, Start: tc.start})

			if tc.starts {
				if err != nil {
					t.Fatalf("the spawn = %v, want it to start the process", err)
				}
				h.wantStarts(testToken, 1)
				h.wantState(StateLaunching)
				return
			}
			if !errors.Is(err, ErrStartLeft) {
				t.Fatalf("the spawn of work the issue left = %v, want ErrStartLeft", err)
			}
			h.wantStarts(testToken, 0)
			h.wantState(StateQueued)
		})
	}
}

// A workflow retry of work the issue has left is judged before anything of the claim changes: the
// failed claim keeps its state, its session and the record of its failure, and no process starts.
// The control is the same retry while its work holds, which relaunches the session it kept.
func TestARetryOfWorkTheIssueLeftLeavesTheFailedClaimAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		holds bool
	}{
		{"a retry of work the issue has left", false},
		{"the control: a retry of work that holds", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := tc.holds, 0
			h := newBareHarness(t)
			phaseHoldsAnswering(&holds, &asked)(&h.deps)
			failed := queuedClaim()
			failed.State, failed.Generation, failed.Session, failed.SessionFile = StateFailed, 1, session, sessionFile
			failed.Failure, failed.FailureCause = "launch failures ran out", CauseNeverRegistered
			if err := h.store.PutClaim(h.ctx, failed); err != nil {
				t.Fatal(err)
			}
			h.start(failed)

			err := h.handle(RequestRetry{Claim: testToken, FreshSession: true, Start: startOfStep})

			if tc.holds {
				if err != nil {
					t.Fatalf("the retry = %v, want it to relaunch the claim", err)
				}
				h.wantStarts(testToken, 1)
				return
			}
			got := h.claim()
			if !errors.Is(err, ErrStartLeft) || got.State != StateFailed || got.Session != session || got.Failure != failed.Failure {
				t.Fatalf("the retry of work the issue left = %v, the claim %s in session %q with failure %q; want ErrStartLeft and the failed claim as it was",
					err, got.State, got.Session, got.Failure)
			}
			h.wantStarts(testToken, 0)
		})
	}
}

// A start's launch the wake pacing parks waits its turn written launching with no process, and the
// issue can leave the start's work during the wait. The turn judges the work again: a launch whose
// work the issue left starts no process and suspends the claim for the daemon, its task retired
// with the suspension, so the start of the role's next work resumes it by rule. The control is the
// same parked start whose work still holds at the turn, which launches.
func TestAParkedStartOfWorkTheIssueLeftStartsNoProcessAtItsTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holds  bool
		starts bool
	}{
		{"the issue leaves the start's work during the wait", false, false},
		{"the control: the work still holds at the turn", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := true, 0
			h := pacedHarness(t, wakeRate, phaseHoldsAnswering(&holds, &asked))
			second := h.machineFor(wakeClaim(secondToken, StateQueued))
			h.must(RequestSpawn{Claim: h.token})
			h.mustPostTo(second, RequestSpawn{Claim: secondToken, Start: startOfStep})
			h.mustPostTo(second, RequestDeliver{Claim: secondToken, Task: "check the step", Phase: phase.Step, Generation: 1})
			if state := second.Claim().State; state != StateLaunching {
				t.Fatalf("the parked start's claim is %s, want %s", state, StateLaunching)
			}
			h.wantStarts(secondToken, 0)

			holds = tc.holds
			h.clock.Advance(wakeInterval + wakePart)
			second.Wait()

			got := second.Claim()
			if tc.starts {
				h.wantStarts(secondToken, 1)
				if got.State != StateLaunching {
					t.Fatalf("the parked start's claim is %s after its turn, want %s", got.State, StateLaunching)
				}
				return
			}
			h.wantStarts(secondToken, 0)
			if got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon || got.Pending != nil {
				t.Fatalf("the parked start's claim after its turn = %s by %q holding %+v, want suspended by the daemon holding no task",
					got.State, got.SuspendedBy, got.Pending)
			}
		})
	}
}

// A start's launch can park at its issue pod's gate as well, behind another role's session load
// (askPod), written launching with no process, and the issue can leave the start's work before the
// gate grants the pod. The grant judges the work again, as the wake pacing's turn does
// (startLeftWhileParked): a launch whose work the issue left starts no process, its claim is
// suspended for the daemon, and the pod goes to the next start that waits. The control is the same
// parked start whose work still holds at the grant, which starts once while the next start waits.
func TestAStartParkedAtThePodsGateIsJudgedAgainAtItsGrant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		holds bool
	}{
		{"the issue leaves the start's work before the grant", false},
		{"the control: the work still holds at the grant", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := true, 0
			h := podHarness(t, "")
			phaseHoldsAnswering(&holds, &asked)(&h.deps)
			first, second := h.podRole(claim.RoleArchitect, StateSuspended), h.podRole(claim.RoleImplementer, StateSuspended)
			third := h.podRole(claim.RoleTester, StateSuspended)
			h.mustPostTo(first.m, RequestResume{Claim: first.token, By: SuspendedByOperator})
			h.wantStarts(first.token, 1)
			h.parks(second, RequestRuleResume{Claim: second.token, Reason: "it owns its issue's step", Start: startOfStep}, 0)
			h.parks(third, RequestResume{Claim: third.token, By: SuspendedByOperator}, 0)

			holds = tc.holds
			h.loadEnds(first)

			if tc.holds {
				h.wantGrantedStart(second, 1)
				h.wantStarts(third.token, 0)
				return
			}
			h.wantGrantedStart(third, 1)
			second.m.Wait()
			h.wantStarts(second.token, 0)
			if got := second.m.Claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon || got.Locator != nil {
				t.Fatalf("the parked start's claim after the grant = %s by %q recording %+v, want suspended by the daemon with no process",
					got.State, got.SuspendedBy, got.Locator)
			}
		})
	}
}

// A start's launch the session store could not answer is tried again a minute later with the claim
// launching and no process (startWaitsForStore), and that retry carries the start's work, so it
// judges the work again as a parked launch's turn does: a start whose work the issue left during
// the minute starts no process and the claim is suspended for the daemon. The control is the same
// retry while the work holds, which resumes the session again.
func TestAStartRetriedAfterTheSessionStoreFailedJudgesItsWorkAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holds   bool
		resumes int
	}{
		{"the issue leaves the start's work before the retry", false, 1},
		{"the control: the work still holds at the retry", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := true, 0
			h := newBareHarness(t)
			phaseHoldsAnswering(&holds, &asked)(&h.deps)
			suspended := queuedClaim()
			suspended.State, suspended.Generation, suspended.Session, suspended.SessionFile = StateSuspended, 1, session, sessionFile
			suspended.SuspendedBy = SuspendedByDaemon
			if err := h.store.PutClaim(h.ctx, suspended); err != nil {
				t.Fatal(err)
			}
			h.start(suspended)
			storeDown := fmt.Errorf("resume %s: %w: dial tcp: connection refused", testToken, runtime.ErrSessionStore)
			h.rt.OnResume = func(context.Context) error { return storeDown }

			if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step", Start: startOfStep}); !errors.Is(err, runtime.ErrSessionStore) {
				t.Fatalf("the start's resume answered %v, want the store's refusal", err)
			}
			h.wantState(StateLaunching)
			h.rt.OnResume = nil
			holds = tc.holds
			h.advance(sessionStoreRetry)

			h.wantCalls("Resume", tc.resumes)
			if got := h.claim(); !tc.holds && (got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon) {
				t.Fatalf("the retried start's claim = %s by %q, want suspended by the daemon", got.State, got.SuspendedBy)
			}
		})
	}
}

// A retry relaunchLater armed comes back over a claim left launching with no process, and its start
// is judged before the retry asks the wake gate for a place (launchAfter, startLeftWhileParked):
// a retry whose start's work the issue left during its wait suspends the claim for the daemon at
// once and takes no place in the pacing, whose next turn then goes to a launch that starts. Judged
// only at the turn, the claim would wait a whole turn written launching, which over a wave is tens
// of minutes, and spend a turn of the pacing on a launch that starts nothing. The retry here is the
// session store's, met while another claim's launch holds the pacing's one place. The control is
// the same retry while the work holds, which the pacing does hold: it waits its turn, launching
// with no process.
func TestARetryOfWorkTheIssueLeftIsJudgedBeforeItTakesAPlaceInThePacing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		holds bool
	}{
		{"the issue leaves the start's work before the retry", false},
		{"the control: the work still holds at the retry", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := true, 0
			h := pacedHarness(t, wakeRate, phaseHoldsAnswering(&holds, &asked))
			retried := h.machineFor(wakeClaim(secondToken, StateSuspended))
			storeDown := fmt.Errorf("resume %s: %w: dial tcp: connection refused", secondToken, runtime.ErrSessionStore)
			h.rt.OnResume = func(context.Context) error { return storeDown }
			if err := h.postTo(retried, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's step", Start: startOfStep}); !errors.Is(err, runtime.ErrSessionStore) {
				t.Fatalf("the start's resume answered %v, want the store's refusal", err)
			}
			if state := retried.Claim().State; state != StateLaunching {
				t.Fatalf("the start the store refused is %s, want %s with its retry armed", state, StateLaunching)
			}
			h.rt.OnResume = nil
			holds = tc.holds

			// A second before the retry is due, another claim's launch takes the pacing's one
			// place, so the retry finds none free.
			h.clock.Advance(sessionStoreRetry - time.Second)
			h.must(RequestSpawn{Claim: h.token})
			h.wantStarts(h.token, 1)
			h.clock.Advance(time.Second)
			retried.Wait()

			h.wantStarts(secondToken, 1)
			got := retried.View()
			if tc.holds {
				if got.Claim.State != StateLaunching || got.WakeWaiting == nil {
					t.Fatalf("the retry of work that holds left the claim %s holding wake %+v, want it launching with its wake waiting for the pacing's next turn",
						got.Claim.State, got.WakeWaiting)
				}
				return
			}
			if got.Claim.State != StateSuspended || got.Claim.SuspendedBy != SuspendedByDaemon || got.WakeWaiting != nil {
				t.Fatalf("the retry of work the issue left left the claim %s by %q holding wake %+v, want it suspended by the daemon at the retry, holding no place in the pacing",
					got.Claim.State, got.Claim.SuspendedBy, got.WakeWaiting)
			}
		})
	}
}

// A pod-round wait must keep the workflow start's identity, as a session-store wait does.
// Dropping that identity at the retry launches obsolete work; the control keeps the work
// current and must resume the same session after the pod is replaced.
func TestAStartRetriedAfterItsPodRoundWaitJudgesItsWorkAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holds   bool
		resumes int
	}{
		{"the issue leaves the start's work before the retry", false, 1},
		{"the control: the work still holds at the retry", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holds, asked := true, 0
			h := newBareHarness(t)
			phaseHoldsAnswering(&holds, &asked)(&h.deps)
			suspended := queuedClaim()
			suspended.State, suspended.Generation, suspended.Session, suspended.SessionFile = StateSuspended, 1, session, sessionFile
			suspended.SuspendedBy = SuspendedByDaemon
			if err := h.store.PutClaim(h.ctx, suspended); err != nil {
				t.Fatal(err)
			}
			h.start(suspended)
			waiting := fmt.Errorf("resume %s: %w", testToken, runtime.ErrAwaitingPodRound)
			h.rt.OnResume = func(context.Context) error { return waiting }
			if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step", Start: startOfStep}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
				t.Fatalf("the start's resume answered %v, want the wait for its pod's round", err)
			}
			h.wantState(StateLaunching)
			h.rt.OnResume = nil
			holds = tc.holds
			h.advance(podRoundRetry)

			h.wantCalls("Resume", tc.resumes)
			got := h.claim()
			if !tc.holds && (got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon || got.Locator != nil) {
				t.Fatalf("the retried start's claim = %s by %q at %+v, want suspended by the daemon with no process", got.State, got.SuspendedBy, got.Locator)
			}
			if tc.holds && (got.Locator == nil || got.SessionFile != sessionFile) {
				t.Fatalf("the current start's retry = %+v, want the same session running", got)
			}
		})
	}
}
