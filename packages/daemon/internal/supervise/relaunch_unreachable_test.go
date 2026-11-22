package supervise

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The relaunch of a claim Envoy cannot reach (R5): the controller's `relaunch`, or the daemon's own
// of its controller, stops the claim's process and resumes its same session, charging nothing, once
// the guard lets it through: no step in the last 20 minutes, no activity report in the last 20
// minutes of a turn the claim is still in, an open call that is a `wait` if there is one, and an
// activity report since the launch that shows no background job and no subagent running.

// relaunchCause is the cause a relaunch's caller hands the machine, which the claim records.
const relaunchCause = "Envoy could not reach its session for 20 minutes, so the project's controller relaunched it"

// inPlaceRuntime is the fake runtime answering the relaunch's two questions as a case sets them,
// recording each call, as the Sandbox runtime answers them for a role in an issue pod.
type inPlaceRuntime struct {
	runtime.Runtime
	mu       sync.Mutex
	refusal  error
	stopErr  error
	refusals []bool
	stops    []bool
}

func (r *inPlaceRuntime) RelaunchRefusal(_ context.Context, _ runtime.Locator, mayReplacePod bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals = append(r.refusals, mayReplacePod)
	return r.refusal
}

func (r *inPlaceRuntime) StopForRelaunch(_ context.Context, _ runtime.Locator, mayReplacePod bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops = append(r.stops, mayReplacePod)
	return r.stopErr
}

func (r *inPlaceRuntime) counts() (refusals, stops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refusals), len(r.stops)
}

// waitingSilently walks the claim into a turn of a delivered task whose last call is an open
// `wait`, older than the 20 minutes the guard asks, with an activity report of this launch that
// shows nothing running: the claim the guard lets through.
func (h *harness) waitingSilently() string {
	h.t.Helper()
	sent := h.workingOnATask("implement the plan")
	h.stepped("wait", "toolu_wait_0", `{"jobs":["bg_1"]}`, false)
	h.reportActivity()
	h.advance(UnreachableAfter + time.Minute)
	return sent
}

// relaunchOf is the request the daemon's relaunch posts, with the step record the store holds.
func (h *harness) relaunchOf(mayReplacePod bool) RequestRelaunch {
	return RequestRelaunch{Claim: h.token, Step: h.store.step(h.token), Cause: relaunchCause, MayReplacePod: mayReplacePod}
}

func TestARelaunchStopsTheProcessAndResumesTheSameSessionChargingNothing(t *testing.T) {
	h := newHarness(t)
	sent := h.waitingSilently()
	generation := h.generation()

	h.must(h.relaunchOf(false))

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if h.generation() != generation+1 {
		t.Fatalf("generation = %d, want the next launch %d", h.generation(), generation+1)
	}
	h.wantCalls("Suspend", 1)
	if resumes := h.wantCalls("Resume", 1); resumes[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %q, want the claim's own session %q", resumes[0].Spec.ResumeSessionFile, sessionFile)
	}
	if p := h.pending(); !p.Interrupted || p.ID == sent {
		t.Fatalf("pending after the relaunch = %+v, want the task taken back and interrupted", p)
	}
	if got := h.claim().EndedWork.Cause; got != relaunchCause {
		t.Fatalf("the claim records cause %q, want the relaunch's own %q", got, relaunchCause)
	}

	h.relaunched()
	if resent := h.wantPrompts(2)[1]; !strings.Contains(resent.Message, relaunchCause) {
		t.Fatalf("the task came back as %q, want the daemon's cut-turn note naming the relaunch", resent.Message)
	}
	if h.store.load(h.token).Session != session {
		t.Fatalf("session after the relaunch = %q, want the same session", h.store.load(h.token).Session)
	}
	h.wantBudgets(Budgets{})
}

// An idle claim the relaunch reaches records its cause too, though its process ended no turn and
// nothing it reported runs: the relaunch's cause is on the claim whatever the process was doing.
func TestARelaunchOfAnIdleClaimRecordsItsCause(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.reportActivity()
	h.advance(UnreachableAfter + time.Minute)

	h.must(h.relaunchOf(false))

	h.wantState(StateLaunching)
	if got := h.claim().EndedWork.Cause; got != relaunchCause {
		t.Fatalf("the claim records cause %q, want %q", got, relaunchCause)
	}
	h.wantBudgets(Budgets{})
}

