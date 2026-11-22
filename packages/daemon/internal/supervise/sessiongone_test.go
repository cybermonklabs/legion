package supervise

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// sessionStops is a harness whose Envoy clean stops are recorded rather than sent, over a claim
// walked to ready: its registration is the session the stop must name.
func sessionStops(t *testing.T) (*harness, *[]string) {
	t.Helper()
	return sessionStopsOf(t, queuedClaim())
}

// sessionStopsOf is sessionStops over the queued claim c; the walks drive c's token.
func sessionStopsOf(t *testing.T, c Claim) (*harness, *[]string) {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	stopped := &[]string{}
	h.deps.SessionGone = func(session string) <-chan struct{} {
		*stopped = append(*stopped, session)
		return nil
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, stopped
}

// wantStops is the clean stops a row recorded against the ones it wants, failing with what went
// wrong in words: no stop where one is owed, a stop where none is, or more than one.
func wantStops(t *testing.T, got, want []string) {
	t.Helper()
	switch {
	case slices.Equal(got, want):
	case len(got) == 0:
		t.Fatalf("no clean stop of the session was recorded, want %v", want)
	case len(want) == 0:
		t.Fatalf("the clean stop was recorded %d times (%v), want none", len(got), got)
	default:
		t.Fatalf("the clean stop was recorded %d times (%v), want %v", len(got), got, want)
	}
}

// A closed stream says nothing about the process that opened it: a daemon restart closes every
// one of them while the agents run on, and stopping those sessions would unseat live roles. So the
// clean stop of a claim's Envoy session follows the probe's verdict, never the close, and it
// carries the session the claim registered.
func TestTheSessionStopFollowsTheProbeNotTheClosedStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe runtime.ObservationKind
		want  []string
	}{
		{"the probe finds the process gone", runtime.Gone, []string{session}},
		{"the probe finds the process alive", runtime.Alive, nil},
		{"the probe cannot tell", runtime.Uncertain, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stopped := sessionStops(t)
			h.reach(StateReady)
			h.rt.ScriptProbe(fake.ProbeResult{Kind: tc.probe})

			h.must(StreamClosed{Claim: testToken})

			if !slices.Equal(*stopped, tc.want) {
				t.Fatalf("stopped sessions %v, want %v", *stopped, tc.want)
			}
		})
	}
}

// The other runtime reaches the same judgement without a stream at all: under Kubernetes the
// launcher reports its child's exit (shimwire.LauncherState.LastExit), which the sandbox runtime
// observes as gone, so that claim's session is stopped too.
func TestALauncherReportedExitStopsTheSession(t *testing.T) {
	h, stopped := sessionStops(t)
	h.reach(StateReady)

	h.observe(runtime.Gone)

	if !slices.Equal(*stopped, []string{session}) {
		t.Fatalf("stopped sessions %v, want the claim's own session once", *stopped)
	}
}

// A claim whose agent never registered holds no session, so its death stops nothing: there is no
// registration in the listener to end.
func TestADeathBeforeAnyRegistrationStopsNoSession(t *testing.T) {
	h, stopped := sessionStops(t)
	h.launch()
	h.connect()
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

	h.must(StreamClosed{Claim: testToken})

	if len(*stopped) != 0 {
		t.Fatalf("stopped sessions %v, want none for a claim that registered none", *stopped)
	}
}

