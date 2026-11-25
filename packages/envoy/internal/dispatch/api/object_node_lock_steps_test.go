package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestEveryObjectNodeWriterTakesTheStoreLockOrder holds the create sites that write an ask's
// and a message's node, in its project or between its parties, and the removal that states a
// document's node gone, to the store's lock order, as TestEveryApiStoreWriterTakesTheStoreLockOrder
// holds the comment paths, with the same machinery: each path lists its steps in the order it
// takes them, the list is checked against the written order (requireTheWrittenOrderAdmits), and
// each step is held in turn while the path's own backend is read at the statement it waits at and
// the table locks it holds (requireEachStepIsWaitedAtInTurn). The paths are a question and a
// message on an issue, an approval request (openApprovalAsk), the ask a settled document's new
// ask block opens (createAskBlock), a broadcast's copy (writeBroadcast), a session's reply to a
// message (insertSessionReply) and the removal of an untouched document (removeArtifact), once of
// a document no lens stands on and once of one a lens with a finding stands on, whose lens cascade
// that removal is held at between the row's delete and the event (lensedDocumentRemoval). The
// issue create and a document's upload, which write their object's node before the block index
// runs, are paths of TestEveryApiStoreWriterTakesTheStoreLockOrder, which holds that node step
// with the block index's.
//
// What each create is held to is the order this slice adds: its owner lock first, then the
// object's node, written before the row that holds the object, the event, appended after that
// row, and the facts the node write stated, appended after the event that causes them. The id of
// each object is drawn inside its own transaction, so no other transaction can hold its node or
// its row before they exist: those steps are held as the tables they are written to.
//
// Its controls are the ask create's node write skipped (api/asks.go): held at the ask's node, the
// path then waits at the author follow's node write rather than its own, or at none; the session
// reply's node write moved after its row (api/message_delivery.go insertSessionReply): held at the
// reply's node, the path then already holds the lock its row takes, and held at its row, it holds
// no lock of its node; the removal's gone statement moved after the row's delete
// (api/artifact_removal.go), which fails the same two ways; the removal's lens cascade moved
// before the row's delete, where it reads no stale lens and writes nothing, so held at the
// finding's node row the path never waits there; and the cascade run in a transaction of its own
// after the removal commits, which held at either node row of the cascade holds no lock of the
// document's row, held at the cascade's state tuples waits at the gone statement's append
// instead, and held at the event holds no lock of graph_tuples.
func TestEveryObjectNodeWriterTakesTheStoreLockOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.objectCreate("createAskFor on an issue", "the ask", "asks", "insert into asks (",
			[]storeStep{ownerLockStep(store.IssueOwner(rig.issue))},
			"/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Does the lock order hold?"}),
		rig.objectCreate("createStoredMessage on an issue", "the message", "messages", "insert into messages (",
			[]storeStep{ownerLockStep(store.IssueOwner(rig.issue))},
			"/api/v1/issues/"+rig.issue+"/messages", map[string]any{"body": "A message the lock steps hold."}),
		rig.approvalRequest(),
		rig.askBlockSettle(t),
		rig.broadcastCopy(),
		rig.sessionReply(),
		rig.documentRemoval(),
		rig.lensedDocumentRemoval(),
	} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheWrittenOrderAdmits(t, writer)
			requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// objectNodeSteps are the steps every create here takes after its owner lock: the object's node,
// the row that holds it, the event, and the facts the node write stated, appended under it.
func objectNodeSteps(object, table, insert string) []storeStep {
	node := object + "'s node"
	return []storeStep{
		{
			name: node, order: nodeStep,
			hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
			takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
		},
		{
			name: object + "'s row", order: projectionStep, names: node,
			hold: holdTableWrites(table), at: insert,
			takes: []tableLock{{table, rowExclusiveMode}},
		},
		eventAppendStep(),
		tupleAppendStep(),
	}
}

// objectCreate is a create through route, which takes the owner steps it lists and then
// objectNodeSteps.
func (r *storeWriterRig) objectCreate(name, object, table, insert string, owner []storeStep, route string, body map[string]any) storeWriter {
	return storeWriter{
		name:  name,
		steps: append(append([]storeStep(nil), owner...), objectNodeSteps(object, table, insert)...),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodPost, route, body, "alice"), http.StatusCreated)
			}}
		},
	}
}

