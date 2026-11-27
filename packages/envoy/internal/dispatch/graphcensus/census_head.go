package graphcensus

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The head projection (migration 20261123180100_issue_tree_head): issue_head_nodes holds one row
// per node of an issue's tree as the store holds it (graphstore.HeadRowsSQL), and issue_head_marks
// names each issue the head read may read the rows of. Every writer of this build writes the rows of
// the nodes it writes (graphstore.QueueHeads); a write of a build that keeps none takes the marks of
// the issues it reaches away (issue_head_marks_drop). The fill (FillIssueHeads) writes every row of
// one issue no mark names from the store and marks it, which the backfill runs over every such issue
// (backfillIssueHeadNodes) and the block index's tick over a bounded batch each tick (docs'
// ReconcileBlockIndex); the census kind (CensusOfHeads) holds every row of a marked issue, every column of it, to
// what the store holds.

// headKind is the head projection's kind, as the census and the backfill report it.
const headKind = "issue_head_node"

// headBackfills are the backfill's pass over the issues no mark names (backfillIssueHeadNodes).
// It runs last, once every other pass has written the nodes and tuples it owes, so the rows it
// writes are of the store those passes leave.
var headBackfills = []backfillPass{oneKind(backfillIssueHeadNodes)}

// HeadFillBatch is how many issues one tick of the block index fills (FillIssueHeads): a tick
// fills a few issues and moves on, and an issue a write of an older build unmarked is filled again
// within a few ticks.
const HeadFillBatch = 20

// headFillLocked, which only a test sets, runs inside the fill of the issue key right after the
// fill takes the issue's head fill lock, so the test holds the fill there while a write of the
// release before starts (TestTheFillHoldsTheHeadFillLockAgainstAWriteOfTheReleaseBefore).
var headFillLocked func(key string)

// unmarkedIssuesStatement is up to $1 issues no mark names, in key order: the tick's list, from the
// first issue each time (FillIssueHeads).
const unmarkedIssuesStatement = `
	select i.key from issues i
	where not exists (select 1 from issue_head_marks mark where mark.issue_key = i.key)
	order by i.key collate "C"
	limit $1`

// unmarkedIssuesAfterStatement is up to $2 issues no mark names past the key $1, in the order of
// issues' primary key: the backfill's walk (backfillIssueHeadNodes). Each batch reads issues_pkey
// from the last batch's key on and probes issue_head_marks_pkey for each issue it reads, so a run
// reads each issue once however many batches it takes. Listed from the first issue each time, as the
// tick lists them, the n-th batch read every issue the batches before it had marked
// (TestEveryTopicPassWalksOneBatchThroughAnIndex).
const unmarkedIssuesAfterStatement = `
	select i.key from issues i
	where i.key > $1 and not exists (select 1 from issue_head_marks mark where mark.issue_key = i.key)
	order by i.key
	limit $2`

