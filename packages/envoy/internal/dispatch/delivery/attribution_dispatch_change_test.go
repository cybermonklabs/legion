package delivery

// Each test of this file makes one change in Dispatch that gives a stored pull request another
// issue. No event of the pull request comes with the change, and no pass runs. Each test stores
// a results row, its texts and a merged timeline row by SQL, each row with the issue a writer of
// the base stored. Then it makes the change with the values that the route of the change writes.
// The reader by key and each reader by issue, of the results and of the timeline (wantReaders),
// must give the new answer. Then the test makes the reverse change, and the readers must give the
// old answer again.
//
// Control in each test: a second pull request of the same shape has no texts row. Its stored
// issue is the old answer, as a writer of the base stores it. After the same change it keeps the
// old answer, because the statement reads a stored issue only for a pull request with no facts.

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// changeIssueLink adds or deletes the link from issue to url, as the issue routes write a link.
// The patch inserts each link with its kind, and it deletes each link that it does not send.
func changeIssueLink(t *testing.T, ctx context.Context, pool *store.Pool, issue, url, kind string, add bool) {
	t.Helper()
	var err error
	if add {
		_, err = pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, $3)`, issue, url, kind)
	} else {
		_, err = pool.Exec(ctx, `delete from issue_external_links where issue_key = $1 and url = $2`, issue, url)
	}
	if err != nil {
		t.Fatalf("change the link of %s to %s: %v", issue, url, err)
	}
}

// execDispatchChange runs one statement of a change, and stops the test where it fails.
func execDispatchChange(t *testing.T, ctx context.Context, pool *store.Pool, what, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// insertProjectIssue makes one issue of project. The issue has no repository of its own, so it
// takes the default repository of its project.
func insertProjectIssue(t *testing.T, ctx context.Context, pool *store.Pool, project, key string, number int) {
	t.Helper()
	execDispatchChange(t, ctx, pool, "insert "+key, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}', $1)`, key, project, number)
}

// mapRepository writes the mapping of repo as the settings route writes it. A default first
// takes the default from each other repository of the project.
func mapRepository(t *testing.T, ctx context.Context, pool *store.Pool, repo, project string, isDefault bool, renamedTo *string) {
	t.Helper()
	if isDefault {
		execDispatchChange(t, ctx, pool, "take the default of "+project, `
			update repo_projects set is_default = false
			where project = $1 and is_default and repo <> $2`, project, repo)
	}
	execDispatchChange(t, ctx, pool, "map "+repo, `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ($1, $2, '{"kind":"system","id":"test"}'::jsonb, $3, $4)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by,
			is_default = excluded.is_default, renamed_to = excluded.renamed_to`, repo, project, isDefault, renamedTo)
}

// storeChangedPullRequests stores the two pull requests of a test: the one with texts and the
// control with none. Each has a results row and a timeline row that store stored, and the
// timeline row is merged, so the timeline's window reads it (mergeTimelineRow).
func storeChangedPullRequests(t *testing.T, ctx context.Context, pool *store.Pool, repo string, stored *string, withTexts, control statementRow) {
	t.Helper()
	for _, row := range []statementRow{withTexts, control} {
		row.repo, row.stored, row.timeline = repo, stored, &statementTimeline{issue: stored}
		storeStatementRow(t, ctx, pool, row)
		mergeTimelineRow(t, ctx, pool, repo, row.number)
	}
}

// The first source: an issue links the pull request after its head is stored.
func TestAPullRequestLinkMadeAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-151")
	storeChangedPullRequests(t, ctx, pool, statementRepo, nil,
		statementRow{number: 151, branch: "work/none", title: "no key", texts: readTexts("")},
		statementRow{number: 152, branch: "work/none", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 151, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 152, "nobody from none, no facts")
	wantListed(t, ctx, pool, "ACME-151")
	for _, number := range []int{151, 152} {
		changeIssueLink(t, ctx, pool, "ACME-151", fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 151, "ACME-151 from external_link")
	wantListed(t, ctx, pool, "ACME-151", "#151")
	wantReaders(t, ctx, pool, "ACME-151", headOf(151), `at head true, newest acme/stmt#151, base "main", window [acme/stmt#151], of issues [acme/stmt#151]`)
	wantAnswer(t, ctx, pool, statementRepo, 152, "nobody from none, no facts")
	for _, number := range []int{151, 152} {
		changeIssueLink(t, ctx, pool, "ACME-151", fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number), "url", false)
	}
	wantAnswer(t, ctx, pool, statementRepo, 151, "nobody from none")
	wantListed(t, ctx, pool, "ACME-151")
	wantReaders(t, ctx, pool, "ACME-151", headOf(151), `at head false, newest none, base "", window [], of issues []`)
}

