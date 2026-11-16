package workflow

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// noGateMoves is how many moves back this proof drives: ten times the review round cap it runs
// under, and six times the every of its hook, so a build that stopped the work at any count the
// daemon keeps would stop it well before the last move. noGateReviews is how many of them are a
// review's request for changes, the one kind that opens a review round (recordRound), which is
// what the cap reads.
const (
	noGateMoves   = 30
	noGateReviews = noGateMoves / 5
)

// startWriteMark is when the issue's start row was last written: the number an insert of an
// outbox row takes, which the start record.Store.EnqueueStart writes in place of the one the
// issue owes takes afresh with it. It is what tells the start one move wrote from the start of
// the move before it, which it takes out while no lane has delivered it, carrying that row's
// creation time, which holds the issue's place in the waiting line.
func startWriteMark(t *testing.T, pool *pgxpool.Pool, id int64) int64 {
	t.Helper()
	var mark int64
	if err := pool.QueryRow(context.Background(), "select write_mark from outbox where id = $1", id).Scan(&mark); err != nil {
		t.Fatalf("read the write mark of start row %d: %v", id, err)
	}
	return mark
}

// No count the daemon keeps gates the work: whatever the counts read, the move back hands the
// work to the next role and the issue goes on.
//
// Thirty moves back are driven here, of five kinds, under a review round cap of 3 and a hook
// that fires every 3 moves back, so the hook's count is past its number from the third move back
// on and the cap's from the twelfth move, which is the third review. Every move is read as it is
// made: the issue stands in the phase the move hands the work to, and the row the outbox would
// deliver is a start of that phase's role, written by this move
// and runnable at once. A build that stopped or held the work at any count leaves a move whose
// start is still the one before it, or an issue in held, and fails at the move it stopped at — a
// proof that read only the last move could not tell a stop at move 7 from no stop at all.
//
// The issue holds its admission slot throughout. An issue that holds none owes its start to the
// waiting line, and the owed row is what the next move's start replaces, so the work would wait
// for the cap rather than for anything this proof is about.
func TestNoCountGatesTheWork(t *testing.T) {
	pool := migratedPool(t)
	engine := workBackEngine(3, Hook{Name: "work-back", On: HookEventWorkBack, Every: 3,
		Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer},
		Prompt: saySoFar})
	now := engine.now()
	seedWorkBackIssue(t, pool, headOne)
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: now})

	mark := int64(0)
	for move := 1; move <= noGateMoves; move++ {
		var kind string
		var to phase.Phase
		switch move % 5 {
		case 1:
			kind, to = "a tester's fail", phase.Implementing
			testerFails(t, pool, engine, strconv.Itoa(move))
		case 2:
			kind, to = "a review's request for changes", phase.Implementing
			reviewerRequestsChanges(t, pool, engine, strconv.Itoa(move), headOne)
		case 3:
			kind, to = "the architect's move back to testing", phase.Testing
			architectMovesBack(t, pool, engine, phase.Testing)
		case 4:
			kind, to = "the architect's move back to the plan", phase.Planning
			architectMovesBack(t, pool, engine, phase.Planning)
		case 0:
			kind, to = "the merger's own move back to the reviewer", phase.Reviewing
			mergerMovesBack(t, pool, engine)
		}

		switch got := issuePhase(t, pool); {
		case got == phase.Held:
			t.Fatalf("move %d, %s, left the issue held: no count the daemon keeps holds an issue, and this is move %d of %d", move, kind, move, noGateMoves)
		case got != to:
			t.Fatalf("move %d, %s, left the issue in %s, want %s: the move hands the work on whatever the counts read", move, kind, got, to)
		}
		starts := oneStartRowsOf(t, pool, "LEGION-208", now)
		if len(starts) != 1 {
			t.Fatalf("after move %d, %s, the issue holds %d start rows, want the one this move wrote: %+v", move, kind, len(starts), starts)
		}
		start := starts[0]
		written := startWriteMark(t, pool, start.id)
		if written <= mark {
			t.Fatalf("move %d, %s, wrote no start of its own: the row the outbox would deliver is still the one written before this move (write mark %d, and the move before it wrote %d), so this move handed the work to nobody",
				move, kind, written, mark)
		}
		mark = written
		if start.role != phase.RoleFor(to) || start.stands != to || start.task == "" {
			t.Fatalf("move %d, %s, wrote the start %+v, want the %s's, carrying a task and naming the %s phase it is for", move, kind, start, phase.RoleFor(to), to)
		}
		if !start.claimable {
			t.Fatalf("move %d, %s, wrote a start no lane may run: %+v. The issue holds its admission slot, so there is nothing left for its start to wait for",
				move, kind, start)
		}
	}

	// What the two counts did while the work went on: both were told, and neither stopped
	// anything. The cap's message went once, at the review round that reached 3, and the hook
	// fired on every third move back of the thirty.
	if pass := implementerPass(t, pool); pass != noGateMoves {
		t.Fatalf("the implementer's row counts %d passes, want the %d moves back this proof drove: every move back raises the pass, whatever kind it is", pass, noGateMoves)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != noGateReviews || conflicts != 0 {
		t.Fatalf("the implementer's row counts %d review rounds of which %d conflict rounds, want the %d reviews of the thirty moves and no conflict round: a review that requested changes is what opens a round (recordRound)", rounds, conflicts, noGateReviews)
	}
	if posted := firingMessages(t, pool); len(posted) != noGateMoves/3 {
		t.Fatalf("the hook fired %d times over %d moves back, want %d: it fires every 3, and the firings are no gate either", len(posted), noGateMoves, noGateMoves/3)
	}
	messages, blocked := capPosts(t, pool)
	if len(messages) != 1 || blocked != 1 {
		t.Fatalf("review_round_cap posted %q and %d pr-blocked notices over %d moves back, want one of each: the cap tells the architect and gates nothing", messages, blocked, noGateMoves)
	}
}
