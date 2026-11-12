// issue_advances.go is the `advances` relation seen from the issue it is stated on: the field
// POST /api/v1/issues and PATCH /api/v1/issues/{key} write, the refusals those two answer, and
// the revision rule a whole-set write passes. The rows belong to
// internal/dispatch/prioritystore, the one place in the tree that writes `priority_tuples`;
// nothing here writes a row itself.
//
// The field has two shapes under one name. A write carries a list of priority ids, which is the
// issue's own relations and nothing else. A read answers a list of entries, each an id and
// whether the relation is the issue's own or inherited from an issue above it
// (model.IssueAdvance), because an id alone cannot tell the two apart and only an own relation
// is a write's to retract. This slice writes own relations alone, so every entry it answers is
// own; the inheritance walk is the slice after it.
package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

const (
	// codeAdvancesInput refuses an `advances` input no relation can be written from, as
	// BLOCKED_BY_INPUT refuses a blocker list beside it: more ids than the cap holds, an id no
	// priority row names, a priority whose state is gone, and a relation whose issue does not
	// exist. Each of the four is a 400 naming the key it refuses.
	codeAdvancesInput = "ADVANCES_INPUT"
	// codeAdvancesRevision refuses a write the revision rule does not let through: the one
	// refusal of this field that is not a 400.
	codeAdvancesRevision = "ADVANCES_REVISION"
)

// normalizeIssueAdvances bounds and deduplicates the priority ids of one write, before any row
// lock is taken, as normalizeIssueBlockers does for blocked_by. The cap counts the issue's own
// relations alone: inheritance is read-side, so counting what an issue inherits would let an
// issue five deep pass the cap without writing anything.
func normalizeIssueAdvances(ids []string) ([]string, error) {
	if len(ids) > contracts.MaxIssueAdvances {
		return nil, countExceededError(codeAdvancesInput, "advances", len(ids), contracts.MaxIssueAdvances)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// ownAdvances is the priority ids of the issue's own relations, read off what a read answered:
// the set a write of the whole field replaces. An inherited entry is no relation of this issue,
// so no write of this issue states or retracts it.
func ownAdvances(entries []model.IssueAdvance) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Inherited {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

// refuseAdvancesRevision is the revision rule of a PATCH carrying `advances`, decided against
// the issue as the write's own row lock read it (held, the ids it states now, and revision, the
// seq of the newest tuple the store holds for them).
//
// A write that carries the whole set rather than one act carries the revision the read it is
// answering returned, and a revision below the one the server holds is refused 409 naming the
// revision it has. A writer that sends no revision writes no whole set: it may still assert or
// retract one relation at a time, which needs none. So a write with no revision that would
// change two or more relations is refused 409 too, naming the revision the server holds and
// saying that a whole-set write carries it.
//
// A create carries no revision and is never refused here: the issue holds no relation yet, so
// there is no read to be answering.
func refuseAdvancesRevision(held []string, revision int64, carried *int64, ids []string) error {
	if carried != nil {
		if *carried < revision {
			return errorf(http.StatusConflict, codeAdvancesRevision,
				"advances revision %d is below the revision this issue holds, %d: read the issue again and write the set you mean against %d",
				*carried, revision, revision)
		}
		return nil
	}
	acts := 0
	for _, id := range ids {
		if !slices.Contains(held, id) {
			acts++
		}
	}
	for _, id := range held {
		if !slices.Contains(ids, id) {
			acts++
		}
	}
	if acts > 1 {
		return errorf(http.StatusConflict, codeAdvancesRevision,
			"this write changes %d of the issue's advances relations and carries no revision: a write with no revision asserts or retracts one relation at a time, and a whole-set write carries the revision the read it is answering returned, which is %d here",
			acts, revision)
	}
	return nil
}

// writeIssueAdvances states every relation ids names that the issue does not hold and retracts
// every one it holds that ids does not, in the caller's transaction, which is the transaction
// the issue's own row lock is held in. Nothing is updated and nothing is deleted: a relation
// that ends is a second tuple that does not assert it (prioritystore.Retract).
//
// A relation the write keeps states nothing, so the liveness of its priority is not asked
// again: a priority somebody deleted is refused as a new relation and still retractable as an
// old one, which is what lets a wrong state be taken back.
func writeIssueAdvances(
	ctx context.Context, tx pgx.Tx, issue string, held, ids []string, actor model.Actor, at time.Time,
) error {
	if err := requireIssueForAdvances(ctx, tx, issue); err != nil {
		return err
	}
	for _, id := range ids {
		if slices.Contains(held, id) {
			continue
		}
		if err := requireLivePriority(ctx, tx, id); err != nil {
			return err
		}
		if err := prioritystore.Advance(ctx, tx, issue, id, actor, at); err != nil {
			return err
		}
	}
	for _, id := range held {
		if slices.Contains(ids, id) {
			continue
		}
		if err := prioritystore.Retract(ctx, tx, issue, id, actor, at); err != nil {
			return err
		}
	}
	return nil
}

// requireIssueForAdvances refuses a relation whose issue does not exist. prioritystore holds no
// issue row, so this is the caller's refusal: a tuple out of a key no issue carries would
// address nothing and be answered by no read.
func requireIssueForAdvances(ctx context.Context, tx pgx.Tx, issue string) error {
	var exists bool
	err := tx.QueryRow(ctx, `select true from issues where key = $1`, issue).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusBadRequest, codeAdvancesInput, "advances issue %s not found", issue)
	}
	return err
}

// requireLivePriority refuses an id no priority row names and a priority whose state is gone,
// the two refusals prioritystore.Advance leaves to its caller.
func requireLivePriority(ctx context.Context, tx pgx.Tx, id string) error {
	node, held, err := prioritystore.Read(ctx, tx, id)
	if err != nil {
		return err
	}
	if !held {
		return errorf(http.StatusBadRequest, codeAdvancesInput, "advances priority %s not found", id)
	}
	if node.State != prioritystore.StateLive {
		return errorf(http.StatusBadRequest, codeAdvancesInput, "advances priority %s is gone", id)
	}
	return nil
}
