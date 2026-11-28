package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A repository's recovery tells each child its release_children left unrecorded to the architect
// that releases that child: its notice is queued on the child's parent whenever the record holds
// that parent, so the owner walk over the Dispatch chain takes it to the architect that owns that
// parent (the parent's own while it holds its role, else the nearest one above it that does; R18
// of dispatch://INFRA-3320), and two children under one parent share one notice. No stored tree is
// read, so a parent another root's architect owns is told through that root. A child whose parent
// is not recorded has no architect of its parent, so its notice is queued on the issue the release
// was asked on.
func TestARecoveryTellsEachChildItDidNotRecordOnItsRecordedParent(t *testing.T) {
	t.Parallel()
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

	type told struct{ issue, reason string }
	var got []told
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select issue, payload->>'reason' from outbox
			where kind = 'notice' and payload->>'kind' = 'child-release-stalled' order by issue`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var notice told
			if err := rows.Scan(&notice.issue, &notice.reason); err != nil {
				return err
			}
			got = append(got, notice)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the stalled notices: %v", err)
	}
	if len(got) != 3 || got[0].issue != root || got[1].issue != own || got[2].issue != foreign {
		t.Fatalf("stalled notices %+v; want one on %s, one on %s and one on %s", got, root, own, foreign)
	}
	for i, tc := range []struct {
		names, not []string
	}{
		{[]string{
			"LEGION-5 (its parent LEGION-9 is not recorded: release LEGION-9 first, then LEGION-5)",
			"LEGION-6 (its parent LEGION-1 is recorded now: release LEGION-6 again)",
		}, []string{"LEGION-3", "LEGION-4", "LEGION-7"}},
		{[]string{
			"LEGION-3 (its parent LEGION-2 is recorded now: release LEGION-3 again)",
			"LEGION-4 (its parent LEGION-2 is recorded now: release LEGION-4 again)",
		}, []string{"LEGION-5", "LEGION-6", "LEGION-7"}},
		{[]string{
			"LEGION-7 (its parent LEGION-50 is recorded now: release LEGION-7 again)",
		}, []string{"LEGION-3 (", "LEGION-4 (", "LEGION-5 (", "LEGION-6 ("}},
	} {
		notice := got[i]
		for _, want := range tc.names {
			if !strings.Contains(notice.reason, want) {
				t.Errorf("the notice on %s says %q; want it to name %q", notice.issue, notice.reason, want)
			}
		}
		for _, other := range tc.not {
			if strings.Contains(notice.reason, other) {
				t.Errorf("the notice on %s says %q; want it not to name %s", notice.issue, notice.reason, other)
			}
		}
	}
}
