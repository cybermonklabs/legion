package supervise

// The work hold's stop as a claim's machine meets it (decisions 14 to 16 of the work hold's design,
// rows M1 to M14 of its work-hold table, and its inventory test I2): the suspension source
// `hold`, the stop that waits for the agent's turn or cuts it at a stored deadline, the sources
// that take a held claim over and the ones it takes over, and its end (Machine.EndHold).

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
	// (workflow's Engine.ResumeTask, as it reads for an issue with no plan rows).
	holdOpening = "The work hold stopped your turn. Version 4 of the spec document of LEGION-208 closed the design gate. " +
		"The design gate of LEGION-208 opened. Check what your turn already did: a tool call may not have finished."
	resumeTask = "Continue Implement LEGION-209. Issue: LEGION-209. Phase: implementing. Resume the existing phase work."
)

// mergerToken is the merger's claim of the harness's issue.
const mergerToken claim.Token = "legion-LEGION-209-merger"

// holdStop is the work hold's stop of h's claim as the outbox posts it, once the close has written
// the tree's work hold row (closeHold): the message that asked the agent to end its turn reached
// the claim at after, and the stop is cut holdGrace later.
func holdStop(h *harness, after time.Time) RequestSuspend {
	h.store.closeHold(h.claim().Tree)
	return RequestSuspend{Claim: h.token, By: SuspendedByHold, Reason: "the design gate of LEGION-208 closed",
		TurnAfter: after, Deadline: after.Add(holdGrace)}
}

// directHoldStop is the hold's stop of a claim no message is sent to, a released worker that is
// not a merger: the close writes the tree's work hold row (closeHold) and records the hold over
// its suspension at once, with no deadline.
func directHoldStop(h *harness) RequestSuspend {
	h.store.closeHold(h.claim().Tree)
	return RequestSuspend{Claim: h.token, By: SuspendedByHold, Reason: "the design gate of LEGION-208 closed"}
}

