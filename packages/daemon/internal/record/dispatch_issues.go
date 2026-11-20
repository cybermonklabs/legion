package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) PutDispatchIssue(ctx context.Context, tx pgx.Tx, reading DispatchReading) error {
	if !reading.Status.Carried && !reading.Parent.Carried && !reading.Blockers.Carried &&
		!reading.Labels.Carried && !reading.Rank.Carried {
		return fmt.Errorf("put the Dispatch copy of %s: the reading at %d carries no value", reading.Key, reading.Seq)
	}
	// A zero sequence is no reading of Dispatch's at all: it is a fact the daemon minted from its
	// own record. Written, it would put a value the daemon means into the copy as Dispatch's, on a
	// new row or over a value no reading has written yet, which any reading fills (sequence 0).
	// Such a fact writes nothing, and the next event, the periodic turn or the next boot writes
	// what Dispatch says.
	if reading.Seq == 0 {
		return nil
	}
	// A value the reading does not carry goes into a new row as its column's default, and a nil
	// list it carries as the empty list, since neither list column takes null; a parent of none
	// is null.
	status, rank := carriedOr(reading.Status, ""), carriedOr(reading.Rank, "")
	blockers, labels := carriedOr(reading.Blockers, nil), carriedOr(reading.Labels, nil)
	if blockers == nil {
		blockers = []string{}
	}
	if labels == nil {
		labels = []string{}
	}
	parent := ParentOf(carriedOr(reading.Parent, ""))
	// Each value carried is inserted at the reading's sequence, and each one not carried at 0.
	// Over a row that exists, each value the reading carries changes, with its sequence, only where
	// no reading has written it (0) or an older one did; the where clause skips a reading that
	// changes no value, so the project and the row's sequence move only with a value.
	if _, err := tx.Exec(ctx, `insert into dispatch_issues as held (key, project, dispatch_seq,
			status, status_seq, parent, parent_seq, blockers, blockers_seq, labels, labels_seq, rank, rank_seq)
		values ($1, $2, $3::bigint,
			$4, case when $5 then $3 else 0 end,
			$6, case when $7 then $3 else 0 end,
			$8, case when $9 then $3 else 0 end,
			$10, case when $11 then $3 else 0 end,
			$12, case when $13 then $3 else 0 end)
		on conflict (key) do update set project = excluded.project,
			dispatch_seq = greatest(held.dispatch_seq, excluded.dispatch_seq),
			status = case when $5 and (held.status_seq = 0 or held.status_seq < $3) then excluded.status else held.status end,
			status_seq = case when $5 and (held.status_seq = 0 or held.status_seq < $3) then $3 else held.status_seq end,
			parent = case when $7 and (held.parent_seq = 0 or held.parent_seq < $3) then excluded.parent else held.parent end,
			parent_seq = case when $7 and (held.parent_seq = 0 or held.parent_seq < $3) then $3 else held.parent_seq end,
			blockers = case when $9 and (held.blockers_seq = 0 or held.blockers_seq < $3) then excluded.blockers else held.blockers end,
			blockers_seq = case when $9 and (held.blockers_seq = 0 or held.blockers_seq < $3) then $3 else held.blockers_seq end,
			labels = case when $11 and (held.labels_seq = 0 or held.labels_seq < $3) then excluded.labels else held.labels end,
			labels_seq = case when $11 and (held.labels_seq = 0 or held.labels_seq < $3) then $3 else held.labels_seq end,
			rank = case when $13 and (held.rank_seq = 0 or held.rank_seq < $3) then excluded.rank else held.rank end,
			rank_seq = case when $13 and (held.rank_seq = 0 or held.rank_seq < $3) then $3 else held.rank_seq end
		where ($5 and (held.status_seq = 0 or held.status_seq < $3))
			or ($7 and (held.parent_seq = 0 or held.parent_seq < $3))
			or ($9 and (held.blockers_seq = 0 or held.blockers_seq < $3))
			or ($11 and (held.labels_seq = 0 or held.labels_seq < $3))
			or ($13 and (held.rank_seq = 0 or held.rank_seq < $3))`,
		reading.Key, reading.Project, reading.Seq,
		status, reading.Status.Carried,
		parent, reading.Parent.Carried,
		blockers, reading.Blockers.Carried,
		labels, reading.Labels.Carried,
		rank, reading.Rank.Carried); err != nil {
		return fmt.Errorf("put the Dispatch copy of %s: %w", reading.Key, err)
	}
	return s.PutDispatchClaim(ctx, tx, reading.Key, reading.ClaimedBy, reading.ClaimSeq)
}

// carriedOr is the value a reading carries, or none when it does not carry one.
func carriedOr[T any](value Carried[T], none T) T {
	if value.Carried {
		return value.Value
	}
	return none
}