// headFillNodesStatement is every node the tree of the issue $1 can hold, whose rows its fill writes
// (graphstore.QueueHeads also writes the rows of each under another issue): the issue's own node;
// each node whose address begins with its key ($2, its LIKE pattern), a document's, a version's and
// a block's of its documents, and each block that ever stood in a document of the issue
// (in_document), whatever its address; its asks, comments and messages and the blocks of each; each
// node the issue's rows hold already; and under each of those that its walk enters - one of the
// issue's own, or one whose issue its parent gives - every node a part_of into it asserts, down to
// the leaves. A node of another issue placed under one of them is taken, as one the issue's walk
// answers and does not enter, and nothing under it is. The issue's rows sort their kinds and
// addresses byte by byte (collate "C"); the statement reads them at the default collation, as every
// other seed reads its own, so that each step's lookup of graph_tuples by the node it reached is a
// probe of that table's index (graph_tuples_as_of_in) rather than a scan of the table.
//
// The nodes whose address begins with the key are found from the issue's own rows and tuples, each
// held to the pattern: each document an artifacts row of the issue names, or a part_of into the
// issue's node from a document names, which every writer of a document's node states and which
// stays in the log of a document whose row a removal deleted, and each version and block a part_of
// or an in_document into one of those documents names, each a probe of an index. A LIKE of graph_nodes'
// ids would be no index range: the database's collation is not C (en_US in production), and a
// btree on it serves no LIKE prefix, so it read every document, version and block node the store
// holds for each issue it filled.
var headFillNodesStatement = `
	with recursive head_seeds (kind, id) as (
		select '` + graphstore.KindIssue + `', $1::text
		union
		select n.kind, n.id
		from (
			select a.ref_key from artifacts a where a.issue_key = $1
			union
			select t.from_id from graph_tuples t
			where t.to_kind = '` + graphstore.KindIssue + `' and t.to_id = $1 and t.kind = '` + graphstore.TuplePartOf + `'
			  and t.from_kind = '` + graphstore.KindArtifact + `'
		) document (ref_key)
		cross join lateral (
			select '` + graphstore.KindArtifact + `', document.ref_key
			union all
			select t.from_kind, t.from_id from graph_tuples t
			where t.to_kind = '` + graphstore.KindArtifact + `' and t.to_id = document.ref_key
			  and t.kind in ('` + graphstore.TuplePartOf + `', '` + refs.TupleInDocument + `')
			  and t.from_kind in ('` + graphstore.KindVersion + `', '` + refs.BlockNodeKind + `')
		) named (kind, id)
		join graph_nodes n on n.kind = named.kind and n.id = named.id
		where n.id like $2 escape '\'
		union
		select t.from_kind, t.from_id
		from artifacts a
		join graph_tuples t on t.to_kind = '` + graphstore.KindArtifact + `' and t.to_id = a.ref_key and t.kind = '` + refs.TupleInDocument + `'
		where a.issue_key = $1
		union
		select '` + graphstore.KindAsk + `', id::text from asks where issue_key = $1
		union
		select '` + graphstore.KindComment + `', id::text from comments where issue_key = $1
		union
		select '` + graphstore.KindMessage + `', id::text from messages where issue_key = $1
		union
		select '` + refs.ItemAsk.BlockKind() + `', b.node_id from ask_blocks b join asks a on a.id = b.ask_id where a.issue_key = $1
		union
		select '` + graphstore.KindCommentBlock + `', b.node_id from comment_blocks b join comments c on c.id = b.comment_id where c.issue_key = $1
		union
		select '` + refs.ItemMessage.BlockKind() + `', b.node_id from message_blocks b join messages m on m.id = b.message_id where m.issue_key = $1
		union
		select h.node_kind collate "default", h.node_address collate "default" from issue_head_nodes h where h.issue_key = $1
	), head_reached (kind, id, depth) as (
		select kind, id, 0 from head_seeds
		union
		select child.from_kind, child.from_id, reached.depth + 1
		from head_reached reached
		cross join lateral (
			select under.from_kind, under.from_id from (
				select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
				from graph_tuples t
				where t.to_kind = reached.kind and t.to_id = reached.id and t.kind = '` + graphstore.TuplePartOf + `'
				  and t.to_kind <> '` + graphstore.KindValue + `'
				order by t.from_kind, t.from_id, t.at desc, t.seq desc
			) under
			where under.asserted
		) child
		where reached.depth < 32
		  and (reached.kind = '` + graphstore.KindIssue + `' and reached.id = $1
		       or reached.kind not in ('` + graphstore.KindIssue + `', '` + graphstore.KindArtifact + `', '` + graphstore.KindVersion + `', '` + refs.BlockNodeKind + `')
		       or reached.id like '` + graphstore.KindLens + `/%'
		       or split_part(reached.id, '/', 1) = $1)
	)
	select distinct kind, id from head_reached`

// likePrefix is the LIKE pattern of every text that begins with prefix, its own wildcards escaped.
func likePrefix(prefix string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
}

// FillIssueHeads fills up to limit issues no mark names (fillIssueHeads), in key order, each in a
// transaction of its own, and answers how many it filled and whether more stood unmarked when it
// listed them. An issue a fill finds marked once it holds the issue's locks is left as it is, and so
// is one whose owner lock a writer holds, which a later tick fills.
func FillIssueHeads(ctx context.Context, database *store.Store, limit int) (filled int, more bool, err error) {
	rows, err := database.Pool.Query(ctx, unmarkedIssuesStatement, limit+1)
	if err != nil {
		return 0, false, fmt.Errorf("list the issues no head mark names: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, false, fmt.Errorf("list the issues no head mark names: %w", err)
	}
	if len(keys) > limit {
		keys, more = keys[:limit], true
	}
	filled, err = fillEachIssueHeads(ctx, database, keys)
	return filled, more, err
}

// fillEachIssueHeads fills each issue of keys (fillIssueHeads), each in a transaction of its own
// whose first statement has Postgres plan its statements at the tables' sizes as they run
// (graphstore.PlanAtCurrentSizes), and answers how many it filled.
func fillEachIssueHeads(ctx context.Context, database *store.Store, keys []string) (int, error) {
	filled := 0
	for _, key := range keys {
		var wrote bool
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			if err := graphstore.PlanAtCurrentSizes(ctx, tx); err != nil {
				return err
			}
			var err error
			wrote, err = fillIssueHeads(ctx, tx, key)
			return err
		}); err != nil {
			return filled, fmt.Errorf("fill the head rows of %s: %w", key, err)
		}
		if wrote {
			filled++
		}
	}
	return filled, nil
}

