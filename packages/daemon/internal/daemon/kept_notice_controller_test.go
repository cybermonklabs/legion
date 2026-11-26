package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pod no role runs in, kept for its disk, is told to someone (the Legion reviewer's point 1 on
// R6 of #5285). The daemon counts the pod told when it queues the issue's
// architect's workspace-kept notice (store's recordDecline), so the notice must not finish with
// nobody told: where the owning architect will not hold its role again (its claim failed, as the
// root of a production issue's round did on a provider's 429), the notice goes to the project's controller
// instead, once, however often its row runs, and the controller's row publishes it on the
// controller topic with the notice's own words. The same holds where the notice's row runs after
// the architect stopped with its tree, which lingers now (stoppedWithTree, the Legion reviewer's
// point 3 on R6's review fixes): a row that waited on its backoff while the tree finished ends
// there before any publish, and its notice goes to the controller as well. Control: an architect
// that holds its role gets the notice, and nothing goes to the controller.
func TestAKeptPodsNoticeGoesToTheControllerWhenNoArchitectWillHearIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		state      supervise.ClaimState
		lingers    bool
		absent     bool
		controller int
	}{
		{"its_architect_failed", supervise.StateFailed, false, true, 1},
		{"its_tree_lingers_and_its_architect_stopped_with_it", supervise.StateSuspended, true, true, 1},
		{"control:_its_architect_holds_its_role", supervise.StateWorking, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.state)
			publisher := &holderPublisher{}
			if tc.absent {
				publisher.setAbsent(architectTopic(t, "LEGION-1"))
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
				supervisor: sup, project: "legion", now: time.Now}
			kept := record.Notice{Kind: record.WorkspaceKeptNotice, Role: claim.RoleArchitect,
				Reason: "The daemon did not delete the pod of LEGION-1 (pod pod-a)."}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", kept, time.Now()))
			// The row runs twice, as a run whose finish was lost runs again.
			for range 2 {
				if err := runner.notice(ctx, row, kept); err != nil {
					t.Fatalf("run the workspace-kept notice: %v", err)
				}
			}
			var queued int
			if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'controller_notice'
				and payload->>'kind' = 'workspace-kept'`).Scan(&queued); err != nil {
				t.Fatalf("count the controller's rows: %v", err)
			}
			if queued != tc.controller {
				t.Fatalf("workspace-kept rows for the controller = %d, want %d", queued, tc.controller)
			}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("drain the outbox: %v", err)
			}
			publisher.mu.Lock()
			defer publisher.mu.Unlock()
			toController, toArchitect := 0, 0
			for _, published := range publisher.delivered {
				notice, ok := published.payload.(record.Notice)
				if !ok || notice.Kind != record.WorkspaceKeptNotice || notice.Reason != kept.Reason {
					continue
				}
				switch published.topic {
				case notify.ControllerTopic("legion"):
					toController++
				case architectTopic(t, "LEGION-1"):
					toArchitect++
				}
			}
			wantArchitect := 0
			if !tc.absent {
				wantArchitect = 2
			}
			if toController != tc.controller || toArchitect != wantArchitect {
				t.Fatalf("the notice reached the controller %d times and the architect %d times, want %d and %d",
					toController, toArchitect, tc.controller, wantArchitect)
			}
		})
	}
}
