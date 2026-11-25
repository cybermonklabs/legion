package eval

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// lockSeedAt is the time every row of the lock steps test is written at.
var lockSeedAt = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// The project, the issue and the issue's document the lock steps test writes, and the markdown of
// the document the version write indexes.
const (
	lockSeedProject  = "LOCK"
	lockSeedIssue    = "LOCK-1"
	lockSeedSlug     = "notes"
	lockSeedMarkdown = "# The seed's lock order\n\nThe first paragraph.\n\nThe second paragraph.\n"
)

// TestTheSeedsStoreWritesTakeTheStoreLockOrder holds the seed's two writers of the node and tuple
// store, the write of one comment (writeComment) and of one document's version
// (writeDocumentVersion), whose call sites graphstore's TestEveryStoreWriteCallSiteIsListed
// lists, to the lock order written on api/inbox_read.go's markInboxReadThreads, against a real
// Postgres. Each takes a subset of the order's steps, in the order, as cmd/dispatch-seed's
// writers of the same rows do.
//
// Neither takes an owner row, and neither appends an event before the tuples it states: WriteSeed
// writes a Dispatch nobody is serving, in one transaction, and what each writer states carries the
// sentinel, the cause of a write no event recorded. Nor does either take the owner's lock before
// its store rows: the eval's seed writers are one of the six exceptions the item index's own
// census names (refs' LockItemOwner), because they "write only items on issues and documents their
// own transaction inserts into a database holding no project, so no other writer sees those owners
// until it commits". So the owner held here is the issue's row, as cmd/dispatch-seed's writers'
// tests hold it, and neither writer may queue behind it.
//
// Each writer runs the real code on a connection of its own, in a transaction of its own, over a
// database holding the seed's person, project, issue and document, while a transaction of the
// test's holds a row or a table of every step; the test lets go of each hold the writer queues
// behind, one at a time, and reads where it waits and which tables it holds locks on
// (lockstep.Follow). What a writer holds locks on says nothing of the arm it is running when its
// steps are one batch, whose tables Postgres locks as the batch starts (graphcensus's backfill
// tests say the same): the order it queues in is what says it, and a table is named unlocked here
// only where a step of its own writes it.
//
// The comment write states the comment's node and its author's person node in one graphstore.Write,
// which takes them in ascending (kind, id) and appends their tuples after them, and only then
// writes the comments row whose foreign key names the comment's node. Both nodes are new, so each
// is held as a row another transaction inserted and has not committed; the row is held the same
// way, by its id.
//
// The document write inserts the version, whose trigger writes the document's revision fence row
// (document_block_versions_owe, migration 0122), and then indexes it (refs.IndexBlocks): the fence
// row again, the blocks' nodes, their document_blocks rows, which name those nodes, and what the
// index stated, under the sentinel (graphstore.Caused). The document is new, so no transaction can
// hold its fence row, its nodes, its rows or its tuples before the write makes them, and each step
// is held as the table it writes: share mode, which conflicts with an insert's lock and with no
// read. It is a document of the issue, as writeArtifacts writes one; a spec's version adds the
// issue's task progress after every step here (docs.RecordTaskProgressMarkdown), which takes the
// issue's row and so is no step of the store.
func TestTheSeedsStoreWritesTakeTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database, seed := newLockSeedDatabase(t)
	issueRow := func(t *testing.T) *lockstep.Hold {
		t.Helper()
		return lockstep.HoldRow(t, database, &lockstep.Hold{Name: "issue " + lockSeedIssue + "'s row", Step: lockstep.StepOwner},
			`select 1 from issues where key = $1 for no key update`, lockSeedIssue)
	}

	t.Run("a comment's nodes come before its row", func(t *testing.T) {
		comment := Comment{ID: "lock-comment", Owner: lockSeedIssue, Author: "alice", Body: "A remark on the issue.", CreatedAt: lockSeedAt}
		id := ItemID("comment", comment.ID)
		author := model.Actor{Kind: "user", ID: comment.Author}
		node, _ := graphstore.CommentFacts(nil, id, author, comment.CreatedAt, graphstore.NoEvent, "", "", lockSeedProject, nil)
		authors, _, named := graphstore.AuthorFacts(graphstore.KindComment, id, author, comment.CreatedAt, graphstore.NoEvent)
		if !named || len(authors) != 1 {
			t.Fatalf("the seed's author %s names %d nodes of the store, want one person", comment.Author, len(authors))
		}
		commentNode := holdNewNode(t, database, &lockstep.Hold{
			Name: "the comment's node row", Step: lockstep.StepNode, Taken: true, Kind: node.Kind, ID: node.ID,
			Statement: "insert into graph_nodes (", Unlocked: []string{"comments", "events"},
		}, node)
		personNode := holdNewNode(t, database, &lockstep.Hold{
			Name: "the author's node row", Step: lockstep.StepNode, Taken: true, Kind: authors[0].Kind, ID: authors[0].ID,
			Statement: "insert into graph_nodes (", Unlocked: []string{"comments", "events"},
		}, authors[0])
		row := &lockstep.Hold{
			Name: "the comment's comments row", Step: lockstep.StepProjection, Taken: true, Names: commentNode,
			Statement: "insert into comments (", Unlocked: []string{"events"},
		}
		lockstep.Begin(t, database, row)
		if _, err := row.Tx.Exec(ctx, `insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, 'held')`,
			id, lockSeedIssue, `{"kind":"user","id":"alice"}`); err != nil {
			t.Fatalf("hold %s: %v", row.Name, err)
		}

		pid, done := startLockSeedWrite(t, database, seed, func(ctx context.Context, w *seedWriter) error {
			return w.writeComment(ctx, comment)
		})
		holds := []*lockstep.Hold{issueRow(t), commentNode, personNode, row}
		if queued := lockstep.Follow(t, database, "writeComment", pid, done, holds); len(queued) != 3 {
			t.Fatalf("the comment write queued behind %s, want the comment's node row, the author's and the comments row",
				lockstep.HoldNames(queued))
		}
		var written int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from comments c
			where c.id = $1::uuid and c.node_kind = $2 and c.node_address = c.id::text
			  and exists (
				select 1 from graph_tuples t
				where t.from_kind = c.node_kind and t.from_id = c.node_address
				  and t.kind = $3 and t.to_kind = $4 and t.to_id = $5)
		`, id, graphstore.KindComment, graphstore.TupleAuthoredBy, authors[0].Kind, authors[0].ID).Scan(&written); err != nil {
			t.Fatalf("read back what the comment write wrote: %v", err)
		}
		if written != 1 {
			t.Errorf("the comment write left no comments row naming its node with an authored_by tuple to %s/%s", authors[0].Kind, authors[0].ID)
		}
	})

	t.Run("a version's fence comes before its nodes, its rows and its tuples", func(t *testing.T) {
		document := artifactID(lockSeedIssue, lockSeedSlug)
		fence := holdTable(t, database, &lockstep.Hold{
			Name: "the document's document_block_versions row", Step: lockstep.StepFence, Taken: true, ID: document,
			Statement: "insert into artifact_versions (", Unlocked: []string{"graph_nodes", "document_blocks", "graph_tuples"},
		}, "document_block_versions")
		nodes := holdTable(t, database, &lockstep.Hold{
			Name: "the blocks' node rows", Step: lockstep.StepNode, Taken: true, Statement: "insert into graph_nodes (",
		}, "graph_nodes")
		blocks := holdTable(t, database, &lockstep.Hold{
			Name: "the blocks' document_blocks rows", Step: lockstep.StepProjection, Taken: true, Names: nodes,
			Statement: "insert into document_blocks (",
		}, "document_blocks")
		// The append that follows a node write starts with the nodes it reads back, and its
		// insert into graph_tuples comes after them (graphstore's queueAppend).
		tuples := holdTable(t, database, &lockstep.Hold{
			Name: "the blocks' tuple append", Step: lockstep.StepTuple, Taken: true, Statement: "with written as (",
		}, "graph_tuples")

		pid, done := startLockSeedWrite(t, database, seed, func(ctx context.Context, w *seedWriter) error {
			return w.writeDocumentVersion(ctx, document, lockSeedMarkdown, lockSeedAt, false)
		})
		holds := []*lockstep.Hold{issueRow(t), fence, nodes, blocks, tuples}
		if queued := lockstep.Follow(t, database, "writeDocumentVersion", pid, done, holds); len(queued) != 4 {
			t.Fatalf("the document write queued behind %s, want its fence, its nodes, its rows and its tuples",
				lockstep.HoldNames(queued))
		}
		tree, err := pmdoc.Parse(lockSeedMarkdown)
		if err != nil {
			t.Fatalf("parse the document: %v", err)
		}
		_, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
		if err != nil {
			t.Fatalf("render the document: %v", err)
		}
		var rows, named, stated int
		if err := database.Pool.QueryRow(ctx, `
			select count(*),
			       count(*) filter (where exists (
			           select 1 from graph_nodes n where n.kind = b.node_kind and n.id = b.node_address)),
			       count(*) filter (where exists (
			           select 1 from graph_tuples t
			           where t.from_kind = b.node_kind and t.from_id = b.node_address and t.kind = $2))
			from document_blocks b where b.artifact_id = $1::uuid
		`, document, refs.TupleInDocument).Scan(&rows, &named, &stated); err != nil {
			t.Fatalf("read back the blocks of the document: %v", err)
		}
		if rows != len(offsets) || named != rows || stated != rows {
			t.Errorf("the document write left the document with %d block rows, %d naming a node of the store and %d with an in_document tuple, want %d of each",
				rows, named, stated, len(offsets))
		}
	})
}

// newLockSeedDatabase is a database holding the lock steps test's seed: its one person, its
// project, its issue and the issue's document, written by the seed's own writers and committed.
// The document's row is the one writeArtifacts writes, and its version is the subtest's to write.
func newLockSeedDatabase(t *testing.T) (*store.Store, *Seed) {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	seed := &Seed{
		Now:      lockSeedAt,
		People:   []Person{{Login: "alice", Name: "Alice"}},
		Projects: []Project{{Key: lockSeedProject, Name: "The seed's lock order"}},
		Issues:   []Issue{{Key: lockSeedIssue, Title: "The seed's writes of the store", Status: "todo", CreatedAt: lockSeedAt}},
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed's issue: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := &seedWriter{tx: tx, seed: seed, sessions: map[string]string{}, seqs: map[string]int{}}
	if err := w.writePeopleAndProjects(ctx); err != nil {
		t.Fatalf("write the seed's person and project: %v", err)
	}
	if err := w.writeIssues(ctx); err != nil {
		t.Fatalf("write the seed's issue: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, last_seq)
		values ($1, $2, $3, $4, 'notes.md', 'doc', false, $5::jsonb, $6, 0)
	`, artifactID(lockSeedIssue, lockSeedSlug), lockSeedIssue, lockSeedProject, lockSeedSlug,
		`{"kind":"user","id":"alice"}`, lockSeedAt); err != nil {
		t.Fatalf("write the seed's document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed's issue: %v", err)
	}
	return database, seed
}