// The first source in GitHub's /issues/ form of the number, the URL `dispatch issue --external`
// stores (linkedPullRequest): an issue links the pull request after its head is stored, and no
// pass runs. The reader by key and each reader by issue, of the results and of the timeline, give
// the new answer at once, and lose it at once when the link goes. Control in the same test: #156
// has no texts, so its stored issue (none) stays its answer.
func TestAPullRequestLinkInItsIssuesFormMadeAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-155")
	storeChangedPullRequests(t, ctx, pool, statementRepo, nil,
		statementRow{number: 155, branch: "work/none", title: "no key", texts: readTexts("")},
		statementRow{number: 156, branch: "work/none", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 155, "nobody from none")
	wantListed(t, ctx, pool, "ACME-155")
	for _, number := range []int{155, 156} {
		changeIssueLink(t, ctx, pool, "ACME-155", fmt.Sprintf("https://github.com/%s/issues/%d", statementRepo, number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 155, "ACME-155 from external_link")
	wantListed(t, ctx, pool, "ACME-155", "#155")
	wantReaders(t, ctx, pool, "ACME-155", headOf(155), `at head true, newest acme/stmt#155, base "main", window [acme/stmt#155], of issues [acme/stmt#155]`)
	wantAnswer(t, ctx, pool, statementRepo, 156, "nobody from none, no facts")
	for _, number := range []int{155, 156} {
		changeIssueLink(t, ctx, pool, "ACME-155", fmt.Sprintf("https://github.com/%s/issues/%d", statementRepo, number), "url", false)
	}
	wantAnswer(t, ctx, pool, statementRepo, 155, "nobody from none")
	wantListed(t, ctx, pool, "ACME-155")
	wantReaders(t, ctx, pool, "ACME-155", headOf(155), `at head false, newest none, base "", window [], of issues []`)
}

// Two issues hold one pull request in its two forms, as two links stored before the link check
// compared the forms do: ACME-158 links /pull/157 first, and then ACME-157 links /issues/157. The
// pull request's head serves the issue whose key comes first in byte order, ACME-157, in every
// reader, whichever link was written first; and dispatch attribution counts the pair once among the
// links in two cases. When ACME-157's link goes, the head serves ACME-158.
func TestAPullRequestTwoIssuesLinkInItsTwoFormsServesTheFirstKey(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-157", "ACME-158")
	storeChangedPullRequests(t, ctx, pool, statementRepo, nil,
		statementRow{number: 157, branch: "work/none", title: "no key", texts: readTexts("")},
		statementRow{number: 159, branch: "work/none", title: "no key"})
	changeIssueLink(t, ctx, pool, "ACME-158", fmt.Sprintf("https://github.com/%s/pull/157", statementRepo), "url", true)
	changeIssueLink(t, ctx, pool, "ACME-157", fmt.Sprintf("https://github.com/%s/issues/157", statementRepo), "github_issue", true)
	wantAnswer(t, ctx, pool, statementRepo, 157, "ACME-157 from external_link")
	wantListed(t, ctx, pool, "ACME-157", "#157")
	wantListed(t, ctx, pool, "ACME-158")
	wantReaders(t, ctx, pool, "ACME-157", headOf(157), `at head true, newest acme/stmt#157, base "main", window [acme/stmt#157], of issues [acme/stmt#157]`)
	counts, err := ReadAttributionCounts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if counts.LinksInTwoCases != 1 {
		t.Errorf("dispatch attribution counts %d links in two cases, want 1: one pull request in its two forms", counts.LinksInTwoCases)
	}
	changeIssueLink(t, ctx, pool, "ACME-157", fmt.Sprintf("https://github.com/%s/issues/157", statementRepo), "github_issue", false)
	wantAnswer(t, ctx, pool, statementRepo, 157, "ACME-158 from external_link")
	wantListed(t, ctx, pool, "ACME-158", "#157")
	wantListed(t, ctx, pool, "ACME-157")
}

// The second source: an issue links a GitHub issue that the body of the pull request cites. The
// external create writes only such links, with the kind github_issue.
func TestAGitHubIssueLinkOfACitedIssueMadeAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-161")
	body := "Closes https://github.com/acme/stmt/issues/77."
	storeChangedPullRequests(t, ctx, pool, statementRepo, nil,
		statementRow{number: 161, branch: "work/none", title: "no key", texts: readTexts(body)},
		statementRow{number: 162, branch: "work/none", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 161, "nobody from none")
	wantListed(t, ctx, pool, "ACME-161")
	changeIssueLink(t, ctx, pool, "ACME-161", "https://github.com/acme/stmt/issues/77", "github_issue", true)
	wantAnswer(t, ctx, pool, statementRepo, 161, "ACME-161 from github_issue_link")
	wantListed(t, ctx, pool, "ACME-161", "#161")
	wantReaders(t, ctx, pool, "ACME-161", headOf(161), `at head true, newest acme/stmt#161, base "main", window [acme/stmt#161], of issues [acme/stmt#161]`)
	wantAnswer(t, ctx, pool, statementRepo, 162, "nobody from none, no facts")
	changeIssueLink(t, ctx, pool, "ACME-161", "https://github.com/acme/stmt/issues/77", "github_issue", false)
	wantAnswer(t, ctx, pool, statementRepo, 161, "nobody from none")
	wantListed(t, ctx, pool, "ACME-161")
	wantReaders(t, ctx, pool, "ACME-161", headOf(161), `at head false, newest none, base "", window [], of issues []`)
}

// A new issue: the branch names an issue that a write creates after the head is stored.
func TestAnIssueCreatedAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "ACME", "ACME", statementRepo)
	storeChangedPullRequests(t, ctx, pool, statementRepo, nil,
		statementRow{number: 171, branch: "legion/ACME-171", title: "no key", texts: readTexts("")},
		statementRow{number: 172, branch: "legion/ACME-172", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 171, "nobody from none")
	wantListed(t, ctx, pool, "ACME-171")
	statementIssues(t, ctx, pool, "ACME-171", "ACME-172")
	wantAnswer(t, ctx, pool, statementRepo, 171, "ACME-171 from branch")
	wantListed(t, ctx, pool, "ACME-171", "#171")
	wantReaders(t, ctx, pool, "ACME-171", headOf(171), `at head true, newest acme/stmt#171, base "main", window [acme/stmt#171], of issues [acme/stmt#171]`)
	wantAnswer(t, ctx, pool, statementRepo, 172, "nobody from none, no facts")
	wantListed(t, ctx, pool, "ACME-172")
	// No route deletes an issue. So the reverse change is the state before, which SQL makes.
	execDispatchChange(t, ctx, pool, "delete the two issues", `delete from issues where key in ('ACME-171', 'ACME-172')`)
	wantAnswer(t, ctx, pool, statementRepo, 171, "nobody from none")
	wantListed(t, ctx, pool, "ACME-171")
}

// A new parent moves an issue to another repository. The project PRNT maps two repositories.
// Its issues take the default, and PRNT-180 holds the other one, so a child of PRNT-180 takes it.
func TestANewParentThatMovesTheIssueToThePullRequestsRepositoryReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "PRNT", "PRNT", "acme/prnt-main")
	mapRepository(t, ctx, pool, "acme/prnt-side", "PRNT", false, nil)
	for i, key := range []string{"PRNT-180", "PRNT-181", "PRNT-182"} {
		insertProjectIssue(t, ctx, pool, "PRNT", key, 180+i)
	}
	execDispatchChange(t, ctx, pool, "give PRNT-180 its own repository", `update issues set repo = 'acme/prnt-side' where key = 'PRNT-180'`)
	storeChangedPullRequests(t, ctx, pool, "acme/prnt-side", nil,
		statementRow{number: 181, branch: "legion/PRNT-181", title: "no key", texts: readTexts("")},
		statementRow{number: 182, branch: "legion/PRNT-182", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "nobody from none")
	wantListed(t, ctx, pool, "PRNT-181")
	// The issue patch writes parent_key.
	execDispatchChange(t, ctx, pool, "move the two issues under PRNT-180", `update issues set parent_key = 'PRNT-180' where key in ('PRNT-181', 'PRNT-182')`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "PRNT-181 from branch")
	wantListed(t, ctx, pool, "PRNT-181", "#181")
	wantReaders(t, ctx, pool, "PRNT-181", headOf(181), `at head true, newest acme/prnt-side#181, base "main", window [acme/prnt-side#181], of issues [acme/prnt-side#181]`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 182, "nobody from none, no facts")
	wantListed(t, ctx, pool, "PRNT-182")
	execDispatchChange(t, ctx, pool, "move the two issues back", `update issues set parent_key = null where key in ('PRNT-181', 'PRNT-182')`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "nobody from none")
	wantListed(t, ctx, pool, "PRNT-181")
	wantReaders(t, ctx, pool, "PRNT-181", headOf(181), `at head false, newest none, base "", window [], of issues []`)
}

