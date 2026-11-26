package supervise

// The project's pause as a machine meets it (dispatch://INFRA-3759, the plan's decisions 5 to 8,
// 14 and 22): the flag the daemon reads from its pause service, the stop transaction it writes a
// pause's stop through, and the resume's event, each as a double on the package's own harness.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// pauseFlag is the daemon's pause flag as a test drives it (Deps.Paused): the open pause's id, 0
// while the project runs. clearAfterNextRead has it read the pause once more and none after, as a
// resume that clears the flag between two reads of one decision leaves it.
type pauseFlag struct {
	mu    sync.Mutex
	id    int64
	clear bool
}

func (f *pauseFlag) Paused() (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id
	if f.clear {
		f.id, f.clear = 0, false
	}
	return id, id != 0
}

func (f *pauseFlag) set(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id = id
}

func (f *pauseFlag) clearAfterNextRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clear = true
}

// stopCall is one call of the stop transaction: the claim and the stop it was handed, and whether
// it landed.
type stopCall struct {
	claim  Claim
	stop   PauseStop
	landed bool
}

// stopTransaction is the daemon's stop transaction as a double (Deps.PauseStopped): it writes the
// claim, and the delivery the claim carries, as the plain write would, and records each call. The
// next fails calls fail with err instead, writing nothing.
type stopTransaction struct {
	store *memStore
	mu    sync.Mutex
	calls []stopCall
	fails int
	err   error
}

func (s *stopTransaction) PauseStopped(ctx context.Context, c Claim, stop PauseStop) error {
	s.mu.Lock()
	failing := s.fails > 0
	if failing {
		s.fails--
	}
	s.calls = append(s.calls, stopCall{claim: copyClaim(c), stop: stop, landed: !failing})
	err := s.err
	s.mu.Unlock()
	if failing {
		return err
	}
	if c.Pending != nil {
		return s.store.PutClaimAndDelivery(ctx, StoredClaim(c), *c.Pending)
	}
	return s.store.PutClaim(ctx, c)
}

// failNext makes the next n calls fail with err.
func (s *stopTransaction) failNext(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails, s.err = n, err
}

func (s *stopTransaction) made() []stopCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// wantStops is the stop transaction asked exactly these stops, in order, each landed as given.
func (s *stopTransaction) wantStops(t *testing.T, want ...stopCall) []stopCall {
	t.Helper()
	got := s.made()
	if len(got) != len(want) {
		t.Fatalf("the stop transaction was asked %d times (%+v), want %d", len(got), stopsOf(got), len(want))
	}
	for i := range want {
		if got[i].stop != want[i].stop || got[i].landed != want[i].landed {
			t.Fatalf("stop transaction call %d = %+v landed %v, want %+v landed %v",
				i, got[i].stop, got[i].landed, want[i].stop, want[i].landed)
		}
	}
	return got
}

func stopsOf(calls []stopCall) []PauseStop {
	stops := make([]PauseStop, 0, len(calls))
	for _, c := range calls {
		stops = append(stops, c.stop)
	}
	return stops
}

// pauseHarness is a machine over the queued claim c whose daemon has a pause service and a stop
// transaction: the project runs until the test sets the flag. tune changes the Deps before the
// machine is built.
func pauseHarness(t *testing.T, c Claim, tune ...func(*Deps)) (*harness, *pauseFlag, *stopTransaction) {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	flag := &pauseFlag{}
	stops := &stopTransaction{store: h.store}
	h.deps.Paused, h.deps.PauseStopped = flag.Paused, stops.PauseStopped
	for _, set := range tune {
		set(&h.deps)
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, flag, stops
}

// pauseStop is the pause's own stop row reaching the claim (the daemon's pause_stop executor).
func pauseStop(token claim.Token, pause int64) RequestSuspend {
	return RequestSuspend{Claim: token, By: SuspendedByReleased, Pause: pause, Reason: "the project is paused"}
}

// heldPause is the pause the claim's held suspension names, and whether one is held.
func (h *harness) heldPause() (int64, bool) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.m.held == nil {
		return 0, false
	}
	return h.m.held.Pause, true
}

// armed is whether timer kind is armed on the machine now.
func (h *harness) armed(kind TimerKind) bool {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	_, ok := h.m.timers[kind]
	return ok
}

// owedStopsNow is the pause stops the machine owes, read under its lock.
func (h *harness) owedStopsNow() []PauseStop {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return slices.Clone(h.m.owedStops())
}

// countedPacer is a pacer over limiter on clock, as the daemon builds its own (internal/daemon's
// run), and the count of the places joined in it so far. The pacer reads its clock once at each
// join and once at each turn it gives (wakepace.Pacer's Join and turn), so the joins are the reads
// less the turns. read, when not nil, is called at each read, before it. A waiting place has drawn
// no token, so a count of tokens taken, which these tests once read off a wrapped Reserve, is now
// a count of the places the machines joined: each paced launch that enters the pacing joins one.
func countedPacer(clock Clock, limiter *wakepace.Limiter, read func()) (*wakepace.Pacer, func() int) {
	var mu sync.Mutex
	reads, turns := 0, 0
	pacer := wakepace.NewPacer(limiter, func() time.Time {
		if read != nil {
			read()
		}
		mu.Lock()
		reads++
		mu.Unlock()
		return clock.Now()
	}, func(at time.Time, f func()) {
		clock.At(at, func() {
			mu.Lock()
			turns++
			mu.Unlock()
			f()
		})
	})
	return pacer, func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads - turns
	}
}

// countedJoins sets the harness's pacing to a pacer of its own over a limiter at the pace
// pacedDeps sets (countedPacer), before any machine is built over it, and answers how many places
// the machines have joined in it.
func countedJoins(h *harness) func() int {
	limiter := wakepace.New(h.deps.WakePace.Rate, 1, func(time.Duration) time.Duration { return wakePart })
	pacer, joined := countedPacer(h.deps.Clock, limiter, nil)
	h.deps.WakePace.Pacer = pacer
	return joined
}

// A pause's hold over a claim in a turn survives a start row leased before the pause (decision
// 5): the start is recorded beside the hold rather than dropping it, so the role does not run on
// past the pause. The turn's end runs the hold, through the stop transaction, with the pause's id
// and no launch owed — the turn's task is done.
func TestAPauseHoldOverAWorkingClaimSurvivesAStartAndStopsItAtTheTurnsEnd(t *testing.T) {
	h, flag, stops := pauseHarness(t, queuedClaim())
	h.reach(StateWorking)
	flag.set(7)

	if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the pause's stop of a claim in a turn = %v, want it held", err)
	}
	if err := h.m.StartedBy(h.ctx, 41, nil); err != nil {
		t.Fatalf("the start row = %v", err)
	}
	if pause, held := h.heldPause(); !held || pause != 7 {
		t.Fatalf("after the start the claim holds pause %d (held %v), want pause 7's hold kept", pause, held)
	}
	if stored := h.store.load(h.token); stored.LastStartRow != 41 {
		t.Fatalf("the stored claim records start row %d, want 41", stored.LastStartRow)
	}
	h.wantCalls("Suspend", 0)

	h.must(StreamTurnEnd{Claim: h.token})

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	if got := h.claim().SuspendedBy; got != SuspendedByReleased {
		t.Fatalf("the claim is suspended by %q, want released", got)
	}
	calls := stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7}, landed: true})
	if calls[0].claim.State != StateSuspended || calls[0].claim.LastStartRow != 41 {
		t.Fatalf("the stop transaction wrote %s at start row %d, want the suspended claim with the start recorded",
			calls[0].claim.State, calls[0].claim.LastStartRow)
	}
	if stored := h.store.load(h.token); stored.State != StateSuspended {
		t.Fatalf("the stored claim is %s, want suspended", stored.State)
	}
	if lines := h.records.of(PauseRoleStoppedMessage); len(lines) != 1 {
		t.Fatalf("the stop was logged %d times, want once", len(lines))
	} else if pause, _ := lines[0].attr("pause"); pause.Int64() != 7 {
		t.Fatalf("the stop's line names pause %v, want 7", pause)
	}
}

