package graphcensus

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// backfillComments writes a node and the facts its row is the projection of for every comment
// written before the store existed: the artifact it is anchored to and the artifact that owns it
// where it has them, and the project of the issue or the document it belongs to, or the parties
// of the conversation that owns that document (graphstore.CommentFacts, as api's commentStored states them), the base
// attributes graphstore.Write states for every node it makes - its visibility and its state, and
// its project or its parties where it belongs to one - and who wrote it, as
// graphstore.QueueAuthored states that (graphstore.AuthorFacts). A comment of a conversation
// between a party graphstore.PartiesValue refuses is skipped and counted in Skipped, so it stops
// neither its batch nor any later run. Each batch is p's, and p paces the pass (pacer).
func backfillComments(ctx context.Context, database *store.Store, p *pacer) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindComment}
	after := "00000000-0000-0000-0000-000000000000"
	p.begin(done.Kind)
	for {
		ids, causes, refused, err := readCommentRows(ctx, database, after, p.throttle.Batch)
		if err != nil {
			return done, err
		}
		if len(ids) == 0 {
			break
		}
		after = ids[len(ids)-1]
		writable := make([]string, 0, len(ids))
		writableCauses := make([]int64, 0, len(ids))
		for index, id := range ids {
			if refused[index] {
				done.Skipped++
				continue
			}
			writable, writableCauses = append(writable, id), append(writableCauses, causes[index])
		}
		if len(writable) > 0 {
			rows, nodes, tuples, err := writeCommentBatch(ctx, database, writable, writableCauses)
			if err != nil {
				return done, err
			}
			done.Rows += rows
			done.Nodes += nodes
			done.Tuples += tuples
		}
		if err := p.after(ctx, len(ids)); err != nil {
			return done, err
		}
		if len(ids) < p.throttle.Batch {
			break
		}
	}
	p.end()
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from comments c`+refs.CommentStoreJoins+`
		where c.node_kind is null and `+refs.CommentInTheStore+`
	`).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the comments still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readCommentRows reads the next batch of comments rows after the comment after, in id order,
// that name no node and whose relations fit the store's bound: each one's id, beside it the
// first comment.created event that names it, or the sentinel (graphstore.NoEvent) where no event
// does, which is the cause of what the batch states of it, and whether its node would be between
// a party graphstore.PartiesValue refuses (partiesRefused), which no writer writes
// (refs.CommentsNamingNoNode reads the rest of the row again under the owner locks)
// (CommentRowsStatement).
func readCommentRows(ctx context.Context, database *store.Store, after string, limit int) (ids []string, causes []int64, refused []bool, err error) {
	rows, err := database.Pool.Query(ctx, CommentRowsStatement, after, limit, graphstore.NoEvent)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the comments naming no node: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, session string
		var cause int64
		if err := rows.Scan(&id, &cause, &project, &session); err != nil {
			return nil, nil, nil, fmt.Errorf("read a comment naming no node: %w", err)
		}
		_, parties := graphstore.SessionScope(project, session)
		ids, causes, refused = append(ids, id), append(causes, cause), append(refused, partiesRefused(parties))
	}
	return ids, causes, refused, rows.Err()
}

// CommentRowsStatement is readCommentRows' selection: at most $2 of the comments naming no node
// whose relations fit the store's bound, past the comment $1, in id order, each with the first
// comment.created event that names it, or $3, the store's sentinel, where none does, and the
// project and the session its node would be scoped to.
//
// The event is read once per comment, and each such read probes events_comment_created_id
// (migration 20261119020800) by the comment, whose expression it compares as that index writes it,
// and whose predicate its own type clause proves: with no index to serve it, each one read the
// whole of events, so a run took time growing with the square of the data
// (TestTheBackfillsEventReadsProbeTheirIndexes).
var CommentRowsStatement = `
		select c.id::text,
		       coalesce((
			select e.id from events e
			where coalesce(e.issue_key, e.artifact_id::text, e.project_key)
			      = coalesce(c.issue_key, c.artifact_id::text)
			  and e.type = 'comment.created'
			  and e.payload->>'id' = c.id::text
			order by e.id limit 1
		       ), $3),
		       coalesce(issue.project_key, owner.project_key, ''), coalesce(owner.session_id, '')
		from comments c` + refs.CommentStoreJoins + `
		left join issues issue on issue.key = c.issue_key
		where c.node_kind is null and c.id > $1::uuid and ` + refs.CommentInTheStore + `
		order by c.id
		limit $2
	`

// writeCommentBatch writes one batch's comment nodes, the comments of ids each caused by the event
// beside it in causes (readCommentRows), the facts each row projects and who wrote each, in one
// transaction, and answers how many rows it gave a node, how many nodes it wrote and how many
// tuples it stated, as the upper bound the report states: an author's node another write already
// made moves nothing, and a tuple stated again under its event appends nothing. The count is the
// batch's own tuples and the base attributes the store write states for each comment node its
// insert makes, which the batch does not state itself - its visibility and its state, and its
// project or its parties where it belongs to one (refs.CommentWrite.BaseAttributes): every
// comment here is new, since a row that already names its node is never read.
//
// The batch is read before this transaction, and another run can mark a row before this run
// writes. So the transaction takes the owner lock of every comment of the batch first
// (refs.LockCommentOwners), the lock every writer of a comments row takes, and reads the rows
// again under it (refs.CommentsNamingNoNode), so a row a writer of the comment moved or named
// meanwhile, or another run marked, is written as it now stands, or left alone.
// Then it runs its writes back to back in one batch, holding a transaction id for those statements
// alone: every node of the batch goes in its one store write, the comments' and their authors',
// so the transaction takes every node row in ascending (kind, id) before it appends its first
// tuple, and names the nodes on the comments rows only after both: the order of the lock order on
// api/inbox_read.go's markInboxReadThreads. Who wrote a comment is what graphstore.QueueAuthored
// states of it (graphstore.AuthorFacts): the author's node, made at the first comment of the batch
// that names them, the comment's authored_by tuple and the aliases the server proved. The store
// write makes an author's node as QueueAuthored's arms make it and moves nothing on one that is
// already there, since every writer of a person or a session node writes it as AuthorFacts does:
// project-visible, live and carrying no attribute. The batch restates that node too, so a state
// or a visibility another write gave it stays.
func writeCommentBatch(ctx context.Context, database *store.Store, ids []string, causes []int64) (int64, int64, int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin the comment batch: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := refs.LockCommentOwners(ctx, tx, ids); err != nil {
		return 0, 0, 0, err
	}
	write, err := refs.CommentsNamingNoNode(ctx, tx, ids, causes)
	if err != nil {
		return 0, 0, 0, err
	}
	if write.Comments() == 0 {
		return 0, 0, 0, nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		write.Queue(b)
		return nil
	}); err != nil {
		return 0, 0, 0, fmt.Errorf("write the nodes of a comment batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit a comment batch: %w", err)
	}
	return int64(write.Comments()), int64(len(write.Nodes)), int64(len(write.Tuples)) + write.BaseAttributes(), nil
}
