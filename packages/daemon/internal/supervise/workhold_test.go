package supervise

// The work hold's stop as a claim's machine meets it (decisions 14 to 16 of the work hold's design,
// rows M1 to M14 of its work-hold table, and its inventory test I2): the suspension source
// `hold`, the stop that waits for the agent's turn or cuts it at a stored deadline, the sources
// that take a held claim over and the ones it takes over, and its end (Machine.EndHold).

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

const (
	// holdGrace is the 2 minutes the close message gives a worker to end its turn (decision 14):
	// the outbox names the stop's deadline that far after the message reached the claim.
	holdGrace = 2 * time.Minute
	// phaseTask and laterTask are two tasks of the implementer's phase, the second one the
	// workflow gives while the first one's turn runs.
	phaseTask = "Implement the plan."
	laterTask = "Implement the review's changes."
	// holdOpening is the opening's sentences, which the one task the opening gives starts with;
	// resumeTask is the phase task the boot builds for a phase holder that kept no task of its own
	// (workflow's ResumePhaseTask).
	holdOpening = "The work hold stopped your turn. Version 4 of the spec document of LEGION-208 closed the design gate. " +
		"The design gate of LEGION-208 opened. Check what your turn already did: a tool call may not have finished."
	resumeTask = "Continue Implement LEGION-209. Issue: LEGION-209. Phase: implementing. Resume the existing phase work."
)

// mergerToken is the merger's claim of the harness's issue.
const mergerToken claim.Token = "legion-LEGION-209-merger"

// holdStop is the work hold's stop of h's claim as the outbox posts it: the message that asked
// the agent to end its turn reached the claim at after, and the stop is cut holdGrace later.
func holdStop(h *harness, after time.Time) RequestSuspend {
	return RequestSuspend{Claim: h.token, By: SuspendedByHold, Reason: "the design gate of LEGION-208 closed",
		TurnAfter: after, Deadline: after.Add(holdGrace)}
}

// directHoldStop is the hold's stop of a claim no message is sent to, a released worker that is
// not a merger: the close records the hold over its suspension at once, with no deadline.
func directHoldStop(h *harness) RequestSuspend {
	return RequestSuspend{Claim: h.token, By: SuspendedByHold, Reason: "the design gate of LEGION-208 closed"}
}

// openingStart is the start the opening writes for h's claim, as the outbox hands it to EndHold:
// the opening's sentences, then the phase task the boot builds.
func openingStart(h *harness) RequestDeliver {
	return RequestDeliver{Claim: h.token, Task: holdOpening + "\n\n" + resumeTask, ID: "outbox:12", Row: 12,
		Phase: phase.Implementing, Generation: 1}
}

// inPhaseTurn walks h's claim into the turn of its phase's task, an outbox row's.
func (h *harness) inPhaseTurn() {
	h.t.Helper()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: phaseTask, ID: "outbox:7", Row: 7, Phase: phase.Implementing, Generation: 1})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
}

// stoppedByTheHold walks h's claim to the work hold's suspension: the stop held for the turn of
// its phase task, a later task of the phase given meanwhile, and the turn's end.
func (h *harness) stoppedByTheHold() {
	h.t.Helper()
	h.inPhaseTurn()
	if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
		h.t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
	}
	h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantHeld()
}

// releasedWithATask walks h's claim to a release of its idle pod while it holds a phase task no
// turn has taken, which the release keeps.
func (h *harness) releasedWithATask() {
	h.t.Helper()
	h.reach(StateRegistered)
	h.must(RequestDeliver{Claim: h.token, Task: phaseTask, ID: "outbox:7", Row: 7, Phase: phase.Implementing, Generation: 1, Entry: 2})
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByReleased, Reason: "the idle pod was released"})
	h.wantState(StateSuspended)
	if !h.claim().Released() {
		h.t.Fatalf("the claim is %+v, want it released", h.claim())
	}
}

// wantHeld is h's claim suspended by the hold, in memory and in the store: a source no rule
// resumes, that is neither the operator's nor a release.
func (h *harness) wantHeld() {
	h.t.Helper()
	h.wantState(StateSuspended)
	c, stored := h.claim(), h.store.load(h.token)
	if c.SuspendedBy != SuspendedByHold || stored.SuspendedBy != SuspendedByHold || stored.State != StateSuspended {
		h.t.Fatalf("the claim is suspended by %q, stored %s by %q; want both suspended by the hold", c.SuspendedBy, stored.State, stored.SuspendedBy)
	}
	if !c.WorkHeld() || c.RuleMayResume() || c.RuleHolds() || c.Released() {
		h.t.Fatalf("the held claim reads WorkHeld %v, RuleMayResume %v, RuleHolds %v, Released %v; want held by the hold alone",
			c.WorkHeld(), c.RuleMayResume(), c.RuleHolds(), c.Released())
	}
}

