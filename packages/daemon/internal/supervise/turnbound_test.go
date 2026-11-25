package supervise

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// turnBound is the bound these tests end a controller turn past (controller_turn_bound_minutes'
// default, dispatch://INFRA-2045 R2).
const turnBound = 90 * time.Minute

// inATurn is the controller handed its start message and in the turn that message began, a second
// after the harness clock's moment, so its start is after the last turn's end (Claim.OpenTurn
// reads a start and an end at one moment as closed): the turn whose start a TurnPastBound names.
func (h *harness) inATurn() time.Time {
	h.t.Helper()
	h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
	h.advance(time.Second)
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	return h.claim().LastTurnStart
}

// wantNoStartSince is a claim the runtime was asked to start nothing for since the counts given.
func (h *harness) wantNoStartSince(resumes, spawns int) {
	h.t.Helper()
	if gotResumes, gotSpawns := h.startsSoFar(); gotResumes != resumes || gotSpawns != spawns {
		h.t.Fatalf("the runtime was asked for %d resumes and %d spawns since, want none", gotResumes-resumes, gotSpawns-spawns)
	}
}

// wantTurnEnd is the claim's last turn end, in the machine and in the store, the one given: an R2
// end that ended nothing left the turn open, and wrote it so.
func (h *harness) wantTurnEnd(want time.Time) {
	h.t.Helper()
	for where, got := range map[string]time.Time{"the machine": h.claim().LastTurnEnd, "the store": h.store.load(h.token).LastTurnEnd} {
		if !got.Equal(want) {
			h.t.Fatalf("%s records the last turn's end at %s, want %s: the end that ended nothing closed the turn", where, got, want)
		}
	}
}

// displacedTurn is the claim through a turn a displacement cut off: the relaunched agent stands
// ready with that turn's start after its last end, since a displacement dates no end
// (Claim.LastTurnEnd).
func displacedTurn(h *harness) {
	h.t.Helper()
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: h.token})
	started := h.claim().LastTurnStart
	h.observeDisplaced(runtime.Gone)
	h.relaunched()
	if c := h.claim(); !c.LastTurnStart.Equal(started) || !c.LastTurnStart.After(c.LastTurnEnd) {
		h.t.Fatalf("the relaunched claim's turn started %s and the last ended %s, want the cut-off turn's start %s after its end",
			c.LastTurnStart, c.LastTurnEnd, started)
	}
}

