package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pastReviewRoot records key as a root of a live tree of its own, standing in stands with the
// Dispatch status that phase shows: its record, its mirror row and its tree lifecycle, open
// under the workflow. It writes no row of the cap's, which is what every reading below is about.
func pastReviewRoot(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, status, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, HandedOver: true, LastDispatchSeq: 1})
	seedMirror(t, pool, key, status)
	seedLifecycle(t, pool, key)
}

// runStart finishes the oldest supervise start row of issue, as that row's lifecycle lane does
// once it has delivered the task. A lane runs one row of an issue at a time, oldest first
// (record.Store's outboxInOrder), so a reading of what the next start may do runs the start
// before it first.
func runStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		var id int64
		if err := tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' = 'start' order by id limit 1`, issue).Scan(&id); err != nil {
			t.Fatalf("read the oldest start of %s: %v", issue, err)
		}
		if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
			t.Fatalf("finish the start %d of %s: %v", id, issue, err)
		}
	})
}

// theOneStart is the one supervise start row issue holds, which every reading of a start below
// is written over: a phase worker's start is one row that carries its task (workflow.StartOrOwe).
func theOneStart(t *testing.T, pool *pgxpool.Pool, issue string) midPhaseStart {
	t.Helper()
	starts := startsOf(t, pool, issue)
	if len(starts) != 1 {
		t.Fatalf("the starts of %s = %+v, want the one row the transition wrote", issue, starts)
	}
	return starts[0]
}

// The phases past an issue's review run on no row of the cap's at all. The cap counts the issues
// whose change is still to write, and the sign-off, the retro and the merge write no more of it
// (record.KeepsSlot), so the start of each of those phases waits for no row (workflow.StartOrOwe
// reads record.PhaseKeepsSlot) and is claimable the moment it is written.
//
// The cap is full throughout, which is what tells the two kinds of start apart: in the same
// reading, the start of an issue at work that holds no row stays unclaimable, and so does the
// start of the issue whose architect sends its work back from accepting to implementing. That
// one is delivered when a row frees and the pass grants it one, which is the mark reading the
// phase the start is for.
func TestAPhasePastReviewRunsWithNoSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	// The cap's two rows, held by two trees already at work.
	seedSlotted(t, pool, "LEGION-100", "A")
	seedSlotted(t, pool, "LEGION-110", "B")

	// The issue the architect accepts. Its design gate stands open, so the READY its merger
	// builds is held by nobody.
	const accepted = "LEGION-200"
	pastReviewRoot(t, pool, accepted, phase.Accepting, "needs_review", "C")
	seedGate(t, pool, record.DesignGate{Issue: accepted, ArtifactID: "spec-200", LatestVersion: 1, ApprovedVersion: new(1)})
	// An issue at work that holds no row, whose start waits for one throughout.
	const atWork = "LEGION-300"
	pastReviewRoot(t, pool, atWork, phase.Implementing, "in_progress", "D")
	oweMemberStart(t, pool, atWork, atWork, phase.Implementing)

	applyFact(t, pool, admission, "sign-off", intake.SignOff{Issue: accepted})

	if got := issue(t, pool, accepted); got.Phase != phase.Retro {
		t.Fatalf("the accepted issue = %#v, want it in retro: the sign-off moves it there", got)
	}
	if holdsSlot(t, pool, accepted) {
		t.Fatalf("rows = %#v, want %s holding none: its work is past its review", slots(t, pool), accepted)
	}
	if start := theOneStart(t, pool, accepted); start.role != claim.RoleImplementer || start.needsSlot || !start.claimable {
		t.Fatalf("the retro start of %s = %+v, want the implementer's, marked for no row and claimable at once", accepted, start)
	}
	if claimableStart(t, pool, atWork) {
		t.Fatalf("the starts of %s = %+v, want none claimable in the same reading: it is at work, it holds no row and the cap has none to give",
			atWork, startsOf(t, pool, atWork))
	}

	// The retro's own worker reports it finished, which starts the merger.
	runStart(t, pool, accepted)
	applyFact(t, pool, admission, "retro-complete", intake.HandoffComplete{Issue: accepted, Role: claim.RoleImplementer,
		Generation: 1, Claim: "claim-" + accepted, Commit: "retro-head"})

	if got := issue(t, pool, accepted); got.Phase != phase.Merging {
		t.Fatalf("the accepted issue = %#v, want it merging: its retro completion moves it there", got)
	}
	if holdsSlot(t, pool, accepted) {
		t.Fatalf("rows = %#v, want %s holding none in merging either", slots(t, pool), accepted)
	}
	if start := theOneStart(t, pool, accepted); start.role != claim.RoleMerger || start.needsSlot || !start.claimable {
		t.Fatalf("the merging start of %s = %+v, want the merger's, marked for no row and claimable at once", accepted, start)
	}

	// The merger's READY takes the issue to its wait for the merge, still on no row.
	runStart(t, pool, accepted)
	applyFact(t, pool, admission, "ready", intake.HandoffComplete{Issue: accepted, Role: claim.RoleMerger, Generation: 1,
		Claim: "claim-" + accepted, Ready: true, Summary: "READY #42 at head for " + accepted})

	if got := issue(t, pool, accepted); got.Phase != phase.AwaitingMerge {
		t.Fatalf("the accepted issue = %#v, want it awaiting its merge: its READY takes it there", got)
	}
	if holdsSlot(t, pool, accepted) {
		t.Fatalf("rows = %#v, want %s holding none while it waits for its merge", slots(t, pool), accepted)
	}

	// An issue whose architect sends the work back from accepting to implementing writes the
	// change again, so its start waits for a row as any phase worker's start does.
	const sentBack = "LEGION-400"
	pastReviewRoot(t, pool, sentBack, phase.Accepting, "needs_review", "E")

	applyFact(t, pool, admission, "sent-back", intake.BackwardMove{Issue: sentBack, Requester: claim.RoleArchitect,
		To: phase.Implementing, Reason: "the change needs work the review found"})

	if got := issue(t, pool, sentBack); got.Phase != phase.Implementing {
		t.Fatalf("the issue sent back = %#v, want it implementing", got)
	}
	if start := theOneStart(t, pool, sentBack); !start.needsSlot || start.claimable {
		t.Fatalf("the start of %s = %+v, want it marked for a row and held while the cap has none: the work is being written again",
			sentBack, start)
	}
	assertWaiting(t, pool, []string{atWork, sentBack})

	// Each row the cap frees goes to the head of the line, and the start it was holding runs.
	closeTree(t, pool, admission, "first-closed", "LEGION-100", "A")

	if !claimableStart(t, pool, atWork) {
		t.Fatalf("the starts of %s = %+v, want its implementer's task claimable on the row it took", atWork, startsOf(t, pool, atWork))
	}
	if claimableStart(t, pool, sentBack) {
		t.Fatalf("the starts of %s = %+v, want none claimable: the one row that freed went to the issue ahead of it in the line",
			sentBack, startsOf(t, pool, sentBack))
	}

	closeTree(t, pool, admission, "second-closed", "LEGION-110", "B")

	if !holdsSlot(t, pool, sentBack) || !claimableStart(t, pool, sentBack) {
		t.Fatalf("the starts of %s = %+v on rows %#v, want its implementer's task claimable on the row the line gave it",
			sentBack, startsOf(t, pool, sentBack), slots(t, pool))
	}
}

