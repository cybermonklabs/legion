package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// batchLockWait bounds each wait of TestEveryBackfillBatchTakesTheStoreLockOrder: for the run to
// queue behind the next hold once the one before it let go, and for the run to answer once it has
// queued behind the last. It is far above what the work takes; a wait that passes it is the run
// waiting on something the test does not hold.
const batchLockWait = 30 * time.Second

// The steps a hold stands for, named as a failure names them: the owner row; the two steps a
// writer under the revision fence (R23) adds after it, the document's document_block_versions row
// and the projection rows it reads again under those two locks, `for update`, before it writes
// anything (heldBlockRows, heldBlockRefRows); the node rows; the tuple append; the projection rows
// a batch writes; and the project of an event. requireTheStoreLockOrder holds the order a run
// takes them in to the invariants on api/inbox_read.go's markInboxReadThreads, not to one sequence
// of these steps.
const (
	stepOwner      = "the owner row"
	stepFence      = "the revision fence's document_block_versions row"
	stepHeld       = "the projection rows, read again under the fence"
	stepNode       = "the node rows"
	stepTuple      = "the tuple append"
	stepProjection = "the projection rows"
	stepEvent      = "the project of the event"
)

// fencedSteps are the steps whose rows the revision fence guards: a writer under it takes the
// document's document_block_versions row before it reads a projection row again under it, or
// writes a node, a tuple or a projection row.
var fencedSteps = []string{stepHeld, stepNode, stepTuple, stepProjection}

