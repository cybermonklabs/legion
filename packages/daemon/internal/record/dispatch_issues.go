package record

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
	// no reading has written it (0) or an older one did. The applied sequence rises to the
	// reading's own only for an applied reading, and only where it stands lower: an issue fact is
	// applied at a sequence the values may already hold, from a turn's re-read or a listing that
	// read the issue after the fact was published, so it is fenced apart from them. The where clause
	// skips a reading that would replace no value and raise no applied sequence, so the project and
	// the row's sequence move only with one of those.
	tag, err := tx.Exec(ctx, `insert into dispatch_issues as held (key, project, dispatch_seq,
			status, status_seq, parent, parent_seq, blockers, blockers_seq, labels, labels_seq, rank, rank_seq, applied_seq)
		values ($1, $2, $3::bigint,
			$4, case when $5 then $3 else 0 end,
			$6, case when $7 then $3 else 0 end,
			$8, case when $9 then $3 else 0 end,
			$10, case when $11 then $3 else 0 end,
			$12, case when $13 then $3 else 0 end,
			case when $14 then $3 else 0 end)
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
			rank_seq = case when $13 and (held.rank_seq = 0 or held.rank_seq < $3) then $3 else held.rank_seq end,
			applied_seq = greatest(held.applied_seq, excluded.applied_seq)
		where ($5 and (held.status_seq = 0 or held.status_seq < $3))
			or ($7 and (held.parent_seq = 0 or held.parent_seq < $3))
			or ($9 and (held.blockers_seq = 0 or held.blockers_seq < $3))
			or ($11 and (held.labels_seq = 0 or held.labels_seq < $3))
			or ($13 and (held.rank_seq = 0 or held.rank_seq < $3))
			or ($14 and held.applied_seq < $3)`,
		reading.Key, reading.Project, reading.Seq,
		status, reading.Status.Carried,
		parent, reading.Parent.Carried,
		blockers, reading.Blockers.Carried,
		labels, reading.Labels.Carried,
		rank, reading.Rank.Carried,
		reading.Applied)
	if err != nil {
		return fmt.Errorf("put the Dispatch copy of %s: %w", reading.Key, err)
	}
	// A reading the fences refuse whole writes no row, so it changes nothing a pass reads.
	noteRowsWritten(ctx, tag)
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
	if len(removed) > 0 {
		noteRecordWrite(ctx)
	}
	return removed, true, nil
}