// openingStart is the start the opening writes for h's claim, as the outbox hands it to EndHold:
// the opening's sentences, then the phase task the boot builds, with the opening's sentences
// carried apart (RequestDeliver.Opening).
func openingStart(h *harness) RequestDeliver {
	return RequestDeliver{Claim: h.token, Task: holdOpening + "\n\n" + resumeTask, ID: "outbox:12", Row: 12,
		Phase: phase.Implementing, Generation: 1, Opening: holdOpening}
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
// (Event.door) waits on a claim the hold stopped, by its caller: an operator's own act on that
// claim starts it — the operator's resume (RequestResume whose By is the operator, which the
// operator route and its fresh session post) and the operator's retry (RequestAskedRetry whose By
// is the operator) — and every other caller's request waits, the project's controller's included:
// its resume, its retry and its fresh session's resume (decision 16 of dispatch://INFRA-4528's
// plan, askedDoor). The list is the machine's own: every event type declares its door, the
// compiler holds each type to it, and samples holds this test to every type the package declares,
// so a type added later is in the list or fails to compile. The workflow's own task, an outbox
// row's, waits as the operator's own task does.
func TestWorkHoldHoldsEveryRequestThatCouldStartATurn(t *testing.T) {
	starts := map[string]bool{}
	all := samples(t)
	for name, events := range all {
		for _, ev := range events {
			if ev.door() != noDoor {
				starts[name] = true
			}
		}
	}
	for _, name := range []string{"RequestSpawn", "RequestReady", "RequestResume", "RequestRuleResume", "RequestRetry",
		"RequestAskedRetry", "RequestDeliver", "RequestCompact", "ResumePause"} {
		if !starts[name] {
			t.Errorf("%s declares that it starts no turn, want it among the requests that can start or resume one", name)
		}
	}
	for _, by := range []SuspendSource{SuspendedByOperator, SuspendedByController} {
		want := ruleDoor
		if by == SuspendedByOperator {
			want = ownerDoor
		}
		for _, ev := range []Event{RequestResume{By: by}, RequestAskedRetry{By: by}} {
			if got := ev.door(); got != want {
				t.Errorf("%T asked by %s declares door %d, want %d: only the operator's own act starts a held claim", ev, by, got, want)
			}
		}
	}
	type door struct {
		name string
		ev   func(c Claim) Event
	}
	var list []door
	for _, name := range slices.Sorted(maps.Keys(starts)) {
		sample := all[name][0]
		list = append(list, door{name: name, ev: func(c Claim) Event { return fenced(sample, c) }})
	}
	// fenced asks a resume and a retry as the operator; the controller asks each too, and its
	// fresh session's resume is its resume.
	list = append(list,
		door{name: "RequestResume of the controller", ev: func(c Claim) Event {
			return RequestResume{Claim: c.Token, By: SuspendedByController}
		}},
		door{name: "RequestAskedRetry of the controller", ev: func(c Claim) Event {
			return RequestAskedRetry{Claim: c.Token, By: SuspendedByController}
		}},
		door{name: "RequestDeliver of an outbox row", ev: func(c Claim) Event {
			return RequestDeliver{Claim: c.Token, Task: "A CI settlement: the checks of the branch passed.", ID: "outbox:9", Row: 9,
				Phase: phase.Implementing, Generation: 1}
		}})
	for _, d := range list {
		t.Run(d.name, func(t *testing.T) {
			h := newHarness(t)
			h.stoppedByTheHold()
			calls, prompts, pending := len(h.rt.Methods()), len(h.prompts()), h.pending()
			ev := d.ev(h.claim())

			err := h.handle(ev)

			if ev.door() == ownerDoor {
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

		resumed, err := h.m.EndHold(h.ctx, &start)

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

		if resumed, err := h.m.EndHold(h.ctx, nil); resumed || err != nil {
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
// operator's fresh session's resume, which starts it in a new session; the claim is handed the
// task it kept. The controller's are no operator's act and wait (I2,
// TestWorkHoldRefusesTheControllersStartsWhileTheTreeIsHeld).
func TestWorkHoldStartsOnTheOperatorsResumeOrRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(h *harness)
		calls string
	}{
		{"the operator's resume", func(h *harness) { h.must(RequestResume{Claim: h.token, By: SuspendedByOperator}) }, "Resume"},
		{"the operator's retry", func(h *harness) { h.must(RequestAskedRetry{Claim: h.token, By: SuspendedByOperator}) }, "Resume"},
		{"the operator's fresh session's resume", func(h *harness) {
			h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByOperator})
			h.must(RequestResume{Claim: h.token, By: SuspendedByOperator})
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

// M8: the daemon's own suspension (a close, a linger, a move out of the workflow) takes a held
// claim over: the claim is the daemon's from then on, and the daemon's suspension ends its phase,
// so the task the hold kept goes with it.
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
		if resumed, err := h.m.EndHold(h.ctx, nil); resumed || err != nil {
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

		if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
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
		resumed, err := h.m.EndHold(h.ctx, &start)
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

		if resumed, err := h.m.EndHold(h.ctx, nil); resumed || err != nil {
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

		if resumed, err := h.m.EndHold(h.ctx, nil); resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want nothing started", resumed, err)
		}
		h.m.Wait()

		if prompts := h.wantPrompts(2); prompts[1].Message != laterTask {
			t.Fatalf("the claim was sent %q, want the task the stop held back", prompts[1].Message)
		}
	})
}

// The hold's end keeps no task of news alone (Delivery.NewsOnly). Such a task holds no work, so
// the one task is the opening start's own, and the news the kept task told, which no turn
// confirmed, is still owed for the opening's start to carry (the daemon's startNewsOf). A kept
// task that carries news keeps its work under the newest opening's sentences. The opening's start
// carries the news the claim is owed now, which the daemon renders over that kept work, and that
// news goes with the one task in place of the kept task's own: a merger the hold stopped reads
// what changed before it merges. The start's news goes with the work after a restart, which reads
// the kept task back from the store, and after an attempt of the hold's end that a session refusal
// answered (resumeRefusal), which changes nothing of the claim. A start that carries no news
// leaves the kept task's news with its work, and each send renders that news again behind the
// sentences (Deps.DesignNews).
func TestWorkHoldOpeningOfAClaimThatKeptNews(t *testing.T) {
	news := []byte(`{"task":"` + phaseTask + `"}`)
	const told = phaseTask + "\n\nThe news of the approved version."
	// releasedWith is a claim released while it held task, which no turn took, that the hold's
	// stop took over at once.
	releasedWith := func(t *testing.T, task RequestDeliver) *harness {
		t.Helper()
		h := newHarness(t)
		h.reach(StateRegistered)
		task.Claim = h.token
		h.must(task)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByReleased, Reason: "the idle pod was released"})
		h.must(directHoldStop(h))
		h.wantHeld()
		return h
	}
	// keptTold is a claim that kept told, the task of entry 2 that carries news.
	keptTold := func(t *testing.T) *harness {
		t.Helper()
		return releasedWith(t, RequestDeliver{Task: told, ID: "outbox:7", Row: 7, Phase: phase.Implementing,
			Generation: 1, Entry: 2, News: news})
	}
	// startWithNews is the opening's start as the daemon builds it over the kept task: its text is
	// the opening's sentences, the kept work and the news the claim is owed now, and it carries
	// that news apart.
	startWithNews := func(h *harness) RequestDeliver {
		start := openingStart(h)
		start.Task = holdOpening + "\n\n" + phaseTask + "\n\nThe news of version 5."
		start.News = []byte(`{"task":"` + phaseTask + `","items":[{"to":5}]}`)
		return start
	}
	wantTheStartsNews := func(t *testing.T, h *harness, start RequestDeliver) {
		t.Helper()
		if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
		}
		p := h.wantPendingTask(start.Task)
		if string(p.News) != string(start.News) || p.Opening != holdOpening || p.ID != start.ID || p.Entry != 2 {
			t.Fatalf("the one task is %+v, want the kept work of entry 2 with the start's news, under the start's id, behind the opening's sentences", p)
		}
	}
	t.Run("a task of news alone gives way to the opening start's own", func(t *testing.T) {
		h := releasedWith(t, RequestDeliver{Task: "A person approved a new version of the design.",
			ID: "outbox:7", Row: 7, Phase: phase.Implementing, Generation: 1, News: news, NewsOnly: true})
		start := openingStart(h)
		if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
		}
		if p := h.wantPendingTask(start.Task); p.NewsOnly || p.News != nil || p.Opening != holdOpening || p.ID != start.ID {
			t.Fatalf("the one task is %+v, want the opening start's own, with no news and the opening apart", p)
		}
	})
	t.Run("the start's news goes with the kept task's work", func(t *testing.T) {
		h := keptTold(t)
		wantTheStartsNews(t, h, startWithNews(h))
	})
	t.Run("the start's news goes with the kept task's work after a restart", func(t *testing.T) {
		h := keptTold(t)
		h.restart()
		wantTheStartsNews(t, h, startWithNews(h))
	})
	t.Run("the start's news goes with the kept task's work after a refused attempt", func(t *testing.T) {
		h := keptTold(t)
		h.store.setHold(sessionID(session), claim.Token("legion-legion-LEGION-209-tester"))
		h.restart()
		start := startWithNews(h)
		var refused *RefusedError
		if resumed, err := h.m.EndHold(h.ctx, &start); resumed || !errors.As(err, &refused) {
			t.Fatalf("the hold's end of a claim recording a session another claim holds = %v, %v; want a refusal", resumed, err)
		}
		h.wantHeld()
		if p := h.wantPendingTask(told); string(p.News) != string(news) {
			t.Fatalf("the refused claim holds %+v, want the task it kept with its own news", p)
		}
		h.store.setHold(sessionID(session), h.token)
		wantTheStartsNews(t, h, start)
	})
	t.Run("a start that carries no news leaves the kept task's news with its work", func(t *testing.T) {
		h := keptTold(t)
		start := openingStart(h)
		if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
			t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
		}
		if p := h.wantPendingTask(holdOpening + "\n\n" + told); string(p.News) != string(news) || p.Opening != holdOpening {
			t.Fatalf("the one task is %+v, want the kept task's work and news behind the opening's sentences", p)
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

	if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
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

	resumed, err := h.m.EndHold(h.ctx, &start)
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
	if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
		t.Fatalf("the hold's end of a claim holding its session = %v, %v; want the claim started", resumed, err)
	}
	if calls := h.calls("Resume")[resumes:]; len(calls) != 1 {
		t.Fatalf("the hold's end asked the runtime to resume %d times, want once", len(calls))
	}
	h.wantPendingTask(holdOpening + "\n\n" + laterTask)
}

// Spec step 3: a stop the daemon has not made when the gate opens again is not made. The opening
// deletes the tree's work hold row in its own transaction, and each decision of a claim the hold's
// stop is held over reads whether that row stands (Store.WorkHoldStands, readHoldForStop). The
// first such decision after the opening's commit reads no row and drops the stop, whatever the
// outbox's stop row has run since: the turn's end leaves the claim idle and sends the task the
// stop held back, and the hold's deadline cuts no turn. The hold's stop asked of an opened tree is
// answered ErrWorkHoldEnded and makes no stop. A read that fails keeps the stop held, so the turn's
// end makes it; the read of the row after that stop fails as well, so the turn's end answers its
// error. The stop's row then asks again, and its decision has the daemon start the claim
// (TestWorkHoldStopAskedAgainOfAClaimItStopped).
func TestWorkHoldDropsItsStopAtTheFirstDecisionAfterTheOpening(t *testing.T) {
	const dropped = `msg="supervise: the work hold ended before its stop was made; the stop is not made"`
	heldInTurn := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t)
		h.inPhaseTurn()
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
		}
		h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
		h.wantPrompts(1)
		return h
	}
	wantNoStop := func(t *testing.T, h *harness) {
		t.Helper()
		h.wantCalls("Suspend", 0)
		if c := h.claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the claim reads WorkHeld %v, a stop held %v; want the stop dropped", c.WorkHeld(), c.SuspensionHeld)
		}
		for _, put := range h.store.history() {
			if put.State == StateSuspended {
				t.Fatalf("the store was written the claim suspended by %q, want no stop written", put.SuspendedBy)
			}
		}
		if lines := h.logs.lines(dropped); len(lines) != 1 {
			t.Fatalf("logged %q, want the stop's drop logged once", lines)
		}
	}
	t.Run("the turn's end leaves the claim idle and sends the task the stop held back", func(t *testing.T) {
		h := heldInTurn(t)
		h.store.openHold(h.claim().Tree)

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantState(StateIdle)
		wantNoStop(t, h)
		if prompts := h.wantPrompts(2); prompts[1].Message != laterTask {
			t.Fatalf("the claim was sent %q, want the task the stop held back", prompts[1].Message)
		}
		if stored := h.store.load(h.token); stored.State != StateIdle {
			t.Fatalf("the store holds the claim %s by %q, want it idle", stored.State, stored.SuspendedBy)
		}
	})
	t.Run("the hold's deadline cuts no turn", func(t *testing.T) {
		h := heldInTurn(t)
		h.store.openHold(h.claim().Tree)

		h.advance(holdGrace)

		h.wantState(StateWorking)
		wantNoStop(t, h)
		h.wantPendingTask(laterTask)
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		if prompts := h.wantPrompts(2); prompts[1].Message != laterTask {
			t.Fatalf("the claim was sent %q at its turn's end, want the task the stop held back", prompts[1].Message)
		}
	})
	t.Run("an idle claim's deadline sends the task the stop held back", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of an idle claim answered %v, want it held for the turn its message starts", err)
		}
		h.must(RequestDeliver{Claim: h.token, Task: laterTask, ID: "outbox:8", Row: 8, Phase: phase.Implementing, Generation: 1})
		h.wantPrompts(1)
		h.store.openHold(h.claim().Tree)

		// The deadline's timer reaches a decision whose drop disarms it, so the fence refuses it,
		// and the task goes all the same.
		h.advance(holdGrace)

		h.wantState(StateIdle)
		wantNoStop(t, h)
		if prompts := h.wantPrompts(2); prompts[1].Message != laterTask {
			t.Fatalf("the claim was sent %q, want the task the stop held back", prompts[1].Message)
		}
	})
	t.Run("a read that fails keeps the stop held", func(t *testing.T) {
		h := heldInTurn(t)
		h.store.openHold(h.claim().Tree)
		refused := errors.New("the database did not answer")
		h.store.fail("WorkHoldStands", refused)

		// The stop is made with no read of the row, and the read of the row after it fails too
		// (holdReadAfterStop), so the turn's end answers that read's error.
		if err := h.handle(StreamTurnEnd{Claim: h.token}); !errors.Is(err, refused) {
			t.Fatalf("the turn's end answered %v, want the refused read of the row after the stop", err)
		}

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		h.wantPendingTask(laterTask)
		h.wantPrompts(1)
		if lines := h.logs.lines("level=WARN", "the hold's stop stays held", "the database did not answer"); len(lines) != 1 {
			t.Fatalf("logged %q, want one warning that the stop stays held, naming the read that failed", lines)
		}
		if lines := h.logs.lines(dropped); len(lines) != 0 {
			t.Fatalf("logged %q, want no drop of the stop", lines)
		}
	})
	t.Run("the hold's stop of an opened tree answers that the hold ended and makes no stop", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		h.store.openHold(h.claim().Tree)
		puts := len(h.store.history())

		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrWorkHoldEnded) {
			t.Fatalf("the hold's stop of a claim whose tree the opening opened answered %v, want %v", err, ErrWorkHoldEnded)
		}

		h.wantState(StateWorking)
		if c := h.claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the claim reads WorkHeld %v, a stop held %v; want no stop held", c.WorkHeld(), c.SuspensionHeld)
		}
		h.wantCalls("Suspend", 0)
		if got := len(h.store.history()); got != puts {
			t.Fatalf("the store was written %d claims, want none: the stop changes nothing of the claim", got-puts)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
	})
	t.Run("a read that fails for the hold's stop answers its error and makes no stop", func(t *testing.T) {
		h := newHarness(t)
		h.inPhaseTurn()
		failed := errors.New("the read waited past the claim-write deadline")
		h.store.fail("WorkHoldStands", failed)

		if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, failed) || errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop whose read failed answered %v, want the read's error and no hold", err)
		}

		if c := h.claim(); c.WorkHeld() || c.SuspensionHeld {
			t.Fatalf("the claim reads WorkHeld %v, a stop held %v; want no stop held", c.WorkHeld(), c.SuspensionHeld)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
	})
}

