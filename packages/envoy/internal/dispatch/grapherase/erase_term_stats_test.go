package grapherase

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// erasedStatsBody is the body the statistics test gives the comment it erases: one word no other
// item of its project holds, zymurgy, beside words the project's other items hold too.
const erasedStatsBody = "Zymurgy ledger totals drift after the reseat."

// statsProjectItems is how many comments beside the erased one the statistics test writes on
// issue CORE-1: each holds astrolabe, a word the erased comment does not, and two of the erased
// comment's words, so a count of the project reads every one of them.
const statsProjectItems = 200

// TestTheEraseTakesTheErasedTermsOutOfTheStatisticsUnderItsLocksAndRecountsAfter holds the erase
// to taking the lexemes it removed out of the term statistics inside its transaction, under its
// owner locks, by statements bounded by its own rows (api.ForgetErasedTermsTx), and to counting the
// project again in full only after its commit, outside those locks (recountTermStats).
//
// Project CORE holds the comment seedAgreeingStore writes, given a body whose word zymurgy no
// other item holds, and statsProjectItems comments of issue CORE-1 holding astrolabe and two of the
// erased comment's words. A full build counts the project first, so its statistics hold
// zymurgy's lexeme. A transaction then holds astrolabe's row of those statistics `for update`:
// the erase's own statements delete only the rows of the lexemes the erased comment held, so they
// pass it, and a count of the whole project clears every row of it (rebuildSearchTermStats) and
// waits there.
//
// The proof that no such count runs under the owner locks is where the run waits on that row. It
// must wait there having committed - the comment is gone for every other reader - holding no
// owner lock (store.LockOwners' transaction advisory lock, keyed in store.OwnerLockNamespace), and
// a comment create on issue CORE-1, which takes that issue's owner lock first and waits on it no
// longer than its lock_timeout, must commit while it waits. At that moment, between the commit
// and the count, the statistics already hold no row of zymurgy's lexeme (a). Once the row is let
// go the count runs: zymurgy's lexeme is still absent, and the statistics equal what a fresh full
// build writes (b). A hold is used rather than a trace of the statements, because the row lock is
// the one step the count takes that the statements under the locks do not, so the wait names the
// transaction the count runs in, and the comment create measures what the owner lock costs a
// writer then.
func TestTheEraseTakesTheErasedTermsOutOfTheStatisticsUnderItsLocksAndRecountsAfter(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	node := commentNodeKind + ":" + seeded.comment
	const project = "CORE"
	if tag, err := database.Pool.Exec(ctx, `update comments set body = $2 where id = $1::uuid`,
		seeded.comment, erasedStatsBody); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the erased comment's body: %d rows, %v", tag.RowsAffected(), err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select 'CORE-1', '{"kind":"user","id":"bob"}', 'Astrolabe ledger totals, reading ' || n
		from generate_series(1, $1::int) n`, statsProjectItems); err != nil {
		t.Fatalf("write the project's other comments: %v", err)
	}
	erased := commentLexeme(t, database, `c.id = '`+seeded.comment+`'::uuid`, "zymurg")
	kept := commentLexeme(t, database, `c.body like 'Astrolabe%'`, "astrolab")

	api.RefreshEverySearchTermStats(ctx, database.Pool)
	if row, held := statsRow(t, database, project, erased); !held {
		t.Fatalf("the full build wrote no row of %s for %s, so this test would prove nothing", erased, project)
	} else {
		t.Logf("before the erase the statistics of %s hold %s", project, row)
	}

	hold := &lockstep.Hold{Name: "the statistics row of " + kept}
	lockstep.Begin(t, database, hold)
	if tag, err := hold.Tx.Exec(ctx, `select 1 from search_term_stats where project_key = $1 and lexeme = $2 for update`,
		project, kept); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold the statistics row of %s: %d rows, %v", kept, tag.RowsAffected(), err)
	}

	pid, answered := startErase(t, database, commentNodeKind, seeded.comment)
	if early, done := storetest.WaitForQueuedBehind(t, hold.Tx, pid, answered, lockstep.Wait); done {
		t.Fatalf("the erase of %s answered (err %v) without waiting on the statistics row of %s, which a count of %s clears",
			node, early.err, kept, project)
	}
	waited := lockstep.WaitingStatement(t, database, pid)
	var ownerLocks int64
	var committed bool
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from pg_locks
		         where pid = $1 and locktype = 'advisory' and granted and classid = $3::bigint::oid and objsubid = 2),
		       not exists (select 1 from comments where id = $2::uuid)`,
		pid, seeded.comment, store.OwnerLockNamespace).Scan(&ownerLocks, &committed); err != nil {
		t.Fatalf("read the erase's locks while it waits: %v", err)
	}
	t.Logf("the erase waits behind the statistics row of %s at %q, holding %d owner lock(s), its erase committed: %t",
		kept, waited, ownerLocks, committed)
	if ownerLocks != 0 || !committed {
		t.Errorf("the erase of %s counts the statistics of %s at %q inside the transaction that holds its owner locks "+
			"(%d owner lock(s) held, the erase committed: %t): that count reads every item of the project, and every "+
			"writer of issue CORE-1 waits for it", node, project, waited, ownerLocks, committed)
	}
	if err := createACommentUnderItsOwnerLock(t, database, "A remark written while the erase counted the statistics."); err != nil {
		t.Errorf("a comment create on issue CORE-1 failed while the erase of %s counted the statistics of %s at %q: %v; "+
			"that count runs under the erase's owner lock of the issue, so every writer of it waits for the count",
			node, project, waited, err)
	}
	requireNoErasedLexeme(t, database, node, project, erased, "once the erase had committed and before its count after the commit")

	hold.Release()
	var answer eraseAnswer
	select {
	case answer = <-answered:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase of %s did not answer within %s of the release of the statistics row of %s", node, lockstep.Wait, kept)
	}
	if answer.err != nil {
		t.Fatalf("the erase of %s failed: %v", node, answer.err)
	}
	requireTheCommentErased(t, database, answer.erasure, seeded.comment)
	if !slices.Equal(answer.erasure.Recounted, []string{project}) || len(answer.erasure.NotRecounted) != 0 {
		t.Errorf("the erase answered %v as the projects it took the erased words out of and %+v as those it could not count "+
			"again after the commit, want [%s] and none", answer.erasure.Recounted, answer.erasure.NotRecounted, project)
	}
	requireNoErasedLexeme(t, database, node, project, erased, "once the erase had counted the project again")

	got := projectTermStats(t, database, project)
	if _, err := database.Pool.Exec(ctx, `update search_term_totals set snapshot = null where project_key = $1`, project); err != nil {
		t.Fatalf("clear %s's snapshot, so the next pass counts it in full: %v", project, err)
	}
	api.RefreshEverySearchTermStats(ctx, database.Pool)
	want := projectTermStats(t, database, project)
	if got.items != want.items || !maps.Equal(got.ndoc, want.ndoc) {
		t.Errorf("after the erase the statistics of %s read %s, want the full build's %s", project, got, want)
	}
}

