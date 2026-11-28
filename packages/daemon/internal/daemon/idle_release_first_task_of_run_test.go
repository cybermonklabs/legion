package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dispatch://INFRA-2746 R1 at the release of a pod, at a pause's stop and at a release a pause
// cancels (check c-idle-keeps-nothing-for-a-claim-that-owes-nothing, its sixth control): a
// released claim of the open phase's role that has confirmed no task of the issue's run, and
// holds the first task of that run and phase unconfirmed, its send failed before any turn, still
// gets a backstop. Nothing else sends that task again while its pod is gone, so the backstop is
// the one wake that delivers it. Two shapes reach it: a claim that has retired no run, and a live
// claim kept across a new run N+1, which retired run N. The same claim whose phases row records
// its completion of the row's entry owes nothing a wake could bring back, and gets none.
//
// A claim that has retired no run serves the run and the entry of the task it holds of the
// issue's run and phase, as the owed answer reads them (supervise.Claim.ServingAt), so it gets
// the backstop because it owes its phase. The kept claim still serves run N, owes nothing, and
// gets it by the first kind of claim that owes nothing (holdsUndeliveredEntryTask), whose terms
// are that it is behind its entry or still serves an earlier run. A kept claim that has served no
// entry is level with its row's entry (0 compares with none), so it takes that kind for the
// earlier run it serves alone and gets one too, and gets none where its row records its
// completion. A kept claim whose turn confirmed the first task of run N+1 serves that task's run
// and entry, level with its row, owes its phase and gets one. A kept claim and a claim that has
// retired no run get none when they hold no task, or hold a task of another run or of another
// phase unconfirmed.
//
// LEGION-208's implementer has retired the run named servingGeneration, the issue stands at run
// generation in implementing, its row stands on entry rowEntry, and the claim holds the task of
// that run, that phase and that entry. Each case runs through the three writers that arm a
// backstop: the release's last step (outbox.armBackstops), the stop transaction of a pause
// (projectPause.stopped) and the cancel of a release while the project is paused
// (outbox.cancelRelease, store.ReleaseCancelPause.GetsBackstop).
func TestIdleOfAClaimThatOwesNothingStillGetsABackstopForTheUndeliveredFirstTaskOfItsRun(t *testing.T) {
	t.Parallel()
	for _, writer := range []struct {
		name string
		// arm runs the writer, and answers the pause whose stop it recorded, 0 for the release,
		// which records none.
		arm func(r *pauseRig, issue record.Issue, implementer supervise.Claim) int64
	}{
		{name: "the release", arm: func(r *pauseRig, issue record.Issue, implementer supervise.Claim) int64 {
			r.putClaim(implementer)
			runner := &outbox{pool: r.pool, records: r.pauses.records, trees: r.st, project: "legion", backstop: time.Hour,
				now: time.Now, log: quietLogger()}
			if err := runner.armBackstops(r.ctx, issue); err != nil {
				r.t.Fatalf("arm the backstops of %s's release: %v", pauseIssue, err)
			}
			return 0
		}},
		{name: "the stop transaction", arm: func(r *pauseRig, _ record.Issue, implementer supervise.Claim) int64 {
			held := r.pauseLegion()
			r.mustStop(implementer, supervise.PauseStop{Pause: held.ID})
			return held.ID
		}},
		{name: "a cancel of its release while paused", arm: func(r *pauseRig, issue record.Issue, implementer supervise.Claim) int64 {
			r.markReleased(implementer)
			if _, err := r.pool.Exec(r.ctx, `update issue_releases set generation = $2 where issue = $1`, pauseIssue,
				int64(issue.Generation)); err != nil {
				r.t.Fatalf("mark the release of %s at run %d: %v", pauseIssue, issue.Generation, err)
			}
			held := r.pauseLegion()
			r.cancelRelease()
			return held.ID
		}},
	} {
		for _, c := range []struct {
			name string
			// generation is the run LEGION-208 stands at, and servingGeneration the run the
			// claim's row records it retired (supervise.Claim.ServingGeneration), 0 for a claim
			// that has retired no run. The claim has confirmed no task of run generation.
			generation, servingGeneration uint64
			rowEntry                      int64
			completed                     bool
			backstop                      bool
			why                           string
			// noServedEntry leaves a kept claim having served no entry, level with its row's entry,
			// in place of the entry below its row's.
			noServedEntry bool
			// The task the claim holds in place of the first task of the run unconfirmed: none
			// (noTask), that task confirmed by a turn (confirmed), or, unconfirmed, a task of run
			// taskGeneration or of phase taskPhase.
			noTask, confirmed bool
			taskGeneration    uint64
			taskPhase         phase.Phase
		}{
			{name: "the sixth control: a claim that has retired no run holds the first task of run 1",
				generation: 1, servingGeneration: 0, rowEntry: 1, backstop: true,
				why: "nothing sends its first task again, so the backstop is the one wake that delivers it"},
			{name: "the sixth control: a claim kept across run 2 still serving run 1 holds the first task of run 2",
				generation: 2, servingGeneration: 1, rowEntry: 2, backstop: true,
				why: "nothing sends its first task again, so the backstop is the one wake that delivers it"},
			{name: "a claim that has retired no run holds the first task of run 1, and its row records its completion",
				generation: 1, servingGeneration: 0, rowEntry: 1, completed: true,
				why: "its completion of its row's entry stands, so it owes nothing a wake could bring back"},
			{name: "a claim kept across run 2 still serving run 1 holds the first task of run 2, and its row records its completion",
				generation: 2, servingGeneration: 1, rowEntry: 2, completed: true,
				why: "its completion of its row's entry stands, so it owes nothing a wake could bring back"},
			{name: "a claim kept across run 2 still serving run 1, which served no entry, holds the first task of run 2",
				generation: 2, servingGeneration: 1, rowEntry: 2, noServedEntry: true, backstop: true,
				why: "it is level with its row's entry and still serves an earlier run, and nothing sends its first task again"},
			{name: "a claim kept across run 2 still serving run 1, which served no entry, holds the first task of run 2, and its row records its completion",
				generation: 2, servingGeneration: 1, rowEntry: 2, noServedEntry: true, completed: true,
				why: "its completion of its row's entry stands, so it owes nothing a wake could bring back"},
			{name: "a claim kept across run 2 holds the first task of run 2 confirmed",
				generation: 2, servingGeneration: 1, rowEntry: 2, confirmed: true, backstop: true,
				why: "a turn confirmed the task, so the claim serves run 2 and entry 2, level with its row, and owes its phase"},
			{name: "a claim kept across run 2 still serving run 1 holds no task",
				generation: 2, servingGeneration: 1, rowEntry: 2, noTask: true,
				why: "it holds no task for a wake to deliver and owes nothing"},
			{name: "a claim kept across run 2 still serving run 1 holds a task of run 1 unconfirmed",
				generation: 2, servingGeneration: 1, rowEntry: 2, taskGeneration: 1,
				why: "the task it holds is of another run, so it is no task of the issue's run for a wake to deliver"},
			{name: "a claim kept across run 2 still serving run 1 holds a task of run 2's retro unconfirmed",
				generation: 2, servingGeneration: 1, rowEntry: 2, taskPhase: phase.Retro,
				why: "the task it holds is of another phase, so it is no task of the issue's phase for a wake to deliver"},
			{name: "a claim that has retired no run holds no task",
				generation: 1, servingGeneration: 0, rowEntry: 1, noTask: true,
				why: "it holds no task of the issue's run, so it serves no run and owes nothing"},
			{name: "a claim that has retired no run holds a task of run 1 unconfirmed, and the issue stands at run 2",
				generation: 2, servingGeneration: 0, rowEntry: 1, taskGeneration: 1,
				why: "the task it holds is of another run, so it serves no run and owes nothing"},
			{name: "a claim that has retired no run holds a task of run 1's retro unconfirmed",
				generation: 1, servingGeneration: 0, rowEntry: 1, taskPhase: phase.Retro,
				why: "the task it holds is of another phase, so it serves no run and owes nothing"},
		} {
			t.Run(writer.name+"/"+c.name, func(t *testing.T) {
				r := newPauseRig(t, claim.RoleImplementer)
				if _, err := r.pool.Exec(r.ctx, `update issues set generation = $2 where key = $1`, pauseIssue,
					int64(c.generation)); err != nil {
					t.Fatalf("move %s to run %d: %v", pauseIssue, c.generation, err)
				}
				issue := record.Issue{Key: pauseIssue, Tree: pauseIssue, Phase: phase.Implementing, Generation: c.generation}
				implementer := r.claim(claim.RoleImplementer, supervise.StateSuspended)
				implementer.Generation, implementer.ServingGeneration = c.generation, c.servingGeneration
				implementer.ServedEntry = 0
				if c.servingGeneration != 0 {
					implementer.ServedEntry = c.rowEntry - 1
				}
				if c.noServedEntry {
					implementer.ServedEntry = 0
				}
				task := undeliveredTask(phase.Implementing, c.rowEntry)
				task.Generation = c.generation
				implementer.GivenEntry, implementer.Pending = c.rowEntry, task
				if c.taskGeneration != 0 {
					task.Generation = c.taskGeneration
				}
				if c.taskPhase != "" {
					task.Phase = c.taskPhase
				}
				if c.confirmed {
					task.DeliveredAt = task.LastSendFailureAt.Add(time.Minute)
					task.ConfirmedAt = task.DeliveredAt
				}
				if c.noTask {
					implementer.Pending = nil
				}
				r.putPhaseRow(claim.RoleImplementer, c.rowEntry, c.completed)

				// A pause writer records the stop in every case, owing no launch, so a case that arms
				// none is a stop that read the backstop's rule and not one that never ran.
				if held := writer.arm(r, issue, implementer); held != 0 {
					if got := r.stops(held, claim.RoleImplementer); len(got) != 1 || got[0] != (pauseStopRecord{end: store.PauseStopStopped}) {
						t.Fatalf("the implementer's stops = %+v, want one stopped and owed no launch", got)
					}
				}

				armed := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer' and not fired`, pauseIssue)
				want := 0
				if c.backstop {
					want = 1
				}
				if armed != want {
					t.Errorf("%s of %s's implementer armed %d open backstops, want %d: %s", writer.name, pauseIssue, armed, want, c.why)
				}
			})
		}
	}
}
