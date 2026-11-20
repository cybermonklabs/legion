package delivery

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

// A change of an issue's links that names a row no writer stored the inputs of — a row of the
// release before, which the reconcile has not read again yet — leaves the row for the reconcile to
// judge again (LinksChanged): the stored facts cannot judge it, and a merged row the current rule
// judged is visited by nothing else. The row whose issue the removed link gave is the issue's no
// longer, at the change. A row the change linked keeps its stored issue until a read judges it,
// and a row whose URL no changed link names is left exactly as it was.
func TestALinkChangeHasTheReconcileJudgeTheRowAgain(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedLinkedIssue(t, ctx, pool, linkedPullURL(21), linkedPullURL(22))
	eventAt := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	read := time.Now().Add(-time.Minute)
	issue := linkedIssue
	// #21 and #22 merged and the issue's by its links, #23 merged and nobody's: each judged by
	// this build's rule, which an insert stamps.
	for number, credited := range map[int]bool{21: true, 22: true, 23: false} {
		head := PullRequestHead{
			Repo: linkedPullRepo, Number: number, HeadSHA: fmt.Sprintf("%040d", number),
			HeadRef: "work/deploy-job", BaseRef: "main", State: "merged", AttributionSource: string(sourceNone),
			AttributionComplete: true, Title: "Move the deploy job", URL: linkedPullURL(number),
			EventAt: &eventAt, CheckedAt: &read,
		}
		if credited {
			head.IssueKey, head.AttributionSource = &issue, string(sourceExternalLink)
		}
		if err := UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
			t.Fatalf("store the merged head of %s#%d: %v", linkedPullRepo, number, err)
		}
	}
	// The change: the link to #21 goes, and one to #23 comes, in another spelling of its URL.
	if _, err := pool.Exec(ctx, `delete from issue_external_links where issue_key = $1 and url = $2`, linkedIssue, linkedPullURL(21)); err != nil {
		t.Fatalf("remove the link to #21: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')`,
		linkedIssue, "https://github.com/ACME/infra/pull/23"); err != nil {
		t.Fatalf("link #23: %v", err)
	}

	changed, err := LinksChanged(ctx, pool, linkedIssue, []string{linkedPullURL(21), "https://github.com/ACME/infra/pull/23"})
	if err != nil {
		t.Fatalf("judge again the pull requests whose links changed: %v", err)
	}
	want := []HeadOfChangedLink{
		{Repo: linkedPullRepo, Number: 21, URL: linkedPullURL(21), From: &issue, To: nil, Source: string(sourceNone)},
		{Repo: linkedPullRepo, Number: 23, URL: linkedPullURL(23), From: nil, To: nil, Source: string(sourceNone)},
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("the pull requests the change named = %+v, want %+v", changed, want)
	}
	if row := readHead(t, ctx, pool, linkedPullRepo, 21); row.IssueKey != nil || row.AttributionSource != string(sourceNone) || row.attributionRule != 0 {
		t.Fatalf("#21 after its link was removed = %+v (rule %d), want nobody's, source none, rule 0", row.PullRequestHead, row.attributionRule)
	}
	if row := readHead(t, ctx, pool, linkedPullRepo, 23); row.IssueKey != nil || row.attributionRule != 0 {
		t.Fatalf("#23 after it was linked = %+v (rule %d), want nobody's until a read judges it, rule 0", row.PullRequestHead, row.attributionRule)
	}
	if row := readHead(t, ctx, pool, linkedPullRepo, 22); row.IssueKey == nil || *row.IssueKey != linkedIssue ||
		row.AttributionSource != string(sourceExternalLink) || row.attributionRule != AttributionRuleVersion {
		t.Fatalf("#22, whose link did not change, = %+v (rule %d), want %s's by its link, rule %d",
			row.PullRequestHead, row.attributionRule, linkedIssue, AttributionRuleVersion)
	}
	heads := linkedHeads(t, ctx, pool)
	if _, held := heads[21]; held {
		t.Fatalf("the heads of %s = %+v, want no #21: its link is gone", linkedIssue, heads)
	}
	if _, held := heads[22]; !held {
		t.Fatalf("the heads of %s = %+v, want #22: %s still links it", linkedIssue, heads, linkedIssue)
	}

	stale, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	var offered []int
	for _, head := range stale {
		if head.Repo == linkedPullRepo {
			offered = append(offered, head.Number)
		}
	}
	slices.Sort(offered)
	if !slices.Equal(offered, []int{21, 23}) {
		t.Fatalf("the reconcile offers %s's rows %v, want [21 23]: the change named both, and nothing else revisits a merged row", linkedPullRepo, offered)
	}
}