func (s *Postgres) PutDispatchClaim(ctx context.Context, tx pgx.Tx, key, holder string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `insert into dispatch_claims (key, claimed_by, claim_seq) values ($1, $2, $3)
		on conflict (key) do update set claimed_by = excluded.claimed_by, claim_seq = excluded.claim_seq
		where dispatch_claims.claim_seq < excluded.claim_seq`,
		key, holder, seq)
	if err != nil {
		return fmt.Errorf("put the Dispatch claim of %s: %w", key, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) DispatchIssueApplied(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `select applied_seq from dispatch_issues where key = $1`, key).Scan(&seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read the sequence of the newest issue fact of %s the daemon applied: %w", key, err)
	}
	return seq, nil
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
// daemon's listing of its own project, and a child of another project is no child the second
// answers, as a parent of another project is no link of a chain (inProjectParentSQL).
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
//
// It answers the children of project alone: a child of another project is no open child of a
// wait, as a parent of another project is no link of that child's chain (inProjectParentSQL, R19
// of dispatch://INFRA-3320). The hold such a child puts on its parent's acceptance is Dispatch's
// (FOREIGN_CHILD_OPEN, in packages/envoy/internal/dispatch/api/issue_parent.go), not this wait's.
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

// ChainRows walks the parent chains a decision pass reads in the database, in one recursive
// statement whatever their number and depth: a read per link would
// cost a round trip per level on every pass (R16 of dispatch://INFRA-3320). The walk starts from
// every record of project not at phase done, which the issues_live index serves, every row of the
// project's copy at todo and each of extra, and carries each walk's path, so a key it has passed
// ends that walk however the copy loops, and its depth, so no walk climbs more than
// MaxChainLinks+1 links: one more than Dispatch allows, which is what tells a chain a stale row
// made too long from the longest Dispatch keeps. A link the copy holds no row of is read as a row
// that is not Present, which ends its walk, so a missing row is a fact of the answer rather than
// a gap in it. A parent of another project ends the walk as a parent of none does
// (inProjectParentSQL): the row reads with no parent, so its chain is known and ends there, and no
// walk climbs from a row of one project to a row of another. The walk is the copy's alone:
// nothing of the daemon's records decides a link.
//
// Each key is answered once, whichever walks reached it, with the fewest links above a start it
// stands at, 0 for a key a walk started from. The rows of every walk together are the chain of
// each start up to the bound, and the chain of each link above a start as far as those walks
// reached (Chains.Chain, ChainUnread).
func (s *Postgres) ChainRows(ctx context.Context, tx pgx.Tx, project string, extra []string) ([]ChainRow, error) {
	read, err := chainRows(ctx, tx, `select key from issues where project = $1 and phase <> 'done'
			union select key from dispatch_issues where project = $1 and status = 'todo'
			union select unnest($2::text[])`, project, extra)
	if err != nil {
		return nil, fmt.Errorf("read the chains of %s: %w", project, err)
	}
	return read, nil
}

// ChainRowsOf walks the chain of each of keys as ChainRows walks every chain of a pass, from keys
// alone: the statement reads each key's chain and no other row of the copy.
func (s *Postgres) ChainRowsOf(ctx context.Context, tx pgx.Tx, keys []string) ([]ChainRow, error) {
	read, err := chainRows(ctx, tx, `select unnest($1::text[])`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the chains of %v: %w", keys, err)
	}
	return read, nil
}

// inProjectParentSQL is the parent the walk climbs to from the copy's row d: d's parent where that
// parent is of d's own project, and null, a parent of none, where it is of another. It is the
// statement of the project boundary every chain the daemon reads from the copy stops at (R19 of
// dispatch://INFRA-3320): a parent of another project is no issue above, as Dispatch's own walk
// climbs only to a parent of the issue's project (legionWalkSQL, in
// packages/envoy/internal/dispatch/api/issue_parent.go). The parent's project is its row's where
// the copy holds one, and otherwise its key's: the part before the one `-` a Dispatch key holds
// (claim.KeyProject). It reads d alone, not the walk that reached it, so each key reads with one
// parent whichever walk reached it; and since no walk climbs past the boundary, every row a walk
// reaches is of the project of the row it started from. InProjectParent is the same rule in Go,
// for a parent the copy holds no link to.
const inProjectParentSQL = `case when coalesce((select p.project from dispatch_issues p where p.key = d.parent),
		split_part(d.parent, '-', 1)) = d.project then d.parent end`

// InProjectParent is parent as a link of key's chain: parent where its key is of key's own
// project (claim.KeyProject), and "", a parent of none, where it is of another. It is the chain
// walk's project boundary (inProjectParentSQL) for a parent the copy holds no link to: the old
// parent of a change of parent, whose link the copy has already replaced (workflow's reparent
// reads), and a parent Dispatch answers while the daemon reads a chain again (the daemon's
// rereadChain) or reads the chains a status call names (api's dispatchReads.readings). It reads
// the parent's key where the walk reads the parent's row first, and the two agree: Dispatch never
// moves an issue to another project, so a row's project is its key's.
func InProjectParent(key, parent string) string {
	if parent == "" || claim.KeyProject(parent) != claim.KeyProject(key) {
		return ""
	}
	return parent
}

// chainRows is the one walk ChainRows and ChainRowsOf make, from the keys starts selects with
// args, up each row's parent of its own project (inProjectParentSQL) to MaxChainLinks+1 links,
// each key once at the fewest links above a start it stands at.
func chainRows(ctx context.Context, tx pgx.Tx, starts string, args ...any) ([]ChainRow, error) {
	rows, err := tx.Query(ctx, `with recursive starts (key) as (`+starts+`
		), walk (key, depth, parent, status, labels, present, path) as (
			select s.key, 0, `+inProjectParentSQL+`, d.status, d.labels, d.key is not null, array[s.key]
			from starts s left join dispatch_issues d on d.key = s.key
			union all
			select w.parent, w.depth + 1, `+inProjectParentSQL+`, d.status, d.labels, d.key is not null, w.path || w.parent
			from walk w left join dispatch_issues d on d.key = w.parent
			where w.present and w.parent is not null and w.depth < `+strconv.Itoa(MaxChainLinks+1)+` and w.parent <> all(w.path)
		)
		select key, min(depth), coalesce(parent, ''), coalesce(status, ''), coalesce(labels, '{}'), present
		from walk group by key, parent, status, labels, present order by key`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChainRow, error) {
		var chained ChainRow
		err := row.Scan(&chained.Key, &chained.Depth, &chained.Parent, &chained.Status, &chained.Labels, &chained.Present)
		return chained, err
	})
}

// HasDispatchChildren answers whether the mirror holds a child of parent, with the project term
// DispatchChildren keeps, in one exists query.
func (s *Postgres) HasDispatchChildren(ctx context.Context, tx pgx.Tx, project, parent string) (bool, error) {
	var has bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from dispatch_issues where project = $1 and parent = $2)`,
		project, parent).Scan(&has); err != nil {
		return false, fmt.Errorf("read whether %s has a Dispatch child in %s: %w", parent, project, err)
	}
	return has, nil
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