// A pause's hold over a claim whose turn ends before its prompt's result waits for that result
// rather than stopping the claim at the turn's end (stopWaitsForResult): a provider's refusal of
// the prompt takes the task back for another prompt (providerErrored), and the stop that follows
// keeps it and owes the role a launch for it, while the ladder that would send it again sends
// nothing. Stopped at the turn's end, the claim retired the task as a finished turn's, and the
// refusal arriving behind it was fenced out: the role lost the task the provider refused (check k1
// of dispatch://INFRA-3759, role D).
func TestAPauseHoldWaitsForTheTurnsResultAndKeepsATaskAProviderRefused(t *testing.T) {
	h, flag, stops := pauseHarness(t, queuedClaim())
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText})
	h.must(StreamTurnStart{Claim: h.token})
	task := h.pending().ID
	flag.set(7)
	if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the pause's stop of a claim in a turn = %v, want it held", err)
	}

	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	stops.wantStops(t)
	h.wantCalls("Suspend", 0)

	h.must(StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529})

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	if stored, held := h.store.delivery(h.token); !held || stored.Task != providerErrorTaskText || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("the stored task is %+v (held %v), want the refused task back unconfirmed", stored, held)
	}
	if p := h.claim().Pending; p == nil || p.Task != providerErrorTaskText {
		t.Fatalf("the stopped claim holds %+v, want the task the provider refused", p)
	}
	h.advance(promptRetryDelay(1))
	h.wantPrompts(1)
}

// A pause's hold over a claim whose turn ends before the answer to its prompt's send waits for
// that answer as it waits for the result of an acknowledged prompt (stopWaitsForResult,
// resultPending's send window). The send's outcome reaches the machine on the send's own
// goroutine and the turn's frames on the stream's queue, so the turn's end can be decided first.
// Stopped at that end, the claim retired the task as a finished turn's, and the acknowledgement
// and the result behind it were fenced out: a provider's refusal of the task was lost with them,
// as in the row above. The provider's refusal as the result then runs the stop, in either order
// with the acknowledgement. A send then lost or refused brings no result and runs no stop: the
// claim stays idle under the hold with the task kept, unconfirmed, and nothing is sent, until the
// pause's stop asked again stops it with a launch owed for the task (suspend). That keeps a task
// main retired at the turn's end.
func TestAPauseHoldWaitsForTheAnswerOfASendWhoseTurnAlreadyEnded(t *testing.T) {
	const providerError = "overloaded_error"
	// stopAskedAgain has the send's outcome, which the gate releases, bring no result, and then the
	// pause's stop asked again reach the claim the hold has kept idle with its task.
	stopAskedAgain := func(t *testing.T, h *harness, gated *gatedConn, task string) {
		t.Helper()
		gated.release <- struct{}{}
		h.m.Wait()
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		if pause, held := h.heldPause(); !held || pause != 7 {
			t.Fatalf("after the send's outcome the claim holds pause %d (held %v), want pause 7's hold kept", pause, held)
		}
		if p := h.pending(); p.ID != task || !p.ConfirmedAt.IsZero() {
			t.Fatalf("after the send's outcome the claim holds %+v, want %s kept, unconfirmed: its prompt ran no turn the daemon knows of", p, task)
		}
		select {
		case again := <-gated.entered:
			t.Fatalf("the task was sent again as %s while the pause's hold stands", again)
		default:
		}
		if err := h.m.Handle(h.ctx, pauseStop(h.token, 7)); err != nil {
			t.Fatalf("the pause's stop asked again of the idle claim: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		// answer ends the send and the prompt's work, after the turn's end and while the hold stands.
		answer func(t *testing.T, h *harness, gated *gatedConn, task string)
	}{
		{name: "acknowledged, then the provider's refusal as the result", answer: func(t *testing.T, h *harness, gated *gatedConn, task string) {
			gated.release <- struct{}{}
			h.m.Wait()
			h.wantState(StateIdle)
			h.wantCalls("Suspend", 0)
			if err := h.m.Handle(h.ctx, StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true, Error: providerError, HTTPStatus: 529}); err != nil {
				t.Fatalf("the prompt's result: %v", err)
			}
		}},
		{name: "the provider's refusal as the result, then the acknowledgement", answer: func(t *testing.T, h *harness, gated *gatedConn, task string) {
			if err := h.m.Handle(h.ctx, StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true, Error: providerError, HTTPStatus: 529}); err != nil {
				t.Fatalf("the prompt's result: %v", err)
			}
			gated.release <- struct{}{}
			h.m.Wait()
		}},
		{name: "the send lost, then the pause's stop asked again", answer: func(t *testing.T, h *harness, gated *gatedConn, task string) {
			gated.FailPrompt(errBoom)
			stopAskedAgain(t, h, gated, task)
		}},
		{name: "the send refused, then the pause's stop asked again", answer: func(t *testing.T, h *harness, gated *gatedConn, task string) {
			gated.RefusePrompt("the model provider refused the request")
			stopAskedAgain(t, h, gated, task)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim())
			h.reach(StateReady)
			gated := newGatedConn()
			h.conns.Register(h.token, gated)
			if err := h.m.Handle(h.ctx, RequestDeliver{Claim: h.token, Task: providerErrorTaskText}); err != nil {
				t.Fatal(err)
			}
			task := testwait.Receive(t, "the send", gated.entered)
			if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: h.token}); err != nil {
				t.Fatal(err)
			}
			flag.set(7)
			if err := h.m.Handle(h.ctx, pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the pause's stop of a claim in a turn = %v, want it held", err)
			}

			if err := h.m.Handle(h.ctx, StreamTurnEnd{Claim: h.token}); err != nil {
				t.Fatalf("the turn's end: %v", err)
			}
			stops.wantStops(t)
			h.wantCalls("Suspend", 0)
			if p := h.pending(); p.ID != task || p.ConfirmedAt.IsZero() {
				t.Fatalf("the claim holds %+v after the turn's end, want the task its turn confirmed, kept for the send's answer", p)
			}

			tc.answer(t, h, gated, task)

			h.wantState(StateSuspended)
			h.wantCalls("Suspend", 1)
			stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
			if stored, held := h.store.delivery(h.token); !held || stored.Task != providerErrorTaskText || !stored.ConfirmedAt.IsZero() {
				t.Fatalf("the stored task is %+v (held %v), want the task back unconfirmed", stored, held)
			}
		})
	}
}

// A pause's stop of a claim whose relaunch waits in the pacing over the process that died is
// decided where it arrives (holdSuspendUntilReady): the process the claim still records is
// stopped, the claim is suspended through the stop transaction with the parked launch owed, and
// the wake's turn, when it comes, launches nothing. Held until a ready instead, the stop landed
// only at that turn, which a wave puts tens of minutes out (decision 4 of dispatch://INFRA-3759: a
// parked wake with no process is stopped at once; check k1, role E).
func TestAPauseStopsAClaimWhoseRelaunchWaitsOverItsDeadProcessAtOnce(t *testing.T) {
	h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
	h.must(RequestSpawn{Claim: h.token})
	h.wantStarts(h.token, 1)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	h.wantStarts(h.token, 1)
	suspended := len(h.calls("Suspend"))
	flag.set(7)

	h.must(pauseStop(h.token, 7))

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", suspended+1)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the stopped claim still names the wake %+v, want it gone with the stop", held)
	}
	h.advance(waiting.At.Sub(h.clock.Now()) + time.Second)
	h.wantStarts(h.token, 1)
}