// approvalRequest is a session's request for approval of a document of the issue
// (openApprovalAsk), through its route: it takes the issue's owner lock, as every owner check
// does, then writes the request's node, its row, the ask.opened and the facts the node write
// stated. Each run asks about a document of its own, just uploaded, since a request on a document
// with one open renews that one rather than opening another.
func (r *storeWriterRig) approvalRequest() storeWriter {
	return storeWriter{
		name: "openApprovalAsk",
		steps: append([]storeStep{ownerLockStep(store.IssueOwner(r.issue))},
			objectNodeSteps("the approval request", "asks", "insert into asks (")...),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			document := uploadDocument(t, r.handler, "/api/v1/issues/"+r.issue, r.fresh("approval")+".md",
				"# Approval\n\nWhat the request asks about.\n").Artifact.ID
			return storeWriterRun{write: func() error {
				return statusOrError(bearerRequest(t, r.call, http.MethodPost, "/api/v1/artifacts/"+document+"/approval-requests",
					map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}}), http.StatusCreated)
			}}
		},
	}
}

// askBlockSettle is the settlement of a document of the issue that holds an ask block no
// settlement has opened: it takes the issue's owner lock (lockArtifactOwner), writes the ask's
// node and then its row (createAskBlock), appends the ask.opened with the settlement's other
// events, and then the facts the node write stated, under that event (appendEvents). Each run
// uploads a document holding a block of its own and settles it in settler, a document service of
// its own on the rig's one connection, so the settlement's transaction runs on the backend the
// test watches; the rig's own service settles an hour on, never inside the test. A settlement
// runs on the service's own goroutine, so a run's write ends once the block's ask is there, which
// the settlement's commit puts there.
func (r *storeWriterRig) askBlockSettle(t *testing.T) storeWriter {
	settler := docs.New(docs.Deps{Store: r.callStore, Events: r.direct.deps.Events, ServerURL: testServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := settler.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the settling document service: %v", err)
		}
	})
	return storeWriter{
		name: "createAskBlock",
		steps: append([]storeStep{ownerLockStep(store.IssueOwner(r.issue))},
			objectNodeSteps("the ask", "asks", "insert into asks (")...),
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			block := r.fresh("lock-block")
			document := uploadDocument(t, r.handler, "/api/v1/issues/"+r.issue, block+".md",
				"# Decision\n\n"+askBlockMarkdown(block, "Does the settled ask take the lock order?")).Artifact.ID
			return storeWriterRun{write: func() error {
				settler.SettleNow(document)
				return r.askBlockOpened(ctx, document, block)
			}}
		},
	}
}

// askBlockOpened waits for the ask of block on document, which the commit of the settlement that
// opens it puts there, and answers an error once the settlement has had longer than any run waits
// for it.
func (r *storeWriterRig) askBlockOpened(ctx context.Context, document, block string) error {
	within := 3 * storeStepWait
	for deadline := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
		var open bool
		if err := r.database.Pool.QueryRow(ctx, `
			select exists (select 1 from asks where block_artifact_id = $1::uuid and block_id = $2)
		`, document, block).Scan(&open); err != nil {
			return fmt.Errorf("read the ask of block %s: %w", block, err)
		}
		if open {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the settlement of %s opened no ask for block %s within %s", document, block, within)
		}
	}
}

// broadcastCopy is a broadcast's copy to one session (writeBroadcast), called as the broadcast
// route calls it once its recipients are resolved: it writes the broadcast's row, which owns its
// copies, then the copy's node, the copy's row, its message.created and the facts the node write
// stated. It is called on the rig's direct server, so no delivery runs on the rig's one connection
// behind it; the attempt it opens stays pending, and nothing here sweeps it.
func (r *storeWriterRig) broadcastCopy() storeWriter {
	return storeWriter{
		name: "writeBroadcast",
		steps: append([]storeStep{{
			name: "the broadcast's row", order: ownerStep,
			hold: holdTableWrites("broadcasts"), at: "insert into broadcasts (",
			takes: []tableLock{{"broadcasts", rowExclusiveMode}},
		}}, objectNodeSteps("the copy", "messages", "insert into messages (")...),
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			actor := model.Actor{Kind: "user", ID: "alice"}
			return storeWriterRun{write: func() error {
				_, _, err := r.direct.writeBroadcast(ctx, actor, "A broadcast the lock steps hold.", "btw",
					[]broadcastTarget{{sessionID: "s1", title: "planner"}}, []broadcastExclusion{})
				return err
			}}
		},
	}
}

