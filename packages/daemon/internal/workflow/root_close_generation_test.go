package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root's close episode is fenced on the root's own generation, which its next change raises,
// and not on the tree's run, which the change leaves. The root lingers at generation 4 with tree
// generation 4. Its next change writes it at generation 5 and leaves its tree generation at 4.
// The expiry of the linger of generation 4, as its timer's fact and as the outbox's run of its
// LingerClose row, then closes nothing, also after the root lingers again at generation 5. The
// expiry of the linger of generation 5 closes the tree, so the reads below are seen to read a
// close.
func TestARootsCloseIsFencedOnItsOwnGeneration(t *testing.T) {
	const root = "ACME-1"
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "ACME",
		Title: "Root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{
		Project: "ACME", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, Linger: time.Hour, ProductionCheckRole: claim.RoleTester,
		ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, nil)
	apply := func(source, id string, fact intake.Fact) {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, source, id, fact, engine, admissionStub{})
		if err != nil || result.Refusal != nil {
			t.Fatalf("apply %s: %+v, %v", id, result.Refusal, err)
		}
	}
	generations := func() (generation, treeGeneration uint64, lingers bool) {
		t.Helper()
		var g, tg int64
		if err := pool.QueryRow(ctx, `select generation, tree_generation, linger_until is not null from issues where key = $1`, root).
			Scan(&g, &tg, &lingers); err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		return uint64(g), uint64(tg), lingers
	}
	closes := func() (treeCloses, removals int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) filter (where kind = 'supervise' and payload->>'op' = 'tree_close'),
			count(*) filter (where kind = 'workspace_remove') from outbox`).Scan(&treeCloses, &removals); err != nil {
			t.Fatalf("read the closes of %s: %v", root, err)
		}
		return treeCloses, removals
	}
	closesNothing := func(when string) {
		t.Helper()
		if treeCloses, removals := closes(); treeCloses != 0 || removals != 0 {
			t.Fatalf("%s: %d tree closes and %d workspace removals are queued, want none: the linger of generation 4 is over", when, treeCloses, removals)
		}
	}

	apply("api", "close-4", intake.CloseRoot{Issue: root, Reason: "the change is made"})
	if generation, treeGeneration, lingers := generations(); generation != 4 || treeGeneration != 4 || !lingers {
		t.Fatalf("after the close, %s stands at generation %d, tree generation %d, lingering %t; want 4, 4, lingering", root, generation, treeGeneration, lingers)
	}

	// The next change, as its write records it: the root's own generation rises, and the linger
	// is disarmed.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		stored, err := record.NewStore().Issue(ctx, tx, root)
		if err != nil {
			return err
		}
		stored.Generation, stored.Phase, stored.Status, stored.LingerUntil = 5, phase.Admitted, "in_progress", nil
		return record.NewStore().PutIssue(ctx, tx, *stored)
	})
	if generation, treeGeneration, lingers := generations(); generation != 5 || treeGeneration != 4 || lingers {
		t.Fatalf("after the next change, %s stands at generation %d, tree generation %d, lingering %t; want 5, 4, not lingering", root, generation, treeGeneration, lingers)
	}
	apply("timer", "linger-4", intake.LingerExpired{Issue: root, Generation: 4})
	closesNothing("after the expiry of the linger of generation 4 under the next change")

	apply("api", "close-5", intake.CloseRoot{Issue: root, Reason: "the next change is made"})
	if generation, treeGeneration, lingers := generations(); generation != 5 || treeGeneration != 4 || !lingers {
		t.Fatalf("after the later close, %s stands at generation %d, tree generation %d, lingering %t; want 5, 4, lingering", root, generation, treeGeneration, lingers)
	}
	rows, err := pool.Query(ctx, `select (payload->>'generation')::bigint from outbox where kind = 'linger_close' and issue = $1 order by id`, root)
	if err != nil {
		t.Fatalf("read the linger rows of %s: %v", root, err)
	}
	lingerRows, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || fmt.Sprint(lingerRows) != "[4 5]" {
		t.Fatalf("the linger rows of %s name the generations %v, %v; want [4 5]", root, lingerRows, err)
	}
	apply("timer", "linger-4-again", intake.LingerExpired{Issue: root, Generation: 4})
	// The outbox runs a LingerClose row as this fact, under this event ID (internal/daemon's linger).
	apply("outbox", fmt.Sprintf("linger:%s:%d", root, 4), intake.LingerExpired{Issue: root, Generation: 4})
	closesNothing("after the expiry of the linger of generation 4 in the linger of generation 5")
	if generation, _, lingers := generations(); generation != 5 || !lingers {
		t.Fatalf("after the expiry of generation 4, %s stands at generation %d, lingering %t; want the linger of 5 standing", root, generation, lingers)
	}

	apply("outbox", fmt.Sprintf("linger:%s:%d", root, 5), intake.LingerExpired{Issue: root, Generation: 5})
	if treeCloses, removals := closes(); treeCloses != len(claim.Roles) || removals != 1 {
		t.Fatalf("after the expiry of the linger of generation 5, %d tree closes and %d workspace removals are queued, want %d and 1",
			treeCloses, removals, len(claim.Roles))
	}
}
