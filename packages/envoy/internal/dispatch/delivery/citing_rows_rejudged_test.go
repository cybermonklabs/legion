package delivery

import (
	"fmt"
	"testing"
)

// The write that first records a pull request gives rule 0 to each row that a citation of it
// credited (citingRowsRejudged), so the reconcile's merged arm judges that row again. The build
// before this one, writing during a roll, stores a citation under the name its repository was
// renamed away from, and the pull request is first recorded under the new name: that citation
// names the same pull request, so its row is judged again too. Rows whose citation names the new
// name, another repository or another number show that the comparison is the rename pairs' own,
// and no wider: the new name is judged again, and the other two keep their rule.
func TestTheFirstRecordOfAPullRequestRejudgesARowCitingItUnderTheOldName(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedGadgetsIssue(t, ctx, pool, 30)
	seedRenamedGadgets(t, ctx, pool)
	const cited = 77
	cites := []struct {
		number int
		url    string
		rule   int
		why    string
	}{
		{80, fmt.Sprintf("https://github.com/%s/issues/%d", gadgetsOldRepo, cited), 0,
			"its citation names the pull request under the name the repository was renamed away from"},
		{81, fmt.Sprintf("https://github.com/%s/issues/%d", gadgetsRepo, cited), 0,
			"its citation names the pull request under the repository's own name"},
		{82, fmt.Sprintf("https://github.com/acme/widgets/issues/%d", cited), AttributionRuleVersion,
			"its citation names that number in another repository"},
		{83, fmt.Sprintf("https://github.com/%s/issues/%d", gadgetsOldRepo, cited+1), AttributionRuleVersion,
			"its citation names another number"},
	}
	for _, row := range cites {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, url, issue_key,
				attribution_source, attribution_rule, attribution_cited_issues)
			values ('acme/widgets', $1, $2, 'merged', $3, $4, $5, $6, array[$7])
		`, row.number, fmt.Sprintf("%040d", row.number), fmt.Sprintf("https://github.com/acme/widgets/pull/%d", row.number),
			issue, string(sourceGitHubIssueLink), AttributionRuleVersion, row.url); err != nil {
			t.Fatalf("store #%d citing %s: %v", row.number, row.url, err)
		}
	}

	if err := NewAttributor(renamesOf(t, ctx, pool)).UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
		Repo: gadgetsRepo, Number: cited, HeadSHA: fmt.Sprintf("%040d", cited), HeadRef: "topic", BaseRef: "main",
		State: "open", URL: fmt.Sprintf("https://github.com/%s/pull/%d", gadgetsRepo, cited),
	}); err != nil {
		t.Fatalf("record %s#%d: %v", gadgetsRepo, cited, err)
	}

	for _, row := range cites {
		var rule int
		if err := pool.QueryRow(ctx, `
			select attribution_rule from pull_request_heads where repo = 'acme/widgets' and number = $1
		`, row.number).Scan(&rule); err != nil {
			t.Fatalf("read the rule of #%d: %v", row.number, err)
		}
		if rule != row.rule {
			t.Errorf("#%d holds rule %d after %s#%d is first recorded, want %d: %s",
				row.number, rule, gadgetsRepo, cited, row.rule, row.why)
		}
	}
}