// The gate (decision 7): a launch of a claim that is not the controller's, met while the project
// is paused, starts nothing, takes no token from the pacing and arms no retry. The claim is left
// in a state its row names — launching for one with no process, its own state for one whose
// process runs — and its next decision stops it for the pause, through the stop transaction, with
// a launch owed.
func TestALaunchTheGateMeetsWhilePausedRunsNothingAndStopsTheClaimWithALaunchOwed(t *testing.T) {
	gated := func(t *testing.T, h *harness, taken func() int, starts int, held ClaimState) {
		t.Helper()
		h.wantState(held)
		h.wantStarts(h.token, starts)
		if n := taken(); n != 0 {
			t.Fatalf("the gate took %d tokens from the pacing, want none", n)
		}
		for _, kind := range []TimerKind{TimerWake, TimerRelaunch} {
			if h.armed(kind) {
				t.Fatalf("timer %s is armed, want no retry of the held launch", kind)
			}
		}
		if !h.armed(TimerPause) {
			t.Fatal("no stop of the claim is posted for its next decision")
		}
		if waiting := h.m.View().WakeWaiting; waiting != nil {
			t.Fatalf("the claim publishes wake %+v, want none: the gate's launch waits in no pacing", waiting)
		}
	}
	stopped := func(t *testing.T, h *harness, stops *stopTransaction, taken func() int, starts int) {
		t.Helper()
		h.advance(0)
		h.wantState(StateSuspended)
		h.wantStarts(h.token, starts)
		if n := taken(); n != 0 {
			t.Fatalf("the stop took %d tokens from the pacing, want none", n)
		}
		if got := h.claim().SuspendedBy; got != SuspendedByReleased {
			t.Fatalf("the claim is suspended by %q, want released", got)
		}
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	}

	t.Run("a resume of a claim that records no process", func(t *testing.T) {
		c := wakeClaim(testToken, StateSuspended)
		c.SuspendedBy = SuspendedByReleased
		h, flag, stops := pauseHarness(t, c, pacedDeps(wakeRate))
		taken := countedJoins(h)
		h.start(c)
		flag.set(7)

		h.must(RequestRuleResume{Claim: h.token, Reason: "a listener wake"})

		gated(t, h, taken, 0, StateLaunching)
		if stored := h.store.load(h.token); stored.State != StateLaunching {
			t.Fatalf("the stored claim is %s, want launching, which a restart relaunches into the gate", stored.State)
		}
		stopped(t, h, stops, taken, 0)
	})

	t.Run("a relaunch over the process of an idle claim that died", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
		taken := countedJoins(h)
		h.start(queuedClaim())
		h.reach(StateIdle)
		before := taken()
		flag.set(7)

		h.observe(runtime.Gone)

		gated(t, h, func() int { return taken() - before }, 1, StateIdle)
		stopped(t, h, stops, func() int { return taken() - before }, 1)
	})

	t.Run("a stalled turn's replacement", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
		taken := countedJoins(h)
		h.start(queuedClaim())
		last, swept := stalledSweep(h)
		before := taken()
		flag.set(7)

		h.must(h.backstopOf(last, swept))

		gated(t, h, func() int { return taken() - before }, 1, StateWorking)
		// The turn is held to a deadline of now and cut off there, its task taken back.
		stopped(t, h, stops, func() int { return taken() - before }, 1)
		if p := h.pending(); !p.ConfirmedAt.IsZero() {
			t.Fatalf("the stopped claim holds %+v, want its task taken back for the launch it is owed", p)
		}
	})
}

// A pause's stop of a restored claim whose record says its process died owes the role the
// relaunch that death raised. The boot's stop can run before the boot replays the death, so the
// stop reads the durable charge with the locator rather than only the wake the restart forgot.
// No RecordedDeath is posted here: the stop must preserve that launch on its own. A pause's stop
// carries no deadline (internal/daemon's pause stop rows), so over a working claim it is held for
// the turn, and the death's own report ends that hold (died, endHeld, letProcessGoFor). The other
// rows reach the stop at once: the ready row is never held; the working row's deadline of now ends
// the hold (suspendAtTimeout); and the row whose recovery read found the process gone is decided
// where it arrives (holdSuspension, suspendOverADeadProcess, letProcessGoFor).
func TestAPausesStopOfAClaimWhoseRecordSaysItsProcessDiedOwesItsLaunch(t *testing.T) {
	for _, row := range []struct {
		name     string
		working  bool
		read     bool
		byReport bool
	}{
		{name: "ready"},
		{name: "working", working: true},
		{name: "working whose process a recovery read found gone", working: true, read: true},
		{name: "working, held until the death's report ends the hold", working: true, byReport: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
			h.reach(StateReady)
			if row.working {
				h.must(StreamTurnStart{Claim: h.token})
			}
			queueAhead(h, 1)
			h.observe(runtime.Gone)
			h.wantWaitingWake(WakeLaunch)
			flag.set(7)
			pacedDeps(wakeRate)(&h.deps)
			taken := countedJoins(h)
			h.restart()
			if c := h.claim(); c.Locator == nil || !c.DeathCharged || c.Pending != nil {
				t.Fatalf("the restored claim records %+v, want a charged death with a locator and no task that could owe its own launch", c)
			}
			if row.read {
				h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
				if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
					t.Fatalf("the recovery read found %s, want gone", got)
				}
			}
			request := pauseStop(h.token, 7)
			if !row.byReport {
				request.Deadline = h.clock.Now()
			}
			if err := h.handle(request); err != nil && !errors.Is(err, ErrSuspendHeld) {
				t.Fatal(err)
			}
			if row.byReport {
				if !h.claim().SuspensionHeld {
					t.Fatalf("the pause's stop of the restored working claim is not held, state %s", h.state())
				}
				h.observe(runtime.Gone)
			}
			h.advance(0)

			h.wantState(StateSuspended)
			h.wantStarts(h.token, 1)
			if n := taken(); n != 0 {
				t.Fatalf("the pause's stop joined %d places in the pacing, want none", n)
			}
			stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
		})
	}
}

