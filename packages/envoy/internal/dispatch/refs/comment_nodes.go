package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// CommentStoreJoins are the two artifacts a comment's relations name, joined so that
// CommentInTheStore can read their addresses: the document its anchor names and the artifact
// that owns it. It follows `from comments c`.
const CommentStoreJoins = `
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		left join artifacts owner on owner.id = c.artifact_id`

// CommentInTheStore is true of a comments row the store holds a node for: one whose anchor's
// and owner's addresses both fit the store's bound (graphstore.MaxAddressBytes;
// CommentStoreJoins names the two). A comment with a longer one is left whole out of the store -
// no node, no tuple and no stamp, so that the old comments arm answers both of its edges (api's
// commentStored) - and the backfill leaves it alone for the same reason. So no such row names an
// author the store is held to a node for, and every side that reads the people and the sessions
// comments name binds this.
var CommentInTheStore = `(anchor.ref_key is null or octet_length(anchor.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + `)
		  and (owner.ref_key is null or octet_length(owner.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + `)`

// commentRow is one comments row the comment write reads (CommentsNamingNoNode): its id, its
// author as the row stored it, when it was written, the addresses of the artifact it is anchored
// to and the artifact that owns it, the event that stored it, and the project of the issue or the
// document it belongs to, or the session of the conversation that owns that document
// (graphstore.ScopeOf's rule).
type commentRow struct {
	id        string
	author    model.Actor
	createdAt time.Time
	anchor    string
	owner     string
	event     int64
	project   string
	parties   []string
}

// CommentsNamingNoNode is the backfill's write (CommentWrite) of each comment of ids whose row
// names no node, read through q, for the block-index tick's write of the comments the events after
// its cursor name (docs' readEventsAfterCursor). Beside each id, causes holds the event that
// stored the comment, the comment.created or comment.answered the tick read, which is the cause of
// what the write states, as graphcensus's readCommentRows reads it for the backfill's own pass: the
// tick has it in hand, so it reads no event of the log to find it. A row that names its node
// already is left alone, as the backfill leaves it, and so is a comment one of whose relations is
// past the store's address bound (CommentInTheStore), which no writer gives a node. The caller
// reads it under the owner locks of those comments (LockCommentOwners), which it takes before, so
// what it reads is what it writes.
func CommentsNamingNoNode(ctx context.Context, q graphstore.Queryer, ids []string, causes []int64) (CommentWrite, error) {
	rows, err := q.Query(ctx, `
		select c.id::text, c.author, c.created_at,
		       coalesce(anchor.ref_key, ''), coalesce(owner.ref_key, ''), named.cause,
		       coalesce(issue.project_key, owner.project_key, ''), coalesce(owner.session_id, '')
		from unnest($1::uuid[], $2::bigint[]) as named (id, cause)
		join comments c on c.id = named.id`+CommentStoreJoins+`
		left join issues issue on issue.key = c.issue_key
		where c.node_kind is null and `+CommentInTheStore+`
		order by c.id
	`, ids, causes)
	if err != nil {
		return CommentWrite{}, fmt.Errorf("read the comments the events name that name no node: %w", err)
	}
	batch, err := scanCommentRows(rows)
	if err != nil {
		return CommentWrite{}, err
	}
	return commentWriteOf(batch), nil
}

// LockCommentOwners takes the owner lock of every comment of ids (store.LockOwners): its issue's,
// or the document's that owns a comment on a document no issue owns, the lock every writer of a
// comments row takes before it reads what it writes. A comment's owner never moves, so the owners
// are read plainly first and the rows a writer goes on to write are read again under the locks.
// A comment no row holds names no owner.
func LockCommentOwners(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select distinct issue_key, artifact_id::text from comments where id = any($1::uuid[])
	`, ids)
	if err != nil {
		return fmt.Errorf("read the owners of %d comments: %w", len(ids), err)
	}
	var owners []store.Owner
	for rows.Next() {
		var issueKey, artifactID *string
		if err := rows.Scan(&issueKey, &artifactID); err != nil {
			rows.Close()
			return fmt.Errorf("read the owner of a comment: %w", err)
		}
		if owner := store.OwnerOf(issueKey, artifactID); owner != nil {
			owners = append(owners, *owner)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the owners of %d comments: %w", len(ids), err)
	}
	return store.LockOwners(ctx, tx, owners...)
}

// scanCommentRows reads every row of rows, a read of commentRow's columns in its order, and
// closes it.
func scanCommentRows(rows pgx.Rows) ([]commentRow, error) {
	defer rows.Close()
	var batch []commentRow
	for rows.Next() {
		var row commentRow
		var author []byte
		var session string
		if err := rows.Scan(&row.id, &author, &row.createdAt, &row.anchor, &row.owner, &row.event, &row.project, &session); err != nil {
			return nil, fmt.Errorf("read a comment naming no node: %w", err)
		}
		if err := json.Unmarshal(author, &row.author); err != nil {
			return nil, fmt.Errorf("read the author of comment %s: %w", row.id, err)
		}
		row.project, row.parties = graphstore.SessionScope(row.project, session)
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// CommentWrite is the backfill's write of a batch of comments rows that name no node, built once
// (commentWriteOf) for both of its writers: the backfill's own pass (graphcensus's
// writeCommentBatch) and the block-index tick's write of the comments the events after its cursor
// name (docs' readEventsAfterCursor). Nodes and Tuples are what the writer states in its one store
// write (graphstore.QueueWrite): each comment's node and the facts its row projects
// (graphstore.CommentFacts), and who wrote it (graphstore.AuthorFacts). Queue then names each
// comment's node on its row.
type CommentWrite struct {
	Nodes  []graphstore.Node
	Tuples []graphstore.Tuple
	// ids are the comments the write names the nodes of. A comment's node address is its own id.
	ids []string
	// baseAttributes is how many base attributes the store write states of the comment nodes it
	// makes: each one's visibility and state, and its project or its parties where it belongs to
	// one.
	baseAttributes int64
}

// Comments is how many comments rows the write names the nodes of.
func (w CommentWrite) Comments() int { return len(w.ids) }

// BaseAttributes is how many base attributes the store write states of the comment nodes it
// makes, which its writer counts beside its own Tuples: no comment here names its node yet, so
// the write makes every one.
func (w CommentWrite) BaseAttributes() int64 { return w.baseAttributes }

// Queue queues the write on b: its one store write of every node and every tuple
// (graphstore.QueueWrite), and then the node named on each comment's row, which has to follow the
// store write of the nodes: the row's foreign key names the node.
func (w CommentWrite) Queue(b *store.WriteBatch) {
	graphstore.QueueWrite(b, w.Nodes, w.Tuples)
	b.Queue("name the nodes of a comment batch on their rows", `
		update comments c
		set node_kind = $3, node_address = p.address
		from unnest($1::text[], $2::text[]) as p (id, address)
		where c.id = p.id::uuid
	`, w.ids, w.ids, graphstore.KindComment)
}

// commentWriteOf builds the write of one batch of comments rows (CommentWrite). One write writes
// a node once, so an author several comments of the batch name is made by the first of them.
func commentWriteOf(batch []commentRow) CommentWrite {
	write := CommentWrite{Nodes: make([]graphstore.Node, 0, 2*len(batch)), ids: make([]string, 0, len(batch))}
	written := map[[2]string]struct{}{}
	for _, row := range batch {
		// A comment any of whose relation ends is past the store's address bound is left whole
		// out of the store, so neither reader reads it and no batch writes one: a far end past
		// the bound does not reach here (api's commentStored, outbox's rule).
		var node graphstore.Node
		node, write.Tuples = graphstore.CommentFacts(write.Tuples, row.id, row.author, row.createdAt, row.event, row.anchor, row.owner, row.project, row.parties)
		write.Nodes = append(write.Nodes, node)
		// The owner a proved session names is made once too; each comment still states its own
		// authored_by and aliases.
		if authors, facts, named := graphstore.AuthorFacts(graphstore.KindComment, row.id, row.author, row.createdAt, row.event); named {
			for _, author := range authors {
				if _, made := written[[2]string{author.Kind, author.ID}]; !made {
					written[[2]string{author.Kind, author.ID}] = struct{}{}
					// The author's node is the person's or the session's, whose state and
					// visibility no comment of theirs owns.
					author.Restates = true
					write.Nodes = append(write.Nodes, author)
				}
			}
			write.Tuples = append(write.Tuples, facts...)
		}
		write.ids = append(write.ids, row.id)
		write.baseAttributes += 2
		if row.project != "" || len(row.parties) > 0 {
			write.baseAttributes++
		}
	}
	return write
}