// The repository mapping changes as the settings route writes it: first the default repository
// of a project, then a rename of a repository.
func TestAChangedRepositoryMappingReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "MAPD", "MAPD", "acme/elsewhere")
	insertProjectIssue(t, ctx, pool, "MAPD", "MAPD-191", 191)
	insertProjectIssue(t, ctx, pool, "MAPD", "MAPD-192", 192)
	storeChangedPullRequests(t, ctx, pool, "acme/mapped", nil,
		statementRow{number: 191, branch: "legion/MAPD-191", title: "no key", texts: readTexts("")},
		statementRow{number: 192, branch: "legion/MAPD-192", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "nobody from none")
	wantListed(t, ctx, pool, "MAPD-191")
	mapRepository(t, ctx, pool, "acme/mapped", "MAPD", true, nil)
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "MAPD-191 from branch")
	wantListed(t, ctx, pool, "MAPD-191", "#191")
	wantReaders(t, ctx, pool, "MAPD-191", headOf(191), `at head true, newest acme/mapped#191, base "main", window [acme/mapped#191], of issues [acme/mapped#191]`)
	wantAnswer(t, ctx, pool, "acme/mapped", 192, "nobody from none, no facts")
	wantListed(t, ctx, pool, "MAPD-192")
	execDispatchChange(t, ctx, pool, "delete the mapping of acme/mapped", `delete from repo_projects where repo = 'acme/mapped'`)
	mapRepository(t, ctx, pool, "acme/elsewhere", "MAPD", true, nil)
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "nobody from none")
	wantListed(t, ctx, pool, "MAPD-191")
	wantReaders(t, ctx, pool, "MAPD-191", headOf(191), `at head false, newest none, base "", window [], of issues []`)

	// A rename: GitHub renamed the repository of the stored name to the repository of the issue.
	statementIssues(t, ctx, pool, "ACME-193", "ACME-194")
	storeChangedPullRequests(t, ctx, pool, "acme/stmt-before", nil,
		statementRow{number: 193, branch: "legion/ACME-193", title: "no key", texts: readTexts("")},
		statementRow{number: 194, branch: "legion/ACME-194", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "nobody from none")
	wantListed(t, ctx, pool, "ACME-193")
	mapRepository(t, ctx, pool, "acme/stmt-before", "ACME", false, new(statementRepo))
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "ACME-193 from branch")
	wantListed(t, ctx, pool, "ACME-193", "#193")
	wantReaders(t, ctx, pool, "ACME-193", headOf(193), `at head true, newest acme/stmt#193, base "main", window [acme/stmt-before#193], of issues [acme/stmt-before#193]`)
	wantAnswer(t, ctx, pool, "acme/stmt-before", 194, "nobody from none, no facts")
	wantListed(t, ctx, pool, "ACME-194")
	execDispatchChange(t, ctx, pool, "delete the rename", `delete from repo_projects where repo = 'acme/stmt-before'`)
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "nobody from none")
	wantListed(t, ctx, pool, "ACME-193")
	wantReaders(t, ctx, pool, "ACME-193", headOf(193), `at head false, newest none, base "", window [], of issues []`)
}