func TestTheRelaunchGuardRefusesWithNothingStopped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(h *harness)
		says  string
	}{
		{
			name: "a step in the last 20 minutes",
			setUp: func(h *harness) {
				h.workingOnATask("implement the plan")
				h.reportActivity()
				h.advance(UnreachableAfter + time.Minute)
				h.stepped("wait", "toolu_wait_1", `{}`, false)
				h.advance(5 * time.Minute)
			},
			says: "step",
		},
		{
			name: "an open call that is not a wait",
			setUp: func(h *harness) {
				h.workingOnATask("implement the plan")
				h.stepped("bash", "toolu_bash_0", toolArgs, false)
				h.reportActivity()
				h.advance(UnreachableAfter + time.Minute)
			},
			says: "bash",
		},
		{
			name: "a report that shows background work",
			setUp: func(h *harness) {
				h.workingOnATask("implement the plan")
				h.stepped("wait", "toolu_wait_0", `{}`, false)
				h.reportActivity("async-job")
				h.advance(UnreachableAfter + time.Minute)
			},
			says: "background",
		},
		{
			name: "a report that shows a subagent",
			setUp: func(h *harness) {
				h.workingOnATask("implement the plan")
				h.stepped("wait", "toolu_wait_0", `{}`, false)
				h.reportActivity("subagent")
				h.advance(UnreachableAfter + time.Minute)
			},
			says: "subagent",
		},
		{
			name: "no report since its launch",
			setUp: func(h *harness) {
				h.workingOnATask("implement the plan")
				h.stepped("wait", "toolu_wait_0", `{}`, false)
				h.advance(UnreachableAfter + time.Minute)
			},
			says: "no activity report",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setUp(h)
			before := h.claim()

			err := h.handle(h.relaunchOf(false))

			if !errors.Is(err, ErrRelaunchGuard) {
				t.Fatalf("relaunch = %v, want the guard's refusal", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal %q does not say %q", err, tc.says)
			}
			h.wantState(before.State)
			h.wantCalls("Suspend", 0)
			h.wantCalls("Resume", 0)
			if h.generation() != before.Generation {
				t.Fatalf("generation = %d, want it unmoved at %d", h.generation(), before.Generation)
			}
		})
	}
}

// The guard is one function the daemon's sweep reads too, for the notice that says the guard
// would now let a refused claim through; a named bash service is in no activity report and so is
// no work, as the idle release reads it.
func TestTheRelaunchGuardReadsTheRecordItIsHanded(t *testing.T) {
	now := epoch.Add(time.Hour)
	quiet := Claim{
		Step:       StepRecord{At: now.Add(-30 * time.Minute), Tool: "wait", CallID: "c1", StartedAt: now.Add(-30 * time.Minute)},
		ActivityAt: now.Add(-25 * time.Minute),
	}
	if why := RelaunchGuard(quiet, time.Time{}, now); why != "" {
		t.Fatalf("RelaunchGuard of a claim in a long wait with nothing running = %q, want it let through", why)
	}
	ended := quiet
	ended.Step = StepRecord{At: now.Add(-30 * time.Minute), Tool: "bash", CallID: "c2", StartedAt: now.Add(-31 * time.Minute), EndedAt: now.Add(-30 * time.Minute)}
	if why := RelaunchGuard(ended, time.Time{}, now); why != "" {
		t.Fatalf("RelaunchGuard of a claim whose last call ended = %q, want it let through", why)
	}
	if why := RelaunchGuard(quiet, now.Add(-time.Minute), now); why == "" {
		t.Fatal("RelaunchGuard let through a claim the machine saw step a minute ago")
	}
}

// R5's turn heartbeat: a session in a turn sends its activity report again every 5 minutes
// whatever it says (pi-legion's TURN_ACTIVITY_HEARTBEAT_MS), from a timer of its own process, so a
// report made in that turn in the last 20 minutes is the process still running the turn, though
// its worker stream shows no step. That is a `wait` on a named `bash` service on the pinned Oh My
// Pi: the call blocks with no step until the service ends, and the service is in no report. The
// claim the guard refuses here is that one, measured: a wait 25 minutes old, and the turn's last
// report 4 minutes old, which names nothing running.
func TestTheRelaunchGuardRefusesAClaimWhoseSessionReportedInItsTurn(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")

	h.wantRelaunchGuardedInTurn("a claim in a turn")
}