// wantPendingTask is the claim holding text as its task, unconfirmed, for the implementer's phase.
func (h *harness) wantPendingTask(text string) Delivery {
	h.t.Helper()
	p := h.claim().Pending
	if p == nil || p.Task != text || !p.ConfirmedAt.IsZero() || p.Phase != phase.Implementing {
		h.t.Fatalf("the claim holds %+v, want the unconfirmed phase task %q", p, text)
	}
	if stored := h.store.load(h.token).Pending; stored == nil || stored.ID != p.ID || stored.Task != text {
		h.t.Fatalf("the store holds %+v, want the claim's task %q under %s", stored, text, p.ID)
	}
	return *p
}

// wantSession is the claim keeping the session its stop ended.
func (h *harness) wantSession() {
	h.t.Helper()
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		h.t.Fatalf("the claim records session %q at %q, want %q at %q kept", c.Session, c.SessionFile, session, sessionFile)
	}
}

// M1: a claim in a turn when the close message reaches it is stopped when that turn ends, its
// session kept, as the hold's: a source no rule resumes. The task the workflow gave while the
// stop waited is kept with the claim, unsent, as an operator's suspension keeps one and a
// daemon's retires it (decision 16: the hold keeps the phase task).
func TestWorkHoldStopsAClaimWhenItsTurnEnds(t *testing.T) {
	h := newHarness(t)
	h.inPhaseTurn()
	loc := h.locator()

	if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
	}
	if c := h.claim(); !c.WorkHeld() || c.SuspensionHeldBy != SuspendedByHold || c.RuleHolds() {
		t.Fatalf("the claim reads WorkHeld %v, held by %q, RuleHolds %v; want the hold's stop held", c.WorkHeld(), c.SuspensionHeldBy, c.RuleHolds())
	}
	h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
	h.advance(time.Minute)
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)

	h.must(StreamTurnEnd{Claim: h.token})

	h.wantHeld()
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the process whose turn ended, %+v", suspend.Locator, loc)
	}
	h.wantSession()
	h.wantPendingTask(laterTask)
	h.wantPrompts(1)
	h.advance(holdGrace)
	h.wantCalls("Suspend", 1)
}

// M2: a turn still running at the stop's deadline is cut there: the process is stopped mid-turn,
// its session kept, and the phase task it was working is taken back interrupted and kept for the
// opening, which tells the worker the hold ended its turn.
func TestWorkHoldCutsATurnStillRunningAtItsDeadline(t *testing.T) {
	h := newHarness(t)
	h.inPhaseTurn()

	if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
	}
	h.advance(holdGrace - time.Second)
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)

	h.advance(time.Second)

	h.wantHeld()
	h.wantCalls("Suspend", 1)
	h.wantSession()
	if p := h.wantPendingTask(phaseTask); !p.Interrupted {
		t.Fatalf("the kept task %+v, want it taken back from the turn the deadline cut", p)
	}
	if cause := h.claim().EndedWork.Cause; cause != endSuspended(SuspendedByHold) {
		t.Fatalf("the cut is recorded as %q, want %q", cause, endSuspended(SuspendedByHold))
	}
}

// M3: an idle claim is not stopped at once. The close message starts a turn, and the stop waits
// for that turn to end; with no turn by the deadline, the claim is stopped there. A task given
// while the stop waits is held, not sent. A stop that reaches the claim after the turn its
// message started has already ended stops the claim at once.
func TestWorkHoldStopsAnIdleClaimAfterTheTurnItsMessageStarts(t *testing.T) {
	t.Run("when the turn its message starts ends", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of an idle claim answered %v, want it held for the turn its message starts", err)
		}
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
		h.wantPrompts(1)
		h.advance(30 * time.Second)

		h.must(StreamTurnStart{Claim: h.token})
		h.wantState(StateWorking)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		h.wantPendingTask(laterTask)
		h.wantPrompts(1)
	})
	t.Run("at its deadline when no turn starts", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of an idle claim answered %v, want it held for the turn its message starts", err)
		}
		h.advance(holdGrace - time.Second)
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)

		h.advance(time.Second)

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		h.wantSession()
	})
	t.Run("at once when the turn its message started has ended", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		reached := h.clock.Now()
		h.advance(10 * time.Second)
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)

		h.must(holdStop(h, reached))

		h.wantHeld()
		h.wantCalls("Suspend", 1)
	})
}

// M4: a released merger the daemon woke for the close message is stopped as an idle claim is,
// its 2 minutes counted from its start: not at its ready, which stops a launch the hold meets
// before the message (M12), and not at 2 minutes after the close, long past by the time its pod
// came back, but when the turn the message starts ends, or 2 minutes after the merger came up.
func TestWorkHoldCountsAWokenMergersDeadlineFromItsStart(t *testing.T) {
	c := queuedClaim()
	c.Token, c.Role = mergerToken, claim.RoleMerger
	h := newHarnessOf(t, c)
	suspendedBy(h, SuspendedByReleased)
	closed := h.clock.Now()
	h.advance(10 * time.Minute)

	h.must(RequestRuleResume{Claim: h.token, Reason: "a wake for the close message of LEGION-208"})
	h.relaunched()
	started := h.clock.Now()
	if started.Sub(closed) <= holdGrace {
		t.Fatalf("the merger came up %s after the close, want it past the close's own 2 minutes", started.Sub(closed))
	}
	if err := h.handle(holdStop(h, started)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the hold's stop of the woken merger answered %v, want it held for the turn its message starts", err)
	}
	h.wantState(StateReady)
	h.wantCalls("Suspend", 1)

	h.must(StreamTurnStart{Claim: h.token})
	h.advance(holdGrace - time.Second)
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 1)

	h.advance(time.Second)

	h.wantHeld()
	h.wantCalls("Suspend", 2)
	if got := h.claim().Role; got != claim.RoleMerger {
		t.Fatalf("the held claim is the %s's, want the merger's", got)
	}
}

