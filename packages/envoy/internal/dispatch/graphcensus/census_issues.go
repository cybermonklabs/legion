package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueProjections are the census's kinds of the relations between issues (projections), each
// held triple by triple (projection.tuples) against the tuples the issue's node states now
// (dispatch://INFRA-1253 R9, R10): issues.parent_key against the issue's child_of, and each
// issue_links row against the needs of its pair. A row that names no node is a row a process of
// the release before wrote, which graph_edges' old arm answers until the backfill names its node
// (the object pass for an issue's own row, backfillIssueLinks for a link), and which the census
// counts as a class of its own. The project of each issue's node is the project census's
// (ProjectCensus).
var issueProjections = []projection{{
	name:     "issue_parent",
	nodeKind: graphstore.KindIssue,
	table:    "issues.parent_key",
	note: "an issue's parent: the row side is every marked issues row with a parent as the triple of its node," +
		" child_of and its parent issue, against every child_of a present issue node states now",
	rows: `
		select 'issue ' || i.key, i.node_kind, i.node_address, '` + graphstore.TupleChildOf + `', '` + graphstore.KindIssue + `', i.parent_key
		from issues i
		where i.node_kind is not null and i.parent_key is not null`,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleChildOf+`'`, graphstore.KindIssue),
	unmarked: `
		select count(*) from issues i where i.node_kind is null and i.parent_key is not null`,
	emptyAgrees: true,
}, {
	name:     "issue_link",
	nodeKind: graphstore.KindIssue,
	table:    "issue_links",
	note: "an issue's waits: the row side is every marked issue_links row as the triple of its issue's node," +
		" needs and the issue it waits on, against every needs to an issue a present issue node states now",
	rows: `
		select 'issue_link ' || l.issue_key || ' ' || l.target_key, l.node_kind, l.node_address,
		       '` + graphstore.TupleNeeds + `', '` + graphstore.KindIssue + `', l.target_key
		from issue_links l
		where l.node_kind is not null and l.kind = 'blocked_by'`,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleNeeds+`' and t.to_kind = '`+graphstore.KindIssue+`'`, graphstore.KindIssue),
	unmarked: `
		select count(*) from issue_links l where l.node_kind is null and l.kind = 'blocked_by'`,
	emptyAgrees: true,
}}

// issueBackfills are the backfill's passes over the relations between issues (backfillPasses),
// after the project pass, whose object pass gave every issue row its node and stated its
// child_of with it (backfillObjectNodes): the waits a process of the release before wrote
// (backfillIssueLinks).
var issueBackfills = []backfillPass{oneKind(backfillIssueLinks)}

// issueLinkRow is one issue_links row a process of the release before wrote, naming no node.
type issueLinkRow struct {
	issue, target string
	createdAt     time.Time
}

// backfillIssueLinks states, for every issue_links row naming no node, the needs tuple it is the
// projection of, dated at the row's own created_at and at the sentinel, as graph_edges' old arm
// dates the edge, and names the issue's node on the row in the transaction that states the tuple,
// so a second run reads no row the first one wrote (dispatch://INFRA-1149's r-view-end contract).
// It walks the rows in their primary key's order, one batch a transaction (writeIssueLinkBatch).
func backfillIssueLinks(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "issue_link"}
	var afterIssue, afterTarget string
	for {
		rows, err := database.Pool.Query(ctx, `
			select issue_key, target_key, created_at from issue_links
			where kind = 'blocked_by' and node_kind is null and (issue_key, target_key) > ($1, $2)
			order by issue_key, target_key
			limit $3
		`, afterIssue, afterTarget, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the issue links naming no node: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (issueLinkRow, error) {
			var link issueLinkRow
			err := row.Scan(&link.issue, &link.target, &link.createdAt)
			return link, err
		})
		if err != nil {
			return done, fmt.Errorf("read the issue links naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		afterIssue, afterTarget = batch[len(batch)-1].issue, batch[len(batch)-1].target
		written, err := writeIssueLinkBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Tuples += written
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from issue_links where kind = 'blocked_by' and node_kind is null
	`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the issue links still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeIssueLinkBatch states one batch's needs tuples and names their issue's node on each row,
// in one transaction and in the store's lock order (api/inbox_read.go's markInboxReadThreads):
// the issue rows the batch's links stand under first, each once, in ascending key
// (events.LockOwnerRows), then the batch's rows read again under them, then the tuples
// (graphstore.Write, which writes no node: each issue's node is the object pass's), then the rows
// that name the node. The batch was read outside this transaction, so a link the issue's own
// write deleted or named meanwhile, under the issue's row, is not read again and states nothing;
// nor is a link of an issue whose own row names no node yet, which the object pass reaches first
// on its next run. It answers how many rows it named, one tuple each.
func writeIssueLinkBatch(ctx context.Context, database *store.Store, batch []issueLinkRow) (int64, error) {
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		issues := make([]string, len(batch))
		targets := make([]string, len(batch))
		for index, link := range batch {
			owners.Add(link.issue, "", "issue "+link.issue)
			issues[index], targets[index] = link.issue, link.target
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select l.issue_key, l.target_key, l.created_at
			from issue_links l
			join unnest($1::text[], $2::text[]) as batch (issue_key, target_key)
			  on batch.issue_key = l.issue_key and batch.target_key = l.target_key
			join issues i on i.key = l.issue_key and i.node_kind is not null
			where l.kind = 'blocked_by' and l.node_kind is null
			order by l.issue_key, l.target_key
		`, issues, targets)
		if err != nil {
			return fmt.Errorf("read a batch of issue links again under their issues: %w", err)
		}
		held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (issueLinkRow, error) {
			var link issueLinkRow
			err := row.Scan(&link.issue, &link.target, &link.createdAt)
			return link, err
		})
		if err != nil {
			return fmt.Errorf("read a batch of issue links again under their issues: %w", err)
		}
		if len(held) == 0 {
			return nil
		}
		tuples := make([]graphstore.Tuple, len(held))
		heldIssues := make([]string, len(held))
		heldTargets := make([]string, len(held))
		for index, link := range held {
			tuples[index] = graphstore.IssueNeeds(link.issue, link.target, true, backfillActor, link.createdAt)
			heldIssues[index], heldTargets[index] = link.issue, link.target
		}
		if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update issue_links l set node_kind = $3, node_address = l.issue_key
			from unnest($1::text[], $2::text[]) as batch (issue_key, target_key)
			where l.issue_key = batch.issue_key and l.target_key = batch.target_key and l.kind = 'blocked_by'
		`, heldIssues, heldTargets, graphstore.KindIssue); err != nil {
			return err
		}
		written = int64(len(held))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of issue links: %w", err)
	}
	return written, nil
}
