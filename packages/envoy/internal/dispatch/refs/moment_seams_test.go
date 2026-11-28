package refs

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The moment forms of the excerpt backoff's reads are the server's own work, as the forms they
// stand beside (DriftedExcerpts, NextExcerptRetry) are: the reference-excerpt reconciliation calls
// them, and their answer reaches no reader. Their reasons are named here, beside their rows, as
// visibility_census_seed_test.go sets the census's seed from a file of its own.
func init() {
	readsWithoutAViewer["DriftedExcerptsAt"] = serverWork
	readsWithoutAViewer["NextExcerptRetryAt"] = serverWork
}

// driftAt is what DriftedExcerptsAt lists at the moment at.
func driftAt(t *testing.T, database *store.Store, at time.Time) []ExcerptDrift {
	t.Helper()
	drifted, err := DriftedExcerptsAt(context.Background(), database.Pool, 20, &at)
	if err != nil {
		t.Fatalf("list the drift at %v: %v", at, err)
	}
	return drifted
}

// A deferral stamped at a fake's moment (DeferExcerptsAt) counts its backoff from that moment,
// and the drift (DriftedExcerptsAt) judges it by the moment it is handed, never the database's
// now(): the document is held back from the moment it was deferred until its next attempt, which
// falls no sooner than its base after that moment, and it is listed again by its ceiling, as the
// fake moves and the test waits out nothing. The next retry (NextExcerptRetryAt) is the wait from
// the fake's moment to that next attempt, within the fifth of jitter a deferral adds; with no
// backoff row, nothing is owed.
func TestADeferralAtAMomentFallsDueOnThatMomentsClock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	document := owedDocument(t, database, "design")
	// A moment months off the database's own now(), so a read that judged by now() rather than
	// by the moment it is handed lists the document, or holds it back, at the wrong moments.
	clock := clocktest.New(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC))
	const base, ceiling = time.Hour, 6 * time.Hour

	deferred := clock.Now()
	if wait, owed, err := NextExcerptRetryAt(ctx, database.Pool, &deferred); err != nil || owed || wait != 0 {
		t.Fatalf("next retry with no backoff row = %v (owed %t), %v; want none owed", wait, owed, err)
	}
	if err := DeferExcerptsAt(ctx, database.Pool, document, 1, base, ceiling, &deferred); err != nil {
		t.Fatalf("defer at %v: %v", deferred, err)
	}
	var due time.Time
	if err := database.Pool.QueryRow(ctx, `select next_attempt_at from reference_excerpt_backoff where artifact_id = $1::uuid`, document).Scan(&due); err != nil {
		t.Fatalf("read the deferral's next attempt: %v", err)
	}
	// A first failure waits least(base, ceiling), the base, times 1 + 0.2 * random().
	if wait := due.Sub(deferred); wait < base || wait > base+base/5 {
		t.Fatalf("the deferral at %v falls due at %v, %v later; want [%v, %v] later", deferred, due, wait, base, base+base/5)
	}
	if wait, owed, err := NextExcerptRetryAt(ctx, database.Pool, &deferred); err != nil || !owed || wait != due.Sub(deferred) {
		t.Fatalf("next retry from %v = %v (owed %t), %v; want owed, %v, the wait to the deferral's next attempt at %v",
			deferred, wait, owed, err, due.Sub(deferred), due)
	}

	if drifted := driftAt(t, database, clock.Now()); len(drifted) != 0 {
		t.Fatalf("drift at the deferral's moment %v = %#v; want none, the document waits out its backoff", clock.Now(), drifted)
	}
	clock.Advance(base - time.Microsecond)
	if drifted := driftAt(t, database, clock.Now()); len(drifted) != 0 {
		t.Fatalf("drift at %v, short of the base after the deferral at %v = %#v; want none", clock.Now(), deferred, drifted)
	}
	clock.Set(due.Add(-time.Microsecond))
	if drifted := driftAt(t, database, clock.Now()); len(drifted) != 0 {
		t.Fatalf("drift at %v, short of the next attempt at %v = %#v; want none", clock.Now(), due, drifted)
	}
	backedOff := ExcerptDrift{FromID: document, Version: 1, BackedOff: true}
	clock.Set(due)
	if drifted := driftAt(t, database, clock.Now()); len(drifted) != 1 || drifted[0] != backedOff {
		t.Fatalf("drift at the next attempt %v = %#v; want the document, backed off", clock.Now(), drifted)
	}
	clock.Set(deferred.Add(ceiling))
	if drifted := driftAt(t, database, clock.Now()); len(drifted) != 1 || drifted[0] != backedOff {
		t.Fatalf("drift at %v, the ceiling after the deferral at %v = %#v; want the document, backed off", clock.Now(), deferred, drifted)
	}
}

