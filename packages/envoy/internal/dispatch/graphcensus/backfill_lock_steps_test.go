package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestEveryBackfillBatchTakesTheStoreLockOrder is the backfill's half of the check
// c-lock-order for the three batches backfill_fence_test.go does not prove: writeBlockRefBatch,
// writeCommentBatch and writeRoleBatch, three of the store write call sites graphstore's
// TestEveryStoreWriteCallSiteIsListed lists. graphstore's TestEveryStoreWriteTakesOneLockOrder
// cannot run them, since this package imports graphstore.
//
// Each subtest seeds the rows one batch writes from, then holds, each in a transaction of its own,
// a row of every step the batch takes and a row of every step it does not take, and runs the real
// Backfill on a connection of its own (startBackfill). It lets go of each hold the run's backend
// queues behind, one at a time, and reads the order the run queued in (followTheBackfill). Each
// time the run queues it also reads, in pg_stat_activity, the statement the run waits at and, in
// pg_locks, the tables the run's transaction holds a lock on, so a step the run took ahead of one
// an invariant puts first shows as a lock on that step's table. The run must never queue behind a
// step's row the batch does not take, must take every step it does take, and must take them as the
// invariants on markInboxReadThreads have them (lockstep.RequireTheStoreLockOrder). The subtest
// then reads what the run wrote, so the run it followed is the one that committed.
func TestEveryBackfillBatchTakesTheStoreLockOrder(t *testing.T) {
	t.Run("writeBlockRefBatch takes the owner row, the fence, its block_refs rows, then the tuple append, and no node row", requireTheBlockRefBatchLockOrder)
	t.Run("writeCommentBatch takes no owner row, then every node row ascending, then the tuple append and the comments rows", requireTheCommentBatchLockOrder)
	t.Run("writeRoleBatch takes no owner row, then its node rows ascending and the part_of tuple", requireTheRoleBatchLockOrder)
	t.Run("writeSessionBatch takes no owner row, then its node rows ascending", requireTheSessionBatchLockOrder)
}

