package supervise

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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
	h.deps.SessionGone = func(session string) { *stopped = append(*stopped, session) }
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
// such a claim, and a death ends one as well; neither sends a stop. A store that does not answer
// whose claim the name is held for sends none either. Across the same restart, a claim holding its
// own session is stopped as before, once (R9).
//
// The control (the pull request records the run): sessionGone sending the stop without asking
// the hold, as cybermonklabs/legion#4088 wrote it, records a stop of ses_implementer in the first
// three rows.
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
	for _, row := range []struct {
		name string
		// before is what changes in the store between the claim's registration and the restart.
		before func(h *harness)
		end    func(h *harness)
		want   []string
	}{
		{
			name:   "a death of a claim whose session another claim recorded first",
			before: func(h *harness) { h.store.setHold(sessionID(session), other) },
			end: dies(func(err error) bool {
				var refusal claim.Refusal
				return errors.As(err, &refusal) && refusal == claim.SessionHeldRefusal(session)
			}),
		},
		{
			name:   "the daemon's suspension of a claim whose session another claim recorded first",
			before: func(h *harness) { h.store.setHold(sessionID(session), other) },
			end: func(h *harness) {
				h.must(RequestSuspend{Claim: h.token, Reason: "the boot contains it", By: SuspendedByDaemon})
				h.wantState(StateSuspended)
			},
		},
		{
			name:   "a death of a claim whose hold the store does not answer",
			before: func(h *harness) { h.store.fail("HoldSessions", errors.New("the store is down")) },
			end:    dies(func(err error) bool { return errors.Is(err, errHoldUnanswered) }),
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
		})
	}
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
				h.m.deps.SessionGone = func(s string) {
					if !slices.ContainsFunc(h.calls("Suspend"), func(c fake.Call) bool { return c.Locator == loc }) {
						toldFirst = true
					}
					record(s)
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