// A launch that passed the wake gate's pause check before the pause began meets the pause after
// its first write, before it asks the runtime for a process (launchTries): it starts nothing, the
// pause's gate holds it and stops the claim for the pause, a launch owed. The pause's catch-up
// reads the claim's published view (internal/daemon's Paused), which shows the claim as it stood
// before the launch until that first write, so without this check the launch started its process
// during the pause. A relaunch held so stops the process it let go first, so that process does not
// run unrecorded through the pause, and a stop the runtime refuses leaves the launch held all the
// same, the refusal recorded (pauseHeldTry, stopLetGo). A pause that ends before the gate's stop
// lets the launch out without the pacing: it took its turn and wrote its wake line at the wake
// gate already (wake.paid, wake.ran).
func TestALaunchPastThePauseCheckWhenThePauseBeginsStartsNothing(t *testing.T) {
	// passed is a harness whose pacing lets every launch out at once — a pacer over a limiter that
	// paces nothing, which lets every place out at its join — and begins the pause as it lets out
	// the first launch after arm, past that launch's pause check (reserveWake); taken is the count
	// of the places joined in the pacing (countedPacer).
	passed := func(t *testing.T) (h *harness, flag *pauseFlag, stops *stopTransaction, taken func() int, arm func()) {
		t.Helper()
		var armed atomic.Bool
		h, flag, stops = pauseHarness(t, queuedClaim())
		var pacer *wakepace.Pacer
		pacer, taken = countedPacer(h.deps.Clock, wakepace.New(0, 1, nil), func() {
			if armed.CompareAndSwap(true, false) {
				flag.set(7)
			}
		})
		h.deps.WakePace = WakePace{Pacer: pacer, Rate: wakeRate, Burst: 1, Interval: time.Minute / wakeRate}
		h.start(queuedClaim())
		return h, flag, stops, taken, func() { armed.Store(true) }
	}
	const refusedStopLine = "supervise: the runtime did not confirm the stop of the process this launch let go; the launch waits for the project's pause to end all the same"
	// staleRelaunch is the runtime finding the idle claim's live process at an address a process
	// launched now is not handed, past the pause's start: the relaunch lets that process go, and
	// the pause meets its try after its write. It answers the process let go.
	staleRelaunch := func(t *testing.T, h *harness, arm func()) runtime.Locator {
		t.Helper()
		h.reach(StateIdle)
		replaced := h.locator()
		arm()
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: replaced, Kind: runtime.StaleAddress, At: h.clock.Now()}})
		h.wantStarts(h.token, 1)
		if !h.armed(TimerPause) {
			t.Fatal("the relaunch the pause met posted no stop of the claim")
		}
		return replaced
	}

	t.Run("the pause's gate holds the launch and stops the claim", func(t *testing.T) {
		h, _, stops, taken, arm := passed(t)
		arm()

		h.must(RequestSpawn{Claim: h.token})

		h.wantStarts(h.token, 0)
		h.wantState(StateLaunching)
		if !h.armed(TimerPause) {
			t.Fatal("no stop of the claim is posted for its next decision")
		}
		h.advance(0)
		h.wantState(StateSuspended)
		h.wantStarts(h.token, 0)
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
		if n := taken(); n != 1 {
			t.Fatalf("the pacing joined %d places, want the one the launch took at the wake gate", n)
		}
	})

	t.Run("a pause that ends before the gate's stop lets the launch out once, unpaced", func(t *testing.T) {
		h, flag, stops, taken, arm := passed(t)
		arm()

		h.must(RequestSpawn{Claim: h.token})
		h.wantStarts(h.token, 0)
		flag.set(0)
		h.advance(0)

		h.wantStarts(h.token, 1)
		h.wantState(StateLaunching)
		if n := taken(); n != 1 {
			t.Fatalf("the pacing joined %d places, want the one the launch took at the wake gate", n)
		}
		if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token)); len(lines) != 1 {
			t.Fatalf("wake lines = %v, want the one the launch wrote at the wake gate", lines)
		}
		stops.wantStops(t)
	})

	t.Run("a relaunch the pause meets stops the process it let go", func(t *testing.T) {
		h, _, stops, _, arm := passed(t)

		replaced := staleRelaunch(t, h, arm)

		if calls := h.calls("Suspend"); len(calls) != 1 || calls[0].Locator != replaced {
			t.Fatalf("stops = %+v, want the one stop of the process the relaunch let go, %+v", calls, replaced)
		}
		if lines := h.logs.lines(refusedStopLine, "claim="+string(h.token)); len(lines) != 0 {
			t.Fatalf("refused-stop lines = %v, want none", lines)
		}
		h.advance(0)
		h.wantState(StateSuspended)
		h.wantStarts(h.token, 1)
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	})

	t.Run("a relaunch the pause meets over a stop the runtime refuses is held all the same", func(t *testing.T) {
		h, _, _, _, arm := passed(t)
		h.rt.FailSuspend(errors.New("the runtime refused the stop"))

		replaced := staleRelaunch(t, h, arm)

		if calls := h.calls("Suspend"); len(calls) != 1 || calls[0].Locator != replaced {
			t.Fatalf("stops = %+v, want the one refused stop of the process the relaunch let go, %+v", calls, replaced)
		}
		h.m.mu.Lock()
		unconfirmed := h.m.stopUnconfirmed
		h.m.mu.Unlock()
		if !unconfirmed {
			t.Fatal("the stop the runtime refused is not recorded as unconfirmed")
		}
		if lines := h.logs.lines(refusedStopLine, "claim="+string(h.token)); len(lines) != 1 {
			t.Fatalf("refused-stop lines = %v, want one", lines)
		}
		h.wantState(StateLaunching)
	})
}

// The controller is the one role the pause never holds: its launch runs while the project is
// paused, and the gate posts no stop of it.
func TestTheControllersLaunchRunsWhileTheProjectIsPaused(t *testing.T) {
	controller := Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: StateQueued}
	h, flag, stops := pauseHarness(t, controller, pacedDeps(wakeRate))
	flag.set(7)

	h.must(RequestSpawn{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 1)
	if h.armed(TimerPause) {
		t.Fatal("the controller's launch posted a stop for the pause")
	}
	stops.wantStops(t)
}

// No task leaves for a role but the controller while the project is paused (decision 8): the
// task waits pending and unsent. The resume's event sends it (decision 22), once the flag is
// cleared.
func TestNoTaskIsSentWhilePausedButTheControllersAndTheResumeSendsIt(t *testing.T) {
	t.Run("a phase worker's task waits for the resume", func(t *testing.T) {
		h, flag, _ := pauseHarness(t, queuedClaim())
		h.reach(StateReady)
		flag.set(7)

		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})

		h.wantPrompts(0)
		if p := h.pending(); !p.ConfirmedAt.IsZero() || p.Task != "implement the plan" {
			t.Fatalf("the claim holds %+v, want the task pending and unsent", p)
		}
		h.must(IssueTurnEnded{Claim: h.token})
		h.wantPrompts(0)

		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})

		h.wantPrompts(1)
	})

	t.Run("the controller's task goes", func(t *testing.T) {
		controller := Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: StateQueued}
		h, flag, _ := pauseHarness(t, controller)
		h.reach(StateReady)
		flag.set(7)

		h.must(RequestDeliver{Claim: h.token, Task: "look at the project"})

		h.wantPrompts(1)
	})
}

// A claim of another project's issue — one the operator's spawn makes under this project's token —
// is a session this daemon runs, so the pause holds it as it holds every claim but the
// controller's: while the project is paused the gate starts none of its launches and stops it in
// its next decision through the stop transaction, with a launch owed, and its task waits for the
// resume.
func TestThePauseHoldsAClaimOfAnotherProjectsIssue(t *testing.T) {
	other := func() Claim {
		c := queuedClaim()
		c.Token, c.Tree, c.Issue = "legion-legion-other-5-implementer", "OTHER-5", "OTHER-5"
		return c
	}

	t.Run("its launch is held and the gate stops it", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, other())
		flag.set(7)

		h.must(RequestSpawn{Claim: h.token})

		h.wantState(StateLaunching)
		h.wantStarts(h.token, 0)
		if !h.armed(TimerPause) {
			t.Fatal("the gate posted no stop of the claim of another project's issue")
		}
		h.advance(0)
		h.wantState(StateSuspended)
		h.wantStarts(h.token, 0)
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	})

	t.Run("its task waits for the resume", func(t *testing.T) {
		h, flag, _ := pauseHarness(t, other())
		h.reach(StateReady)
		flag.set(7)

		h.must(RequestDeliver{Claim: h.token, Task: "summarize the incident"})

		h.wantPrompts(0)
		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})
		h.wantPrompts(1)
	})
}

// A pause's hold whose pause has ended is dropped at the start of the decision that would fire it
// (decision 5, dropEndedPauseHold): the turn's end, a start, and the agent's ready each leave the
// claim running, and nothing is stopped or written through the stop transaction. The flag is
// cleared and the resume's event has not arrived.
func TestAHoldWhosePauseEndedDoesNotStopItsClaim(t *testing.T) {
	t.Run("at the turn's end", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateWorking)
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop = %v, want it held", err)
		}
		flag.set(0)

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		if _, held := h.heldPause(); held {
			t.Fatal("the hold of the ended pause is still held")
		}
		stops.wantStops(t)
	})

	t.Run("at a start", func(t *testing.T) {
		h, flag, _ := pauseHarness(t, queuedClaim())
		h.reach(StateWorking)
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop = %v, want it held", err)
		}
		flag.set(0)

		if err := h.m.StartedBy(h.ctx, 9, nil); err != nil {
			t.Fatalf("the start row = %v", err)
		}

		if _, held := h.heldPause(); held {
			t.Fatal("the start kept the hold of a pause that has ended")
		}
	})

	t.Run("at the agent's ready", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.launch()
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop of a launch = %v, want it held until ready", err)
		}
		flag.set(0)

		h.connect()
		h.register()
		h.ready()

		h.wantState(StateReady)
		h.wantCalls("Suspend", 0)
		stops.wantStops(t)
	})
}

