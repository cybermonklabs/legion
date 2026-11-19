package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// storeStepWait bounds how long a run waits for the write under test to queue behind a hold, and
// how long a write that was let go is given to finish. Both are far above what the work takes; a
// run that passes either is the write waiting on something it should not, not a slow machine.
const storeStepWait = 30 * time.Second

// TestEveryDocsStoreWriterTakesTheStoreLockOrder is this package's half of the check
// c-lock-order. graphstore's TestEveryStoreWriteCallSiteIsListed lists every call site of the node
// and tuple store's writers, beside the test that drives each site's lock steps. Three of them
// live here, refs' BlockIndexing.Queue runs inside the tick and inside every settlement that
// writes a version, and the tick queues graphcensus's comment write, so this test drives all of
// them, along five paths. One more site lives here, createAskBlock, listed beside api's
// TestEveryObjectNodeWriterTakesTheStoreLockOrder; the two settlements below that open an ask run
// it, so they hold its node and its row too, in turn with the fence row before them and the
// follow after them:
//
//   - the block-index tick's pass over an owed document (indexOwedDocument), which takes the
//     document's owner lock in its read phase and, in its write phase, the document's
//     document_block_versions row (the block index's queueStand), the blocks' nodes in ascending
//     (kind, id), then their document_blocks rows; it then appends what the index stated under
//     the event that recorded the version, an event the log held before the tick began
//     (graphstore.QueueCaused), so it appends no event of its own and its tuples' cause is in the
//     log before any step of it;
//   - a settlement that writes a version (commitVersion): the owner lock, then, first in its write
//     phase, the document's fence row (lockSettlementFence), the block index as the tick runs it,
//     the artifact.version event, and then what the index stated under it (appendEvents);
//   - a settlement that opens an ask a session wrote as a block (createAskBlock, then
//     appendEvents): the owner lock, the fence row, the ask's node, marked for its cause before
//     the asks row that carries its id, that row, the session's follow of the ask, whose node is
//     made marked for its cause too (asks.QueueFollowAuthor), the ask.opened event, and then both
//     nodes moved onto that event with what each node write stated appended under it
//     (graphstore.QueueCaused). The edit that wrote the block wrote its version too, as an
//     agent's edit through the API does, so the settlement writes no version, which the run reads
//     back;
//   - a settlement that does both: it opens the ask of a block a session wrote in a live edit and
//     writes the version holding it, so the ask's node, its row and the follow's node come before
//     the block index's, and the fence row comes before all of them.
//   - the block-index tick's read of the events after its build's cursor (readEventsAfterCursor),
//     which takes, in its read phase, the owner lock of each comment the events name whose row
//     names no node (graphcensus.LockCommentOwners), and in its write phase its build's cursor
//     row, then marks the documents the events name in one statement that takes their
//     document_block_versions rows `for no key update`, and only then writes those comments
//     (graphcensus.CommentWrite.Queue): the comment's node and its author's in one store write,
//     their tuples, and the node named on the comment's row. The fence rows come before the nodes
//     because a settlement takes its document's fence row and then a session's node: a tick that
//     took that node first and then waited for the fence row would hold what the settlement
//     waits for.
//
// The order they are held to is the one written on api/inbox_read.go's markInboxReadThreads, which
// graphstore's TestEveryStoreWriteTakesOneLockOrder reads, as api's
// TestEveryApiStoreWriterTakesTheStoreLockOrder holds its writers to it: the owner lock first, a
// node's row before the projection row naming it, the event before the tuples it causes, several
// node rows in ascending (kind, id), and a writer under the revision fence taking the document's
// document_block_versions row before any node, tuple or projection row it writes. Each path lists
// the steps it takes in the order it takes them, and a path that takes fewer steps than the order
// lists declares only those; the list is checked against the order (requireTheDocsOrderAdmits).
// Then each step is held, with one run of the path per step: another transaction holds what the
// step takes, and once the path's backend has queued behind it the test reads the statement that
// backend waits at, which has to be the statement that takes the step, and the table locks its
// transaction holds, which have to include every earlier step's and none of a later step's
// (requireEachDocsStepIsWaitedAtInTurn). Held at the revision fence, which every path takes right
// after its owner lock or its event cursor row, the write holds no lock of a table or of a row but
// the ones the steps before the fence take and the fence's own table lock
// (requireNothingBetweenTheStepsAndTheFence). These writers run on the service's own pool, so the
// backend a run waits on is read from the holder: the one backend queued behind it. Every run is
// then held to leaving no node or tuple row with a negative cause (storetest.RequireNoMarkedCause).
//
// On a tick that owes it, the tick also runs the backfill's object pass, whose batch,
// graphcensus's writeObjectBatch, is listed beside graphcensus's
// TestEveryBackfillBatchTakesTheStoreLockOrder; that package cannot run a tick, so the pass is held
// here too, at the tick's own call site, after the event read that runs before it
// (requireTheRollTicksObjectPassLockOrder).
func TestEveryDocsStoreWriterTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newDocsWriterRig(t)
	for _, writer := range []docsWriter{rig.tick(), rig.settledVersion(), rig.settledAsk(), rig.settledAskAndVersion(), rig.tickEvents()} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheDocsOrderAdmits(t, writer)
			requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
	t.Run("the roll tick's object pass", requireTheRollTicksObjectPassLockOrder)
}

// --- the steps ----------------------------------------------------------------------------

// docsOrderStep is the step of the written order a lock step takes.
type docsOrderStep int

const (
	docsOwnerStep docsOrderStep = iota
	// docsCursorStep is the block-index tick's own event cursor row, which no writer but a tick
	// takes, so the written order names no place for it; the tick takes it before anything else
	// but the owner locks of the comments its events name.
	docsCursorStep
	docsFenceStep
	docsNodeStep
	docsProjectionStep
	docsEventStep
	docsTupleStep
)

// The lock modes a step shows in pg_locks: `for update`, `for no key update` and a foreign key's
// `for key share` take the row share mode on their table, and an insert, an update or a delete
// takes the row exclusive mode. A plain read takes the access share mode, which no step is named
// by, so it is left out of what a run reads.
const (
	docsRowShareMode     = "RowShareLock"
	docsRowExclusiveMode = "RowExclusiveLock"
)

// docsTableLock is one table lock a transaction holds, as pg_locks names it.
type docsTableLock struct {
	table, mode string
}

func (l docsTableLock) String() string { return l.mode + " on " + l.table }

// docsRowLock is one row a transaction has locked, named by its table and its primary key, its
// columns' values joined by ", " (rowsLocked).
type docsRowLock struct {
	table, key string
}