// A tool step and a model's answer read nothing of the tree's work hold row, though the hold's
// stop is held over the claim (readHoldForStop): no row of either kind can make the stop, and a
// read for each step of a turn would cost the store a transaction for nothing. The step is
// recorded all the same, and the turn's end reads the row and makes the stop, and then reads the
// row once more after the stop's write (holdReadAfterStop).
func TestWorkHoldTakesNoLockForAToolStepOrAnAnswer(t *testing.T) {
	h := newHarness(t)
	h.inPhaseTurn()
	if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
	}
	before := h.store.holdReadCount()

	h.must(StreamToolStep{Claim: h.token, Tool: "bash", CallID: "call-1", ArgsBytes: 12, ArgsHash: "hash-1"})
	h.must(StreamToolStep{Claim: h.token, Tool: "bash", CallID: "call-1", End: true})
	h.must(StreamAnswered{Claim: h.token, Model: "anthropic/claude-sonnet", Generation: h.generation()})

	if reads := h.store.holdReadCount(); reads != before {
		t.Fatalf("after a tool step and an answer the store had answered %d reads of the row, want %d: neither reads it", reads, before)
	}
	if step := h.store.step(h.token); step.CallID != "call-1" || step.EndedAt.IsZero() {
		t.Fatalf("the store records the step %+v, want the call's start and end", step)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantHeld()
	if reads := h.store.holdReadCount(); reads != before+2 {
		t.Fatalf("after the turn's end the store had answered %d reads of the row, want two more: the turn end's and the read after its stop",
			reads)
	}
}