// A pause's hold whose pause has ended holds nothing back where it does not fire either: the
// next decision drops it whatever that decision is (dropEndedPauseHold). The claim here is ready
// with the hold standing — the runtime refused its stop at the agent's ready, which the hold
// retries at the probe interval — when the pause ends and no resume's event comes. A compaction
// asked for then is taken rather than refused for the hold, and a task delivered then is sent
// rather than held back behind it. Dropped only where a hold fires, the hold refused the one and
// held back the other.
func TestAHoldWhosePauseEndedHoldsNothingBack(t *testing.T) {
	heldAtReady := func(t *testing.T) (*harness, *stopTransaction) {
		t.Helper()
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.launch()
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop of a launch = %v, want it held until ready", err)
		}
		h.connect()
		h.register()
		h.rt.FailSuspend(errors.New("the runtime refused the stop"))
		if err := h.handle(RequestReady{Claim: h.token, Generation: h.generation(), Session: session}); err == nil {
			t.Fatal("the ready whose held stop the runtime refused succeeded, want the runtime's refusal")
		}
		if pause, held := h.heldPause(); h.state() != StateReady || !held || pause != 7 {
			t.Fatalf("after the refused stop the claim is %s holding pause %d (held %v), want it ready with pause 7's hold",
				h.state(), pause, held)
		}
		h.rt.FailSuspend(nil)
		flag.set(0)
		return h, stops
	}

	t.Run("a compaction is taken", func(t *testing.T) {
		h, stops := heldAtReady(t)

		if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
			t.Fatalf("a compaction of a claim whose hold names an ended pause = %v, want it taken", err)
		}
		h.m.Wait()

		if asked := h.conn.Compactions(); len(asked) != 1 {
			t.Fatalf("Oh My Pi was asked %d compactions, want the one", len(asked))
		}
		h.wantState(StateReady)
		if _, held := h.heldPause(); held {
			t.Fatal("the hold of the ended pause is still held")
		}
		h.advance(testProbe)
		h.wantState(StateReady)
		h.wantCalls("Suspend", 1)
		stops.wantStops(t)
	})

	t.Run("a task is sent", func(t *testing.T) {
		h, stops := heldAtReady(t)

		h.must(RequestDeliver{Claim: h.token, Task: "address the review"})

		if prompts := h.wantPrompts(1); prompts[0].Message != "address the review" {
			t.Fatalf("the claim was sent %q, want the task delivered", prompts[0].Message)
		}
		if _, held := h.heldPause(); held {
			t.Fatal("the hold of the ended pause is still held")
		}
		h.wantCalls("Suspend", 1)
		stops.wantStops(t)
	})
}

// A second pause's request over the hold of the first takes the hold over (decision 5), so the
// first resume's event, arriving after the second pause began, drops nothing of it (decision 22):
// the turn's end stops the claim for the second pause.
func TestASecondPauseTakesTheHoldOverAndTheFirstPausesResumeDropsNothing(t *testing.T) {
	h, flag, stops := pauseHarness(t, queuedClaim())
	h.reach(StateWorking)
	flag.set(1)
	if err := h.handle(pauseStop(h.token, 1)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the first pause's stop = %v, want it held", err)
	}
	flag.set(2)
	if err := h.handle(pauseStop(h.token, 2)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the second pause's stop = %v, want it held", err)
	}
	if pause, _ := h.heldPause(); pause != 2 {
		t.Fatalf("the hold names pause %d, want the second pause's", pause)
	}

	h.must(ResumePause{Claim: h.token, Pause: 1})

	if pause, held := h.heldPause(); !held || pause != 2 {
		t.Fatalf("after the first pause's resume the claim holds pause %d (held %v), want the second pause's hold kept", pause, held)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateSuspended)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 2}, landed: true})
}

// A pause's request that reaches a claim whose daemon suspension is already held leaves it the
// daemon's, in a turn and in a launch alike (decision 5): the claim is stopped for the daemon's
// own reason, with no pause's records.
func TestAPausesRequestKeepsAHeldDaemonSuspensionsSource(t *testing.T) {
	linger := RequestSuspend{By: SuspendedByDaemon, Reason: "the tree lingers"}

	t.Run("in a turn", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateWorking)
		linger.Claim = h.token
		if err := h.handle(linger); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's stop = %v, want it held", err)
		}
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop = %v, want it held", err)
		}
		if got := h.claim(); got.SuspensionHeldBy != SuspendedByDaemon {
			t.Fatalf("the hold is %q's, want the daemon's kept", got.SuspensionHeldBy)
		}

		h.must(StreamTurnEnd{Claim: h.token})

		if got := h.claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon {
			t.Fatalf("the claim is %s by %q, want suspended by the daemon", got.State, got.SuspendedBy)
		}
		stops.wantStops(t)
	})

	t.Run("in a launch", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.launch()
		linger.Claim = h.token
		if err := h.handle(linger); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's stop of a launch = %v, want it held until ready", err)
		}
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop of a launch = %v, want it held until ready", err)
		}
		if got := h.claim(); got.SuspensionHeldBy != SuspendedByDaemon {
			t.Fatalf("the hold is %q's, want the daemon's kept", got.SuspensionHeldBy)
		}

		h.connect()
		h.register()
		h.ready()

		if got := h.claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon {
			t.Fatalf("the claim is %s by %q, want suspended by the daemon", got.State, got.SuspendedBy)
		}
		stops.wantStops(t)
	})
}

// A pause's id is the release's to carry, and a pause's: any other source with one, or an id that
// names no pause, is refused before anything is stopped.
func TestAPauseStopNamesTheReleasesSourceAndAPausesID(t *testing.T) {
	for _, request := range []RequestSuspend{
		{By: SuspendedByDaemon, Pause: 7},
		{By: SuspendedByOperator, Pause: 7},
		{By: SuspendedByReleased, Pause: -1},
	} {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateIdle)
		flag.set(7)
		request.Claim = h.token
		if err := h.handle(request); err == nil || errors.Is(err, ErrPauseEnded) {
			t.Fatalf("the suspension %+v answered %v, want it refused for its source or its id", request, err)
		}
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		stops.wantStops(t)
	}
}

// A pause's stop that reaches its claim once its pause is no longer the open one stops nothing,
// whatever row it meets: the resume ended the pause between the `pause_stop` executor's read of
// it and the machine's decision, or a later pause followed. An idle claim (suspend), a claim in a
// turn (holdSuspension), a launch waiting for its agent (holdSuspendUntilReady), a claim already
// suspended (suspendedAgain) and a claim the operator's held suspension holds each answer
// ErrPauseEnded and are left as they stood: no process is stopped, no stop transaction is made,
// and no hold of the ended pause is taken. Stopped instead, a role that holds nothing a wake would
// bring back stays stopped after the resume until its next wake (R4 of dispatch://INFRA-3759).
func TestAPauseStopWhosePauseHasEndedStopsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reach func(*harness)
		state ClaimState
		// open is the pause the project stands under when the stop arrives, 0 for none.
		open int64
		// heldBy is the source of the suspension the claim holds before the stop and after it.
		heldBy SuspendSource
	}{
		{name: "an idle claim", reach: func(h *harness) { h.reach(StateIdle) }, state: StateIdle},
		{name: "an idle claim under a later pause", reach: func(h *harness) { h.reach(StateIdle) }, state: StateIdle, open: 8},
		{name: "a claim in a turn", reach: func(h *harness) { h.reach(StateWorking) }, state: StateWorking},
		{name: "a launch waiting for its agent", reach: func(h *harness) { h.launch() }, state: StateLaunching},
		{name: "a claim already suspended", reach: func(h *harness) { suspendedBy(h, SuspendedByReleased) }, state: StateSuspended},
		{name: "a claim the operator's held suspension holds", reach: func(h *harness) {
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator drains the daemon"}); !errors.Is(err, ErrSuspendHeld) {
				h.t.Fatalf("the operator's suspension of a turn = %v, want it held", err)
			}
		}, state: StateWorking, heldBy: SuspendedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim())
			tc.reach(h)
			suspended := len(h.calls("Suspend"))
			flag.set(tc.open)

			if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrPauseEnded) {
				t.Fatalf("the stop of pause 7, which has ended, = %v, want ErrPauseEnded", err)
			}

			h.wantState(tc.state)
			h.wantCalls("Suspend", suspended)
			stops.wantStops(t)
			if got := h.claim().SuspensionHeldBy; got != tc.heldBy {
				t.Fatalf("the claim's held suspension is %q's after the stop, want %q's", got, tc.heldBy)
			}
			if pause, held := h.heldPause(); held && pause != 0 {
				t.Fatalf("the claim holds a suspension of pause %d, want none of a pause", pause)
			}
		})
	}
}

