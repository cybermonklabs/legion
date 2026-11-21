package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// message (insertSessionReply) and the removal of an untouched document (removeArtifact). The
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
// no lock of its node; and the removal's gone statement moved after the row's delete
// (api/artifact_removal.go), which fails the same two ways.
func TestEveryObjectNodeWriterTakesTheStoreLockOrder(t *testing.T) {
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
// (graphstore.PlanUncaused and QueueNodes, an update of the node its create wrote), deletes the
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
