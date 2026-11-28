package delivery

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// A change of an issue's links judges a row from its stored inputs only where this build's rule
// judged the row last (LinksChanged). A writer of the release before this one rewrites a row during
// a roll, its issue, its title and its rule, and leaves the inputs an earlier write of this release
// stored, so those inputs are not the facts its issue was judged from: the change judges such a
// row as a row with no inputs and gives it rule 0, so the reconcile's merged arm reads it again.
//
// #41 and #42 are acme/infra rows that ACME-1 holds by a link of their own URL, each with inputs
// this release stored that name nothing; a writer of the release before rewrote #41 and stamped its
// own rule. The link to each goes. #42, the control, is judged from its inputs: nobody's, at this
// rule, and the merged arm leaves it. #41 is nobody's at rule 0, and the merged arm offers it, so a
// read sees what the rewrite saw.
//
// #43 is an acme/app row whose stored citations name nothing, which a writer of the release before
// rewrote to ACME-1 by a citation of acme/app's issue 7, a GitHub issue ACME-1 links. #44 is
// ACME-1's by a citation of issue 8 that it stores, judged by this rule. The link to issue 7 goes.
// #43 is nobody's at once, as a row that stores no citations is, since its stored citations
// cannot say which citation held it. #44 still holds by issue 8 and is left as it was.
//
// Catches: a change of the links that judges a row of an older rule from inputs that rule's writer
// did not write and stamps it with this rule, so that a merged row's issue is never read again; and
// one that leaves a row of an older rule the issue's by a citation whose link is gone.
func TestALinkChangeJudgesARowOfAnOlderRuleAsARowWithNoInputs(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issueSeven := "https://github.com/" + linkedIssueRepo + "/issues/7"
	issueEight := "https://github.com/" + linkedIssueRepo + "/issues/8"
	seedLinkedIssue(t, ctx, pool, linkedPullURL(41), linkedPullURL(42), issueSeven, issueEight)
	eventAt := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	read := time.Now().Add(-time.Minute)
	issue := linkedIssue
	ownHead := true
	// Each row as a writer of this release stores it from a read of GitHub: merged, the inputs it
	// judged the issue from beside the issue, and stamped with this rule.
	write := func(repo string, number int, issueKey *string, source string, inputs AttributionInputs, commitsRead bool) {
		t.Helper()
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
			Repo: repo, Number: number, HeadSHA: fmt.Sprintf("%040d", number), HeadRef: "work/deploy-job",
			BaseRef: "main", State: "merged", IssueKey: issueKey, AttributionSource: source,
			AttributionComplete: true, Title: "Move the deploy job",
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), EventAt: &eventAt, CheckedAt: &read,
			Inputs: &inputs, CommitsRead: commitsRead, HeadInRepo: &ownHead,
		}); err != nil {
			t.Fatalf("store the merged head of %s#%d: %v", repo, number, err)
		}
	}
	namesNothing := AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}}
	write(linkedPullRepo, 41, &issue, string(sourceExternalLink), namesNothing, true)
	write(linkedPullRepo, 42, &issue, string(sourceExternalLink), namesNothing, true)
	write(linkedIssueRepo, 43, nil, string(sourceNone), namesNothing, true)
	write(linkedIssueRepo, 44, &issue, string(sourceGitHubIssueLink),
		AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{issueEight}, BranchKeys: []string{}}, false)
	// The release before this one judges by the rule below this build's, and its upsert writes the
	// issue, the source, the title and the rule, and no input column: #41 as it was, and #43 to
	// ACME-1 by the citation of issue 7 a later body holds, which no stored input names.
	olderRule := AttributionRuleVersion - 1
	stampAttributionRule(t, ctx, pool, linkedPullRepo, 41, olderRule)
	if _, err := pool.Exec(ctx, `
		update pull_request_heads set issue_key = $3, attribution_source = $4, attribution_rule = $5
		where repo = $1 and number = $2
	`, linkedIssueRepo, 43, linkedIssue, string(sourceGitHubIssueLink), olderRule); err != nil {
		t.Fatalf("rewrite %s#43 as the release before writes it: %v", linkedIssueRepo, err)
	}
	removed := []string{linkedPullURL(41), linkedPullURL(42), issueSeven}
	if _, err := pool.Exec(ctx, `delete from issue_external_links where issue_key = $1 and url = any($2::text[])`,
		linkedIssue, removed); err != nil {
		t.Fatalf("remove the links of %s: %v", linkedIssue, err)
	}

	changed, err := unrenamed().LinksChanged(ctx, pool, linkedIssue, removed)
	if err != nil {
		t.Fatalf("judge again the pull requests whose links changed: %v", err)
	}
	var named []int
	for _, head := range changed {
		named = append(named, head.Number)
	}
	if !slices.Equal(named, []int{43, 41, 42}) {
		t.Fatalf("the change judged %v, want [43 41 42] in key order: #43 is ACME-1's by a citation its stored citations, of an older rule, cannot name; #44 holds by a link that stays", named)
	}
	for _, want := range []struct {
		repo   string
		number int
		issue  *string
		source attributionSource
		rule   int
	}{
		{linkedPullRepo, 41, nil, sourceNone, 0},
		{linkedPullRepo, 42, nil, sourceNone, AttributionRuleVersion},
		{linkedIssueRepo, 43, nil, sourceNone, 0},
		{linkedIssueRepo, 44, &issue, sourceGitHubIssueLink, AttributionRuleVersion},
	} {
		row := readHead(t, ctx, pool, want.repo, want.number)
		if deref(row.IssueKey) != deref(want.issue) || row.AttributionSource != string(want.source) || row.attributionRule != want.rule {
			t.Errorf("%s#%d after the change = issue %q, source %s, rule %d; want issue %q, source %s, rule %d",
				want.repo, want.number, deref(row.IssueKey), row.AttributionSource, row.attributionRule,
				deref(want.issue), want.source, want.rule)
		}
	}
	if _, held := linkedHeads(t, ctx, pool)[43]; held {
		t.Errorf("the heads of %s hold #43, whose citation's link is gone", linkedIssue)
	}

	stale, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	var offered []int
	for _, head := range stale {
		if head.Number >= 41 && head.Number <= 44 {
			offered = append(offered, head.Number)
		}
	}
	slices.Sort(offered)
	if !slices.Equal(offered, []int{41, 43}) {
		t.Fatalf("the reconcile offers %v, want [41 43]: each is a row an older rule judged, and #42 and #44 this rule judged from what this release read", offered)
	}
}