// cutShortSpawn holds Spawn until its context ends, and calls ended then, before the launch
// returns: a moment between a suspension's cut of the start (Handle, stopStart) and its decision,
// which runs once the cut launch lets the machine go.
type cutShortSpawn struct {
	*fake.Runtime
	entered chan struct{}
	ended   func()
}

func (g *cutShortSpawn) Spawn(ctx context.Context, _ runtime.SpawnSpec) (runtime.Locator, error) {
	g.entered <- struct{}{}
	<-ctx.Done()
	g.ended()
	return runtime.Locator{}, ctx.Err()
}

// A pause's stop that cut its claim's start short while its pause stood is decided as asked though
// the resume ends the pause before the decision: the cut start left the claim launching with
// nothing running, which only this stop's own row ends (holdSuspendUntilReady). Answered
// ErrPauseEnded instead, the claim would stand launching over no process until the next boot. It
// is suspended through the stop transaction owing the launch the stop cut, which the stop
// transaction launches, since its pause has ended (projectPause.stopTx in internal/daemon).
func TestAPauseStopThatCutAStartShortIsDecidedThoughItsPauseEnded(t *testing.T) {
	spawn := &cutShortSpawn{entered: make(chan struct{}, 1)}
	h, flag, stops := pauseHarness(t, queuedClaim(), func(d *Deps) {
		spawn.Runtime = d.Runtime.(*fake.Runtime)
		d.Runtime = spawn
	})
	spawn.ended = func() { flag.set(0) }
	launched := make(chan error, 1)
	go func() { launched <- h.m.Handle(h.ctx, RequestSpawn{Claim: h.token}) }()
	select {
	case <-spawn.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the launch never reached the runtime")
	}
	flag.set(7)

	if err := h.handle(pauseStop(h.token, 7)); err != nil {
		t.Fatalf("the stop that cut the start short, decided once its pause had ended, = %v; want the claim suspended", err)
	}
	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut launch returned %v, want its context's cancellation", err)
	}
	h.wantState(StateSuspended)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
}

// A stop transaction the store did not take is owed (decision 6): every claim write that lands
// after it goes through the stop transaction with the pause's records — the next decision's
// repair, a start row's write, and a retirement — and nothing writes the stopped claim without
// them. The write that lands is the repair, and closes the story as a repair does.
func TestAFailedPauseStopIsWrittenAgainWithThePausesRecords(t *testing.T) {
	failedStop := func(t *testing.T) (*harness, *stopTransaction) {
		t.Helper()
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateIdle)
		flag.set(7)
		stops.failNext(1, brokenConnection())
		if err := h.handle(pauseStop(h.token, 7)); err == nil {
			t.Fatal("the stop whose transaction failed returned no error")
		}
		h.wantState(StateSuspended)
		if owed := h.owedStopsNow(); !slices.Equal(owed, []PauseStop{{Pause: 7}}) {
			t.Fatalf("the machine owes stops %+v, want pause 7's", owed)
		}
		if stored := h.store.load(h.token); stored.State != StateIdle {
			t.Fatalf("the stored claim is %s, want it as it was: the failed transaction wrote nothing", stored.State)
		}
		return h, stops
	}

	t.Run("by the next decision's repair", func(t *testing.T) {
		h, stops := failedStop(t)

		h.must(pauseStop(h.token, 7)) // the row's retry, which finds the claim suspended already

		calls := stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7}}, stopCall{stop: PauseStop{Pause: 7}, landed: true})
		if calls[1].claim.State != StateSuspended {
			t.Fatalf("the repair wrote %s, want the suspended claim", calls[1].claim.State)
		}
		if stored := h.store.load(h.token); stored.State != StateSuspended || stored.SuspendedBy != SuspendedByReleased {
			t.Fatalf("the stored claim is %s by %q, want suspended by the release", stored.State, stored.SuspendedBy)
		}
		if owed := h.owedStopsNow(); len(owed) != 0 {
			t.Fatalf("the machine still owes %+v after the repair landed", owed)
		}
		if repaired := h.records.of(repairedWriteLine); len(repaired) != 1 {
			t.Fatalf("the repaired line was written %d times, want once", len(repaired))
		} else if store, _ := repaired[0].attr("store"); store.String() != "PauseStopped" {
			t.Fatalf("the repaired line names %v, want the stop transaction", store)
		}
		if lines := h.records.of(PauseRoleStoppedMessage); len(lines) != 1 {
			t.Fatalf("the stop was logged %d times, want once, where it landed", len(lines))
		}
	})

	t.Run("by a start row's write", func(t *testing.T) {
		h, stops := failedStop(t)

		if err := h.m.StartedBy(h.ctx, 12, nil); err != nil {
			t.Fatalf("the start row = %v", err)
		}

		calls := stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7}}, stopCall{stop: PauseStop{Pause: 7}, landed: true})
		if calls[1].claim.LastStartRow != 12 || calls[1].claim.State != StateSuspended {
			t.Fatalf("the start's write went through the stop transaction as %s at start row %d, want the suspended claim at row 12",
				calls[1].claim.State, calls[1].claim.LastStartRow)
		}
		if owed := h.owedStopsNow(); len(owed) != 0 {
			t.Fatalf("the machine still owes %+v after the start's write landed", owed)
		}
	})

	t.Run("by a retirement", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateWorking)
		task := h.pending().ID
		flag.set(7)
		if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the pause's stop = %v, want it held", err)
		}
		stops.failNext(1, brokenConnection())

		// The turn ends with its prompt's result still to come, so the pause's stop waits for that
		// result (stopWaitsForResult), which says the turn finished its task: the stop runs there,
		// and the retirement of the task after it, in the same decision (settle). The stop's
		// transaction fails, and the retirement writes it first, with the task, before it retires
		// the task.
		if err := h.handleQuiet(StreamTurnEnd{Claim: h.token}); err != nil {
			t.Fatalf("the turn's end, whose stop waits for its prompt's result: %v", err)
		}
		stops.wantStops(t)
		if err := h.handle(StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true}); err == nil {
			t.Fatal("the prompt's result whose stop failed returned no error")
		}

		calls := stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7}}, stopCall{stop: PauseStop{Pause: 7}, landed: true})
		if p := calls[1].claim.Pending; p == nil || p.ID != task {
			t.Fatalf("the retirement's stop carried delivery %+v, want the task %s it then retires", p, task)
		}
		if _, held := h.store.delivery(h.token); held {
			t.Fatal("the task is still stored, want it retired after the stop")
		}
		if log := h.store.deliveryLog(); log[len(log)-1] != "retire "+task {
			t.Fatalf("the delivery log ends %v, want the task's retirement last", log)
		}
		if owed := h.owedStopsNow(); len(owed) != 0 {
			t.Fatalf("the machine still owes %+v after the retirement landed", owed)
		}
	})
}

