package supervise

import (
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// endedWorkLine is the one line a let-go that records what it ended writes (recordEnd).
const endedWorkLine = "supervise: the relaunch ended work the agent had running"

// The statements of the sentence of a turn cut off that tell what of its background work ended
// (relaunchSentence): the kind the record names; that the daemon has no report of the turn, and so
// that its subagents and jobs are gone; that every other one is gone too, where the record names
// none; and that the turn's subagents and jobs are gone, where the record holds a report of it.
const (
	saysSubagentStopped    = "Each subagent that you started stopped with that process."
	saysNoReport           = "The daemon has no report"
	saysTurnGoneUnreported = "So the subagents and background jobs of that turn are gone."
	saysEveryOtherGone     = "Every other subagent and background job that you started in that process is gone too."
	saysTurnGone           = "The subagents and background jobs of that turn are gone."
)

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
// and relaunched. It ran no turn, so the only work it can end is what its own report named. Its
// cause names what the agent never did and then the retirement, so the sentence the next process
// reads has the one "because" (endNeverReady, relaunchSentence).
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
	cause := "its agent registered and never said it was ready, so the daemon retired it"
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: retired,
		Background: true, Kinds: []string{"async-job"}, Reported: true})
	h.wantEndedLine(cause)
	because := "Your previous process ended because " + cause + ". You were not in a turn then."
	if got := RelaunchSentence(h.claim().EndedWork); !strings.HasPrefix(got, because) {
		t.Errorf("the relaunch is told %q, want it to begin %q", got, because)
	}
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

// A suspension that lands over a process the daemon found gone (suspendOverADeadProcess, the
// recovery a fresh session makes): either no death was judged for it, because the recovery's own
// read is the first to find it gone, or the death that was judged could not write the take-back of
// its task. Either way the suspension lets a dead process go, so the end is recorded as the death it
// is, under the runtime's account of it (endDied, deathCause), and not as the suspension: the
// record names the dead turn and its task, and the subagent the launch's report named. Where the
// death was judged, its own record stands and the suspension adds none, so one line is written
// either way.
func TestARelaunchRecordsADeathASuspensionLandsOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone func(h *harness) runtime.Observation
	}{
		{name: "the recovery's read is the first to find the process gone", gone: func(h *harness) runtime.Observation {
			h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
				h.t.Fatalf("the read of the claim's process answered %q, want %q", got, ProcessGone)
			}
			return runtime.Observation{Kind: runtime.Gone}
		}},
		{name: "the death the sweep judged could not take its task back", gone: func(h *harness) runtime.Observation {
			observation := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}
			h.store.fail("PutDelivery", errBoom)
			if err := h.handle(RuntimeObservation{Observation: observation}); err == nil {
				h.t.Fatal("the death was judged whole, want the take-back the store would not write")
			}
			h.store.fail("PutDelivery", nil)
			return observation
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateWorking)
			h.reportActivity("subagent")
			dead, at := h.generation(), h.clock.Now()
			observation := tc.gone(h)

			h.must(RequestSuspend{Claim: h.token, Reason: "a fresh session was asked for", By: SuspendedByOperator, NeverHold: true})

			h.wantState(StateSuspended)
			cause := endDied(deathCause(observation))
			h.wantEnded(EndedWork{At: at, Cause: cause, Generation: dead, Turn: true, TaskTurn: true,
				Background: true, Kinds: []string{"subagent"}, Reported: true})
			h.wantEndedLine(cause)
		})
	}
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
// runs in the very decision that decided it, a subagent reported running or not — the runtime's
// Resume is called before that decision returns — and what it ended is a record and a line
// beside it. A store that cannot answer the read costs the record its report — the process is
// recorded as one the daemon has no report of — and never the relaunch.
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
		h.wantEnded(EndedWork{At: at, Cause: endStalePlugin, Generation: stale,
			Background: true, Kinds: []string{"subagent"}, Reported: true})
	})

	t.Run("a read the store does not answer", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		h.reportActivity("subagent")
		h.store.fail("ClaimActivity", errBoom)
		died, at := h.generation(), h.clock.Now()

		h.observe(runtime.Gone)

		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(runtime.Observation{Kind: runtime.Gone})), Generation: died,
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
	h.relaunched()
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	second, at := h.generation(), h.clock.Now()
	if second == first {
		t.Fatalf("the relaunch kept launch %d, want a new one", first)
	}

	h.observe(runtime.Gone)

	h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(runtime.Observation{Kind: runtime.Gone})), Generation: second, Turn: true, TaskTurn: true})
}

// Unknown is not silence: a process that ran a turn and left no report of its launch — a Claude
// Code worker reports only at its Stop hook, a stale plugin's report is refused — is recorded as
// one the daemon has no report of (EndedWork.Reported false), whose sentence says that any
// background work it started is gone (relaunchSentence), though no turn was running and no task
// was in one when it was let go.
func TestARelaunchRecordsAProcessWithNoReportAsUnknown(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	died, at := h.generation(), h.clock.Now()

	h.observe(runtime.Gone)

	cause := endDied(deathCause(runtime.Observation{Kind: runtime.Gone}))
	h.wantEnded(EndedWork{At: at, Cause: cause, Generation: died})
	h.wantEndedLine(cause)
}