// The clean stop deletes the registration the Envoy listener holds for the session id, whichever
// process wrote it last, so a claim that records a session another claim recorded first sends no
// stop when its process ends (sessionGone, plan E's t-e-54): that claim's live process can be the
// last writer, as after a rollback, and the stop would delete its registration. The boot suspends
// such a claim, and a death ends one as well; neither sends a stop, and each says so in the line
// of a claim that does not hold its session. A store that does not answer whose claim the name is
// held for sends none either, and says so in a line of its own at Warn, since that claim may well
// hold its session. Across the same restart, a claim holding its own session is stopped as
// before, once, with neither line (R9).
//
// The controls (the pull request records the runs): sessionGone sending the stop without asking
// the hold, as cybermonklabs/legion#4088 wrote it, records a stop of ses_implementer in the first
// three rows; one line for both answers, as 24f9841a08 wrote it, fails the store row's line.
func TestAClaimThatDoesNotHoldItsSessionSendsNoStopOfIt(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	// dies is the probe's verdict of a death, whose relaunch resumes the recorded session and so
	// is refused by the same hold, with refused the error that relaunch must answer.
	dies := func(refused func(error) bool) func(h *harness) {
		return func(h *harness) {
			h.t.Helper()
			err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
			if !refused(err) {
				h.t.Fatalf("the death's relaunch = %v, want the hold's refusal of the session it resumes", err)
			}
		}
	}
	const unheld = "no clean stop of the Envoy session of a claim that does not hold it"
	const unanswered = "no clean stop of the Envoy session: the store did not answer whether the claim holds it"
	for _, row := range []struct {
		name string
		// before is what changes in the store between the claim's registration and the restart.
		before func(h *harness)
		end    func(h *harness)
		want   []string
		// line is the log line the row's withheld stop writes, "" for none.
		line string
	}{
		{
			name:   "a death of a claim whose session another claim recorded first",
			before: func(h *harness) { h.store.setHold(sessionID(session), other) },
			end: dies(func(err error) bool {
				var refusal claim.Refusal
				return errors.As(err, &refusal) && refusal == claim.SessionHeldRefusal(session)
			}),
			line: unheld,
		},
		{
			name:   "the daemon's suspension of a claim whose session another claim recorded first",
			before: func(h *harness) { h.store.setHold(sessionID(session), other) },
			end: func(h *harness) {
				h.must(RequestSuspend{Claim: h.token, Reason: "the boot contains it", By: SuspendedByDaemon})
				h.wantState(StateSuspended)
			},
			line: unheld,
		},
		{
			name:   "a death of a claim whose hold the store does not answer",
			before: func(h *harness) { h.store.fail("HoldSessions", errors.New("the store is down")) },
			end:    dies(func(err error) bool { return errors.Is(err, errHoldUnanswered) }),
			line:   unanswered,
		},
		{
			name:   "the daemon's suspension of a claim holding its own session",
			before: func(*harness) {},
			end: func(h *harness) {
				h.must(RequestSuspend{Claim: h.token, Reason: "the issue left the workflow", By: SuspendedByDaemon})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, stopped := sessionStops(t)
			h.reach(StateReady)
			row.before(h)
			h.restart()
			row.end(h)
			wantStops(t, *stopped, row.want)
			for _, line := range []string{unheld, unanswered} {
				n := len(h.logs.lines(line))
				if want := map[bool]int{true: 1}[line == row.line]; n != want {
					t.Fatalf("the line %q was written %d times, want %d", line, n, want)
				}
			}
			if row.line == unanswered && len(h.logs.lines(unanswered, "level=WARN")) != 1 {
				t.Fatalf("the line %q is not at Warn: %v", unanswered, h.logs.lines(unanswered))
			}
		})
	}
}

// The hold check a clean stop asks first runs on a context the decision's end does not cancel
// (sessionGone): a holder that has not admitted its names since a restart asks the store, and the
// daemon's stop must not cut that ask short, since sessionStop.send was written so the daemon's
// stop does not cut a clean stop short. So a restarted holder whose decision's context has ended
// is still stopped, once.
//
// The control (the pull request records the run): the check on the decision's context, as
// 3bce483568 wrote it, records no stop.
func TestARestartedHoldersStopIsNotCutShortByTheEndOfItsDecision(t *testing.T) {
	h, stopped := sessionStops(t)
	h.reach(StateReady)
	h.restart()
	ended, cancel := context.WithCancel(h.ctx)
	cancel()
	h.m.mu.Lock()
	h.m.sessionGone(ended)
	h.m.mu.Unlock()
	wantStops(t, *stopped, []string{session})
}