// requireTheBlockRefBatchLockOrder holds writeBlockRefBatch to its steps. The batch is one of the
// writers under the revision fence: it takes the document's owner row, then its
// document_block_versions row, then reads the batch's block_refs rows again `for update`, and only
// then appends the references' tuples and names the block's node on each row. A block
// reference's node is its block's, which the block pass wrote before it, so the batch writes no
// node, and it appends no event.
//
// The batch's block_refs row is held, so the run queues behind it where it reads the row again;
// from there the run holds the row itself, and the update that names the node on it waits for
// nothing. Where the tuple append falls against that update no invariant says, and it is not read.
//
// The block's node row is held `for no key update`, the level the store's own writer takes a node
// row at. The projection's foreign key reads that row `for key share`, which does not conflict with
// it, so a batch that writes no node never queues behind it, while one that wrote the node would.
func requireTheBlockRefBatchLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "only")
	block := document.refKey + "#only"
	// The block pass runs first, as it does in production: it writes the block's node and names it
	// on the document_blocks row. The reference is written after it and so names no node yet.
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the block before its reference exists: %v", err)
	}
	// The reference's own source_seq is the cause of its tuple, and a cause other than the sentinel
	// is what gives the tuple a repeat key a hold can stand on. Neither block_refs.source_seq nor
	// graph_tuples.source_event carries a foreign key, so a fixed number stands for the event.
	const sourceSeq int64 = 9500001
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, source_seq)
		values ($1::uuid, 'only', $2, 'issue', 'CORE-1', 'mentions', 1, $3)
	`, document.artifactID, block, sourceSeq); err != nil {
		t.Fatalf("write the block's reference to CORE-1: %v", err)
	}

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CORE-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"document_block_versions", "block_refs", "graph_nodes", "graph_tuples"},
	}, store.IssueOwner("CORE-1"))
	fence := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the document's document_block_versions row", Step: lockstep.StepFence, Taken: true,
		Statement: "select 1 from document_block_versions where artifact_id = $1 for no key update",
		Unlocked:  []string{"block_refs", "graph_nodes", "graph_tuples"},
	}, `select 1 from document_block_versions where artifact_id = $1::uuid for update`, document.artifactID)
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the block_refs row", Step: lockstep.StepHeld, Taken: true,
		Statement: "select r.artifact_id::text, r.block_id, r.kind, r.to_kind, r.to_id from block_refs r",
	}, `select 1 from block_refs where artifact_id = $1::uuid and block_id = 'only' for no key update`, document.artifactID)
	tuple := holdWrite(t, database, &lockstep.Hold{
		Name: "the reference's mentions tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: block, Kind: "mentions", ToKind: "issue", ToID: "CORE-1",
		At: time.Now(), Who: backfillActor, SourceEvent: sourceSeq, Asserted: true,
	}})
	node := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "the block's node row", Step: lockstep.StepNode},
		`select 1 from graph_nodes where kind = $1 and id = $2 for no key update`, refs.BlockNodeKind, block)
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CORE's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CORE")

	report := followTheBackfill(t, database, "writeBlockRefBatch", []*lockstep.Hold{owner, fence, row, tuple, node, project})

	var nodeKind, nodeAddress *string
	var tuples int64
	if err := database.Pool.QueryRow(ctx, `
		select r.node_kind, r.node_address,
		       (select count(*) from graph_tuples
		        where from_kind = $2 and from_id = $3 and kind = 'mentions' and to_kind = 'issue'
		          and to_id = 'CORE-1' and source_event = $4 and asserted)
		from block_refs r where r.artifact_id = $1::uuid and r.block_id = 'only'
	`, document.artifactID, refs.BlockNodeKind, block, sourceSeq).Scan(&nodeKind, &nodeAddress, &tuples); err != nil {
		t.Fatalf("read the block reference back: %v", err)
	}
	if nodeKind == nil || *nodeKind != refs.BlockNodeKind || nodeAddress == nil || *nodeAddress != block || tuples != 1 {
		t.Errorf("the block reference names (%v, %v) and has %d mentions tuple(s), want (%s, %s) and one: the run the test followed is not one that committed",
			nodeKind, nodeAddress, tuples, refs.BlockNodeKind, block)
	}
	if done := reportedKind(t, report, "block_ref"); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for block references, want 1 row, 1 tuple and the cutover complete", done)
	}
}

// requireTheCommentBatchLockOrder holds writeCommentBatch to its steps. The batch writes every
// node it writes - its comments' and their authors' - in one store write, which takes their rows
// in ascending (kind, id) and then appends every tuple: the facts each comment row projects, and
// each author's authored_by and aliases (graphstore.AuthorFacts). Only then does it name the
// nodes on the comments rows. It takes the owner lock of its comments' issue first, as every
// writer of a comments row does (refs.LockCommentOwners), and appends no event.
//
// Three comments are written with comment.created events, so every tuple carries a cause other
// than the sentinel and so a repeat key a hold can stand on. Their ids order them c1, c2, c3, which
// is the order the batch reads them in, and their authors are a session, zed and amy, so the order
// the batch reads its authors in is the reverse of ascending (kind, id): person amy, person zed,
// session. What the invariants hold the batch to is its node rows in ascending (kind, id) and
// comment c1's node row before the comments row whose foreign key names it. Where its tuples fall
// among its node rows they leave to the batch: a batch that appended its comments' tuples before
// it wrote its authors' nodes, as the comment path appends its comment's tuples before its
// author's node (graphstore.QueueAuthored), passes as well.
func requireTheCommentBatchLockOrder(t *testing.T) {
	ctx := context.Background()
	writes := newAddressingWrites(t)
	database := writes.database
	alice := model.Actor{Kind: "user", ID: "alice"}
	writes.createIssue(t, "CENS-1", 1, "U", alice)

	session := model.Actor{Kind: "session", ID: "ses_lock_steps"}
	zed := model.Actor{Kind: "user", ID: "zed"}
	amy := model.Actor{Kind: "user", ID: "amy"}
	type seededComment struct {
		id    string
		by    model.Actor
		event int64
	}
	comments := []*seededComment{
		{id: "00000000-0000-4000-8000-000000000c01", by: session},
		{id: "00000000-0000-4000-8000-000000000c02", by: zed},
		{id: "00000000-0000-4000-8000-000000000c03", by: amy},
	}
	for _, seeded := range comments {
		seeded.event = writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			author, err := json.Marshal(seeded.by)
			if err != nil {
				return model.Event{}, err
			}
			comment := model.Comment{ID: seeded.id, IssueKey: new("CENS-1"), Author: seeded.by, Body: "Written before the store existed."}
			if err := tx.QueryRow(ctx, `
				insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3, $4) returning created_at
			`, comment.ID, comment.IssueKey, author, comment.Body).Scan(&comment.CreatedAt); err != nil {
				return model.Event{}, err
			}
			comment.Mentions, comment.Deliveries = []model.Mention{}, []model.CommentDelivery{}
			return model.Event{
				IssueKey: new("CENS-1"), Type: "comment.created", Actor: seeded.by,
				Payload: model.CommentEventPayload{Comment: comment},
			}, nil
		}).ID
	}
	c1, c2, c3 := comments[0], comments[1], comments[2]
	now := time.Now()
	commentNode := func(id string) graphstore.Node {
		return graphstore.Node{
			Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: now, MadeBy: backfillActor, SourceEvent: graphstore.NoEvent,
		}
	}
	// A node the batch makes is held as the same key inserted by a transaction still open, which is
	// what its insert waits on; the hold's own base attributes are at the sentinel, which no repeat
	// key covers, so the hold holds the node's row and nothing else. Every node goes in one
	// statement, which reads no tuple and touches no comments row.
	nodeHold := func(name, kind, id string, node graphstore.Node) *lockstep.Hold {
		return holdWrite(t, database, &lockstep.Hold{
			Name: name, Step: lockstep.StepNode, Taken: true, Kind: kind, ID: id,
			Statement: "insert into graph_nodes (",
		}, []graphstore.Node{node}, nil)
	}

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CENS-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, store.IssueOwner("CENS-1"))
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CENS's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CENS")
	firstComment := nodeHold("comment c1's node row", graphstore.KindComment, c1.id, commentNode(c1.id))
	secondComment := nodeHold("comment c2's node row", graphstore.KindComment, c2.id, commentNode(c2.id))
	amyNode := nodeHold("person amy's node row", graphstore.KindPerson, "amy",
		graphstore.PersonNode("amy", backfillActor, now, graphstore.NoEvent))
	zedNode := nodeHold("person zed's node row", graphstore.KindPerson, "zed",
		graphstore.PersonNode("zed", backfillActor, now, graphstore.NoEvent))
	sessionNode := nodeHold("session ses_lock_steps's node row", graphstore.KindSession, session.ID,
		graphstore.SessionNode(session.ID, backfillActor, now, graphstore.NoEvent))
	state := holdWrite(t, database, &lockstep.Hold{
		Name: "comment c1's state tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "with written as (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: c1.id, Kind: graphstore.AttributeState, ToKind: graphstore.KindValue, ToID: graphstore.StateLive,
		At: now, Who: c1.by, SourceEvent: c1.event, Asserted: true,
	}})
	amyAuthored := holdWrite(t, database, &lockstep.Hold{
		Name: "comment c3's authored_by tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "with written as (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: c3.id, Kind: graphstore.TupleAuthoredBy, ToKind: graphstore.KindPerson, ToID: "amy",
		At: now, Who: c3.by, SourceEvent: c3.event, Asserted: true,
	}})
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "comment c1's comments row", Step: lockstep.StepProjection, Taken: true, Names: firstComment,
		Statement: "update comments c set node_kind",
	}, `select 1 from comments where id = $1::uuid for no key update`, c1.id)

	report := followTheBackfill(t, database, "writeCommentBatch",
		[]*lockstep.Hold{owner, project, firstComment, secondComment, amyNode, zedNode, sessionNode, state, amyAuthored, row})

	for _, seeded := range comments {
		authorKind, authorID, _ := graphstore.AuthorOf(seeded.by)
		var nodeKind, nodeAddress *string
		var authored int64
		if err := database.Pool.QueryRow(ctx, `
			select c.node_kind, c.node_address,
			       (select count(*) from graph_tuples
			        where from_kind = $2 and from_id = c.id::text and kind = $3 and to_kind = $4 and to_id = $5
			          and source_event = $6 and asserted)
			from comments c where c.id = $1::uuid
		`, seeded.id, graphstore.KindComment, graphstore.TupleAuthoredBy, authorKind, authorID, seeded.event).Scan(
			&nodeKind, &nodeAddress, &authored); err != nil {
			t.Fatalf("read comment %s back: %v", seeded.id, err)
		}
		if nodeKind == nil || *nodeKind != graphstore.KindComment || nodeAddress == nil || *nodeAddress != seeded.id || authored != 1 {
			t.Errorf("comment %s names (%v, %v) and has %d authored_by tuple(s) to %s %s, want (%s, %s) and one: the run the test followed is not one that committed",
				seeded.id, nodeKind, nodeAddress, authored, authorKind, authorID, graphstore.KindComment, seeded.id)
		}
	}
	if done := reportedKind(t, report, graphstore.KindComment); done.Rows != 3 || !done.Complete {
		t.Errorf("the backfill reports %#v for comments, want 3 rows and the cutover complete", done)
	}
}

// requireTheRoleBatchLockOrder holds writeRoleBatch to its steps. The batch writes a role's node,
// its page's node and the page's part_of the role as one statement on a pooled connection: the
// arms graphstore.WriteArms splices in, the node rows in ascending (kind, id) and the claim after
// them. A role, its page and that part_of have no projection row, and the batch takes no owner
// lock and appends no event. writeRoleBatch lists the role's node before its page's, and a page sorts
// below a role, so the order the batch lists its nodes in is the reverse of ascending.
//
// The role is one an issue's route names, with the issue.updated event that routed it, so its
// nodes and its part_of carry that event as their cause, which gives the part_of a repeat key a
// hold can stand on. The issue's row and its project's row are held throughout, though the role
// owns neither, so a batch that took either would queue behind it.
func requireTheRoleBatchLockOrder(t *testing.T) {
	const role = "lock-steps-role"
	writes := newAddressingWrites(t)
	database := writes.database
	alice := model.Actor{Kind: "user", ID: "alice"}
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	routed := writes.route(t, "CENS-1", role, alice)
	page := graphstore.RolePageID(role)
	if graphstore.KindPage >= graphstore.KindRole {
		t.Fatalf("the page kind %q does not sort below the role kind %q; the test needs the order the batch lists its nodes in and ascending order to disagree",
			graphstore.KindPage, graphstore.KindRole)
	}
	now := time.Now()

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{Name: "issue CENS-1's owner lock", Step: lockstep.StepOwner},
		store.IssueOwner("CENS-1"))
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CENS's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "CENS")
	// The batch is one statement, and Postgres takes the table locks of every arm when the statement
	// starts, so the tables it holds say nothing about which arm it has run: the order it queues in
	// is what proves the order of its arms.
	pageNode := holdWrite(t, database, &lockstep.Hold{
		Name: "the role's page's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindPage, ID: page,
		Statement: "with graph_nodes_written as (",
	}, []graphstore.Node{graphstore.PageNode(page, backfillActor, now, graphstore.NoEvent)}, nil)
	roleNode := holdWrite(t, database, &lockstep.Hold{
		Name: "the role's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindRole, ID: role,
		Statement: "with graph_nodes_written as (",
	}, []graphstore.Node{graphstore.RoleNode(role, backfillActor, now, graphstore.NoEvent)}, nil)
	partOf := holdWrite(t, database, &lockstep.Hold{
		Name: "the page's part_of tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "with graph_nodes_written as (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindPage, FromID: page, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindRole, ToID: role,
		At: now, Who: backfillActor, SourceEvent: routed.ID, Asserted: true,
	}})

	report := followTheBackfill(t, database, "writeRoleBatch", []*lockstep.Hold{owner, project, pageNode, roleNode, partOf})

	requireBackfilledRole(t, database, role, routed.ID)
	if done := reportedKind(t, report, graphstore.KindRole); done.Rows != 1 || done.Nodes != 2 || !done.Complete {
		t.Errorf("the backfill reports %#v for roles, want 1 row, 2 nodes and every role complete", done)
	}
}

// The two documents the owner-order tests seed, one on each of two issues, whose ids sort the other
// way round from their owner keys: issue:ACME-1 sorts before issue:CORE-1, and CORE-1's document's
// id before ACME-1's.
const (
	coreDocumentID = "00000000-0000-4000-8000-0000000000d1"
	acmeDocumentID = "00000000-0000-4000-8000-0000000000d2"
)

// TestTheBlockBatchTakesEveryOwnerRowBeforeAnyFenceRow holds writeBlockBatch over two documents of
// two issues to the order lockBlockDocuments takes their locks in: both owner locks first, in
// ascending owner key, the order the erase takes several owner locks in, and only then each
// document's document_block_versions row, in document id order. The documents' ids sort the other
// way round from their issues' keys, so a batch that took one document's owner lock and fence row
// before the next document's owner lock, in document id order, would take issue CORE-1's lock
// first and its document's fence row before issue ACME-1's lock.
//
// Each of the four rows is held in a transaction of its own while the real Backfill runs
// (followTheBackfill), and the order the run queues behind them in is held to the invariants
// (lockstep.RequireTheStoreLockOrder): the owner rows first and in ascending owner key, the fence
// rows before the rows they guard and in document id order. The test then reads that both blocks
// name their nodes, so the run it followed is the one that committed.
func TestTheBlockBatchTakesEveryOwnerRowBeforeAnyFenceRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	core := seedIssueDocument(t, database, "CORE", coreDocumentID, "Written before the store existed.")
	acme := seedIssueDocument(t, database, "ACME", acmeDocumentID, "Written before the store existed.")
	seedPreStoreBlock(t, database, core, "only")
	seedPreStoreBlock(t, database, acme, "only")

	ownerHold := func(issue string) *lockstep.Hold {
		return lockstep.HoldOwner(t, database, &lockstep.Hold{
			Name: "issue " + issue + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: issue,
			Statement: lockstep.OwnerLockStatement,
			Unlocked:  []string{"document_block_versions", "document_blocks", "graph_nodes", "graph_tuples"},
		}, store.IssueOwner(issue))
	}
	fenceHold := func(document preStoreDocument) *lockstep.Hold {
		return lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: document.refKey + "'s document_block_versions row", Step: lockstep.StepFence, Taken: true,
			Kind: "document", ID: document.artifactID,
			Statement: "select 1 from document_block_versions where artifact_id = $1 for no key update",
			Unlocked:  []string{"document_blocks", "graph_nodes", "graph_tuples"},
		}, `select 1 from document_block_versions where artifact_id = $1::uuid for update`, document.artifactID)
	}
	holds := []*lockstep.Hold{ownerHold("ACME-1"), ownerHold("CORE-1"), fenceHold(core), fenceHold(acme)}

	report := followTheBackfill(t, database, "writeBlockBatch", holds)

	var named int64
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_blocks
		where artifact_id = any($1::uuid[]) and block_id = 'only' and node_kind = $2 and node_address is not null
	`, []string{core.artifactID, acme.artifactID}, refs.BlockNodeKind).Scan(&named); err != nil {
		t.Fatalf("read the two blocks back: %v", err)
	}
	if named != 2 {
		t.Errorf("%d of the two blocks name their node, want both: the run the test followed is not one that committed", named)
	}
	if done := reportedKind(t, report, refs.BlockNodeKind); done.Rows != 2 {
		t.Errorf("the backfill reports %#v for blocks, want 2 rows", done)
	}
}