// A report read before the cut turn began is no report of that turn, as the idle rule holds
// (pods.IdleClaim.Reported): a Claude Code worker reports only at its Stop hook, and Oh My Pi at a
// turn's end and otherwise only when what runs has changed (pi-legion's legion.ts), so when a turn
// of its task is cut off, its newest report can be the turn before's, which says nothing of what
// the cut turn started. The process ran a turn and reported — nothing running, or a subagent — and
// a later turn of the task began after that report; the process then died in it. The record names
// the turn and the task and says the daemon has no report of that turn (EndedWork.Reported false),
// and it keeps the old report's background work, which ran in that process (recordEnd). So the
// task taken back is sent naming the subagent the old report named, saying that the daemon has no
// report of the turn and that its subagents and background jobs are gone, and — where the old
// report named nothing — that every other one is gone too (relaunchSentence). The controls are
// reports read after the turn began: each is the turn's own, counted as a report, and the task goes
// back saying that the turn's subagents and background jobs are gone, with no word that the daemon
// has no report, and naming the subagent one of them names.
func TestARelaunchRecordsAReportOlderThanTheCutTurnAsNoReport(t *testing.T) {
	for _, row := range []struct {
		name string
		// before is whether the report is read before the task's turn begins.
		before   bool
		kinds    []string
		reported bool
		// says and saysNot are what the task taken back is sent saying, and not saying.
		says, saysNot []string
	}{
		{name: "a report before the turn began said nothing runs", before: true,
			says: []string{saysNoReport, saysTurnGoneUnreported, saysEveryOtherGone}, saysNot: []string{"stopped with that process", saysTurnGone}},
		{name: "a report before the turn began named a subagent", before: true, kinds: []string{"subagent"},
			says: []string{saysSubagentStopped, saysNoReport, saysTurnGoneUnreported}, saysNot: []string{saysEveryOtherGone, saysTurnGone}},
		{name: "a report after the turn began said nothing runs", reported: true,
			says: []string{saysTurnGone}, saysNot: []string{saysNoReport, saysTurnGoneUnreported, saysEveryOtherGone, "stopped with that process"}},
		{name: "a report after the turn began named a subagent", kinds: []string{"subagent"}, reported: true,
			says: []string{saysSubagentStopped, saysTurnGone}, saysNot: []string{saysNoReport, saysTurnGoneUnreported, saysEveryOtherGone}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			if row.before {
				h.reportActivity(row.kinds...)
				h.advance(time.Second)
			}
			h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
			h.must(StreamTurnStart{Claim: h.token})
			if !row.before {
				h.advance(time.Second)
				h.reportActivity(row.kinds...)
			}
			died, at := h.generation(), h.clock.Now()

			h.observe(runtime.Gone)

			h.wantState(StateLaunching)
			h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(runtime.Observation{Kind: runtime.Gone})),
				Generation: died, Turn: true, TaskTurn: true, Background: len(row.kinds) > 0, Kinds: row.kinds,
				Reported: row.reported})
			p := h.pending()
			if !p.Interrupted {
				t.Fatalf("the task after the death is %+v, want it taken back from the turn", p)
			}
			message, _ := p.message(h.claim())
			wantSays(t, message, row.says...)
			wantSaysNot(t, message, row.saysNot...)
		})
	}
}

// A process that dies idle, after a turn no report covers, keeps what its older report saw: the
// process reported a subagent, then ran a turn of a task to its end, and its turn-end report never
// landed; it then died in no turn. The record says no turn was cut off and that the daemon has no
// report of the process's last turn (EndedWork.Reported false), and keeps the subagent the older
// report named (recordEnd). So the sentence its next process is told names the subagent and says
// that the subagents and background jobs of that last turn are gone too (relaunchSentence), rather
// than dropping the subagent for an unknown. The control is the same subagent reported after the
// turn began: the record holds a report of the last turn, and the sentence names the subagent and
// says nothing of a missing report.
func TestARelaunchOfAnIdleProcessKeepsWhatItsOlderReportSaw(t *testing.T) {
	const saysLastTurnGone = "So the subagents and background jobs of that turn are gone too."
	for _, row := range []struct {
		name string
		// before is whether the report is read before the last turn begins.
		before   bool
		reported bool
		// says and saysNot are what the relaunch's sentence says, and does not say.
		says, saysNot []string
	}{
		{name: "a report before the last turn began named a subagent", before: true,
			says:    []string{saysSubagentStopped, "The daemon has no report of the background work of your last turn.", saysLastTurnGone},
			saysNot: []string{"no report of your background work in that process", "Your tool calls in flight"}},
		{name: "a report after the last turn began named a subagent", reported: true,
			says:    []string{saysSubagentStopped, "You were not in a turn then."},
			saysNot: []string{saysNoReport, saysLastTurnGone}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			if row.before {
				h.reportActivity("subagent")
				h.advance(time.Second)
			}
			h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
			h.must(StreamTurnStart{Claim: h.token})
			if !row.before {
				h.advance(time.Second)
				h.reportActivity("subagent")
			}
			h.advance(time.Second)
			h.must(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)
			died := h.generation()

			h.observe(runtime.Gone)

			ended := h.claim().EndedWork
			if ended.Generation != died || ended.Turn || !ended.Background || !slices.Equal(ended.Kinds, []string{"subagent"}) || ended.Reported != row.reported {
				t.Fatalf("the record of the idle process's end is %+v, want generation %d, no turn, the subagent and Reported %v", ended, died, row.reported)
			}
			sentence := RelaunchSentence(ended)
			wantSays(t, sentence, row.says...)
			wantSaysNot(t, sentence, row.saysNot...)
		})
	}
}