// M5: the deadline is stored in the stop the outbox posts, which it posts again after a restart
// until the claim answers nil, so a restart keeps it as named: the re-posted stop cuts the turn
// at the deadline the first one named, a re-post after that deadline cuts it at once, and an
// idle claim whose message's turn ended while the hold was forgotten is stopped by the re-post.
func TestWorkHoldDeadlineSurvivesARestart(t *testing.T) {
	t.Run("a turn still running", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		stop := holdStop(h, h.clock.Now())
		if err := h.handle(stop); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		h.advance(time.Minute)

		h.restart()
		h.wantState(StateWorking)
		if err := h.handle(stop); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the stop posted again after the restart answered %v, want it held", err)
		}
		h.advance(holdGrace - time.Minute - time.Second)
		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)

		h.advance(time.Second)

		h.wantHeld()
		h.wantCalls("Suspend", 1)
	})
	t.Run("a re-post past the deadline", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		stop := holdStop(h, h.clock.Now())
		if err := h.handle(stop); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		h.advance(time.Minute)
		h.restart()
		h.advance(holdGrace)
		h.wantState(StateWorking)

		if err := h.handle(stop); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the stop posted again past its deadline answered %v, want it held for its deadline, which is now", err)
		}
		h.advance(0)

		h.wantHeld()
		h.wantCalls("Suspend", 1)
	})
	t.Run("an idle claim whose message's turn ended meanwhile", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		stop := holdStop(h, h.clock.Now())
		if err := h.handle(stop); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held for the turn its message starts", err)
		}
		h.restart()
		h.advance(10 * time.Second)
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.restart()

		h.must(stop)

		h.wantHeld()
		h.wantCalls("Suspend", 1)
	})
}

// M6 (I2): every request the machine lists as one that can start or resume a turn of its agent
// (Event.door) waits on a claim the hold stopped, but an operator's own act on that claim: the
// operator's resume, which the operator route and fresh_session post, and the operator's retry.
// The list is the machine's own: every event type declares its door, the compiler holds each type
// to it, and samples holds this test to every type the package declares, so a type added later is
// in the list or fails to compile. The workflow's own task, an outbox row's, waits as the
// operator's own task does.
func TestWorkHoldHoldsEveryRequestThatCouldStartATurn(t *testing.T) {
	doors := map[string]turnDoor{}
	all := samples(t)
	for name, events := range all {
		for _, ev := range events {
			if door := ev.door(); door != noDoor {
				doors[name] = door
			}
		}
	}
	for _, name := range []string{"RequestSpawn", "RequestReady", "RequestResume", "RequestRuleResume", "RequestRetry",
		"RequestOperatorRetry", "RequestDeliver", "RequestCompact", "ResumePause"} {
		if _, listed := doors[name]; !listed {
			t.Errorf("%s declares that it starts no turn, want it among the requests that can start or resume one", name)
		}
	}
	for _, name := range []string{"RequestResume", "RequestOperatorRetry"} {
		if doors[name] != ownerDoor {
			t.Errorf("%s is not an operator's own door, want it to start a held claim", name)
		}
	}
	type door struct {
		name string
		ev   func(c Claim) Event
		door turnDoor
	}
	var list []door
	for _, name := range slices.Sorted(maps.Keys(doors)) {
		sample := all[name][0]
		list = append(list, door{name: name, ev: func(c Claim) Event { return fenced(sample, c) }, door: doors[name]})
	}
	list = append(list, door{name: "RequestDeliver of an outbox row", door: ruleDoor, ev: func(c Claim) Event {
		return RequestDeliver{Claim: c.Token, Task: "A CI settlement: the checks of the branch passed.", ID: "outbox:9", Row: 9,
			Phase: phase.Implementing, Generation: 1}
	}})
	for _, d := range list {
		t.Run(d.name, func(t *testing.T) {
			h := newHarness(t)
			h.stoppedByTheHold()
			calls, prompts, pending := len(h.rt.Methods()), len(h.prompts()), h.pending()

			err := h.handle(d.ev(h.claim()))

			if d.door == ownerDoor {
				if err != nil {
					t.Fatalf("the operator's own %s answered %v, want the held claim started", d.name, err)
				}
				h.wantState(StateLaunching)
				h.wantCalls("Resume", 1)
				if c := h.claim(); c.WorkHeld() || c.SuspendedBy != "" {
					t.Fatalf("the started claim reads WorkHeld %v, suspended by %q; want the hold gone", c.WorkHeld(), c.SuspendedBy)
				}
				return
			}
			if !errors.Is(err, ErrHeldByWorkHold) || !errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("%s answered %v, want the work hold's wait", d.name, err)
			}
			h.wantHeld()
			if got := h.rt.Methods(); len(got) != calls {
				t.Fatalf("%s asked the runtime %v, want nothing", d.name, got[calls:])
			}
			if got := len(h.prompts()); got != prompts {
				t.Fatalf("%s sent %d prompts, want none", d.name, got-prompts)
			}
			if got := h.pending(); got.ID != pending.ID || got.Task != pending.Task {
				t.Fatalf("%s left the claim holding %+v, want the task it kept, %+v", d.name, got, pending)
			}
		})
	}
}

