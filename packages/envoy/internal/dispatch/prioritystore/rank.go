package prioritystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
)

// The order a priority holds is a fractional key, written as the issue board's rank is
// (api/issue_rank.go), with the project dropped: a priority belongs to none, so the whole store
// is one order. The helpers below are that file's, reading the key off the node row's attributes
// rather than off a column of its own.

// rankKey is a priority's order key as every read that orders or compares one takes it: ordered
// as bytes, which is the order rank.Between builds a key in. The `collate "C"` is there because
// the key is read out of `attributes`, a jsonb column, and text taken out of jsonb carries no
// collation of its own, so a comparison or an `order by` on it falls to the database's default
// collation — ICU en-US on this deployment's server and on its test databases, where `a` sorts
// before `Z`. Left to that collation, lastRank answers `Z` once the seventh key (`a`, the one
// rank.Between writes after `Z`) is written, so the eighth create and every create after it
// takes `a` again and the listing's order is not the key's. The issue board's own key needs no
// clause of this kind only because its column is declared with one (`issues.rank`,
// store/migrations/0049_issue_rank_c_collation.up.sql). Take the clause out and the duplicate
// keys come back: TestEveryPriorityAppendedTakesADistinctKeyAndTheListingFollowsIt reads them.
const rankKey = `attributes->>'` + AttributeRank + `' collate "C"`

// RankAt answers the order key one priority takes between two of its neighbours, and takes the
// allocation lock first, so two moves cannot read one gap and write two keys into it. Each
// neighbour is named by its priority id or is empty, which means unbounded: before is the
// priority this one lands above, after the one it lands below, as `rank.before` and `rank.after`
// name them on the issue board. Naming neither puts it last.
//
// The id is the priority being moved, which is left out of its own neighbourhood: a move that
// named it would read its own key as the bound and never leave where it is. It need not be a
// priority the store holds, so a caller that is about to create one asks for the key with the id
// it is about to write.
func RankAt(ctx context.Context, tx pgx.Tx, id, before, after string) (string, error) {
	if err := lockRankAllocation(ctx, tx); err != nil {
		return "", err
	}
	var (
		previous string
		next     string
		err      error
	)
	if after != "" {
		previous, err = rankBoundary(ctx, tx, id, after)
		if err != nil {
			return "", err
		}
	}
	if before != "" {
		next, err = rankBoundary(ctx, tx, id, before)
		if err != nil {
			return "", err
		}
	}
	if previous != "" && next != "" && previous >= next {
		return "", fmt.Errorf("%w: %s and %s are not in that order", ErrRankNeighbor, after, before)
	}
	if previous != "" {
		next, err = nextRank(ctx, tx, id, previous, next)
	} else if next != "" {
		previous, err = previousRank(ctx, tx, id, next)
	} else {
		previous, err = lastRank(ctx, tx, id)
	}
	if err != nil {
		return "", err
	}
	return rank.Between(previous, next), nil
}

// Move writes one priority's order: the key RankAt answers for the neighbours named, onto the
// node row and into the log, in the caller's one transaction. It answers the key it wrote, which
// is what the route hands back. An id the store does not hold is ErrNoPriority, and a neighbour
// it does not hold is ErrRankNeighbor.
func Move(ctx context.Context, tx pgx.Tx, id, before, after string, actor model.Actor, at time.Time) (string, error) {
	key, err := RankAt(ctx, tx, id, before, after)
	if err != nil {
		return "", err
	}
	if err := SetAttributes(ctx, tx, id, map[string]string{AttributeRank: key}, actor, at); err != nil {
		return "", err
	}
	return key, nil
}

// lockRankAllocation serialises the whole order, as lockProjectRankAllocation serialises one
// project's board. There is one lock and not one per project, because a priority is in no
// project: every move reads the same neighbourhood.
func lockRankAllocation(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('priority-rank'))`); err != nil {
		return fmt.Errorf("lock priority rank allocation: %w", err)
	}
	return nil
}

// nextRank is the key of the first priority after previous, bounded above by next where the
// caller named one: the upper bound of the gap the move lands in.
func nextRank(ctx context.Context, q Queryer, id, previous, next string) (string, error) {
	var key string
	err := q.QueryRow(ctx, `
		select `+rankKey+` from priority_nodes
		where kind = '`+KindPriority+`' and id <> $1
			and `+rankKey+` > $2::text
			and ($3::text = '' or `+rankKey+` < $3::text)
		order by `+rankKey+`
		limit 1
	`, id, previous, next).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return next, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the priority after %s: %w", previous, err)
	}
	return key, nil
}

// previousRank is the key of the last priority before next: the lower bound of the gap, empty
// where next is the first priority of the order.
func previousRank(ctx context.Context, q Queryer, id, next string) (string, error) {
	var key string
	err := q.QueryRow(ctx, `
		select `+rankKey+` from priority_nodes
		where kind = '`+KindPriority+`' and id <> $1
			and `+rankKey+` < $2::text
		order by `+rankKey+` desc
		limit 1
	`, id, next).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the priority before %s: %w", next, err)
	}
	return key, nil
}

// lastRank is the key of the last priority of the order, empty where the store holds no other:
// the lower bound of a move that names no neighbour, which lands at the end.
func lastRank(ctx context.Context, q Queryer, id string) (string, error) {
	var key string
	err := q.QueryRow(ctx, `
		select `+rankKey+` from priority_nodes
		where kind = '`+KindPriority+`' and id <> $1
			and attributes->>'`+AttributeRank+`' is not null
		order by `+rankKey+` desc
		limit 1
	`, id).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the last priority of the order: %w", err)
	}
	return key, nil
}

// rankBoundary reads one neighbour's key. A neighbour that is this priority, or one the store
// does not hold, or one carrying no order of its own, is ErrRankNeighbor: each would be a move to
// a place the order does not have.
func rankBoundary(ctx context.Context, q Queryer, id, neighbor string) (string, error) {
	if neighbor == id {
		return "", fmt.Errorf("%w: %s must name another priority", ErrRankNeighbor, neighbor)
	}
	var key string
	err := q.QueryRow(ctx, `
		select coalesce(`+rankKey+`, '') from priority_nodes
		where kind = '`+KindPriority+`' and id = $1
	`, neighbor).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s is no priority", ErrRankNeighbor, neighbor)
	}
	if err != nil {
		return "", fmt.Errorf("read the order of priority %s: %w", neighbor, err)
	}
	if key == "" {
		return "", fmt.Errorf("%w: %s holds no order", ErrRankNeighbor, neighbor)
	}
	return key, nil
}