// A turn Oh My Pi starts under a compaction — a follow-up an Envoy delivery queued while the
// session was being compacted — is the claim's turn once the compaction ends and leaves the claim
// working for it (compactEnded), so the guard reads the claim in that turn as it reads any other.
// Read against the start of the turn before the compaction, which has ended, the claim would be in
// no turn, and the relaunch would cut the live one. The daemon's sweep asks the guard of the
// stored claim, which the compaction's end wrote.
func TestTheRelaunchGuardRefusesAClaimInATurnThatBeganUnderACompaction(t *testing.T) {
	h := newHarness(t)
	end := h.compactingIdle()
	h.must(StreamTurnStart{Claim: h.token})
	if err := end(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	h.wantRelaunchGuardedInTurn("a claim in a turn begun under a compaction")
}

// compactingIdle takes an idle claim, whose last turn has ended, for a compaction that Oh My Pi
// is still writing: the claim stored working with no turn begun under the compaction yet. end lets
// Oh My Pi answer the compaction and returns what Compact answered, once the machine has decided
// everything the answer started. A test that restarts the daemon instead leaves end uncalled: the
// machine that asked is dropped, as a stopped daemon's is, and its Compact waits on the held
// connection until the harness's context ends.
func (h *harness) compactingIdle() (end func() error) {
	h.t.Helper()
	h.reach(StateIdle)
	h.advance(time.Minute)
	release := h.conn.HoldCompact()
	asked := h.m
	compacted := make(chan error, 1)
	go func() { compacted <- asked.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(h.t, h)
	return func() error {
		release()
		err := <-compacted
		asked.Wait()
		return err
	}
}

// wantRelaunchGuardedInTurn walks a claim in a turn through a `wait` on a named `bash` service 25
// minutes old whose session reported in that turn 4 minutes ago, and wants the relaunch refused by
// the guard's rule for a report in the turn, naming the report's time, with nothing stopped or
// started — both by the machine and by the guard read of the stored claim the daemon's sweep makes.
// That is R5's turn heartbeat, measured: a session in a turn sends its activity report again every
// 5 minutes whatever it says (pi-legion's TURN_ACTIVITY_HEARTBEAT_MS), from a timer of its own
// process, so a report made in that turn in the last 20 minutes is the process still running the
// turn, though its worker stream shows no step, as a `wait` on a named `bash` service does on the
// pinned Oh My Pi, and the service is in no report.
func (h *harness) wantRelaunchGuardedInTurn(what string) {
	h.t.Helper()
	h.wantState(StateWorking)
	h.stepped("wait", "toolu_wait_0", `{"jobs":["activity-service"]}`, false)
	h.advance(21 * time.Minute)
	h.reportActivity()
	stamp := h.clock.Now().UTC().Format(time.RFC3339)
	h.advance(4 * time.Minute)
	generation := h.generation()

	err := h.handle(h.relaunchOf(false))

	if !errors.Is(err, ErrRelaunchGuard) || !strings.Contains(err.Error(), stamp) {
		h.t.Fatalf("relaunch of %s whose session reported 4 minutes ago in its turn = %v, want the guard's refusal naming the report at %s", what, err, stamp)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 0)
	if h.generation() != generation {
		h.t.Fatalf("generation = %d, want it unmoved at %d", h.generation(), generation)
	}
	if why := RelaunchGuard(h.store.load(h.token), time.Time{}, h.clock.Now()); !strings.Contains(why, stamp) {
		h.t.Fatalf("RelaunchGuard of the stored claim = %q, want a refusal naming the report at %s", why, stamp)
	}
}

// The start of a turn begun under a compaction is written when the turn starts, not only at the
// compaction's end: a daemon that restarts in between restores the claim from the store, and a
// start held in memory alone is lost with that daemon. Restored with the start of the turn before
// the compaction, which has ended, the claim reads as in no turn, and the guard lets a relaunch
// cut the live `wait`.
func TestATurnBegunUnderACompactionKeepsItsStartAcrossARestart(t *testing.T) {
	h := newHarness(t)
	h.compactingIdle()
	h.must(StreamTurnStart{Claim: h.token})
	started := h.clock.Now()
	if stored := h.store.load(h.token).LastTurnStart; !stored.Equal(started) {
		t.Errorf("the store dates the turn begun under the compaction from %v, want its start %v", stored, started)
	}

	h.restart()

	h.wantState(StateWorking)
	h.wantRelaunchGuardedInTurn("a claim restored in a turn begun under a compaction")
}

// An agent that says it is streaming when a restarted daemon asks it of a claim stored working is
// in a turn, and that answer is the restarted daemon's first word of it, as it is for a restored
// ready or idle claim (sendPending's askFirst branch): a turn that began while no daemon read the
// stream, a turn under a compaction among them, is dated nowhere else. So the answer dates the
// turn where the record holds none running, and the guard reads the claim in it.
func TestAStreamingAnswerDatesATurnTheRecordHoldsNoStartOf(t *testing.T) {
	h := newHarness(t)
	h.compactingIdle()
	h.restart()
	h.conn.SetStreaming(true)

	h.connect()

	h.wantState(StateWorking)
	answered := h.clock.Now()
	for where, start := range map[string]time.Time{"the machine": h.claim().LastTurnStart, "the store": h.store.load(h.token).LastTurnStart} {
		if !start.Equal(answered) {
			t.Errorf("%s dates the turn the agent says it is in from %v, want the answer's %v", where, start, answered)
		}
	}
	h.wantRelaunchGuardedInTurn("a claim whose agent answered it is streaming")
}

// A process that froze sends no report from then on, since its heartbeat runs on its own event
// loop: 20 minutes after the turn's last report the guard lets the claim through, as it let every
// quiet wait through before the heartbeat.
func TestTheRelaunchGuardLetsThroughAClaimWhoseTurnsLastReportIsOlderThan20Minutes(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	h.stepped("wait", "toolu_wait_0", `{"jobs":["activity-service"]}`, false)
	h.advance(4 * time.Minute)
	h.reportActivity()
	h.advance(UnreachableAfter + time.Minute)

	h.must(h.relaunchOf(false))

	h.wantState(StateLaunching)
	h.wantCalls("Suspend", 1)
}

// A claim in no turn is judged as before: its report says what runs in its process, and a fresh one
// that names nothing running lets an idle claim through, since no turn of it is cut.
func TestTheRelaunchGuardLetsThroughAnIdleClaimWithAFreshReport(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.advance(UnreachableAfter + time.Minute)
	h.reportActivity()
	h.advance(time.Minute)

	h.must(h.relaunchOf(false))

	h.wantState(StateLaunching)
	h.wantCalls("Suspend", 1)
}

// Only a report made in the turn the claim is in says that turn is running: a report from before
// the turn began is the turn before's, or the ready's. The claim's turn start (Claim.LastTurnStart)
// and its step record are two writes of the store's (putClaim, PutClaimStep), so a record whose
// step write the store refused holds a turn start newer than its last step, which is what isolates
// the rule here: every other condition lets the claim through.
func TestTheRelaunchGuardCountsOnlyAReportMadeInTheTurn(t *testing.T) {
	now := epoch.Add(time.Hour)
	before := Claim{
		State:         StateWorking,
		LastTurnStart: now.Add(-10 * time.Minute),
		LastTurnEnd:   now.Add(-40 * time.Minute),
		Step:          StepRecord{At: now.Add(-30 * time.Minute), Tool: "wait", CallID: "c1", StartedAt: now.Add(-30 * time.Minute)},
		ActivityAt:    now.Add(-11 * time.Minute),
	}
	if why := RelaunchGuard(before, time.Time{}, now); why != "" {
		t.Fatalf("RelaunchGuard of a claim whose only report is older than its turn = %q, want it let through", why)
	}
	in := before
	in.ActivityAt = now.Add(-4 * time.Minute)
	if why := RelaunchGuard(in, time.Time{}, now); !strings.Contains(why, in.ActivityAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("RelaunchGuard of a claim that reported 4 minutes ago in its turn = %q, want a refusal naming the report's time", why)
	}
	idle := in
	idle.State, idle.LastTurnEnd = StateIdle, now.Add(-5*time.Minute)
	if why := RelaunchGuard(idle, time.Time{}, now); why != "" {
		t.Fatalf("RelaunchGuard of an idle claim with a fresh report = %q, want it let through", why)
	}
}

// The report in the turn is the last rule the guard reads: it catches only a claim every older
// rule lets through. A claim in a long `bash` call, or with background work or a subagent running,
// also reports in its turn while its process runs, and the refusal names that claim's own reason,
// which tells the controller and a person what the claim is doing, rather than only that it runs.
func TestTheRelaunchGuardNamesAClaimsOwnReasonBeforeItsReportInTheTurn(t *testing.T) {
	now := epoch.Add(time.Hour)
	inTurn := Claim{
		State:         StateWorking,
		LastTurnStart: now.Add(-40 * time.Minute),
		LastTurnEnd:   now.Add(-50 * time.Minute),
		ActivityAt:    now.Add(-4 * time.Minute),
	}
	bash := inTurn
	bash.Step = StepRecord{At: now.Add(-25 * time.Minute), Tool: "bash", CallID: "c1", StartedAt: now.Add(-25 * time.Minute)}
	if why := RelaunchGuard(bash, time.Time{}, now); !strings.Contains(why, "its open call is bash") {
		t.Fatalf("RelaunchGuard of a claim in a bash call that reports in its turn = %q, want the open call named", why)
	}
	busy := inTurn
	busy.Step = StepRecord{At: now.Add(-25 * time.Minute), Tool: "wait", CallID: "c2", StartedAt: now.Add(-25 * time.Minute)}
	busy.ActivityBackground, busy.ActivityKinds = true, []string{"subagent"}
	if why := RelaunchGuard(busy, time.Time{}, now); !strings.Contains(why, "shows background work running: subagent") {
		t.Fatalf("RelaunchGuard of a claim with a subagent running that reports in its turn = %q, want the background work named", why)
	}
	quiet := busy
	quiet.ActivityBackground, quiet.ActivityKinds = false, nil
	if why := RelaunchGuard(quiet, time.Time{}, now); !strings.Contains(why, "so its process still runs that turn") {
		t.Fatalf("RelaunchGuard of a claim in a wait whose report names nothing = %q, want the report in its turn named", why)
	}
}

func TestARelaunchTheRuntimeRefusesStopsNothing(t *testing.T) {
	for _, refusal := range []error{runtime.ErrLauncherNotConnected, runtime.ErrRelaunchReplacesPod} {
		t.Run(refusal.Error(), func(t *testing.T) {
			h := newBareHarness(t)
			rt := &inPlaceRuntime{Runtime: h.rt, refusal: refusal}
			h.deps.Runtime = rt
			c := queuedClaim()
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				t.Fatal(err)
			}
			h.token = c.Token
			h.start(c)
			h.waitingSilently()
			before := h.claim()

			err := h.handle(h.relaunchOf(false))

			if !errors.Is(err, refusal) {
				t.Fatalf("relaunch = %v, want the runtime's %v", err, refusal)
			}
			if refusals, stops := rt.counts(); refusals != 1 || stops != 0 {
				t.Fatalf("the runtime was asked %d refusals and %d stops, want one question and no stop", refusals, stops)
			}
			h.wantState(before.State)
			h.wantCalls("Suspend", 0)
			h.wantCalls("Resume", 0)
		})
	}
}

// The tree lifecycle's recheck (checkLaunch) refuses a launch for reasons that need nothing
// stopped: a reserved cleanup of the claim's tree, or a bound epoch that is no longer the open
// one. The relaunch asks it before the stop, as it asks the runtime's own refusals, so a relaunch
// it refuses stops nothing, takes nothing back and records no relaunch. Asked only after the stop,
// the refusal would leave the process gone with no launch after it, the claim still recording it,
// and the controller's next try refused as a relaunch made within 6 hours. It is a refusal of the
// claim as the others are (RefusedError), which the API answers with the code that says nothing
// was stopped, rather than a failure the controller must read as a claim that may be stopped.
func TestARelaunchTheTreeLifecycleRefusesStopsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(store *epochStore)
		says   string
	}{
		{name: "its tree's cleanup reserved", refuse: func(s *epochStore) { s.reserved = true }, says: "cleanup is reserved"},
		{name: "its bound epoch no longer open", refuse: func(s *epochStore) { s.epoch++ }, says: "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &epochStore{epoch: 1}
			h := lifecycleHarness(t, store, queuedClaim())
			sent := h.waitingSilently()
			before := h.claim()
			tc.refuse(store)

			err := h.handle(h.relaunchOf(false))

			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("relaunch = %v, want the lifecycle's refusal saying %q", err, tc.says)
			}
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.State != StateWorking || refused.Request != "relaunch" {
				t.Fatalf("relaunch = %v (%T), want the relaunch refused at working, as every refusal that stops nothing is", err, err)
			}
			h.wantCalls("Suspend", 0)
			h.wantCalls("Resume", 0)
			h.wantState(StateWorking)
			if c := h.claim(); c.Generation != before.Generation || c.Locator == nil || *c.Locator != *before.Locator || !c.LastRelaunch.IsZero() {
				t.Fatalf("the refused relaunch left the claim at generation %d on %+v with a relaunch at %v; want it on its process at %d with no relaunch",
					c.Generation, c.Locator, c.LastRelaunch, before.Generation)
			}
			if p := h.pending(); p.ID != sent || p.Interrupted {
				t.Fatalf("pending = %+v, want the task the turn runs left as it was", p)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// A refusal can still come once the stop has run: a cleanup reserved, or a store that does not
// answer the lifecycle's recheck, while the stop waits on the runtime, which for a Sandbox pod is
// minutes. The stop lets the process go, as a retirement's stop does, so the launch's refusal
// meets a claim that records no process and is answered as a retirement's is (relaunchRefused): a
// reserved cleanup suspends the claim for the tree's close, its session kept, and a check the
// store did not answer is tried again at the probe interval, which then resumes the same session.
// Neither leaves the claim recording a process that is gone with nothing to launch it. The second
// is a relaunch still going on, not a refusal: the process is stopped, the relaunch recorded, and
// the daemon makes the launch itself, so it answers as a relaunch that ran. Answered as a failure,
// it sent the controller to read a claim that may be stopped, and a second try met the refusal of
// a relaunch made within 6 hours, which is a case for a person.
func TestARelaunchRefusedAfterItsStopLeavesNoClaimRecordingAStoppedProcess(t *testing.T) {
	t.Run("its tree's cleanup reserved while the stop waits", func(t *testing.T) {
		store := &epochStore{epoch: 1}
		h := lifecycleHarness(t, store, queuedClaim())
		h.waitingSilently()
		h.m.deps.Runtime = &reservingSuspend{Runtime: h.rt, store: store}

		err := h.handle(h.relaunchOf(false))

		if !errors.Is(err, treelifecycle.ErrCleanupReserved) {
			t.Fatalf("relaunch = %v, want the reservation's wait", err)
		}
		h.wantCalls("Suspend", 1)
		h.wantCalls("Resume", 0)
		for where, c := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(h.token)} {
			if c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon || c.Locator != nil || c.Session != session {
				t.Errorf("%s holds the claim %s by %q on %+v with session %q; want it suspended by the daemon for the tree's close, with no process and its session kept",
					where, c.State, c.SuspendedBy, c.Locator, c.Session)
			}
			if c.EndedWork.Cause != relaunchCause {
				t.Errorf("%s records the stop's end as caused by %q, want the relaunch's %q", where, c.EndedWork.Cause, relaunchCause)
			}
		}
		h.wantBudgets(Budgets{})
	})
	t.Run("its store not answering the check while the stop waits", func(t *testing.T) {
		store := &epochStore{epoch: 1}
		h := lifecycleHarness(t, store, queuedClaim())
		h.waitingSilently()
		generation := h.generation()
		h.m.deps.Runtime = &unansweredSuspend{Runtime: h.rt, store: store, checks: 1}

		if err := h.handle(h.relaunchOf(false)); err != nil {
			t.Fatalf("relaunch whose launch waits for the store = %v, want it answered as a relaunch that ran", err)
		}
		h.wantCalls("Suspend", 1)
		h.wantCalls("Resume", 0)
		if c := h.claim(); c.State != StateLaunching || c.Locator != nil || c.Generation != generation {
			t.Fatalf("the unchecked launch left the claim %s at generation %d on %+v; want it launching at %d with no process, its launch to be tried again",
				c.State, c.Generation, c.Locator, generation)
		}

		h.advance(testProbe)

		resumed := h.wantCalls("Resume", 1)[0]
		if c := h.claim(); c.State != StateLaunching || c.Generation != generation+1 || resumed.Spec.ResumeSessionFile != sessionFile {
			t.Fatalf("after the probe interval the claim is %s at generation %d resuming %q; want the same session launching at %d",
				c.State, c.Generation, resumed.Spec.ResumeSessionFile, generation+1)
		}
		if got := h.claim().EndedWork.Cause; got != relaunchCause {
			t.Fatalf("the claim records the stop's end as caused by %q, want the relaunch's %q", got, relaunchCause)
		}
		// The launch the probe interval retried is the relaunch's own, so the generation it left the
		// claim at is recorded beside the relaunch, in the store as on the claim: the keeper counts
		// the claim's later launches from it.
		if got := h.claim().LastRelaunchGeneration; got != generation+1 {
			t.Fatalf("after the retried launch the claim records its relaunch's launch at generation %d, want %d", got, generation+1)
		}
		if got := h.store.load(h.token).LastRelaunchGeneration; got != generation+1 {
			t.Fatalf("after the retried launch the store records its relaunch's launch at generation %d, want %d", got, generation+1)
		}
		h.wantBudgets(Budgets{})
	})
}

// The tree lifecycle's recheck is a transaction of the store's (Store.CheckLaunch), and a store
// that does not answer it — a dropped connection, a pool that timed out, a lock wait past the
// request's time — is no refusal of the tree's. The relaunch stops nothing then either, and says
// so with a sentinel of its own, which the API answers with a code that asks for the relaunch
// again. The machine writes no line of its own for it: each caller writes the one line, the API's
// route (api's claimRelaunch) and the daemon's relaunch of its own controller (internal/daemon's
// relaunchOwnController), so one failed check is one line. Read as the tree's refusal, the
// controller would leave the claim to a close or a start of its tree that nothing is about to
// make, and the claim would stay out of reach with nobody acting.
func TestARelaunchWhoseTreeCheckTheStoreDoesNotAnswerStopsNothingAndIsAskedAgain(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	sent := h.waitingSilently()
	before := h.claim()
	store.unanswered = 1

	err := h.handle(h.relaunchOf(false))

	if errors.Is(err, ErrTreeRefusesLaunch) {
		t.Fatalf("relaunch whose tree check the store did not answer = %v, read as the tree's refusal", err)
	}
	var refused *RefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrTreeCheckFailed) || !errors.Is(err, errUnanswered) {
		t.Fatalf("relaunch whose tree check the store did not answer = %v, want a refusal that the check failed, carrying the store's error", err)
	}
	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 0)
	h.wantState(StateWorking)
	if c := h.claim(); c.Generation != before.Generation || c.Locator == nil || *c.Locator != *before.Locator || !c.LastRelaunch.IsZero() {
		t.Fatalf("the refused relaunch left the claim at generation %d on %+v with a relaunch at %v; want it on its process at %d with no relaunch",
			c.Generation, c.Locator, c.LastRelaunch, before.Generation)
	}
	if p := h.pending(); p.ID != sent || p.Interrupted {
		t.Fatalf("pending = %+v, want the task the turn runs left as it was", p)
	}
	if lines := h.logs.lines(errUnanswered.Error()); len(lines) != 0 {
		t.Fatalf("the machine logged %v, want no line of its own: the relaunch's caller writes the one line of the failed check", lines)
	}
	h.wantBudgets(Budgets{})
}

