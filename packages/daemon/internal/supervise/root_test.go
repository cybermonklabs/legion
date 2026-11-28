package supervise

// A root's claim — the architect of an issue that holds its own design gate, which the outbox marks
// on the claim at each start from the Dispatch chain (Claim.HoldsGate, R15 and R18 of
// dispatch://INFRA-3320) — ends only when its run closes: its exit and its registration deadline
// suspend it, and only the close releases it. The mark decides, never the tree the claim stores.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The registration deadline relaunches the claim, so it suspends the unregistered process and never
// releases it: under a sandbox a release deletes the claim's objects, and the root issue's volume
// with them.
func TestTheRegistrationDeadlineRetiresARootThroughSuspendNeverRelease(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.launch()
	alive := h.locator()

	h.advance(deadline)

	if suspends := h.wantCalls("Suspend", 1); suspends[0].Locator != alive {
		t.Errorf("suspended %+v, want the root's live process", suspends[0].Locator)
	}
	h.wantCalls("Release", 0)
	h.wantCalls("Spawn", 2)
	h.wantState(StateLaunching)
}

// The tree's root claim is not retired before its tree closes: its agent's exit suspends it, so
// the claim keeps its session and stays resumable — and known to the orphan sweep, which under a
// sandbox would otherwise delete its Sandbox and the issue's volume with it.
func TestARootClaimsExitSuspendsIt(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			loc := h.locator()

			h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

			if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
				t.Errorf("suspended %+v, want %+v", suspend.Locator, loc)
			}
			h.wantCalls("Release", 0)
			h.wantState(StateSuspended)
			c := h.claim()
			if c.Locator != nil || c.Session != session || c.SessionFile != sessionFile || h.clock.Live() != 0 {
				t.Errorf("claim %+v with %d timers armed, want no locator, the session kept, and nothing armed", c, h.clock.Live())
			}
			if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.Locator != nil {
				t.Errorf("stored %+v, want the suspension persisted", stored)
			}

			h.must(RequestResume{Claim: rootToken, By: SuspendedByOperator})
			if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != loc || resume.Spec.ResumeSessionFile != sessionFile {
				t.Errorf("resumed %+v waiting out %+v, want the session after the suspended incarnation", resume.Spec, resume.Previous)
			}
		})
	}
}

// An exit is the agent's own end, so a runtime that cannot suspend its process does not keep a
// finished root live: the error is logged and the claim is suspended anyway, its secret revoked,
// with the process remembered for the resume to wait out.
func TestARootExitWhoseSuspendFailsStillSuspendsIt(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	loc := h.locator()
	h.rt.FailSuspend(errBoom)

	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

	h.wantState(StateSuspended)
	if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.Locator != nil || stored.CapabilityHash != nil {
		t.Errorf("stored %+v, want the root suspended with no process and no capability", stored)
	}
	if lines := h.logs.lines("suspend", errBoom.Error()); len(lines) != 1 {
		t.Errorf("logged %v, want the failed suspension named once", lines)
	}
	h.rt.FailSuspend(nil)
	h.must(RequestResume{Claim: rootToken, By: SuspendedByOperator})
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != loc {
		t.Errorf("resumed waiting out %+v, want the exited process %+v", resume.Previous, loc)
	}
}

// A root's exit ends its phase as a suspension does, so a task still pending unconfirmed — sent and
// acknowledged, no turn started — is retired with it: the next resume starts with its new phase's
// task, never the finished one's. That holds whether or not the runtime could suspend the process.
func TestARootsExitRetiresItsPhasesUnconfirmedTask(t *testing.T) {
	for _, failSuspend := range []bool{false, true} {
		name := "suspend ok"
		if failSuspend {
			name = "suspend fails"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(StateReady)
			// A phase's task, which is what the exit retires: a task of no phase is nobody's to
			// end here (TestAnOperatorsTaskSurvivesASuspensionAndGoesOnResume).
			h.must(RequestDeliver{Claim: rootToken, Task: "the task", Phase: phase.Implementing})
			h.wantPrompts(1)
			if stored, ok := h.store.delivery(rootToken); !ok || stored.DeliveredAt.IsZero() || !stored.ConfirmedAt.IsZero() {
				t.Fatalf("stored delivery %+v (ok=%v), want it acknowledged and unconfirmed", stored, ok)
			}
			if failSuspend {
				h.rt.FailSuspend(errBoom)
			}

			h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})

			h.wantState(StateSuspended)
			if p := h.claim().Pending; p != nil {
				t.Errorf("pending %+v, want the unconfirmed task retired with the root's exit", p)
			}
			if d, ok := h.store.delivery(rootToken); ok {
				t.Errorf("stored delivery %+v, want it retired", d)
			}
		})
	}
}