// The machine's half of dispatch://INFRA-2045 R2, on the harness's moved clock: a controller turn
// open longer than its bound is ended when the keeper says so (TurnPastBound), whether or not steps
// still arrive in it, and the end counts under R1; the machine re-reads the turn first, and leaves
// alone a turn inside the bound, a turn that ended, and a turn begun since the keeper's read.
// internal/daemon's test of the same name holds the keeper's half, and internal/config's the
// default of 90. The control: on main no such event exists, so the test cannot compile there.
func TestAControllerTurnOpenPastItsBoundIsEnded(t *testing.T) {
	t.Run("a: a turn open one minute past the bound, steps still arriving, is ended and counted, and the session resumed", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(45 * time.Minute)
		h.stepped("bash", "toolu_long_1", `{"command":"sleep 1"}`, false)
		h.advance(46 * time.Minute)
		h.stepped("bash", "toolu_long_1", `{"command":"sleep 1"}`, true)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
		if p := h.claim().Pending; p == nil || p.Task != controllerStart {
			t.Fatalf("the relaunched controller holds %+v, want the task its ended turn served still owed", p)
		}
	})

	t.Run("b: a second such end in a row makes the next launch fresh", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound + time.Minute)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.relaunched()
		h.advance(time.Second)
		h.must(StreamTurnStart{Claim: h.token})
		start = h.claim().LastTurnStart
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("c: a turn that ends one minute before the bound changes nothing", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound - time.Minute)
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.advance(2 * time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateIdle)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
	})

	t.Run("the machine re-reads the turn: one inside the bound, and one begun since the keeper's read, are left alone", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		h.advance(turnBound - time.Minute)
		resumes, spawns := h.startsSoFar()
		// The machine's own clock decides, not the keeper's read.
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantState(StateWorking)

		// The turn ends and another begins; the keeper read the first.
		h.must(StreamTurnEnd{Claim: h.token})
		h.must(RequestDeliver{Claim: h.token, Task: "the next wake"})
		h.must(StreamTurnStart{Claim: h.token})
		h.advance(turnBound + time.Minute)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
	})

	t.Run("a refused take-back ends nothing and counts nothing, and the next post counts the turn once", func(t *testing.T) {
		h := controllerUp(t)
		start := h.inATurn()
		ended := h.claim().LastTurnEnd
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.store.fail("PutDelivery", errBoom)
		_ = h.handle(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		h.wantTurnEnd(ended)

		// The store answers again, and the keeper's next look posts the same turn.
		h.store.fail("PutDelivery", nil)
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("a launch the lifecycle refused after the take-back wrote the claim is taken back in the store too", func(t *testing.T) {
		h := controllerUp(t)
		h.inATurn()
		// A displacement cuts the turn off, and the relaunched agent's prompt of the task tells it
		// what became of that turn: the confirmation of that prompt carries the telling, so the
		// take-back of the task writes the claim with its delivery (interrupted, untellInFlight).
		h.advance(time.Minute)
		h.observeDisplaced(runtime.Gone)
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
		c := h.claim()
		if c.Pending == nil || !c.EndedWork.Told || c.EndedWork.ToldBy != c.Pending.ID {
			t.Fatalf("the relaunched controller holds %+v and the record %+v, want the task's confirmed prompt the one that told the record", c.Pending, c.EndedWork)
		}
		start, ended := c.LastTurnStart, c.LastTurnEnd
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		// The take-back writes the claim, with the end counted and the turn closed, before the
		// lifecycle check refuses the launch over the process the claim still records.
		store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		h.m.deps.Store = store
		_ = h.handle(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		h.wantTurnEnd(ended)

		// The lifecycle answers again, and the keeper's next look posts the same turn.
		store.failing = false
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("the R1 Error line of an end a take-back undid is written once, when the end stands, naming the session and both endings though the relaunch lost the session", func(t *testing.T) {
		// A controller whose session has never finished a turn, with one end counted.
		h := newHarnessOf(t, controllerClaim())
		h.reach(StateReady)
		h.dies()
		h.relaunched()
		h.wantEnds(1)
		if h.claim().SessionRun.TurnFinished {
			t.Fatal("the controller's session reads as having finished a turn, want one that never has")
		}
		start := h.inATurn()
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.store.fail("PutDelivery", errBoom)
		_ = h.handle(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		h.wantEnds(1)
		if lines := h.records.of(sessionNotReplacedLine); len(lines) != 0 {
			t.Fatalf("the refused take-back wrote %d lines that say a fresh session also failed, want none", len(lines))
		}

		// The relaunch the standing end makes finds the session gone, so it forgets the session
		// and its run and spawns a fresh agent (startFresh): the line still names what ended.
		h.store.fail("PutDelivery", nil)
		h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session anywhere", h.token, runtime.ErrSessionFileMissing)})
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})
		if got, gotSpawns := h.startsSoFar(); got != resumes+1 || gotSpawns != spawns+1 {
			t.Fatalf("the relaunch made %d resumes and %d spawns, want the one resume the runtime refused and one fresh spawn", got-resumes, gotSpawns-spawns)
		}
		if c := h.claim(); c.Session != "" || c.SessionRun.Ends != 0 {
			t.Fatalf("after the lost session's relaunch the claim records session %q with the run %+v, want both cleared", c.Session, c.SessionRun)
		}
		lines := h.records.of(sessionNotReplacedLine)
		if len(lines) != 1 || lines[0].level != slog.LevelError {
			t.Fatalf("the lines that say a fresh session also failed are %+v, want one at Error", lines)
		}
		if got, _ := lines[0].attr("session"); got.String() != session {
			t.Errorf("the Error line names session %q, want %q", got, session)
		}
		endings, _ := lines[0].attr("endings")
		for _, want := range []string{"died after the controller was ready", "its turn stayed open longer than 1h30m0s"} {
			if !strings.Contains(endings.String(), want) {
				t.Errorf("the Error line names the endings %q, want one that says %q", endings, want)
			}
		}
	})

	t.Run("a turn begun under a compaction is ended past the bound after a daemon restart", func(t *testing.T) {
		h := controllerUp(t)
		release, compacted := h.compactHeld()
		h.advance(time.Minute)
		h.must(StreamTurnStart{Claim: h.token})
		began := h.claim().LastTurnStart
		// The daemon restarts with the compaction and the turn under it running: the compaction's
		// record is memory only, so the new machine reads the turn from the store alone.
		h.restart()
		if c := h.claim(); c.State != StateWorking || !c.LastTurnStart.Equal(began) {
			t.Fatalf("the restarted machine reads the claim %s with its turn started %s, want working in the turn begun at %s", c.State, c.LastTurnStart, began)
		}
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: h.claim().LastTurnStart})

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
		release()
		<-compacted
	})

	t.Run("a compaction with no turn under it is left alone after a daemon restart", func(t *testing.T) {
		h := controllerUp(t)
		// A turn that runs a minute and ends, so its start is before its end.
		h.must(StreamTurnStart{Claim: h.token})
		h.advance(time.Minute)
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		release, compacted := h.compactHeld()
		h.restart()
		// The restarted machine knows no compaction: it reads a working claim whose start is that
		// of the turn which had ended before the compaction.
		if c := h.claim(); c.State != StateWorking || !c.LastTurnStart.Before(c.LastTurnEnd) {
			t.Fatalf("the restarted machine reads the claim %s, its turn started %s and the last ended %s; want working with a start before the end",
				c.State, c.LastTurnStart, c.LastTurnEnd)
		}
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: h.claim().LastTurnStart})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		release()
		<-compacted
	})

	t.Run("after a displacement, a compaction with no turn under it is left alone after a daemon restart", func(t *testing.T) {
		h := controllerUp(t)
		h.advance(time.Minute)
		h.must(StreamTurnStart{Claim: h.token})
		cutOff := h.claim().LastTurnStart
		// A displacement ends the turn without dating its end, so the relaunched controller
		// stands ready with that turn's start after its last end.
		h.observeDisplaced(runtime.Gone)
		h.relaunched()
		if c := h.claim(); !c.LastTurnStart.Equal(cutOff) || !c.LastTurnStart.After(c.LastTurnEnd) {
			t.Fatalf("the relaunched controller's turn started %s and the last ended %s, want the cut-off turn's start %s after its end",
				c.LastTurnStart, c.LastTurnEnd, cutOff)
		}
		h.advance(turnBound + time.Minute)
		release, compacted := h.compactHeld()
		defer func() {
			release()
			<-compacted
		}()
		// The daemon restarts during the compaction, and the new machine knows no compaction: it
		// reads the claim stored working, with the cut-off turn closed by the compaction's start.
		// The times are checked without stopping the row, so a break reads on to what R2 then does.
		h.restart()
		if c := h.claim(); c.State != StateWorking || !c.LastTurnStart.Before(c.LastTurnEnd) {
			t.Errorf("the restarted machine reads the claim %s, its turn started %s and the last ended %s; want working with the cut-off turn closed",
				c.State, c.LastTurnStart, c.LastTurnEnd)
		}
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: cutOff})

		c, stored := h.claim(), h.store.load(h.token)
		gotResumes, gotSpawns := h.startsSoFar()
		if c.State != StateWorking || gotResumes != resumes || gotSpawns != spawns || c.SessionRun.Ends != 0 || stored.SessionRun.Ends != 0 {
			t.Fatalf("R2 left the claim %s after %d resumes and %d spawns, with %d ends counted (%d stored); want it working, nothing started and no end counted",
				c.State, gotResumes-resumes, gotSpawns-spawns, c.SessionRun.Ends, stored.SessionRun.Ends)
		}
	})

	// A follow-up turn that starts under a compaction a daemon restart forgot, after the new
	// daemon's hello found the agent streaming and stopped asking: the turn is dated from its own
	// start, and ended past the bound. Both ways a claim reaches the compaction: after a turn it
	// finished, and after one a displacement cut off, which leaves that turn's end undated.
	for _, before := range []struct {
		name string
		turn func(h *harness)
	}{
		{"after a finished turn", func(h *harness) {
			h.must(StreamTurnStart{Claim: h.token})
			h.advance(time.Minute)
			h.must(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)
		}},
		{"after a displaced turn", displacedTurn},
	} {
		t.Run("a follow-up turn under a compaction a daemon restart forgot is dated and ended past the bound, "+before.name, func(t *testing.T) {
			h := controllerUp(t)
			before.turn(h)
			h.advance(time.Minute)
			release, compacted := h.compactHeld()
			defer func() {
				release()
				<-compacted
			}()
			h.restart()
			// The follow-up turn is already running at the new daemon's hello, which finds the
			// agent streaming and stops asking (askTurnOver); its agent_start then arrives.
			h.conn.SetStreaming(true)
			h.connect()
			h.advance(time.Minute)
			began := h.clock.Now()
			h.must(StreamTurnStart{Claim: h.token})
			for where, got := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(h.token)} {
				if !got.LastTurnStart.Equal(began) || !got.LastTurnStart.After(got.LastTurnEnd) {
					t.Errorf("%s dates the follow-up turn %s with the last turn ended %s, want it dated %s", where, got.LastTurnStart, got.LastTurnEnd, began)
				}
			}
			h.advance(turnBound + time.Minute)
			resumes, spawns := h.startsSoFar()
			h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: h.claim().LastTurnStart})

			h.wantResumedSince(resumes, spawns, session)
			h.wantEnds(1)
		})
	}

	t.Run("an operator's compaction past the bound is left alone and counts nothing", func(t *testing.T) {
		h := controllerUp(t)
		start := h.claim().LastTurnStart
		release, compacted := h.compactHeld()
		h.wantState(StateWorking)
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: start})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("the compaction answered %v, want it run to its end", err)
		}
	})

	t.Run("a turn that starts under an operator's compaction is dated from its own start, and ended past the bound", func(t *testing.T) {
		h := controllerUp(t)
		release, compacted := h.compactHeld()
		h.advance(time.Minute)
		began := h.clock.Now()
		h.must(StreamTurnStart{Claim: h.token})
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("the compaction answered %v, want it run to its end", err)
		}
		// What the keeper reads: the claim working, in the turn that began under the compaction.
		c := h.claim()
		if c.State != StateWorking || !c.LastTurnStart.Equal(began) || !c.LastTurnStart.After(c.LastTurnEnd) {
			t.Fatalf("after the compaction the claim is %s, its turn started %s and the last ended %s; want working in the turn that began at %s",
				c.State, c.LastTurnStart, c.LastTurnEnd, began)
		}
		if stored := h.store.load(h.token); !stored.LastTurnStart.Equal(began) {
			t.Fatalf("the store dates the claim's turn %s, want %s", stored.LastTurnStart, began)
		}
		h.advance(turnBound + time.Minute)
		resumes, spawns := h.startsSoFar()
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: c.LastTurnStart})

		h.wantResumedSince(resumes, spawns, session)
		h.wantEnds(1)
	})

	t.Run("after a displacement, an operator's compaction and the turn under it are not ended for the turn the displacement cut off", func(t *testing.T) {
		h := controllerUp(t)
		h.advance(time.Minute)
		h.must(StreamTurnStart{Claim: h.token})
		before := h.claim().LastTurnStart
		// A displacement ends the turn without writing its end, so the relaunched controller
		// stands ready with that turn's start after its last end.
		h.observeDisplaced(runtime.Gone)
		h.relaunched()
		if c := h.claim(); !c.LastTurnStart.Equal(before) || !c.LastTurnStart.After(c.LastTurnEnd) {
			t.Fatalf("the relaunched controller's turn started %s and the last ended %s, want the displaced turn's start %s after its end",
				c.LastTurnStart, c.LastTurnEnd, before)
		}
		h.advance(turnBound + time.Minute)
		release, compacted := h.compactHeld()
		resumes, spawns := h.startsSoFar()
		// The keeper reads a working claim with that start past the bound.
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: before})
		h.must(StreamTurnStart{Claim: h.token})
		if got := h.claim().LastTurnStart; !got.After(before) {
			t.Fatalf("the turn that began under the compaction is dated %s, want after the displaced turn's %s", got, before)
		}
		h.must(TurnPastBound{Claim: h.token, Bound: turnBound, TurnStart: before})

		h.wantState(StateWorking)
		h.wantNoStartSince(resumes, spawns)
		h.wantEnds(0)
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("the compaction answered %v, want it run to its end", err)
		}
	})
}
