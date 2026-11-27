package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// A version that moves an open approval request falling through to its project's default assignee
// takes that project's owner lock shared (moveApprovalAsk, inboxwrite.HoldAskDefault) in its read
// phase, after the document owner's lock and before its write phase locks any asks row. A
// settlement's version write locks the asks rows of the document's ask blocks (reconcileAskBlocks,
// lockAskBlocksForWrite) and then rewrites the request's row; a move of the default
// (putProjectDefaultAssignee) takes the project's lock exclusive and then locks, in ascending id,
// every open asks row that falls through to it. Where the request's id is below a block ask's, the
// move takes the request's row first, and with no hold the settlement would hold the block ask's
// row while it waited for the request's, and the move hold the request's while it waited for the
// block ask's: a deadlock. A settlement that opens or restores no ask takes no hold of its own
// (reconcileAskBlocks), so the request's move is where this one comes from.
//
// DOC-1, which nobody holds, is in project DOC, whose default is alice. Its spec holds an open ask
// block, and an approval request over the spec's latest version, handed to nobody, is stored under
// the lowest id there is, below the block ask's, so both fall through to DOC's default and a move
// of it locks the request first. Alice adds a paragraph, which leaves the spec owing a settlement
// that opens, restores and answers nothing and moves the request to the version it writes. The
// issue's owner lock, the project's owner lock, held exclusively as a move of the default holds it,
// and the rows of the block ask and of the request, each locked `for no key update` as that move
// locks them, are each held in a transaction of its own while the settlement runs on a document
// service whose store is one connection of its own (lockstep.Follow): it must queue behind the
// issue, then the project, and only then the block ask's row and the request's. A settlement's
// writes are queued, so its read phase holds no asks row while it waits for the project, and the
// two rows it queues behind after the project are the first asks rows its write phase locks. The
// spec then holds the version the settlement wrote and the request names it, so the run followed
// is the one that committed.
func TestAVersionThatMovesARequestHoldsTheDefaultItFallsThroughToBeforeAnyAsksRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const block = ":::ask{#q-open urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n\n"+block)
	opened := blockAskOn(t, issue.service, issue.spec, "q-open").ID
	latest := latestVersionNumber(t, issue.service, issue.spec)
	documents := []model.AskApprovalDocument{{ArtifactID: issue.spec, Name: "spec.md", Version: latest, RequestedVersion: latest}}
	approval, err := EncodeApproval(&model.AskApproval{Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	const request = "00000000-0000-0000-0000-000000000001"
	if _, err := issue.database.Pool.Exec(ctx, `
		insert into asks (id, issue_key, author, question, options, urgency, kind, approval)
		values ($1::uuid, 'DOC-1', jsonb_build_object('kind', 'session', 'id', $2::text), $3,
			'[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval', $4)
	`, request, agentOfTheReleaseBefore.ID, ApprovalQuestion(documents, "Ships it."), approval); err != nil {
		t.Fatalf("store the approval request of the spec: %v", err)
	}
	// The request's node, as the boot step's backfill states a row the release before wrote, so
	// the move's rewording is stated there.
	if _, err := graphcensus.BackfillAsks(ctx, issue.database); err != nil {
		t.Fatalf("state the approval request on its node: %v", err)
	}
	for _, ask := range []string{request, opened} {
		var project string
		if err := issue.database.Pool.QueryRow(ctx, `select `+inboxwrite.AskDefaultProject+` from asks a where a.id = $1::uuid`, ask).Scan(&project); err != nil || project != "DOC" {
			t.Fatalf("ask %s falls through to the default of project %q (%v), want DOC's", ask, project, err)
		}
	}
	var below bool
	if err := issue.database.Pool.QueryRow(ctx, `select $1::uuid < $2::uuid`, request, opened).Scan(&below); err != nil || !below {
		t.Fatalf("the request's id is below the block ask's %t (%v), want it below, so a move of the default locks it first", below, err)
	}
	if _, err := joinedApplyOps(issue.service, issue.spec, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "The base release writes the corpus."},
	}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("add a paragraph to the spec: %v", err)
	}
	// Every live update of the edit reaches the room's persistence and lands before the holds, so
	// no append of the room's own runs while one of them is held.
	awaitRoomAppends(t, issue.service, issue.spec, "reach persistence", issue.service.waitForPendingUpdates)
	awaitRoomAppends(t, issue.service, issue.spec, "become durable", issue.service.waitForDurableAppends)

	runner := storetest.SingleConnection(t, issue.database)
	// Its shutdown is registered after the store's close and so runs before it, cleanups running
	// last registered first, which leaves the service its connection while it shuts down.
	settler := newTestServiceOn(t, runner)
	settler.settle = time.Hour
	pid := backendOf(t, runner)

	owner := lockstep.HoldOwner(t, issue.database, &lockstep.Hold{
		Name: "issue DOC-1's owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: "DOC-1",
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"asks", "graph_nodes", "graph_tuples"},
	}, store.IssueOwner("DOC-1"))
	project := lockstep.HoldOwner(t, issue.database, &lockstep.Hold{
		Name: "project DOC's owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "project", ID: "DOC",
		Statement: lockstep.OwnersLockSharedStatement,
	}, store.ProjectOwner("DOC"))
	blockRow := lockstep.HoldRow(t, issue.database, &lockstep.Hold{
		Name: "the open block ask's row", Step: lockstep.StepProjection, Taken: true,
		Statement: docsAskRowsStatement,
	}, `select 1 from asks where id = $1::uuid for no key update`, opened)
	requestRow := lockstep.HoldRow(t, issue.database, &lockstep.Hold{
		Name: "the approval request's row", Step: lockstep.StepProjection, Taken: true,
		Statement: "update asks set question = $2, approval = $3, edited_at = $4",
	}, `select 1 from asks where id = $1::uuid for no key update`, request)

	done := make(chan error, 1)
	go func() { done <- settler.SettleIssueDocuments(ctx, "DOC-1") }()
	lockstep.Follow(t, issue.database, "the settlement", pid, done, []*lockstep.Hold{owner, project, blockRow, requestRow})

	if written := latestVersionNumber(t, issue.service, issue.spec); written != latest+1 {
		t.Fatalf("the spec's latest version is %d after the settlement, want %d, the one it wrote", written, latest+1)
	}
	moved, err := ScanAsk(issue.database.Pool.QueryRow(ctx, `select `+AskColumns+` from asks a where a.id = $1::uuid`, request))
	if err != nil {
		t.Fatalf("read the approval request: %v", err)
	}
	if moved.Approval == nil {
		t.Fatalf("the approval request reads no approval after the settlement: %+v", moved)
	}
	if named := moved.Approval.Document(issue.spec); named == nil || named.Version != latest+1 {
		t.Fatalf("the approval request names %+v of the spec after the settlement, want version %d, the one it wrote", named, latest+1)
	}
}
