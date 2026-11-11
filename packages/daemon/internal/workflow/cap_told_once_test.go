package workflow

import (
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// capRounds is how many rounds this proof drives past a review round cap of 3: enough that a
// build telling the architect on every round at or above the cap tells it fifteen times.
const capRounds = 17

// capNotices is how many notices of kind the architect has been sent about the issue. The cap's
// own notice is read by its kind and not by the number of notices, because a red round wakes the
// architect with a checks-red notice of its own: a count of every wake could not tell the two
// apart, and would rise on a build that tells the cap once.
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
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().ClearGeneration(t.Context(), tx, "LEGION-208", time.Now().UTC())
	})
}

// The architect is told once for each value review_round_cap holds, and not on every round at or
// above it.
//
// Seventeen rounds are driven past a cap of 3, five of them red CI verdicts, and the architect
// gets one cap message and one pr-blocked notice out of all of them. The notice is counted by
// its kind: every red round wakes the architect with a checks-red notice, so a count of the
// architect's wakes would rise whatever the cap did. Against the build before this change, which
// told the cap on every counted round at or above it, the pr-blocked notices over these rounds
// were fifteen.
//
// The three cases after it read the mark the telling leaves (record.PhaseRow.CapTold): it is the
// cap value told, compared with !=, so a cap raised under the row tells again at its new number,
// a cap lowered under a row that already told a higher value tells once at the lower one, which
// a < would never do, and a generation cleared from the row tells again at that generation's own
// first crossing.
func TestTheArchitectIsToldOncePerCapValue(t *testing.T) {
	t.Run("seventeen rounds past the cap", func(t *testing.T) {
		pool := migratedPool(t)
		engine := workBackEngine(3)
		seedWorkBackIssue(t, pool, "h0")

		head, reds := "h0", 0
		for round := 1; round <= capRounds; round++ {
			switch round % 3 {
			case 1:
				testerFails(t, pool, engine, strconv.Itoa(round))
			case 2:
				reviewerRequestsChanges(t, pool, engine, strconv.Itoa(round), head)
			case 0:
				reds++
				pushed := "h" + strconv.Itoa(reds)
				ciTurnsRed(t, pool, engine, phase.Reviewing, "needs_review", head, pushed, reds)
				head = pushed
			}
		}

		if rounds := implementerRounds(t, pool); rounds != capRounds {
			t.Fatalf("the implementer's row counts %d rounds, want the %d this proof drove", rounds, capRounds)
		}
		want := "Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted."
		messages, blocked := capPosts(t, pool)
		if len(messages) != 1 || messages[0] != want || blocked != 1 {
			t.Fatalf("over %d rounds the cap posted %q and %d pr-blocked notices, want the one message %q and one notice: the cap is told once for each value it holds",
				capRounds, messages, blocked, want)
		}
		if red := capNotices(t, pool, "checks-red"); red != reds {
			t.Fatalf("the architect was sent %d checks-red notices over %d red rounds, want one each: this is what a count of the architect's wakes would read instead of the cap's own notice",
				red, reds)
		}
	})

	t.Run("a cap raised under the row", func(t *testing.T) {
		pool := migratedPool(t)
		seedWorkBackIssue(t, pool, "h0")
		round := toldAtThree(t, pool)

		raised := workBackEngine(5)
		for ; round <= 6; round++ {
			testerFails(t, pool, raised, strconv.Itoa(round))
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
		seedWorkBackIssue(t, pool, "h0")
		round := toldAtThree(t, pool)

		lowered := workBackEngine(2)
		for ; round <= 5; round++ {
			testerFails(t, pool, lowered, strconv.Itoa(round))
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
		seedWorkBackIssue(t, pool, "h0")
		engine := workBackEngine(3)
		round := toldAtThree(t, pool)

		clearsGeneration(t, pool)
		if rounds := implementerRounds(t, pool); rounds != 0 {
			t.Fatalf("the cleared generation left the implementer's row counting %d rounds, want none", rounds)
		}
		for crossing := 1; crossing <= 4; crossing, round = crossing+1, round+1 {
			testerFails(t, pool, engine, strconv.Itoa(round))
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

// toldAtThree drives the three rounds that reach a cap of 3 and holds that the cap was told once
// there, which is what the three cases below start from. It answers the number of the next
// round, so each case goes on counting where it left off: the tester's completion of a round
// names a handoff record of its own.
func toldAtThree(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	engine := workBackEngine(3)
	for round := 1; round <= 3; round++ {
		testerFails(t, pool, engine, strconv.Itoa(round))
	}
	messages, blocked := capPosts(t, pool)
	if want := []string{toldAt(3, 3)}; !sameStrings(messages, want) || blocked != 1 {
		t.Fatalf("the three rounds that reach a cap of 3 posted %q and %d pr-blocked notices, want %q and one notice", messages, blocked, want)
	}
	return 4
}
