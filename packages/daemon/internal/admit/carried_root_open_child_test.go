package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The carried root of these cases, a root a hand-over of the release before carried in, in
// implementing (record.CarriedIn), and a Dispatch child under it that Legion does not run: it
// stands in backlog, so Dispatch shows it open under the root (dispatch://INFRA-3320 R9).
const (
	crRoot      = "LEGION-400"
	crOpenChild = "LEGION-402"
)

// stopsOf counts the rows that would stop or wake a worker of issue in the middle of its phase:
// a `retire_task` of its claim's task and a phase-ended message to its role, which the admission
// pass's own stop of a started parent queues (workflow's StopForChildren).
func stopsOf(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var n int
	inTx(t, pool, func(tx pgx.Tx) {
		if err := tx.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and
			((kind = 'supervise' and payload->>'op' = 'retire_task') or kind = 'phase_ended')`, issue).Scan(&n); err != nil {
			t.Fatalf("count the stops of %s: %v", issue, err)
		}
	})
	return n
}

// noWorkerLaunch fails if a start of key's phase role, of work or of news alone, stands in the
// outbox (startsOf reads every one), or a row that stops or wakes such a worker (stopsOf).
func noWorkerLaunch(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) {
	t.Helper()
	for _, start := range startsOf(t, pool, key) {
		if start.role == role {
			t.Fatalf("the waiting %s has a start of its %s queued (%+v), want none: no worker is launched while its child is open", key, role, start)
		}
	}
	if got := stopsOf(t, pool, key); got != 0 {
		t.Fatalf("the waiting %s has %d rows that stop or wake its worker, want none: no worker of it took the phase", key, got)
	}
}

// deliverStart finishes the newest start row of role on issue, as the outbox lane does once it
// has launched that worker and handed it the task, so the worker stands in the middle of its
// phase.
func deliverStart(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		var id int64
		if err := tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
			and payload->>'role' = $2 order by id desc limit 1`, issue, string(role)).Scan(&id); err != nil {
			t.Fatalf("read the %s's start of %s: %v", role, issue, err)
		}
		if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
			t.Fatalf("finish the %s's start %d of %s: %v", role, id, issue, err)
		}
	})
}

// dispatch://INFRA-3320 R9 at the promotion of a root a hand-over of the release before carried
// in, carrying its phase (INFRA-3320 item 45). The start of the phase it carries is a phase start
// like any other (workflow.Engine.StartCarriedRun, carriedStartHeld):
//
//  1. with an open Dispatch child under it, the root waits in awaiting_children at its promotion
//     holding the start of the phase it carries, with no launch of its worker queued and nothing
//     that stops or wakes one, since no worker of it took the phase; the pass after keeps it
//     waiting and queues none either;
//  2. the control row has no open Dispatch child: the root's carried phase starts at its
//     promotion, its worker's launch queued, and once the outbox has delivered that start the
//     pass after it moves the root nowhere and tells its worker nothing.
//
// Each runs with the root's design gate opened before the promotion and with none open at it:
// the open children are read whatever the gate stands at.
func TestAPromotedCarriedRootWaitsOnItsOpenChildWhateverItsDesignGate(t *testing.T) {
	for _, gate := range []struct {
		name string
		open bool
	}{
		{name: "the design gate opens first", open: true},
		{name: "the promotion finds no gate open", open: false},
	} {
		for _, row := range []struct {
			name  string
			child bool
		}{
			{name: "an open child holds the root", child: true},
			{name: "the control: no open child", child: false},
		} {
			t.Run(gate.name+"/"+row.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := pwAdmission(t, 2)
				carriedRoot(t, pool, crRoot, phase.Implementing, "A")
				if gate.open {
					seedGate(t, pool, record.DesignGate{Issue: crRoot, ArtifactID: "artifact-400", LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true})
				}
				if row.child {
					seedMirrorChild(t, pool, crOpenChild, "backlog", crRoot)
				}

				spPass(t, pool, admission)

				root := issue(t, pool, crRoot)
				if !issueOpen(t, pool, crRoot) {
					t.Fatalf("the carried root %s holds no open lifecycle after the pass: its promotion starts it", crRoot)
				}
				if ended := pwPayloads[record.PhaseEnded](t, pool, crRoot); len(ended) != 0 {
					t.Fatalf("phase-ended messages to the root at its promotion = %+v, want none", ended)
				}
				if !row.child {
					if root.Phase != phase.Implementing || root.ChildrenWait != nil {
						t.Fatalf("the promoted root = phase %s, held start %+v; want implementing, nothing held", root.Phase, root.ChildrenWait)
					}
					if starts := pwWorkerStarts(t, pool, crRoot); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task == "" {
						t.Fatalf("the root's worker starts = %+v, want the implementer's start of the phase it carries", starts)
					}

					// The outbox delivers the root's start, and the pass after that, with no open
					// child, moves the root nowhere.
					deliverStart(t, pool, crRoot, claim.RoleImplementer)
					spPass(t, pool, admission)

					root = issue(t, pool, crRoot)
					if ended := pwPayloads[record.PhaseEnded](t, pool, crRoot); root.Phase != phase.Implementing || len(ended) != 0 {
						t.Fatalf("the root after the next pass = phase %s with phase-ended messages %+v, want implementing with none", root.Phase, ended)
					}
					return
				}
				if root.Phase != phase.AwaitingChildren || root.ChildrenWait == nil || root.ChildrenWait.Start != phase.Implementing ||
					!slices.Equal(root.ChildrenWait.Children, []string{crOpenChild}) {
					t.Fatalf("the promoted root with an open child = phase %s, held start %+v; want awaiting_children holding the start of implementing for %s",
						root.Phase, root.ChildrenWait, crOpenChild)
				}
				noWorkerLaunch(t, pool, crRoot, claim.RoleImplementer)

				spPass(t, pool, admission)

				if root := issue(t, pool, crRoot); root.Phase != phase.AwaitingChildren {
					t.Fatalf("the root after the next pass = phase %s, want still awaiting_children", root.Phase)
				}
				noWorkerLaunch(t, pool, crRoot, claim.RoleImplementer)
			})
		}
	}
}