// A fresh admission can open the claim's tree again while the relaunch's stop waits on the
// runtime, leaving the claim bound to an epoch that is no longer open. That is the tree's refusal,
// as a reserved cleanup is, and no store that did not answer: every later try would meet the same
// epoch. So the claim, whose process the stop let go, is suspended for the daemon with its session
// kept, for its tree's next start to bind and launch, nothing is tried again at the probe interval,
// and the relaunch is answered as the refusal it met, not as a relaunch made.
func TestARelaunchWhoseTreeOpensAgainWhileItsStopWaitsSuspendsTheClaimAndTriesNothingAgain(t *testing.T) {
	store := &epochStore{epoch: 1}
	h := lifecycleHarness(t, store, queuedClaim())
	h.waitingSilently()
	h.m.deps.Runtime = &reopeningSuspend{Runtime: h.rt, store: store}

	err := h.handle(h.relaunchOf(false))

	if !errors.Is(err, treelifecycle.ErrEpochStale) {
		t.Fatalf("relaunch whose tree opened again while its stop waited = %v, want the stale epoch it met, not a relaunch made", err)
	}
	h.wantCalls("Suspend", 1)
	for where, c := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(h.token)} {
		if c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon || c.Locator != nil || c.Session != session {
			t.Errorf("%s holds the claim %s by %q on %+v with session %q; want it suspended by the daemon for its tree's next start, with no process and its session kept",
				where, c.State, c.SuspendedBy, c.Locator, c.Session)
		}
	}
	if _, armed := h.m.timers[TimerRelaunch]; armed || h.m.relaunch != nil {
		t.Fatalf("the stale epoch armed a retry (timer %t, relaunch %t), want none: every try meets the same epoch", armed, h.m.relaunch != nil)
	}

	h.advance(testProbe)

	h.wantCalls("Resume", 0)
	h.wantBudgets(Budgets{})
}

