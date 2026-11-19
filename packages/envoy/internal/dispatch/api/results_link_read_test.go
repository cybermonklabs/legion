package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkedOpenPull is GitHub's answer for the open pull request number of repo at head sha. Its
// title, body and branch name no issue, so only a link ties it to an issue.
func linkedOpenPull(repo string, number int, sha string) string {
	return fmt.Sprintf(`{"number":%d,"title":"A change","body":"","html_url":"https://github.com/%s/pull/%d",
		"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
		"head":{"ref":"feature/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
		number, repo, number, sha, repo)
}

// A person links an open pull request to an issue by hand, with a PATCH of external_links (R5 of
// the results lookup). Dispatch has no event, no result and no head row of the pull request, and
// GitHub holds its head. One reconcile pass reads the pull request from GitHub and stores its head
// row. The results read of the issue then compares check k1 with that head. The test fails where
// the link write owes no read: then no step reads the pull request, and no head row is stored.
func TestALinkByHandStoresTheHeadOfThePullRequestItLinks(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Linked by hand")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	fake.pullAnswers(func(repo, number string) string {
		if repo != "acme/tools" || number != "12" {
			return ""
		}
		return linkedOpenPull(repo, 12, headCommit)
	})
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/tools/pull/12"}}})
	onePass(t, database, served)
	var sha string
	var readAt *time.Time
	err := database.Pool.QueryRow(context.Background(), `
		select head_sha, github_read_at from pull_request_heads where repo = 'acme/tools' and number = 12
	`).Scan(&sha, &readAt)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("after the link by hand and one pass, no head row of acme/tools#12 is stored; want the head that GitHub holds, %s", headCommit)
	}
	if err != nil {
		t.Fatalf("read the head row of acme/tools#12: %v", err)
	}
	if sha != headCommit || readAt == nil {
		t.Fatalf("the head row of acme/tools#12 holds %s with github_read_at %v; want %s, from a read of GitHub", sha, readAt, headCommit)
	}
	read := readResults(t, handler, issue.Key)
	if !comparedWithPull(t, read, "k1", 12) {
		check := checkOf(t, read, "k1")
		t.Fatalf("check k1 compares with %+v for the reason %q; want the head of acme/tools#12", check.ComparedWith, check.ComparedNothing)
	}
}

// owedReads is each read that pull_request_reads_owed holds, as "<repo>#<number> <reason>", in
// order.
func owedReads(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select repo || '#' || number || ' ' || reason from pull_request_reads_owed order by 1`)
	if err != nil {
		t.Fatalf("read the owed reads: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the owed reads: %v", err)
	}
	return owed
}

// A link by hand owes a read only for a pull request of a mapped repository, where no stored head
// row has the link as its URL. Six links owe nothing:
//
//   - a GitHub issue;
//   - a page of a pull request;
//   - a URL of another site;
//   - a pull request of a repository that the App reads and Dispatch does not map;
//   - a number with a zero before it;
//   - a pull request whose head row is stored.
//
// The same write with one more link owes one read, so the count can see an owe. A link held before
// the write, in other letters, owes nothing again.
func TestALinkByHandOwesNoReadForAURLThatIsNoNewPullRequestOfAMappedRepository(t *testing.T) {
	handler, database, _, _ := readThroughGitHub(t, "acme/tools", "acme/elsewhere")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Links that owe nothing")
	writeReadMergedRow(t, database, "acme/tools", 30, headCommit, "")
	links := []map[string]string{
		{"url": "https://github.com/acme/tools/issues/31", "kind": "github_issue"},
		{"url": "https://github.com/acme/tools/pull/32/files"},
		{"url": "https://example.com/acme/tools/pull/33"},
		{"url": "https://github.com/acme/elsewhere/pull/34"},
		{"url": "https://github.com/acme/tools/pull/035"},
		{"url": "https://github.com/acme/tools/pull/30"},
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": links})
	if owed := owedReads(t, database); len(owed) != 0 {
		t.Fatalf("after six links that name no pull request to read, the owed reads are %q; want none", owed)
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": append(slices.Clone(links),
		map[string]string{"url": "https://github.com/acme/tools/pull/36"})})
	if owed, want := owedReads(t, database), []string{"acme/tools#36 failure"}; !slices.Equal(owed, want) {
		t.Fatalf("after a link to acme/tools#36, which has no head row, the owed reads are %q; want %q", owed, want)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from pull_request_reads_owed`); err != nil {
		t.Fatalf("settle the owed read: %v", err)
	}
	patchIssue(t, handler, issue.Key, map[string]any{"external_links": append(slices.Clone(links),
		map[string]string{"url": "https://GitHub.com/Acme/Tools/pull/36"})})
	if owed := owedReads(t, database); len(owed) != 0 {
		t.Fatalf("after the same link in other letters, the owed reads are %q; want none", owed)
	}
}
