package replay

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pausedWorkWindow is a window from 09:00 to 13:00 recorded under main, of one root whose work a
// project's pause stops. ACME-1, a root, is promoted at 09:05, plans from 09:20 and implements
// from 09:50. At 10:00 the operator pauses the project, which stops ACME-1's three live claims,
// its architect, its planner and its implementer, whose turn ends there. At 10:40 the operator
// resumes the project; the implementer is launched again and works to the move to testing at
// 11:10, and ACME-1 works 15 minutes in each phase after it to its merge at 12:25.
func pausedWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.turnEnds(clock(10, 0), "ACME-1", claim.RoleImplementer)
	stopped := []string{string(b.token("ACME-1", claim.RoleArchitect)), string(b.token("ACME-1", claim.RoleImplementer)),
		string(b.token("ACME-1", claim.RolePlanner))}
	b.info(clock(10, 0), "pause: the project is paused", "pause", 1, "by", "operator", "by_session", "",
		"reason", "the operator moves the database to a larger disk", "stops", len(stopped), "claims", stopped, "told_controller", true)
	b.info(clock(10, 40), "pause: the project is resumed", "pause", 1, "by", "operator", "by_session", "",
		"paused_by", "operator", "paused_by_session", "", "held", 0, "told_controller", true)
	b.launched(clock(10, 40), "ACME-1", claim.RoleImplementer, true)
	b.turnStarts(clock(10, 40), "ACME-1", claim.RoleImplementer)
	from := phase.Implementing
	for _, move := range []builtMove{
		{clock(11, 10), phase.Testing, workflow.TriggerImplementationReady},
		{clock(11, 25), phase.Reviewing, workflow.TriggerTesterPassed},
		{clock(11, 40), phase.Accepting, workflow.TriggerReviewApproved},
		{clock(11, 55), phase.Retro, workflow.TriggerSignOff},
		{clock(12, 10), phase.Merging, workflow.TriggerRetroCompleted},
		{clock(12, 25), phase.Done, workflow.TriggerPullRequestMerged},
	} {
		b.segmentEnds(move.at, "ACME-1", from, move.to, move.trigger)
		from = move.to
	}
	b.leaves(clock(12, 25), "ACME-1", record.DoneStatus)
	return b.build()
}

// pauseSettle is what a replay's database holds of a project's pause at one instant the snapshot
// timeline settles: the stops the pause asked (project_pause_stops) and the pause_stop outbox
// rows still waiting to run.
type pauseSettle struct {
	at             time.Time
	asked, waiting int
}

// A replay pauses the project where its record shows the pause (driver.pause, through the store's
// own pause), and the stop row the pause queues for each live claim comes due at that moment, as
// the daemon's comes due at its own now, so the replay's stub runs it there, finishing it under
// assumption 28 (the work a pause stops lies inside the recorded segments and the record's own
// pause). The test replays pausedWorkWindow under main and reads, at each instant the snapshot
// timeline settles (RunConfig.atSettled), the stops the pause asked and the stop rows the outbox
// still holds. It fails where the pause of 10:00 asked no stop, and where any instant finds a stop
// row waiting: a row due at the moment the replay ran, after the window, waits to the resume,
// which deletes it unrun.
func TestAPausesStopRowsRunAtItsRecordedMoment(t *testing.T) {
	t.Parallel()
	var settles []pauseSettle
	_, err := Run(context.Background(), RunConfig{Window: pausedWorkWindow(t), Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir(), atSettled: func(ctx context.Context, st *store.Store, at time.Time) error {
			settle := pauseSettle{at: at}
			if err := st.Pool().QueryRow(ctx, `select (select count(*) from project_pause_stops),
				(select count(*) from outbox where kind = $1 and payload->>'op' = 'pause_stop')`, string(record.OutboxKindSupervise)).
				Scan(&settle.asked, &settle.waiting); err != nil {
				return err
			}
			settles = append(settles, settle)
			return nil
		}})
	if err != nil {
		t.Fatalf("replay the window under main: %v", err)
	}
	paused := false
	for _, settle := range settles {
		if settle.at.Equal(clock(10, 0)) {
			paused = true
			if settle.asked == 0 {
				t.Error("the pause of 10:00 asked no stop, want one of each of ACME-1's live claims")
			}
		}
		if settle.waiting > 0 {
			t.Errorf("at %s the outbox holds %d pause_stop rows waiting, want each run at the pause's own moment, 10:00", stamp(settle.at),
				settle.waiting)
		}
	}
	if !paused {
		t.Errorf("no instant settled at 10:00, the pause's moment, among the %d that settled", len(settles))
	}
}