// Once the stop has ended the process, nothing the relaunch does after it may leave the claim
// recording that process: the take-back of the task the cut turn ran writes its delivery, and a
// store that refuses that write is the case a stop that waits minutes on the runtime meets first.
// Left recording the stopped process, the claim was found dead by the next sweep, charged a death
// and relaunched as one, under the death's cause rather than the relaunch's, which R5 says
// charges nothing. So the process is let go first, the task taken back in memory, and the write
// tried again before the launch: the launch waits at the probe interval for the take-back's write,
// so no launch is written while the store still holds the task confirmed.
func TestARelaunchWhoseTakeBackTheStoreRefusesLetsItsProcessGoAndLaunchesOnceItIsWritten(t *testing.T) {
	h := newHarness(t)
	sent := h.waitingSilently()
	generation := h.generation()
	stopped := h.locator()
	h.store.fail("PutDelivery", errors.New("the store is down"))

	err := h.handle(h.relaunchOf(false))

	h.wantCalls("Suspend", 1)
	h.wantCalls("Resume", 0)
	c := h.claim()
	if c.Locator != nil || c.State != StateLaunching || c.Generation != generation {
		t.Errorf("after a take-back the store refused the claim is %s at generation %d on %+v; want it launching at %d recording no process, since the stop ended that one",
			c.State, c.Generation, c.Locator, generation)
	}
	if p := c.Pending; p == nil || p.ID == sent || !p.Interrupted || !p.ConfirmedAt.IsZero() {
		t.Errorf("pending after the refused take-back = %+v, want the task taken back from the cut turn, interrupted under a new id", p)
	}
	if c.EndedWork.Cause != relaunchCause {
		t.Errorf("the claim records the stop's end as caused by %q, want the relaunch's %q", c.EndedWork.Cause, relaunchCause)
	}
	if err != nil {
		t.Errorf("relaunch whose take-back the store refused = %v, want it answered as a relaunch made, its launch still to come", err)
	}
	// While the store refuses the write, the launch waits for it.
	h.advance(testProbe)
	if resumes := len(h.calls("Resume")); resumes != 0 {
		t.Errorf("the relaunch launched %d times while the store still refused its take-back, want it waiting", resumes)
	}
	// The store answers again, and the sweep's next report finds the stopped process gone: it
	// charges nothing, since the claim no longer records that process.
	h.store.fail("PutDelivery", nil)
	_ = h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: stopped, Kind: runtime.Gone, At: h.clock.Now()}})
	if b := h.budgets(); b != (Budgets{}) {
		t.Errorf("after the sweep found the stopped process gone the claim is charged %+v, want nothing", b)
	}
	if t.Failed() {
		t.FailNow()
	}

	h.advance(testProbe)

	resumed := h.wantCalls("Resume", 1)[0]
	if c := h.claim(); c.Generation != generation+1 || resumed.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("once the store answers the claim is at generation %d resuming %q; want the same session launching at %d",
			c.Generation, resumed.Spec.ResumeSessionFile, generation+1)
	}
	if stored, ok := h.store.delivery(h.token); !ok || stored.ID == sent || !stored.Interrupted || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("the store holds the delivery %+v, want the task taken back, written before the launch", stored)
	}
	h.wantBudgets(Budgets{})
	h.relaunched()
	if resent := h.wantPrompts(2)[1]; !strings.Contains(resent.Message, relaunchCause) {
		t.Fatalf("the task came back as %q, want the daemon's cut-turn note naming the relaunch", resent.Message)
	}
}

