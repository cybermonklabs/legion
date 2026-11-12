package supervise

import (
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The read of a claim's process answers what the runtime says of it, and records that answer as
// the sweep's own report of the same process is recorded (ReadProcess, reportedProcess). The
// claim read here is the one the recovery meets: stored working over the process a death found
// gone, whose relaunch the lifecycle recheck refused, so what the daemon last heard of that
// process is all that tells it from a claim in a turn (Claim.ProcessGone).
//
// Every verdict is scripted, the uninteresting one included: the fake answers Alive when nothing
// is queued (runtime/fake's ScriptProbe), so a row that queued none would pass whichever way the
// read went. The two verdicts that find the process running clear what was charged for its death
// and write that clear, as judge writes it (Claim.DeathCharged); the two that find it gone leave
// both records standing; and the two that are no answer at all — a verdict of Uncertain and a
// runtime that could not be asked — leave the claim exactly as it stood.
func TestReadProcessAnswersEachVerdictAndRecordsProcessGone(t *testing.T) {
	for _, row := range []struct {
		name  string
		probe fake.ProbeResult
		want  ProcessRead
		// gone is what the claim reports of its process after the read, and charged what the
		// store holds of the death already charged for it.
		gone    bool
		charged bool
	}{
		{"nothing is there", fake.ProbeResult{Kind: runtime.Gone}, ProcessGone, true, true},
		{"what is there is not the process the claim records", fake.ProbeResult{Kind: runtime.NotRecordedProcess}, ProcessGone, true, true},
		{"the process is running", fake.ProbeResult{Kind: runtime.Alive}, ProcessRuns, false, false},
		{"the process is running at a stale address", fake.ProbeResult{Kind: runtime.StaleAddress}, ProcessRuns, false, false},
		{"the runtime cannot say what became of it", fake.ProbeResult{Kind: runtime.Uncertain}, ProcessUnanswered, true, true},
		{"the runtime itself could not be asked", fake.ProbeResult{Err: errBoom}, ProcessUnanswered, true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, dead := workingOverTheProcessItsRefusedRelaunchKept(t)
			probes := len(h.calls("Probe"))
			h.rt.ScriptProbe(row.probe)

			got := h.m.ReadProcess(h.ctx)

			if got != row.want {
				t.Fatalf("the read of the claim's process answered %q, want %q", got, row.want)
			}
			if read := h.wantCalls("Probe", probes+1)[probes]; read.Locator != dead {
				t.Errorf("the runtime was asked about %+v, want the process the claim records, %+v", read.Locator, dead)
			}
			if c := h.claim(); c.ProcessGone != row.gone {
				t.Errorf("the claim reports ProcessGone %v after the read, want %v", c.ProcessGone, row.gone)
			}
			if stored := h.store.load(h.token); stored.DeathCharged != row.charged {
				t.Errorf("the stored claim reports DeathCharged %v after the read, want %v", stored.DeathCharged, row.charged)
			}
			// The read reports and judges nothing else: no death, no relaunch, no stop.
			h.wantState(StateWorking)
			if c := h.claim(); c.Locator == nil || *c.Locator != dead {
				t.Errorf("the claim records %+v after the read, want the process it recorded, %+v", c.Locator, dead)
			}
			h.wantCalls("Spawn", 1)
			h.wantCalls("Resume", 0)
			h.wantCalls("Suspend", 0)
		})
	}
}