func (l docsRowLock) String() string { return fmt.Sprintf("the %s row %q", l.table, l.key) }

// docsStep is one lock step of a write path.
type docsStep struct {
	name string
	// order is the step of the written order this one takes.
	order docsOrderStep
	// names is, for a projection row, the node step whose node the row's foreign key names.
	names string
	// caused says the step appends tuples that carry the event the path appends.
	caused bool
	// fenced says the step writes part of what a version's block index states about the
	// document, which a writer under the revision fence writes only once it holds the fence's row,
	// or, in the block-index tick's read of the events after its cursor, a node, a tuple or a
	// projection row of a comment those events name, which that tick writes only once its marking
	// holds the fence rows it marks (readEventsAfterCursor).
	fenced bool
	// hold takes, in holder, what the step takes, so the write waits for holder at this step.
	hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun)
	// at is the head of the statement that takes the step, the one the write waits at.
	at string
	// takes are the table locks the step shows once it has run.
	takes []docsTableLock
	// row is the one row the step locks, for a step whose statement names that row by its key:
	// the tick's event cursor row. A write held at the revision fence has locked no row but the
	// ones the steps before the fence name (requireNothingBetweenTheStepsAndTheFence).
	row docsRowLock
}

// docsWriter is one write path under test: the steps it takes, in the order it takes them, and
// how one run of it is readied.
type docsWriter struct {
	name  string
	steps []docsStep
	// ready writes what one run needs and answers the run, not yet started.
	ready func(t *testing.T, ctx context.Context) docsRun
}

// docsRun is one run of a write path.
type docsRun struct {
	// document is the document the run writes.
	document string
	// made is the session node the run's follow makes, which no write before the run recorded,
	// and the zero node for a path that follows nobody.
	made graphstore.Node
	// block is the node of the block the run's version adds, where the test knows its address
	// before the run, and the zero node otherwise.
	block graphstore.Node
	// write runs the path to its commit and answers how it ended. It runs off the test's
	// goroutine, so it answers its failures rather than failing the test.
	write func() error
}

// The statement heads a step is read at, lower-cased with their whitespace folded.
var (
	// docsOwnerStatement is store.LockOwners' advisory lock of an owner, which lockArtifactOwner
	// and graphcensus.LockCommentOwners take.
	docsOwnerStatement = store.OwnerLockStatement
)

const (
	// docsStandStatement is the block index's record of the version it indexes on the
	// document's document_block_versions row (refs' queueStand), the first statement of the tick's
	// write phase that takes that row.
	docsStandStatement = "insert into document_block_versions (artifact_id, version, store_version, windows_owed_at)"
	// docsSettlementFenceStatement is a settlement's take of its document's
	// document_block_versions row, the first statement of its write phase (lockSettlementFence).
	docsSettlementFenceStatement = "select 1 from document_block_versions where artifact_id = $1 for no key update"
	// docsNodeStatement is graphstore's node write (queueNodes), which the block index, the
	// comment write and createAskBlock queue.
	docsNodeStatement = "insert into graph_nodes ("
	// docsBlockRowStatement is the block index's write of the version's document_blocks rows.
	docsBlockRowStatement = "insert into document_blocks ("
	// docsAskRowStatement is createAskBlock's insert of the asks row of a new ask block.
	docsAskRowStatement = "insert into asks ("
	// docsFollowStatement is asks' follow statement: the follower row and the session's node.
	docsFollowStatement = "with followed as ("
	// docsEventStatement is events.Broker's append, after its owner row's lock.
	docsEventStatement = "with appended as ( insert into events ("
	// docsTupleStatement is graphstore's tuple append for a write that wrote nodes, which states
	// their base attributes and moves them onto the cause in the same statement (queueAppend).
	docsTupleStatement = "with written as ("
	// docsCursorStatement is the block-index tick's move of its build's event cursor row, which
	// records whether the tick found another build and the debt of the object pass with it.
	docsCursorStatement = "insert into block_index_event_cursors (build_commit, event_id, another_build_seen, object_nodes_owed_at)"
	// docsMarkingStatement is the block-index tick's marking (markVersionedDocuments), which takes
	// the document_block_versions row of each document it marks `for no key update`.
	docsMarkingStatement = "with held as ( select b.artifact_id from document_block_versions b"
	// docsCommentRowStatement is the comment write's naming of each comment's node on its row
	// (graphcensus.CommentWrite.Queue).
	docsCommentRowStatement = "update comments c set node_kind"
)

// --- the paths ----------------------------------------------------------------------------