// M7: an operator's suspension takes a held claim over, whether the hold's stop still waits for
// the agent's turn, has stopped the claim, or waits for a launch's ready; from then on the claim is
// the operator's, as RuleHolds says.
func TestWorkHoldGivesWayToAnOperatorsSuspension(t *testing.T) {
	operator := func(h *harness) RequestSuspend {
		return RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator suspended it"}
	}
	wantOperators := func(t *testing.T, h *harness) {
		t.Helper()
		h.wantState(StateSuspended)
		c := h.claim()
		if c.SuspendedBy != SuspendedByOperator || h.store.load(h.token).SuspendedBy != SuspendedByOperator || !c.RuleHolds() || c.WorkHeld() {
			t.Fatalf("the claim is suspended by %q (stored %q), RuleHolds %v, WorkHeld %v; want the operator's",
				c.SuspendedBy, h.store.load(h.token).SuspendedBy, c.RuleHolds(), c.WorkHeld())
		}
	}
	t.Run("a stop held for the agent's turn", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		if err := h.handle(operator(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend answered %v, want it held", err)
		}
		if c := h.claim(); c.SuspensionHeldBy != SuspendedByOperator || !c.RuleHolds() || c.WorkHeld() {
			t.Fatalf("the held stop is %q's, RuleHolds %v, WorkHeld %v; want the operator's", c.SuspensionHeldBy, c.RuleHolds(), c.WorkHeld())
		}
		h.advance(testStop - time.Second)
		h.wantState(StateWorking)

		h.advance(time.Second)

		wantOperators(t, h)
	})
	t.Run("a claim the hold stopped", func(t *testing.T) {
		h := newHarness(t)
		h.stoppedByTheHold()

		h.must(operator(h))

		wantOperators(t, h)
		h.wantPendingTask(laterTask)
		h.must(holdStop(h, h.clock.Now()))
		wantOperators(t, h)
	})
	t.Run("a launch held until its ready", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByDaemon)
		h.must(RequestRuleResume{Claim: h.token, Reason: "it owns its issue's phase"})
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of a launch answered %v, want it held until the launch is ready", err)
		}
		if err := h.handle(operator(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend of the launch answered %v, want it held", err)
		}
		h.connect()
		h.register()
		h.ready()

		wantOperators(t, h)
	})
}

// M7: the hold's end leaves an operator's suspension alone: a claim the operator took over from
// the hold is not started by the opening, and the operator's stop held for a turn still runs.
func TestWorkHoldReleaseLeavesAnOperatorsSuspension(t *testing.T) {
	t.Run("a claim the operator took over", func(t *testing.T) {
		h := newHarness(t)
		h.stoppedByTheHold()
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator suspended it"})
		start := openingStart(h)

		resumed, err := h.m.EndHold(h.ctx, holdOpening, &start)

		if resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}
		h.wantState(StateSuspended)
		if got := h.claim().SuspendedBy; got != SuspendedByOperator {
			t.Fatalf("the claim is suspended by %q, want the operator still", got)
		}
		h.wantCalls("Resume", 0)
		h.wantPendingTask(laterTask)
	})
	t.Run("the operator's stop held for a turn", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator suspended it"}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend answered %v, want it held", err)
		}

		if resumed, err := h.m.EndHold(h.ctx, holdOpening, nil); resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}
		if c := h.claim(); c.SuspensionHeldBy != SuspendedByOperator {
			t.Fatalf("the held stop is %q's, want the operator's kept", c.SuspensionHeldBy)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateSuspended)
		if got := h.claim().SuspendedBy; got != SuspendedByOperator {
			t.Fatalf("the claim is suspended by %q, want the operator", got)
		}
	})
}

