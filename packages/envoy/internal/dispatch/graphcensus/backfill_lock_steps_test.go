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

// The steps of the store's lock order, as the lock order on api/inbox_read.go's
// markInboxReadThreads states them, with the two steps a writer under the revision fence adds
// after the owner row: the document's document_block_versions row, and the projection rows it
// reads again under those two locks, `for update`, before it writes anything (heldBlockRows,
// heldBlockRefRows). A failure names the step a hold stands for with these words.
const (
	stepOwner      = "the owner row"
	stepFence      = "the revision fence's document_block_versions row"
	stepHeld       = "the projection rows, read again under the fence"
	stepNode       = "the node rows"
	stepTuple      = "the tuple append"
	stepProjection = "the projection rows"
	stepEvent      = "the project of the event"
)

// storeLockOrder is the steps in the order a writer takes them. A writer may leave steps out, and
// never takes one before a step that comes ahead of it here.
var storeLockOrder = []string{stepOwner, stepFence, stepHeld, stepNode, stepTuple, stepProjection, stepEvent}

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
// pg_locks, the tables the run's transaction holds a lock on, so a step the run took too early
// shows as a lock on that step's table. The run must never queue behind a step's row the batch does
// not take, must take every step it does take, must take them in the order storeLockOrder lists
// them in, and must take its node rows in ascending (kind, id) (requireTheStoreLockOrder). The
// subtest then reads what the run wrote, so the run it followed is the one that committed.
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
// nothing. That the update comes after the tuple append shows in the tables: the update's foreign
// key reads graph_nodes, and the run holds no lock on graph_nodes while it waits at the tuple.
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
		unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, `select 1 from block_refs where artifact_id = $1::uuid and block_id = 'only' for no key update`, document.artifactID)
	tuple := holdWrite(t, database, &batchHold{
		name: "the reference's mentions tuple", step: stepTuple, taken: true,
		statement: "insert into graph_tuples (",
		unlocked:  []string{"graph_nodes"},
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
// session. Every node row has to be taken before the first tuple: a batch that appended its
// comments' tuples before it wrote its authors' nodes would queue behind a tuple first.
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
			unlocked:  []string{"graph_tuples", "comments"},
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
		unlocked:  []string{"comments"},
	}, nil, []graphstore.Tuple{{
		FromKind: commentNodeKind, FromID: c1.id, Kind: tupleState, ToKind: graphstore.KindValue, ToID: graphstore.StateLive,
		At: now, Who: c1.by, SourceEvent: c1.event, Asserted: true,
	}})
	amyAuthored := holdWrite(t, database, &batchHold{
		name: "comment c3's authored_by tuple", step: stepTuple, taken: true,
		statement: "insert into graph_tuples (",
		unlocked:  []string{"comments"},
	}, nil, []graphstore.Tuple{{
		FromKind: commentNodeKind, FromID: c3.id, Kind: graphstore.TupleAuthoredBy, ToKind: graphstore.KindPerson, ToID: "amy",
		At: now, Who: c3.by, SourceEvent: c3.event, Asserted: true,
	}})
	row := holdRow(t, database, &batchHold{
		name: "comment c1's comments row", step: stepProjection, taken: true,
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

// batchHold is one row of a step of the store's lock order, held in a transaction of its own while
// the backfill runs, and what the test expects of the run while it waits behind it.
type batchHold struct {
	// name is the row as a failure names it, and step the step of the lock order it stands for.
	name, step string
	// taken is whether the batch takes the row. A row it does not take is held for the whole run,
	// and the run must never queue behind it.
	taken bool
	// kind and id are a node step's node, which orders it among the batch's other node rows.
	kind, id string
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

// requireTheStoreLockOrder holds the order the run queued behind holds in to the store's lock
// order. A row of a step the batch does not take must never be queued behind: the owner row comes
// first or not at all, and a batch that queued behind it took it after a step it had already
// taken. Every row of a step the batch takes must be queued behind once, or the step was not shown
// to be taken while it was held. Each step comes after every step storeLockOrder lists ahead of
// it, so every node row comes before the first tuple and every tuple before the projection rows,
// and the node rows are taken in ascending (kind, id).
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
	// A step that has to come before another and was never queued behind at all did not come
	// before it either, so it counts as taken after it.
	for _, hold := range queued {
		if !hold.taken {
			continue
		}
		position, rank := at(hold), slices.Index(storeLockOrder, hold.step)
		for _, earlier := range holds {
			if !earlier.taken || slices.Index(storeLockOrder, earlier.step) >= rank {
				continue
			}
			if before := at(earlier); before < 0 || before > position {
				t.Errorf("%s took %s (%s) before %s (%s): the store's lock order takes %s before %s (queued in order: %s)",
					batch, hold.name, hold.step, earlier.name, earlier.step, earlier.step, hold.step, order)
			}
		}
	}
	var previous *batchHold
	for _, hold := range queued {
		if !hold.taken || hold.step != stepNode {
			continue
		}
		if previous != nil && (hold.kind < previous.kind || hold.kind == previous.kind && hold.id < previous.id) {
			t.Errorf("%s took %s before %s: %s are taken in ascending (kind, id), so %s %s before %s %s (queued in order: %s)",
				batch, previous.name, hold.name, stepNode, hold.kind, hold.id, previous.kind, previous.id, order)
		}
		previous = hold
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
