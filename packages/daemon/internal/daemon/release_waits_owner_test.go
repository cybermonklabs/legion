package daemon

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A repository's recovery tells each child its release_children left unrecorded to the architect
// that releases that child: its notice is queued on the child's parent when the record holds that
// parent in the child's tree, so the owner walk takes it to the architect that owns that parent
// (the parent's own while it holds its role, else the nearest one above it that does), and two
// children under one parent share one notice. A child under the root, one whose parent is not
// recorded, and one whose parent is recorded in another tree have no architect of the tree between
// them and the root, so their notice is queued on the tree root, as every such notice was.
func TestARecoveryTellsEachChildItDidNotRecordOnItsRecordedParent(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, own, foreign = "LEGION-1", "LEGION-2", "LEGION-50"
	for _, member := range []struct{ key, tree, parent string }{{root, root, ""}, {own, root, root}, {foreign, foreign, ""}} {
		issue := record.Issue{Key: member.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: member.tree,
			Title: member.key, Phase: phase.Planning, Generation: 1, Status: "in_progress"}
		if member.parent != "" {
			issue.Parent = new(member.parent)
		}
		putOutboxIssue(t, pool, records, issue)
	}
	w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
	waits := map[string]releaseWait{
		"LEGION-3": {tree: root, parent: own},
		"LEGION-4": {tree: root, parent: own},
		"LEGION-5": {tree: root, parent: "LEGION-9"},
		"LEGION-6": {tree: root, parent: root},
		"LEGION-7": {tree: root, parent: foreign},
	}
	if err := w.tellReleaseWaits(ctx, []string{"zeta/tools"}, waits); err != nil {
		t.Fatalf("tell the release waits: %v", err)
	}

	type told struct {
		issue   string
		payload string
	}
	var got []told
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select issue, payload::text from outbox
			where kind = 'notice' and payload->>'kind' = 'child-release-stalled' order by issue`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var notice told
			if err := rows.Scan(&notice.issue, &notice.payload); err != nil {
				return err
			}
			got = append(got, notice)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the stalled notices: %v", err)
	}
	if len(got) != 2 || got[0].issue != root || got[1].issue != own {
		t.Fatalf("stalled notices %+v; want one on %s and one on %s", got, root, own)
	}
	for i, want := range [][]texts.UnrecordedChild{
		{{Child: "LEGION-5", Parent: "LEGION-9"}, {Child: "LEGION-6", Parent: "LEGION-1", ParentRecorded: true}, {Child: "LEGION-7", Parent: "LEGION-50", ParentRecorded: true}},
		{{Child: "LEGION-3", Parent: "LEGION-2", ParentRecorded: true}, {Child: "LEGION-4", Parent: "LEGION-2", ParentRecorded: true}},
	} {
		if !recoveredUnrecorded(t, got[i].payload, want) {
			t.Errorf("the notice on %s is %s; want it to name exactly %+v", got[i].issue, got[i].payload, want)
		}
	}
}