// A suspension that ends the claim's process sends the clean stop of its Envoy session, so the
// listener reads the session gone at once rather than live for the rest of its window, and a
// message to a released role finds it gone and raises the wake that brings it back. The stop is
// sent once for each process end: where the runtime's stop has returned (suspendNow,
// letProcessGoFor, the root's exit), and over a process a report found gone whatever the runtime
// answered (suspendOverADeadProcess). A death under a held suspension is one end, stopped once by
// the death and not again by the suspension it ends (died, endHeld). A suspension that ends no
// process sends nothing, and neither does a stop the runtime refused over a process that may still
// run: deleting a running agent's registration would leave it unreachable until its next
// heartbeat.
//
// The negative controls are the stop taken out of suspendNow, where the first five rows and the
// release before the repeated suspension record no stop, and letProcessGoFor ignoring the
// stop-already-sent argument (cleanStopSent), where the suspension over a process the read found
// gone and the death under a held release each record two.
func TestASuspensionThatEndsAProcessStopsItsSessionOnce(t *testing.T) {
	refused := errors.New("the pod would not stop")
	held := func(t *testing.T, h *harness, by SuspendSource) {
		t.Helper()
		if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the test suspends it", By: by}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension of a claim in a turn = %v, want it held for the turn", err)
		}
	}
	overAProcessTheReadFoundGone := func(t *testing.T, h *harness) {
		t.Helper()
		h.reach(StateWorking)
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
		if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
			t.Fatalf("the read of the claim's process answered %q, want %q", got, ProcessGone)
		}
	}
	stalledUnderAHold := func(t *testing.T, h *harness) StallBackstop {
		t.Helper()
		h.workingOnATask("implement the plan")
		held(t, h, SuspendedByOperator)
		last := h.clock.Now().Add(-backstopBound - time.Minute)
		stalled := StepRecord{At: last, TurnStartedAt: last, Tool: "bash", CallID: "toolu_turn1_0", StartedAt: last}
		h.m.steppedAt, h.m.claim.Step = last, stalled
		return h.backstopOf(last, stalled)
	}
	for _, row := range []struct {
		name string
		root bool
		// end drives a claim from queued to the end the row is about, asserting what the row
		// says of the claim on the way.
		end  func(t *testing.T, h *harness, stopped *[]string)
		want []string
	}{
		{
			name: "an idle release of an idle claim",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateIdle)
				loc := h.locator()
				// The stop is dated when the machine tells it, so the runtime must have been asked
				// to stop the process by then: that is what puts the process's last heartbeat
				// before the moment the listener deletes under.
				record, toldFirst := h.m.deps.SessionGone, false
				h.m.deps.SessionGone = func(s string) <-chan struct{} {
					if !slices.ContainsFunc(h.calls("Suspend"), func(c fake.Call) bool { return c.Locator == loc }) {
						toldFirst = true
					}
					return record(s)
				}
				h.must(RequestSuspend{Claim: h.token, Reason: "the idle pod was released", By: SuspendedByReleased})
				h.wantState(StateSuspended)
				if by := h.claim().SuspendedBy; by != SuspendedByReleased {
					t.Fatalf("the claim is suspended by %q, want %q", by, SuspendedByReleased)
				}
				if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
					t.Fatalf("the runtime was asked to stop %+v, want the claim's process %+v", suspend.Locator, loc)
				}
				if toldFirst {
					t.Fatalf("the clean stop was told before the runtime was asked to stop %+v, so its moment would come before the process's last heartbeat", loc)
				}
			},
			want: []string{session},
		},
		{
			name: "an operator's suspension of an idle claim",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateIdle)
				h.must(RequestSuspend{Claim: h.token, Reason: "the operator suspends it", By: SuspendedByOperator})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a daemon's suspension of a ready claim",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateReady)
				h.must(RequestSuspend{Claim: h.token, Reason: "the issue left the workflow", By: SuspendedByDaemon})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "an idle release of a working claim, at its turn's end",
			end: func(t *testing.T, h *harness, stopped *[]string) {
				h.reach(StateWorking)
				held(t, h, SuspendedByReleased)
				if len(*stopped) != 0 {
					t.Fatalf("the clean stop was recorded (%v) while the suspension waits for the turn, want none yet: the process still runs", *stopped)
				}
				h.must(StreamTurnEnd{Claim: h.token})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "an operator's suspension of a working claim, at its deadline",
			end: func(t *testing.T, h *harness, stopped *[]string) {
				h.reach(StateWorking)
				held(t, h, SuspendedByOperator)
				if len(*stopped) != 0 {
					t.Fatalf("the clean stop was recorded (%v) while the suspension waits for the turn, want none yet: the process still runs", *stopped)
				}
				h.advance(testStop)
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a suspension over a process the read found gone",
			end: func(t *testing.T, h *harness, _ *[]string) {
				overAProcessTheReadFoundGone(t, h)
				h.must(RequestSuspend{Claim: h.token, Reason: "a fresh session was asked for", By: SuspendedByOperator, NeverHold: true})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a suspension over a process the read found gone, whose stop the runtime refuses",
			end: func(t *testing.T, h *harness, _ *[]string) {
				overAProcessTheReadFoundGone(t, h)
				h.rt.FailSuspend(refused)
				h.must(RequestSuspend{Claim: h.token, Reason: "a fresh session was asked for", By: SuspendedByOperator, NeverHold: true})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a death while an idle release is held",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateWorking)
				held(t, h, SuspendedByReleased)
				h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
				h.must(StreamClosed{Claim: h.token})
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a suspension of a claim already suspended",
			end: func(t *testing.T, h *harness, stopped *[]string) {
				h.reach(StateIdle)
				h.must(RequestSuspend{Claim: h.token, Reason: "the idle pod was released", By: SuspendedByReleased})
				h.wantState(StateSuspended)
				if !slices.Equal(*stopped, []string{session}) {
					t.Fatalf("the release recorded clean stops %v, want %v", *stopped, []string{session})
				}
				// The release's own stop is the row before; this row is the suspension after it.
				*stopped = nil
				h.must(RequestSuspend{Claim: h.token, Reason: "the operator suspends it", By: SuspendedByOperator})
				if by := h.claim().SuspendedBy; by != SuspendedByOperator {
					t.Fatalf("the claim is suspended by %q, want the operator's suspension to have taken it over", by)
				}
			},
		},
		{
			name: "an idle release of an idle claim whose stop the runtime refuses",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateIdle)
				loc := h.locator()
				h.rt.FailSuspend(refused)
				if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the idle pod was released", By: SuspendedByReleased}); !errors.Is(err, refused) {
					t.Fatalf("the release whose stop the runtime refused = %v, want that refusal", err)
				}
				h.wantState(StateIdle)
				if c := h.claim(); c.Locator == nil || *c.Locator != loc {
					t.Fatalf("the claim records %+v, want the process the runtime would not stop, %+v", c.Locator, loc)
				}
			},
		},
		{
			name: "a held suspension the stall backstop's replacement ends",
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.must(stalledUnderAHold(t, h))
				h.wantState(StateSuspended)
			},
			want: []string{session},
		},
		{
			name: "a held suspension the stall backstop's replacement ends, whose stop the runtime refuses",
			end: func(t *testing.T, h *harness, _ *[]string) {
				backstop := stalledUnderAHold(t, h)
				h.rt.FailSuspend(refused)
				h.must(backstop)
				h.wantState(StateSuspended)
			},
		},
		{
			name: "the tree root's own exit",
			root: true,
			end: func(t *testing.T, h *harness, _ *[]string) {
				h.reach(StateIdle)
				h.exit()
				h.wantState(StateSuspended)
				if by := h.claim().SuspendedBy; by != SuspendedByExit {
					t.Fatalf("the root is suspended by %q, want %q", by, SuspendedByExit)
				}
			},
			want: []string{session},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := queuedClaim()
			if row.root {
				c = rootClaim()
			}
			h, stopped := sessionStopsOf(t, c)
			row.end(t, h, stopped)
			wantStops(t, *stopped, row.want)
		})
	}
}

