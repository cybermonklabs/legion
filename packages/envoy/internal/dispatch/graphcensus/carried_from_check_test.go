package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// c-carried-from, the census's side (R27 of the issue-tree spec). A carried approval says that it
// is carried: writeCarriedReview writes, for every carry, a row of artifact_review_carries keyed by
// the review's id, naming the version the approval was carried from. A review written before this
// build holds no such row, and a bounded pass of the backfill (Backfill) fills the rows of the
// carried reviews that exist. It reads artifact_reviews in batches of backfillBatch, ordered by
// artifact_id and model.ReviewOrder. The census kind review_carried_from
// (reviewCarriedFromKind) holds the table to the reviews: a carried review - one whose reason
// docs.CarriedFromVersion reads as carried - with no row, a row naming another version than the
// reason, and a row whose review is gone are each one difference, and the pass brings the three
// to zero.

// carriedFromReview is the id of the unsequenced carried review the census test seeds.
// The backfillBatch sequenced approvals of the same artifact sort before it, so a pass that
// read one batch would never reach the carry.
const carriedFromReview = "f0000000-0000-4000-8000-000000000001"

// carriedFromReason is the reason docs' reading case records when it carries the approval of
// version from (docs' carriedReasonPrefix, readingCase, carriedReasonEnd), the sentence
// docs.CarriedFromVersion reads from out of. This package cannot import docs, which imports it, so
// it spells the sentence, as api's readingCarrySentence does.
func carriedFromReason(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the readings of its decisions and of its answered asks, which change no decision, were the whole change since it.", from)
}

// seedCarriedFromReviews writes issue LOCK-1 of project LOCK and its spec (seedKeepIssue), then the
// spec's reviews as the release before this one writes them, with no artifact_review_carries row:
// backfillBatch sequenced approvals of version 4 that alice gave herself, each with a reason of her
// own; and the unsequenced review carriedFromReview, alice's approval of version 5,
// whose reason says it was carried from the approval of version 4.
func seedCarriedFromReviews(t *testing.T, database *store.Store) {
	t.Helper()
	document, _, _ := seedKeepIssue(t, database)
	ctx := context.Background()
	drivenTx(t, database, "write the reviews of LOCK-1's spec", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into artifact_reviews (id, artifact_id, version, state, actor, reason, event_seq)
			select ('00000000-0000-4000-8000-' || lpad(n::text, 12, '0'))::uuid, $1::uuid, 4, 'approved',
			       '{"kind":"user","id":"alice"}', 'Read it through and it holds.', n
			from generate_series(1, $2::int) n
		`, document, backfillBatch); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifact_reviews (id, artifact_id, version, state, actor, reason)
			values ($1::uuid, $2::uuid, 5, 'approved', '{"kind":"user","id":"alice"}', $3)
		`, carriedFromReview, document, carriedFromReason(4))
		return err
	})
}

// carriedFromRowOf is the carried_from_version of the artifact_review_carries row of review, and
// whether the table holds one.
func carriedFromRowOf(t *testing.T, database *store.Store, review string) (int, bool) {
	t.Helper()
	var version int
	err := database.Pool.QueryRow(context.Background(),
		`select carried_from_version from artifact_review_carries where review_id = $1::uuid`, review).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the artifact_review_carries row of the review %s: %v", review, err)
	}
	return version, true
}

// requireCarriedFromDifference fails t unless the census kind review_carried_from reads one
// difference, which names the review carriedFromReview, while broken holds.
func requireCarriedFromDifference(t *testing.T, database *store.Store, broken string) {
	t.Helper()
	found, reported, _ := censusDifferences(t, database, reviewCarriedFromKind)
	named := len(reported) == 1 &&
		(strings.Contains(reported[0].At, carriedFromReview) || strings.Contains(reported[0].Cause, carriedFromReview))
	if found != 1 || !named {
		t.Errorf("with %s the census reads %d difference(s) of kind %s, %v, want the one naming the review %s",
			broken, found, reviewCarriedFromKind, reported, carriedFromReview)
	}
}

