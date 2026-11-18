package delivery

import "testing"

// wantAnswers asks PullRequestIssues for every key of want in one statement, and wants each
// answer (answerOf's words).
func wantAnswers(t *testing.T, q Querier, want map[PullRequestKey]string) {
	t.Helper()
	keys := make([]PullRequestKey, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	answers, err := PullRequestIssues(t.Context(), q, keys)
	if err != nil {
		t.Fatalf("PullRequestIssues(%v): %v", keys, err)
	}
	for key, line := range want {
		got := "absent"
		if answer, found := answers[key]; found {
			got = describeAttribution(answer)
		}
		if got != line {
			t.Errorf("%s#%d answers %q, want %q", key.Repo, key.Number, got, line)
		}
	}
}

// TestAttributionMatchesGitHubURLsWhateverTheirCase: GitHub spells an owner and a repository as
// their owners do (`Acme/Widgets`), and Dispatch stores an issue's GitHub link lowercased, so an
// external link naming the pull request (source 1) and one naming a GitHub issue its body cites
// (source 2) credit a pull request stored under that spelling.
func TestAttributionMatchesGitHubURLsWhateverTheirCase(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	// URLs no other row of this test links: a link's URL is unique across every issue.
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-201", "ATTR-202"}, map[string][]string{
		"ATTR-201": {"https://github.com/acme/widgets/pull/201"},
		"ATTR-202": {"https://github.com/acme/widgets/issues/277"},
	})
	storeStatementRow(t, ctx, pool, statementRow{repo: "Acme/Widgets", number: 201, branch: "main", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: "Acme/Widgets", number: 202, branch: "main", title: "no key", texts: readTexts("Closes #277")})
	wantAnswers(t, pool, map[PullRequestKey]string{
		{Repo: "Acme/Widgets", Number: 201}: "ATTR-201 from external_link",
		{Repo: "Acme/Widgets", Number: 202}: "ATTR-202 from github_issue_link",
	})
}

// TestAttributingStoredPullRequestsTakesEverySourceInOrder: one statement reads the five sources
// of many pull requests in one order. Every row's texts name issues from several sources, and
// each row's texts start one source later than the row before, so each source wins exactly one
// row; a statement that handed any two sources over in another order would credit some row from
// the wrong source.
func TestAttributingStoredPullRequestsTakesEverySourceInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-211", "ATTR-212", "ATTR-213", "ATTR-214", "ATTR-215"}, map[string][]string{
		"ATTR-212": {"https://github.com/acme/widgets/pull/210"},
		"ATTR-213": {"https://github.com/acme/widgets/issues/213"},
	})
	cites := "Fixes https://github.com/acme/widgets/issues/213."
	want := map[PullRequestKey]string{}
	for _, row := range []struct {
		number              int
		branch, title, body string
		messages            []string
		want                string
	}{
		// Every source names an issue, and ATTR-212's link names #210 itself: the external link
		// wins.
		{210, "legion/ATTR-214", "feat: ATTR-211", cites, []string{"ATTR-215"}, "ATTR-212 from external_link"},
		// No issue links #211, so what the body cites wins.
		{211, "legion/ATTR-214", "feat: ATTR-211", cites, []string{"ATTR-215"}, "ATTR-213 from github_issue_link"},
		{212, "legion/ATTR-214", "feat: ATTR-211", "", []string{"ATTR-215"}, "ATTR-214 from branch"},
		{213, "main", "feat: ATTR-211", "", []string{"ATTR-215"}, "ATTR-211 from title_body"},
		{214, "main", "feat: every source", "", []string{"ATTR-215"}, "ATTR-215 from commit_message"},
	} {
		storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: row.number, branch: row.branch, title: row.title,
			texts: &statementTexts{body: row.body, messages: row.messages}})
		want[PullRequestKey{Repo: "acme/widgets", Number: row.number}] = row.want
	}
	wantAnswers(t, pool, want)
}

// TestAttributingAStoredPullRequestTitledAfterAnotherRepositorysIssueKeepsItsOwn: the statement
// answers a two-key title as the head row's writer does. Its pair is
// TestAPullRequestTitledAfterAnotherRepositorysIssueKeepsItsOwn (pull_request_heads_test.go). One
// rule answers both — every candidate the five sources name (attributedTerms), the first of
// them held to the pull request's own repository (attributionHeld).
func TestAttributingAStoredPullRequestTitledAfterAnotherRepositorysIssueKeepsItsOwn(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	foreign := seedHeadIssue(t, ctx, pool, 9)
	own := seedGadgetsIssue(t, ctx, pool, 9)
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/gadgets", number: 220, branch: "main",
		title: foreign + ": the other repository's issue, and " + own + " (this one)", texts: readTexts("")})
	wantAnswers(t, pool, map[PullRequestKey]string{{Repo: "acme/gadgets", Number: 220}: own + " from title_body"})
}

