package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// R6 of the results lookup: a read-through of a result's commit stores a row for each other open
// or merged pull request that GitHub names at that commit, even where a pull request Dispatch
// holds has that commit as its head. The case is two open pull requests of one issue at one head,
// headCommit of acme/tools: #41, whose row and event Dispatch has, and #42, into another base,
// whose event never arrived. GitHub also names the closed #43, which was not merged, and the
// merged #44 at that head.

// sameHeadAnswer is GitHub's answer to GET /repos/{owner}/{repo}/commits/{sha}/pulls for
// headCommit, with the pull requests of numbers among #41 to #44, each titled after issueKey. Any
// other commit names none.
func sameHeadAnswer(issueKey string, numbers ...int) func(repo, sha string) string {
	return func(repo, sha string) string {
		if sha != headCommit {
			return `[]`
		}
		pulls := make([]string, 0, len(numbers))
		for _, number := range numbers {
			state, merged, mergedAt, base := "open", false, "null", "main"
			switch number {
			case 42:
				base = "release"
			case 43:
				state = "closed"
			case 44:
				state, merged, mergedAt, base = "closed", true, `"2026-11-18T01:00:00Z"`, "release"
			}
			pulls = append(pulls, fmt.Sprintf(`{"number":%d,"title":%q,"body":"","html_url":"https://github.com/%s/pull/%d",
				"state":%q,"merged":%t,"merged_at":%s,"updated_at":"2026-11-18T01:00:00Z",
				"head":{"ref":"work/shared","sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}`,
				number, sameHeadTitle(issueKey, number), repo, number, state, merged, mergedAt, sha, repo, base))
		}
		return "[" + strings.Join(pulls, ",") + "]"
	}
}

// sameHeadTitle is the title GitHub answers for pull request number of the case.
func sameHeadTitle(issueKey string, number int) string {
	return fmt.Sprintf("%s part %d", issueKey, number)
}

// sameHeadRow is one stored results row of acme/tools, as the case reads it.
type sameHeadRow struct {
	head, state, title string
	readFromGitHub     bool
}

// sameHeadRowOf is the stored results row of acme/tools#number, and found false where Dispatch
// holds none. readFromGitHub says that the row holds the mark of a read of GitHub
// (github_read_at); the row is read through to_jsonb, so the same query runs on a schema that
// does not have that column, and answers false there.
func sameHeadRowOf(t *testing.T, database *store.Store, number int) (sameHeadRow, bool) {
	t.Helper()
	var row sameHeadRow
	err := database.Pool.QueryRow(context.Background(), `
		select h.head_sha, h.state, coalesce(to_jsonb(h)->>'title', ''), to_jsonb(h)->>'github_read_at' is not null
		from pull_request_heads h where h.repo = 'acme/tools' and h.number = $1
	`, number).Scan(&row.head, &row.state, &row.title, &row.readFromGitHub)
	if errors.Is(err, pgx.ErrNoRows) {
		return sameHeadRow{}, false
	}
	if err != nil {
		t.Fatalf("read the stored row of acme/tools#%d: %v", number, err)
	}
	return row, true
}

// sameHeadFirst stores #41 as Dispatch holds it after its own event: an open row of the issue at
// headCommit. Its time is before GitHub's answer, so a read that writes GitHub's answer over it
// owes no read.
func sameHeadFirst(t *testing.T, database *store.Store, issueKey string) {
	t.Helper()
	writeHead(t, database, "acme/tools", 41, headCommit, "open", issueKey)
	if _, err := database.Pool.Exec(context.Background(), `
		update pull_request_heads set event_at = '2026-11-18T00:00:00Z' where repo = 'acme/tools' and number = 41
	`); err != nil {
		t.Fatalf("set the time of #41's row: %v", err)
	}
}

// wantSameHeadRows checks the rows that one read of headCommit must leave: a row of #42 and of
// #44, each with GitHub's head, state and title and the mark of a read of GitHub, and no row of
// the closed #43.
func wantSameHeadRows(t *testing.T, database *store.Store, issueKey, after string) {
	t.Helper()
	for _, want := range []struct {
		number int
		state  string
	}{{42, "open"}, {44, "merged"}} {
		row, found := sameHeadRowOf(t, database, want.number)
		if !found {
			t.Errorf("%s: no row of #%d, want the %s row GitHub names at %.7s: a second pull request at the same head gets no row (R6 of the results lookup)",
				after, want.number, want.state, headCommit)
			continue
		}
		if row.head != headCommit || row.state != want.state || row.title != sameHeadTitle(issueKey, want.number) || !row.readFromGitHub {
			t.Errorf("%s: #%d is %+v, want head %.7s, state %s, title %q, read from GitHub",
				after, want.number, row, headCommit, want.state, sameHeadTitle(issueKey, want.number))
		}
	}
	if row, found := sameHeadRowOf(t, database, 43); found {
		t.Errorf("%s: a row of the closed #43 (%+v), want none: a closed pull request that was not merged gets no row", after, row)
	}
}

// The record path. Recording a pass at headCommit, which #41 of the issue heads, asks GitHub for
// the commit's pull requests, and the record stores #42 and #44.
func TestARecordedResultStoresEveryPullRequestGitHubNamesAtItsCommit(t *testing.T) {
	handler, database, _, fake := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Two pull requests at one head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	sameHeadFirst(t, database, issue.Key)
	fake.answers(sameHeadAnswer(issue.Key, 41, 42, 43, 44))

	recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	t.Logf("after the record: asks=%d", fake.asks("acme/tools"))
	wantSameHeadRows(t, database, issue.Key, "after the record")
}

// The read path. The pass at headCommit is recorded while GitHub names only #41. Then #42 opens
// at the same head, #43 is opened and closed, and #44 merges, and no event of them arrives. One
// read of the results asks GitHub once and stores #42 and #44.
func TestAResultsReadStoresEveryPullRequestGitHubNamesAtAResultsCommit(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/tools")
	issue := issueInRepository(t, handler, "ACME", "acme/tools", "Two pull requests at one head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	sameHeadFirst(t, database, issue.Key)
	fake.answers(sameHeadAnswer(issue.Key, 41))
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if row, found := sameHeadRowOf(t, database, 42); found {
		t.Fatalf("SETUP: a row of #42 (%+v) before #42 opens", row)
	}

	fake.answers(sameHeadAnswer(issue.Key, 41, 42, 43, 44))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/tools")
	read := readResults(t, handler, issue.Key)
	asked := fake.asks("acme/tools") - spent
	var compared []int
	for _, basis := range checkOf(t, read, "k1").ComparedWith {
		if basis.PullRequest != nil {
			compared = append(compared, *basis.PullRequest)
		}
	}
	t.Logf("the read: asks=%d compared_with=%v", asked, compared)
	wantSameHeadRows(t, database, issue.Key, "after one read of the results")
	if asked != 1 {
		t.Errorf("the read asked GitHub %d times for the commit's pull requests, want 1: one read for each commit", asked)
	}
}
