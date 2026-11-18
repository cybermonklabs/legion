package delivery

// Each test of this file makes one change in Dispatch that gives a stored pull request another
// issue. No event of the pull request comes with the change, and no pass runs. Each test stores
// a results row and its texts by SQL. Then it makes the change with the values that the route of
// the change writes. The reader by key and each reader by issue must give the new answer. Then
// the test makes the reverse change, and the readers must give the old answer again.
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

// The first source: an issue links the pull request after its head is stored.
func TestAPullRequestLinkMadeAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-151")
	storeStatementRow(t, ctx, pool, statementRow{number: 151, branch: "work/none", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{number: 152, branch: "work/none", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 151, "nobody from none")
	wantAnswer(t, ctx, pool, statementRepo, 152, "nobody from none, no facts")
	wantListed(t, ctx, pool, "ACME-151")
	for _, number := range []int{151, 152} {
		changeIssueLink(t, ctx, pool, "ACME-151", fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 151, "ACME-151 from external_link")
	wantListed(t, ctx, pool, "ACME-151", "#151")
	wantReaders(t, ctx, pool, "ACME-151", headOf(151), `at head true, newest acme/stmt#151, base "main"`)
	wantAnswer(t, ctx, pool, statementRepo, 152, "nobody from none, no facts")
	for _, number := range []int{151, 152} {
		changeIssueLink(t, ctx, pool, "ACME-151", fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number), "url", false)
	}
	wantAnswer(t, ctx, pool, statementRepo, 151, "nobody from none")
	wantListed(t, ctx, pool, "ACME-151")
	wantReaders(t, ctx, pool, "ACME-151", headOf(151), `at head false, newest none, base ""`)
}

// The second source: an issue links a GitHub issue that the body of the pull request cites. The
// external create writes only such links, with the kind github_issue.
func TestAGitHubIssueLinkOfACitedIssueMadeAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-161")
	body := "Closes https://github.com/acme/stmt/issues/77."
	storeStatementRow(t, ctx, pool, statementRow{number: 161, branch: "work/none", title: "no key", texts: readTexts(body)})
	storeStatementRow(t, ctx, pool, statementRow{number: 162, branch: "work/none", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 161, "nobody from none")
	wantListed(t, ctx, pool, "ACME-161")
	changeIssueLink(t, ctx, pool, "ACME-161", "https://github.com/acme/stmt/issues/77", "github_issue", true)
	wantAnswer(t, ctx, pool, statementRepo, 161, "ACME-161 from github_issue_link")
	wantListed(t, ctx, pool, "ACME-161", "#161")
	wantReaders(t, ctx, pool, "ACME-161", headOf(161), `at head true, newest acme/stmt#161, base "main"`)
	wantAnswer(t, ctx, pool, statementRepo, 162, "nobody from none, no facts")
	changeIssueLink(t, ctx, pool, "ACME-161", "https://github.com/acme/stmt/issues/77", "github_issue", false)
	wantAnswer(t, ctx, pool, statementRepo, 161, "nobody from none")
	wantListed(t, ctx, pool, "ACME-161")
}

// A new issue: the branch names an issue that a write creates after the head is stored.
func TestAnIssueCreatedAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "ACME", "ACME", statementRepo)
	storeStatementRow(t, ctx, pool, statementRow{number: 171, branch: "legion/ACME-171", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{number: 172, branch: "legion/ACME-172", title: "no key"})
	wantAnswer(t, ctx, pool, statementRepo, 171, "nobody from none")
	wantListed(t, ctx, pool, "ACME-171")
	statementIssues(t, ctx, pool, "ACME-171", "ACME-172")
	wantAnswer(t, ctx, pool, statementRepo, 171, "ACME-171 from branch")
	wantListed(t, ctx, pool, "ACME-171", "#171")
	wantReaders(t, ctx, pool, "ACME-171", headOf(171), `at head true, newest acme/stmt#171, base "main"`)
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
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/prnt-side", number: 181, branch: "legion/PRNT-181", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/prnt-side", number: 182, branch: "legion/PRNT-182", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "nobody from none")
	wantListed(t, ctx, pool, "PRNT-181")
	// The issue patch writes parent_key.
	execDispatchChange(t, ctx, pool, "move the two issues under PRNT-180", `update issues set parent_key = 'PRNT-180' where key in ('PRNT-181', 'PRNT-182')`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "PRNT-181 from branch")
	wantListed(t, ctx, pool, "PRNT-181", "#181")
	wantReaders(t, ctx, pool, "PRNT-181", headOf(181), `at head true, newest acme/prnt-side#181, base "main"`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 182, "nobody from none, no facts")
	wantListed(t, ctx, pool, "PRNT-182")
	execDispatchChange(t, ctx, pool, "move the two issues back", `update issues set parent_key = null where key in ('PRNT-181', 'PRNT-182')`)
	wantAnswer(t, ctx, pool, "acme/prnt-side", 181, "nobody from none")
	wantListed(t, ctx, pool, "PRNT-181")
}