// Two claims are asked nothing and answer why no read was made, each with its own answer
// (ReadProcess): one that records no process, which has none to read (ProcessNotRecorded), and
// one whose paced launch waits over the process it records, whose fate that launch's turn
// already decides (ProcessReplacedByLaunch, launchWaits, wakeNow). probe leaves both alone for the
// same reasons.
//
// The scripted verdict is the one that would show a read had been made: a gone verdict over the
// claim that records nothing would set the record, and the alive verdict over the parked claim
// would clear the record its death left and the charge with it. Neither moves, and the runtime
// is asked nothing at all.
func TestReadProcessAsksNothingOfAClaimWithNoProcessOrADeadProcessAParkedLaunchReplaces(t *testing.T) {
	t.Run("a claim that records no process", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateSuspended)
		if c := h.claim(); c.Locator != nil {
			t.Fatalf("the suspended claim records %+v, want the process it let go", c.Locator)
		}
		probes := len(h.calls("Probe"))
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

		if got := h.m.ReadProcess(h.ctx); got != ProcessNotRecorded {
			t.Fatalf("the read of a claim that records no process answered %q, want %q", got, ProcessNotRecorded)
		}
		h.wantCalls("Probe", probes)
		if h.claim().ProcessGone {
			t.Error("the claim reports its process gone, want nothing recorded of a process it does not have")
		}
	})

	t.Run("a dead process a parked launch replaces", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateWorking)
		dead := h.locator()
		// A claim of the wave takes the turn the limiter had saved, so the relaunch this death
		// raises waits for the next one rather than running where it is raised.
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		waiting := h.wantWaitingWake(WakeLaunch)
		probes := len(h.calls("Probe"))
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})

		if got := h.m.ReadProcess(h.ctx); got != ProcessReplacedByLaunch {
			t.Fatalf("the read of a claim whose launch waits over its process answered %q, want %q", got, ProcessReplacedByLaunch)
		}
		h.wantCalls("Probe", probes)
		if !h.claim().ProcessGone {
			t.Error("the claim reports nothing of its process, want the gone verdict the park kept")
		}
		if c := h.claim(); c.Locator == nil || *c.Locator != dead {
			t.Errorf("the claim records %+v, want the dead process the park kept, %+v", c.Locator, dead)
		}
		h.wantWakeDueAt(waiting)
	})
}

// workingOverADeadProcessTheTakeBackMissed is the claim a take-back the store would not write
// leaves: the sweep found the process gone and the task of the turn it was running could not be
// handed back, so the death's own decision never ran and nothing of the claim was written but the
// gone verdict (died, interrupted, Claim.ProcessGone's first window). The claim stands stored
// working over a dead process, holding the task its dead turn held, confirmed, with nothing
// charged and no launch coming for it.
func workingOverADeadProcessTheTakeBackMissed(t *testing.T) (*harness, runtime.Locator) {
	t.Helper()
	h := newHarness(t)
	h.reach(StateWorking)
	dead := h.locator()
	h.store.fail("PutDelivery", errBoom)

	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}}); err == nil {
		t.Fatal("the death was judged whole, want the take-back the store would not write")
	}
	h.store.fail("PutDelivery", nil)

	h.wantState(StateWorking)
	h.wantBudgets(Budgets{})
	if c := h.claim(); c.Locator == nil || *c.Locator != dead || !c.ProcessGone {
		t.Fatalf("the claim records %+v with ProcessGone %v, want the dead process it kept, %+v, reported gone",
			c.Locator, c.ProcessGone, dead)
	}
	if p := h.pending(); p.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task of the dead turn still confirmed: its take-back was not written", p)
	}
	return h, dead
}

// A suspension of a claim stored working that no process stands behind lands where it arrives
// rather than being held (holdSuspension): there is no turn to cut off and no process to protect,
// and held instead it would wait for a turn that never ends. This is the suspension the fresh
// session posts, which may not be held at all (RequestSuspend.NeverHold, api's
// suspendForFreshSession): refused here, the claim it is asked over would have no door left.
//
// What runs is the take-back of the dead turn and the stop, in that order (suspendOverADeadProcess,
// letProcessGoFor): the task goes back unconfirmed under a new id, the process is let go, the
// claim is suspended by whoever asked, and nothing is charged.
func TestASuspendOverADeadProcessLandsWhereItArrives(t *testing.T) {
	h, dead := workingOverADeadProcessTheTakeBackMissed(t)
	held := h.pending().ID

	err := h.handle(RequestSuspend{Claim: testToken, Reason: "a fresh session was asked for",
		By: SuspendedByOperator, NeverHold: true})

	if err != nil {
		t.Fatalf("the suspension of a claim over a dead process returned %v, want it landed", err)
	}
	h.wantState(StateSuspended)
	c := h.claim()
	if c.Locator != nil || c.SuspensionHeld || c.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the suspended claim is %+v, want its process let go, nothing held, and the operator named as the source", c)
	}
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != dead {
		t.Errorf("the runtime was asked to stop %+v, want the dead process the claim recorded, %+v", suspend.Locator, dead)
	}
	if p := h.pending(); p.ID == held || !p.ConfirmedAt.IsZero() || !p.Interrupted {
		t.Errorf("the claim holds %+v, want the task of the dead turn back under a new id, unconfirmed and marked interrupted", p)
	}
	h.wantBudgets(Budgets{})
	h.wantCalls("Spawn", 1)
	h.wantCalls("Resume", 0)
	if stored := h.store.load(testToken); stored.State != StateSuspended {
		t.Errorf("the stored claim is %s, want the suspension written", stored.State)
	}
}

