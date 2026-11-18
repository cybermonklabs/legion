package supervise

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
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
	h.stepped("wait", "toolu_wait_0", `{"jobs":["activity-service"]}`, false)
	h.advance(21 * time.Minute)
	h.reportActivity()
	reported := h.clock.Now()
	h.advance(4 * time.Minute)
	before := h.claim()

	err := h.handle(h.relaunchOf(false))

	if !errors.Is(err, ErrRelaunchGuard) {
		t.Fatalf("relaunch of a claim whose session reported 4 minutes ago in its turn = %v, want the guard's refusal", err)
	}
	if stamp := reported.UTC().Format(time.RFC3339); !strings.Contains(err.Error(), stamp) {
		t.Fatalf("the refusal %q does not name the report's time %s", err, stamp)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 0)
	if h.generation() != before.Generation {
		t.Fatalf("generation = %d, want it unmoved at %d", h.generation(), before.Generation)
	}
}

// A turn Oh My Pi starts under a compaction — a follow-up an Envoy delivery queued while the
// session was being compacted — is the claim's turn once the compaction ends and leaves the claim
// working for it (compactEnded), so the guard reads the claim in that turn as it reads any other:
// the turn here sits in a `wait` on a named `bash` service for 25 minutes, and its session's
// heartbeat reported in it 4 minutes ago. Read against the start of the turn before the
// compaction, which has ended, the claim would be in no turn, and the relaunch would cut the live
// one. The daemon's sweep asks the guard of the stored claim, which the compaction's end wrote.
func TestTheRelaunchGuardRefusesAClaimInATurnThatBeganUnderACompaction(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.advance(time.Minute)
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)
	h.must(StreamTurnStart{Claim: h.token})
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.wantState(StateWorking)
	h.stepped("wait", "toolu_wait_0", `{"jobs":["activity-service"]}`, false)
	h.advance(21 * time.Minute)
	h.reportActivity()
	stamp := h.clock.Now().UTC().Format(time.RFC3339)
	h.advance(4 * time.Minute)
	before := h.claim()

	err := h.handle(h.relaunchOf(false))

	if !errors.Is(err, ErrRelaunchGuard) || !strings.Contains(err.Error(), stamp) {
		t.Fatalf("relaunch of a claim whose session reported 4 minutes ago in a turn begun under a compaction = %v, want the guard's refusal naming the report at %s", err, stamp)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	h.wantCalls("Resume", 0)
	if h.generation() != before.Generation {
		t.Fatalf("generation = %d, want it unmoved at %d", h.generation(), before.Generation)
	}
	if why := RelaunchGuard(h.store.load(h.token), time.Time{}, h.clock.Now()); !strings.Contains(why, stamp) {
		t.Fatalf("RelaunchGuard of the stored claim = %q, want a refusal naming the report at %s", why, stamp)
	}
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
// launches the next process carries it (Reach.LastRelaunch): the 6 hours in which a second
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
	if launch == nil || !launch.Reach.LastRelaunch.Equal(at) || launch.Reach.LastRelaunchGeneration != 0 {
		t.Fatalf("the first write of generation %d = %+v, want it to carry the relaunch at %v, its generation not yet recorded", generation+1, launch, at)
	}
	if stored := h.store.load(h.token).Reach; !stored.LastRelaunch.Equal(at) || stored.LastRelaunchGeneration != generation+1 {
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
	if last := h.claim().Reach.LastRelaunch; !last.IsZero() {
		t.Fatalf("the claim records a relaunch at %v after the guard refused it, want none", last)
	}
	if last := h.store.load(h.token).Reach.LastRelaunch; !last.IsZero() {
		t.Fatalf("the store holds a relaunch at %v after the guard refused it, want none", last)
	}
}