// The compound failure: a root's exit whose Suspend fails leaves a process that may still run, the
// resumes that follow are refused while it does, and the claim fails. The retry the workflow sends
// next resumes the same session after that exited process — never with nothing to wait out, which
// on tmux would start a second agent on the session beside the first.
func TestARetryAfterAFailedRootExitAndRefusedResumesWaitsOutTheExitedProcess(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	exited := h.locator()
	h.rt.FailSuspend(errBoom)
	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})
	h.wantState(StateSuspended)
	still := errors.New("previous incarnation still running")
	h.rt.ScriptResume(fake.SpawnResult{Err: still}, fake.SpawnResult{Err: still}, fake.SpawnResult{Err: still})
	if err := h.handle(RequestResume{Claim: rootToken, By: SuspendedByOperator}); !errors.Is(err, still) {
		t.Fatalf("resume returned %v, want the runtime's refusal", err)
	}
	h.wantState(StateFailed)

	h.must(RequestRetry{Claim: rootToken})

	resumes := h.wantCalls("Resume", 4)
	for i, resume := range resumes {
		if resume.Previous == nil || *resume.Previous != exited || resume.Spec.ResumeSessionFile != sessionFile {
			t.Errorf("resume %d waited out %+v for %q, want the exited process %+v and the recorded session",
				i+1, resume.Previous, resume.Spec.ResumeSessionFile, exited)
		}
	}
	h.wantCalls("Spawn", 1)
}

// The issue's close is the one thing of the workflow's that releases its root: a root the daemon
// already suspended has no process, so the release carries no locator.
func TestIssueCloseReleasesASuspendedRootWithNoLocator(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateIdle)
	h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "tree waiting"})
	h.wantState(StateSuspended)

	h.must(RequestIssueClose{Claim: rootToken})

	wantReleasedWithNoLocator(t, h, rootToken)
	h.wantState(StateRetired)
}

// Its close releases the root from any state: whatever runs is stopped, and the claim retires.
func TestIssueCloseReleasesALiveRoot(t *testing.T) {
	for _, state := range liveStates {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			loc := h.locator()

			h.must(RequestIssueClose{Claim: rootToken})

			if released := h.wantCalls("Release", 1)[0].Released; released.Claim != rootToken || released.Locator == nil || *released.Locator != loc {
				t.Errorf("released %+v, want %s at %+v", released, rootToken, loc)
			}
			h.wantState(StateRetired)
		})
	}
}