// tick is the block-index tick's pass over one owed document (indexOwedDocument), run as the tick
// runs it for each document it lists. Each run's document is a new one a build without the block
// index wrote: its version 1 and the artifact.created that recorded it, and no block index. Its
// fence row is there, owed, from the version's insert, and is held as a row; its block nodes and
// rows are new, and are held as their tables.
func (r *docsWriterRig) tick() docsWriter {
	return docsWriter{
		name: "indexOwedDocument",
		steps: []docsStep{
			r.ownerStep(),
			{
				name: "the revision fence", order: docsFenceStep,
				hold: holdFenceRow, at: docsStandStatement,
				takes: []docsTableLock{{"document_block_versions", docsRowExclusiveMode}},
			},
			docsBlockNodesStep(),
			docsBlockRowsStep("the block nodes"),
			{
				// The tuples carry the artifact.created that recorded the version, which the log
				// held before the tick began, so no event of this path orders them.
				name: "the block index's tuple append", order: docsTupleStep, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			slug := r.fresh("lock-steps-owed")
			document := createOwedDocument(t, r.database, r.service, "CORE-1", "", slug, "# "+slug+"\n\nA paragraph the tick indexes.\n", "")
			settle(t, r.database, document)
			r.recordEvent(t, ctx, "artifact.created", map[string]any{"artifact": map[string]any{"id": document}})
			r.awaitDurable(t, document)
			return docsRun{document: document, write: func() error {
				indexed, liveAhead, err := r.service.indexOwedDocument(ctx, owedDocument{id: document, version: 1})
				if err != nil {
					return err
				}
				if !indexed || liveAhead {
					return fmt.Errorf("the tick answered indexed=%v live-ahead=%v, want the document indexed", indexed, liveAhead)
				}
				return nil
			}}
		},
	}
}

// tickEvents is the block-index tick's read of the events after its build's cursor
// (readEventsAfterCursor): the owner lock of each comment the events name whose row names no node
// (graphcensus.LockCommentOwners), then its cursor row, then the marking statement's
// document_block_versions rows `for no key update`, then those comments
// (graphcensus.CommentWrite.Queue) - the comment's node and its author's in one store write, their
// tuples, and the node named on the comment's row - all in one transaction. Each run first runs
// that read once, so its build's cursor row is there and stands past every event before the run's
// own. It then writes as the release before this one writes them a document whose version 1 no
// build of this code has indexed, with the artifact.created that recorded it, and a comment whose
// row names no node, with the comment.created that stored it, and waits for the event log's safe
// horizon to pass both. The owner lock is held as the advisory lock; the cursor row and the fence
// row are there and are held as rows; the comment's node is new, and is held as a row another
// transaction inserted and has not committed; the tuple append and the comments row are held as
// their tables.
func (r *docsWriterRig) tickEvents() docsWriter {
	awaitHorizon := func(t *testing.T, ctx context.Context) {
		t.Helper()
		awaited, cancel := context.WithTimeout(ctx, storeStepWait)
		defer cancel()
		if _, err := r.service.blockIndexHorizon().Await(awaited); err != nil {
			t.Fatalf("await the event log's safe horizon: %v", err)
		}
	}
	return docsWriter{
		name: "readEventsAfterCursor",
		steps: []docsStep{
			r.ownerStep(),
			{
				name: "the event cursor", order: docsCursorStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
					holdDocsRow(t, ctx, holder, `select 1 from block_index_event_cursors where build_commit = $1 for update`, "")
				},
				at: docsCursorStatement,
				// The row is there, so the insert that moves it waits on its conflict with the
				// held row and then updates it.
				takes: []docsTableLock{{"block_index_event_cursors", docsRowExclusiveMode}},
				row:   docsRowLock{"block_index_event_cursors", ""},
			},
			{
				name: "the revision fence", order: docsFenceStep,
				hold: holdFenceRow, at: docsMarkingStatement,
				takes: []docsTableLock{{"document_block_versions", docsRowShareMode}, {"document_block_versions", docsRowExclusiveMode}},
			},
			{
				name: "the comment's node", order: docsNodeStep, fenced: true,
				hold: holdDocsNewNode, at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				// The tuples carry the comment.created that stored the comment, which the log
				// held before the tick began, so no event of this path orders them.
				name: "the comment's tuples", order: docsTupleStep, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
			{
				name: "the comment's row", order: docsProjectionStep, names: "the comment's node", fenced: true,
				hold: holdDocsTableWrites("comments"), at: docsCommentRowStatement,
				takes: []docsTableLock{{"comments", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			awaitHorizon(t, ctx)
			if _, _, err := r.service.readEventsAfterCursor(ctx, &BlockIndexTick{}); err != nil {
				t.Fatalf("read the events before the run's: %v", err)
			}
			slug := r.fresh("lock-steps-unseen")
			document := createOwedDocument(t, r.database, r.service, "CORE-1", "", slug, "# "+slug+"\n\nA paragraph the release before wrote.\n", "")
			settle(t, r.database, document)
			r.recordEvent(t, ctx, "artifact.created", map[string]any{"artifact": map[string]any{"id": document}})
			author := model.Actor{Kind: "user", ID: r.fresh("lock-steps-commenter")}
			encoded, err := json.Marshal(author)
			if err != nil {
				t.Fatalf("encode the comment's author: %v", err)
			}
			var comment string
			var createdAt time.Time
			if err := r.database.Pool.QueryRow(ctx, `
				insert into comments (issue_key, author, body) values ('CORE-1', $1, 'A remark the release before wrote.')
				returning id::text, created_at
			`, encoded).Scan(&comment, &createdAt); err != nil {
				t.Fatalf("write a comment as the release before writes one: %v", err)
			}
			r.recordEvent(t, ctx, "comment.created", map[string]any{"id": comment, "issue_key": "CORE-1", "author": author})
			r.awaitDurable(t, document)
			awaitHorizon(t, ctx)
			return docsRun{
				document: document,
				made: graphstore.Node{
					Kind: graphstore.KindComment, ID: comment, Supertag: graphstore.KindComment,
					Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
					MadeAt: createdAt, MadeBy: author, SourceEvent: graphstore.NoEvent,
				},
				write: func() error {
					var tick BlockIndexTick
					if _, _, err := r.service.readEventsAfterCursor(ctx, &tick); err != nil {
						return err
					}
					if tick.Marked != 1 || tick.Comments != 1 {
						return fmt.Errorf("the tick marked %d document(s) and wrote %d comment(s), want the run's one of each", tick.Marked, tick.Comments)
					}
					return nil
				},
			}
		},
	}
}

// settledVersion is a settlement that writes the version of a person's live edit (commitVersion).
// Each run adds a paragraph to the rig's versioned document through the document service, which
// leaves the live document ahead of its latest version, and settles it. The fence row is there from
// the document's first version, so it is held as a row.
func (r *docsWriterRig) settledVersion() docsWriter {
	return docsWriter{
		name: "commitVersion",
		steps: []docsStep{
			r.ownerStep(),
			settlementFenceStep(),
			docsBlockNodesStep(),
			docsBlockRowsStep("the block nodes"),
			docsEventAppendStep(),
			{
				name: "the block index's tuple append", order: docsTupleStep, caused: true, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			paragraph := r.fresh("A paragraph the settlement versions, number")
			if _, err := joinedApplyOps(r.service, r.versioned, []model.EditOp{{Op: "insert", After: "end", Markdown: paragraph + "."}},
				model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
				t.Fatalf("edit the versioned document: %v", err)
			}
			r.awaitDurable(t, r.versioned)
			versions := r.versionCount(t, r.versioned)
			return docsRun{document: r.versioned, write: func() error {
				if err := r.settle(ctx, r.versioned); err != nil {
					return err
				}
				if now, err := r.versionsOf(ctx, r.versioned); err != nil || now != versions+1 {
					return fmt.Errorf("the settlement left %d versions (%v), want the %d before it and its own", now, err, versions)
				}
				return nil
			}}
		},
	}
}

// settledAsk is a settlement that opens an ask a session wrote as a block (createAskBlock, then
// appendEvents). Each run first writes the block as a new session's edit with its version, in one
// transaction, as POST /api/v1/artifacts/{id}/edits writes it (editAsVersion); the settlement then
// writes the ask's node and its row, follows the ask for that session, and appends ask.opened, and
// writes no version: the edit versioned the block's words already. It takes the fence row all the
// same, first in its write phase, since whether it writes a version is decided only after the
// follow (lockSettlementFence). The ask's node is the settlement's first write of graph_nodes and
// its row the first write of asks, so each is held as its table; the session is new, so its node,
// which comes after the ask's, is held as a row another transaction inserted and has not
// committed.
func (r *docsWriterRig) settledAsk() docsWriter {
	return docsWriter{
		name: "appendEvents",
		steps: []docsStep{
			r.ownerStep(),
			settlementFenceStep(),
			askNodeStep(),
			askRowStep(),
			askerNodeStep(),
			docsEventAppendStep(),
			{
				name: "the tuple append", order: docsTupleStep, caused: true, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			session := r.fresh("lock-steps-asker")
			block := r.fresh("lock-ask")
			asker := model.Actor{Kind: "session", ID: session}
			r.editAsVersion(t, ctx, r.asked, asker, askBlockMarkdown(block))
			r.awaitDurable(t, r.asked)
			versions := r.versionCount(t, r.asked)
			return docsRun{
				document: r.asked,
				made:     graphstore.SessionNode(session, asker, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					if err := r.settle(ctx, r.asked); err != nil {
						return err
					}
					if now, err := r.versionsOf(ctx, r.asked); err != nil || now != versions {
						return fmt.Errorf("the settlement left %d versions (%v), want the %d before it: it was to open the ask and write no version", now, err, versions)
					}
					return r.askOpenedAndCaused(ctx, r.asked, session, block)
				},
			}
		},
	}
}

// settledAskAndVersion is a settlement that both opens an ask a session wrote as a block and
// writes the version that holds it (createAskBlock and appendEvents beside commitVersion). Each run
// adds the block as a new session's live edit, with no version of its own, so the settlement
// writes the ask's node and its row, follows the ask for that session, and then writes the
// version, whose block index writes the block's node: the ask's node, its row and the follow's
// node come before the block's, and the settlement takes the fence row before all of them
// (lockSettlementFence). The ask's node and its row are the settlement's first writes of
// graph_nodes and of asks, so each is held as its table. The session's node and the ask block's
// node come after the ask's, and are each held as a new row another transaction inserted and has
// not committed: the block's address is the document's ref_key and the block's own id, both known
// before the run. The block rows, the event and the tuple append are held as their tables, so the
// run stops at the first of each: the version's artifact.version and the tuples under it. The
// ask.opened and the follow's tuples after them are the steps the path without a version holds.
func (r *docsWriterRig) settledAskAndVersion() docsWriter {
	return docsWriter{
		name: "appendEvents beside commitVersion",
		steps: []docsStep{
			r.ownerStep(),
			settlementFenceStep(),
			askNodeStep(),
			askRowStep(),
			askerNodeStep(),
			{
				name: "the ask block's node", order: docsNodeStep, fenced: true,
				hold: holdDocsBlockNode, at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			docsBlockRowsStep("the ask block's node"),
			docsEventAppendStep(),
			{
				name: "the block index's tuple append", order: docsTupleStep, caused: true, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			session := r.fresh("lock-steps-asker")
			block := r.fresh("lock-ask")
			asker := model.Actor{Kind: "session", ID: session}
			if _, err := joinedApplyOps(r.service, r.askedLive, []model.EditOp{{Op: "insert", After: "end", Markdown: askBlockMarkdown(block)}},
				asker, nil); err != nil {
				t.Fatalf("write ask block %s as %s: %v", block, session, err)
			}
			r.awaitDurable(t, r.askedLive)
			versions := r.versionCount(t, r.askedLive)
			at := time.Now().UTC()
			return docsRun{
				document: r.askedLive,
				made:     graphstore.SessionNode(session, asker, at, graphstore.NoEvent),
				block: graphstore.Node{
					Kind: refs.BlockNodeKind, ID: r.askedLiveRefKey + "#" + block, Supertag: refs.BlockSupertag,
					Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
					MadeAt: at, MadeBy: asker, SourceEvent: graphstore.NoEvent,
				},
				write: func() error {
					if err := r.settle(ctx, r.askedLive); err != nil {
						return err
					}
					if now, err := r.versionsOf(ctx, r.askedLive); err != nil || now != versions+1 {
						return fmt.Errorf("the settlement left %d versions (%v), want the %d before it and its own", now, err, versions)
					}
					return r.askOpenedAndCaused(ctx, r.askedLive, session, block)
				},
			}
		},
	}
}

// askBlockMarkdown is an ask block of id block as an agent writes one into a document.
func askBlockMarkdown(block string) string {
	return ":::ask{#" + block + " urgency=\"high\" multiple=\"false\"}\n" +
		"Which transport does " + block + " take?\n\n- REST: Matches the platform\n- gRPC: Adds streaming\n:::\n"
}

// askOpenedAndCaused answers nil where the settlement opened the ask of block in document and
// moved its asker's node, the session's, off the sentinel onto the ask.opened that records it
// (appendEvents), so the run the test followed is the one that committed.
func (r *docsWriterRig) askOpenedAndCaused(ctx context.Context, document, session, block string) error {
	var caused int64
	if err := r.database.Pool.QueryRow(ctx, `
		select n.source_event from asks a
		join graph_nodes n on n.kind = $3 and n.id = $2
		where a.block_artifact_id = $1 and a.block_id = $4
	`, document, session, graphstore.KindSession, block).Scan(&caused); err != nil {
		return fmt.Errorf("read the ask of block %s and its asker's node: %w", block, err)
	}
	if caused == graphstore.NoEvent {
		return fmt.Errorf("the asker's node of block %s is still at the sentinel: appendEvents moved nothing", block)
	}
	return nil
}

// ownerStep is the owner lock of the rig's documents and their comments, issue CORE-1's, which
// lockArtifactOwner and graphcensus.LockCommentOwners take first: a transaction advisory lock,
// which locks no table and no row.
func (r *docsWriterRig) ownerStep() docsStep {
	return docsStep{
		name: "the owner lock", order: docsOwnerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, store.IssueOwner("CORE-1")); err != nil {
				t.Fatalf("hold issue CORE-1's owner lock: %v", err)
			}
		},
		at: docsOwnerStatement,
	}
}

// settlementFenceStep is the document's fence row, which a settlement takes first in its write
// phase (lockSettlementFence) and which is there from the document's first version, so it is held
// as a row.
func settlementFenceStep() docsStep {
	return docsStep{
		name: "the revision fence", order: docsFenceStep,
		hold: holdFenceRow, at: docsSettlementFenceStatement,
		takes: []docsTableLock{{"document_block_versions", docsRowShareMode}},
	}
}

// askNodeStep is the node of the ask a settlement opens for a new ask block (createAskBlock),
// marked for its cause before the asks row that carries its id. The ask's id is drawn in that
// call, so no hold can name the node's row before the run; the write is the settlement's first of
// graph_nodes, so the table is held. It is one of the settlement's node rows, which the fence row
// comes before.
func askNodeStep() docsStep {
	return docsStep{
		name: "the ask's node", order: docsNodeStep, fenced: true,
		hold: holdDocsTableWrites("graph_nodes"), at: docsNodeStatement,
		takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
	}
}

// askRowStep is the asks row of that ask, the row naming the ask's node by its id: asks carries no
// foreign key into graph_nodes, but its id is the node's address, and createAskBlock writes the
// node first. The settlement reads its document's asks `for no key update` before it
// (loadAskBlocks), a row share lock the table hold does not conflict with, and writes no ask
// before this insert, so the table is held.
func askRowStep() docsStep {
	return docsStep{
		name: "the ask's row", order: docsProjectionStep, names: "the ask's node", fenced: true,
		hold: holdDocsTableWrites("asks"), at: docsAskRowStatement,
		takes: []docsTableLock{{"asks", docsRowExclusiveMode}},
	}
}

// askerNodeStep is a settlement's follow of the ask it opens for the session that wrote the
// block (asks.QueueFollowAuthor): the follower row and the session's node, marked for its cause,
// in one statement, after the ask's node and its row. It is one of the settlement's node rows,
// which the fence row comes before.
func askerNodeStep() docsStep {
	return docsStep{
		name: "the asker's node", order: docsNodeStep, fenced: true,
		hold: holdDocsNewNode, at: docsFollowStatement,
		takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
	}
}

// docsBlockNodesStep is the block index's node write: the nodes of the blocks the version changed,
// in one statement and in ascending (kind, id). Their ids are drawn from the version, and the
// write is the first to name them, so the table is held.
func docsBlockNodesStep() docsStep {
	return docsStep{
		name: "the block nodes", order: docsNodeStep, fenced: true,
		hold: holdDocsTableWrites("graph_nodes"), at: docsNodeStatement,
		takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
	}
}

// docsBlockRowsStep is the block index's document_blocks rows, each naming its block's node:
// names is the node step that writes those nodes.
func docsBlockRowsStep(names string) docsStep {
	return docsStep{
		name: "the block rows", order: docsProjectionStep, names: names, fenced: true,
		hold: holdDocsTableWrites("document_blocks"), at: docsBlockRowStatement,
		takes: []docsTableLock{{"document_blocks", docsRowExclusiveMode}},
	}
}

// docsEventAppendStep is the event's append, after the lock of its owner's row, which an issue's
// foreign key reads before it as well, so no table lock tells it apart. The events table is held,
// so what it waits at is its own insert.
func docsEventAppendStep() docsStep {
	return docsStep{
		name: "the event", order: docsEventStep,
		hold: holdDocsTableWrites("events"), at: docsEventStatement,
		takes: []docsTableLock{{"events", docsRowExclusiveMode}},
	}
}

// --- what a step is held by ---------------------------------------------------------------

// holdDocsRow takes the lock statement names on one row, which has to be there.
func holdDocsRow(t *testing.T, ctx context.Context, holder pgx.Tx, statement, key string) {
	t.Helper()
	tag, err := holder.Exec(ctx, statement, key)
	if err != nil {
		t.Fatalf("hold the row %s with %q: %v", key, statement, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("hold the row %s with %q: it locked %d rows, want one", key, statement, tag.RowsAffected())
	}
}

// holdFenceRow holds the run's document's document_block_versions row at the level the fence
// takes it.
func holdFenceRow(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun) {
	t.Helper()
	holdDocsRow(t, ctx, holder, `select 1 from document_block_versions where artifact_id = $1::uuid for no key update`, run.document)
}

// holdDocsTableWrites holds a table in share mode, which conflicts with the row exclusive lock
// every insert, update and delete takes and with no read and no row lock, so the write stops at
// its first write of the table and nowhere before it.
func holdDocsTableWrites(table string) func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun) {
	return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
		t.Helper()
		if _, err := holder.Exec(ctx, "lock table "+table+" in share mode"); err != nil {
			t.Fatalf("hold the writes of %s: %v", table, err)
		}
	}
}

// holdDocsNewNode writes the run's new node in holder and leaves it uncommitted: the write's
// upsert of the same key waits to learn whether this row commits.
func holdDocsNewNode(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun) {
	t.Helper()
	if err := graphstore.Write(ctx, holder, []graphstore.Node{run.made}, nil); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", run.made.Kind, run.made.ID, err)
	}
}

// holdDocsBlockNode writes the node of the block the run's version adds in holder and leaves it
// uncommitted, as holdDocsNewNode does the session's.
func holdDocsBlockNode(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun) {
	t.Helper()
	if err := graphstore.Write(ctx, holder, []graphstore.Node{run.block}, nil); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", run.block.Kind, run.block.ID, err)
	}
}

// --- the written order --------------------------------------------------------------------

// requireTheDocsOrderAdmits checks a path's steps, in the order the path takes them, against the
// invariants on markInboxReadThreads: the owner lock first, a node before the projection row
// naming it, the event before every step appending tuples that carry it, and the revision fence's
// row before every step of the block statement it guards, and before every write of a comment the
// block-index tick's events name. The tick's own event cursor row, which the invariants do not
// name, comes before every other step of its path but the owner lock. A path skips the steps it
// does not take, and is checked on the order of those it does. Several nodes of one write in
// ascending (kind, id) is graphstore's own (queueNodes, WriteArms): every node step here is one
// statement.
func requireTheDocsOrderAdmits(t *testing.T, writer docsWriter) {
	t.Helper()
	position := make(map[string]int, len(writer.steps))
	event, fence := -1, -1
	for index, step := range writer.steps {
		position[step.name] = index
		switch step.order {
		case docsEventStep:
			event = index
		case docsFenceStep:
			fence = index
		}
	}
	for index, step := range writer.steps {
		if step.order == docsOwnerStep && index != 0 {
			t.Errorf("%s takes %s as its step %d: it comes first", writer.name, step.name, index+1)
		}
		if step.order == docsCursorStep && index != 0 && (index != 1 || writer.steps[0].order != docsOwnerStep) {
			t.Errorf("%s takes %s as its step %d: it comes first but for the owner lock", writer.name, step.name, index+1)
		}
		if step.order == docsProjectionStep && step.names != "" {
			node, listed := position[step.names]
			switch {
			case !listed || writer.steps[node].order != docsNodeStep:
				t.Errorf("%s's %s names %q, which is no node step of it", writer.name, step.name, step.names)
			case node > index:
				t.Errorf("%s takes %s before %s, the node its foreign key names", writer.name, step.name, step.names)
			}
		}
		if step.caused && (event < 0 || event > index) {
			t.Errorf("%s takes %s, whose tuples carry its event, before it appends that event", writer.name, step.name)
		}
		if step.fenced && (fence < 0 || fence > index) {
			t.Errorf("%s takes %s, which the revision fence guards, before it takes the fence's document_block_versions row", writer.name, step.name)
		}
	}
}

// requireEachDocsStepIsWaitedAtInTurn runs the path once per step with that step held, and holds
// the stopped write to the statement that takes the step, to the locks of the steps before it,
// and to no lock of a later step, as api's requireEachStepIsWaitedAtInTurn does. A lock several
// steps show is read for the first of them, the one that takes it. A later step is read by its
// rows (rowsLocked) and by its table locks of every mode but the two a batch's preparation takes
// (docsParseTimeMode). Every event these paths append is an issue's, so none of them locks a
// projects row. Held at the revision fence, the write is held to taking no lock besides those
// as well (requireNothingBetweenTheStepsAndTheFence).
func requireEachDocsStepIsWaitedAtInTurn(t *testing.T, ctx context.Context, rig *docsWriterRig, writer docsWriter) {
	t.Helper()
	first := map[docsTableLock]int{}
	for position, step := range writer.steps {
		for _, lock := range step.takes {
			if _, taken := first[lock]; !taken {
				first[lock] = position
			}
		}
	}
	for index, step := range writer.steps {
		run := writer.ready(t, ctx)
		holder, release := rig.beginHolder(t, ctx)
		step.hold(t, ctx, holder, run)
		done := make(chan error, 1)
		go func() { done <- run.write() }()
		backend := rig.queuedBackend(t, ctx, holder, done, writer.name+" held at "+step.name)

		running := rig.runningStatement(t, ctx, backend)
		held := rig.locksHeld(t, ctx, backend)
		t.Logf("held at %s, %s waits at: %s", step.name, writer.name, running)
		t.Logf("held at %s, %s holds: %v", step.name, writer.name, held)
		if !strings.HasPrefix(running, docsNormalizedStatement(step.at)) {
			t.Errorf("%s held at %s waits at %q, which is not the statement that takes %s (%q)",
				writer.name, step.name, running, step.name, step.at)
		}
		for position, other := range writer.steps {
			for _, lock := range other.takes {
				if first[lock] != position || position == index {
					continue
				}
				switch {
				case position < index && !slices.Contains(held, lock):
					t.Errorf("%s held at %s holds no %s, which %s takes, an earlier step: it reached %s before it took %s (it holds %v)",
						writer.name, step.name, lock, other.name, step.name, other.name, held)
				case position > index:
					// A table lock of a mode no writer's lock conflicts with is taken when pgx
					// prepares the batch, so it says nothing of order (docsParseTimeMode); any
					// stronger mode held before its step still fails.
					if !docsParseTimeMode(lock.mode) && slices.Contains(held, lock) {
						t.Errorf("%s held at %s already holds %s, which %s takes, a later step: it took %s before %s (it holds %v)",
							writer.name, step.name, lock, other.name, other.name, step.name, held)
					}
					if locked := rig.rowsLocked(t, ctx, backend, []string{lock.table}); len(locked) > 0 {
						t.Errorf("%s held at %s has locked %v, which %s takes, a later step: it took %s before %s",
							writer.name, step.name, locked, other.name, other.name, step.name)
					}
				}
			}
		}
		// An event's append moves its owner's last_seq in one statement whose arms update issues,
		// artifacts and projects, so every append holds the row exclusive lock of projects with
		// no row of it. What an issue's event must not take is a projects row.
		if locked := rig.rowsLocked(t, ctx, backend, []string{"projects"}); len(locked) > 0 {
			t.Errorf("%s held at %s has locked %v: it took the project of its event, which an issue's event does not name",
				writer.name, step.name, locked)
		}
		if step.order == docsFenceStep {
			rig.requireNothingBetweenTheStepsAndTheFence(t, ctx, writer, index, backend, held)
		}
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s, once let go at %s: %v", writer.name, step.name, err)
			}
		case <-time.After(storeStepWait):
			t.Fatalf("%s did not finish within %s of being let go at %s", writer.name, storeStepWait, step.name)
		}
		storetest.RequireNoMarkedCause(t, rig.database, writer.name+" let go at "+step.name)
	}
}

// docsParseTimeMode says a table lock of mode tells nothing of the order a write ran its
// statements in: RowShareLock, which a `for update`, `for no key update`, `for share` or a foreign
// key's `for key share` takes on its table, and RowExclusiveLock, which an insert, an update or a
// delete takes. Each write phase is one pipelined batch (store.WriteBatch), and pgx prepares every
// statement its connection has not cached in one round trip before the first of them runs
// (conn.go, sendBatchExtendedWithDescription); Postgres takes a statement's table locks when it
// parses it and keeps them to the end of the transaction. So a write stopped at one step may
// already hold those two modes on the tables of its later statements without having run them. No
// writer's lock conflicts with either mode - only a `lock table` or a schema change does - so
// neither can make a writer wait, and the order that can is the rows' (rowsLocked). A stronger
// mode is taken only by a statement that asks for it, and held before its step it still fails.
func docsParseTimeMode(mode string) bool {
	return mode == docsRowShareMode || mode == docsRowExclusiveMode
}

// requireNothingBetweenTheStepsAndTheFence holds a write stopped at the revision fence to the
// locks of the steps before it and the fence's own table lock, which its statement takes before
// it waits on the row, and to nothing more. A settlement takes the fence first in its write phase
// (lockSettlementFence), the tick's pass over an owed document first in its write phase too, and
// its read of the events right after its event cursor row, so a lock the write holds there that
// none of those steps takes is one it took between the step before the fence and the fence, and
// the failure names it. It reads both kinds of lock: the table locks the transaction holds of a
// mode a statement asks for when it runs (docsParseTimeMode), and the rows it has locked
// (rowsLocked), each of which has to be a row a step before the fence names (docsStep's row),
// since a lock of another row of a table an earlier step locks shows no table lock of its own.
// Only the fence is held to this: past it, a path takes locks no step of
// the written order names - a version's row, an ask's row, the share of a row a foreign key reads.
func (r *docsWriterRig) requireNothingBetweenTheStepsAndTheFence(t *testing.T, ctx context.Context, writer docsWriter, index int, backend uint32, held []docsTableLock) {
	t.Helper()
	step := writer.steps[index]
	before := "the start of its transaction"
	if index > 0 {
		before = writer.steps[index-1].name
	}
	taken := map[docsTableLock]bool{}
	for _, earlier := range writer.steps[:index+1] {
		for _, lock := range earlier.takes {
			taken[lock] = true
		}
	}
	var named []docsRowLock
	for _, earlier := range writer.steps[:index] {
		if earlier.row.table != "" {
			named = append(named, earlier.row)
		}
	}
	var tables []string
	for _, lock := range held {
		if !slices.Contains(tables, lock.table) {
			tables = append(tables, lock.table)
		}
		if !taken[lock] && !docsParseTimeMode(lock.mode) {
			t.Errorf("%s held at %s holds %s, which neither %s nor a step before it takes: it took that lock between %s and %s, which it takes right after %s (it holds %v)",
				writer.name, step.name, lock, step.name, before, step.name, before, held)
		}
	}
	locked := r.rowsLocked(t, ctx, backend, tables)
	t.Logf("held at %s, %s has locked: %v", step.name, writer.name, locked)
	for _, row := range locked {
		if !slices.Contains(named, row) {
			t.Errorf("%s held at %s has locked %s, which no step before it locks: it took that lock between %s and %s, which it takes right after %s (it has locked %v)",
				writer.name, step.name, row, before, step.name, before, locked)
		}
	}
}

// --- the rig ------------------------------------------------------------------------------

// docsWriterRig is the document service the paths run in, whose settle delay is an hour so no
// timer settles a document under a hold (blockIndexService), with three documents of issue CORE-1
// written through the request path: versioned, whose settlements write versions; asked, whose
// settlements open asks; and askedLive, whose settlements open asks and write versions, with the
// ref_key every one of its blocks' node addresses starts with.
type docsWriterRig struct {
	database        *store.Store
	service         *Service
	versioned       string
	asked           string
	askedLive       string
	askedLiveRefKey string
	// named counts the fresh names the rig has handed out.
	named int
}

func newDocsWriterRig(t *testing.T) *docsWriterRig {
	t.Helper()
	database, service := blockIndexService(t)
	rig := &docsWriterRig{database: database, service: service}
	rig.versioned = writeDocumentThroughTheRequestPath(t, service, database, "lock-steps-versioned", "# Versioned\n\nThe first paragraph.\n")
	rig.asked = writeDocumentThroughTheRequestPath(t, service, database, "lock-steps-asked", "# Asked\n\nThe first paragraph.\n")
	rig.askedLive = writeDocumentThroughTheRequestPath(t, service, database, "lock-steps-asked-live", "# Asked live\n\nThe first paragraph.\n")
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, rig.askedLive).Scan(&rig.askedLiveRefKey); err != nil {
		t.Fatalf("read the ref_key of %s: %v", rig.askedLive, err)
	}
	// Each document's seed leaves it owing a settlement; the runs settle it themselves.
	for _, document := range []string{rig.versioned, rig.asked, rig.askedLive} {
		rig.awaitDurable(t, document)
		if err := rig.settle(ctx, document); err != nil {
			t.Fatalf("settle the seed of %s: %v", document, err)
		}
	}
	return rig
}

