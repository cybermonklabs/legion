package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// blockIndexBacklogQuery is what the block-index reconciliation still owes documents: how many
// hold no indexed version (document_block_versions.version null) or hold this build's own mark
// (store_owed_at, which the backfill and every debt this build owes beside the null version set)
// of a kind a build of this binary's marking rule lists (refs.OwedMarkListed), and when the
// oldest of those debts was incurred. The server's own tick reads the same two figures and logs
// them (docs.ReconcileBlockIndex); this is the read for somebody holding the database and not the
// log. It binds the marking rule as $1 (readBlockIndexBacklog).
//
// Nothing dates a debt, so the rows are dated by the write that owed them, which is what
// refs.OwedBlockIndexDebts derives; the tick's own read of the same backlog derives it from
// that one text too, so the two figures cannot drift.
var blockIndexBacklogQuery = `
	select count(*), min(owed.owed_at) from (` + refs.OwedBlockIndexDebts + `
	) owed`

// blockIndexBacklogBeforeTheMarkQuery is the same backlog on a database migration
// 20261120122600 has not reached, where the mark's rule (store_owed_rule) the full text reads
// does not exist and a null version is the only debt read (refs.OwedBlockIndexDebtsBeforeTheMark).
// The sizing takes its readings on such a database too.
var blockIndexBacklogBeforeTheMarkQuery = `
	select count(*), min(owed.owed_at) from (` + refs.OwedBlockIndexDebtsBeforeTheMark + `
	) owed`

// backfillMarkHeldQuery says whether document_block_versions holds the column of the backfill's
// mark that the full backlog reads last: store_owed_rule, which migration 20261120122600 adds
// beside the store_owed_at that migration 20261116221539 added.
const backfillMarkHeldQuery = `select exists (
	select 1 from pg_attribute
	where attrelid = to_regclass('document_block_versions') and attname = 'store_owed_rule' and not attisdropped)`

// backlogClockQuery is the newest write the database recorded: the created_at of its highest
// event id, read through events_pkey. It is the clock the oldest debt's age is measured
// against, in place of now(), because this report must print the same bytes twice over an
// unchanged database and an age against now() would move between the two runs. The same clock
// dates every write rate above it, and a document version write appends an event of its own,
// so the gap between the two is the reconciliation's own lag behind the writes it serves. A
// database that recorded no event at all has no clock, and the age prints as zero against
// `none`.
const backlogClockQuery = `select created_at from events order by id desc limit 1`

// BlockIndexBacklog is what the block-index reconciliation owes documents in this snapshot:
// how many are owed an index, when the oldest of those debts was incurred, and the clock the
// age is measured against.
type BlockIndexBacklog struct {
	Documents    int64
	OldestOwedAt time.Time
	ClockAt      time.Time
}

// Age is how long the oldest owed document has waited: from the write that owed it the index
// to the newest write the database recorded. Zero where nothing is owed, where the database
// recorded no write, and where the debt is newer than that write, which is the write that owed
// it arriving after the last event the clock read.
func (b BlockIndexBacklog) Age() time.Duration {
	if b.OldestOwedAt.IsZero() || b.ClockAt.IsZero() || !b.ClockAt.After(b.OldestOwedAt) {
		return 0
	}
	return b.ClockAt.Sub(b.OldestOwedAt)
}

// readBlockIndexBacklog reads what the block-index reconciliation owes documents and the clock
// its age is measured against. It refuses nothing: a backlog is a fact for the reader of the
// report and for the driver that watches it grow, and what counts as too large depends on the
// rate the database is written at, which the lines above this one carry.
func readBlockIndexBacklog(ctx context.Context, tx pgx.Tx) (BlockIndexBacklog, error) {
	var backlog BlockIndexBacklog
	var oldest *time.Time
	var markHeld bool
	if err := tx.QueryRow(ctx, backfillMarkHeldQuery).Scan(&markHeld); err != nil {
		return BlockIndexBacklog{}, fmt.Errorf("read whether the backfill's mark exists: %w", err)
	}
	query, args := blockIndexBacklogQuery, []any{refs.BlockIndexMarkingRule}
	if !markHeld {
		query, args = blockIndexBacklogBeforeTheMarkQuery, nil
	}
	if err := tx.QueryRow(ctx, query, args...).Scan(&backlog.Documents, &oldest); err != nil {
		return BlockIndexBacklog{}, fmt.Errorf("read the documents owed a block index: %w", err)
	}
	if oldest != nil {
		backlog.OldestOwedAt = *oldest
	}
	var clock *time.Time
	if err := tx.QueryRow(ctx, backlogClockQuery).Scan(&clock); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BlockIndexBacklog{}, fmt.Errorf("read the newest write the database recorded: %w", err)
	}
	if clock != nil {
		backlog.ClockAt = *clock
	}
	return backlog, nil
}

// backlogDatedBy says on the line itself what dates a document's debt, because no column of
// the schema does and a reader of this report would otherwise have to read the source to know
// what the age is measured from (blockIndexBacklogQuery).
const backlogDatedBy = "the created_at of the latest markdown version each owed document holds," +
	" the write that owed it the index, or the document's own created_at where it holds no version"