// fillIssueHeads writes in tx every row of the issue key's tree from what the store holds there
// (headFillNodesStatement, graphstore.WriteHeads) and marks the issue whole (issue_head_marks), and
// answers whether it did: under the issue's owner lock, which every writer of the issue holds, and
// then its head fill lock (store.LockHeadFill), which a write of a build that keeps no head rows
// takes before it takes the mark away, so such a write waits for this one and takes away the mark
// it commits, while this one waits for nothing such a write holds. The owner lock is tried, not
// waited for (store.TryLockOwners), as a background pass takes the owners nobody is writing: the
// fill waits for no writer of the issue either, and an issue a writer holds is left unmarked, its
// head read walking the tuples, until a later tick of the block index fills it. The mark is read
// again under both locks: a fill that ran meanwhile has written the rows and marked the issue
// already.
func fillIssueHeads(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	taken, err := store.TryLockOwners(ctx, tx, store.IssueOwner(key))
	if err != nil || len(taken) == 0 {
		return false, err
	}
	if err := store.LockHeadFill(ctx, tx, key); err != nil {
		return false, err
	}
	if headFillLocked != nil {
		headFillLocked(key)
	}
	var marked bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_head_marks where issue_key = $1)`, key).Scan(&marked); err != nil {
		return false, fmt.Errorf("read the head mark of %s: %w", key, err)
	}
	if marked {
		return false, nil
	}
	rows, err := tx.Query(ctx, headFillNodesStatement, key, likePrefix(key+"/"))
	if err != nil {
		return false, fmt.Errorf("read the nodes of the tree of %s: %w", key, err)
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.NodeKey, error) {
		var node graphstore.NodeKey
		return node, row.Scan(&node.Kind, &node.ID)
	})
	if err != nil {
		return false, fmt.Errorf("read the nodes of the tree of %s: %w", key, err)
	}
	if err := graphstore.WriteHeads(ctx, tx, nodes); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `insert into issue_head_marks (issue_key, filled_at) values ($1, clock_timestamp())`, key); err != nil {
		return false, fmt.Errorf("mark the head rows of %s whole: %w", key, err)
	}
	return true, nil
}

// backfillIssueHeadNodes fills every issue no mark names (fillEachIssueHeads), walking the issues a
// batch at a time in their primary key's order (unmarkedIssuesAfterStatement): the issues a store
// of the release before holds, whose writers kept no head rows, and each issue a write of such a
// release unmarked before the walk passed it. It reports the issues it filled as its rows, and as
// remaining the issues no mark names once the walk has ended: one a write of such a release
// unmarked behind the walk, and one whose owner lock a writer held as the walk reached it
// (fillIssueHeads), which the block index's tick fills again (FillIssueHeads).
func backfillIssueHeadNodes(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: headKind}
	after := ""
	for {
		rows, err := database.Pool.Query(ctx, unmarkedIssuesAfterStatement, after, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("list the issues no head mark names: %w", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return done, fmt.Errorf("list the issues no head mark names: %w", err)
		}
		if len(keys) == 0 {
			break
		}
		after = keys[len(keys)-1]
		filled, err := fillEachIssueHeads(ctx, database, keys)
		done.Rows += int64(filled)
		if err != nil {
			return done, err
		}
		if len(keys) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from issues i where not exists (select 1 from issue_head_marks mark where mark.issue_key = i.key)
	`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the issues no head mark names: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// headDigest is the md5 of every column of a head row but its key, read off the relation alias,
// whose columns are graphstore.HeadColumns.
func headDigest(alias string) string {
	columns := strings.Split(graphstore.HeadColumns, ",")
	for index, column := range columns {
		columns[index] = alias + "." + strings.TrimSpace(column)
	}
	return `md5(row(` + strings.Join(columns, ", ") + `)::text)`
}

