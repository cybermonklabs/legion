package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestEveryAskTransitionAndKeepTakesTheStoreLockOrder holds the routes of this package that write
// an ask's answer state through asks' one writer (asks.PlanSettle, queued), and the Keep of an
// agent's decision, to the store's lock order, with the machinery of
// TestEveryApiStoreWriterTakesTheStoreLockOrder: each path lists its steps in the order it takes
// them, the list is checked against the order written on api/inbox_read.go's markInboxReadThreads
// (requireTheWrittenOrderAdmits), and each step is held in turn while the path's own backend is
// read at the statement it waits at, at the table locks it holds and at the rows it has locked
// (requireEachStepIsWaitedAtInTurn). Each path takes its owner's lock first, reads under it
// holding no transaction id, and writes in one write phase, the batch its ledger or its own
// store.RunWrite sends. The paths are:
//
//   - a person's answer of an open ask (closeAskTx, through the answer route): the issue's owner
//     lock, then in the write phase the answerer's node, which the settle writes before the row it
//     settles, the ask's row written, the ask.answered, and the answer's facts under it;
//   - the close of an issue that holds an open ask (recordWithdrawnAsksTx, through the issue's
//     PATCH): the issue's owner lock, then the ask's row written withdrawn, the ask.resolved, and
//     the withdrawal's facts under it, which come before the issue.closed;
//   - the reopen of that issue (prepareRestoration, which queues the restore of the withdrawn
//     ask's row, and recordRestoredAsksTx, which queues its ask.opened and the restoration's
//     facts under it): the issue's owner lock, the ask's row written open, the ask.opened, and
//     those facts;
//   - a person's Keep of an agent's decision (keepDecisionNode, through the Keep route): the
//     issue's owner lock, then in one batch the person's node, the decision_keeps row that
//     projects the Keep, the decision.kept, and the kept_by and kept_text_sum under it.
//
// The ask's row is the row its node projects (asks.node_kind), and none of these paths writes
// that node, so no node step orders the row; the answerer, whose node an answer writes, is the far
// end of a tuple, which no row names. No path locks the ask's row before it writes it: every
// writer of an ask takes its owner's lock first, under which the row is read plainly. A Keep's
// decision_keeps row names its keeper (kept_by), whose node the Keep writes before it. Each run
// answers, closes or keeps something of its own, made just before it through the rig's own
// server; the answer and the Keep are a person's no write has recorded, whose node is held as a
// row another transaction inserted and has not committed. Every other write of a step is the
// path's first of its table, held as that table.
func TestEveryAskTransitionAndKeepTakesTheStoreLockOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.askAnswer(), rig.issueCloseWithdrawing(), rig.issueReopenRestoring(), rig.decisionKeep(t, ctx),
	} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheWrittenOrderAdmits(t, writer)
			requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// settledRowStatement is the head of the write of an ask's row the settle queues (asks.Settled's
// Queue).
const settledRowStatement = "update asks set state = $2,"

// askRowWrittenStep is the step every path here takes of the ask it settles: the row written by
// the settle after the node it states, held as the table, which no row lock conflicts with.
func askRowWrittenStep() storeStep {
	return storeStep{
		name: "the ask's row written", order: projectionStep,
		hold: holdTableWrites("asks"), at: settledRowStatement,
		takes: []tableLock{{"asks", rowExclusiveMode}},
	}
}

// runOwnerLockStep is ownerLockStep for the issue the run draws, whose key is known once the run
// is readied: the lock is named when the step is held.
func runOwnerLockStep(issue *string) storeStep {
	return storeStep{
		name: "the owner lock", order: ownerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
			t.Helper()
			ownerLockStep(store.IssueOwner(*issue)).hold(t, ctx, holder, run)
		},
		at: store.OwnerLockStatement,
	}
}

// askAnswer is a person's answer of an open ask of the issue (closeAskTx), through its route. It
// takes the issue's owner lock (requireOpenOwner), reads the ask under it, and plans its settle
// (asks.PlanSettle); its write phase writes the answerer's node, held for the event, and then the
// ask's row, queues the answer's part, then appends ask.answered and the answer's facts under it
// (graphstore.QueueCaused). Each run answers an ask of its own, opened just before it.
func (r *storeWriterRig) askAnswer() storeWriter {
	return storeWriter{
		name: "closeAskTx answering",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the answerer's node", order: nodeStep,
				hold: holdNewNode, at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			askRowWrittenStep(),
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			ask := decodeBody[model.Ask](t, created(t, "open the ask the run answers", dispatchRequest(t, r.handler, http.MethodPost,
				"/api/v1/issues/"+r.issue+"/asks", map[string]any{"question": r.fresh("Which way do the lock steps answer, number") + "?"}, "alice"))).ID
			login := r.fresh("lock-answerer")
			return storeWriterRun{
				made: graphstore.PersonNode(login, model.Actor{Kind: "user", ID: login}, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/asks/"+ask+"/answer",
						map[string]any{"text": "This way."}, login), http.StatusOK)
				},
			}
		},
	}
}

