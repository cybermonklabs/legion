package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// A backstop of an issue that waits for its admission slot fires and wakes no one. The issue owes
// its worker a start of its current phase and generation that the outbox holds until the slot
// comes (record.SuperviseRequest.NeedsSlot), and the idle rule releases its pod meanwhile; a wake
// would put that worker to work while the admission cap says the issue may not run, and would
// repeat at every backstop of the wait. Once a slot of the issue stands, the start is due and the
// backstop wakes the worker as before. A start of an earlier phase or generation silences nothing:
// admission finishes it without acting (finishFencedStarts), so it brings no worker back, and a
// backstop it silenced would leave the released worker with nothing to wake it.
func TestABackstopWakesNoWorkerOfAnIssueThatWaitsForItsSlot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		phase      string
		generation int
		slot       bool
		wantWake   bool
	}{
		{name: "no slot of the issue stands", phase: "implementing", generation: wakeIssueGeneration, wantWake: false},
		{name: "the issue holds its slot again", phase: "implementing", generation: wakeIssueGeneration, slot: true, wantWake: true},
		{name: "the waiting start is of an earlier generation", phase: "implementing", generation: wakeIssueGeneration - 1, wantWake: true},
		{name: "the waiting start is of an earlier phase", phase: "testing", generation: wakeIssueGeneration, wantWake: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWakeRig(t)
			ctx := context.Background()
			if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ('supervise', 'LEGION-208', $1, 0, now(), '')`,
				fmt.Sprintf(`{"op":"start","tree":"LEGION-208","role":"implementer","generation":%d,"phase":%q,"task":"Continue.","needsSlot":true}`,
					tc.generation, tc.phase)); err != nil {
				t.Fatalf("queue the start that waits for the slot: %v", err)
			}
			if tc.slot {
				if _, err := rig.pool.Exec(ctx, `insert into slots (issue, index, admitted_at) values ('LEGION-208', 0, now())`); err != nil {
					t.Fatalf("give the issue its slot: %v", err)
				}
			}
			id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
			rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})

			var fired bool
			if err := rig.pool.QueryRow(ctx, `select fired from pod_wake_backstops where id = $1`, id).Scan(&fired); err != nil {
				t.Fatalf("read the backstop: %v", err)
			}
			if !fired {
				t.Fatal("the backstop is not marked fired")
			}
			resumes := wakeResumes(t, rig.pool)
			switch {
			case tc.wantWake && (len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleImplementer), pods.WakeBackstop}):
				t.Fatalf("pod resumes = %v, want the one backstop wake of the implementer", resumes)
			case !tc.wantWake && len(resumes) != 0:
				t.Fatalf("pod resumes = %v, want none: the issue waits for its admission slot, whose start brings the implementer back", resumes)
			}
			wantStarts, what := 1, "the start that waits for the slot alone"
			if tc.wantWake {
				wantStarts, what = 2, "the start that waits for the slot and the backstop's wake"
			}
			if starts := wakeStarts(t, rig.pool); len(starts) != wantStarts {
				t.Fatalf("supervise start rows = %+v, want %s", starts, what)
			}
		})
	}
}