// The writes that settle no owed write when they land — the queueing of a task, a provider
// error's take-back and the ladder's write of it — leave a pause's stop owed: the claim row they
// write carries nothing of the pause, so the next repair writes the stop transaction, with the
// delivery they wrote and a launch owed for it. The stop is owed here as a daemon whose stop
// failed would hold it, on a claim that runs: every write of it while it is owed goes through
// the stop transaction, which keeps failing until the test lets it land.
func TestAWriteThatSettlesNoMarkLeavesAPauseStopOwed(t *testing.T) {
	owe := func(h *harness) {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		h.m.owedWrite = &owedClaimWrite{stops: []PauseStop{{Pause: 7}}}
	}

	t.Run("a task queued", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateIdle)
		flag.set(7)
		owe(h)
		stops.failNext(100, brokenConnection())

		h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "row-9", Row: 9})

		if stored := h.store.load(h.token); !slices.Contains(stored.DeliveredRows, 9) || stored.Pending == nil {
			t.Fatalf("the stored claim is %+v, want the task and its row written", stored)
		}
		if owed := h.owedStopsNow(); !slices.Equal(owed, []PauseStop{{Pause: 7}}) {
			t.Fatalf("the machine owes %+v after the queue's write, want pause 7's stop still owed", owed)
		}

		stops.failNext(0, nil)
		h.must(IssueTurnEnded{Claim: h.token})

		calls := stops.made()
		last := calls[len(calls)-1]
		if !last.landed || last.stop != (PauseStop{Pause: 7, LaunchOwed: true}) || last.claim.Pending == nil || last.claim.Pending.ID != "row-9" {
			t.Fatalf("the repair's stop was %+v landed %v carrying %+v, want pause 7's with a launch owed for the task row-9",
				last.stop, last.landed, last.claim.Pending)
		}
		if owed := h.owedStopsNow(); len(owed) != 0 {
			t.Fatalf("the machine still owes %+v after the repair landed", owed)
		}
	})

	t.Run("a provider error's take-back and its ladder", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText})
		h.must(StreamTurnStart{Claim: h.token})
		flag.set(7)
		owe(h)
		stops.failNext(100, brokenConnection())
		task := h.pending().ID

		_ = h.handleQuiet(StreamTurnEnd{Claim: h.token}) // its own claim write goes through the failing stop
		if err := h.handle(StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true,
			Error: "overloaded_error", HTTPStatus: 529}); err != nil {
			t.Fatalf("the provider error = %v", err)
		}

		stored, held := h.store.delivery(h.token)
		if !held || stored.ProviderErrorRetries != 1 || !stored.ConfirmedAt.IsZero() {
			t.Fatalf("the stored task is %+v (held %v), want it taken back and counted one send", stored, held)
		}
		if owed := h.owedStopsNow(); !slices.Equal(owed, []PauseStop{{Pause: 7}}) {
			t.Fatalf("the machine owes %+v after the provider error's writes, want pause 7's stop still owed", owed)
		}

		stops.failNext(0, nil)
		h.must(IssueTurnEnded{Claim: h.token})

		calls := stops.made()
		last := calls[len(calls)-1]
		if !last.landed || last.stop != (PauseStop{Pause: 7, LaunchOwed: true}) || last.claim.Pending == nil || last.claim.Pending.ID != stored.ID {
			t.Fatalf("the repair's stop was %+v landed %v carrying %+v, want pause 7's with a launch owed for the task taken back",
				last.stop, last.landed, last.claim.Pending)
		}
	})
}

// A resume that happens while a stop's runtime call runs — the flag cleared and the resume's
// event posted — still leaves the stopped role its stop transaction (decision 6): with a launch
// owed where the role holds a task, and with none where it holds none, which is where the daemon
// arms the role's backstop instead. The resume's event finds nothing of the pause left on the
// stopped claim.
func TestAResumeWhileAPauseStopsRuntimeCallRunsStillWritesTheStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		task bool
		owed bool
	}{
		{name: "a role that holds a task is owed a launch", task: true, owed: true},
		{name: "a role that holds none is owed no launch", owed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gated := &gatedRuntime{entered: make(chan struct{}, 1), release: make(chan struct{})}
			h, flag, stops := pauseHarness(t, queuedClaim(), func(d *Deps) {
				gated.Runtime = d.Runtime.(*fake.Runtime)
				d.Runtime = gated
			})
			h.reach(StateIdle)
			flag.set(7)
			if tc.task {
				h.must(RequestDeliver{Claim: h.token, Task: "address the review"})
				h.wantPrompts(1) // the reach's own task alone; this one waits for the resume
			}

			stopped := make(chan error, 1)
			go func() { stopped <- h.m.Handle(h.ctx, pauseStop(h.token, 7)) }()
			select {
			case <-gated.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the stop never reached the runtime")
			}
			flag.set(0)
			resumed := make(chan error, 1)
			go func() { resumed <- h.m.Handle(h.ctx, ResumePause{Claim: h.token, Pause: 7}) }()
			select {
			case err := <-resumed:
				t.Fatalf("the resume's event was decided (%v) while the stop's runtime call still ran", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(gated.release)
			if err := <-stopped; err != nil {
				t.Fatalf("the stop = %v", err)
			}
			if err := <-resumed; err != nil {
				t.Fatalf("the resume's event = %v", err)
			}
			h.m.Wait()

			h.wantState(StateSuspended)
			stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: tc.owed}, landed: true})
			if tc.task {
				h.wantPrompts(1)
			}
		})
	}
}