// fresh is a name no run has used.
func (r *docsWriterRig) fresh(prefix string) string {
	r.named++
	return fmt.Sprintf("%s-%d", prefix, r.named)
}

// awaitDurable waits until every live update of document has reached the service's persistence
// and landed, so no append of the room's own runs while a step is held.
func (r *docsWriterRig) awaitDurable(t *testing.T, document string) {
	t.Helper()
	awaitRoomAppends(t, r.service, document, "reach persistence", r.service.waitForPendingUpdates)
	awaitRoomAppends(t, r.service, document, "become durable", r.service.waitForDurableAppends)
}

// settle runs the document's settlement as its timer does, at the generation its room stands at
// now, and answers why it stopped short where it did.
func (r *docsWriterRig) settle(ctx context.Context, document string) error {
	state := r.service.room(document)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	return r.service.settleRoomWithin(ctx, document, generation, false).err
}

// recordEvent appends one event of issue CORE-1 and commits it.
func (r *docsWriterRig) recordEvent(t *testing.T, ctx context.Context, kind string, payload map[string]any) int64 {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the %s event's payload: %v", kind, err)
	}
	var event int64
	if err := pgx.BeginFunc(ctx, r.database.Pool, func(tx pgx.Tx) error {
		event, err = documentEvent(ctx, tx, kind, encoded)
		return err
	}); err != nil {
		t.Fatalf("record the %s event: %v", kind, err)
	}
	return event
}