// M7: an operator's own resume or retry starts a held claim (spec step 5), and so does the
// fresh session's resume, which starts it in a new session; the claim is handed the task it kept.
func TestWorkHoldStartsOnTheOperatorsResumeOrRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(h *harness)
		calls string
	}{
		{"the operator's resume", func(h *harness) { h.must(RequestResume{Claim: h.token}) }, "Resume"},
		{"the operator's retry", func(h *harness) { h.must(RequestOperatorRetry{Claim: h.token}) }, "Resume"},
		{"the fresh session's resume", func(h *harness) {
			h.must(RequestForgetSession{Claim: h.token, Session: session})
			h.must(RequestResume{Claim: h.token})
		}, "Spawn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.stoppedByTheHold()
			spawns := len(h.calls("Spawn"))

			tc.start(h)

			h.wantState(StateLaunching)
			if tc.calls == "Resume" {
				if resume := h.wantCalls("Resume", 1)[0]; resume.Spec.ResumeSessionFile != sessionFile {
					t.Fatalf("the resume names session file %q, want the held claim's %q", resume.Spec.ResumeSessionFile, sessionFile)
				}
			} else if got := len(h.calls("Spawn")); got != spawns+1 {
				t.Fatalf("spawns = %d, want one more for the fresh session", got-spawns)
			}
			if c := h.claim(); c.WorkHeld() || c.SuspendedBy != "" {
				t.Fatalf("the started claim reads WorkHeld %v, suspended by %q; want the hold gone", c.WorkHeld(), c.SuspendedBy)
			}
			sent := len(h.prompts())
			h.connect()
			h.register()
			h.ready()
			if prompts := h.wantPrompts(sent + 1); !strings.HasSuffix(prompts[sent].Message, laterTask) {
				t.Fatalf("the started claim was sent %q, want the task it kept", prompts[sent].Message)
			}
		})
	}
}

// M8: the daemon's own suspension (a close, a linger, a park) takes a held claim over: the claim
// is the daemon's from then on, and the daemon's suspension ends its phase, so the task the hold
// kept goes with it.
func TestWorkHoldGivesWayToADaemonSuspension(t *testing.T) {
	daemon := func(h *harness) RequestSuspend {
		return RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "LEGION-209 left the workflow"}
	}
	wantDaemons := func(t *testing.T, h *harness) {
		t.Helper()
		h.wantState(StateSuspended)
		c := h.claim()
		if c.SuspendedBy != SuspendedByDaemon || h.store.load(h.token).SuspendedBy != SuspendedByDaemon || c.WorkHeld() || !c.RuleMayResume() {
			t.Fatalf("the claim is suspended by %q (stored %q), WorkHeld %v, RuleMayResume %v; want the daemon's",
				c.SuspendedBy, h.store.load(h.token).SuspendedBy, c.WorkHeld(), c.RuleMayResume())
		}
		if p := c.Pending; p != nil {
			t.Fatalf("the daemon's suspension left the claim holding %+v, want its phase task retired with the phase", p)
		}
		if resumed, err := h.m.EndHold(h.ctx, holdOpening, nil); resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}
	}
	t.Run("a stop held for the agent's turn", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		if err := h.handle(daemon(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspend answered %v, want it held", err)
		}
		if c := h.claim(); c.SuspensionHeldBy != SuspendedByDaemon || c.WorkHeld() {
			t.Fatalf("the held stop is %q's, WorkHeld %v; want the daemon's", c.SuspensionHeldBy, c.WorkHeld())
		}
		h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})

		h.must(StreamTurnEnd{Claim: h.token})

		wantDaemons(t, h)
	})
	t.Run("a claim the hold stopped", func(t *testing.T) {
		h := newHarness(t)
		h.stoppedByTheHold()

		h.must(daemon(h))

		wantDaemons(t, h)
		h.must(holdStop(h, h.clock.Now()))
		if got := h.claim().SuspendedBy; got != SuspendedByDaemon {
			t.Fatalf("the hold's stop over the daemon's suspension left it %q's, want the daemon's", got)
		}
	})
	t.Run("a launch held until its ready", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByReleased)
		h.must(RequestRuleResume{Claim: h.token, Reason: "a wake for its role"})
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of a launch answered %v, want it held until the launch is ready", err)
		}
		if err := h.handle(daemon(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspend of the launch answered %v, want it held", err)
		}
		h.connect()
		h.register()
		h.ready()

		wantDaemons(t, h)
	})
}

// M9: the hold takes a released claim over, keeping its bundle's claim and its pending task: the
// claim is no longer released, so no wake reads it as one, and a wake's resume, a CI settlement's
// task and the idle release's own stop each leave it stopped. A release's stop held for a turn is
// taken over the same way.
func TestWorkHoldTakesAReleasedClaimOverAndNoWakeResumesIt(t *testing.T) {
	t.Run("a released claim", func(t *testing.T) {
		h := newHarness(t)
		h.releasedWithATask()

		h.must(directHoldStop(h))

		h.wantHeld()
		h.wantPendingTask(phaseTask)
		resumes := len(h.calls("Resume"))
		if err := h.handle(RequestRuleResume{Claim: h.token, Reason: "a wake for its role"}); !errors.Is(err, ErrHeldByWorkHold) {
			t.Fatalf("the wake's resume answered %v, want the work hold's wait", err)
		}
		if err := h.handle(RequestDeliver{Claim: h.token, Task: "A CI settlement: the checks passed.", ID: "outbox:9", Row: 9,
			Phase: phase.Implementing, Generation: 1}); !errors.Is(err, ErrHeldByWorkHold) {
			t.Fatalf("the CI settlement's task answered %v, want the work hold's wait", err)
		}
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByReleased, Reason: "the idle pod was released"})
		h.wantHeld()
		h.wantPendingTask(phaseTask)
		if got := len(h.calls("Resume")); got != resumes {
			t.Fatalf("the held claim was resumed %d times, want none", got-resumes)
		}
	})
	t.Run("a release's stop held for the agent's turn", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByReleased, Reason: "the idle pod was released"}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the release's stop answered %v, want it held", err)
		}
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		if c := h.claim(); c.SuspensionHeldBy != SuspendedByHold {
			t.Fatalf("the held stop is %q's, want the hold's", c.SuspensionHeldBy)
		}

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantHeld()
	})
}