// A launch the gate held is let out into the pacing once its pause is over (decision 7): by the
// resume's event, or by the gate's own stop finding no pause open, it joins a place in the pacing,
// tells the daemon the moment the pacer lets that place out, and launches then; a launch that paid
// before the gate held it (wake.paid) launches at once, with no place of its own and no moment
// told. One held for a pause that a later pause has followed is stopped for the later one, and the
// earlier pause's resume touches nothing of it.
func TestALaunchThePauseGateHeldIsLetOutIntoThePacingOnceItsPauseEnds(t *testing.T) {
	gate := func(t *testing.T) (*harness, *pauseFlag, *stopTransaction, func() int, *[]time.Time) {
		t.Helper()
		var stamps []time.Time
		h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), func(d *Deps) {
			d.WakeLetOut = func(_ claim.Token, _ string, _ claim.Role, at time.Time) { stamps = append(stamps, at) }
		})
		taken := countedJoins(h)
		h.start(queuedClaim())
		flag.set(7)
		h.must(RequestSpawn{Claim: h.token})
		h.wantState(StateLaunching)
		h.wantStarts(h.token, 0)
		return h, flag, stops, taken, &stamps
	}

	t.Run("by the resume's event", func(t *testing.T) {
		h, flag, stops, taken, stamps := gate(t)
		flag.set(0)

		h.must(ResumePause{Claim: h.token, Pause: 7})

		h.wantStarts(h.token, 1)
		if n := taken(); n != 1 {
			t.Fatalf("the let-out joined %d places, want one", n)
		}
		if len(*stamps) != 1 || !(*stamps)[0].Equal(h.clock.Now()) {
			t.Fatalf("the let-out stamped %v, want the moment the pacer let its place out, now", *stamps)
		}
		if h.armed(TimerPause) {
			t.Fatal("the gate's stop is still posted after the launch was let out")
		}
		h.advance(0)
		h.wantState(StateLaunching)
		stops.wantStops(t)
	})

	t.Run("by the gate's own stop finding the pause over", func(t *testing.T) {
		h, flag, stops, taken, _ := gate(t)
		flag.set(0)

		h.advance(0)

		h.wantState(StateLaunching)
		h.wantStarts(h.token, 1)
		if n := taken(); n != 1 {
			t.Fatalf("the let-out joined %d places, want one", n)
		}
		stops.wantStops(t)
	})

	t.Run("by the gate's own stop the resume overtakes between its reads", func(t *testing.T) {
		h, flag, stops, taken, _ := gate(t)
		flag.clearAfterNextRead()

		h.advance(0)
		h.advance(0)

		h.wantState(StateLaunching)
		h.wantStarts(h.token, 1)
		if n := taken(); n != 1 {
			t.Fatalf("the let-out joined %d places, want one", n)
		}
		stops.wantStops(t)
	})

	t.Run("held for a pause a later pause followed", func(t *testing.T) {
		h, flag, stops, taken, _ := gate(t)
		flag.set(8)

		h.must(ResumePause{Claim: h.token, Pause: 7})
		h.advance(0)

		h.wantState(StateSuspended)
		h.wantStarts(h.token, 0)
		if n := taken(); n != 0 {
			t.Fatalf("the launch joined %d places, want none while the project is paused", n)
		}
		stops.wantStops(t, stopCall{stop: PauseStop{Pause: 8, LaunchOwed: true}, landed: true})
	})

	// parked is a harness whose pacing parks its first launch a minute out — a pacer over a
	// limiter of one wake a minute whose one token the test spends before any launch — and the
	// count of the places joined in it (countedPacer).
	parked := func(t *testing.T) (*harness, *pauseFlag, *stopTransaction, func() int) {
		t.Helper()
		h, flag, stops := pauseHarness(t, queuedClaim())
		limiter := wakepace.New(1, 1, nil)
		limiter.Reserve(h.clock.Now())
		pacer, taken := countedPacer(h.deps.Clock, limiter, nil)
		h.deps.WakePace = WakePace{Pacer: pacer, Rate: 1, Burst: 1, Interval: limiter.Interval()}
		h.start(queuedClaim())
		return h, flag, stops, taken
	}
	// letOutOnce is the wake's turn meeting a pause that is over when the gate's own stop, which
	// the same moment of the clock fires, reads it again: the gate holds the wake and lets it out
	// at once, and the wake, which has paid, launches on no second place, writing its one line.
	letOutOnce := func(t *testing.T, h *harness, flag *pauseFlag, stops *stopTransaction, taken func() int, pause int64) {
		t.Helper()
		flag.set(pause)
		flag.clearAfterNextRead()

		h.advance(time.Minute)

		const heldLine = "supervise: the project is paused; its gate holds this claim's launch, and the claim is stopped for the pause"
		if lines := h.logs.lines(heldLine, "claim="+string(h.token), fmt.Sprintf("pause=%d", pause)); len(lines) != 1 {
			t.Fatalf("held lines = %v, want the pause's gate to have held the wake at its turn", lines)
		}
		h.wantStarts(h.token, 1)
		if n := taken(); n != 1 {
			t.Fatalf("the pacing joined %d places, want the one whose let-out the pause met", n)
		}
		if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token)); len(lines) != 1 {
			t.Fatalf("wake lines = %v, want one, written at the let-out", lines)
		}
		stops.wantStops(t)
	}

	// A wake the pacing parked took its place at the wake gate; a pause that meets its let-out at
	// its turn holds it, and its let-out runs it with no second place (wake.paid).
	t.Run("a wake the pacing parked, met by the pause at its turn", func(t *testing.T) {
		h, flag, stops, taken := parked(t)
		h.must(RequestSpawn{Claim: h.token})
		h.wantStarts(h.token, 0)

		letOutOnce(t, h, flag, stops, taken, 7)
	})

	// A launch the gate held at the wake gate took no place there; its let-out joins one for it
	// and parks it, and a second pause that meets it at that turn holds it again, whose let-out
	// joins no second place (letOutHeldLaunch, wake.paid).
	t.Run("a let-out the pacing parked, met by a second pause at its turn", func(t *testing.T) {
		h, flag, stops, taken := parked(t)
		flag.set(7)
		h.must(RequestSpawn{Claim: h.token})
		flag.set(0)
		h.must(ResumePause{Claim: h.token, Pause: 7})
		h.wantStarts(h.token, 0)
		if n := taken(); n != 1 {
			t.Fatalf("the let-out joined %d places, want one", n)
		}

		letOutOnce(t, h, flag, stops, taken, 8)
	})
}

// Every paced launch is told to the daemon at its let-out, with the moment the pacer let its place
// out (decision 14), and never at its join: the one the pacer lets out at its join is told then,
// and the one that waits is told only when its turn comes, of that turn's moment. A launch that
// waits is told that it waits instead (Deps.WakeQueued), which is what the resume's stuck clock
// reads to stop for the wait. Before the place-based pacing the parked launch was told its
// reserved moment at the park; a place has no moment until it is let out, so the stamp moves to
// the let-out.
func TestTheWakeLetOutStampIsTheMomentOfEachLetOut(t *testing.T) {
	type stamp struct {
		token claim.Token
		at    time.Time
	}
	var mu sync.Mutex
	var stamps []stamp
	var queued []claim.Token
	h := pacedHarness(t, wakeRate, func(d *Deps) {
		d.WakeLetOut = func(token claim.Token, _ string, _ claim.Role, at time.Time) {
			mu.Lock()
			defer mu.Unlock()
			stamps = append(stamps, stamp{token: token, at: at})
		}
		d.WakeQueued = func(token claim.Token, _ string, _ claim.Role) {
			mu.Lock()
			defer mu.Unlock()
			queued = append(queued, token)
		}
	})
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	start := h.clock.Now()

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})

	waiting := second.View().WakeWaiting
	if waiting == nil || !waiting.At.After(h.clock.Now()) {
		t.Fatalf("the second claim holds wake %+v, want one parked behind the first", waiting)
	}
	want := func(t *testing.T, want ...stamp) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(stamps) != len(want) {
			t.Fatalf("the daemon was told %+v, want %+v", stamps, want)
		}
		for i := range want {
			if stamps[i].token != want[i].token || !stamps[i].at.Equal(want[i].at) {
				t.Fatalf("stamp %d = %+v, want %+v", i, stamps[i], want[i])
			}
		}
	}
	want(t, stamp{token: h.token, at: start})
	mu.Lock()
	if len(queued) != 1 || queued[0] != secondToken {
		t.Fatalf("the daemon was told %v began to wait, want the second claim alone", queued)
	}
	mu.Unlock()

	h.clock.Advance(waiting.At.Sub(h.clock.Now()))
	second.Wait()

	h.wantStarts(secondToken, 1)
	want(t, stamp{token: h.token, at: start}, stamp{token: secondToken, at: waiting.At})
}

// A wait the daemon was told of ends with no let-out where a request the pacing lets through
// launches the claim in the waiting wake's place, and the daemon is told that end at the request's
// moment. The resume's stuck clock stops for a wait (Deps.WakeQueued), so a wait whose end went
// untold would leave that clock stopped, and a launch that then took no turn would never be
// reported. A request that launches nothing leaves the wake waiting at its place, and the daemon
// is told nothing for it.
func TestAWaitThatEndsWithNoLetOutIsToldAtItsEnd(t *testing.T) {
	var mu sync.Mutex
	var queued int
	var ended []time.Time
	h := pacedHarness(t, wakeRate, func(d *Deps) {
		d.WakeQueued = func(token claim.Token, _ string, _ claim.Role) {
			mu.Lock()
			defer mu.Unlock()
			if token == secondToken {
				queued++
			}
		}
		d.WakeLetOut = func(token claim.Token, _ string, _ claim.Role, at time.Time) {
			mu.Lock()
			defer mu.Unlock()
			if token == secondToken {
				ended = append(ended, at)
			}
		}
	})
	second := h.machineFor(wakeClaim(secondToken, StateSuspended))
	want := func(t *testing.T, wantQueued int, wantEnded ...time.Time) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if queued != wantQueued || !slices.EqualFunc(ended, wantEnded, time.Time.Equal) {
			t.Fatalf("the daemon was told %d waits began and waits ended at %v, want %d and %v", queued, ended, wantQueued, wantEnded)
		}
	}

	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(second, RequestRuleResume{Claim: secondToken, Reason: "it owns its issue's phase"})
	want(t, 1)

	h.mustPostTo(second, RequestForgetSession{Claim: secondToken, Session: second.Claim().Session})
	h.wantStarts(secondToken, 0)
	want(t, 1)

	h.clock.Advance(wakePart)
	h.mustPostTo(second, RequestResume{Claim: secondToken})
	h.wantStarts(secondToken, 1)
	want(t, 1, h.clock.Now())
}
