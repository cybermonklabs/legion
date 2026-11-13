package supervise

import (
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// endedWorkLine is the one line a let-go that records what it ended writes (recordEnd).
const endedWorkLine = "supervise: the relaunch ended work the agent had running"

// reportActivity is the claim's process reporting what it is doing, through the store's own write
// onto the launch it runs now (memStore.reportActivity, store's PutClaimActivity): background work
// of the kinds named, or none running when it names none.
func (h *harness) reportActivity(kinds ...string) {
	h.t.Helper()
	report := ActivityReport{At: h.clock.Now(), Background: len(kinds) > 0, Kinds: kinds}
	if !h.store.reportActivity(h.token, h.generation(), report) {
		h.t.Fatalf("the store refused the report of launch %d", h.generation())
	}
}

// wantEnded is the record of what the claim's last let-go ended, as the machine holds it and as
// the store holds it: the let-go's own write carries it, so a restart reads what the machine did.
func (h *harness) wantEnded(want EndedWork) {
	h.t.Helper()
	for where, got := range map[string]EndedWork{"the machine": h.claim().EndedWork, "the store": h.store.load(h.token).EndedWork} {
		if !reflect.DeepEqual(got, want) {
			h.t.Errorf("%s records the let-go's end as %+v, want %+v", where, got, want)
		}
	}
}

// wantNoEnded is a claim no let-go of which recorded anything, and no line saying one did.
func (h *harness) wantNoEnded() {
	h.t.Helper()
	h.wantEnded(EndedWork{})
	if lines := h.records.of(endedWorkLine); len(lines) != 0 {
		h.t.Errorf("logged %d lines that the relaunch ended work, want none", len(lines))
	}
}

// wantEndedLine is the one Warn line the let-go wrote, naming its cause.
func (h *harness) wantEndedLine(cause string) {
	h.t.Helper()
	lines := h.records.of(endedWorkLine)
	if len(lines) != 1 {
		h.t.Fatalf("logged %d lines that the relaunch ended work, want one", len(lines))
	}
	if lines[0].level != slog.LevelWarn {
		h.t.Errorf("the line is %s, want WARN", lines[0].level)
	}
	if got, ok := lines[0].attr("cause"); !ok || got.String() != cause {
		h.t.Errorf("the line names the cause %q, want %q", got, cause)
	}
	for _, key := range []string{"turn", "kinds", "delivery"} {
		if _, ok := lines[0].attr(key); !ok {
			h.t.Errorf("the line names no %s", key)
		}
	}
}

// The prompt-failure retirement: three acknowledged prompts that started no turn retire the
// process and relaunch the same session. The process had run a turn and its own report named a
// subagent still running, which the retirement ends with it, so the retirement records it under
// its own cause before the relaunch's write clears that report.
func TestARelaunchRecordsAPromptRetirement(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.reportActivity("subagent")
	h.must(RequestDeliver{Claim: h.token, Task: "the task"})
	h.advance(testRPC)
	h.observe(runtime.Alive)
	h.advance(testRPC)
	h.observe(runtime.Alive)
	retired, at := h.generation(), h.clock.Now().Add(testRPC)

	h.advance(testRPC)

	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 1)
	h.wantState(StateLaunching)
	h.wantEnded(EndedWork{At: at, Cause: endRetired, Generation: retired,
		Background: true, Kinds: []string{"subagent"}, Reported: true})
	h.wantEndedLine(endRetired)
	if _, held, _ := h.store.ClaimActivity(h.ctx, h.token, h.generation()); held {
		t.Error("the relaunched launch reads a report, want the launch's write to have cleared the retired process's")
	}
}