// attemptRowStatement is the reply route's lock of the delivery attempt a session answers.
const attemptRowStatement = "select " + messageDeliveryColumns + " from message_deliveries where message_id = $1 and attempt = $2 for update"

// sessionReply is a session's reply to a message on the issue (insertSessionReply), through the
// reply route: it takes the issue's owner lock (lockIssue) and then the attempt it answers `for
// update`, then writes the reply's node, the reply's row, the message.answered and the facts the
// node write stated. The attempt's row belongs to a projected table and names no node the reply
// writes, so the written order lets it come before the reply's node, as a mention reply's
// answered comment does (mentionReply). Each run answers a message of its own, sent to s1 and
// delivered, since an attempt already answered takes no second reply.
func (r *storeWriterRig) sessionReply() storeWriter {
	// parent is the message the run under way answers, which its ready sends and its attempt's
	// hold locks.
	var parent string
	return storeWriter{
		name: "insertSessionReply",
		steps: append([]storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the answered attempt's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from message_deliveries where message_id = $1::uuid and attempt = 1 for update`, parent)
				},
				at:    attemptRowStatement,
				takes: []tableLock{{"message_deliveries", rowShareMode}},
			},
		}, objectNodeSteps("the reply", "messages", "insert into messages (")...),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			parent = createIssueMessage(t, r.handler, r.issue, map[string]any{
				"body": "Does the reply take the lock order?", "target": "session:s1", "delivery": "steer",
			}, "alice").ID
			answered := parent
			return storeWriterRun{write: func() error {
				return statusOrError(bearerRequest(t, r.call, http.MethodPost, "/api/v1/messages/"+answered+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "It does.",
				}), http.StatusCreated)
			}}
		},
	}
}

// documentRemoval is the removal of an untouched document of the issue (removeArtifact), through
// its route: it takes the issue's owner lock (beginArtifactWrite) and the document's room, clears
// what the document's own delete would leave behind, states the document's node gone
// (graphstore's PlanUncaused and QueueNodes, an update of the node its create wrote), deletes the
// document's row, which names that node, and appends artifact.removed and the gone statement
// under it in one batch (appendCausingEvent).
// Each run removes a document of its own, made just before it by typing a title (emptyDocument).
// The gone statement is the removal's first write of graph_nodes and the delete its first write of
// artifacts, so each is held as its table, and so are the event and the tuples it causes.
func (r *storeWriterRig) documentRemoval() storeWriter {
	return storeWriter{
		name: "removeArtifact",
		steps: append([]storeStep{ownerLockStep(store.IssueOwner(r.issue))},
			objectNodeSteps("the document", "artifacts", "delete from artifacts where id = $1")...),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			document := emptyDocument(t, r.handler, r.issue, r.fresh("Lock steps removal")+".md")
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodDelete, "/api/v1/artifacts/"+document.ID, nil, "alice"),
					http.StatusNoContent)
			}}
		},
	}
}

// keptCauseMoveStatement is graphstore's node move of a write that keeps its nodes' cause
// (keptCauseNodeMove), as a write of a node's state makes it (graphstore.WriteState): it moves
// each node's row and appends, in the same statement, the state tuple of each node it moved.
const keptCauseMoveStatement = "with incoming as ("