// M10: the hold takes over a claim the project's pause holds: a pause's stop held for a turn is
// the hold's from then on, run at the turn's end through the plain claim write, and the pause's
// end does not drop it; a claim the pause stopped is the hold's, so neither the pause's resume nor
// the resume of a launch it owed starts it. An opening while the project is still paused starts
// nothing: the pause's gate stops the claim for the pause with its launch owed, and the pause's
// resume then starts it with the opening's task.
func TestWorkHoldTakesAPauseHeldClaimOver(t *testing.T) {
	t.Run("a pause's stop held for the agent's turn", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.inPhaseTurn()
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop answered %v, want it held", err)
		}
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		if pause, held := h.heldPause(); !held || pause != 0 {
			t.Fatalf("the held stop names pause %d (held %v), want the hold's, which names none", pause, held)
		}
		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})
		if c := h.claim(); c.SuspensionHeldBy != SuspendedByHold {
			t.Fatalf("after the pause's resume the held stop is %q's, want the hold's kept", c.SuspensionHeldBy)
		}

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantHeld()
		stops.wantStops(t)
	})
	t.Run("a claim the pause stopped", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateIdle)
		flag.set(7)
		h.must(pauseStop(h.token, 7))
		h.wantState(StateSuspended)
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7}, landed: true})

		h.must(directHoldStop(h))

		h.wantHeld()
		flag.set(0)
		if err := h.handle(ResumePause{Claim: h.token, Pause: 7}); !errors.Is(err, ErrHeldByWorkHold) {
			t.Fatalf("the pause's resume answered %v, want the work hold's wait", err)
		}
		if err := h.handle(RequestRuleResume{Claim: h.token, Reason: "the project's resume: pause 7 stopped it with a launch owed"}); !errors.Is(err, ErrHeldByWorkHold) {
			t.Fatalf("the resume of the launch the pause owed answered %v, want the work hold's wait", err)
		}
		h.wantHeld()
		h.wantCalls("Resume", 0)
	})
	t.Run("an opening while the project is paused", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.stoppedByTheHold()
		flag.set(7)
		start := openingStart(h)

		if resumed, err := h.m.EndHold(h.ctx, holdOpening, &start); !resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
		}
		h.advance(0)

		h.wantState(StateSuspended)
		h.wantCalls("Resume", 0)
		if got := h.claim().SuspendedBy; got != SuspendedByReleased {
			t.Fatalf("the claim is suspended by %q, want the pause's (released)", got)
		}
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
		opening := holdOpening + "\n\n" + laterTask
		h.wantPendingTask(opening)

		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})
		h.must(RequestRuleResume{Claim: h.token, Reason: "the project's resume: pause 7 stopped it with a launch owed"})
		sent := len(h.prompts())
		h.relaunched()
		if prompts := h.wantPrompts(sent + 1); !strings.HasSuffix(prompts[sent].Message, opening) {
			t.Fatalf("the claim was sent %q after the pause's resume, want the opening's task", prompts[sent].Message)
		}
	})
}

// M11: a claim in a compaction of its session is stopped when the compaction ends, or at the
// deadline if it is still compacting then.
func TestWorkHoldWaitsForACompactionToEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		land   func(h *harness, release func())
		answer error
	}{
		{name: "the compaction's end", land: func(_ *harness, release func()) { release() }, answer: ErrCompactSuspended},
		{name: "the deadline", land: func(h *harness, release func()) {
			h.advance(holdGrace)
			release()
		}, answer: ErrCompactProcessGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			release, compacted := h.compactHeld()
			if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the hold's stop during a compaction answered %v, want it held for the compaction's end", err)
			}
			h.advance(holdGrace - time.Second)
			h.wantState(StateWorking)
			h.wantCalls("Suspend", 0)

			tc.land(h, release)

			if err := <-compacted; !errors.Is(err, tc.answer) {
				t.Fatalf("the compaction answered %v, want %v", err, tc.answer)
			}
			h.m.Wait()
			h.wantHeld()
			h.wantCalls("Suspend", 1)
		})
	}
}