// The stale-plugin relaunch and the stale registration: both let a process that ran an older
// plugin go and start it again on the current one, and both record what it ended under that
// cause. The lane's own pass relaunches an idle claim; the registration path relaunches the
// process whose registration it refuses.
func TestARelaunchRecordsAStalePluginRelaunch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relaunch func(h *harness)
	}{
		{name: "the stale-plugin lane's relaunch", relaunch: func(h *harness) {
			h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		}},
		{name: "the stale registration", relaunch: func(h *harness) {
			h.m.deps.PluginRelaunchable = true
			err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
				SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: 24})
			if err == nil {
				h.t.Fatal("the stale registration was accepted, want it refused for the relaunch")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.reportActivity("subagent", "async-job")
			h.m.deps.PluginContract = 25
			stale, at := h.generation(), h.clock.Now()

			tc.relaunch(h)

			h.wantState(StateLaunching)
			h.wantCalls("Resume", 1)
			h.wantEnded(EndedWork{At: at, Cause: endStalePlugin, Generation: stale,
				Background: true, Kinds: []string{"subagent", "async-job"}, Reported: true})
			h.wantEndedLine(endStalePlugin)
		})
	}
}

// A death: the process the claim records is found gone in the middle of its task's turn, with a
// subagent its report named. The death records the turn, the task, the subagent and the runtime's
// own account of the death, before it takes the task back.
func TestARelaunchRecordsADeath(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.reportActivity("subagent")
	died, at := h.generation(), h.clock.Now()
	observation := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: at,
		Detail: "pod legion-legion-209 (uid 8a1f) ended: OOMKilled"}

	h.must(RuntimeObservation{Observation: observation})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	cause := endDied(deathCause(observation))
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: died, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"subagent"}, Reported: true})
	h.wantEndedLine(cause)
}

// A displacement: the daemon replaced the pod the process ran in, mid-turn. It records what the
// process ended as one whose pod was replaced, before the replacement takes the task back.
func TestARelaunchRecordsADisplacement(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.reportActivity("claude-background-task")
	displaced, at := h.generation(), h.clock.Now()

	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: at,
		Displaced: true, Detail: "the daemon replaced the pod to launch another role"}})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantEnded(EndedWork{At: at, Cause: endDisplaced, Generation: displaced, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"claude-background-task"}, Reported: true})
	h.wantEndedLine(endDisplaced)
}

// A repoint: the process is alive at an address a process launched now is not handed, and it is
// replaced mid-turn. The replacement takes the task back before the launch lets the process go,
// and the record still names the task's turn it cut off (Machine.cutTurnOf).
func TestARelaunchRecordsARepoint(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.reportActivity("async-job")
	repointed, at := h.generation(), h.clock.Now()

	h.observe(runtime.StaleAddress)

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if p := h.pending(); !p.Interrupted || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the repoint is %+v, want it taken back from the turn", p)
	}
	h.wantEnded(EndedWork{At: at, Cause: endRepointed, Generation: repointed, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"async-job"}, Reported: true})
	h.wantEndedLine(endRepointed)
}

// The stall backstop: a turn whose agent stopped stepping past its role's bound is ended and the
// process replaced. The record names the bound the turn passed.
func TestARelaunchRecordsTheStallBackstop(t *testing.T) {
	h := newHarness(t)
	last := stalledFor(h)
	h.reportActivity("subagent")
	stalled, at := h.generation(), h.clock.Now()

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	cause := endStalled(backstopBound)
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: stalled, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"subagent"}, Reported: true})
	h.wantEndedLine(cause)
}

// A parked wake's turn: a relaunch the wake pacing holds over a process that still runs lets that
// process go at its turn, and records what it ended then, as the process stands at the turn — the
// park itself writes nothing, and what the process did in the wait is what the turn ends.
func TestARelaunchRecordsAtAParkedWakesTurn(t *testing.T) {
	t.Run("the stale-plugin lane's refresh", func(t *testing.T) {
		h, waiting := refreshParkedIdle(t)
		if got := h.store.load(h.token).EndedWork; got.Cause != "" {
			t.Fatalf("the park recorded %+v, want nothing before the turn lets the process go", got)
		}
		h.reportActivity("subagent")
		parked := h.generation()

		h.advance(waiting.At.Sub(h.clock.Now()))

		h.wantState(StateLaunching)
		h.wantEnded(EndedWork{At: waiting.At, Cause: endStalePlugin, Generation: parked,
			Background: true, Kinds: []string{"subagent"}, Reported: true})
		h.wantEndedLine(endStalePlugin)
	})

	t.Run("a repoint whose process began its task's turn in the wait", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		sent := h.pending().ID
		h.observe(runtime.StaleAddress)
		waiting := h.wantWaitingWake(WakeLaunch)
		if got := h.store.load(h.token).EndedWork; got.Cause != "" {
			t.Fatalf("the park recorded %+v, want nothing before the turn lets the process go", got)
		}
		h.must(StreamTurnStart{Claim: h.token, DeliveryID: sent})
		h.reportActivity("async-job")
		parked := h.generation()

		h.advance(waiting.At.Sub(h.clock.Now()))

		h.wantState(StateLaunching)
		h.wantEnded(EndedWork{At: waiting.At, Cause: endRepointed, Generation: parked, Turn: true, TaskTurn: true,
			Background: true, Kinds: []string{"async-job"}, Reported: true})
		h.wantEndedLine(endRepointed)
	})
}