func (s *Postgres) TakeDispatchRefill(ctx context.Context, tx pgx.Tx, project string) ([]string, bool, error) {
	tag, err := tx.Exec(ctx, `update dispatch_issue_refills set refilled_at = now()
		where project = $1 and refilled_at is null`, project)
	if err != nil {
		return nil, false, fmt.Errorf("take the Dispatch copy's refill mark of %s: %w", project, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, false, nil
	}
	rows, err := tx.Query(ctx, `with removed as (delete from dispatch_issues where project = $1 returning key)
		select key from removed order by key`, project)
	if err != nil {
		return nil, false, fmt.Errorf("empty the Dispatch copy of %s: %w", project, err)
	}
	removed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, fmt.Errorf("empty the Dispatch copy of %s: %w", project, err)
	}
	return removed, true, nil
}

func (s *Postgres) PutDispatchClaim(ctx context.Context, tx pgx.Tx, key, holder string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into dispatch_claims (key, claimed_by, claim_seq) values ($1, $2, $3)
		on conflict (key) do update set claimed_by = excluded.claimed_by, claim_seq = excluded.claim_seq
		where dispatch_claims.claim_seq < excluded.claim_seq`,
		key, holder, seq); err != nil {
		return fmt.Errorf("put the Dispatch claim of %s: %w", key, err)
	}
	return nil
}

// DispatchIssueSeq is the highest sequence of any reading of key the copy took, and whether it
// holds a row of key: every issue event the daemon applies writes its row first (blockers.Mirror).
func (s *Postgres) DispatchIssueSeq(ctx context.Context, tx pgx.Tx, key string) (int64, bool, error) {
	var seq int64
	err := tx.QueryRow(ctx, `select dispatch_seq from dispatch_issues where key = $1`, key).Scan(&seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read the Dispatch mirror sequence of %s: %w", key, err)
	}
	return seq, true, nil
}

func (s *Postgres) DispatchClaim(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	var holder string
	err := tx.QueryRow(ctx, `select claimed_by from dispatch_claims where key = $1`, key).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the Dispatch claim of %s: %w", key, err)
	}
	return holder, nil
}

func (s *Postgres) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]DispatchIssueRow, error) {
	mirrored, err := queryDispatchIssues(ctx, tx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0), coalesce(i.parent, ''), i.labels, i.rank
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.project = $1 order by i.key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	return mirrored, nil
}

// DispatchIssuesOf reads the keys a judgement names and every key their rows wait on in one
// statement: the set is built first, from the keys and the blockers of their rows, and the rows are
// then read by primary key, so the read never scans the project's mirror.
//
// It names no project at all: a key the judgement asks for is read whatever project it is in,
// since an issue may wait on an issue of another project and a blocker whose row the judgement
// cannot read counts as unfinished for ever (blockers.Blocked), which would hold its slot back
// for good. The table is keyed by the issue alone, so a key names one row and project is the
// row's own. DispatchIssues and DispatchChildren keep their project term: the first is the
// daemon's listing of its own project, and a parent link cannot cross a project, so a child of
// one is of that project too.
func (s *Postgres) DispatchIssuesOf(ctx context.Context, tx pgx.Tx, keys []string) ([]DispatchIssueRow, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	mirrored, err := queryDispatchIssues(ctx, tx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0), coalesce(i.parent, ''), i.labels, i.rank
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.key = any(array(
			select unnest($1::text[])
			union select unnest(judged.blockers) from dispatch_issues judged where judged.key = any($1)))
		order by i.key`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the Dispatch mirror of %v: %w", keys, err)
	}
	return mirrored, nil
}

// DispatchChildren groups the mirror's children of each of parents under the parent they name. A
// later step reads it to decide whether a parent's own first phase waits for a child of its own
// that is still open, which is a question about every child Dispatch knows of, recorded here or
// not, so it is the mirror that answers it rather than the daemon's records.
func (s *Postgres) DispatchChildren(ctx context.Context, tx pgx.Tx, project string, parents []string) (map[string][]DispatchIssueRow, error) {
	children := make(map[string][]DispatchIssueRow, len(parents))
	if len(parents) == 0 {
		return children, nil
	}
	mirrored, err := queryDispatchIssues(ctx, tx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0), coalesce(i.parent, ''), i.labels, i.rank
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.project = $1 and i.parent = any($2) order by i.key`, project, parents)
	if err != nil {
		return nil, fmt.Errorf("read the Dispatch children of %v in %s: %w", parents, project, err)
	}
	for _, row := range mirrored {
		children[row.Parent] = append(children[row.Parent], row)
	}
	return children, nil
}

// queryDispatchIssues is every row of the copy query selects with args, in its order; an empty
// list of blockers or labels reads as an empty list.
func queryDispatchIssues(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]DispatchIssueRow, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mirrored []DispatchIssueRow
	for rows.Next() {
		var row DispatchIssueRow
		if err := rows.Scan(&row.Key, &row.Project, &row.Status, &row.Blockers, &row.Seq, &row.ClaimedBy, &row.ClaimSeq, &row.Parent, &row.Labels, &row.Rank); err != nil {
			return nil, err
		}
		mirrored = append(mirrored, row)
	}
	return mirrored, rows.Err()
}
