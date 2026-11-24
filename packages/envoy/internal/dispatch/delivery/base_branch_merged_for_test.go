package delivery

import (
	"testing"
	"time"
)

// BaseBranchMergedFor answers the branch the issue's newest merged pull request of one repository
// merged into, both repositories compared under their canonical names by the Attributor's rename
// pairs. A merged row the build before this one wrote during a roll, under the name its repository
// was renamed away from, is a merge of the repository the issue resolves to now, so it answers for
// that repository by either name. A newer merge of the issue in another repository answers for
// neither: it merged into a branch of that repository.
func TestBaseBranchMergedForReadsAMergeStoredUnderTheRepositorysOldName(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedGadgetsIssue(t, ctx, pool, 31)
	seedRenamedGadgets(t, ctx, pool)
	older := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		repo, base string
		number     int
		at         time.Time
	}{
		{gadgetsOldRepo, "integration/gadgets", 90, older},
		{"acme/widgets", "integration/widgets", 91, older.Add(time.Hour)},
	} {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, url, issue_key, base_ref, event_at)
			values ($1, $2, $3, 'merged', $4, $5, $6, $7)
		`, row.repo, row.number, "9"+row.base[len(row.base)-1:]+"00000000000000000000000000000000000000",
			"https://github.com/"+row.repo+"/pull/", issue, row.base, row.at); err != nil {
			t.Fatalf("store the merge of %s#%d: %v", row.repo, row.number, err)
		}
	}
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	for _, asked := range []string{gadgetsRepo, gadgetsOldRepo} {
		branch, err := attributor.BaseBranchMergedFor(ctx, pool, issue, asked)
		if err != nil {
			t.Fatalf("BaseBranchMergedFor(%s, %s): %v", issue, asked, err)
		}
		if branch != "integration/gadgets" {
			t.Errorf("BaseBranchMergedFor(%s, %s) = %q, want %q: the merge stored under %s is a merge of %s, and the newer one is acme/widgets'",
				issue, asked, branch, "integration/gadgets", gadgetsOldRepo, gadgetsRepo)
		}
	}
}