// TestAttributingAStoredPullRequestUnderARepositorysOldNameCreditsItsIssue: the pull-request side
// of the renaming rule. A results row and its texts stored under the name its repository was
// renamed away from are held to an issue recorded under the new name, because every side of the
// statement goes through refs.CanonicalRepoSQL: the pull request answers its issue under both
// names, and a read by the issue lists it.
func TestAttributingAStoredPullRequestUnderARepositorysOldNameCreditsItsIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedRenamedGadgets(t, ctx, pool)
	own := seedGadgetsIssue(t, ctx, pool, 11)
	storeStatementRow(t, ctx, pool, statementRow{repo: gadgetsOldRepo, number: 240, branch: "main",
		title: own + ": the work, merged before the rename was recorded", texts: readTexts("")})
	wantAnswers(t, pool, map[PullRequestKey]string{
		{Repo: gadgetsOldRepo, Number: 240}: own + " from title_body",
		{Repo: gadgetsRepo, Number: 240}:    own + " from title_body",
	})
	wantListed(t, ctx, pool, own, "#240")
}

// TestAttributingAStoredPullRequestReadsOneCanonicalNameFromTwoMappingsOfOneRepository: the
// renaming rule answers one name however many rows name it. repo_projects' primary key is the
// repository's own spelling, so two mappings whose names differ only by case can exist — direct
// SQL or a hand repair, which 20261105075455_repo_projects_renamed_to expects — and the rule is
// spliced into the statement as an expression (refs.CanonicalRepoSQL): a scalar subquery over
// both rows would fail the whole statement with `more than one row returned by a subquery`, and
// every reader with it.
func TestAttributingAStoredPullRequestReadsOneCanonicalNameFromTwoMappingsOfOneRepository(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedRenamedGadgets(t, ctx, pool)
	own := seedGadgetsIssue(t, ctx, pool, 12)
	// The second mapping of the same name, as only direct SQL writes it: the route canonicalises
	// a mapping's path, so it would land on the row above instead.
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ('OldCo/Gadgets', 'GDGT', '{"kind":"system","id":"delivery-test"}'::jsonb, false, $1)
		on conflict (repo) do update set renamed_to = excluded.renamed_to
	`, gadgetsRepo); err != nil {
		t.Fatalf("record a second mapping of %s: %v", gadgetsOldRepo, err)
	}
	storeStatementRow(t, ctx, pool, statementRow{repo: gadgetsOldRepo, number: 241, branch: "main",
		title: own + ": the work, under a twice-mapped old name", texts: readTexts("")})
	wantAnswers(t, pool, map[PullRequestKey]string{{Repo: gadgetsOldRepo, Number: 241}: own + " from title_body"})
	wantListed(t, ctx, pool, own, "#241")
}

// TestAttributingABatchPicksEachRowsOwnRepositorysIssue: the statement resolves every candidate
// of every pull request it reads in one walk (refs.IssueRepoTerms over all of them, then
// `distinct on (pr_repo, pr_number)`), so the pick must be per pull request and not per walk. Two
// pull requests, one in each of two repositories, whose titles name the same two issue keys in
// the same order, read in one statement, are the case that tells them apart: each must take the
// issue of its own repository, the widgets one the first key and the gadgets one the second.
func TestAttributingABatchPicksEachRowsOwnRepositorysIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	widgets := seedHeadIssue(t, ctx, pool, 10)
	gadgets := seedGadgetsIssue(t, ctx, pool, 10)
	title := widgets + " and " + gadgets + ", in that order"
	for repo, number := range map[string]int{"acme/widgets": 230, "acme/gadgets": 231} {
		storeStatementRow(t, ctx, pool, statementRow{repo: repo, number: number, branch: "main", title: title, texts: readTexts("")})
	}
	wantAnswers(t, pool, map[PullRequestKey]string{
		{Repo: "acme/widgets", Number: 230}: widgets + " from title_body",
		{Repo: "acme/gadgets", Number: 231}: gadgets + " from title_body",
	})
}

// TestAStoredPullRequestTitledAfterTwoOfItsOwnIssuesTakesTheFirst: precedence inside one
// repository is what it always was. A candidate is passed over only where it fails the
// repository test (attributionHeld), so two candidates that both pass it keep their ranked
// order and the first one named wins. The rule reaches past a candidate of another
// repository, never past one of this repository's own.
func TestAStoredPullRequestTitledAfterTwoOfItsOwnIssuesTakesTheFirst(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-221", "ATTR-222"}, nil)
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 221, branch: "main",
		title: "ATTR-221 and ATTR-222, both of this repository", texts: readTexts("")})
	wantAnswers(t, pool, map[PullRequestKey]string{{Repo: "acme/widgets", Number: 221}: "ATTR-221 from title_body"})
}