// Nothing else retires the tree's root (N-v2-4): a retired root leaves the orphan sweep's known
// set, and under a sandbox the sweep would then delete its Sandbox and the root issue's volume with
// it, its workspace and sessions included. A stop that is not the tree's close — the operator's — is
// refused in every state and changes nothing. The refusal is ErrRootStop, whatever the state, and
// says what stops the root's process where one runs: suspend, once its agent has registered. What
// ends a tree no workflow issue backs is the operator's close, so its refusal names that too; a
// workflow issue's tree ends with its issue's close, and its refusal does not.
func TestAStopThatIsNotTheTreesCloseNeverRetiresItsRoot(t *testing.T) {
	const (
		refused   = "stop refused: the tree's root claim ends only when its tree closes; "
		closeIt   = "; no workflow issue backs its tree, so legion claims close ends it"
		suspendIt = refused + "suspend it to stop its process" + closeIt
		booting   = refused + "suspend it to stop its process once its agent has registered" + closeIt
		nothing   = refused + "no process of this claim is running" + closeIt
	)
	wantRefused := func(t *testing.T, h *harness, state ClaimState, want string) {
		t.Helper()
		before := h.claim()
		err := h.handle(RequestStop{Claim: rootToken, By: SuspendedByOperator})
		if !errors.Is(err, ErrRootStop) || err.Error() != want {
			t.Fatalf("stop of a root in %s returned %v, want ErrRootStop saying %q", state, err, want)
		}
		h.wantCalls("Release", 0)
		h.wantState(state)
		if after := h.claim(); !reflect.DeepEqual(after.Locator, before.Locator) || after.Generation != before.Generation {
			t.Errorf("claim %+v after the refused stop, want it unchanged from %+v", after, before)
		}
		if stored := h.store.load(rootToken); stored.State != state {
			t.Errorf("stored state %s, want %s", stored.State, state)
		}
	}
	for state, want := range map[ClaimState]string{
		StateQueued: nothing, StateSuspended: nothing,
		StateLaunching: booting, StateShimConnected: booting,
		StateRegistered: suspendIt, StateReady: suspendIt, StateWorking: suspendIt, StateIdle: suspendIt,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(state)
			wantRefused(t, h, state, want)
		})
	}
	t.Run(string(StateLaunchUncertain), func(t *testing.T) {
		uncertain := rootClaim()
		uncertain.State = StateLaunchUncertain
		h := newHarnessOf(t, uncertain)
		wantRefused(t, h, StateLaunchUncertain, nothing)
	})
	t.Run(string(StateFailed), func(t *testing.T) {
		h := newHarnessOf(t, rootClaim())
		h.reach(StateReady)
		for range 3 {
			h.observe(runtime.Gone)
		}
		h.wantState(StateFailed)
		wantRefused(t, h, StateFailed, nothing)
	})
	t.Run("a workflow issue's tree", func(t *testing.T) {
		h := newBareHarness(t)
		h.deps.TreeClosable = func(context.Context, Claim) (bool, error) { return false, nil }
		root := rootClaim()
		h.token = root.Token
		if err := h.store.PutClaim(h.ctx, root); err != nil {
			t.Fatal(err)
		}
		h.start(root)
		h.reach(StateReady)
		wantRefused(t, h, StateReady, refused+"suspend it to stop its process")
	})
	// The read that picks the refusal's last clause can fail. That failure is the stop's, the
	// request the operator made, and it changes nothing; no close was asked for.
	t.Run("an unreadable tree", func(t *testing.T) {
		h := newBareHarness(t)
		h.deps.TreeClosable = func(context.Context, Claim) (bool, error) { return false, errors.New("store down") }
		root := rootClaim()
		h.token = root.Token
		if err := h.store.PutClaim(h.ctx, root); err != nil {
			t.Fatal(err)
		}
		h.start(root)
		h.reach(StateReady)
		if err := h.handle(RequestStop{Claim: rootToken, By: SuspendedByOperator}); err == nil || err.Error() != "stop "+string(rootToken)+": store down" {
			t.Fatalf("stop with the tree unreadable returned %v, want the stop's own failure", err)
		}
		h.wantCalls("Release", 0)
		h.wantState(StateReady)
	})
}

// An operator's close of a gate holder's claim that a workflow issue backs is refused naming that
// issue, the one Deps.TreeClosable is asked about, never the tree the claim stores: a claim of an
// issue first recorded under another root stores that root's tree.
//
// Negative control, run when this was written: with the refusal naming the claim's stored tree,
// it reads "LEGION-100 is a workflow issue's tree".
func TestAWorkflowIssuesCloseRefusalNamesTheClaimsIssue(t *testing.T) {
	h := newBareHarness(t)
	h.deps.TreeClosable = func(context.Context, Claim) (bool, error) { return false, nil }
	root := rootClaim()
	root.Tree = "LEGION-100"
	h.token = root.Token
	if err := h.store.PutClaim(h.ctx, root); err != nil {
		t.Fatal(err)
	}
	h.start(root)
	h.reach(StateReady)
	const want = "LEGION-208 is a workflow issue's tree, which ends with its issue's close"
	var refusal *RefusedError
	if err := h.handle(RequestOperatorClose{Claim: rootToken}); !errors.As(err, &refusal) || refusal.Err.Error() != want {
		t.Fatalf("the close = %v, want refused: %s", err, want)
	}
	h.wantCalls("Release", 0)
	h.wantState(StateReady)
}