// providerErrorWrites is the store double that keeps, for each claim-and-delivery write of a task
// a provider error took back (Delivery.ProviderErrorNote), the deadline of the context it was
// handed, or the zero time where that context had none.
type providerErrorWrites struct {
	Store
	mu        sync.Mutex
	deadlines []time.Time
}

func (s *providerErrorWrites) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if d.ProviderErrorNote != "" {
		deadline, _ := ctx.Deadline()
		s.mu.Lock()
		s.deadlines = append(s.deadlines, deadline)
		s.mu.Unlock()
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

// Every claim and delivery write of a decision waits at most the claim-write deadline
// (Timeouts.ClaimWrite, Machine.write), so a store that stops answering holds the machine's lock
// no longer than that bound. The write of a task a provider error took back is one of them: it
// waits at most that deadline (providerErrorClears).
func TestWorkHoldBoundsTheProviderErrorWriteByTheClaimWriteDeadline(t *testing.T) {
	var writes *providerErrorWrites
	h := workingClaim(t, func(d *Deps) {
		writes = &providerErrorWrites{Store: d.Store}
		d.Store = writes
	})
	asked := time.Now()

	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})

	if p := h.pending(); p.ProviderErrorRetries != 1 {
		t.Fatalf("the task is %+v, want it kept and counted one send", p)
	}
	writes.mu.Lock()
	deadlines := slices.Clone(writes.deadlines)
	writes.mu.Unlock()
	if len(deadlines) == 0 {
		t.Fatal("the store was asked no write of the task the provider error took back")
	}
	bound := h.deps.claimWrite()
	for i, deadline := range deadlines {
		if deadline.IsZero() || deadline.After(time.Now().Add(bound)) || deadline.Before(asked) {
			t.Fatalf("write %d of the task the provider error took back had the deadline %v, want one within the claim-write deadline, %s", i+1, deadline, bound)
		}
	}
}