func TestARelaunchWhoseProcessOutlivesItsStopDoesNothingMore(t *testing.T) {
	h := newBareHarness(t)
	rt := &inPlaceRuntime{Runtime: h.rt, stopErr: runtime.ErrProcessOutlivedStop}
	h.deps.Runtime = rt
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.token = c.Token
	h.start(c)
	sent := h.waitingSilently()
	before := h.claim()

	err := h.handle(h.relaunchOf(false))

	if !errors.Is(err, runtime.ErrProcessOutlivedStop) {
		t.Fatalf("relaunch = %v, want the stop the process outlived", err)
	}
	h.wantState(StateWorking)
	h.wantCalls("Resume", 0)
	if p := h.pending(); p.ID != sent || p.Interrupted {
		t.Fatalf("pending = %+v, want the task the turn runs left as it was", p)
	}
	if h.generation() != before.Generation || h.claim().Locator.Incarnation != before.Locator.Incarnation {
		t.Fatal("the claim moved off the process that outlived its stop")
	}
	h.wantBudgets(Budgets{})
}

// The daemon's relaunch of its controller may replace the controller's pod, and only that one
// does: what the request says is what the runtime is asked.
func TestARelaunchHandsTheRuntimeWhetherItMayReplaceThePod(t *testing.T) {
	h := newBareHarness(t)
	rt := &inPlaceRuntime{Runtime: h.rt}
	h.deps.Runtime = rt
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.token = c.Token
	h.start(c)
	h.waitingSilently()

	h.must(h.relaunchOf(true))

	if len(rt.refusals) != 1 || !rt.refusals[0] || len(rt.stops) != 1 || !rt.stops[0] {
		t.Fatalf("the runtime was asked refusals %v and stops %v, want one of each that may replace the pod", rt.refusals, rt.stops)
	}
	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 1)
}