// startLockSeedWrite runs write as one step of WriteSeed runs it, over a seed writer of its own in
// a transaction of its own, on a connection of its own, so the test knows the backend it watches,
// and answers that backend and the channel the write's result arrives on.
func startLockSeedWrite(t *testing.T, database *store.Store, seed *Seed, write func(context.Context, *seedWriter) error) (uint32, <-chan error) {
	t.Helper()
	ctx := context.Background()
	single := storetest.SingleConnection(t, database)
	var pid int64
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the seed's write runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := single.Pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := write(ctx, &seedWriter{tx: tx, seed: seed, sessions: map[string]string{}, seqs: map[string]int{}}); err != nil {
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	return uint32(pid), done
}

// holdNewNode holds the row of node, which the store does not hold yet, as a row another
// transaction inserted and has not committed: a write that makes the node waits at its own insert
// of that key to learn whether the holder's row commits.
func holdNewNode(t *testing.T, database *store.Store, hold *lockstep.Hold, node graphstore.Node) *lockstep.Hold {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.Tx, []graphstore.Node{node}, nil); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// holdTable holds every write of table, in share mode, which an insert's lock conflicts with and
// no read does: the hold of a step whose rows are new.
func holdTable(t *testing.T, database *store.Store, hold *lockstep.Hold, table string) *lockstep.Hold {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if _, err := hold.Tx.Exec(context.Background(), "lock table "+table+" in share mode"); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}