// headSides is the two sides of the head projection's census, each a select of (issue_key,
// node_kind, node_address, digest): the rows issue_head_nodes holds, and the rows the store says it
// should hold (graphstore.HeadRowsSQL) of every node graph_nodes holds, every block of a comment
// (comment_blocks, whose nodes graph_nodes holds no row of) and every node a row names, the latter
// read at the default collation, as graph_nodes' are, so the store's side probes the store's
// indexes by them.
var headSides = `
	with head_stored as (
		select stored.issue_key, stored.node_kind, stored.node_address, ` + headDigest("stored") + ` as digest
		from issue_head_nodes stored
		where exists (select 1 from issue_head_marks mark where mark.issue_key = stored.issue_key)
	), head_expected as (
		select expected.issue_key, expected.kind as node_kind, expected.id as node_address, ` + headDigest("expected") + ` as digest
		from (` + graphstore.HeadRowsSQL(`select kind, id from graph_nodes
			union select '`+graphstore.KindCommentBlock+`', node_id from comment_blocks
			union select node_kind collate "default", node_address collate "default" from issue_head_nodes`) + `) expected
		where exists (select 1 from issue_head_marks mark where mark.issue_key = expected.issue_key)
	)`

// headNodeUnprojected is the cause of a row the store gives that issue_head_nodes holds none of,
// the one difference of the head census that is a node's (CensusOfHeads).
const headNodeUnprojected = "a node the issue's tree holds and no row projects"

// headDifferences is each row the two sides do not hold alike, as (cause, at): a row the table holds
// that the store gives no row of, or gives another, and a row the store gives that the table holds
// none of (headNodeUnprojected), each named by its issue and its node.
var headDifferences = headSides + `
	select case when expected.issue_key is null then 'a row of a node no issue''s tree holds, or of an issue that holds it no more'
	            else 'the row and the store disagree' end,
	       'issue ' || stored.issue_key || ' ' || stored.node_kind || ' ' || stored.node_address
	from head_stored stored
	left join head_expected expected on expected.issue_key = stored.issue_key
	  and expected.node_kind = stored.node_kind and expected.node_address = stored.node_address
	where expected.digest is distinct from stored.digest
	union all
	select ` + store.QuoteLiteral(headNodeUnprojected) + `,
	       'issue ' || expected.issue_key || ' ' || expected.node_kind || ' ' || expected.node_address
	from head_expected expected
	where not exists (
		select 1 from head_stored stored where stored.issue_key = expected.issue_key
		  and stored.node_kind = expected.node_kind and stored.node_address = expected.node_address)`

// CensusOfHeads is the census of the head projection, the kind issue_head_node of the report: every
// row issue_head_nodes holds of a marked issue, held column by column to the row the store gives of
// its node under its issue (graphstore.HeadRowsSQL), and every row the store gives of a marked issue,
// held to the table, so a row a writer left behind the store, a row of a node no tree holds, and a
// node of an issue's tree no row projects each read as a difference, named by its issue and its
// node. No row of an unmarked issue is compared, since the head read reads none. A deployment that
// holds no marked issue holds no row and gives none (EmptyAgrees): no head row of it is written yet,
// which is no refusal. It reads zero once the backfill has filled every issue and every writer since
// has kept the rows. The store census reads it as its last kind, and a test reads it in its own
// transaction.
func CensusOfHeads(ctx context.Context, tx pgx.Tx) (KindReport, error) {
	kind := KindReport{Kind: headKind, Table: "issue_head_nodes", EmptyAgrees: true,
		Note: "one row per node of a marked issue's tree, under each issue whose tree holds it; every column of each row" +
			" against the row the store gives of the node (graphstore.HeadRowsSQL), in both directions"}
	if err := tx.QueryRow(ctx, headSides+`
		select (select count(*) from head_stored), (select count(*) from head_expected)`).Scan(&kind.Rows, &kind.Nodes); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", headKind, err)
	}
	rows, err := tx.Query(ctx, headDifferences+`
		order by 2, 1`)
	if err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", headKind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var found Difference
		if err := rows.Scan(&found.Cause, &found.At); err != nil {
			return KindReport{}, fmt.Errorf("census of %s: %w", headKind, err)
		}
		if found.Cause == headNodeUnprojected {
			kind.NodeDifferences++
			if len(kind.ReportedNodes) < reportedDifferences {
				kind.ReportedNodes = append(kind.ReportedNodes, found)
			}
			continue
		}
		kind.RowDifferences++
		if len(kind.ReportedRows) < reportedDifferences {
			kind.ReportedRows = append(kind.ReportedRows, found)
		}
	}
	if err := rows.Err(); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", headKind, err)
	}
	return kind, nil
}
