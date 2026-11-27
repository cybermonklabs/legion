package textversions

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The first-body repair is an update path of the keyword table beside the fill's own writes: its
// insert of each new version, and its delete of every row of an event log that was reset.
//
// A fill of the release before this one decides a comment.created version whose event cut its
// body by that release's reader, which takes revision 1 of the comment's blocks as the comment's
// own only where each block's row holds the event's created_at, the transaction's start, as its
// own created_at. This release dates a block's row by the comment's own moment (refs'
// queueBlocks), so that fill, holding the hold during a roll or after a rollback, keeps the event's
// cut text for a long comment this release wrote and an edit changed before the fill reached its
// event, and no later batch decides that event again. The repair decides each such version again
// by this release's reader (firstRevisionBody): a comment.created version marked cut whose
// revision 1 holds a text in every block and proves, block by block, the comment's own
// transaction (firstRevisionWrittenAt) takes revision 1's body, from blocks, and that body's
// vector, as this release's fill would have written it. A version no source holds, revision 1 of
// which another transaction wrote, fails the same test and stays cut.

// repairWindow bounds the cut comment versions one batch of the repair reads, in key order.
const repairWindow = 1000

// rebuiltMessage is the line the repair logs of each version it rebuilds, and repairedMessage the
// line of a walk that rebuilt any.
const (
	rebuiltMessage  = "dispatch: the keyword fill's first-body repair rebuilt a comment's cut first body from revision 1 of its blocks"
	repairedMessage = "dispatch: the keyword fill's first-body repair walked the cut comment versions"
)

// cutCommentVersions is the test of a comment version of the keyword table, aliased alias, marked
// cut: the predicate of the partial index search_text_versions_cut_comments (migration
// 20261122100004), which holds those versions alone, keyed by (id, event_seq). The repair's read
// and the census's count each name it, so the planner reads them through that index, and a table
// that holds no cut comment version costs each of them one read of an empty index.
func cutCommentVersions(alias string) string {
	return alias + `.kind = 'comment' and ` + alias + `.body_from = '` + bodyFromCut + `'`
}

// repairWindowQuery reads one repair batch's window: the comment versions of the keyword table
// marked cut (cutCommentVersions) after the key ($1, $2), in (id, event_seq) order through the
// partial index that holds them, at most $3 of them, each with its event_seq and event_id, and,
// of a version a comment.created of the same comment started, that event's created_at, null for
// every other version.
var repairWindowQuery = `
	select v.id, v.event_seq, v.event_id, e.created_at
	  from (select s.id, s.event_seq, s.event_id
	          from public.search_text_versions s
	         where ` + cutCommentVersions("s") + ` and (s.id, s.event_seq) > ($1::text, $2::integer)
	         order by s.id, s.event_seq
	         limit $3) v
	  left join public.events e on e.id = v.event_id
	        and e.type = 'comment.created' and e.payload->>'id' = v.id
	 order by v.id, v.event_seq`

// repairFirstBodyQuery is the repair's one write: comment version ($1, $2), started by event $3
// and still marked cut, takes the body $4, rebuilt from revision 1 of its blocks, from blocks, and
// that body's vector (postVectorSQL), as the fill's insert would have made it.
const repairFirstBodyQuery = `
	update public.search_text_versions v
	   set body = r.body, body_from = '` + bodyFromBlocks + `', search = ` + postVectorSQL + `
	  from (select $4::text as body) r
	 where v.kind = 'comment' and v.id = $1 and v.event_seq = $2 and v.event_id = $3
	   and v.body_from = '` + bodyFromCut + `'`

// ProvenCutFirstBodiesQuery counts the versions the first-body repair rebuilds: the comment.created
// versions of the keyword table marked cut whose revision 1 holds a text in every block and was
// written, block by block, by the transaction the event's created_at is the start of
// (firstRevisionWrittenAt), as firstRevisionBody, the reader the repair decides by, requires. It
// reads the comment versions marked cut alone, through their partial index (cutCommentVersions).
// A version id that is no uuid names no comment, so it counts nothing. The store census reads it
// (graphcensus's StoreReport.KeywordCutFirstBodies), and once the repair has run it reads 0.
var ProvenCutFirstBodiesQuery = `
	select count(*)
	  from public.search_text_versions v
	  join public.events e on e.id = v.event_id and e.type = 'comment.created' and e.payload->>'id' = v.id
	 where ` + cutCommentVersions("v") + `
	   and exists (select ` + firstRevisionWrittenAt + ` ` + firstRevisionRows(versionComment) + `)
	   and not exists (select ` + firstRevisionWrittenAt + ` ` + firstRevisionRows(versionComment) + `
	                    and (t.text is null or ` + firstRevisionWrittenAt + ` is distinct from e.created_at))`

// versionComment is the comment a comment version v names, as a uuid, and null where its id is no
// uuid (store.CastableUUID).
var versionComment = store.CastableUUID(`v.id`)

// repairKey is where the repair's walk stands: the key of the last cut comment version it read.
type repairKey struct {
	id  string
	seq int32
}

// repairResult is what one repair batch did: how many cut comment versions it rebuilt, the key the
// next batch reads after, whether the walk is over, and how long the batch ran.
type repairResult struct {
	rebuilt int
	next    repairKey
	done    bool
	took    time.Duration
}