// A relaunch starts no process while a clean stop of the session the claim records is in flight
// (awaitSessionStops). The stop reads which client process the listener's registration names
// before it deletes it, and a relaunch that had registered first would be the one it named. Here
// the death's own decision relaunches the claim: the launch says it waits, starts nothing while
// the stop runs, and resumes the recorded session once the stop has ended. A suspension that comes
// while it waits cuts the launch short, as it cuts a start the runtime is making short, and
// charges nothing beyond the one launch failure the death itself charged (relaunchAfterFailure).
//
// The suspension is sent from a goroutine of its own, under a bound of its own: a wait that did
// not end with its context would keep the death's decision, and the machine's lock, for as long as
// the stop runs, and the row fails here, saying so, rather than hanging to the binary's timeout.
func TestARelaunchStartsNoProcessWhileTheCleanStopIsInFlight(t *testing.T) {
	for _, suspends := range []bool{false, true} {
		name := "the stop ends"
		if suspends {
			name = "a suspension comes first"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := sessionStops(t)
			stopping := make(chan struct{})
			h.m.deps.SessionGone = func(string) <-chan struct{} { return stopping }
			h.reach(StateReady)
			died := make(chan error, 1)
			go func() {
				died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{
					Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
			}()
			// The launch either says it waits or, with nothing to hold it, resumes the session at
			// once; either ends the wait for it, and only the first is right.
			testwait.Eventually(t, "the relaunch to wait for the clean stop in flight, or to resume", func() bool {
				return len(h.records.of(launchWaitsForSessionStop)) > 0 || len(h.calls("Resume")) > 0
			})
			if resumes := h.calls("Resume"); len(resumes) != 0 {
				t.Fatalf("the relaunch resumed the session %d times while the stop of the process before it was in flight, want none", len(resumes))
			}

			bound := testwait.Bounded(t, time.Minute, time.Second)
			if suspends {
				suspended := make(chan error, 1)
				go func() {
					suspended <- h.m.Handle(h.ctx, RequestSuspend{Claim: h.token, Reason: "the operator stops it", By: SuspendedByOperator})
				}()
				select {
				case err := <-suspended:
					if err != nil {
						t.Fatalf("suspend: %v", err)
					}
				case <-time.After(bound):
					// Ending the stop lets the death's decision go, so the row's own failure is
					// what this run reports.
					close(stopping)
					t.Fatalf("the suspension was not decided within %s while the relaunch waited for the clean stop: the wait went on past the suspension's cancel, holding the death's decision", bound)
				}
			} else {
				close(stopping)
			}
			select {
			case <-died:
			case <-time.After(bound):
				t.Fatalf("the death's decision did not return within %s once the wait for the stop was over", bound)
			}
			h.m.Wait()

			if failures := h.claim().Budgets.LaunchFailures; failures != 1 {
				t.Fatalf("the claim is charged %d launch failures, want the death's own one and none for the wait", failures)
			}
			if suspends {
				h.wantCalls("Resume", 0)
				h.wantState(StateSuspended)
				return
			}
			if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != h.claim().SessionFile {
				t.Fatalf("the relaunch resumed %q, want the recorded session %q", resumed.Spec.ResumeSessionFile, h.claim().SessionFile)
			}
		})
	}
}

