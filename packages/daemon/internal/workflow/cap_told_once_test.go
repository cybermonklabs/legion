package workflow

import (
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// capMoves is how many moves back this proof drives past a review round cap of 3, of three
// kinds. capReviews is how many of them are the review App's request for changes, the one kind
// that opens a round the cap counts (recordRound), and capConflicts how many are the daemon's
// own move for a head that conflicts with its base. Six counted rounds is enough that a build
// telling the architect on every counted round at or above the cap tells it four times.
const (
	capMoves     = 17
	capReviews   = 6
	capConflicts = 5
)

// capNotices is how many notices of kind the architect has been sent about the issue. The cap's
// own notice is read by its kind and not by the number of notices, because the daemon's move for
// a conflicting head wakes the architect with a checks-red notice of its own: a count of every
// wake could not tell the two apart, and would rise on a build that tells the cap once.
func capNotices(t *testing.T, pool *pgxpool.Pool, kind record.NoticeKind) int {
	t.Helper()
	count := 0
	for _, got := range noticeKinds(t, pool, "LEGION-208") {
		if got == kind {
			count++
		}
	}
	return count
}

// clearsGeneration empties the issue's generation-scoped facts as a re-admission of its tree
// does (record.Store.ClearGeneration): its rounds, its counts by move and the cap value its row
// last told go with them.
func clearsGeneration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().ClearGeneration(t.Context(), tx, "LEGION-208") })
}