// commentLexeme is the lexeme starting with prefix of the search vector of a comment where holds,
// read from the vector the comment's trigger wrote, so the test names the lexeme Postgres made
// of a word rather than guessing its stem.
func commentLexeme(t *testing.T, database *store.Store, where, prefix string) string {
	t.Helper()
	var lexeme string
	if err := database.Pool.QueryRow(context.Background(), `
		select u.lexeme from comments c cross join lateral unnest(c.search) u
		where `+where+` and u.lexeme like $1 || '%' order by u.lexeme limit 1`, prefix).Scan(&lexeme); err != nil {
		t.Fatalf("read the lexeme %s* of a comment where %s: %v", prefix, where, err)
	}
	return lexeme
}

// statsRow reads the row of lexeme in project's term statistics, spelled as a failure names it,
// and whether there is one.
func statsRow(t *testing.T, database *store.Store, project, lexeme string) (string, bool) {
	t.Helper()
	var ndoc int
	err := database.Pool.QueryRow(context.Background(),
		`select ndoc from search_term_stats where project_key = $1 and lexeme = $2`, project, lexeme).Scan(&ndoc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the statistics row of %s for %s: %v", lexeme, project, err)
	}
	return fmt.Sprintf("(project_key %s, lexeme %s, ndoc %d)", project, lexeme, ndoc), true
}

// requireNoErasedLexeme fails the test where project's term statistics hold a row of lexeme, a
// lexeme only the erased text held, read at the moment when names.
func requireNoErasedLexeme(t *testing.T, database *store.Store, node, project, lexeme, when string) {
	t.Helper()
	if row, held := statsRow(t, database, project, lexeme); held {
		t.Errorf("the erase of %s left the lexeme %s of the erased text in search_term_stats of %s: %s, read %s",
			node, lexeme, project, row, when)
	}
}

// termStats is one project's term statistics: its item count and each lexeme's ndoc.
type termStats struct {
	items int
	ndoc  map[string]int
}

func (s termStats) String() string {
	lexemes := slices.Sorted(maps.Keys(s.ndoc))
	counts := make([]string, 0, len(lexemes))
	for _, lexeme := range lexemes {
		counts = append(counts, fmt.Sprintf("%s=%d", lexeme, s.ndoc[lexeme]))
	}
	return fmt.Sprintf("items %d, ndoc {%s}", s.items, strings.Join(counts, " "))
}

// projectTermStats reads project's term statistics as they stand.
func projectTermStats(t *testing.T, database *store.Store, project string) termStats {
	t.Helper()
	ctx := context.Background()
	stats := termStats{ndoc: map[string]int{}}
	if err := database.Pool.QueryRow(ctx,
		`select items from search_term_totals where project_key = $1`, project).Scan(&stats.items); err != nil {
		t.Fatalf("read %s's item count: %v", project, err)
	}
	rows, err := database.Pool.Query(ctx, `select lexeme, ndoc from search_term_stats where project_key = $1`, project)
	if err != nil {
		t.Fatalf("read %s's term statistics: %v", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var lexeme string
		var ndoc int
		if err := rows.Scan(&lexeme, &ndoc); err != nil {
			t.Fatalf("read a row of %s's term statistics: %v", project, err)
		}
		stats.ndoc[lexeme] = ndoc
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's term statistics: %v", project, err)
	}
	return stats
}