// holdVersion is the version of the spec that closed the gate, as the hold's stop names it.
const holdVersion = 4

// versioned is the hold's stop request naming holdVersion, as the outbox's stop row posts it
// (record.SuperviseRequest.HoldVersion).
func versioned(request RequestSuspend) RequestSuspend {
	request.HoldVersion = holdVersion
	return request
}

// stopHook is the harness's runtime with at run at each process stop (Suspend), before the stop
// itself: what a test reads or changes while the runtime stops the process. Every call goes on
// to the fake, which records it.
type stopHook struct {
	*fake.Runtime
	at func()
}

func (s stopHook) Suspend(ctx context.Context, loc runtime.Locator) error {
	s.at()
	return s.Runtime.Suspend(ctx, loc)
}

// endedUnderStop is the daemon as Deps.WorkHoldEndedUnderStop reaches it: each claim it was told
// of, with the version, in order.
type endedUnderStop struct {
	mu   sync.Mutex
	told []holdEndTold
}

// holdEndTold is one call of Deps.WorkHoldEndedUnderStop.
type holdEndTold struct {
	token   claim.Token
	state   ClaimState
	by      SuspendSource
	version int
}

func (e *endedUnderStop) tell(c Claim, version int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.told = append(e.told, holdEndTold{token: c.Token, state: c.State, by: c.SuspendedBy, version: version})
}

