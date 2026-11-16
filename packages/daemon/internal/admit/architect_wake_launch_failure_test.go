package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The second arm names a failure no further send can cure (dispatch://INFRA-2614, R7), and an
// architect whose every launch fails is that failure. On a running daemon the start that
// launches it is the one row that stays queued for it: the start's launches fail, the claim goes
// to failed, the start row fails with it, and the outbox runs that row again on its backoff,
// which relaunches the claim and fails it again, for as long as the launches fail. Measured by
// INFRA-2614's tester on the whole daemon at cca3313b237a, with every spawn of one root's
// architect failing: the claim stood failed for three minutes, its start row ran eight times
// (attempts 4, 5, 6, 7, 8, next run 64 s apart), the state named no arm for the root, and no
// controller wake was queued, before or after a controller registered.
//
// A start of the run that is queued and has not run yet is the daemon's own next move, which the
// arm waits for (TestTheUnreachableClaimArchitectWakeIsRaisedOnlyWhereNoQueuedStartRelaunchesTheClaim).
// A start that has run against the claim and left it failed is no next move: running it again is
// what has already failed. So the arm holds for a failed claim whose only queued start is the
// one that last ran against it and failed, and the controller is told once.
func TestTheUnreachableClaimArchitectWakeIsRaisedForAClaimItsOwnStartKeepsFailingToLaunch(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	registerController(t, pool)
	admission := quiet(t, 1)
	const root = "LEGION-826"
	fixture := architectWakeRoot(root)
	fixture.state, fixture.task = supervise.StateFailed, false
	seedArchitectWakeRoot(t, pool, fixture)
	token := architectToken(t, root)

	// The start of the root's run, as the outbox holds it after its launches failed: queued, run
	// four times, and recorded on the claim as the last start that ran against it.
	var startRow int64
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(root, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
			Generation: architectWakeGeneration, Task: seededArchitectTask(root), Entry: seededEntry}, fixedNow)
		if err != nil {
			t.Fatalf("build the architect start of %s: %v", root, err)
		}
		if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
			t.Fatalf("queue the architect start of %s: %v", root, err)
		}
		if err := tx.QueryRow(ctx, `select max(id) from outbox where issue = $1 and kind = 'supervise'`, root).Scan(&startRow); err != nil {
			t.Fatalf("read the start row of %s: %v", root, err)
		}
		if _, err := tx.Exec(ctx, `update outbox set attempts = 4 where id = $1`, startRow); err != nil {
			t.Fatalf("record the failed runs of start row %d: %v", startRow, err)
		}
		if _, err := tx.Exec(ctx, `update claims set last_start_row = $1 where token = $2`, startRow, string(token)); err != nil {
			t.Fatalf("record start row %d as the last start that ran against %s: %v", startRow, token, err)
		}
	})

	want := []record.ArchitectStallArm{record.ArchitectClaimUnreachable}
	if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
		t.Errorf("the state's reading of %s names the arms %v, want %v: the claim is failed and the one start queued for it "+
			"is the start that already ran against it and failed", root, arms, want)
	}
	for _, pass := range []string{"launch-failing-1", "launch-failing-2"} {
		applySweep(t, pool, admission, pass)
	}
	assertArchitectWakes(t, pool, root, want,
		"an architect whose own start keeps failing to launch it is a failure no further send cures, so the controller is told once")
}