// With sequential scans priced out, the drift query of the moment form (driftedExcerptsAtQuery)
// must still have a plan, and it must read refs through refs_excerpt_drift alone, as
// driftedExcerptsQuery's own does (TestOwedExcerptsAreFoundThroughTheDriftIndexAndRecordedOnce).
// The query's parameters are the limit ($1) and the moment a backoff is judged at ($2, a
// timestamptz), written in as literals so EXPLAIN can plan it.
func TestTheDriftQueryAtAMomentReadsRefsThroughTheDriftIndexAlone(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	owedDocument(t, database, "design")
	// The connection is released however the read ends: the store's cleanup closes the pool,
	// which waits for every connection it lent.
	plan := func() string {
		conn, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `set enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		rows, err := conn.Query(ctx, "explain (costs off) "+strings.NewReplacer("$1", "20", "$2", "now()").Replace(driftedExcerptsAtQuery))
		if err != nil {
			t.Fatalf("explain the drift query: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the drift query's plan: %v", err)
		}
		rows.Close()
		if _, err := conn.Exec(ctx, `reset enable_seqscan`); err != nil {
			t.Fatal(err)
		}
		return plan.String()
	}()
	if strings.Count(plan, " on refs") == 0 || strings.Count(plan, " on refs") != strings.Count(plan, "refs_excerpt_drift on refs") {
		t.Fatalf("the drift plan at a moment does not read refs through refs_excerpt_drift alone:\n%s", plan)
	}
}

// A body planned once for items that do not exist yet (PlanNewItemIndex) and bound to a new item
// and the time it was written (For) is the plan PlanItemIndex makes of that body for that item
// against the index ReadItemIndex reads of it, never indexed: the same blocks, ids, excerpts,
// links and moved targets, and the item's own id and time, all that Queue writes. One plan
// bound to two items is each one's.
func TestANewItemPlanBoundToAnItemIsThePlanOfThatItem(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")

	const body = "The first paragraph cites dispatch://CORE-2.\n\nThe second paragraph cites nothing."
	located := text.ExtractAt(body, itemBlockServer)
	plan, err := PlanNewItemIndex(ItemComment, body, itemBlockServer, located)
	if err != nil {
		t.Fatalf("plan the body for a new item: %v", err)
	}
	// Each comment's row is inserted and never indexed: its state reads as no block-aware build's.
	for _, item := range []struct {
		id string
		at time.Time
	}{
		{createReferenceComment(t, tx, "CORE-1"), time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)},
		{createReferenceComment(t, tx, "CORE-1"), time.Date(2026, 10, 30, 12, 0, 1, 0, time.UTC)},
	} {
		index, err := ReadItemIndex(ctx, tx, ItemComment, item.id)
		if err != nil {
			t.Fatalf("read the index of comment %s: %v", item.id, err)
		}
		want, err := PlanItemIndex(index, ItemComment, item.id, body, itemBlockServer, item.at, located, nil)
		if err != nil {
			t.Fatalf("plan the body for comment %s: %v", item.id, err)
		}
		if len(want.written) != 2 || len(want.pairs) != 1 || want.madeAt == nil || !want.madeAt.Equal(item.at) ||
			!slices.Equal(want.Moved(), []Target{{Kind: "issue", ID: "CORE-2"}}) {
			t.Fatalf("PlanItemIndex's plan of comment %s writes %d blocks and %d links, made at %v, moving %v; want both blocks, the one mention, made at %v, moving CORE-2",
				item.id, len(want.written), len(want.pairs), want.madeAt, want.Moved(), item.at)
		}
		if got := plan.For(item.id, item.at); !reflect.DeepEqual(got, want) {
			t.Fatalf("PlanNewItemIndex's plan bound to comment %s at %v = %+v; want PlanItemIndex's plan of it, %+v", item.id, item.at, got, want)
		}
	}
}