// A relaunch holds its start for as long as the clean stop in flight runs, however long that is
// (awaitSessionStops): the wait sets no timer of its own, and only the stop's end or the start's
// own context ends it. The stop's budget (CleanSessionStopBudget in internal/daemon, 5 s) is what
// bounds the wait; a wait that gave up sooner, on a time of its own, would start the relaunch
// while a slow listener had still not answered the stop's read, and the relaunch would register
// before it, which is the order the wait exists to prevent. The other relaunch tests end the stop
// the moment the launch says it waits, so none of them sees such a wait. Here the bubble's clock,
// which the time package reads, moves an hour, the machine's own (Deps.Clock) moves an hour, and
// the bubble's moves an hour more. A timer that gives up within an hour on either clock, or a poll
// paced at an hour or less by either clock that reads the other, therefore gives up while the stop
// is still in flight.
func TestARelaunchWaitsForTheCleanStopHoweverLongItRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := sessionStops(t)
		stopping := make(chan struct{})
		h.m.deps.SessionGone = func(string) <-chan struct{} { return stopping }
		h.reach(StateReady)
		loc := h.locator()
		died := make(chan error, 1)
		go func() {
			died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{
				Locator: loc, Kind: runtime.Gone, At: h.clock.Now()}})
		}()
		synctest.Wait()
		if len(h.records.of(launchWaitsForSessionStop)) == 0 || len(h.calls("Resume")) != 0 {
			close(stopping)
			<-died
			t.Fatalf("the death's relaunch logged %d waits for the clean stop in flight and resumed %d times, want it waiting and not resumed",
				len(h.records.of(launchWaitsForSessionStop)), len(h.calls("Resume")))
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		h.clock.Advance(time.Hour)
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		if resumes := h.calls("Resume"); len(resumes) != 0 {
			close(stopping)
			<-died
			t.Fatalf("the relaunch resumed the session %d times while the clean stop had been in flight for two hours on the bubble's clock and an hour on the machine's: the wait for the stop ended before the stop did", len(resumes))
		}
		close(stopping)
		if err := <-died; err != nil {
			t.Fatalf("the death's decision once the stop ended: %v", err)
		}
		h.m.Wait()
		if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != h.claim().SessionFile {
			t.Fatalf("the relaunch resumed %q, want the recorded session %q", resumed.Spec.ResumeSessionFile, h.claim().SessionFile)
		}
	})
}