// The link of a stored pull request moves from one issue to another, as the two issue routes
// write it. The patch of the first issue deletes the link, and the patch of the second adds it.
// The pull request with no texts keeps its stored issue, the first.
func TestALinkMovedToAnotherIssueAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-201", "ACME-202")
	url := func(number int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number) }
	storeChangedPullRequests(t, ctx, pool, statementRepo, new("ACME-201"),
		statementRow{number: 201, branch: "work/none", title: "no key", texts: readTexts("")},
		statementRow{number: 203, branch: "work/none", title: "no key"})
	for _, number := range []int{201, 203} {
		changeIssueLink(t, ctx, pool, "ACME-201", url(number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 201, "ACME-201 from external_link")
	wantAnswer(t, ctx, pool, statementRepo, 203, "ACME-201 from none, no facts")
	wantListed(t, ctx, pool, "ACME-202")
	for _, number := range []int{201, 203} {
		changeIssueLink(t, ctx, pool, "ACME-201", url(number), "url", false)
		changeIssueLink(t, ctx, pool, "ACME-202", url(number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 201, "ACME-202 from external_link")
	wantListed(t, ctx, pool, "ACME-202", "#201")
	wantReaders(t, ctx, pool, "ACME-202", headOf(201), `at head true, newest acme/stmt#201, base "main", window [acme/stmt#201], of issues [acme/stmt#201]`)
	wantAnswer(t, ctx, pool, statementRepo, 203, "ACME-201 from none, no facts")
	wantListed(t, ctx, pool, "ACME-201", "#203")
	for _, number := range []int{201, 203} {
		changeIssueLink(t, ctx, pool, "ACME-202", url(number), "url", false)
		changeIssueLink(t, ctx, pool, "ACME-201", url(number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 201, "ACME-201 from external_link")
	wantListed(t, ctx, pool, "ACME-202")
	wantReaders(t, ctx, pool, "ACME-202", headOf(201), `at head false, newest none, base "", window [], of issues []`)
}