// The architect is told once for each value review_round_cap holds, and not on every round at or
// above it.
//
// Seventeen moves back are driven past a cap of 3: six of them the review App's requests for
// changes, which are the rounds the cap counts, six a tester's fail and five the daemon's own
// move for a head that conflicts with its base. The architect gets one cap message and one
// pr-blocked notice out of all of them. The notice is counted by its kind: every move the daemon
// makes for a conflicting head wakes the architect with a checks-red notice, so a count of the
// architect's wakes would rise whatever the cap did. Against the build before this change, which
// told the cap on every counted round at or above it, the pr-blocked notices over these rounds
// were four.
//
// The three cases after it read the mark the telling leaves (record.PhaseRow.CapTold): it is the
// cap value told, compared with !=, so a cap raised under the row tells again at its new number,
// a cap lowered under a row that already told a higher value tells once at the lower one, which
// a < would never do, and a generation cleared from the row tells again at that generation's own
// first crossing.
func TestTheArchitectIsToldOncePerCapValue(t *testing.T) {
	t.Run("seventeen moves back past the cap", func(t *testing.T) {
		pool := migratedPool(t)
		engine := workBackEngine(3)
		seedWorkBackIssue(t, pool, headOne)

		for move := 1; move <= capMoves; move++ {
			switch move % 3 {
			case 1:
				testerFails(t, pool, engine, strconv.Itoa(move))
			case 2:
				reviewerRequestsChanges(t, pool, engine, strconv.Itoa(move), headOne)
			case 0:
				// The daemon's own move, and the read that takes the mark off the head behind
				// it: a round opened while the mark stands is a conflict round the cap does not
				// count, which is another proof's subject (TestConflictRoundsCostTheCapNothing).
				marksConflictingFrom(t, pool, engine, phase.Reviewing, "needs_review", headOne)
				clearsConflicting(t, pool, engine, headOne)
			}
		}

		if pass := implementerPass(t, pool); pass != capMoves {
			t.Fatalf("the implementer's row counts %d passes, want the %d moves back this proof drove", pass, capMoves)
		}
		if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != capReviews || conflicts != 0 {
			t.Fatalf("the implementer's row counts %d review rounds of which %d conflict rounds, want the %d reviews of the %d moves and no conflict round: the mark is cleared behind each conflicting head, so every round here is counted",
				rounds, conflicts, capReviews, capMoves)
		}
		want := "Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted."
		messages, blocked := capPosts(t, pool)
		if len(messages) != 1 || messages[0] != want || blocked != 1 {
			t.Fatalf("over %d moves back the cap posted %q and %d pr-blocked notices, want the one message %q and one notice: the cap is told once for each value it holds",
				capMoves, messages, blocked, want)
		}
		if red := capNotices(t, pool, "checks-red"); red != capConflicts {
			t.Fatalf("the architect was sent %d checks-red notices over %d moves for a conflicting head, want one each: this is what a count of the architect's wakes would read instead of the cap's own notice",
				red, capConflicts)
		}
	})

	t.Run("a cap raised under the row", func(t *testing.T) {
		pool := migratedPool(t)
		seedWorkBackIssue(t, pool, headOne)
		round := toldAtThree(t, pool)

		raised := workBackEngine(5)
		for ; round <= 6; round++ {
			reviewerRequestsChanges(t, pool, raised, strconv.Itoa(round), headOne)
			want := []string{toldAt(3, 3), toldAt(5, 5)}
			if round < 5 {
				want = want[:1]
			}
			messages, blocked := capPosts(t, pool)
			if !sameStrings(messages, want) || blocked != len(want) {
				t.Fatalf("after round %d under a cap of 5 the cap posted %q and %d pr-blocked notices, want %q and %d: a raised cap tells once at its new number",
					round, messages, blocked, want, len(want))
			}
		}
	})

	t.Run("a cap lowered under the row", func(t *testing.T) {
		pool := migratedPool(t)
		seedWorkBackIssue(t, pool, headOne)
		round := toldAtThree(t, pool)

		lowered := workBackEngine(2)
		for ; round <= 5; round++ {
			reviewerRequestsChanges(t, pool, lowered, strconv.Itoa(round), headOne)
			want := []string{toldAt(3, 3), toldAt(2, 4)}
			messages, blocked := capPosts(t, pool)
			if !sameStrings(messages, want) || blocked != len(want) {
				t.Fatalf("after round %d under a cap lowered to 2 the cap posted %q and %d pr-blocked notices, want %q and %d: the row told 3, so the first round under the lower cap tells once at 2 and the rounds after it tell nothing",
					round, messages, blocked, want, len(want))
			}
		}
	})

	t.Run("a generation cleared from the row", func(t *testing.T) {
		pool := migratedPool(t)
		seedWorkBackIssue(t, pool, headOne)
		engine := workBackEngine(3)
		round := toldAtThree(t, pool)

		clearsGeneration(t, pool)
		if rounds := implementerRounds(t, pool); rounds != 0 {
			t.Fatalf("the cleared generation left the implementer's row counting %d rounds, want none", rounds)
		}
		for crossing := 1; crossing <= 4; crossing, round = crossing+1, round+1 {
			reviewerRequestsChanges(t, pool, engine, strconv.Itoa(round), headOne)
			want := []string{toldAt(3, 3)}
			if crossing >= 3 {
				want = append(want, toldAt(3, 3))
			}
			messages, blocked := capPosts(t, pool)
			if !sameStrings(messages, want) || blocked != len(want) {
				t.Fatalf("after round %d of the new generation the cap posted %q and %d pr-blocked notices, want %q and %d: the cleared row tells again at the new generation's own first crossing of the cap, and once",
					crossing, messages, blocked, want, len(want))
			}
		}
	})
}

// toldAt is the message the cap posts when a row reaches it: the cap value and the rounds
// counted against it, with no conflict round among them (recordRound).
func toldAt(value, counted int) string {
	return "Issue reached review_round_cap=" + strconv.Itoa(value) + ": " + strconv.Itoa(counted) +
		" rounds counted, 0 conflict rounds not counted."
}

// toldAtThree drives the three review rounds that reach a cap of 3 and holds that the cap was
// told once there, which is what the three cases below start from. It answers the number of the
// next round, so each case goes on counting where it left off: the reviewer's row of a round
// names a handoff commit of its own.
func toldAtThree(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	engine := workBackEngine(3)
	for round := 1; round <= 3; round++ {
		reviewerRequestsChanges(t, pool, engine, strconv.Itoa(round), headOne)
	}
	messages, blocked := capPosts(t, pool)
	if want := []string{toldAt(3, 3)}; !sameStrings(messages, want) || blocked != 1 {
		t.Fatalf("the three rounds that reach a cap of 3 posted %q and %d pr-blocked notices, want %q and one notice", messages, blocked, want)
	}
	return 4
}
