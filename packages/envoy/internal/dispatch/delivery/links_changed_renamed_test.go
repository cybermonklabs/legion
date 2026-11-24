package delivery

import (
	"fmt"
	"testing"
)

// A change of an issue's links finds a pull request whose row the build before this one wrote
// during a roll, under the name its repository was renamed away from: the link a person adds
// names the pull request under the repository's new name, and the read of the rows the change
// names takes every spelling of that URL (refs.Renames.URLSpellings), as the attribution's link
// sources compare it. The pull request is the issue's once the link holds it. A pull request of
// another number, stored under the old name too, is no row the link names.
func TestALinkChangeFindsARowStoredUnderTheRepositorysOldName(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedGadgetsIssue(t, ctx, pool, 32)
	seedRenamedGadgets(t, ctx, pool)
	for _, number := range []int{40, 41} {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, url, attribution_source,
				attribution_rule, attribution_title_keys, attribution_cited_issues,
				attribution_branch_keys, attribution_commit_keys, head_in_repo)
			values ($1, $2, $3, 'merged', $4, 'none', $5, '{}', '{}', '{}', '{}', true)
		`, gadgetsOldRepo, number, fmt.Sprintf("%040d", number),
			fmt.Sprintf("https://github.com/%s/pull/%d", gadgetsOldRepo, number), AttributionRuleVersion); err != nil {
			t.Fatalf("store %s#%d under the old name: %v", gadgetsOldRepo, number, err)
		}
	}
	linked := fmt.Sprintf("https://github.com/%s/pull/40", gadgetsRepo)
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`,
		issue, linked); err != nil {
		t.Fatalf("link %s to %s: %v", linked, issue, err)
	}

	changed, err := NewAttributor(renamesOf(t, ctx, pool)).LinksChanged(ctx, pool, issue, []string{linked})
	if err != nil {
		t.Fatalf("judge again the pull requests whose links of %s changed: %v", issue, err)
	}
	if len(changed) != 1 || changed[0].Number != 40 || changed[0].To == nil || *changed[0].To != issue {
		t.Fatalf("the link change of %s judged %+v, want #40 alone, to %s: its row is stored under %s, the name the link's %s was renamed away from",
			issue, changed, issue, gadgetsOldRepo, gadgetsRepo)
	}
	for number, want := range map[int]string{40: issue, 41: ""} {
		var held *string
		if err := pool.QueryRow(ctx, `select issue_key from pull_request_heads where repo = $1 and number = $2`,
			gadgetsOldRepo, number).Scan(&held); err != nil {
			t.Fatalf("read the issue of #%d: %v", number, err)
		}
		if got := deref(held); got != want {
			t.Errorf("#%d is %q's after the link change, want %q", number, got, want)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
