package supervise

import (
	"context"
	"errors"
	"fmt"
	"testing"

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

// A start that leaves the session its claim records (StartWork's LeaveSession) brings the claim
// back in a session of its own: the rule's resume of a suspended claim and the workflow's retry of
// a retired one each spawn a fresh agent, and the claim records the session it left as abandoned.
// The control is the same start naming a session the claim does not record, which resumes the one
// it does and abandons nothing.
func TestAStartThatLeavesTheRecordedSessionSpawnsAFreshAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stop  func(h *harness)
		start func(h *harness, start StartWork) Event
	}{
		{"the rule's resume of a suspended claim", func(h *harness) {
			h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "the test stops it"})
			h.wantState(StateSuspended)
		}, func(h *harness, start StartWork) Event {
			return RequestRuleResume{Claim: h.token, Reason: "the test starts it", Start: start}
		}},
		{"the workflow's retry of a retired claim", func(h *harness) {
			h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
			h.wantState(StateRetired)
		}, func(h *harness, start StartWork) Event { return RequestRetry{Claim: h.token, Start: start} }},
	} {
		for _, leave := range []struct {
			name    string
			session string
			spawns  bool
		}{
			{"it names the session the claim records", session, true},
			{"the control: it names another session", "ses-another", false},
		} {
			t.Run(tc.name+": "+leave.name, func(t *testing.T) {
				h := newHarness(t)
				h.reach(StateIdle)
				tc.stop(h)
				spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

				h.must(tc.start(h, StartWork{LeaveSession: leave.session}))

				h.wantState(StateLaunching)
				spawned, resumed := len(h.calls("Spawn"))-spawns, len(h.calls("Resume"))-resumes
				got := h.claim()
				if leave.spawns {
					if spawned != 1 || resumed != 0 || got.Session != "" || got.Abandoned.Session != session {
						t.Fatalf("the start spawned %d and resumed %d, and the claim records %q and abandoned %q; want one spawn, no resume, no session and %s abandoned",
							spawned, resumed, got.Session, got.Abandoned.Session, session)
					}
					return
				}
				if spawned != 0 || resumed != 1 || got.Session != session || got.Abandoned.Session != "" {
					t.Fatalf("the start spawned %d and resumed %d, and the claim records %q and abandoned %q; want one resume of %s and nothing abandoned",
						spawned, resumed, got.Session, got.Abandoned.Session, session)
				}
			})
		}
	}
}

// A start's launch the wake pacing parks waits its turn written launching with no process, and the
// issue can leave the start's work during the wait. The turn judges the work again: a launch whose
// work the issue left, over a claim whose task is of that same work, starts no process and
// suspends the claim for the daemon, its task retired with the suspension, so the start of the
// role's next work resumes it by rule. The control is the same parked start whose work still holds
// at the turn, which launches.
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

// A start that brings its role back and hands it nothing (StartWork.BringsBack: the promotion's
// start of a role of a mid-phase member that has run) can have its launch parked by the wake
// pacing, and while it waits a step can send that role back to work: a checker's return to
// implementing, whose task the outbox delivers to the claim it finds launching (deliverLater). By
// the turn the issue has left the start's work but not the newer task's, so the launch goes on for
// that task (startLeftWhileParked): the claim is not suspended, its process starts, and the task it
// is sent at its ready is the newer one. The control is the same park over a newer task whose work
// the issue has left as well, which suspends the claim for the daemon and retires that task unsent,
// as every park whose start's work the issue left does over a claim that holds no task of work its
// issue still holds.
//
// The negative control is the rule reverted (the `return false, nil` of startLeftWhileParked's
// `if holds` branch deleted, so the case falls through to startLeft): the first case then suspends
// the claim with no process started, which this test reads at the runtime's starts.
func TestAParkedBroughtBackStartGoesOnForTheNewerTaskItsClaimHolds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newerHolds bool
	}{
		{"the newer task's work still holds at the turn", true},
		{"the control: the issue has left the newer task's work too", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broughtBackHolds := true
			h := pacedHarness(t, wakeRate, func(deps *Deps) {
				deps.PhaseHolds = func(_ context.Context, _ Claim, d Delivery) (bool, error) {
					if d.BringsBack {
						return broughtBackHolds, nil
					}
					return tc.newerHolds, nil
				}
			})
			other := h.machineFor(wakeClaim(secondToken, StateQueued))
			h.mustPostTo(other, RequestSpawn{Claim: secondToken})
			h.must(RequestSpawn{Claim: h.token, Start: StartWork{Phase: phase.Step, Generation: 1, BringsBack: true}})
			h.wantState(StateLaunching)
			h.wantStarts(h.token, 0)

			broughtBackHolds = false
			newer := RequestDeliver{Claim: h.token, ID: "outbox:41", Row: 41, Task: "implement what the checker returned",
				Phase: phase.Implementing, Generation: 1}
			h.must(newer)
			if p := h.claim().Pending; p == nil || p.ID != newer.ID {
				t.Fatalf("the parked claim holds %+v, want the newer task %s pending", p, newer.ID)
			}

			h.advance(wakeInterval + wakePart)

			got := h.claim()
			if !tc.newerHolds {
				h.wantStarts(h.token, 0)
				if got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon || got.Pending != nil {
					t.Fatalf("the parked start's claim after its turn = %s by %q holding %+v, want suspended by the daemon holding no task",
						got.State, got.SuspendedBy, got.Pending)
				}
				return
			}
			h.wantStarts(h.token, 1)
			if got.State != StateLaunching || got.SuspendedBy != "" || got.Pending == nil || got.Pending.ID != newer.ID {
				t.Fatalf("the parked start's claim after its turn = %s by %q holding %+v, want launching, not suspended, holding the newer task %s",
					got.State, got.SuspendedBy, got.Pending, newer.ID)
			}
			h.relaunched()
			if prompts := h.wantPrompts(1); prompts[0].DeliveryID != newer.ID {
				t.Fatalf("the launched process was sent delivery %s, want the newer task %s", prompts[0].DeliveryID, newer.ID)
			}
		})
	}
}

// A start's launch can park at its issue pod's gate as well, behind another role's session load
// (askPod), written launching with no process, and the issue can leave the start's work before the
// gate grants the pod. The grant judges the work again, as the wake pacing's turn does
// (startLeftWhileParked): a launch whose work the issue left, over a claim that holds no task,
// starts no process, its claim is suspended for the daemon, and the pod goes to the next start
// that waits. The control is the same parked start whose work still holds at the grant, which
// starts once while the next start waits.
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
// the minute, over a claim that holds no task, starts no process and the claim is suspended for
// the daemon. The control is the same retry while the work holds, which resumes the session again.
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
