package workflow

// The merger owes its ask only while the issue stands in merging (owedTo). So a row of the ask that
// ends while the merger's move back has the issue outside merging sends nothing, and neither does
// a ready of the merger's launch there. At the move back into merging the merger owes the ask
// again, and its launch, as the claim store holds it (claims.generation), is told the ask once: a
// launch a row of the ask reached is told nothing more, a row still queued is left to deliver it,
// and a claim that cannot be told - failed, retired or not held - is asked at the ready of its
// next launch (askAgainAtLaunch).

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// mergerClaimAt records LEGION-208's merger claim at launch (claims.generation) in state, as the
// claim store holds it, under the token the daemon addresses the merger by (addressRoles): the
// issue's project token (wakeProjectToken), then the issue and the role (wakeToken). A suspended
// claim is a released one (supervise.SuspendedByReleased), which a message wakes.
func mergerClaimAt(t *testing.T, pool *pgxpool.Pool, launch uint64, state supervise.ClaimState) {
	t.Helper()
	var by supervise.SuspendSource
	if state == supervise.StateSuspended {
		by = supervise.SuspendedByReleased
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, 'LEGION-208', 'LEGION-208', 'merger', $3, 'ses', '/sessions/ses.jsonl', $4, 0, 0, 0, 0, $5)`,
		string(wakeToken(t, claim.RoleMerger)), wakeProjectToken(t), int64(launch), string(state), string(by)); err != nil {
		t.Fatalf("seed the merger claim of LEGION-208: %v", err)
	}
}

// mergerRelaunched moves LEGION-208's merger claim to launch, a later one, in state, as a
// relaunch's claim write does: the store holds the later launch, and the claim is suspended by
// nobody.
func mergerRelaunched(t *testing.T, pool *pgxpool.Pool, launch uint64, state supervise.ClaimState) {
	t.Helper()
	moved, err := pool.Exec(t.Context(), `update claims set generation = $1, state = $2, suspended_by = ''
		where issue = 'LEGION-208' and role = 'merger'`, int64(launch), string(state))
	if err != nil {
		t.Fatalf("relaunch the merger claim of LEGION-208: %v", err)
	}
	if moved.RowsAffected() != 1 {
		t.Fatalf("the relaunch moved %d merger claims of LEGION-208, want one", moved.RowsAffected())
	}
}

// mergerReady is the ready of the merger's claim at launch, which the daemon applies once for each
// launch of the claim (intake.ClaimReady).
func mergerReady(launch uint64) intake.ClaimReady {
	return intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleMerger, Launch: launch}
}

// mergeForwardQueued fails the test where the merge-forward messages the outbox holds when are
// not want.
func mergeForwardQueued(t *testing.T, pool *pgxpool.Pool, when string, want []record.MergeForwardMessage) {
	t.Helper()
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s the merge-forward messages are %+v, want %+v", when, got, want)
	}
}

// mergerTold fails the test where the record's ask of LEGION-208 when is not the merger's open ask
// of conflictHead told launch (record.MergeForward.SentLaunch), 0 for told no launch.
func mergerTold(t *testing.T, pool *pgxpool.Pool, when string, launch uint64) {
	t.Helper()
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleMerger || mf.Head != conflictHead ||
		mf.Landed != "" || mf.SentLaunch != launch {
		t.Fatalf("%s the record's ask is %+v, want the merger's open ask of %s, told launch %d", when, mf, conflictHead, launch)
	}
}

// mergerAskedInMerging is LEGION-208 on the default steps in merging, its merger's claim held at
// launch 1 in state (mergerClaimAt; no claims row where state is ""), and the conflict two reads
// confirm with a check the base requires not green: the merger is asked, its ask's row is queued,
// and the record's ask is told no launch yet.
func mergerAskedInMerging(t *testing.T, state supervise.ClaimState) *pgxpool.Pool {
	t.Helper()
	pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
	if state != "" {
		mergerClaimAt(t, pool, 1, state)
	}
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	mergeForwardQueued(t, pool, "after the confirmed conflict", []record.MergeForwardMessage{mergerAsked()})
	mergerTold(t, pool, "after the confirmed conflict", 0)
	return pool
}

// mergerMovesOutOfMerging is the merger's move of LEGION-208 back out of merging to to, d after
// the first read (intake.BackwardMove). The merger's ask is owed by nobody there (owedTo), and
// accepting and retro, where the merge comes next on the default steps (classify.MergeComesNext),
// ask nobody else, so the record keeps the merger's ask.
func mergerMovesOutOfMerging(t *testing.T, pool *pgxpool.Pool, d time.Duration, to phase.Phase) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(d), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger, To: to,
		Reason: "the phase's work needs another read before this head can merge"})
	if got := issuePhase(t, pool); got != to {
		t.Fatalf("after the merger's move back the issue is in %s, want %s", got, to)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleMerger {
		t.Fatalf("after the merger's move back to %s the record's ask is %+v, want the merger's kept", to, mf)
	}
}

// askRowEndsOutsideMerging is the daemon's executor ending the merger's ask row undelivered while
// LEGION-208 stands outside merging, three minutes after the first read: it read the row as left
// (leftMergeForwardAt), and the end, at the claim's launch as the executor reads it under the fact
// lock (1 for the claim mergerAskedInMerging wrote in state, 0 for one failed, retired or not held:
// the daemon's claimEnded), sends nothing, since no role owes the merge there. So nothing is
// queued, and the record's ask is still the merger's, told no launch.
func askRowEndsOutsideMerging(t *testing.T, pool *pgxpool.Pool, state supervise.ClaimState) {
	t.Helper()
	launch := uint64(1)
	if state == "" || supervise.FailedOrRetired(state) {
		launch = 0
	}
	askRowEnds(t, pool, readsAfter(3*time.Minute), false, 0, launch)
	mergeForwardQueued(t, pool, "after the row's end outside merging", []record.MergeForwardMessage{})
	mergerTold(t, pool, "after the row's end outside merging", 0)
}

// movesIntoMerging is retro's completion moving LEGION-208 into merging, six minutes after the
// first read, at conflictHead; from accepting the architect's sign-off moves it into retro first,
// a minute before. It fails the test at once where the merge-forward messages queued after the
// move are not want, and says so where the move queued no row.
func movesIntoMerging(t *testing.T, pool *pgxpool.Pool, want []record.MergeForwardMessage) {
	t.Helper()
	if issuePhase(t, pool) == phase.Accepting {
		applyRefusingNothing(t, pool, readsAfter(5*time.Minute), rootsSignOff(t, pool, readsAfter(5*time.Minute), "LEGION-208"))
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("before the move into merging the issue is in %s, want retro", got)
	}
	before := mergeForwardMessages(t, pool)
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after retro's completion the issue is in %s, want merging", got)
	}
	switch got := mergeForwardMessages(t, pool); {
	case reflect.DeepEqual(got, want):
	case reflect.DeepEqual(got, before):
		t.Fatalf("the move into merging queued no row: the merge-forward messages are %+v, want %+v", got, want)
	default:
		t.Fatalf("after the move into merging the merge-forward messages are %+v, want %+v", got, want)
	}
}

// The merger is asked in merging and moves the issue back out of it. Where the ask's row then ends
// there undelivered, which sends nothing while no role owes the merge, retro's completion that
// moves the issue back into merging tells the merger's launch the ask, once, as a row sent at that
// launch: into merging from accepting by the architect's sign-off, or from retro. Its delivery
// records the launch, and neither a ready of that launch nor a later read of the conflict tells it
// again. A row still queued across the move is left to deliver the ask, and a launch a row reached
// before the move out is told nothing at the move.
func TestAMergersAskWhoseRowEndedOutsideMergingIsToldOnceAtTheMoveBack(t *testing.T) {
	t.Parallel()
	asked := []record.MergeForwardMessage{sentAgain(mergerAsked(), 1)}
	for _, to := range []phase.Phase{phase.Accepting, phase.Retro} {
		t.Run("the row ended in "+string(to), func(t *testing.T) {
			t.Parallel()
			pool := mergerAskedInMerging(t, supervise.StateReady)
			mergerMovesOutOfMerging(t, pool, 2*time.Minute, to)
			askRowEndsOutsideMerging(t, pool, supervise.StateReady)
			movesIntoMerging(t, pool, asked)
			mergerTold(t, pool, "after the move into merging", 0)

			askRowEnds(t, pool, readsAfter(7*time.Minute), true, 1, 1)
			mergeForwardQueued(t, pool, "after the row's delivery to launch 1", []record.MergeForwardMessage{})
			mergerTold(t, pool, "after the row's delivery to launch 1", 1)
			applyRefusingNothing(t, pool, readsAfter(8*time.Minute), mergerReady(1))
			applyRefusingNothing(t, pool, readsAfter(9*time.Minute), conflictingRead(&record.BaseRules{Required: []string{"ci"}}))
			mergeForwardQueued(t, pool, "after a ready of launch 1 and a later conflicting read", []record.MergeForwardMessage{})
		})
	}

	t.Run("the move back came before the row's end", func(t *testing.T) {
		t.Parallel()
		pool := mergerAskedInMerging(t, supervise.StateReady)
		mergerMovesOutOfMerging(t, pool, 2*time.Minute, phase.Accepting)
		movesIntoMerging(t, pool, []record.MergeForwardMessage{mergerAsked()})
		askRowEnds(t, pool, readsAfter(7*time.Minute), true, 1, 1)
		mergeForwardQueued(t, pool, "after the row's delivery to launch 1, one ask in all,", []record.MergeForwardMessage{})
	})

	t.Run("a row reached the launch before the move out", func(t *testing.T) {
		t.Parallel()
		pool := mergerAskedInMerging(t, supervise.StateReady)
		askRowEnds(t, pool, readsAfter(2*time.Minute), true, 1, 1)
		mergerTold(t, pool, "after the row's delivery in merging", 1)
		mergerMovesOutOfMerging(t, pool, 3*time.Minute, phase.Accepting)
		movesIntoMerging(t, pool, []record.MergeForwardMessage{})
		mergerTold(t, pool, "after the move into merging", 1)
	})
}

// The move into merging tells each launch of the merger the ask it owes once, with the launch the
// claim store holds at the move. A launch whose ready came while no role owed the merge is told at
// the move; a launch whose ready comes after the move finds the move's row queued and leaves the
// ask to it, and so does a released merger, which the row wakes. A row still queued across the
// move that then ends undelivered sends the ask to the launch at its end. A claim that failed or
// retired, and a merger the daemon holds no claim of, are told nothing at the move: the ready of
// their next launch asks.
func TestTheMoveIntoMergingTellsEachLaunchOfTheMergerOnce(t *testing.T) {
	t.Parallel()
	none := []record.MergeForwardMessage{}
	atLaunch := func(launch uint64) []record.MergeForwardMessage {
		return []record.MergeForwardMessage{sentAgain(mergerAsked(), launch)}
	}
	// relaunchedAfterTheMove is the merger's claim relaunched at 2 while the move's row for launch 1
	// is queued: its ready leaves the ask to the row, which is delivered to launch 2.
	relaunchedAfterTheMove := func(t *testing.T, pool *pgxpool.Pool) {
		mergerRelaunched(t, pool, 2, supervise.StateReady)
		applyRefusingNothing(t, pool, readsAfter(7*time.Minute), mergerReady(2))
		mergeForwardQueued(t, pool, "after the ready of launch 2, no second row,", atLaunch(1))
		askRowEnds(t, pool, readsAfter(8*time.Minute), true, 2, 2)
		mergeForwardQueued(t, pool, "after the row's delivery to launch 2", none)
		mergerTold(t, pool, "after the row's delivery to launch 2", 2)
	}
	// readyAtLaunch2 is the merger's claim relaunched at 2 and that launch's ready, which asks it.
	readyAtLaunch2 := func(t *testing.T, pool *pgxpool.Pool) {
		mergerRelaunched(t, pool, 2, supervise.StateReady)
		applyRefusingNothing(t, pool, readsAfter(7*time.Minute), mergerReady(2))
		mergeForwardQueued(t, pool, "after the ready of launch 2", atLaunch(2))
	}
	for _, tc := range []struct {
		name string
		// state is the merger's claim at launch 1, "" for no claims row.
		state supervise.ClaimState
		// rowEnds says whether the ask's row ends outside merging before the move.
		rowEnds bool
		before  func(t *testing.T, pool *pgxpool.Pool)
		atMove  []record.MergeForwardMessage
		after   func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "a row the executor read as left ends after the move", state: supervise.StateReady,
			atMove: []record.MergeForwardMessage{mergerAsked()}, after: func(t *testing.T, pool *pgxpool.Pool) {
				askRowEnds(t, pool, readsAfter(7*time.Minute), false, 0, 1)
				mergeForwardQueued(t, pool, "after the row's undelivered end in merging", atLaunch(1))
			}},
		{name: "a launch whose ready came while no role owed the merge", state: supervise.StateReady, rowEnds: true,
			before: func(t *testing.T, pool *pgxpool.Pool) {
				mergerRelaunched(t, pool, 2, supervise.StateReady)
				applyRefusingNothing(t, pool, readsAfter(4*time.Minute), mergerReady(2))
				mergeForwardQueued(t, pool, "after the ready of launch 2 in accepting", none)
			},
			atMove: atLaunch(2), after: func(t *testing.T, pool *pgxpool.Pool) {
				askRowEnds(t, pool, readsAfter(7*time.Minute), true, 2, 2)
				mergeForwardQueued(t, pool, "after the row's delivery to launch 2", none)
			}},
		{name: "a relaunch whose ready comes after the move", state: supervise.StateReady, rowEnds: true,
			atMove: atLaunch(1), after: relaunchedAfterTheMove},
		{name: "a released merger", state: supervise.StateSuspended, rowEnds: true,
			atMove: atLaunch(1), after: relaunchedAfterTheMove},
		{name: "a failed merger claim", state: supervise.StateFailed, rowEnds: true,
			atMove: none, after: readyAtLaunch2},
		{name: "a retired merger claim", state: supervise.StateRetired, rowEnds: true,
			atMove: none, after: readyAtLaunch2},
		{name: "no merger claim recorded", rowEnds: true,
			atMove: none, after: func(t *testing.T, pool *pgxpool.Pool) {
				mergerClaimAt(t, pool, 1, supervise.StateReady)
				applyRefusingNothing(t, pool, readsAfter(7*time.Minute), mergerReady(1))
				mergeForwardQueued(t, pool, "after the ready of launch 1", atLaunch(1))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := mergerAskedInMerging(t, tc.state)
			mergerMovesOutOfMerging(t, pool, 2*time.Minute, phase.Accepting)
			if tc.rowEnds {
				askRowEndsOutsideMerging(t, pool, tc.state)
			}
			if tc.before != nil {
				tc.before(t, pool)
			}
			movesIntoMerging(t, pool, tc.atMove)
			tc.after(t, pool)
		})
	}
}