// The repair of a worker that has lost its task reaches an issue standing past its review, which
// holds no row to be found by (admit's resumeSlottedWorkers). The retro's implementer and the
// merger are started again, exactly as a row holder's worker is, and nothing else would ever
// start them: the start their transition wrote has been finished without acting.
//
// What the repair does not reach is an issue at work that holds no row, because it waits in the
// line for one and starting it would put a worker to work on an issue the cap has not admitted.
func TestTheRepairReachesAWorkerPastReview(t *testing.T) {
	pool := migratedPool(t)
	rows := []struct {
		name string
		// root and key are the tree's root and the member whose worker lost its task.
		root, key string
		stands    phase.Phase
		role      claim.Role
		// started is whether the pass starts that worker again.
		started bool
		why     string
	}{
		{name: "retro", root: "LEGION-710", key: "LEGION-711", stands: phase.Retro, role: claim.RoleImplementer, started: true,
			why: "the retro has a worker with work and holds no row, so the repair is the only thing that would ever start it again"},
		{name: "merging", root: "LEGION-720", key: "LEGION-721", stands: phase.Merging, role: claim.RoleMerger, started: true,
			why: "the merger has work while no closed design gate holds its READY (record.AtWork), and it holds no row either"},
		{name: "at work with no row", root: "LEGION-730", key: "LEGION-731", stands: phase.Implementing, role: claim.RoleImplementer,
			why: "an issue at work that holds no row waits in the line for one, and a repair that started its worker would put it to work on a row the cap never gave"},
	}
	for _, row := range rows {
		seedTreeAndMember(t, pool, row.root, row.key, row.stands, 1, false)
		// A worker the daemon suspended holds no task it still works, which is the state this
		// repair exists for.
		putWorkerClaim(t, pool, row.root, row.key, row.role, 1, supervise.StateSuspended, supervise.SuspendedByDaemon)
		if starts := startsOf(t, pool, row.key); len(starts) != 0 {
			t.Fatalf("the starts of %s before the pass = %+v, want none: the row's own premise", row.key, starts)
		}
	}

	tick(t, pool, quiet(t, len(rows)), "tick")

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if holdsSlot(t, pool, row.key) {
				t.Fatalf("rows = %#v, want %s holding none: no pass grants one here, and the repair reads none", slots(t, pool), row.key)
			}
			starts := startsOf(t, pool, row.key)
			if !row.started {
				if len(starts) != 0 {
					t.Fatalf("the starts of %s after the pass = %+v, want none: %s", row.key, starts, row.why)
				}
				return
			}
			if len(starts) != 1 || starts[0].role != row.role || starts[0].task == "" || starts[0].needsSlot || !starts[0].claimable {
				t.Fatalf("the starts of %s after the pass = %+v, want the %s one, carrying its task, marked for no row and claimable at once: %s",
					row.key, starts, row.role, row.why)
			}
		})
	}
}