// repairCutFirstBodies walks the keyword table's comment versions marked cut in (id, event_seq)
// order, from the first, through the partial index that holds them alone (cutCommentVersions), in
// bounded batches (repairBatch), each of which rebuilds the first bodies revision 1 proves among
// the comment.created versions of its window, until a batch reads the last cut comment version; it
// pauses before each batch and rests after it as long as it ran, as the fill's batches do. So a
// walk reads the cut comment versions and no other row of the table, and over a table that holds
// none it is one batch, whose window is one read of an empty index. It answers how many versions
// it rebuilt. It is idempotent: a version it rebuilt holds blocks, which takes it out of the index,
// and a cut version it does not rebuild stays where the next walk reads it and leaves it again.
//
// The fill's runner walks it each time it takes the hold, after its first pass, and again after
// each later pass until a walk succeeds (runner.fillUnder): a version the repair concerns is
// written only by a fill of the release before this one, which writes only while it holds the
// hold, so this build takes the hold after it. The walk follows the pass so the pass reads the
// event log's horizon as soon as the hold is taken, never behind a repair batch's wait at the
// cursor row (runner.fillUnder says why).
func (f *filler) repairCutFirstBodies(ctx context.Context) (int, error) {
	key := repairKey{seq: math.MinInt32}
	rebuilt := 0
	for {
		if f.pause != nil {
			if err := f.pause(ctx); err != nil {
				return rebuilt, err
			}
		}
		result, err := f.repairBatch(ctx, key)
		rebuilt += result.rebuilt
		if err != nil {
			return rebuilt, err
		}
		if result.done {
			break
		}
		if err := f.rest(ctx, result.took); err != nil {
			return rebuilt, err
		}
		key = result.next
	}
	if rebuilt > 0 {
		f.logger.Info(repairedMessage, "rebuilt", rebuilt)
	}
	return rebuilt, nil
}

// repairBatch is one batch of the repair, in one READ COMMITTED transaction under the fill's own
// locks, taken as a batch of the fill takes them (lockBatch): its bounds, the cursor row `for update`
// (creating it at no event where none is), so it writes between two batches and never beside one;
// under a hold, the check that the hold is still this holder's (duty.ErrHoldLost otherwise); and
// the event log against the cursor, a log reset under it ending the walk with nothing written,
// since the next batch empties the table. It reads the window of cut comment versions after after
// (repairWindowQuery) and, of each comment.created version in it, rebuilds the first body where
// revision 1 proves the comment's own transaction (firstRevisionBody, repairFirstBodyQuery). Once
// it has rebuilt bodies of f.maxBytes or more, the batch ends at the version it rebuilt last, and
// the next batch reads after that one.
func (f *filler) repairBatch(ctx context.Context, after repairKey) (repairResult, error) {
	started := time.Now()
	ctx = store.WithTransactionTracking(ctx)
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return repairResult{}, fmt.Errorf("keyword fill: begin a first-body repair batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := f.lockBatch(ctx, tx, "first-body repair batch")
	if err != nil {
		return repairResult{}, err
	}
	if locked.reset {
		return repairResult{done: true, took: time.Since(started)}, nil
	}
	window, err := readRepairWindow(ctx, tx, after, f.repairWindow)
	if err != nil {
		return repairResult{}, err
	}
	result := repairResult{next: after, done: len(window) < f.repairWindow}
	var rebuilt []repairVersion
	var bytes int64
	for index, v := range window {
		result.next = repairKey{id: v.id, seq: v.seq}
		if v.createdAt == nil || !isUUID(v.id) {
			continue
		}
		body, ok, err := firstRevisionBody(ctx, tx, v.id, *v.createdAt)
		if err != nil {
			return repairResult{}, fmt.Errorf("keyword fill: first-body repair: %w", err)
		}
		if !ok {
			continue
		}
		tag, err := tx.Exec(ctx, repairFirstBodyQuery, v.id, v.seq, v.eventID, body)
		if err != nil {
			return repairResult{}, fmt.Errorf("keyword fill: rebuild the first body of comment %s from revision 1: %w", v.id, err)
		}
		if tag.RowsAffected() == 1 {
			rebuilt = append(rebuilt, v)
		}
		if bytes += int64(len(body)); bytes >= f.maxBytes && index < len(window)-1 {
			result.done = false
			break
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return repairResult{}, fmt.Errorf("keyword fill: commit the first-body repair batch: %w", err)
	}
	for _, v := range rebuilt {
		f.logger.Info(rebuiltMessage, "comment", v.id, "event_id", v.eventID, "event_seq", v.seq)
	}
	result.rebuilt, result.took = len(rebuilt), time.Since(started)
	return result, nil
}

// repairVersion is one comment version of a repair batch's window: its key, the event that
// started it, and that event's created_at where it is a comment.created version marked cut.
type repairVersion struct {
	id        string
	seq       int32
	eventID   int64
	createdAt *time.Time
}

// readRepairWindow reads the window of at most limit comment versions after after
// (repairWindowQuery).
func readRepairWindow(ctx context.Context, tx pgx.Tx, after repairKey, limit int) ([]repairVersion, error) {
	rows, err := tx.Query(ctx, repairWindowQuery, after.id, after.seq, limit)
	if err != nil {
		return nil, fmt.Errorf("keyword fill: read the comment versions after %s at %d: %w", after.id, after.seq, err)
	}
	window, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repairVersion, error) {
		var v repairVersion
		err := row.Scan(&v.id, &v.seq, &v.eventID, &v.createdAt)
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("keyword fill: read the comment versions after %s at %d: %w", after.id, after.seq, err)
	}
	return window, nil
}

// QueryRower is what CountProvenCutFirstBodies reads through: a transaction, a connection or a
// pool.
type QueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CountProvenCutFirstBodies counts, through q, the cut first bodies the repair rebuilds
// (ProvenCutFirstBodiesQuery).
func CountProvenCutFirstBodies(ctx context.Context, q QueryRower) (int64, error) {
	var count int64
	if err := q.QueryRow(ctx, ProvenCutFirstBodiesQuery).Scan(&count); err != nil {
		return 0, fmt.Errorf("keyword fill: count the cut first bodies revision 1 proves: %w", err)
	}
	return count, nil
}