func (e *endedUnderStop) calls() []holdEndTold {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.told)
}

// wantNone is the daemon told nothing.
func (e *endedUnderStop) wantNone(t *testing.T) {
	t.Helper()
	if told := e.calls(); len(told) != 0 {
		t.Fatalf("the daemon was told %+v, want nothing: the hold stands", told)
	}
}

// wantOnce is the daemon told once, of h's claim stopped by the hold, with holdVersion.
func (e *endedUnderStop) wantOnce(h *harness) {
	h.t.Helper()
	want := holdEndTold{token: h.token, state: StateSuspended, by: SuspendedByHold, version: holdVersion}
	if told := e.calls(); !slices.Equal(told, []holdEndTold{want}) {
		h.t.Fatalf("the daemon was told %+v, want once %+v", told, want)
	}
}

// holdReadHarness is newHarness over a machine whose runtime runs at at each process stop
// (stopHook), nil for nothing, and whose daemon is told of a hold that ended under a stop
// (endedUnderStop). at runs inside the decision, under the machine's lock, so it reads nothing of
// the machine.
func holdReadHarness(t *testing.T, at func(h *harness)) (*harness, *endedUnderStop) {
	t.Helper()
	h := newBareHarness(t)
	told := &endedUnderStop{}
	h.deps.WorkHoldEndedUnderStop = told.tell
	h.deps.Runtime = stopHook{Runtime: h.rt, at: func() {
		if at != nil {
			at(h)
		}
	}}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, told
}