// The repository mapping changes as the settings route writes it: first the default repository
// of a project, then a rename of a repository.
func TestAChangedRepositoryMappingReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "MAPD", "MAPD", "acme/elsewhere")
	insertProjectIssue(t, ctx, pool, "MAPD", "MAPD-191", 191)
	insertProjectIssue(t, ctx, pool, "MAPD", "MAPD-192", 192)
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/mapped", number: 191, branch: "legion/MAPD-191", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/mapped", number: 192, branch: "legion/MAPD-192", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "nobody from none")
	wantListed(t, ctx, pool, "MAPD-191")
	mapRepository(t, ctx, pool, "acme/mapped", "MAPD", true, nil)
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "MAPD-191 from branch")
	wantListed(t, ctx, pool, "MAPD-191", "#191")
	wantReaders(t, ctx, pool, "MAPD-191", headOf(191), `at head true, newest acme/mapped#191, base "main"`)
	wantAnswer(t, ctx, pool, "acme/mapped", 192, "nobody from none, no facts")
	wantListed(t, ctx, pool, "MAPD-192")
	execDispatchChange(t, ctx, pool, "delete the mapping of acme/mapped", `delete from repo_projects where repo = 'acme/mapped'`)
	mapRepository(t, ctx, pool, "acme/elsewhere", "MAPD", true, nil)
	wantAnswer(t, ctx, pool, "acme/mapped", 191, "nobody from none")
	wantListed(t, ctx, pool, "MAPD-191")

	// A rename: GitHub renamed the repository of the stored name to the repository of the issue.
	statementIssues(t, ctx, pool, "ACME-193", "ACME-194")
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/stmt-before", number: 193, branch: "legion/ACME-193", title: "no key", texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/stmt-before", number: 194, branch: "legion/ACME-194", title: "no key"})
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "nobody from none")
	wantListed(t, ctx, pool, "ACME-193")
	mapRepository(t, ctx, pool, "acme/stmt-before", "ACME", false, new(statementRepo))
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "ACME-193 from branch")
	wantListed(t, ctx, pool, "ACME-193", "#193")
	wantReaders(t, ctx, pool, "ACME-193", headOf(193), `at head true, newest acme/stmt#193, base "main"`)
	wantAnswer(t, ctx, pool, "acme/stmt-before", 194, "nobody from none, no facts")
	wantListed(t, ctx, pool, "ACME-194")
	execDispatchChange(t, ctx, pool, "delete the rename", `delete from repo_projects where repo = 'acme/stmt-before'`)
	wantAnswer(t, ctx, pool, "acme/stmt-before", 193, "nobody from none")
	wantListed(t, ctx, pool, "ACME-193")
}

// The link of a stored pull request moves from one issue to another, as the two issue routes
// write it. The patch of the first issue deletes the link, and the patch of the second adds it.
// The pull request with no texts keeps its stored issue, the first.
func TestALinkMovedToAnotherIssueAfterTheHeadIsStoredReachesEveryReader(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-201", "ACME-202")
	url := func(number int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", statementRepo, number) }
	storeStatementRow(t, ctx, pool, statementRow{number: 201, branch: "work/none", title: "no key", stored: new("ACME-201"), texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{number: 203, branch: "work/none", title: "no key", stored: new("ACME-201")})
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
	wantReaders(t, ctx, pool, "ACME-202", headOf(201), `at head true, newest acme/stmt#201, base "main"`)
	wantAnswer(t, ctx, pool, statementRepo, 203, "ACME-201 from none, no facts")
	wantListed(t, ctx, pool, "ACME-201", "#203")
	for _, number := range []int{201, 203} {
		changeIssueLink(t, ctx, pool, "ACME-202", url(number), "url", false)
		changeIssueLink(t, ctx, pool, "ACME-201", url(number), "url", true)
	}
	wantAnswer(t, ctx, pool, statementRepo, 201, "ACME-201 from external_link")
	wantListed(t, ctx, pool, "ACME-202")
	wantReaders(t, ctx, pool, "ACME-202", headOf(201), `at head false, newest none, base ""`)
}