// seedIssueDocument writes project, its issue <project>-1, and one document of that issue under id
// at version 1 holding markdown. The version carries markdown, so migration 0122's trigger gives
// the document its document_block_versions row, as a version gives one in production.
func seedIssueDocument(t *testing.T, database *store.Store, project, id, markdown string) preStoreDocument {
	t.Helper()
	ctx := context.Background()
	issue := project + "-1"
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice')
	`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'One', '{"kind":"user","id":"alice"}', 'U')
	`, issue, project); err != nil {
		t.Fatalf("seed issue %s: %v", issue, err)
	}
	document := preStoreDocument{artifactID: id}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1::uuid, $2, $3, 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning ref_key
	`, id, issue, project).Scan(&document.refKey); err != nil {
		t.Fatalf("create %s's document: %v", issue, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, id, markdown); err != nil {
		t.Fatalf("write version 1 of %s: %v", document.refKey, err)
	}
	return document
}

// seedPreStoreBlock writes block blockID of document as a document_blocks row naming no node,
// which is what the backfill reads.
func seedPreStoreBlock(t *testing.T, database *store.Store, document preStoreDocument, blockID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, now())
	`, document.artifactID, blockID, document.refKey+"#"+blockID, "Block "+blockID+"."); err != nil {
		t.Fatalf("write block %s of %s: %v", blockID, document.refKey, err)
	}
}