// gatedRelaunchStop holds the relaunch's stop in place until the test lets it through, as a
// Sandbox stop waits on its pod.
type gatedRelaunchStop struct {
	*inPlaceRuntime
	entered chan struct{}
	release chan struct{}
}

func (g *gatedRelaunchStop) StopForRelaunch(ctx context.Context, loc runtime.Locator, mayReplacePod bool) error {
	g.entered <- struct{}{}
	<-g.release
	return g.inPlaceRuntime.StopForRelaunch(ctx, loc, mayReplacePod)
}

// The relaunch ends the claim's process only to start it again on the same session, in the same
// decision, so the claim keeps its role while the stop waits on the runtime, as a process the
// registration deadline retires and relaunches keeps it: the view every reader of the claim reads
// (View) does not say the process is being stopped (ClaimView.Stopping). The daemon's outbox reads
// that view to tell whether a role message or a hook prompt it holds for a claim Envoy cannot
// reach still has a session to wait for (its keepsItsSession): read as stopping, the row would be
// finished as one whose claim ended, in the very relaunch the hold waits for.
func TestARelaunchKeepsTheClaimsRoleWhileItsStopWaitsOnTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gated func(rt *fake.Runtime, entered, release chan struct{}) runtime.Runtime
	}{
		{name: "a stop the runtime makes as a suspension", gated: func(rt *fake.Runtime, entered, release chan struct{}) runtime.Runtime {
			return &gatedRuntime{Runtime: rt, entered: entered, release: release}
		}},
		{name: "a stop the runtime makes in place", gated: func(rt *fake.Runtime, entered, release chan struct{}) runtime.Runtime {
			return &gatedRelaunchStop{inPlaceRuntime: &inPlaceRuntime{Runtime: rt}, entered: entered, release: release}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			h.deps.Runtime = tc.gated(h.rt, entered, release)
			c := queuedClaim()
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				t.Fatal(err)
			}
			h.token = c.Token
			h.start(c)
			h.waitingSilently()

			relaunched := make(chan error, 1)
			go func() { relaunched <- h.m.Handle(h.ctx, h.relaunchOf(false)) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the relaunch's stop never reached the runtime")
			}
			if v := h.m.View(); v.Stopping || v.State != StateWorking || v.Session != session {
				t.Errorf("while the relaunch's stop waits on the runtime View says stopping %t, %s on session %q; want the claim keeping its role: not stopping, working on %q",
					v.Stopping, v.State, v.Session, session)
			}
			close(release)
			if err := <-relaunched; err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			h.m.Wait()
			h.wantState(StateLaunching)
			if v := h.m.View(); v.Stopping {
				t.Error("once the relaunch returned View says the process is stopping")
			}
		})
	}
}