// The same claim after a daemon restart. The restart rebuilds the machine from the store alone,
// which holds no report of the dead process (Claim.ProcessGone is memory only), so the claim
// reads plain working and its suspension is held as any claim in a turn's is. The read of the
// process is what tells the two apart, and the row is driven after it: that is the order the
// route takes, reading the process before it posts the suspension.
func TestASuspendOverADeadProcessAfterARestartLandsOnceTheReadFindsItGone(t *testing.T) {
	h, dead := workingOverADeadProcessTheTakeBackMissed(t)
	h.restart()

	h.wantState(StateWorking)
	if h.claim().ProcessGone {
		t.Fatal("the restored claim reports its process gone, want the restart to have forgotten the report")
	}
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
	if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
		t.Fatalf("the read of the restored claim's process answered %q, want %q", got, ProcessGone)
	}

	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "a fresh session was asked for",
		By: SuspendedByOperator, NeverHold: true}); err != nil {
		t.Fatalf("the suspension of the restored claim returned %v, want it landed", err)
	}

	h.wantState(StateSuspended)
	if c := h.claim(); c.Locator != nil || c.SuspensionHeld {
		t.Fatalf("the suspended claim is %+v, want its process let go and nothing held", c)
	}
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != dead {
		t.Errorf("the runtime was asked to stop %+v, want the dead process the claim recorded, %+v", suspend.Locator, dead)
	}
	if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.Interrupted {
		t.Errorf("the claim holds %+v, want the task of the dead turn back unconfirmed and marked interrupted", p)
	}
	h.wantBudgets(Budgets{})
}

// The recovery's read can be the first report of a death there is: no sweep has reported it yet,
// or a restart forgot the one that did. No death was judged then (died), so the suspension that
// lands over the process does died's work for it (suspendOverADeadProcess): it sends the clean
// stop of the dead process's Envoy session (sessionGone), and it takes the dead turn's task back
// with the note of the end the read found (goneOutOfMemory). A pod that ran out of memory is the
// commonest end of a process in that window, so both verdicts of gone are asked, in both windows.
//
// The negative controls are the stop dropped from the suspension, where the listener would keep
// the dead session's role and issue claims while the fresh session forgets its id, and the
// verdict's OutOfMemory not recorded, where the out-of-memory row would send the task back
// without its note.
func TestASuspendOverAProcessTheReadFoundGoneStopsTheDeadSessionAndKeepsTheEndsNote(t *testing.T) {
	for _, window := range []struct {
		name    string
		restart bool
	}{
		{"before any sweep reported the death", false},
		{"after a restart forgot the report", true},
	} {
		for _, verdict := range []struct {
			name        string
			outOfMemory bool
		}{
			{"the process is gone", false},
			{"the pod ran out of memory", true},
		} {
			t.Run(window.name+": "+verdict.name, func(t *testing.T) {
				h, stopped := sessionStops(t)
				h.reach(StateWorking)
				if window.restart {
					h.restart()
				}
				h.wantState(StateWorking)
				held := h.pending()
				if held.ConfirmedAt.IsZero() || held.Interrupted {
					t.Fatalf("the claim holds %+v, want the task of a turn in flight, confirmed", held)
				}
				if len(*stopped) != 0 {
					t.Fatalf("stopped sessions %v before the read, want none: no death has been judged", *stopped)
				}
				h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone, OutOfMemory: verdict.outOfMemory})
				if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
					t.Fatalf("the read of the claim's process answered %q, want %q", got, ProcessGone)
				}

				if err := h.handle(RequestSuspend{Claim: testToken, Reason: "a fresh session was asked for",
					By: SuspendedByOperator, NeverHold: true}); err != nil {
					t.Fatalf("the suspension over the process the read found gone returned %v, want it landed", err)
				}

				h.wantState(StateSuspended)
				if !slices.Equal(*stopped, []string{session}) {
					t.Errorf("stopped sessions %v, want the dead process's own session once", *stopped)
				}
				p := h.pending()
				if p.ID == held.ID || !p.ConfirmedAt.IsZero() || !p.Interrupted || p.OutOfMemory != verdict.outOfMemory {
					t.Errorf("the claim holds %+v, want the dead turn's task back under a new id, unconfirmed, interrupted and marked out of memory %v",
						p, verdict.outOfMemory)
				}
				want := interruptedTask + held.Task
				if verdict.outOfMemory {
					want = interruptedTask + outOfMemoryTask + held.Task
				}
				if got := p.message(); got != want {
					t.Errorf("the task taken back is sent as %q, want %q", got, want)
				}
				if stored := h.store.load(testToken).Pending; stored == nil || stored.OutOfMemory != verdict.outOfMemory {
					t.Errorf("the stored task is %+v, want it marked out of memory %v", stored, verdict.outOfMemory)
				}
				h.wantBudgets(Budgets{})
			})
		}
	}
}