// The same wait, outside a synctest bubble: the machine's own clock (Deps.Clock), which is the
// wall clock in production (RealClock), moves an hour while the clean stop stays in flight, and
// the death's decision, which holds the relaunch, has still not returned a second later. A wait
// that gave up on a timer of that clock, the way a machine times every other wait (Timeouts),
// returns it at once. The daemon's own tests cannot see such a timer: their machines run on a
// clock whose timers never fire. The clock moves on a goroutine of its own, since a timer whose
// callback is the machine's Handle waits for the machine's lock, which the death's decision holds
// until the stop ends.
func TestARelaunchWaitsForTheCleanStopHoweverFarTheMachinesClockMoves(t *testing.T) {
	h, _ := sessionStops(t)
	stopping := make(chan struct{})
	h.m.deps.SessionGone = func(string) <-chan struct{} { return stopping }
	h.reach(StateReady)
	loc := h.locator()
	died := make(chan error, 1)
	go func() {
		died <- h.m.Handle(h.ctx, RuntimeObservation{Observation: runtime.Observation{
			Locator: loc, Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	testwait.Eventually(t, "the relaunch to wait for the clean stop in flight, or to resume", func() bool {
		return len(h.records.of(launchWaitsForSessionStop)) > 0 || len(h.calls("Resume")) > 0
	})
	advanced := make(chan struct{})
	go func() {
		h.clock.Advance(time.Hour)
		close(advanced)
	}()
	select {
	case err := <-died:
		close(stopping)
		t.Fatalf("the death's decision returned (%v), resuming the session %d times, while the clean stop was in flight and the machine's clock had moved an hour: the wait for the stop ended before the stop did",
			err, len(h.calls("Resume")))
	case <-time.After(time.Second):
	}
	close(stopping)
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case err := <-died:
		if err != nil {
			t.Fatalf("the death's decision once the stop ended: %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("the death's decision did not return within %s of the stop's end", bound)
	}
	select {
	case <-advanced:
	case <-time.After(bound):
		t.Fatalf("the machine's clock had not moved its hour within %s of the stop's end", bound)
	}
	h.m.Wait()
	if resumed := h.wantCalls("Resume", 1)[0]; resumed.Spec.ResumeSessionFile != h.claim().SessionFile {
		t.Fatalf("the relaunch resumed %q, want the recorded session %q", resumed.Spec.ResumeSessionFile, h.claim().SessionFile)
	}
}

// Two clean stops of one session can be in flight at once. A death whose relaunch the wake pacing
// parked keeps the dead process recorded until the wake's turn (parkWake), so a suspension that
// comes meanwhile stops that process again and sends a second stop (sessionGone's own doc). Either
// can be the one still in flight when the claim next starts, since each runs on a goroutine of its
// own to its own budget, and the relaunch registers after every read a stop makes only if it
// waits for both (awaitSessionStops). Here one of the two has ended when the operator resumes the
// claim, the death's in one row and the suspension's in the other: the launch says it waits,
// starts nothing, and resumes the recorded session once the other stop has ended too.
//
// The negative controls are a wait that looks at the newest stop alone, which resumes at once in
// the second row, and a wait that ends with the oldest stop, or that forgets the rest once the
// oldest has ended, which resumes at once in the first.
func TestARelaunchWaitsForBothStopsOfASuspensionOverAParkedRelaunch(t *testing.T) {
	for _, row := range []struct {
		name string
		// ended is the stop that has ended when the operator resumes the claim: 0 is the death's,
		// 1 the suspension's.
		ended int
	}{
		{name: "the death's stop has ended and the suspension's is in flight", ended: 0},
		{name: "the suspension's stop has ended and the death's is in flight", ended: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			stops := []chan struct{}{make(chan struct{}), make(chan struct{})}
			told := 0
			tells := func() int {
				mu.Lock()
				defer mu.Unlock()
				return told
			}
			h := pacedHarness(t, wakeRate, func(d *Deps) {
				d.SessionGone = func(string) <-chan struct{} {
					mu.Lock()
					defer mu.Unlock()
					if told == len(stops) {
						t.Errorf("a clean stop was told %d times, want the death's and the suspension's alone", told+1)
						return nil
					}
					told++
					return stops[told-1]
				}
			})
			h.reach(StateReady)
			// A claim of the wave takes the turn the limiter has, so the death's relaunch waits
			// for the next one over the dead process.
			queueAhead(h, 1)
			h.observe(runtime.Gone)
			h.wantWaitingWake(WakeLaunch)
			if n := tells(); n != 1 {
				t.Fatalf("the death told %d clean stops, want 1", n)
			}
			h.must(RequestSuspend{Claim: h.token, Reason: "the operator suspends it", By: SuspendedByOperator})
			h.wantState(StateSuspended)
			if n := tells(); n != 2 {
				t.Fatalf("the death and the suspension over its parked relaunch told %d clean stops, want 2, one each", n)
			}
			h.wantCalls("Resume", 0)

			close(stops[row.ended])
			bound := testwait.Bounded(t, time.Minute, time.Second)
			resumed := make(chan error, 1)
			go func() { resumed <- h.m.Handle(h.ctx, RequestResume{Claim: h.token}) }()
			testwait.Eventually(t, "the resume to wait for the clean stop still in flight, or to resume", func() bool {
				return len(h.records.of(launchWaitsForSessionStop)) > 0 || len(h.calls("Resume")) > 0
			})
			if resumes := h.calls("Resume"); len(resumes) != 0 {
				t.Fatalf("the resume started the session %d times while the other clean stop was in flight, want none", len(resumes))
			}

			close(stops[1-row.ended])
			select {
			case err := <-resumed:
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
			case <-time.After(bound):
				t.Fatalf("the resume was not decided within %s of both clean stops ending", bound)
			}
			h.m.Wait()
			if resumes := h.wantCalls("Resume", 1); resumes[0].Spec.ResumeSessionFile != h.claim().SessionFile {
				t.Fatalf("the resume started %q, want the recorded session %q", resumes[0].Spec.ResumeSessionFile, h.claim().SessionFile)
			}
		})
	}
}