// M12: a launch that waits its paced turn is stopped before any task: a wake still waiting over
// no process is stopped where the stop arrives, and a launch the pacing has let out is stopped
// at its agent's ready. The task the claim keeps is not sent either way.
func TestWorkHoldStopsAPacedLaunchAtItsReady(t *testing.T) {
	paced := func(t *testing.T) *harness {
		t.Helper()
		h := newBareHarness(t)
		pacedDeps(wakeRate)(&h.deps)
		c := queuedClaim()
		c.State, c.Generation, c.SuspendedBy = StateSuspended, 1, SuspendedByReleased
		c.Session, c.SessionFile = session, sessionFile
		kept := Delivery{ID: "outbox:7", Task: phaseTask, Phase: phase.Implementing, Generation: 1, QueuedAt: epoch}
		c.Pending = &kept
		if err := h.store.PutClaim(h.ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := h.store.PutDelivery(h.ctx, c.Token, kept); err != nil {
			t.Fatal(err)
		}
		h.start(c)
		first := h.machineFor(wakeClaim(secondToken, StateQueued))
		h.mustPostTo(first, RequestSpawn{Claim: secondToken})
		h.must(RequestRuleResume{Claim: h.token, Reason: "a wake for its role"})
		h.wantState(StateLaunching)
		h.wantWaitingWake(WakeLaunch)
		h.wantStarts(h.token, 0)
		return h
	}
	t.Run("a wake waiting its turn over no process", func(t *testing.T) {
		h := paced(t)

		h.must(holdStop(h, h.clock.Now()))

		h.wantHeld()
		h.advance(2*wakeInterval + 2*wakePart)
		h.wantStarts(h.token, 0)
		h.wantPendingTask(phaseTask)
		h.wantPrompts(0)
	})
	t.Run("a launch the pacing let out", func(t *testing.T) {
		h := paced(t)
		h.advance(wakeInterval + wakePart)
		h.wantStarts(h.token, 1)
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of a launch answered %v, want it held until the launch is ready", err)
		}

		h.connect()
		h.register()
		h.ready()

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		h.wantPendingTask(phaseTask)
		h.wantPrompts(0)
	})
}

// M13: the hold's end starts a claim it stopped with one task, which starts with the opening's
// sentences. A claim that kept a task of its own gets that task's work behind them, in place of
// the kept delivery and under the opening start's id, so the start that carried it queues nothing
// more; a claim that kept none, or whose start carries newer work, gets the start's own task. A
// stop not yet made when the hold ends is not made, and a task it held back is sent.
func TestWorkHoldReleaseStartsTheClaimWithItsKeptTask(t *testing.T) {
	started := func(t *testing.T, h *harness, start RequestDeliver, want string) {
		t.Helper()
		resumed, err := h.m.EndHold(h.ctx, holdOpening, &start)
		if !resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
		}
		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		if c := h.claim(); c.WorkHeld() {
			t.Fatal("the started claim still reads held by the hold")
		}
		if p := h.wantPendingTask(want); p.ID != start.ID {
			t.Fatalf("the one task is under %s, want the opening start's %s", p.ID, start.ID)
		}
		// The start goes on as an ordinary start once the hold has ended, and its own delivery
		// queues nothing more.
		if err := h.m.StartedBy(h.ctx, start.Row, &start); err != nil {
			t.Fatalf("the opening's start: %v", err)
		}
		h.must(start)
		h.wantPendingTask(want)
		sent := len(h.prompts())
		h.connect()
		h.register()
		h.ready()
		if prompts := h.wantPrompts(sent + 1); !strings.HasSuffix(prompts[sent].Message, want) {
			t.Fatalf("the started claim was sent %q, want the one task %q", prompts[sent].Message, want)
		}
	}
	t.Run("a released worker's kept task", func(t *testing.T) {
		h := newHarness(t)
		h.releasedWithATask()
		h.must(directHoldStop(h))

		started(t, h, openingStart(h), holdOpening+"\n\n"+phaseTask)

		if log := h.store.deliveryLog(); !slices.Contains(log, "retire outbox:7") {
			t.Fatalf("the delivery log %v, want the kept delivery retired in the one task's place", log)
		}
	})
	t.Run("a task kept from the turn the hold stopped", func(t *testing.T) {
		h := newHarness(t)
		h.stoppedByTheHold()

		started(t, h, openingStart(h), holdOpening+"\n\n"+laterTask)
	})
	t.Run("a claim that kept no task", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantHeld()
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the stopped claim holds %+v, want the task of its finished turn retired", p)
		}
		start := openingStart(h)

		started(t, h, start, start.Task)
	})
	t.Run("a start of a newer entry", func(t *testing.T) {
		h := newHarness(t)
		h.releasedWithATask()
		h.must(directHoldStop(h))
		start := openingStart(h)
		start.Task, start.Entry = holdOpening+"\n\nImplement entry 3 of LEGION-209.", 3

		started(t, h, start, start.Task)
	})
	t.Run("a stop not yet made is not made", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}

		if resumed, err := h.m.EndHold(h.ctx, holdOpening, nil); resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}

		if c := h.claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the claim reads WorkHeld %v, a stop held %v; want the stop dropped", c.WorkHeld(), c.SuspensionHeld)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.advance(holdGrace)
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
	})
	t.Run("a task held back by a stop not yet made is sent", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}
		h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
		h.wantPrompts(1)

		if resumed, err := h.m.EndHold(h.ctx, holdOpening, nil); resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}
		h.m.Wait()

		if prompts := h.wantPrompts(2); prompts[1].Message != laterTask {
			t.Fatalf("the claim was sent %q, want the task the stop held back", prompts[1].Message)
		}
	})
}