// A claim whose process is running keeps the answers it has today, which is what the landing
// above turns on: a suspension that may be held is held for the turn's end, and one that may not
// be held is refused, recording nothing (holdSuspension, RequestSuspend.NeverHold). The api's
// TestAFreshSessionIsRefusedForAClaimInATurn rests on that refusal.
//
// The read of the process is the whole of what separates this claim from the one above: both are
// stored working with a process recorded, and this one's runs.
func TestASuspendIsHeldOverALiveTurnWhereADeadProcessWouldLandIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Alive})
	if got := h.m.ReadProcess(h.ctx); got != ProcessRuns {
		t.Fatalf("the read of a live claim's process answered %q, want %q", got, ProcessRuns)
	}

	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 left implementing",
		By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension of a claim in a turn returned %v, want ErrSuspendHeld", err)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); !c.SuspensionHeld {
		t.Fatalf("the claim is %+v, want the suspension held for the turn's end", c)
	}

	var refused *RefusedError
	err := h.handle(RequestSuspend{Claim: testToken, Reason: "a fresh session was asked for",
		By: SuspendedByOperator, NeverHold: true})

	if !errors.As(err, &refused) || refused.State != StateWorking {
		t.Fatalf("the suspension that may not be held returned %v, want a refusal naming %s", err, StateWorking)
	}
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); c.SuspensionHeldBy != SuspendedByDaemon {
		t.Fatalf("the claim is %+v, want the daemon's hold untouched by the refusal", c)
	}
}

// Two claims stored working over a process the daemon has found gone keep their suspension held
// all the same, because neither is this row's to end (holdSuspension). A claim whose session is
// being compacted has an agent that is busy and an end of its own to give the claim back to
// (compactEnded), and a claim whose paced launch waits over its process has a turn coming that
// replaces that process (launchWaits, wakeNow). The read records the gone verdict in both — it
// judges nothing else, so it drops no compaction and runs no launch — and the suspension waits.
func TestASuspendIsHeldForACompactionOrAParkedLaunchOverADeadProcess(t *testing.T) {
	t.Run("a session in a compaction", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateIdle)
		h.must(RequestCompact{Claim: testToken})
		h.wantState(StateWorking)
		h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

		if got := h.m.ReadProcess(h.ctx); got != ProcessGone {
			t.Fatalf("the read of the compacting claim's process answered %q, want %q", got, ProcessGone)
		}
		if c := h.claim(); !c.ProcessGone || !c.Compacting {
			t.Fatalf("the claim is %+v, want the gone verdict recorded and the compaction still in flight", c)
		}

		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 left implementing",
			By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension of a claim in a compaction returned %v, want ErrSuspendHeld", err)
		}
		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)
		if c := h.claim(); !c.SuspensionHeld {
			t.Fatalf("the claim is %+v, want the suspension held for the compaction's end", c)
		}
	})

	t.Run("a parked launch over the dead process", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.reach(StateWorking)
		dead := h.locator()
		queueAhead(h, 1)
		h.observe(runtime.Gone)
		waiting := h.wantWaitingWake(WakeLaunch)

		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 left implementing",
			By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension of a claim whose launch waits over its process returned %v, want ErrSuspendHeld", err)
		}
		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)
		if c := h.claim(); !c.SuspensionHeld || c.Locator == nil || *c.Locator != dead {
			t.Fatalf("the claim is %+v, want the suspension held over the dead process the park kept, %+v", c, dead)
		}
		h.wantWakeDueAt(waiting)
	})
}
