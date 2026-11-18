package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestEveryObjectNodeWriterTakesTheStoreLockOrder holds the create sites that write an issue's, a
// document's, an ask's and a message's node, in its project, to the store's lock order, as
// TestEveryApiStoreWriterTakesTheStoreLockOrder holds the comment paths, with the same machinery:
// each path lists its steps in the order it takes them, the list is checked against the written
// order (requireTheWrittenOrderAdmits), and each step is held in turn while the path's own backend
// is read at the statement it waits at and the table locks it holds
// (requireEachStepIsWaitedAtInTurn).
//
// What each path is held to is the order this slice adds: the object's node is written before the
// row that holds the object, the event is appended after that row, and the facts the node write
// stated are appended after the event that causes them. The id of each object is drawn inside its
// own transaction, so no other transaction can hold its node or its row before they exist: those
// steps are held as the tables they are written to.
//
// Its control is the ask create's node write skipped (api/asks.go): held at the ask's node, the
// path then waits at the author follow's node write rather than its own, or at none.
func TestEveryObjectNodeWriterTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.objectCreate("createAskFor on an issue", "the ask", "asks", "insert into asks (",
			[]storeStep{rig.issueOwnerStep(issueOwnerStatement)},
			"/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Does the lock order hold?"}),
		rig.objectCreate("createStoredMessage on an issue", "the message", "messages", "insert into messages (",
			nil, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{"body": "A message the lock steps hold."}),
		rig.objectCreate("storeArtifact on an issue", "the document", "artifacts", "insert into artifacts (",
			nil, "/api/v1/issues/"+rig.issue+"/artifacts", nil),
		rig.issueCreate(),
	} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheWrittenOrderAdmits(t, writer)
			requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// objectNodeSteps are the steps every create here takes after its owner row: the object's node,
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
// objectNodeSteps. A nil body is a document upload, which each run names afresh, and whose row's
// foreign key into projects takes that project's row in key share mode.
func (r *storeWriterRig) objectCreate(name, object, table, insert string, owner []storeStep, route string, body map[string]any) storeWriter {
	return storeWriter{
		name:            name,
		steps:           append(append([]storeStep(nil), owner...), objectNodeSteps(object, table, insert)...),
		locksOwnProject: body == nil,
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			sent := body
			if sent == nil {
				sent = map[string]any{"name": r.fresh("lock-notes") + ".md", "content": "# Notes\n\nThe lock steps hold.\n"}
			}
			return storeWriterRun{write: func() error {
				return createdOrError(dispatchRequest(t, r.call, http.MethodPost, route, sent, "alice"))
			}}
		},
	}
}

// issueCreate is the issue create: one node write of the issue's node and its spec's, then the
// issue's row and the spec's row, the issue.created event and the facts it caused. It numbers the
// issue under its project's row, which is not the project of its event. Each run's title and spec
// share no word with another's, so the create's duplicate check lets it through.
func (r *storeWriterRig) issueCreate() storeWriter {
	steps := objectNodeSteps("the issue", "issues", "insert into issues (")
	spec := storeStep{
		name: "the spec's row", order: projectionStep, names: "the issue's node",
		hold: holdTableWrites("artifacts"), at: "insert into artifacts (",
		takes: []tableLock{{"artifacts", rowExclusiveMode}},
	}
	steps = append(steps[:2], append([]storeStep{spec}, steps[2:]...)...)
	return storeWriter{
		name: "createIssue", steps: steps, locksOwnProject: true,
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			title, spec := uuid.NewString(), uuid.NewString()+"\n"
			return storeWriterRun{write: func() error {
				return createdOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/issues",
					map[string]any{"project": "LOCK", "title": title, "spec": spec}, "alice"))
			}}
		},
	}
}