// requireCarriedFromAgrees fails t unless the census kind review_carried_from reads no difference
// after the pass of the backfill that after names.
func requireCarriedFromAgrees(t *testing.T, database *store.Store, after string) {
	t.Helper()
	if found, reported, _ := censusDifferences(t, database, reviewCarriedFromKind); found != 0 {
		t.Errorf("after %s the census reads %d difference(s) of kind %s, %v, want none: the pass left the review %s and its artifact_review_carries row apart",
			after, found, reviewCarriedFromKind, reported, carriedFromReview)
	}
}

// TestCensusHoldsEveryCarriedReviewsSourceVersion is c-carried-from's census case (R27). It seeds
// the spec of LOCK-1 with backfillBatch approvals alice gave herself and one carried review, all as
// the release before writes them, with no artifact_review_carries row (seedCarriedFromReviews). The
// census kind review_carried_from reads the carried review with no row as one difference before the
// backfill, and none after it, which wrote its row naming version 4, the version its reason names,
// and no row of any approval alice gave herself: the pass reads artifact_reviews in batches of
// backfillBatch ordered by artifact_id and model.ReviewOrder, and the carry stands in its second
// batch. An SQL update that changes its source version to 3 is one difference. The next pass
// rewrites it to 4, so the census reads zero. An SQL delete of the review is one difference, its
// row now standing for a review that is gone, and the next pass deletes the row, so the census
// reads zero again.
func TestCensusHoldsEveryCarriedReviewsSourceVersion(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedCarriedFromReviews(t, database)
	backfill := func(over string) {
		t.Helper()
		if _, err := Backfill(ctx, database, io.Discard); err != nil {
			t.Fatalf("run the backfill over %s: %v", over, err)
		}
	}

	// A carried review with no row.
	requireCarriedFromDifference(t, database, "the carried review written with no artifact_review_carries row")
	backfill("the carried review with no row")
	if version, held := carriedFromRowOf(t, database, carriedFromReview); !held || version != 4 {
		t.Errorf("after the backfill the review %s, whose reason names version 4, holds %s, want its artifact_review_carries row naming version 4: the pass reads artifact_reviews in batches of %d in artifact_id and model.ReviewOrder order, and the review stands in the second",
			carriedFromReview, carriedFromRowWords(version, held), backfillBatch)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_review_carries`).Scan(&rows); err != nil {
		t.Fatalf("count the rows of artifact_review_carries: %v", err)
	}
	if rows != 1 {
		t.Errorf("after the backfill artifact_review_carries holds %d rows, want the one of the review %s: the %d approvals alice gave herself carry nothing",
			rows, carriedFromReview, backfillBatch)
	}
	requireCarriedFromAgrees(t, database, "the backfill over the carried review with no row")

	// A row naming another version than the reason.
	if _, err := database.Pool.Exec(ctx, `update artifact_review_carries set carried_from_version = 3 where review_id = $1::uuid`, carriedFromReview); err != nil {
		t.Fatalf("change the version of the row of the review %s: %v", carriedFromReview, err)
	}
	requireCarriedFromDifference(t, database, "the row of the review naming version 3 while its reason names version 4")
	backfill("the row naming another version")
	if version, held := carriedFromRowOf(t, database, carriedFromReview); !held || version != 4 {
		t.Errorf("after the backfill the review %s, whose reason names version 4, holds %s, want the pass to rewrite its row to version 4",
			carriedFromReview, carriedFromRowWords(version, held))
	}
	requireCarriedFromAgrees(t, database, "the backfill over the row naming another version")

	// A row whose review is gone.
	if _, err := database.Pool.Exec(ctx, `delete from artifact_reviews where id = $1::uuid`, carriedFromReview); err != nil {
		t.Fatalf("delete the review %s: %v", carriedFromReview, err)
	}
	requireCarriedFromDifference(t, database, "the row of the review the delete removed")
	backfill("the row whose review is gone")
	if version, held := carriedFromRowOf(t, database, carriedFromReview); held {
		t.Errorf("after the backfill the review %s is gone and its artifact_review_carries row naming version %d stands, want the pass to delete it",
			carriedFromReview, version)
	}
	requireCarriedFromAgrees(t, database, "the backfill over the row whose review is gone")
}

// carriedFromRowWords is what carriedFromRowOf read, as a failure names it.
func carriedFromRowWords(version int, held bool) string {
	if !held {
		return "no artifact_review_carries row"
	}
	return fmt.Sprintf("the artifact_review_carries row naming version %d", version)
}
