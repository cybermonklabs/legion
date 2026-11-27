package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const reviewCarriedFromKind = "review_carried_from"

type reviewCarryVersion struct {
	artifactID string
	id         string
	eventSeq   *int64
	createdAt  time.Time
	reason     *string
	stored     *int
}

func (review reviewCarryVersion) source() (int, bool) {
	if review.reason == nil {
		return 0, false
	}
	return model.CarriedFromVersion(*review.reason)
}

const reviewCarryPageRows = `
	select artifact_id, event_seq, created_at, id, reason from artifact_reviews`

const reviewCarryPageOrder = ` order by artifact_reviews.artifact_id, ` + model.ReviewOrder

const reviewCarryPageRead = `
	select artifact_id::text, artifact_reviews.id::text, event_seq, created_at, reason,
	       (select carried_from_version from artifact_review_carries where review_id = artifact_reviews.id)
	from (`

const reviewCarryFirstPage = reviewCarryPageRead + reviewCarryPageRows + reviewCarryPageOrder + `
	limit $1) artifact_reviews` + reviewCarryPageOrder

// Following a sequenced cursor, three disjoint index ranges remain: lower sequenced keys
// in this artifact, its unsequenced keys, and all keys in later artifacts. Each arm seeks
// through artifact_reviews_by_artifact_event_seq and yields at most one page; the outer
// ORDER BY and LIMIT pick the next page from those candidates.
const reviewCarryAfterSequence = reviewCarryPageRead + `
	(` + reviewCarryPageRows + `
	 where artifact_id = $1::uuid
	   and (event_seq, created_at, id) < ($2::bigint, $3::timestamptz, $4::uuid)` + reviewCarryPageOrder + ` limit $5)
	union all
	(` + reviewCarryPageRows + `
	 where artifact_id = $1::uuid and event_seq is null` + reviewCarryPageOrder + ` limit $5)
	union all
	(` + reviewCarryPageRows + `
	 where artifact_id > $1::uuid` + reviewCarryPageOrder + ` limit $5)
	) artifact_reviews` + reviewCarryPageOrder + ` limit $5`

// Within the nulls-last range, only the older (time, UUID) pairs of this artifact remain,
// followed by all later artifacts.
const reviewCarryAfterUnsequenced = reviewCarryPageRead + `
	(` + reviewCarryPageRows + `
	 where artifact_id = $1::uuid and event_seq is null
	   and (created_at, id) < ($2::timestamptz, $3::uuid)` + reviewCarryPageOrder + ` limit $4)
	union all
	(` + reviewCarryPageRows + `
	 where artifact_id > $1::uuid` + reviewCarryPageOrder + ` limit $4)
	) artifact_reviews` + reviewCarryPageOrder + ` limit $4`

const reviewCarryIDsAfter = `
	select review_id::text from artifact_review_carries
	where review_id >= coalesce($1::uuid, '` + zeroUUID + `'::uuid)
	  and review_id is distinct from $1::uuid
	order by artifact_review_carries.review_id limit $2`

// orphanReviewCarryIDs probes the reviews for one page of carry IDs. The page's order belongs
// to the carry table, not to the reviews; only this second statement reads the reviews.
const orphanReviewCarryIDs = `
	select carry.id from unnest($1::text[]) carry(id)
	where not exists (select 1 from artifact_reviews where id = carry.id::uuid)`

func readReviewCarryPage(ctx context.Context, q graphstore.Queryer, after *reviewCarryVersion) (pgx.Rows, error) {
	switch {
	case after == nil:
		return q.Query(ctx, reviewCarryFirstPage, backfillBatch)
	case after.eventSeq == nil:
		return q.Query(ctx, reviewCarryAfterUnsequenced, after.artifactID, after.createdAt, after.id, backfillBatch)
	default:
		return q.Query(ctx, reviewCarryAfterSequence, after.artifactID, *after.eventSeq, after.createdAt, after.id, backfillBatch)
	}
}

func walkReviewCarryVersions(ctx context.Context, q graphstore.Queryer, visit func([]reviewCarryVersion) error) error {
	var after *reviewCarryVersion
	for {
		rows, err := readReviewCarryPage(ctx, q, after)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (reviewCarryVersion, error) {
			var review reviewCarryVersion
			err := row.Scan(&review.artifactID, &review.id, &review.eventSeq, &review.createdAt, &review.reason, &review.stored)
			return review, err
		})
		if err != nil || len(batch) == 0 {
			return err
		}
		if err := visit(batch); err != nil {
			return err
		}
		if len(batch) < backfillBatch {
			return nil
		}
		after = &batch[len(batch)-1]
	}
}