// editAsVersion inserts markdown at the end of document as actor's edit and versions it in the
// same transaction, as POST /api/v1/artifacts/{id}/edits does: the edit, its version, the
// version's event, and what the version's block index stated appended under that event.
func (r *docsWriterRig) editAsVersion(t *testing.T, ctx context.Context, document string, actor model.Actor, markdown string) {
	t.Helper()
	tx, err := r.service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := r.service.Join(ctx, tx)
	defer ledger.Discard()
	if edit, err := r.service.ApplyOps(joined, document, []model.EditOp{{Op: "insert", After: "end", Markdown: markdown}}, actor, nil); err != nil || !edit.Changed {
		t.Fatalf("edit %s as %s (changed %v): %v", document, actor.ID, edit.Changed, err)
	}
	written, err := r.service.SnapshotVersion(joined, document, actor)
	if err != nil || !written.Wrote {
		t.Fatalf("version the edit of %s (wrote %v): %v", document, written.Wrote, err)
	}
	payload, err := json.Marshal(map[string]any{"artifact_id": document, "version": map[string]any{"number": written.Version.Number}})
	if err != nil {
		t.Fatalf("encode the version event's payload: %v", err)
	}
	event := appendDocumentEvent(t, ctx, tx, "artifact.version", payload)
	if err := causeInWritePhase(joined, tx, written.Blocks, event); err != nil {
		t.Fatalf("append what the version's block index stated: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
}

// versionCount is how many versions document holds, failing the test where it cannot read them.
func (r *docsWriterRig) versionCount(t *testing.T, document string) int {
	t.Helper()
	versions, err := r.versionsOf(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

// versionsOf is how many versions document holds.
func (r *docsWriterRig) versionsOf(ctx context.Context, document string) (int, error) {
	var versions int
	if err := r.database.Pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, document).Scan(&versions); err != nil {
		return 0, fmt.Errorf("count the versions of %s: %w", document, err)
	}
	return versions, nil
}

// beginHolder opens a transaction to hold a step in, and the call that lets it go. The release
// is registered as a cleanup too, so a failed assertion never leaves a write queued on a lock
// nothing will drop.
func (r *docsWriterRig) beginHolder(t *testing.T, ctx context.Context) (pgx.Tx, func()) {
	t.Helper()
	holder, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a holder: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = holder.Rollback(context.Background())
	}
	t.Cleanup(release)
	return holder, release
}

// queuedBackend waits for the write to queue behind holder and answers its backend: the one
// backend waiting on a lock holder's transaction owns, read through holder's own connection. The
// rig's service runs nothing else while a step is held, so a second backend queued there is a
// failure of the test's own reading, named with what each runs. A write that answers first never
// waited at the step, and one that never queues is waiting somewhere the step is not.
func (r *docsWriterRig) queuedBackend(t *testing.T, ctx context.Context, holder pgx.Tx, done <-chan error, what string) uint32 {
	t.Helper()
	deadline := time.Now().Add(storeStepWait)
	for {
		select {
		case err := <-done:
			t.Fatalf("%s finished without waiting there: %v", what, err)
		default:
		}
		rows, err := holder.Query(ctx, `
			select distinct pid from pg_locks
			where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))`)
		if err != nil {
			t.Fatalf("%s: read who waits on the holder: %v", what, err)
		}
		waiting, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
		if err != nil {
			t.Fatalf("%s: read who waits on the holder: %v", what, err)
		}
		switch len(waiting) {
		case 0:
		case 1:
			return waiting[0]
		default:
			statements := make([]string, len(waiting))
			for index, pid := range waiting {
				statements[index] = fmt.Sprintf("%d: %s", pid, r.runningStatement(t, ctx, pid))
			}
			t.Fatalf("%s: %d backends wait on the holder, want the write's alone: %v", what, len(waiting), statements)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never queued there within %s", what, storeStepWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// docsStatementSpace is a run of whitespace in a statement, which docsNormalizedStatement folds.
var docsStatementSpace = regexp.MustCompile(`\s+`)

// docsNormalizedStatement is statement lower-cased with every run of whitespace one space, so a
// statement is compared by its words rather than by how its source lays them out.
func docsNormalizedStatement(statement string) string {
	return strings.TrimSpace(docsStatementSpace.ReplaceAllString(strings.ToLower(statement), " "))
}

// runningStatement is the statement backend runs, which for a backend queued on a lock is the
// statement that asked for it. It reads pg_stat_activity through the pool, a fresh view each time.
func (r *docsWriterRig) runningStatement(t *testing.T, ctx context.Context, backend uint32) string {
	t.Helper()
	var query string
	if err := r.database.Pool.QueryRow(ctx,
		`select query from pg_stat_activity where pid = $1`, backend).Scan(&query); err != nil {
		t.Fatalf("read the statement backend %d runs: %v", backend, err)
	}
	return docsNormalizedStatement(query)
}

// locksHeld is every table lock backend's transaction holds above a plain read's, in table and
// mode order. It reads pg_locks through the pool and leaves out indexes and sequences, since a
// step is named by the table it locks or writes, and a lock the backend still waits for, which is
// the step it is stopped at.
func (r *docsWriterRig) locksHeld(t *testing.T, ctx context.Context, backend uint32) []docsTableLock {
	t.Helper()
	rows, err := r.database.Pool.Query(ctx, `
		select distinct c.relname::text, l.mode from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and l.granted and l.locktype = 'relation' and c.relkind in ('r', 'p')
			and l.mode <> 'AccessShareLock'
		order by 1, 2`, backend)
	if err != nil {
		t.Fatalf("read the locks backend %d holds: %v", backend, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (docsTableLock, error) {
		var lock docsTableLock
		err := row.Scan(&lock.table, &lock.mode)
		return lock, err
	})
	if err != nil {
		t.Fatalf("read the locks backend %d holds: %v", backend, err)
	}
	return held
}

// rowsLocked is every row of tables that backend's transaction has locked, updated or deleted,
// each named by its table and its primary key, in table and key order. A row lock, an update and
// a delete each stamp the row's xmax with the transaction's id, which the row keeps while that
// transaction is open, and a read through the pool still sees the row as it stood before it, so
// such a row is the one whose xmax is the backend's transaction id (pg_stat_activity's
// backend_xid). A transaction is given that id by its first row lock or write, so one with none
// has locked no row. A row two open transactions lock at once carries a multixact rather than
// either id, which no run of these paths holds at its fence. A table with no primary key names a
// row by its ctid.
func (r *docsWriterRig) rowsLocked(t *testing.T, ctx context.Context, backend uint32, tables []string) []docsRowLock {
	t.Helper()
	var xid *string
	if err := r.database.Pool.QueryRow(ctx,
		`select backend_xid::text from pg_stat_activity where pid = $1`, backend).Scan(&xid); err != nil {
		t.Fatalf("read the transaction id of backend %d: %v", backend, err)
	}
	if xid == nil {
		return nil
	}
	var locked []docsRowLock
	for _, table := range tables {
		rows, err := r.database.Pool.Query(ctx, `
			select a.attname::text from pg_index i
			join pg_attribute a on a.attrelid = i.indrelid and a.attnum = any(i.indkey)
			where i.indrelid = $1::text::regclass and i.indisprimary
			order by array_position(i.indkey::int2[], a.attnum)`, table)
		if err != nil {
			t.Fatalf("read the primary key of %s: %v", table, err)
		}
		columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the primary key of %s: %v", table, err)
		}
		key := "locked.ctid::text"
		if len(columns) > 0 {
			for index, column := range columns {
				columns[index] = "locked." + pgx.Identifier{column}.Sanitize()
			}
			key = "concat_ws(', ', " + strings.Join(columns, ", ") + ")"
		}
		rows, err = r.database.Pool.Query(ctx, "select "+key+" from "+pgx.Identifier{table}.Sanitize()+
			" locked where locked.xmax::text = $1 order by 1", *xid)
		if err != nil {
			t.Fatalf("read the rows of %s backend %d has locked: %v", table, backend, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the rows of %s backend %d has locked: %v", table, backend, err)
		}
		for _, key := range keys {
			locked = append(locked, docsRowLock{table: table, key: key})
		}
	}
	return locked
}