// M14: a Claude Code worker is stopped and started as an Oh My Pi worker is: at its turn's end,
// its session and its file kept, and resumed in that session at the hold's end with its task.
func TestWorkHoldStopsAndStartsAClaudeCodeWorker(t *testing.T) {
	h := newBareHarness(t)
	h.deps.Specs = claudeSpecs{h.specs}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.stoppedByTheHold()
	h.wantSession()
	start := openingStart(h)

	if resumed, err := h.m.EndHold(h.ctx, holdOpening, &start); !resumed || err != nil {
		t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
	}

	resume := h.wantCalls("Resume", 1)[0]
	if runtime.Of(resume.Spec.Backend) != runtime.BackendClaude || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("the start is on %q resuming %q, want Claude Code resuming the held session's file %q",
			resume.Spec.Backend, resume.Spec.ResumeSessionFile, sessionFile)
	}
	sent := len(h.prompts())
	h.connect()
	h.register()
	h.ready()
	opening := holdOpening + "\n\n" + laterTask
	if prompts := h.wantPrompts(sent + 1); !strings.HasSuffix(prompts[sent].Message, opening) {
		t.Fatalf("the Claude Code worker was sent %q, want the opening's task", prompts[sent].Message)
	}
}

// The hold's end asks of a claim the hold stopped what a resume asks of a suspended claim
// (resumeRefusal): one recording a session another claim holds is refused its start before
// anything of it changes — a RefusedError naming the forget-session to run, nothing launched or
// charged, and the claim left stopped by the hold with the task it kept — rather than launched
// into the session hold's refusal at start, which charges each try to its launch budget until the
// claim fails. Settled for this claim, the hold's end starts it with its one task.
func TestWorkHoldEndRefusesAClaimRecordingASessionItDoesNotHold(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	h := newHarness(t)
	h.stoppedByTheHold()
	h.store.setHold(sessionID(session), other)
	h.restart()
	resumes := len(h.calls("Resume"))
	start := openingStart(h)

	resumed, err := h.m.EndHold(h.ctx, holdOpening, &start)
	var refused *RefusedError
	if resumed || !errors.As(err, &refused) {
		t.Fatalf("the hold's end of a claim recording %s, which %s holds = %v, %v; want a refusal", session, other, resumed, err)
	}
	if want := "legion claims forget-session --claim " + string(h.token); !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal %q does not name %q", err, want)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
		t.Fatalf("the runtime was asked to resume %d times, want none", len(calls))
	}
	h.wantHeld()
	if c := h.claim(); c.Budgets.LaunchFailures != 0 || c.Session != session {
		t.Fatalf("the refused claim has %d launch failures and records %q, want none and %s", c.Budgets.LaunchFailures, c.Session, session)
	}
	h.wantPendingTask(laterTask)

	h.store.setHold(sessionID(session), h.token)
	if resumed, err := h.m.EndHold(h.ctx, holdOpening, &start); !resumed || err != nil {
		t.Fatalf("the hold's end of a claim holding its session = %v, %v; want the claim started", resumed, err)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 1 {
		t.Fatalf("the hold's end asked the runtime to resume %d times, want once", len(calls))
	}
	h.wantPendingTask(holdOpening + "\n\n" + laterTask)
}

// The hold's stop meeting a tree that no longer holds work is decided in one call of the claim's
// machine (DropHoldStop), so the outbox acts on what the claim is when it asks: a stop held for the
// agent's turn is dropped and the turn goes on; a claim the stop already stopped — its turn ended
// before the outbox's row asked — is left stopped, with the task it kept and nothing launched, for
// the hold's opening to start with the opening's sentences; and a claim the hold never reached is
// left as it is.
func TestWorkHoldStopDropLeavesAStoppedClaimForTheOpening(t *testing.T) {
	t.Run("a stop held for the turn", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop answered %v, want it held", err)
		}

		if dropped, stopped, err := h.m.DropHoldStop(h.ctx); !dropped || stopped || err != nil {
			t.Fatalf("the stop's drop = dropped %v, stopped %v, %v; want the held stop dropped", dropped, stopped, err)
		}

		if c := h.claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the claim reads WorkHeld %v, a stop held %v; want the stop dropped", c.WorkHeld(), c.SuspensionHeld)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.advance(holdGrace)
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
	})
	t.Run("a claim the stop already stopped", func(t *testing.T) {
		h := newHarness(t)
		h.stoppedByTheHold()
		resumes := len(h.calls("Resume"))

		if dropped, stopped, err := h.m.DropHoldStop(h.ctx); dropped || !stopped || err != nil {
			t.Fatalf("the stop's drop = dropped %v, stopped %v, %v; want the stopped claim reported and left", dropped, stopped, err)
		}

		h.wantHeld()
		h.wantPendingTask(laterTask)
		if calls := h.calls("Resume")[resumes:]; len(calls) != 0 {
			t.Fatalf("the runtime was asked to resume %d times, want none: the opening starts the claim with its sentences", len(calls))
		}
	})
	t.Run("a claim the hold never reached", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)

		if dropped, stopped, err := h.m.DropHoldStop(h.ctx); dropped || stopped || err != nil {
			t.Fatalf("the stop's drop = dropped %v, stopped %v, %v; want nothing", dropped, stopped, err)
		}

		h.wantState(StateIdle)
	})
}