// plainIssueWithAsk creates an issue of the rig's project with no spec, which gives it an empty
// primary document, and an open ask on it, and answers the issue. It settles that document at
// once: a close settles the documents of its issue before its transaction
// (settleIssueBeforeClose), and a settlement owed then would take the issue's owner lock on a
// backend of its own, behind the hold of the close's owner lock, while the close waits for it.
func (r *storeWriterRig) plainIssueWithAsk(t *testing.T) string {
	t.Helper()
	issue := decodeBody[model.Issue](t, created(t, "create the issue the run closes", dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": r.project, "title": r.fresh("The lock steps of a close, number"), "force": true}, "alice"))).Key
	created(t, "open the ask the close withdraws", dispatchRequest(t, r.handler, http.MethodPost,
		"/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Does the close withdraw this?"}, "alice"))
	if err := r.direct.deps.Docs.SettleIssueDocuments(context.Background(), issue); err != nil {
		t.Fatalf("settle the documents of %s: %v", issue, err)
	}
	return issue
}

// patchIssueSteps are the steps a close or a reopen takes: the issue's owner lock, its ask's row
// written, the ask's event and the facts its settle held for it. The settle of a withdrawal or a
// restore writes no node, so its facts are graphstore's plain tuple append. issue is the run's.
func patchIssueSteps(issue *string) []storeStep {
	return []storeStep{runOwnerLockStep(issue), askRowWrittenStep(), eventAppendStep(), plainTupleAppendStep()}
}

// plainTupleAppendStep is tupleAppendStep for a write that wrote no node, whose tuples graphstore
// appends with a plain insert (queueAppend).
func plainTupleAppendStep() storeStep {
	step := tupleAppendStep()
	step.at = plainTupleAppendStatement
	return step
}

// issueCloseWithdrawing is the close of an issue that holds an open ask (recordWithdrawnAsksTx),
// through the issue's PATCH. It takes the issue's owner lock (lockPatchOwnersAndParent), reads
// the issue's open asks under it (readOpenIssueAsks) and plans each one's withdrawal
// (planWithdrawal, docs.PlanAskResolution, through asks.PlanSettle), writes the issue's row, and
// queues each withdrawal, which states no node for a resolution and writes the ask's row, then
// ask.resolved and the withdrawal's facts under it, before the issue.closed. The ask.resolved's
// append writes the issue's row a second time (projectKeyShare). Each run closes an issue of its
// own.
func (r *storeWriterRig) issueCloseWithdrawing() storeWriter {
	var issue string
	return storeWriter{
		name: "recordWithdrawnAsksTx closing", projectKeyShare: true,
		steps: patchIssueSteps(&issue),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			issue = r.plainIssueWithAsk(t)
			closing := issue
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodPatch, "/api/v1/issues/"+closing,
					map[string]any{"status": "done"}, "alice"), http.StatusOK)
			}}
		},
	}
}

// issueReopenRestoring is the reopen of an issue whose close withdrew its ask (prepareRestoration
// and recordRestoredAsksTx), through the issue's PATCH. It takes the issue's owner lock, reads
// the withdrawn asks under it, and queues each one's restore (docs.WriteRestoredAsks, through
// asks.PlanSettle), which writes the ask's row, then its parts, ask.opened and the restoration's
// facts under it; once it has written the issue's row, its write phase runs them in that order,
// before the issue.updated. The ask.opened's append writes the issue's row a second time
// (projectKeyShare). Each run reopens an issue of its own, closed just before it.
func (r *storeWriterRig) issueReopenRestoring() storeWriter {
	var issue string
	return storeWriter{
		name: "recordRestoredAsksTx reopening", projectKeyShare: true,
		steps: patchIssueSteps(&issue),
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			issue = r.plainIssueWithAsk(t)
			if closed := dispatchRequest(t, r.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
				t.Fatalf("close %s before its reopen: status=%d body=%s", issue, closed.Code, closed.Body.String())
			}
			reopening := issue
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodPatch, "/api/v1/issues/"+reopening,
					map[string]any{"status": "todo"}, "alice"), http.StatusOK)
			}}
		},
	}
}

// decisionKeep is a person's Keep of an agent's decision (keepDecisionNode), through the Keep
// route, on a document of the issue the agent uploaded. It takes the issue's owner lock
// (LockOwners), reads the node's keepers under it, and then in one batch (store.RunWrite) writes
// the person's node (graphstore.QueueNodes), then the decision_keeps row that projects the Keep,
// appends decision.kept, and appends the kept_by and the kept_text_sum under it
// (graphstore.QueueCaused). Each run keeps the decision as a person no write has recorded.
func (r *storeWriterRig) decisionKeep(t *testing.T, ctx context.Context) storeWriter {
	const name = "lock-steps-choices.md"
	requireAgentWrite(t, "upload the agent's decision", sessionRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
		"name": name, "content": "::::decision{#d1}\nThe lock steps hold REST first.\n::::\n", "actor": sessionActor(),
	}), http.StatusCreated)
	choices := readDecisionDocument(t, r.database, r.issue, artifactSlug(name))
	document, err := r.direct.loadArtifact(ctx, r.database.Pool, choices.id)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	decision, found, err := r.direct.readDecisionNode(ctx, r.database.Pool, model.Actor{Kind: "user", ID: "alice"}, document, "d1")
	if err != nil || !found {
		t.Fatalf("read the decision d1 of %s: found %t, %v", name, found, err)
	}
	return storeWriter{
		name: "keepDecisionNode",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the keeper's node", order: nodeStep,
				hold: holdNewNode, at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the Keep's row", order: projectionStep, names: "the keeper's node",
				hold: holdTableWrites("decision_keeps"), at: "insert into decision_keeps (",
				takes: []tableLock{{"decision_keeps", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			login := r.fresh("lock-keeper")
			return storeWriterRun{
				made: graphstore.PersonNode(login, model.Actor{Kind: "user", ID: login}, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/artifacts/"+choices.id+"/blocks/d1/keep",
						map[string]any{"text_sha256": decision.TextSHA256}, login), http.StatusOK)
				},
			}
		},
	}
}
