package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// closeHoldSpec and closeHoldRecord are the two documents of the issue whose close
// TestTheCloseHoldsTheDefaultItsAsksFallThroughToBeforeAnyAsksRow follows, each holding an ask
// block of its own.
const (
	closeHoldSpec   = "Context\n\n:::ask{#spec-decision urgency=\"med\" multiple=\"false\"}\nShip the spec's way?\n\n- Yes: Now.\n- No: Later.\n:::\n"
	closeHoldRecord = "Record\n\n:::ask{#record-decision urgency=\"med\" multiple=\"false\"}\nKeep the record?\n\n- Yes: Now.\n- No: Later.\n:::\n"
)

// The close of an issue nobody holds takes the owner lock of the project whose default assignee
// the issue's asks fall through to, shared (inboxwrite.HoldDefault), after the issue's owner lock
// and before any asks row (prepareWithdrawal). Its ask-state write locks the asks rows of each
// document's ask blocks `for no key update`, in ascending id within the document and one document
// after another (docs' lockAskBlocksForWrite, WriteIssueAskStates), so it takes its issue's asks
// rows in no one ascending order. A move of that default (putProjectDefaultAssignee) takes the lock
// exclusive and then locks, in ascending id, every open asks row that falls through to the
// default. Taken by neither, a close could hold the first document's rows while it waited for one
// of the second's that the move held, and the move wait for one of the first's: a deadlock.
//
// HOLD-1, which nobody holds, is in project HOLD, whose default is alice, its creator; its spec
// and a second document each hold an ask block, so each has an open ask that falls through to the
// default. The issue's owner lock, the project's owner lock, held exclusively as a move of the
// default holds it, and the row of the ask of the document the close writes second (in id order,
// as WriteIssueAskStates writes them), locked `for no key update` as that move locks it, are each
// held in a transaction of its own while the close runs on a server whose store is one connection
// of its own (lockstep.Follow): it must queue behind the issue, then the project, holding no lock
// on asks, and only then the second document's ask row. The issue then reads closed and both asks
// withdrawn, so the run followed is the one that committed. The documents are settled before the
// holds, so the close's own settlement (settleIssueBeforeClose) has nothing to settle and takes no
// lock on a backend of its own, and the settle delay is an hour, so no settlement runs while the
// test does.
func TestTheCloseHoldsTheDefaultItsAsksFallThroughToBeforeAnyAsksRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "HOLD", "A close over two documents", closeHoldSpec)
	record := createIssueDocument(t, handler, issue.Key, "record.md", closeHoldRecord).ID
	assignIssue(t, handler, issue.Key, "")
	if err := deps.Docs.SettleIssueDocuments(ctx, issue.Key); err != nil {
		t.Fatalf("settle the documents of %s: %v", issue.Key, err)
	}
	awaitSettled(t, database, issue.PrimaryArtifactID, record)
	withdrawing := []string{
		requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "spec-decision"),
		requireBlockAsk(t, handler, issue.Key, record, "record-decision"),
	}
	// Each ask waits on the project's default, by the one rule a move of it reads.
	for _, ask := range withdrawing {
		var project string
		if err := database.Pool.QueryRow(ctx, `select `+inboxwrite.AskDefaultProject+` from asks a where a.id = $1::uuid`, ask).Scan(&project); err != nil || project != "HOLD" {
			t.Fatalf("ask %s falls through to the default of project %q (%v), want HOLD's", ask, project, err)
		}
	}
	var second string
	if err := database.Pool.QueryRow(ctx, `
		select a.id::text from asks a where a.id = any($1::uuid[]) order by a.block_artifact_id desc limit 1
	`, withdrawing).Scan(&second); err != nil {
		t.Fatalf("read the ask of the document the close writes second: %v", err)
	}
	closer, pid := oneConnectionHandler(t, deps)

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue " + issue.Key + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: issue.Key,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"asks", "graph_nodes", "graph_tuples"},
	}, store.IssueOwner(issue.Key))
	project := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "project HOLD's owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "project", ID: "HOLD",
		Statement: lockstep.OwnersLockSharedStatement,
		Unlocked:  []string{"asks"},
	}, store.ProjectOwner("HOLD"))
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the second document's ask row", Step: lockstep.StepProjection, Taken: true,
		Statement: "select 1 from asks a where a.block_artifact_id = $1 and a.block_id is not null",
	}, `select 1 from asks where id = $1::uuid for no key update`, second)

	done := make(chan error, 1)
	go func() {
		done <- statusOrError(dispatchRequest(t, closer, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]string{"status": "done"}, "alice"), http.StatusOK)
	}()
	lockstep.Follow(t, database, "the close", pid, done, []*lockstep.Hold{owner, project, row})

	var status string
	var closed bool
	if err := database.Pool.QueryRow(ctx, `
		select status, closed_at is not null from issues where key = $1
	`, issue.Key).Scan(&status, &closed); err != nil || status != "done" || !closed {
		t.Fatalf("%s reads status %q, closed %t (%v) after the close, want it done and closed", issue.Key, status, closed, err)
	}
	closedBy := latestEventActor(t, database, issue.Key, "issue.closed")
	for _, ask := range withdrawing {
		requireWithdrawn(t, handler, issue.Key, ask, closedBy)
	}
}
