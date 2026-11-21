package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A6's hold, read over the records and the copy the line is actually read over. An issue beneath
// an ancestor the daemon has accepted waits at admitted until that ancestor closes: starting it
// would let the ancestor merge a head that does not carry its work, which is the whole of the rule
// release_children and rerun_child refuse under (CHILD_UNDER_ACCEPTED_PARENT) and the child-status
// notice promises a person.
//
// The chain between the issue and that ancestor holds a child that is already done, which the
// project's live records leave out (record.Store.LiveIssues). The chain is the copy's
// (record.Chains), which holds the done child as Dispatch does, so the hold reads past it and
// names the merging issue above it (accepted_ancestor), as R21 of dispatch://INFRA-3320 reads
// INFRA-500 under INFRA-430.
func TestAnIssueBeneathAnAcceptedAncestorIsHeldThoughTheParentBetweenThemIsDone(t *testing.T) {
	pool := migratedPool(t)
	root, between, grandchild := "LEGION-10", "LEGION-11", "LEGION-12"
	issue := func(key string, parent *string, stands phase.Phase, status, rank string) record.Issue {
		return record.Issue{Key: key, Tree: root, Parent: parent, Project: testProject, Title: key, Phase: stands, Generation: 1,
			Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	// The root's own reviewed head is accepted and its merge awaited; the grandchild is the issue
	// a person moved to todo, recorded at admitted beneath the done child.
	putIssue(t, pool, issue(root, nil, phase.Merging, "retro", "A"))
	putIssue(t, pool, issue(between, record.ParentOf(root), phase.Done, "done", "B"))
	putIssue(t, pool, issue(grandchild, record.ParentOf(between), phase.Admitted, "todo", "C"))
	approved := 1
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: root, Step: record.DesignStepSpec,
			ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
			ApprovedMark: record.EventMark{Seq: 3, At: fixedNow.Add(-time.Hour)}}); err != nil {
			t.Fatalf("put the gate of %s: %v", root, err)
		}
	})

	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		_, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, records, testProject, issues, nil, facts, nil)
		if err != nil {
			t.Fatalf("judge the line: %v", err)
		}
		for _, entry := range line {
			if entry.Issue.Key == grandchild {
				t.Fatalf("%s stands in the waiting line beneath %s, which awaits its merge: a done issue between them is no end of the chain", grandchild, root)
			}
		}
		held := record.HeldList(issues, nil, line, dropped, facts)
		for _, row := range held {
			if row.Issue == grandchild {
				if row.Reason != record.HeldAcceptedAncestor || len(row.HeldBy) != 1 || row.HeldBy[0] != root {
					t.Fatalf("the held row of %s = %+v, want accepted_ancestor held by %s", grandchild, row, root)
				}
				return
			}
		}
		t.Fatalf("the held list = %+v, want a row for %s", held, grandchild)
	})
}