// lensedDocumentRemoval is the removal of an untouched document of the issue that a lens stands on
// (removeArtifact), through its route: the steps documentRemoval holds it to up to the row's
// delete - the issue's owner lock, the document's node stated gone, the document's row - then,
// in the same transaction and under that owner lock, the lens cascade: the node rows of the lens,
// stale once the row it recorded is deleted, and of its finding, in ascending (kind, id), so the
// finding's, a block node, first, then the state tuple of each, at the sentinel, all in one
// graphstore.Write; and last artifact.removed and the gone statement under it, in one batch
// (appendCausingEvent). Each run removes a document of its own, made just before it by typing a
// title (emptyDocument), on which alice then invokes a trade-off analysis holding one finding.
//
// The lens's and the finding's rows stand before the run, so each is held `for update`, which the
// cascade's node write waits on. Their state tuples are at the sentinel, which no repeat key
// covers, so they are held as the writes of graph_tuples, which the cascade's node move waits on
// as it appends them: the path's first write of that table. The gone statement's tuples come after
// the event, and a hold of that table would stop the path at the cascade first, so the gone tuple
// is held by its repeat key instead (holdGoneTupleUnderTheNextEvents).
func (r *storeWriterRig) lensedDocumentRemoval() storeWriter {
	// refKey, lens and finding are the document the run under way removes, the lens on it and the
	// lens's finding, which the run's ready makes and the holds of its steps name.
	var refKey, lens, finding string
	removal := objectNodeSteps("the document", "artifacts", "delete from artifacts where id = $1")
	node, row, event, gone := removal[0], removal[1], removal[2], removal[3]
	gone.hold = holdGoneTupleUnderTheNextEvents(&refKey)
	return storeWriter{
		name: "removeArtifact of a document a lens stands on",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)), node, row,
			{
				name: "the finding's node row", order: nodeStep,
				hold: holdNodeRow(refs.BlockNodeKind, &finding), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the lens's node row", order: nodeStep,
				hold: holdNodeRow(graphstore.KindLens, &lens), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the lens's and the finding's state tuples", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: keptCauseMoveStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			event, gone,
		},
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			document := emptyDocument(t, r.handler, r.issue, r.fresh("Lock steps lensed removal")+".md")
			invoked := decodeBody[lensInvoked](t, created(t, "invoke a lens with a finding on "+document.RefKey,
				dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
					"on": "dispatch://" + r.issue + "/artifact/" + document.Slug, "kind": "trade-off",
					"findings": []string{"A finding the removal takes gone with its lens."},
				}, "alice")))
			if len(invoked.Findings) != 1 {
				t.Fatalf("the lens on %s holds %d findings, want the one it was invoked with", document.RefKey, len(invoked.Findings))
			}
			refKey, lens, finding = document.RefKey, invoked.ID, invoked.Findings[0].ID
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodDelete, "/api/v1/artifacts/"+document.ID, nil, "alice"),
					http.StatusNoContent)
			}}
		},
	}
}

// holdNodeRow holds `for update` the row of the node of kind whose id is *id, a node the run under
// way readied, which the store's node write of that node waits on.
func holdNodeRow(kind string, id *string) func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
	return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
		t.Helper()
		holdRow(t, ctx, holder, `select 1 from graph_nodes where kind = '`+kind+`' and id = $1 for update`, *id)
	}
}

// heldEventIDs is how many ids of the events sequence holdGoneTupleUnderTheNextEvents holds the
// gone tuple under.
const heldEventIDs = 64

// holdGoneTupleUnderTheNextEvents holds the repeat key of the gone state tuple of the document node
// at *refKey that the run under way appends under its event, whose id the run draws from the
// events sequence in its own transaction. The hold draws the next id of that sequence itself, then
// writes that tuple through the store's own writer under each of the heldEventIDs ids after it and
// leaves them uncommitted, so the run's append of its tuple waits to learn whether the hold's
// commits. A run whose event drew an id past them, behind as many events appended meanwhile, does
// not wait there, and fails the step.
func holdGoneTupleUnderTheNextEvents(refKey *string) func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
	return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
		t.Helper()
		var drawn int64
		if err := holder.QueryRow(ctx, `select nextval(pg_get_serial_sequence('events', 'id'))`).Scan(&drawn); err != nil {
			t.Fatalf("draw the next id of the events sequence: %v", err)
		}
		at := time.Now().UTC()
		tuples := make([]graphstore.Tuple, heldEventIDs)
		for index := range tuples {
			tuples[index] = graphstore.Tuple{
				FromKind: graphstore.KindArtifact, FromID: *refKey, Kind: graphstore.AttributeState,
				ToKind: graphstore.KindValue, ToID: graphstore.StateGone,
				At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: drawn + 1 + int64(index), Asserted: true,
			}
		}
		if err := graphstore.Write(ctx, holder, nil, tuples); err != nil {
			t.Fatalf("hold the gone tuple of %s under events %d to %d: %v", *refKey, drawn+1, drawn+heldEventIDs, err)
		}
	}
}