// TestEveryBackfillBatchTakesTheStoreLockOrder is the backfill's half of dispatch://INFRA-1149's
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
// invariants on markInboxReadThreads have them (requireTheStoreLockOrder). The subtest then reads
// what the run wrote, so the run it followed is the one that committed.
func TestEveryBackfillBatchTakesTheStoreLockOrder(t *testing.T) {
	t.Run("writeBlockRefBatch takes the owner row, the fence, its block_refs rows, then the tuple append, and no node row", requireTheBlockRefBatchLockOrder)
	t.Run("writeCommentBatch takes no owner row, then every node row ascending, then the tuple append and the comments rows", requireTheCommentBatchLockOrder)
	t.Run("writeRoleBatch takes no owner row, then its node rows ascending and the part_of tuple", requireTheRoleBatchLockOrder)
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

	owner := holdRow(t, database, &batchHold{
		name: "issue CORE-1's row", step: stepOwner, taken: true,
		statement: "select closed_at is null from issues where key = $1 for no key update",
		unlocked:  []string{"document_block_versions", "block_refs", "graph_nodes", "graph_tuples"},
	}, `select 1 from issues where key = $1 for update`, "CORE-1")
	fence := holdRow(t, database, &batchHold{
		name: "the document's document_block_versions row", step: stepFence, taken: true,
		statement: "select 1 from document_block_versions where artifact_id = $1 for no key update",
		unlocked:  []string{"block_refs", "graph_nodes", "graph_tuples"},
	}, `select 1 from document_block_versions where artifact_id = $1::uuid for update`, document.artifactID)
	row := holdRow(t, database, &batchHold{
		name: "the block_refs row", step: stepHeld, taken: true,
		statement: "select r.artifact_id::text, r.block_id, r.kind, r.to_kind, r.to_id from block_refs r",
	}, `select 1 from block_refs where artifact_id = $1::uuid and block_id = 'only' for no key update`, document.artifactID)
	tuple := holdWrite(t, database, &batchHold{
		name: "the reference's mentions tuple", step: stepTuple, taken: true,
		statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: block, Kind: "mentions", ToKind: "issue", ToID: "CORE-1",
		At: time.Now(), Who: backfillActor, SourceEvent: sourceSeq, Asserted: true,
	}})
	node := holdRow(t, database, &batchHold{name: "the block's node row", step: stepNode},
		`select 1 from graph_nodes where kind = $1 and id = $2 for no key update`, refs.BlockNodeKind, block)
	project := holdRow(t, database, &batchHold{name: "project CORE's row", step: stepEvent},
		`select 1 from projects where key = $1 for update`, "CORE")

	report := followTheBackfill(t, database, "writeBlockRefBatch", []*batchHold{owner, fence, row, tuple, node, project})

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
// nodes on the comments rows. It runs under no owner row and appends no event.
//
// Three comments are written with comment.created events, so every tuple carries a cause other
// than the sentinel and so a repeat key a hold can stand on. Their ids order them c1, c2, c3, which
// is the order the batch reads them in, and their authors are a session, zed and amy, so the order
// the batch reads its authors in is the reverse of ascending (kind, id): person amy, person zed,
// session. What the invariants hold the batch to is its node rows in ascending (kind, id) and
// comment c1's node row before the comments row whose foreign key names it. Where its tuples fall
// among its node rows they leave to the batch: a batch that appended its comments' tuples before
// it wrote its authors' nodes, as the comment path appends its comment's tuples before its
// author's node (graphstore.Authored), passes as well.
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
			Kind: commentNodeKind, ID: id, Supertag: commentSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: now, MadeBy: backfillActor, SourceEvent: graphstore.NoEvent,
		}
	}
	// A node the batch makes is held as the same key inserted by a transaction still open, which is
	// what its insert waits on; the hold's own base attributes are at the sentinel, which no repeat
	// key covers, so the hold holds the node's row and nothing else. Every node goes in one
	// statement, which reads no tuple and touches no comments row.
	nodeHold := func(name, kind, id string, node graphstore.Node) *batchHold {
		return holdWrite(t, database, &batchHold{
			name: name, step: stepNode, taken: true, kind: kind, id: id,
			statement: "with incoming as (",
		}, []graphstore.Node{node}, nil)
	}

	owner := holdRow(t, database, &batchHold{name: "issue CENS-1's row", step: stepOwner},
		`select 1 from issues where key = $1 for update`, "CENS-1")
	project := holdRow(t, database, &batchHold{name: "project CENS's row", step: stepEvent},
		`select 1 from projects where key = $1 for update`, "CENS")
	firstComment := nodeHold("comment c1's node row", commentNodeKind, c1.id, commentNode(c1.id))
	secondComment := nodeHold("comment c2's node row", commentNodeKind, c2.id, commentNode(c2.id))
	amyNode := nodeHold("person amy's node row", graphstore.KindPerson, "amy",
		graphstore.PersonNode("amy", backfillActor, now, graphstore.NoEvent))
	zedNode := nodeHold("person zed's node row", graphstore.KindPerson, "zed",
		graphstore.PersonNode("zed", backfillActor, now, graphstore.NoEvent))
	sessionNode := nodeHold("session ses_lock_steps's node row", graphstore.KindSession, session.ID,
		graphstore.SessionNode(session.ID, backfillActor, now, graphstore.NoEvent))
	state := holdWrite(t, database, &batchHold{
		name: "comment c1's state tuple", step: stepTuple, taken: true,
		statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: commentNodeKind, FromID: c1.id, Kind: tupleState, ToKind: graphstore.KindValue, ToID: graphstore.StateLive,
		At: now, Who: c1.by, SourceEvent: c1.event, Asserted: true,
	}})
	amyAuthored := holdWrite(t, database, &batchHold{
		name: "comment c3's authored_by tuple", step: stepTuple, taken: true,
		statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: commentNodeKind, FromID: c3.id, Kind: graphstore.TupleAuthoredBy, ToKind: graphstore.KindPerson, ToID: "amy",
		At: now, Who: c3.by, SourceEvent: c3.event, Asserted: true,
	}})
	row := holdRow(t, database, &batchHold{
		name: "comment c1's comments row", step: stepProjection, taken: true, names: firstComment,
		statement: "update comments c set node_kind",
	}, `select 1 from comments where id = $1::uuid for no key update`, c1.id)

	report := followTheBackfill(t, database, "writeCommentBatch",
		[]*batchHold{owner, project, firstComment, secondComment, amyNode, zedNode, sessionNode, state, amyAuthored, row})

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
		`, seeded.id, commentNodeKind, graphstore.TupleAuthoredBy, authorKind, authorID, seeded.event).Scan(
			&nodeKind, &nodeAddress, &authored); err != nil {
			t.Fatalf("read comment %s back: %v", seeded.id, err)
		}
		if nodeKind == nil || *nodeKind != commentNodeKind || nodeAddress == nil || *nodeAddress != seeded.id || authored != 1 {
			t.Errorf("comment %s names (%v, %v) and has %d authored_by tuple(s) to %s %s, want (%s, %s) and one: the run the test followed is not one that committed",
				seeded.id, nodeKind, nodeAddress, authored, authorKind, authorID, commentNodeKind, seeded.id)
		}
	}
	if done := reportedKind(t, report, commentNodeKind); done.Rows != 3 || !done.Complete {
		t.Errorf("the backfill reports %#v for comments, want 3 rows and the cutover complete", done)
	}
}

// requireTheRoleBatchLockOrder holds writeRoleBatch to its steps. The batch writes a role's node,
// its page's node and the page's part_of the role as one statement on a pooled connection: the
// arms graphstore.WriteArms splices in, the node rows in ascending (kind, id) and the claim after
// them. A role, its page and that part_of have no projection row, and the batch takes no owner row
// and appends no event. writeRoleBatch lists the role's node before its page's, and a page sorts
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

	owner := holdRow(t, database, &batchHold{name: "issue CENS-1's row", step: stepOwner},
		`select 1 from issues where key = $1 for update`, "CENS-1")
	project := holdRow(t, database, &batchHold{name: "project CENS's row", step: stepEvent},
		`select 1 from projects where key = $1 for update`, "CENS")
	// The batch is one statement, and Postgres takes the table locks of every arm when the statement
	// starts, so the tables it holds say nothing about which arm it has run: the order it queues in
	// is what proves the order of its arms.
	pageNode := holdWrite(t, database, &batchHold{
		name: "the role's page's node row", step: stepNode, taken: true, kind: graphstore.KindPage, id: page,
		statement: "with graph_nodes_written as (",
	}, []graphstore.Node{graphstore.PageNode(page, backfillActor, now, graphstore.NoEvent)}, nil)
	roleNode := holdWrite(t, database, &batchHold{
		name: "the role's node row", step: stepNode, taken: true, kind: graphstore.KindRole, id: role,
		statement: "with graph_nodes_written as (",
	}, []graphstore.Node{graphstore.RoleNode(role, backfillActor, now, graphstore.NoEvent)}, nil)
	partOf := holdWrite(t, database, &batchHold{
		name: "the page's part_of tuple", step: stepTuple, taken: true,
		statement: "with graph_nodes_written as (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindPage, FromID: page, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindRole, ToID: role,
		At: now, Who: backfillActor, SourceEvent: routed.ID, Asserted: true,
	}})

	report := followTheBackfill(t, database, "writeRoleBatch", []*batchHold{owner, project, pageNode, roleNode, partOf})

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
// two issues to the order lockBlockDocuments takes their rows in: both owner rows first, in
// ascending owner key, the order the erase takes several owner rows in, and only then each
// document's document_block_versions row, in document id order. The documents' ids sort the other
// way round from their issues' keys, so a batch that took one document's owner row and fence row
// before the next document's owner row, in document id order, would take issue CORE-1's row first
// and its document's fence row before issue ACME-1's row.
//
// Each of the four rows is held in a transaction of its own while the real Backfill runs
// (followTheBackfill), and the order the run queues behind them in is held to the invariants
// (requireTheStoreLockOrder): the owner rows first and in ascending owner key, the fence rows
// before the rows they guard and in document id order. The test then reads that both blocks name
// their nodes, so the run it followed is the one that committed.
func TestTheBlockBatchTakesEveryOwnerRowBeforeAnyFenceRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	core := seedIssueDocument(t, database, "CORE", coreDocumentID, "Written before the store existed.")
	acme := seedIssueDocument(t, database, "ACME", acmeDocumentID, "Written before the store existed.")
	seedPreStoreBlock(t, database, core, "only")
	seedPreStoreBlock(t, database, acme, "only")

	ownerHold := func(issue string) *batchHold {
		return holdRow(t, database, &batchHold{
			name: "issue " + issue + "'s row", step: stepOwner, taken: true, kind: "issue", id: issue,
			statement: "select closed_at is null from issues where key = $1 for no key update",
			unlocked:  []string{"document_block_versions", "document_blocks", "graph_nodes", "graph_tuples"},
		}, `select 1 from issues where key = $1 for update`, issue)
	}
	fenceHold := func(document preStoreDocument) *batchHold {
		return holdRow(t, database, &batchHold{
			name: document.refKey + "'s document_block_versions row", step: stepFence, taken: true,
			kind: "document", id: document.artifactID,
			statement: "select 1 from document_block_versions where artifact_id = $1 for no key update",
			unlocked:  []string{"document_blocks", "graph_nodes", "graph_tuples"},
		}, `select 1 from document_block_versions where artifact_id = $1::uuid for update`, document.artifactID)
	}
	holds := []*batchHold{ownerHold("ACME-1"), ownerHold("CORE-1"), fenceHold(core), fenceHold(acme)}

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

// TestTheBlockBatchAndAnEraseOverTwoIssuesBothFinish is the deadlock the owner order spares a block
// batch and an erase that share two documents of two issues. The erase is of a comment on issue
// CORE-1's document whose text a version of issue ACME-1's document quotes, so it writes under both
// owner rows and takes them in owner-key order, ACME-1's first (lockInStoreOrder); the batch
// backfills a block of each document. Another transaction holds CORE-1's document's
// document_block_versions row while the batch starts, so the batch stops there holding what it
// took before it; the erase then starts and queues behind the batch. Once that row is let go both
// have to finish.
//
// A batch that took the documents' rows document by document, in id order, would stop at that row
// holding CORE-1's row alone; the erase would take ACME-1's row and wait for CORE-1's, and the
// batch, let go, would wait for ACME-1's, each holding the row the other waits for, until Postgres
// ended one of them with `deadlock detected`. Taking both owner rows first, in the erase's order,
// the batch holds both while it waits, and the erase waits behind it holding neither.
func TestTheBlockBatchAndAnEraseOverTwoIssuesBothFinish(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	core := seedIssueDocument(t, database, "CORE", coreDocumentID, "Written before the store existed.")
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, core.artifactID).Scan(&comment); err != nil {
		t.Fatalf("write the comment on %s: %v", core.refKey, err)
	}
	acme := seedIssueDocument(t, database, "ACME", acmeDocumentID, "Alice wrote: A remark on the spec.")
	// The first run gives the comment its node, which the erase needs; the blocks are written
	// after it, so the run under test has them to backfill.
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comment: %v", err)
	}
	seedPreStoreBlock(t, database, core, "only")
	seedPreStoreBlock(t, database, acme, "only")

	fence := holdRow(t, database, &batchHold{name: core.refKey + "'s document_block_versions row", step: stepFence},
		`select 1 from document_block_versions where artifact_id = $1::uuid for update`, core.artifactID)
	backfillPID, backfilled := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, fence.tx, backfillPID, backfilled, batchLockWait); done {
		t.Fatalf("the backfill answered (err %v) without waiting on %s's document_block_versions row, which another transaction held", early.err, core.refKey)
	}
	t.Logf("the backfill waits behind %s at %q, holding locks on %v", fence.name,
		waitingStatement(t, database, backfillPID), tablesLockedBy(t, ctx, database, backfillPID))
	erasePID, erased := startErase(t, database, commentNodeKind, comment)
	waitBlockedBy(t, ctx, database, erasePID, backfillPID, erased)
	t.Logf("the erase waits behind the backfill at %q", waitingStatement(t, database, erasePID))
	fence.release()

	deadline := time.After(batchLockWait)
	for backfilled != nil || erased != nil {
		select {
		case answer := <-backfilled:
			if answer.err != nil {
				t.Errorf("the backfill failed once %s was let go: %v", fence.name, answer.err)
			}
			backfilled = nil
		case answer := <-erased:
			if answer.err != nil {
				t.Errorf("the erase failed once %s was let go: %v", fence.name, answer.err)
			} else if !answer.erasure.Applied {
				t.Errorf("the erase removed nothing once %s was let go: %+v", fence.name, answer.erasure)
			}
			erased = nil
		case <-deadline:
			t.Fatalf("the backfill and the erase did not both answer within %s of the release of %s", batchLockWait, fence.name)
		}
	}
	if t.Failed() {
		return
	}
	var named, commentStands int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks
		        where artifact_id = any($1::uuid[]) and block_id = 'only' and node_kind = $2 and node_address is not null),
		       (select count(*) from comments where id::text = $3)
	`, []string{core.artifactID, acme.artifactID}, refs.BlockNodeKind, comment).Scan(&named, &commentStands); err != nil {
		t.Fatalf("read what the two runs left: %v", err)
	}
	if named != 2 || commentStands != 0 {
		t.Errorf("after both runs %d of the two blocks name their node and %d comment row(s) stand, want both blocks and none", named, commentStands)
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

// waitBlockedBy waits until backend waiter waits on a lock backend blocker holds, which
// pg_blocking_pids answers, and fails the test if the waiter's run answers first or the wait
// passes batchLockWait.
func waitBlockedBy(t *testing.T, ctx context.Context, database *store.Store, waiter, blocker uint32, answered <-chan eraseAnswer) {
	t.Helper()
	deadline := time.Now().Add(batchLockWait)
	for {
		select {
		case answer := <-answered:
			t.Fatalf("the erase answered (err %v) without waiting on the backfill", answer.err)
		default:
		}
		var blocked bool
		if err := database.Pool.QueryRow(ctx,
			`select $2::int = any(pg_blocking_pids($1::int))`, int32(waiter), int32(blocker)).Scan(&blocked); err != nil {
			t.Fatalf("read what backend %d waits on: %v", waiter, err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d did not wait on backend %d within %s", waiter, blocker, batchLockWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// batchHold is one row of a step of the store's lock order, held in a transaction of its own while
// the backfill runs, and what the test expects of the run while it waits behind it.
type batchHold struct {
	// name is the row as a failure names it, and step the step of the lock order it stands for.
	name, step string
	// taken is whether the batch takes the row. A row it does not take is held for the whole run,
	// and the run must never queue behind it.
	taken bool
	// kind and id order the row among the run's other rows of its step: a node's kind and id, an
	// owner row's owner key split at its colon ("issue", "CORE-1"), a fence row's document id.
	kind, id string
	// names is, for a projection row the run writes, the hold of the node row its foreign key
	// names, when the run writes that node, and nil otherwise.
	names *batchHold
	// statement is how the statement the run waits at behind this hold starts, lower-cased with its
	// whitespace collapsed: the statement that takes the step.
	statement string
	// unlocked are the tables the run's transaction may not hold a lock on while it waits behind
	// this hold: those of the batch's later steps, and those of a step it does not take.
	unlocked []string

	tx       pgx.Tx
	released bool
}

// beginBatchHold opens the transaction hold holds its row in. Its release is registered as a
// cleanup too, so a failed assertion never leaves the run queued on a lock nothing will drop.
func beginBatchHold(t *testing.T, database *store.Store, hold *batchHold) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the transaction holding %s: %v", hold.name, err)
	}
	hold.tx = tx
	t.Cleanup(hold.release)
}

func (h *batchHold) release() {
	if h.released {
		return
	}
	h.released = true
	_ = h.tx.Rollback(context.Background())
}

// holdRow holds a row that is already there, with statement, which locks it.
func holdRow(t *testing.T, database *store.Store, hold *batchHold, statement string, args ...any) *batchHold {
	t.Helper()
	beginBatchHold(t, database, hold)
	tag, err := hold.tx.Exec(context.Background(), statement, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold %s: %d rows, %v", hold.name, tag.RowsAffected(), err)
	}
	return hold
}

// holdWrite holds a row the batch inserts, by writing the same key through the store's own writer
// in a transaction left open: the batch's insert of that key then waits to learn whether the hold's
// row commits. A node is the same (kind, id), and a tuple the same repeat key, which `on conflict
// do nothing` cannot pass until the hold ends.
func holdWrite(t *testing.T, database *store.Store, hold *batchHold, nodes []graphstore.Node, tuples []graphstore.Tuple) *batchHold {
	t.Helper()
	beginBatchHold(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.tx, nodes, tuples); err != nil {
		t.Fatalf("hold %s: %v", hold.name, err)
	}
	return hold
}

// followTheBackfill runs Backfill on a connection of its own while holds hold their rows, lets go
// of each hold the run's backend queues behind, one at a time, holds the order the run queued
// behind them in to the store's lock order (requireTheStoreLockOrder), and answers the run's
// report. A statement waits on one lock at a time, so a run queued behind two holds at once is a
// failure of the test's own reading, and a run that queues behind none and does not answer is
// waiting on something the test does not hold. The order is read before a failed run fails the
// test, since a batch that takes a step too early can fail on it: a projection row written before
// its node is refused by the foreign key, and the order is what names the step.
func followTheBackfill(t *testing.T, database *store.Store, batch string, holds []*batchHold) BackfillReport {
	t.Helper()
	ctx := context.Background()
	pid, answered := startBackfill(t, database)
	// The run's own pool is closed by a cleanup startBackfill registered, which waits for the run's
	// connection; this one runs before it and lets go of every hold, so a failure never leaves that
	// close waiting on a run queued behind a hold.
	t.Cleanup(func() {
		for _, hold := range holds {
			hold.release()
		}
	})
	var queued []*batchHold
	deadline := time.Now().Add(batchLockWait)
	for {
		select {
		case answer := <-answered:
			requireTheStoreLockOrder(t, batch, holds, queued)
			if answer.err != nil {
				t.Fatalf("the backfill failed after %s queued behind %s: %v", batch, holdNames(queued), answer.err)
			}
			return answer.report
		default:
		}
		var behind []*batchHold
		for _, hold := range holds {
			if hold.released {
				continue
			}
			waits, err := storetest.QueuedBehind(ctx, hold.tx, pid)
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
					batch, batchLockWait, holdNames(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			requireStoppedAt(t, ctx, database, pid, batch, behind[0])
			queued = append(queued, behind[0])
			behind[0].release()
			deadline = time.Now().Add(batchLockWait)
		default:
			t.Fatalf("%s queued behind %d holds at once (%s), which one statement cannot do", batch, len(behind), holdNames(behind))
		}
	}
}

// requireStoppedAt reads where the run is while it waits behind hold: the statement it waits at,
// which has to be the statement that takes hold's step, and the tables its transaction holds a lock
// on, none of which may be a table of a later step. Postgres keeps every table lock a transaction
// takes until it ends, so a step the run has already taken shows as a lock on its table, where the
// statement alone names only the step the run is stopped at.
func requireStoppedAt(t *testing.T, ctx context.Context, database *store.Store, pid uint32, batch string, hold *batchHold) {
	t.Helper()
	running := strings.ToLower(waitingStatement(t, database, pid))
	locked := tablesLockedBy(t, ctx, database, pid)
	// The statement is shown by its head, which names it; pg_stat_activity keeps at most its first
	// track_activity_query_size bytes anyway.
	shown := running
	if len(shown) > 120 {
		shown = shown[:120] + "..."
	}
	t.Logf("%s waits behind %s (%s) at %q, holding locks on %v", batch, hold.name, hold.step, shown, locked)
	if hold.statement != "" && !strings.HasPrefix(running, hold.statement) {
		t.Errorf("%s waits behind %s (%s) at %q, which is not the statement that takes that step (it starts %q)",
			batch, hold.name, hold.step, shown, hold.statement)
	}
	for _, table := range hold.unlocked {
		if slices.Contains(locked, table) {
			t.Errorf("%s already holds a lock on %s while it waits behind %s (%s): it ran a statement on %s, a table of a later step or of a step it does not take, before it took %s (it holds locks on %v)",
				batch, table, hold.name, hold.step, table, hold.step, locked)
		}
	}
}

// requireTheStoreLockOrder holds the order the run queued behind holds in to the invariants written
// on api/inbox_read.go's markInboxReadThreads, as api's requireTheWrittenOrderAdmits holds that
// package's writers to them, and not to one sequence of steps: those invariants leave a tuple
// append wherever its writer puts it, before a node row of the same write as the comment path puts
// its comment's tuples before its author's node, or after a projection row. It holds the run to:
//
//   - no row of a step the batch does not take queued behind: the owner row comes first or not at
//     all, and a batch that queued behind one took it after a step it had already taken;
//   - every row of a step the batch takes queued behind once, or the step was not shown to be
//     taken while it was held;
//   - the owner row first: no row of another step taken before an owner row;
//   - the revision fence's row (R23) after the owner row, which the rule before says, and before
//     the rows it guards (fencedSteps);
//   - a node's row before the projection row whose foreign key names it (batchHold.names);
//   - the project of the event last;
//   - several rows of one step in ascending order: node rows in ascending (kind, id), and owner
//     rows in ascending owner key and fence rows in document id order, the order every writer of
//     several documents takes them in (lockBlockDocuments, the erase's lockInStoreOrder,
//     events.lockOwners).
//
// The third invariant, the event before the tuples it causes, holds of a batch with nothing to
// read: a batch appends no event, and every cause its tuples carry is an event the log already
// holds.
func requireTheStoreLockOrder(t *testing.T, batch string, holds, queued []*batchHold) {
	t.Helper()
	order := holdNames(queued)
	t.Logf("%s queued behind, in order: %s", batch, order)
	at := func(hold *batchHold) int { return slices.Index(queued, hold) }
	for _, hold := range holds {
		position := at(hold)
		switch {
		case !hold.taken && position >= 0:
			t.Errorf("%s queued behind %s (%s), a step it does not take, after %s: it took %s after a step it had already taken, out of the store's lock order (queued in order: %s)",
				batch, hold.name, hold.step, holdNames(queued[:position]), hold.step, order)
		case hold.taken && position < 0:
			t.Errorf("%s never queued behind %s (%s) (queued in order: %s): the step was not taken while its row was held",
				batch, hold.name, hold.step, order)
		}
	}
	// before fails the run when it queued behind later, a row it takes, without having queued
	// behind earlier, another row it takes, first: the invariant rule puts earlier first. A row of
	// a step the run takes and never queued behind at all did not come first either, so it counts
	// as taken after.
	before := func(earlier, later *batchHold, rule string) {
		t.Helper()
		first, second := at(earlier), at(later)
		if second >= 0 && (first < 0 || first > second) {
			t.Errorf("%s took %s (%s) before %s (%s): %s (queued in order: %s)",
				batch, later.name, later.step, earlier.name, earlier.step, rule, order)
		}
	}
	for _, hold := range holds {
		if !hold.taken {
			continue
		}
		for _, other := range holds {
			if !other.taken || other.step == hold.step {
				continue
			}
			switch {
			case hold.step == stepOwner:
				before(hold, other, "the owner row comes first")
			case hold.step == stepFence && slices.Contains(fencedSteps, other.step):
				before(hold, other, "the revision fence's row comes before the rows it guards")
			case other.step == stepEvent:
				before(hold, other, "the project of the event comes last")
			}
		}
		if hold.names != nil && hold.names.taken {
			before(hold.names, hold, "a node's row comes before the projection row whose foreign key names it")
		}
	}
	for _, ascending := range []struct{ step, rule string }{
		{stepOwner, "several owner rows are taken in ascending owner key"},
		{stepFence, "several documents' fence rows are taken in document id order"},
		{stepNode, "several node rows are taken in ascending (kind, id)"},
	} {
		var previous *batchHold
		for _, hold := range queued {
			if !hold.taken || hold.step != ascending.step {
				continue
			}
			if previous != nil && (hold.kind < previous.kind || hold.kind == previous.kind && hold.id < previous.id) {
				t.Errorf("%s took %s before %s: %s, so %s %s before %s %s (queued in order: %s)",
					batch, previous.name, hold.name, ascending.rule, hold.kind, hold.id, previous.kind, previous.id, order)
			}
			previous = hold
		}
	}
}

// holdNames names holds in their order, for a failure or a log line.
func holdNames(holds []*batchHold) string {
	names := make([]string, len(holds))
	for index, hold := range holds {
		names[index] = hold.name
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// tablesLockedBy names every table backend pid holds or asks a lock on, in name order, which for a
// run stopped on a lock is every table its transaction has read or written so far. It reads
// pg_locks through the pool, a fresh view on each read, and leaves indexes and sequences out: a
// step is named by the table it writes.
func tablesLockedBy(t *testing.T, ctx context.Context, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select distinct c.relname::text from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and c.relkind in ('r', 'p')
		order by 1`, pid)
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	return tables
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