// A turn a busy answer recorded (recordBusyTurn) starts, for the claim, at that answer: the daemon
// never read the turn's agent_start, so the busy answer is its first word that the turn exists
// (Claim.LastTurnStart), and a report read before it says nothing of that turn, as a report read
// before a start the daemon did read says nothing of its turn
// (TestARelaunchRecordsAReportOlderThanTheCutTurnAsNoReport). The process ran a turn and reported —
// nothing running, or a subagent — and was then sent a task, which Oh My Pi answered busy: the
// agent is in a turn of its own, an Envoy notice's, whose start the daemon never read. The process
// dies in that turn. The record names the turn, says the daemon has no report of it
// (EndedWork.Reported false) and keeps the old report's background work, so the task the busy
// answer left waiting is sent naming the subagent the old report named, saying that the daemon has
// no report of the turn and that its subagents and background jobs are gone, and — where the old
// report named nothing — that every other one is gone too. The control is a report read after the
// busy answer, saying nothing runs: it is the turn's own, counted as a report, and the task goes
// saying that the turn's subagents and background jobs are gone, with no word that the daemon has
// no report.
func TestARelaunchRecordsAReportOlderThanABusyAnswersTurnAsNoReport(t *testing.T) {
	for _, row := range []struct {
		name string
		// before is whether the report is read before the busy answer.
		before   bool
		kinds    []string
		reported bool
		// says and saysNot are what the task the busy answer left waiting is sent saying, and not
		// saying.
		says, saysNot []string
	}{
		{name: "a report before the busy answer said nothing runs", before: true,
			says: []string{saysNoReport, saysTurnGoneUnreported, saysEveryOtherGone}, saysNot: []string{"stopped with that process", saysTurnGone}},
		{name: "a report before the busy answer named a subagent", before: true, kinds: []string{"subagent"},
			says: []string{saysSubagentStopped, saysNoReport, saysTurnGoneUnreported}, saysNot: []string{saysEveryOtherGone, saysTurnGone}},
		{name: "a report after the busy answer said nothing runs", reported: true,
			says: []string{saysTurnGone}, saysNot: []string{saysNoReport, saysTurnGoneUnreported, saysEveryOtherGone, "stopped with that process"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			if row.before {
				h.reportActivity(row.kinds...)
				h.advance(time.Second)
			}
			h.conn.RefusePrompt(busyAnswer)
			h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
			h.wantState(StateWorking)
			answered := h.clock.Now()
			for where, start := range map[string]time.Time{"the machine": h.claim().LastTurnStart, "the store": h.store.load(h.token).LastTurnStart} {
				if !start.Equal(answered) {
					t.Errorf("%s dates the turn the busy answer recorded from %v, want the answer's %v", where, start, answered)
				}
			}
			if !row.before {
				h.advance(time.Second)
				h.reportActivity(row.kinds...)
			}
			died, at := h.generation(), h.clock.Now()

			h.observe(runtime.Gone)

			h.wantState(StateLaunching)
			h.wantEnded(EndedWork{At: at, Cause: endDied(deathCause(runtime.Observation{Kind: runtime.Gone})),
				Generation: died, Turn: true, Background: len(row.kinds) > 0, Kinds: row.kinds, Reported: row.reported})
			p := h.pending()
			if p.Interrupted || !p.ConfirmedAt.IsZero() {
				t.Fatalf("the task after the death is %+v, want the one the busy answer left waiting, in no turn", p)
			}
			message, _ := p.message(h.claim())
			wantSays(t, message, row.says...)
			wantSaysNot(t, message, row.saysNot...)
		})
	}
}

// Only a process in no turn, with no task in one and no background work reported running, is
// recorded as having ended nothing, and then only where it left a report or never ran a turn
// (recordEnd): one that ran a turn and reported nothing running, released idle; and one that never
// ran a turn and reported nothing at all, found dead before its first.
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