// holdWrite holds a row the batch inserts, by writing the same key through the store's own writer
// in a transaction left open (lockstep.Begin): the batch's insert of that key then waits to learn
// whether the hold's row commits. A node is the same (kind, id), and a tuple the same repeat key,
// which `on conflict do nothing` cannot pass until the hold ends.
func holdWrite(t *testing.T, database *store.Store, hold *lockstep.Hold, nodes []graphstore.Node, tuples []graphstore.Tuple) *lockstep.Hold {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.Tx, nodes, tuples); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// followTheBackfill runs Backfill on a connection of its own while holds hold their rows, lets go
// of each hold the run's backend queues behind, one at a time, holds the order the run queued
// behind them in to the store's lock order (lockstep.RequireTheStoreLockOrder), and answers the
// run's report. A statement waits on one lock at a time, so a run queued behind two holds at once
// is a failure of the test's own reading, and a run that queues behind none and does not answer
// is waiting on something the test does not hold. The order is read before a failed run fails the
// test, since a batch that takes a step too early can fail on it: a projection row written before
// its node is refused by the foreign key, and the order is what names the step.
func followTheBackfill(t *testing.T, database *store.Store, batch string, holds []*lockstep.Hold) BackfillReport {
	t.Helper()
	ctx := context.Background()
	pid, answered := startBackfill(t, database)
	// The run's own pool is closed by a cleanup startBackfill registered, which waits for the run's
	// connection; this one runs before it and lets go of every hold, so a failure never leaves that
	// close waiting on a run queued behind a hold.
	t.Cleanup(func() {
		for _, hold := range holds {
			hold.Release()
		}
	})
	var queued []*lockstep.Hold
	deadline := time.Now().Add(lockstep.Wait)
	for {
		select {
		case answer := <-answered:
			lockstep.RequireTheStoreLockOrder(t, batch, holds, queued)
			if answer.err != nil {
				t.Fatalf("the backfill failed after %s queued behind %s: %v", batch, lockstep.HoldNames(queued), answer.err)
			}
			storetest.RequireNoMarkedCause(t, database, "the backfill's "+batch)
			return answer.report
		default:
		}
		var behind []*lockstep.Hold
		for _, hold := range holds {
			if hold.Released() {
				continue
			}
			waits, err := storetest.QueuedBehind(ctx, hold.Tx, pid)
			if err != nil {
				t.Fatalf("read what %s waits on: %v", batch, err)
			}
			if waits {
				behind = append(behind, hold)
			}
		}
		switch len(behind) {
		case 0:
			if time.Now().After(deadline) {
				t.Fatalf("%s queued behind none of the holds still held and the backfill did not answer within %s (it queued behind %s so far)",
					batch, lockstep.Wait, lockstep.HoldNames(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			lockstep.RequireStoppedAt(t, ctx, database, pid, batch, behind[0])
			queued = append(queued, behind[0])
			behind[0].Release()
			deadline = time.Now().Add(lockstep.Wait)
		default:
			t.Fatalf("%s queued behind %d holds at once (%s), which one statement cannot do", batch, len(behind), lockstep.HoldNames(behind))
		}
	}
}

// reportedKind is one kind's line of a backfill report.
func reportedKind(t *testing.T, report BackfillReport, kind string) KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == kind {
			return done
		}
	}
	t.Fatalf("the backfill reports no %s kind: %#v", kind, report)
	return KindBackfill{}
}