// The registration deadline: a process that registered and never said it was ready is retired
// and relaunched. It ran no turn, so the only work it can end is what its own report named.
func TestARelaunchRecordsTheRegistrationDeadline(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.connect()
	h.register()
	h.reportActivity("async-job")
	retired, at := h.generation(), h.clock.Now().Add(deadline)

	h.advance(deadline)

	h.wantCalls("Suspend", 1)
	h.wantState(StateLaunching)
	cause := endNeverReady("registered and never said it was ready")
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: retired,
		Background: true, Kinds: []string{"async-job"}, Reported: true})
	h.wantEndedLine(cause)
}

// A suspension records what its stop ended under the source that asked for it: the operator's
// suspension that stops a turn at its deadline — the turn and the task taken back from it — and
// the release of an idle pod, whose process's own report named a job still running.
func TestARelaunchRecordsASuspension(t *testing.T) {
	t.Run("the held suspension's deadline stops the turn", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		h.reportActivity("subagent")
		if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
		}
		stopped, at := h.generation(), h.clock.Now().Add(testStop)

		h.advance(testStop)

		h.wantState(StateSuspended)
		cause := endSuspended(SuspendedByOperator)
		h.wantEnded(EndedWork{At: at, Cause: cause, Generation: stopped, Turn: true, TaskTurn: true,
			Background: true, Kinds: []string{"subagent"}, Reported: true})
		h.wantEndedLine(cause)
	})

	t.Run("the idle pod's release", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.reportActivity("async-job")
		released, at := h.generation(), h.clock.Now()

		h.must(RequestSuspend{Claim: h.token, Reason: "the issue's pod is released", By: SuspendedByReleased})

		h.wantState(StateSuspended)
		cause := endSuspended(SuspendedByReleased)
		h.wantEnded(EndedWork{At: at, Cause: cause, Generation: released,
			Background: true, Kinds: []string{"async-job"}, Reported: true})
		h.wantEndedLine(cause)
	})
}

// A failure: a provider error that cannot clear fails the claim, which stops its process. The
// record names the failure's own sentence, and the job the process's report named.
func TestARelaunchRecordsAFailure(t *testing.T) {
	h := workingClaim(t)
	h.reportActivity("async-job")
	failed, at := h.generation(), h.clock.Now()

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})

	h.wantState(StateFailed)
	cause := endFailed(h.claim().Failure)
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: failed,
		Background: true, Kinds: []string{"async-job"}, Reported: true})
	h.wantEndedLine(cause)
}

// A relaunch does not wait for the background work it ends: the launch that replaces the process
// runs in the very decision that decided it, a subagent reported running or not, and what it
// ended is a record and a line beside it. A store that cannot answer the read costs the record
// its report — the process is recorded as one the daemon has no report of — and never the
// relaunch.
func TestARelaunchRecordsAndDoesNotWait(t *testing.T) {
	t.Run("a subagent reported running", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.reportActivity("subagent")
		h.m.deps.PluginContract = 25
		stale, at := h.generation(), h.clock.Now()

		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		if waiting := h.m.View().WakeWaiting; waiting != nil {
			t.Fatalf("the relaunch waits %+v, want it run in the decision that decided it", waiting)
		}
		h.wantEnded(EndedWork{At: at, Cause: endStalePlugin, Generation: stale,
			Background: true, Kinds: []string{"subagent"}, Reported: true})
	})

	t.Run("a read the store does not answer", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		h.reportActivity("subagent")
		h.store.fail("ClaimActivity", errBoom)
		died, at := h.generation(), h.clock.Now()
		observation := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: at}

		h.must(RuntimeObservation{Observation: observation})

		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(observation)), Generation: died,
			Turn: true, TaskTurn: true})
	})
}