// A claim between states has no settled agent to cut off, so the table refuses the relaunch there.
func TestARelaunchOfAClaimStillLaunchingIsRefused(t *testing.T) {
	h := newHarness(t)
	h.launch()

	err := h.handle(RequestRelaunch{Claim: h.token, Cause: relaunchCause})

	var refused *RefusedError
	if !errors.As(err, &refused) || refused.State != StateLaunching {
		t.Fatalf("relaunch of a launching claim = %v, want the table's refusal at launching", err)
	}
}

// The relaunch records itself on the claim once its stop has ended the process, so the write that
// launches the next process carries it (Claim.LastRelaunch): the 6 hours in which a second
// relaunch is refused are durable with the launch, whatever the daemon's keeper writes after it.
func TestARelaunchIsRecordedByTheWriteOfItsOwnLaunch(t *testing.T) {
	h := newHarness(t)
	h.waitingSilently()
	generation := h.generation()
	at := time.Date(2026, 11, 12, 16, 12, 0, 0, time.UTC)
	ev := h.relaunchOf(false)
	ev.At = at

	h.must(ev)

	h.store.mu.Lock()
	var launch *Claim
	for _, put := range h.store.puts {
		if put.Generation == generation+1 {
			launch = &put
			break
		}
	}
	h.store.mu.Unlock()
	if launch == nil || !launch.LastRelaunch.Equal(at) || launch.LastRelaunchGeneration != 0 {
		t.Fatalf("the first write of generation %d = %+v, want it to carry the relaunch at %v, its generation not yet recorded", generation+1, launch, at)
	}
	if stored := h.store.load(h.token); !stored.LastRelaunch.Equal(at) || stored.LastRelaunchGeneration != generation+1 {
		t.Fatalf("the store holds the relaunch at %v of generation %d, want %v of generation %d, the launch it made",
			stored.LastRelaunch, stored.LastRelaunchGeneration, at, generation+1)
	}
}

// A relaunch the guard refuses stops nothing and records nothing: a refused relaunch uses no part
// of the 6 hours.
func TestARefusedRelaunchRecordsNoRelaunch(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	h.reportActivity()
	h.advance(UnreachableAfter + time.Minute)
	h.stepped("wait", "toolu_wait_1", `{}`, false)
	h.advance(5 * time.Minute)
	ev := h.relaunchOf(false)
	ev.At = time.Date(2026, 11, 12, 16, 12, 0, 0, time.UTC)

	if err := h.handle(ev); !errors.Is(err, ErrRelaunchGuard) {
		t.Fatalf("relaunch of a claim that stepped 5 minutes ago = %v, want the guard's refusal", err)
	}
	if last := h.claim().LastRelaunch; !last.IsZero() {
		t.Fatalf("the claim records a relaunch at %v after the guard refused it, want none", last)
	}
	if last := h.store.load(h.token).LastRelaunch; !last.IsZero() {
		t.Fatalf("the store holds a relaunch at %v after the guard refused it, want none", last)
	}
}