// walkReviewCarryIDs reports the number of carry rows and the orphan IDs in each page.
func walkReviewCarryIDs(ctx context.Context, q graphstore.Queryer, visit func(int, []string) error) error {
	var after *string
	for {
		rows, err := q.Query(ctx, reviewCarryIDsAfter, after, backfillBatch)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(batch) == 0 {
			return err
		}
		rows, err = q.Query(ctx, orphanReviewCarryIDs, batch)
		if err != nil {
			return err
		}
		orphans, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := visit(len(batch), orphans); err != nil {
			return err
		}
		if len(batch) < backfillBatch {
			return nil
		}
		after = &batch[len(batch)-1]
	}
}

// censusOfReviewCarries holds the carry rows to their immutable reviews in the census snapshot.
// No graph node projects a carry: Nodes and NodeDifferences count the carry-table side.
func censusOfReviewCarries(ctx context.Context, tx pgx.Tx) (KindReport, error) {
	kind := KindReport{
		Kind: reviewCarriedFromKind, Table: "artifact_review_carries", EmptyAgrees: true,
		Note: "carried reviews (rows) and their source-version records (nodes)",
	}
	err := walkReviewCarryVersions(ctx, tx, func(batch []reviewCarryVersion) error {
		for _, review := range batch {
			from, carried := review.source()
			if !carried {
				continue
			}
			kind.Rows++
			if review.stored != nil && *review.stored == from {
				continue
			}
			kind.RowDifferences++
			if len(kind.ReportedRows) < reportedDifferences {
				cause := fmt.Sprintf("missing carry row for source version %d", from)
				if review.stored != nil {
					cause = fmt.Sprintf("carry row names version %d; reason names %d", *review.stored, from)
				}
				kind.ReportedRows = append(kind.ReportedRows, Difference{Cause: cause, At: review.id})
			}
		}
		return nil
	})
	if err != nil {
		return kind, fmt.Errorf("census carried review versions: %w", err)
	}
	err = walkReviewCarryIDs(ctx, tx, func(count int, orphans []string) error {
		kind.Nodes += int64(count)
		kind.NodeDifferences += int64(len(orphans))
		for _, id := range orphans {
			if len(kind.ReportedNodes) == reportedDifferences {
				break
			}
			kind.ReportedNodes = append(kind.ReportedNodes, Difference{Cause: "review is missing", At: id})
		}
		return nil
	})
	if err != nil {
		return kind, fmt.Errorf("census carried review rows: %w", err)
	}
	return kind, nil
}

// backfillReviewCarries repairs only this side table. Each batch writes in one atomic statement:
// no owner row, review, graph node or event is written or locked. Reviews are immutable and
// live writers insert both rows together. The upsert preserves a carry's separate record digest.
func backfillReviewCarries(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: reviewCarriedFromKind}
	err := walkReviewCarryVersions(ctx, database.Pool, func(batch []reviewCarryVersion) error {
		var ids []string
		var versions []int64
		for _, review := range batch {
			from, carried := review.source()
			if carried && (review.stored == nil || *review.stored != from) {
				ids = append(ids, review.id)
				versions = append(versions, int64(from))
			}
		}
		if len(ids) == 0 {
			return nil
		}
		tag, err := database.Pool.Exec(ctx, `
			insert into artifact_review_carries (review_id, carried_from_version)
			select artifact_reviews.id, input.version
			from unnest($1::text[], $2::bigint[]) input(id, version)
			join artifact_reviews on artifact_reviews.id = input.id::uuid
			`+reviewCarryPageOrder+`
			on conflict (review_id) do update set carried_from_version = excluded.carried_from_version
			where artifact_review_carries.carried_from_version is distinct from excluded.carried_from_version`, ids, versions)
		done.Rows += tag.RowsAffected()
		return err
	})
	if err != nil {
		return done, fmt.Errorf("backfill carried review versions: %w", err)
	}
	err = walkReviewCarryIDs(ctx, database.Pool, func(_ int, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		tag, err := database.Pool.Exec(ctx, `
			delete from artifact_review_carries carry
			where carry.review_id = any($1::text[]::uuid[])
			  and not exists (select 1 from artifact_reviews where id = carry.review_id)`, ids)
		done.Rows += tag.RowsAffected()
		return err
	})
	if err != nil {
		return done, fmt.Errorf("remove orphaned carried review rows: %w", err)
	}
	done.Complete = true
	return done, nil
}