// Spec step 7 ("starts each claim that the hold stopped") and R2, check a3d61f8e: once the hold's
// stop is written, the machine reads the tree's work hold row again (holdReadAfterStop). The
// opening takes that row for update before it reads the claims and deletes it, so where the read
// finds the row gone, the opening may have read the claim before the stop's write, and the daemon
// is told once, with the claim and the version, so it starts the claim. A row that stands tells
// nobody: a later opening reads the stop. A read that fails answers its error, so the stop's row
// asks again, and tells nobody. Every write of the hold's stop reaches the read: the stop asked
// now, the takeover of a released claim and a held stop made at the turn's end.
func TestWorkHoldReadsItsRowAgainOnceTheStopIsWritten(t *testing.T) {
	tree := queuedClaim().Tree
	t.Run("a row that stands tells nobody", func(t *testing.T) {
		h, told := holdReadHarness(t, nil)
		h.reach(StateIdle)

		h.must(versioned(directHoldStop(h)))

		h.wantHeld()
		told.wantNone(t)
	})
	t.Run("an opening while the process stops has the daemon start the claim", func(t *testing.T) {
		h, told := holdReadHarness(t, func(h *harness) { h.store.openHold(tree) })
		h.reach(StateIdle)

		h.must(versioned(directHoldStop(h)))

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		told.wantOnce(h)
	})
	t.Run("a read that fails answers its error and tells nobody", func(t *testing.T) {
		refused := errors.New("the read waited past the claim-write deadline")
		h, told := holdReadHarness(t, func(h *harness) {
			h.store.openHold(tree)
			h.store.fail("WorkHoldStands", refused)
		})
		h.reach(StateIdle)

		if err := h.handle(versioned(directHoldStop(h))); !errors.Is(err, refused) {
			t.Fatalf("the hold's stop answered %v, want the refused read of the row after the stop", err)
		}

		h.wantHeld()
		told.wantNone(t)
		if lines := h.logs.lines("level=WARN", "after the hold's stop was written", refused.Error()); len(lines) != 1 {
			t.Fatalf("logged %q, want one warning naming the read that failed", lines)
		}
	})
	t.Run("the takeover of a released claim reads the row", func(t *testing.T) {
		h, told := holdReadHarness(t, nil)
		h.releasedWithATask()
		h.store.onPut = func(c Claim) {
			// The opening commits as the takeover is written. The decision read the row before,
			// in a transaction of its own (readHoldForStop), so the opening waits for nothing of
			// the takeover's. onPut runs under the store's lock, which guards the opened trees.
			if c.SuspendedBy == SuspendedByHold {
				h.store.opened[c.Tree] = true
			}
		}
		stops := len(h.calls("Suspend"))

		h.must(versioned(directHoldStop(h)))

		h.wantHeld()
		if got := len(h.calls("Suspend")); got != stops {
			t.Fatalf("the runtime was asked %d more stops, want none: the takeover of a released claim stops no process", got-stops)
		}
		told.wantOnce(h)
	})
	t.Run("a held stop made at the turn's end reads the row", func(t *testing.T) {
		h, told := holdReadHarness(t, func(h *harness) { h.store.openHold(tree) })
		h.inPhaseTurn()
		if err := h.handle(versioned(holdStop(h, h.clock.Now()))); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
		}

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		told.wantOnce(h)
	})
}

// The hold's stop asked again of a claim the hold already stopped. An earlier attempt of the
// stop's row made the stop, and the read of the row after it failed, so the row asks again. In a
// tree that still holds work, the decision's read finds the row standing, and the stop changes
// nothing by its rank (suspendedAgain): nothing is written and nobody is told, and a later opening
// reads the stop. In a tree the opening has opened since, the decision's read finds no row and the
// answer is ErrWorkHoldEnded, so the stop's row finishes. The opening may have read the claim live
// before the earlier attempt's write, so the decision tells the daemon once, with the version, and
// the daemon starts the claim.
func TestWorkHoldStopAskedAgainOfAClaimItStopped(t *testing.T) {
	refused := errors.New("the database did not answer")
	stoppedWithAFailedRead := func(t *testing.T) (*harness, *endedUnderStop) {
		t.Helper()
		h, told := holdReadHarness(t, func(h *harness) { h.store.fail("WorkHoldStands", refused) })
		h.reach(StateIdle)
		if err := h.handle(versioned(directHoldStop(h))); !errors.Is(err, refused) {
			t.Fatalf("the hold's stop answered %v, want the refused read of the row after the stop", err)
		}
		h.wantHeld()
		h.store.fail("WorkHoldStands", nil)
		return h, told
	}
	t.Run("in a tree that still holds work", func(t *testing.T) {
		h, told := stoppedWithAFailedRead(t)
		puts := len(h.store.history())

		h.must(versioned(directHoldStop(h)))

		h.wantHeld()
		if got := len(h.store.history()); got != puts {
			t.Fatalf("the store was written %d claims, want none: the stop asked again changes nothing", got-puts)
		}
		told.wantNone(t)
	})
	t.Run("in a tree the opening opened", func(t *testing.T) {
		h, told := stoppedWithAFailedRead(t)
		h.store.openHold(h.claim().Tree)

		if err := h.handle(versioned(directHoldStop(h))); !errors.Is(err, ErrWorkHoldEnded) {
			t.Fatalf("the hold's stop asked again after the opening answered %v, want %v", err, ErrWorkHoldEnded)
		}

		h.wantHeld()
		h.wantCalls("Suspend", 1)
		told.wantOnce(h)
	})
}