// Suspend is what stops the root's process short of its tree's close, so it is accepted as soon as
// the agent has registered: a registered agent holds a capability that mints grants and no timer
// runs on it, so a claim that could be neither stopped nor suspended there would keep that
// capability until its process died. The suspension revokes it and keeps the session to resume.
func TestARegisteredRootCanBeSuspendedAndResumed(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateRegistered)
	loc := h.locator()

	h.must(RequestSuspend{Claim: rootToken, By: SuspendedByOperator})

	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Errorf("suspended %+v, want the registered process %+v", suspend.Locator, loc)
	}
	h.wantState(StateSuspended)
	if stored := h.store.load(rootToken); stored.State != StateSuspended || stored.CapabilityHash != nil || stored.Session != session {
		t.Errorf("stored %+v, want it suspended with its capability revoked and its session kept", stored)
	}

	h.must(RequestResume{Claim: rootToken, By: SuspendedByOperator})
	if resume := h.wantCalls("Resume", 1)[0]; resume.Spec.ResumeSessionFile != sessionFile || resume.Previous == nil || *resume.Previous != loc {
		t.Errorf("resumed %+v, want the recorded session after the suspended process", resume)
	}
}

// The machine reads a gate holder's architect by the mark the outbox writes at each start, never by
// the tree the claim stores (R18 of dispatch://INFRA-3320): an architect stored in another issue's
// tree whose issue holds its own gate now — a member a change of parent made a root, or a root
// that keeps its gate under a parent in Legion's work — is that gate holder's, so its exit suspends
// it and a stop is refused; an architect stored as its own tree's whose issue the chain puts under
// another gate holder is no root's, so its exit ends it and a stop stops it.
func TestTheGateHoldersArchitectIsReadByItsMarkNotItsTree(t *testing.T) {
	moved := rootClaim()
	moved.Tree = "LEGION-100"
	t.Run("an architect stored in another tree, its issue holding its own gate", func(t *testing.T) {
		h := newHarnessOf(t, moved)
		h.reach(StateReady)
		h.must(RequestExit{Claim: rootToken, Generation: h.generation(), Session: session, Reason: "waiting"})
		h.wantState(StateSuspended)
		h.wantCalls("Release", 0)
		if err := h.handle(RequestStop{Claim: rootToken, By: SuspendedByOperator}); !errors.Is(err, ErrRootStop) {
			t.Fatalf("stop of a gate holder's architect stored in another tree = %v, want ErrRootStop", err)
		}
	})
	member := rootClaim()
	member.HoldsGate = false
	t.Run("an architect stored as its own tree's, its issue holding no gate of its own", func(t *testing.T) {
		h := newHarnessOf(t, member)
		h.reach(StateReady)
		if err := h.handle(RequestStop{Claim: rootToken, By: SuspendedByOperator}); err != nil {
			t.Fatalf("stop of an architect whose issue holds no gate of its own = %v, want it stopped", err)
		}
		if h.claim().State == StateSuspended {
			t.Fatalf("the stopped architect is %s, want it ended as any claim's stop ends it", h.claim().State)
		}
	})
	// The outbox's start marks a claim whose issue became a gate holder since its last launch
	// (Machine.MarkLaunch), and the mark persists with the claim, the gate root it stands under
	// with it.
	t.Run("an architect marked at its start", func(t *testing.T) {
		h := newHarnessOf(t, member)
		if err := h.m.MarkLaunch(h.ctx, member.Issue, true); err != nil {
			t.Fatalf("mark the launch: %v", err)
		}
		if stored := h.store.load(rootToken); !stored.HoldsGate || stored.LaunchTree != member.Issue {
			t.Fatalf("stored %+v, want the mark and the gate root persisted", stored)
		}
		h.reach(StateReady)
		if err := h.handle(RequestStop{Claim: rootToken, By: SuspendedByOperator}); !errors.Is(err, ErrRootStop) {
			t.Fatalf("stop of an architect its start marked a gate holder's = %v, want ErrRootStop", err)
		}
	})
}
