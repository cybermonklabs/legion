package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A run the release before this one started has no row in step_starts, so the record names no
// agent of a run on its issue, and a fact its agent's request applies waits for nothing
// (api's apply). The boot gives the run an open row (workflow's OpenUncountedRuns), so the
// planner's completion waits for its stream (the daemon says the wait began,
// overrides.streamWaitBegan), and the record of planning ends naming the model that answered
// last, under the id of the count 0, with the planner and the moment its entry was raised. The
// control takes the row out after the boot, as a boot without that pass leaves the store: the
// completion applies at once, with no wait for the planner's stream, and since the boot's row is
// the one path a run no start counted is ended by, it ends no run of planning, and Dispatch holds
// no record of it once every step record the same fact queued, the next step's start among them,
// has left the outbox.
func TestABootOpensTheRunTheReleaseBeforeStartedSoItsAgentsCompletionWaitsForItsStream(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// noRow takes the run's row out after the boot.
		noRow bool
	}{
		{name: "the boot opens the run"},
		{name: "control: no row after the boot, as a boot without the pass leaves it", noRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStepRecordsRig(t, 1)
			planner := r.plannerTheReleaseBeforeStarted()
			if tc.noRow {
				r.exec(`delete from step_starts where issue = $1`, r.key)
			}
			planner.model = "stand-in/planner-first"
			planner.answer()
			planner.send(shimwire.AgentStart{})
			planner.waitState(supervise.StateWorking)
			req := api.HandoffCompleteRequest{Summary: "planned"}
			req.HandoffID, req.HandoffVersion = r.writeHandoff(planner.session, "plan")
			req.GrantID = planner.grant()
			if status, body := r.heldRequest(planner, "stand-in/planner-second", "/legion/v1/handoff/complete", req); status != http.StatusOK {
				t.Fatalf("the planner's completion = %d %s, want 200", status, body)
			}
			if r.waited == tc.noRow {
				t.Fatalf("the planner's completion waited for its stream: %t; want %t, as the run's row in step_starts says (none %t)", r.waited, !tc.noRow, tc.noRow)
			}
			planner.endTurn()
			r.waitPhase(phase.Implementing)
			if tc.noRow {
				// The outbox deletes a row only after Dispatch took its write, so Dispatch can show
				// implementing's start while the outbox still holds that start's row. The wait is for
				// the outbox to hold no step record of the issue: then every record the fact queued
				// has reached Dispatch, a record of planning's run too if the fact queued one. The
				// read of planning comes after that wait, since a read before it can miss a record
				// that reaches Dispatch between the read and the count.
				r.waitSteps("every step record of the issue, implementing's start among them", func(got stepRecordsRead) error {
					if found := got.find("step", 2, 1); found.ID == "" {
						return errors.New("implementing has no record yet")
					}
					if queued := r.unfinishedRows(record.OutboxKindStepRecord); queued != 0 {
						return fmt.Errorf("the outbox still holds %d step records of the issue", queued)
					}
					return nil
				})
				got, err := r.readSteps()
				if err != nil {
					t.Fatalf("read the steps: %v", err)
				}
				if found := got.find("step", 1, 1); found.ID != "" {
					t.Fatalf("planning reads %+v, want no record of it: no row holds its run open", found)
				}
				return
			}
			raised := r.phaseOf(claim.RolePlanner).EntryRaisedAt
			id := workflow.StepID(r.key, 1, record.StepPlace{Task: "default", Kind: "step", Place: 1}, 0)
			r.waitSteps("the end of planning", func(got stepRecordsRead) error {
				switch found := got.find("step", 1, 1); {
				case found.ID != id || found.EndedAt == nil || found.Result != "passed":
					return fmt.Errorf("planning reads %+v, want the record %s ended passed", found, id)
				case found.Agent != string(planner.token) || found.StartedAt == nil || !found.StartedAt.Equal(raised):
					return fmt.Errorf("planning names agent %q started at %v; want %s and the moment its entry was raised, %s", found.Agent, found.StartedAt, planner.token, raised)
				case found.Session != planner.session || found.Model != "stand-in/planner-second":
					return fmt.Errorf("planning names session %q and model %q; want %q and stand-in/planner-second", found.Session, found.Model, planner.session)
				}
				return nil
			})
		})
	}
}

// The boot opens a run no start counted once: a boot that runs again finds the run's row open and
// counts nothing again, and neither boot queues a record of the run.
func TestABootThatRunsAgainCountsNothingAgain(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	planner := r.plannerTheReleaseBeforeStarted()
	opened := r.issueStartRows()
	var starts int64
	var agent string
	var run record.StepRun
	var at *time.Time
	if err := r.st.Pool().QueryRow(context.Background(), `select starts, agent, run, started_at from step_starts where issue = $1 and kind = 'step' and place = 1`, r.key).
		Scan(&starts, &agent, &run, &at); err != nil {
		t.Fatalf("read the row of planning after the boot: %v (rows %q)", err, opened)
	}
	if raised := r.phaseOf(claim.RolePlanner).EntryRaisedAt; len(opened) != 1 || starts != 0 || agent != string(planner.token) || run != record.RunOpen ||
		at == nil || !at.Equal(raised) {
		t.Fatalf("step_starts after the boot = %q; want planning's run alone, open at the count 0, the planner's, started when its entry was raised at %s", opened, raised)
	}
	if queued := r.unfinishedRows(record.OutboxKindStepRecord); queued != 0 {
		t.Fatalf("the boot queued %d step records, want none", queued)
	}
	r.restart()
	if again := r.issueStartRows(); len(again) != 1 || again[0] != opened[0] {
		t.Fatalf("step_starts after a second boot = %q, want it as the first boot left it, %q", again, opened)
	}
	if queued := r.unfinishedRows(record.OutboxKindStepRecord); queued != 0 {
		t.Fatalf("the second boot queued %d step records, want none", queued)
	}
}

// plannerTheReleaseBeforeStarted walks the issue into planning, its planner at work, and leaves
// the store as the release before this one left it: the daemon stopped, no row of step_starts for
// the issue, and no record of its steps in the outbox or in Dispatch. It boots the daemon again on
// that store, and answers the planner, whose pane outlived the daemon.
func (r *stepRecordsRig) plannerTheReleaseBeforeStarted() *stepClaimant {
	r.t.Helper()
	planner := r.worker(claim.RolePlanner)
	planner.take()
	var started string
	r.waitSteps("the start of planning", func(got stepRecordsRead) error {
		if started = got.find("step", 1, 1).ID; started == "" {
			return errors.New("no record of planning")
		}
		return nil
	})
	r.d.stop()
	r.exec(`delete from step_starts where issue = $1`, r.key)
	r.exec(`delete from outbox where issue = $1 and kind = $2`, r.key, string(record.OutboxKindStepRecord))
	r.deleteNode(started)
	r.reboot()
	return planner
}

// issueStartRows is every row of step_starts of the walk's issue, whole, in its key's order: the
// rows of another test's issue can stand in the same store.
func (r *stepRecordsRig) issueStartRows() []string {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select row_to_json(s)::text from step_starts s where issue = $1 order by task, kind, place`, r.key)
	if err != nil {
		r.t.Fatalf("read the rows of step_starts of %s: %v", r.key, err)
	}
	starts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the rows of step_starts of %s: %v", r.key, err)
	}
	return starts
}