// stoppedWithItsWriteOwed walks h's claim from idle to the hold's stop asked now, made with its
// claim write refused by away: the claim stands stopped by the hold in memory, its write owed, and
// the store still holds it idle. Then the opening ends the tree's hold. The read of the row after
// the stop never ran, since the write failed first.
func (h *harness) stoppedWithItsWriteOwed(away error) {
	h.t.Helper()
	h.reach(StateIdle)
	h.store.fail("PutClaim", away)
	if err := h.handle(versioned(directHoldStop(h))); !errors.Is(err, away) {
		h.t.Fatalf("the hold's stop answered %v, want its claim write refused", err)
	}
	if c := h.claim(); c.State != StateSuspended || c.SuspendedBy != SuspendedByHold {
		h.t.Fatalf("the claim is %s by %q, want it stopped by the hold in memory", c.State, c.SuspendedBy)
	}
	if stored := h.store.load(h.token); stored.State == StateSuspended {
		h.t.Fatalf("the store holds the claim %s by %q, want it behind memory: the stop's write failed", stored.State, stored.SuspendedBy)
	}
	h.store.openHold(h.claim().Tree)
}

// The hold's stop asked again, after the opening, of a claim an earlier attempt stopped whose
// claim write the database did not take. Handle's repair fails again, so the store still holds
// the claim live, and a fact of the hold's end would read it live and start nothing. So the
// decision tells the daemon nothing and answers an error that wraps errClaimWriteOwed, not
// ErrWorkHoldEnded: the stop's row is asked again rather than finished. Once the store takes the
// write, the row's next attempt makes it at the head of the decision (Handle's repair), and the
// decision tells the daemon once, with the version, and answers ErrWorkHoldEnded.
func TestWorkHoldStopAskedAgainWaitsForItsOwedWrite(t *testing.T) {
	away := errors.New("the database did not answer")
	h, told := holdReadHarness(t, nil)
	h.stoppedWithItsWriteOwed(away)

	err := h.handle(versioned(directHoldStop(h)))

	if !errors.Is(err, errClaimWriteOwed) || errors.Is(err, ErrWorkHoldEnded) {
		t.Fatalf("the hold's stop asked again with its write owed answered %v, want the owed write's error and not %v", err, ErrWorkHoldEnded)
	}
	told.wantNone(t)
	if stored := h.store.load(h.token); stored.State == StateSuspended {
		t.Fatalf("the store holds the claim %s by %q, want it still behind memory", stored.State, stored.SuspendedBy)
	}

	h.store.fail("PutClaim", nil)
	if err := h.handle(versioned(directHoldStop(h))); !errors.Is(err, ErrWorkHoldEnded) {
		t.Fatalf("the hold's stop asked again once the store takes the write answered %v, want %v", err, ErrWorkHoldEnded)
	}
	h.wantHeld()
	told.wantOnce(h)
}

// The architect's decision 3 on INFRA-4528 (dispatch://INFRA-4528/comment/c0b65f44-c999-411b-bd63-a9e64805bee8):
// an operator's task queued behind the work hold's stop is never retired by the opening; the
// operator's task is delivered and the opening's task follows it. The operator gives the
// implementer a task (RequestDeliver with no phase, run or entry, as the operator's deliver route
// builds it) while a turn runs; the hold's stop is then held for that turn, and the turn's end
// stops the claim with the operator's task kept. A backward move while the gate was closed raised
// a newer entry of the implementer's phase, so the opening's start carries that entry's task.
func TestWorkHoldOpeningKeepsAnOperatorsTaskQueuedBehindTheHeldStop(t *testing.T) {
	const operatorTask = "Read the daemon log of the last hour and report what failed."
	h := newHarness(t)
	h.inPhaseTurn()
	h.must(RequestDeliver{Claim: h.token, Task: operatorTask})
	if err := h.handle(holdStop(h, h.clock.Now())); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the hold's stop of a claim in a turn answered %v, want it held for the turn", err)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantHeld()
	if kept := h.claim().Pending; kept == nil || kept.Task != operatorTask {
		t.Fatalf("the stopped claim keeps %+v, want the operator's task", kept)
	}
	start := openingStart(h)
	start.Task, start.Entry = holdOpening+"\n\nImplement entry 3 of LEGION-209.", 3

	if resumed, err := h.m.EndHold(h.ctx, &start); !resumed || err != nil {
		t.Fatalf("the hold's end = %v, %v; want the claim started", resumed, err)
	}
	if p := h.claim().Pending; p == nil || !strings.Contains(p.Task, operatorTask) {
		t.Fatalf("after the hold's end the claim holds %+v (delivery log %v): the opening retired the operator's task", p, h.store.deliveryLog())
	}
}