// A report of an earlier launch is no report of this one. The process of the first launch dies
// idle, reporting nothing running, so its death records nothing; a report of that dead process
// that reaches the store after the relaunch — naming a subagent — is refused, since a report is
// written onto its own launch alone. When the relaunched process then dies in its task's turn
// having reported nothing, its record says the daemon has no report of it, and names no subagent.
func TestARelaunchRecordsNoReportOfAnEarlierLaunch(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.reportActivity()
	first := h.generation()
	h.observe(runtime.Gone)
	h.wantNoEnded()

	h.advance(time.Minute)
	if h.store.reportActivity(h.token, first, ActivityReport{At: h.clock.Now(), Background: true, Kinds: []string{"subagent"}}) {
		t.Fatal("the dead process's late report was written over the relaunched launch")
	}
	if _, held, _ := h.store.ClaimActivity(h.ctx, h.token, first); held {
		t.Fatal("the earlier launch still reads a report after the relaunch")
	}
	h.relaunched()
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	second, at := h.generation(), h.clock.Now()
	if second == first {
		t.Fatalf("the relaunch kept launch %d, want a new one", first)
	}
	observation := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: at}

	h.must(RuntimeObservation{Observation: observation})

	h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(observation)), Generation: second, Turn: true, TaskTurn: true})
}

// Unknown is not silence: a process that ran a turn and left no report of its launch — a Claude
// Code worker reports only at its Stop hook, a stale plugin's report is refused — is recorded as
// one the daemon has no report of, so its next turn is told any background work it started is
// gone, even though no turn was running and no task was in one when it was let go.
func TestARelaunchRecordsAProcessWithNoReportAsUnknown(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	died, at := h.generation(), h.clock.Now()
	observation := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: at}

	h.must(RuntimeObservation{Observation: observation})

	cause := endDied(deathCause(observation))
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: died})
	h.wantEndedLine(cause)
	if h.claim().EndedWork.Reported {
		t.Fatal("the record says the process reported, want it unreported")
	}
}

// Only a process that ran no turn, held none, and reported no background work is recorded as
// having ended nothing: one that ran a turn and reported nothing running, released idle, and one
// that never ran a turn and reported nothing at all, found dead before its first.
func TestARelaunchRecordsNothingWhenNothingRan(t *testing.T) {
	t.Run("a turn ran and its report says nothing runs", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.reportActivity()

		h.must(RequestSuspend{Claim: h.token, Reason: "the issue's pod is released", By: SuspendedByReleased})

		h.wantState(StateSuspended)
		h.wantNoEnded()
	})

	t.Run("no turn ran and nothing was reported", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)

		h.observe(runtime.Gone)

		h.wantState(StateLaunching)
		h.wantNoEnded()
	})
}

// A newer record replaces one nobody has been told yet as the union of the two: the death that
// cut a subagent and a task's turn off is still untold when the relaunched process — sent the
// task, its turn not yet begun, and running a job of its own — is relaunched onto the current
// plugin, so the newer record takes the newer cause and keeps the turn, the task and both kinds,
// though the process it now lets go was in no turn and confirmed no task.
func TestARelaunchRecordsTheUnionWithAnUntoldRecord(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.reportActivity("subagent")
	h.observe(runtime.Gone)
	if !h.claim().EndedWork.Tells() {
		t.Fatalf("the death recorded %+v, want a record still to tell", h.claim().EndedWork)
	}
	h.relaunched()
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the relaunch is %+v, want it sent and not yet in a turn", p)
	}
	h.reportActivity("async-job")
	h.m.deps.PluginContract = 25
	stale, at := h.generation(), h.clock.Now()

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantEnded(EndedWork{At: at, Cause: endStalePlugin, Generation: stale, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"subagent", "async-job"}, Reported: true})
}
