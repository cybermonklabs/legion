package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// startPastReview writes one start of key's worker in the phase it stands in, through the path
// every phase worker's start is written by (workflow.StartOrOwe). It is oweMemberStart with a
// task and a role of the caller's, which is what tells the row a second start wrote from the
// row it was written over.
func startPastReview(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, role claim.Role, task string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, record.SuperviseRequest{Op: "start",
			Tree: key, Role: role, Generation: 1, Phase: stands, Task: task,
			Assignment: "assignment-" + key}, fixedNow); err != nil {
			t.Fatalf("write the start of %s: %v", key, err)
		}
	})
}

// leaseStart puts a live lease on the one start row issue holds, as the lifecycle lane does when
// it takes that row to deliver the task on it.
func leaseStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(), `update outbox set lease_token = 'lane', lease_until = $2
			where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`, issue, fixedNow.Add(time.Hour))
		if err != nil {
			t.Fatalf("take the lane's lease on the start of %s: %v", issue, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("the lane's lease took %d rows of %s, want the one start it holds", tag.RowsAffected(), issue)
		}
	})
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
//
// The subtests that close it read what such a start is written as. An issue holds one pending
// start however many a worker past its review is written — an operator's retry of a held retro
// goes over the queued row rather than beside it — because two rows would start that worker
// twice for one assignment and no fence of the outbox's would end the second. The control
// beside it is the row a lane already holds, which is left alone with the task it is
// delivering. The last reads where the rule stops: a start that waits for a row is written over
// no queued start that waits for none, since the place in the line belongs to the row that
// waits, and the two never name one phase, so the outbox's phase fence ends the stale one.
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

	// Merging is the last phase of the work past the review: its merger merges the head it read
	// and that merge closes the issue, so there is no further phase for a row to be asked for.

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

	// An operator's retry of a worker held past its review writes a second start while the first
	// is still queued and no lane holds it, and the two are one row: the retry is written over
	// the row the issue already holds, which waits for what it waits for — nothing
	// (record.Store.ReplaceOwedStart). Two rows would start the worker twice for one assignment,
	// and no fence of the outbox's would end the second — the phase and the generation fences
	// end a row that serves a phase the issue has left or an older generation, and both of these
	// serve the phase the issue stands in, at its own generation.
	t.Run("a retry past the review is written over the queued start", func(t *testing.T) {
		const retried = "LEGION-500"
		pastReviewRoot(t, pool, retried, phase.Retro, "in_progress", "F")
		startPastReview(t, pool, retried, phase.Retro, claim.RoleImplementer, "Run the retro.")
		if start := theOneStart(t, pool, retried); start.task != "Run the retro." || start.needsSlot || !start.claimable {
			t.Fatalf("the first start of %s = %+v, want the retro's task, marked for no row and claimable at once", retried, start)
		}

		startPastReview(t, pool, retried, phase.Retro, claim.RoleImplementer, "Run the retro again.")

		start := theOneStart(t, pool, retried)
		if start.role != claim.RoleImplementer || start.task != "Run the retro again." || start.needsSlot || !start.claimable {
			t.Fatalf("the start of %s after the retry = %+v, want the one row the retry wrote over the queued one, carrying the later task: two rows start the retro's worker twice and no fence ends the second",
				retried, start)
		}
	})

	// The control the rule has always had, which this must leave standing: a row a live lease
	// holds is left alone, because its lane is delivering the task under it, so the second start
	// is a row of its own, behind the lane's while that lease is live (the outbox's own lane
	// rule, store.go's outboxLaneFree).
	t.Run("a row a live lease holds is not written over", func(t *testing.T) {
		const leased = "LEGION-510"
		pastReviewRoot(t, pool, leased, phase.Merging, "in_progress", "G")
		startPastReview(t, pool, leased, phase.Merging, claim.RoleMerger, "Merge the change.")
		leaseStart(t, pool, leased)

		startPastReview(t, pool, leased, phase.Merging, claim.RoleMerger, "Merge the change again.")

		starts := startsOf(t, pool, leased)
		if len(starts) != 2 || starts[0].task != "Merge the change." || starts[1].role != claim.RoleMerger ||
			starts[1].task != "Merge the change again." || starts[1].needsSlot || starts[1].claimable {
			t.Fatalf("the starts of %s = %+v, want the lane's row carrying the task it is delivering and the second start as a row of its own behind it, marked for no row and held while that lease is live",
				leased, starts)
		}
	})

	// Where the rule stops: a start that waits for a row is written over no queued start of a
	// phase past the review, and none over it. The two never name one phase, so the outbox's
	// phase fence ends the one the issue has left; what writing over it would cost is the
	// waiting line, whose place belongs to the row that waits, and the issue would stand in it
	// from the moment a row it never waited on was queued, ahead of one that came to wait
	// before it (record.Store.ReplaceOwedStart).
	t.Run("a start that waits for a row is written over no queued one that waits for none", func(t *testing.T) {
		const ahead, behind = "LEGION-520", "LEGION-530"
		pastReviewRoot(t, pool, behind, phase.Retro, "in_progress", "H")
		startPastReview(t, pool, behind, phase.Retro, claim.RoleImplementer, "Run the retro.")
		pastReviewRoot(t, pool, ahead, phase.Implementing, "in_progress", "I")
		oweMemberStart(t, pool, ahead, ahead, phase.Implementing)

		// The architect sends the retro's work back to implementing, whose start waits for a row.
		sentBack := issue(t, pool, behind)
		sentBack.Phase, sentBack.Status, sentBack.DispatchStatus = phase.Implementing, "in_progress", "in_progress"
		putIssue(t, pool, sentBack)
		startPastReview(t, pool, behind, phase.Implementing, claim.RoleImplementer, "Write the change again.")

		starts := startsOf(t, pool, behind)
		if len(starts) != 2 || starts[0].task != "Run the retro." || starts[0].needsSlot ||
			starts[1].task != "Write the change again." || !starts[1].needsSlot || starts[1].claimable {
			t.Fatalf("the starts of %s = %+v, want the retro's row left where it was, which the phase fence ends, and its own row behind it, marked for a row and held while the cap has none",
				behind, starts)
		}
		assertWaiting(t, pool, []string{ahead, behind})
	})
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
