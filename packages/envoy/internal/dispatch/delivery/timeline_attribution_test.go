package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedAttributedHead stores the results row and the texts of repo#number, so that the statement
// gives the pull request the issue from its branch, legion/<issue>. The texts hold the commit
// messages as read, and the messages name no key. The issue must exist and resolve to repo. The
// helper fails the test where the statement gives another answer. It writes no timeline row.
func seedAttributedHead(t *testing.T, pool *store.Pool, repo string, number int, issue string) {
	t.Helper()
	ctx := t.Context()
	storeStatementRow(t, ctx, pool, statementRow{
		repo: repo, number: number, branch: "legion/" + issue,
		title: fmt.Sprintf("pull request %d", number), texts: readTexts(""),
	})
	key := PullRequestKey{Repo: repo, Number: number}
	answers, err := PullRequestIssues(ctx, pool, []PullRequestKey{key})
	if err != nil {
		t.Fatalf("PullRequestIssues(%s#%d): %v", repo, number, err)
	}
	answer := answers[key]
	if answer.IssueKey == nil || *answer.IssueKey != issue || !answer.Known {
		t.Fatalf("seedAttributedHead(%s#%d, %s): the statement answers %+v, want %s: the issue must exist and resolve to %s",
			repo, number, issue, answer, issue, repo)
	}
}

// The repositories of the timeline's tests. Each test maps the project of its issues to one of
// them as the project's default, so that each issue resolves to that repository.
const (
	timelineRepo    = "acme/timeline"
	timelineOldRepo = "acme/timeline-old"
	timelineNewRepo = "acme/timeline-new"
)

// timelineJob is the production job of the deploy runs of these tests. timelineProduction is the
// production rule that reads it.
const timelineJob = "timeline-release / timeline-release"

var timelineProduction = Production{JobNames: []string{timelineJob}}

// The times of these tests. Each timeline row merges on the day of timelineMerged, its number of
// seconds after it (mergeTimelineRow). Each deploy run starts at timelineRunCreated, after every
// merge, and its production job ends at timelineShippedAt (seedShippingRun). timelineWaitingAt is
// between the two.
var (
	timelineMerged     = time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC)
	timelineRunCreated = timelineMerged.Add(2 * time.Hour)
	timelineShippedAt  = timelineRunCreated.Add(10 * time.Minute)
	timelineWaitingAt  = timelineRunCreated.Add(5 * time.Minute)
)

// timelineBody is the body of each pull request that the fake GitHub of these tests serves
// (timelinePull).
const timelineBody = "The body of the change."

// projectIssues makes each key an issue of the project that its prefix names. It maps repo to that
// project as the project's default, so each issue resolves to repo.
func projectIssues(t *testing.T, ctx context.Context, pool *store.Pool, repo string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		project, digits, _ := strings.Cut(key, "-")
		number, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("the issue key %q is not <project>-<number>", key)
		}
		seedProjectRepository(t, ctx, pool, project, project, repo)
		insertProjectIssue(t, ctx, pool, project, key, number)
	}
}

// mergeTimelineRow sets the merge of the timeline row of repo#number to timelineMerged and number
// seconds. So ListPullRequestsInWindow over the day of timelineMerged reads the row.
func mergeTimelineRow(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) {
	t.Helper()
	merged := timelineMerged.Add(time.Duration(number) * time.Second)
	tag, err := pool.Exec(ctx, `update delivery_pull_requests set created_at = $3, merged_at = $4 where repo = $1 and number = $2`,
		repo, number, merged.Add(-time.Hour), merged)
	if err != nil {
		t.Fatalf("merge the timeline row of %s#%d: %v", repo, number, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("merge the timeline row of %s#%d: the update found %d rows, want 1", repo, number, tag.RowsAffected())
	}
}

// storedTimelineIssue is the issue_key column of the timeline row of repo#number: the issue that a
// writer stored there.
func storedTimelineIssue(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) *string {
	t.Helper()
	var issue *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&issue); err != nil {
		t.Fatalf("read the stored issue of the timeline row of %s#%d: %v", repo, number, err)
	}
	return issue
}

// timelineAnswer is the issue that a reader of the timeline gives pr: its key, or "nobody". It adds
// ", not known" where IssueNotKnown is true.
func timelineAnswer(pr DeliveryPullRequest) string {
	answer := "nobody"
	if pr.IssueKey != nil {
		answer = *pr.IssueKey
	}
	if pr.IssueNotKnown {
		answer += ", not known"
	}
	return answer
}

// timelineOf is what ListPullRequestsInWindow gives repo#number over the day of timelineMerged
// (timelineAnswer), or "absent" where the window holds no such row.
func timelineOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	day := timelineMerged.Truncate(24 * time.Hour)
	prs, err := ListPullRequestsInWindow(ctx, pool, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ListPullRequestsInWindow: %v", err)
	}
	for _, pr := range prs {
		if pr.Repo == repo && pr.Number == number {
			return timelineAnswer(pr)
		}
	}
	return "absent"
}

func wantTimeline(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, want string) {
	t.Helper()
	if got := timelineOf(t, ctx, pool, repo, number); got != want {
		t.Errorf("ListPullRequestsInWindow gives %s#%d %q, want %q", repo, number, got, want)
	}
}

// waitingOf is what ListPullRequestsWaitingAt gives repo#number of the deploy repository repo at
// timelineWaitingAt (timelineAnswer), or "absent" where it lists no such row.
func waitingOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	waiting, err := ListPullRequestsWaitingAt(ctx, pool, repo, timelineProduction, timelineWaitingAt)
	if err != nil {
		t.Fatalf("ListPullRequestsWaitingAt(%s): %v", repo, err)
	}
	for _, pr := range waiting {
		if pr.Repo == repo && pr.Number == number {
			return timelineAnswer(pr.DeliveryPullRequest)
		}
	}
	return "absent"
}

func wantWaiting(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, want string) {
	t.Helper()
	if got := waitingOf(t, ctx, pool, repo, number); got != want {
		t.Errorf("ListPullRequestsWaitingAt(%s) gives #%d %q, want %q", repo, number, got, want)
	}
}

// wantShipped fails the test unless listUnrecordedShipped lists want for the deploy repository
// repo, each entry "#<number> <issue>". It answers the list.
func wantShipped(t *testing.T, ctx context.Context, pool *store.Pool, repo string, want ...string) []shippedPullRequest {
	t.Helper()
	shipped, err := listUnrecordedShipped(ctx, pool, repo, timelineProduction)
	if err != nil {
		t.Fatalf("listUnrecordedShipped(%s): %v", repo, err)
	}
	got := []string{}
	for _, pr := range shipped {
		got = append(got, fmt.Sprintf("#%d %s", pr.Number, pr.IssueKey))
	}
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("listUnrecordedShipped(%s) lists %v, want %v", repo, got, want)
	}
	return shipped
}

// seedShippingRun stores deploy run runID of repo on main. It starts at timelineRunCreated, and its
// production job succeeds at timelineShippedAt. So it ships each timeline row of these tests.
func seedShippingRun(t *testing.T, ctx context.Context, pool *store.Pool, repo string, runID int64) {
	t.Helper()
	created, done := timelineRunCreated, timelineShippedAt
	if err := UpsertRun(ctx, pool, DeliveryRun{
		Repo: repo, RunID: runID, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", runID),
		HeadCommitAt: created, StartedAt: created, CreatedAt: &created, CompletedAt: &done,
		URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, runID), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("store the deploy run %d of %s: %v", runID, repo, err)
	}
	success := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, repo, runID, []DeliveryRunJob{{
		Repo: repo, RunID: runID, Name: timelineJob, StartedAt: &created, CompletedAt: &done, Conclusion: &success,
	}}, true); err != nil {
		t.Fatalf("store the jobs of the deploy run %d of %s: %v", runID, repo, err)
	}
}

// timelinePull is GitHub's answer for pull request number of repo, merged at timelineMerged and
// number seconds. It has title, the head branch branch, and its head in repo.
func timelinePull(repo string, number int, title, branch string) map[string]any {
	merged := timelineMerged.Add(time.Duration(number) * time.Second)
	return map[string]any{
		"number": number, "title": title, "body": timelineBody,
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"user":     map[string]any{"login": "octocat"}, "labels": []any{}, "state": "closed", "merged": true,
		"created_at": merged.Add(-24 * time.Hour).Format(time.RFC3339), "updated_at": merged.Add(time.Minute).Format(time.RFC3339),
		"merged_at": merged.Format(time.RFC3339), "merge_commit_sha": fakeSHA("e", number), "additions": 3, "deletions": 1,
		"head": map[string]any{"ref": branch, "sha": fakeSHA("c", number), "repo": map[string]any{"full_name": repo}},
		"base": map[string]any{"ref": "main"},
	}
}

// serveTimelinePull makes the fake GitHub answer GET /repos/{repo}/pulls/{number} with pull. It
// answers each page of the commits with one commit, its message and its time. It answers the count
// of the REST reads of the pull request that the code under test sends. A read that answers a
// batched query of facts is not one of them (restRead).
func serveTimelinePull(t *testing.T, fake *fakeGitHub, repo string, number int, pull map[string]any) *atomic.Int32 {
	t.Helper()
	reads := &atomic.Int32{}
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	fake.handle("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		if restRead(r) {
			reads.Add(1)
		}
		mustEncode(t, w, pull)
	})
	committed := timelineMerged.Add(-2 * time.Hour).Format(time.RFC3339)
	fake.handle("GET "+path+"/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{"sha": fakeSHA("d", number), "commit": map[string]any{
			"message": "Work on the change.", "author": map[string]any{"date": committed},
		}}})
	})
	return reads
}

// The timeline and the results name one issue for a pull request with facts: the statement's,
// whatever the timeline row stores (state S8). #11's branch names ATTR-11, and its timeline row
// stores ATTR-19. The head of #12 is in a fork, so its facts credit nobody. Its timeline row stores
// ATTR-12, the issue of its branch. A timeline that gives the stored issue fails.
func TestTheTimelineAndTheResultsNameOneIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineRepo, "ATTR-11", "ATTR-12", "ATTR-19")
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 11, branch: "legion/ATTR-11", title: "the change",
		texts: readTexts(""), timeline: &statementTimeline{issue: new("ATTR-19")}})
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 12, branch: "legion/ATTR-12", title: "the change",
		texts: &statementTexts{messages: []string{}, headRepo: new("fork/timeline")}, timeline: &statementTimeline{issue: new("ATTR-12")}})
	for _, number := range []int{11, 12} {
		mergeTimelineRow(t, ctx, pool, timelineRepo, number)
	}
	wantAnswer(t, ctx, pool, timelineRepo, 11, "ATTR-11 from branch")
	wantTimeline(t, ctx, pool, timelineRepo, 11, "ATTR-11")
	wantListed(t, ctx, pool, "ATTR-11", "#11")
	wantListed(t, ctx, pool, "ATTR-19")
	wantAnswer(t, ctx, pool, timelineRepo, 12, "nobody from none")
	wantTimeline(t, ctx, pool, timelineRepo, 12, "nobody")
	wantListed(t, ctx, pool, "ATTR-12")
}

// A pull request with no facts gets the issue that its results row stores from a read of GitHub
// (R4, states S5 and S6). The timeline and the results both give it. The results row of #21 stores
// ATTR-2, and its timeline row stores ATTR-1. #25 is a pull request from a fork. Its results row
// stores nobody, and its timeline row stores ATTR-5. A timeline that gives the stored issue fails.
func TestTheTimelineAndTheResultsNameOneIssueForAPullRequestWithNoFacts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineRepo, "ATTR-1", "ATTR-2", "ATTR-5")
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 21, branch: "work/none", title: "the change",
		stored: new("ATTR-2"), timeline: &statementTimeline{issue: new("ATTR-1")}})
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 25, branch: "work/none", title: "the change",
		timeline: &statementTimeline{issue: new("ATTR-5")}})
	for _, number := range []int{21, 25} {
		mergeTimelineRow(t, ctx, pool, timelineRepo, number)
	}
	wantAnswer(t, ctx, pool, timelineRepo, 21, "ATTR-2 from none, no facts")
	wantTimeline(t, ctx, pool, timelineRepo, 21, "ATTR-2")
	wantListed(t, ctx, pool, "ATTR-2", "#21")
	wantListed(t, ctx, pool, "ATTR-1")
	wantAnswer(t, ctx, pool, timelineRepo, 25, "nobody from none, no facts")
	wantTimeline(t, ctx, pool, timelineRepo, 25, "nobody")
	wantListed(t, ctx, pool, "ATTR-5")
}

// The timeline finds the results row of a pull request under the canonical name of its repository
// (state S14). acme/timeline-old has the new name acme/timeline-new. The timeline row of #31 is
// under the old name and stores ATTR-39. Its results row and its texts are under the new name, and
// its branch names ATTR-31. A timeline that gives the stored issue fails. Control in the same test:
// #32 has a timeline row alone, under the old name. It gives its stored ATTR-32.
func TestTheTimelineFindsItsResultsRowUnderTheCanonicalName(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineNewRepo, "ATTR-31", "ATTR-32", "ATTR-39")
	mapRepository(t, ctx, pool, timelineOldRepo, "ATTR", false, new(timelineNewRepo))
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineNewRepo, number: 31, branch: "legion/ATTR-31", title: "the change",
		texts: readTexts("")})
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineOldRepo, number: 31, title: "the change", noHead: true,
		timeline: &statementTimeline{issue: new("ATTR-39")}})
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineOldRepo, number: 32, title: "the change", noHead: true,
		timeline: &statementTimeline{issue: new("ATTR-32")}})
	for _, number := range []int{31, 32} {
		mergeTimelineRow(t, ctx, pool, timelineOldRepo, number)
	}
	wantAnswer(t, ctx, pool, timelineOldRepo, 31, "ATTR-31 from branch")
	wantAnswer(t, ctx, pool, timelineNewRepo, 31, "ATTR-31 from branch")
	wantTimeline(t, ctx, pool, timelineOldRepo, 31, "ATTR-31")
	wantListed(t, ctx, pool, "ATTR-31", "#31")
	wantListed(t, ctx, pool, "ATTR-39")
	wantTimeline(t, ctx, pool, timelineOldRepo, 32, "ATTR-32")
	wantListed(t, ctx, pool, "ATTR-32", "#32?")
}

// A rename after both rows exist keeps one issue for every reader (B1). The results row, the texts
// and the timeline row of acme/timeline-old#5 are under that name. Its branch names ACME-5, and its
// timeline row stores ACME-4. Then acme/timeline-old gets the new name acme/timeline-new. Want
// ACME-5 from the four readers by issue, from PullRequestIssues of either name, and from the three
// readers of the timeline. One reconcile pass reads nothing of the fake GitHub, and the readers give
// the same answers after it. Then recordDeployedEvent records the event on ACME-5 alone.
func TestARenameAfterBothRowsExistKeepsOneIssueForEveryReader(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineOldRepo, "ACME-4", "ACME-5")
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineOldRepo, number: 5, branch: "legion/ACME-5", title: "the change",
		texts: readTexts(""), timeline: &statementTimeline{issue: new("ACME-4")}})
	mergeTimelineRow(t, ctx, pool, timelineOldRepo, 5)
	seedShippingRun(t, ctx, pool, timelineOldRepo, 905)
	execDispatchChange(t, ctx, pool, "give "+timelineOldRepo+" the new name "+timelineNewRepo,
		`update repo_projects set renamed_to = $2 where repo = $1`, timelineOldRepo, timelineNewRepo)
	var shipped []shippedPullRequest
	readers := func(t *testing.T) {
		wantReaders(t, ctx, pool, "ACME-5", headOf(5), `at head true, newest acme/timeline-new#5, base "main"`)
		wantListed(t, ctx, pool, "ACME-5", "#5")
		wantListed(t, ctx, pool, "ACME-4")
		wantAnswer(t, ctx, pool, timelineOldRepo, 5, "ACME-5 from branch")
		wantAnswer(t, ctx, pool, timelineNewRepo, 5, "ACME-5 from branch")
		wantTimeline(t, ctx, pool, timelineOldRepo, 5, "ACME-5")
		wantWaiting(t, ctx, pool, timelineOldRepo, 5, "ACME-5")
		shipped = wantShipped(t, ctx, pool, timelineOldRepo, "#5 ACME-5")
	}
	t.Run("before_the_pass", readers)

	fake := newFakeGitHub(t)
	served := map[string]*githubPulls{
		timelineOldRepo: serveGitHubPulls(t, fake, timelineOldRepo),
		timelineNewRepo: serveGitHubPulls(t, fake, timelineNewRepo),
	}
	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)
	var reads []string
	for _, repo := range []string{timelineOldRepo, timelineNewRepo} {
		g := served[repo]
		if n := g.reads.Load() + g.commitReads.Load() + g.repoReads.Load() + g.factsReads.Load() + g.absenceQueries.Load(); n != 0 {
			reads = append(reads, fmt.Sprintf("%d of %s", n, repo))
		}
	}
	if queries := fake.factsQueries.Load(); queries != 0 || len(reads) != 0 {
		t.Errorf("the pass sent %d batched queries of facts and made the reads %v, want no read: the pull request has current texts and read messages",
			queries, reads)
	}
	t.Run("after_the_pass", readers)

	if len(shipped) != 1 {
		t.Fatalf("listUnrecordedShipped(%s) lists %d pull requests after the pass, want #5 alone", timelineOldRepo, len(shipped))
	}
	recorded, err := recordDeployedEvent(ctx, pool, events.NewBroker(), shipped[0])
	if err != nil {
		t.Fatalf("recordDeployedEvent(%+v): %v", shipped[0], err)
	}
	if !recorded {
		t.Errorf("recordDeployedEvent(%+v) recorded no event, want the event of #5 on ACME-5", shipped[0])
	}
	if found := deployedEvents(t, ctx, pool, "ACME-4"); len(found) != 0 {
		t.Errorf("ACME-4, the issue that the timeline row stores, carries %d issue.deployed events, want none", len(found))
	}
	requireDeployedEvent(t, ctx, pool, "ACME-5", timelineOldRepo, 5, 905, timelineShippedAt.Format(time.RFC3339))
}

// A completed merge writes no issue on the timeline row. The completion of #41 and of #42 reads a
// title, a branch and a message that name ATTR-41. #41 has no timeline row before it, and its new
// row stores no issue. The timeline row of #42 stores ATTR-42, and the completion keeps it. A
// completion that resolves an issue writes ATTR-41 on both rows.
func TestACompletedMergeWritesNoIssueOnTheTimelineRow(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineRepo, "ATTR-41", "ATTR-42")
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 42, title: "the earlier change", noHead: true,
		timeline: &statementTimeline{issue: new("ATTR-42")}})
	mergeTimelineRow(t, ctx, pool, timelineRepo, 42)
	for _, number := range []int{41, 42} {
		merged := timelineMerged.Add(time.Duration(number) * time.Second)
		created := merged.Add(-time.Hour)
		fetched := FetchedPullRequest{
			Number: number, Title: "ATTR-41: the work", URL: fmt.Sprintf("https://github.com/%s/pull/%d", timelineRepo, number),
			Author: "octocat", CreatedAt: created, MergedAt: &merged, Body: "Dispatch: ATTR-41", HeadRef: "legion/ATTR-41",
			FirstCommitAt: &created,
		}
		if err := completePullRequest(ctx, pool, nil, "acme", "timeline", timelineRepo, number, fetched,
			[]string{"Work on the change.\n\nDispatch: ATTR-41"}); err != nil {
			t.Fatalf("complete %s#%d: %v", timelineRepo, number, err)
		}
	}
	if stored := storedTimelineIssue(t, ctx, pool, timelineRepo, 41); stored != nil {
		t.Errorf("the completion of %s#41 stored %s on its new timeline row, want no issue", timelineRepo, *stored)
	}
	if stored := storedTimelineIssue(t, ctx, pool, timelineRepo, 42); stored == nil || *stored != "ATTR-42" {
		t.Errorf("the completion of %s#42 left %v on its timeline row, want ATTR-42, the issue that the row stored before it",
			timelineRepo, storedOrNobody(stored))
	}
}

// storedOrNobody is issue as a failure names it: its key, or "nobody" for nil.
func storedOrNobody(issue *string) string {
	if issue == nil {
		return "nobody"
	}
	return *issue
}

// The completion of a partial row reads GitHub once, and stores the results row and the texts from
// that read. #51 is a partial timeline row with no results row. The reconcile's step for partial
// rows completes it. Want one GET of pulls/51, the results row and the texts of that answer, and
// the timeline row complete with the same title. A completion that stores no results row fails. A
// completion that reads the pull request a second time fails.
func TestAPartialRowsCompletionWritesTheResultsRowFromTheSameRead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineRepo, "ATTR-51")
	created := timelineMerged.Add(-24 * time.Hour)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: timelineRepo, Number: 51, Title: "the title of the search", URL: fmt.Sprintf("https://github.com/%s/pull/51", timelineRepo),
		Author: "octocat", CreatedAt: &created, Partial: true,
	}); err != nil {
		t.Fatalf("store the partial row of %s#51: %v", timelineRepo, err)
	}
	const title = "ATTR-51: the change"
	fake := newFakeGitHub(t)
	reads := serveTimelinePull(t, fake, timelineRepo, 51, timelinePull(timelineRepo, 51, title, "legion/ATTR-51"))
	if err := NewReconcile(pool, fake.newTestClient(), events.NewBroker()).reconcilePartialPullRequests(ctx); err != nil {
		t.Fatalf("reconcilePartialPullRequests: %v", err)
	}

	if n := reads.Load(); n != 1 {
		t.Errorf("the completion sent GET pulls/51 %d times, want 1: one read stores the results row, the texts and the timeline row", n)
	}
	if rows := headRowsOf(t, ctx, pool, timelineRepo, 51); rows != 1 {
		t.Fatalf("after the completion %s#51 has %d results rows, want 1, written from the completion's read of GitHub", timelineRepo, rows)
	}
	if head := readHead(t, ctx, pool, timelineRepo, 51); head.HeadSHA != fakeSHA("c", 51) || head.HeadRef != "legion/ATTR-51" || head.State != "merged" {
		t.Errorf("the results row of #51 holds the head %q on %q in the state %q, want GitHub's %q on legion/ATTR-51, merged",
			head.HeadSHA, head.HeadRef, head.State, fakeSHA("c", 51))
	}
	if texts, found := storedTextsOf(t, ctx, pool, timelineRepo, 51); !found || texts.headSHA != fakeSHA("c", 51) || texts.title != title || texts.body != timelineBody {
		t.Errorf("the texts of #51 = %+v (found %v), want GitHub's head %q, title %q and body %q", texts, found, fakeSHA("c", 51), title, timelineBody)
	}
	timeline, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`,
		timelineRepo, 51))
	if err != nil {
		t.Fatalf("read the timeline row of %s#51: %v", timelineRepo, err)
	}
	if timeline.Partial || timeline.Title != title || timeline.FirstCommitAt == nil {
		t.Errorf("the timeline row of #51 has partial %v, the title %q and the first commit %v, want complete with %q and a first commit",
			timeline.Partial, timeline.Title, timeline.FirstCommitAt, title)
	}
	wantTimeline(t, ctx, pool, timelineRepo, 51, "ATTR-51")
}

// issue.deployed names the issue that the statement gives the pull request, not the issue that its
// timeline row stores. The branch of #61 names ATTR-61, and the texts of #62 name no issue. The
// timeline row of each stores ATTR-69. One run ships both. Want one event, on ATTR-61, and none on
// ATTR-69. An event that names the stored issue fails.
func TestIssueDeployedNamesTheAttributedIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	projectIssues(t, ctx, pool, timelineRepo, "ATTR-61", "ATTR-69")
	stored := &statementTimeline{issue: new("ATTR-69")}
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 61, branch: "legion/ATTR-61", title: "the change",
		texts: readTexts(""), timeline: stored})
	storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 62, branch: "work/none", title: "the change",
		texts: readTexts(""), timeline: stored})
	for _, number := range []int{61, 62} {
		mergeTimelineRow(t, ctx, pool, timelineRepo, number)
	}
	seedShippingRun(t, ctx, pool, timelineRepo, 961)

	written, err := RecordDeployedEvents(ctx, pool, DeliverySettings{DeployRepo: timelineRepo, ProductionJobNames: []string{timelineJob}},
		events.NewBroker())
	if err != nil {
		t.Fatalf("RecordDeployedEvents: %v", err)
	}
	if found := deployedEvents(t, ctx, pool, "ATTR-69"); len(found) != 0 {
		t.Errorf("ATTR-69, the issue that both timeline rows store, carries %d issue.deployed events, want none", len(found))
	}
	if at := deployedEventAt(t, ctx, pool, timelineRepo, 62); at != nil {
		t.Errorf("#62 belongs to nobody, and its marker is set at %v, want no marker", at)
	}
	if written != 1 {
		t.Errorf("RecordDeployedEvents wrote %d events, want 1, the event of #61 on ATTR-61", written)
	}
	requireDeployedEvent(t, ctx, pool, "ATTR-61", timelineRepo, 61, 961, timelineShippedAt.Format(time.RFC3339))
}

// The timeline marks a pull request whose issue is not known (revision 20, state S13). The
// timeline row of #40 stores ATTR-49. Its results row has current texts that name no key, and its
// messages are not read. Want the timeline to give #40 no issue with IssueNotKnown true, and no
// issue.deployed for #40. A timeline that gives the stored issue fails. Control in the same test:
// with the messages read and empty, #40 belongs to nobody, and IssueNotKnown is false.
func TestTheTimelineMarksAPullRequestWhoseIssueIsNotKnown(t *testing.T) {
	for _, read := range []bool{false, true} {
		name := map[bool]string{false: "messages_not_read", true: "control_messages_read"}[read]
		t.Run(name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			projectIssues(t, ctx, pool, timelineRepo, "ATTR-49")
			texts, want := &statementTexts{}, "nobody, not known"
			if read {
				texts, want = readTexts(""), "nobody"
			}
			storeStatementRow(t, ctx, pool, statementRow{repo: timelineRepo, number: 40, branch: "work/none", title: "no key",
				texts: texts, timeline: &statementTimeline{issue: new("ATTR-49")}})
			mergeTimelineRow(t, ctx, pool, timelineRepo, 40)
			seedShippingRun(t, ctx, pool, timelineRepo, 940)

			wantTimeline(t, ctx, pool, timelineRepo, 40, want)
			wantShipped(t, ctx, pool, timelineRepo)
			stale := shippedPullRequest{Repo: timelineRepo, Number: 40, IssueKey: "ATTR-49", RunID: 940, CompletedAt: timelineShippedAt}
			recorded, err := recordDeployedEvent(ctx, pool, events.NewBroker(), stale)
			if err != nil {
				t.Fatalf("recordDeployedEvent(%+v): %v", stale, err)
			}
			if recorded {
				t.Errorf("recordDeployedEvent(%+v) recorded the event on ATTR-49, the stored issue, want no event: the statement gives #40 no issue", stale)
			}
			if found := deployedEvents(t, ctx, pool, "ATTR-49"); len(found) != 0 {
				t.Errorf("ATTR-49 carries %d issue.deployed events, want none", len(found))
			}
			if at := deployedEventAt(t, ctx, pool, timelineRepo, 40); at != nil {
				t.Errorf("the marker of #40 is set at %v, want no marker", at)
			}
		})
	}
}

// Every write of the timeline stores a NUL as U+FFFD (BL2, review 5, note 2). The delivery settings
// exist. The merged search finds #91, whose title holds a NUL (reconcilePullRequest). #92 is a
// partial timeline row, and the title that GitHub gives it holds a NUL. One reconcile pass
// completes both. Want U+FFFD in each title, a pass with no failure, and the search's progress for
// the installation. Then a write of the session s1, a NUL and x stores U+FFFD in the session.
// Control in the same test: a raw insert of a title with its NUL fails with SQLSTATE 22021.
func TestEveryTimelineWriteStoresANulAsUFFFD(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	const searched, completed = "Fix:\x00 the notes", "Docs:\x00 the guide"
	created := timelineMerged.Add(-24 * time.Hour)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: timelineRepo, Number: 92, Title: "Docs: the guide", URL: fmt.Sprintf("https://github.com/%s/pull/92", timelineRepo),
		Author: "octocat", CreatedAt: &created, Partial: true,
	}); err != nil {
		t.Fatalf("store the partial row of %s#92: %v", timelineRepo, err)
	}
	fake := newFakeGitHub(t)
	fake.installationRepos = []string{"acme/widgets", timelineRepo}
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if query, _ := decodeGraphQLRequest(t, r); !strings.Contains(query, "search(") {
			t.Errorf("POST /graphql asked %q, and this fake answers the merged search alone", query)
		}
		node := searchNodeJSON(91, searched, "octocat", created.Format(time.RFC3339), timelineMerged.Add(91*time.Second).Format(time.RFC3339))
		node["url"] = fmt.Sprintf("https://github.com/%s/pull/91", timelineRepo)
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	serveTimelinePull(t, fake, timelineRepo, 91, timelinePull(timelineRepo, 91, searched, "work/notes"))
	serveTimelinePull(t, fake, timelineRepo, 92, timelinePull(timelineRepo, 92, completed, "work/guide"))
	noRuns(t, fake)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if after.LastError != nil || after.LastReconcileAt == nil {
		lastError := "none"
		if after.LastError != nil {
			lastError = *after.LastError
		}
		t.Errorf("the pass recorded last_error %q and last_reconcile_at %v, want a pass with no failure", lastError, after.LastReconcileAt)
	}
	through, err := ReconcileProgressThrough(ctx, pool, mergedPullRequestsStep(fake.installID), searchProgressScope(settings))
	if err != nil {
		t.Fatalf("ReconcileProgressThrough: %v", err)
	}
	if through.IsZero() {
		t.Errorf("the merged search recorded no progress for installation %d, want the window it searched", fake.installID)
	}
	for _, row := range []struct {
		number int
		title  string
	}{{91, "Fix:\uFFFD the notes"}, {92, "Docs:\uFFFD the guide"}} {
		var title string
		var partial bool
		err := pool.QueryRow(ctx, `select title, partial from delivery_pull_requests where repo = $1 and number = $2`,
			timelineRepo, row.number).Scan(&title, &partial)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			t.Errorf("the pass stored no timeline row of %s#%d, want one with the title %q", timelineRepo, row.number, row.title)
		case err != nil:
			t.Fatalf("read the timeline row of %s#%d: %v", timelineRepo, row.number, err)
		case title != row.title || partial:
			t.Errorf("the timeline row of %s#%d has the title %q and partial %v, want %q, complete", timelineRepo, row.number, title, partial, row.title)
		}
	}

	const session = "s1\x00x"
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: timelineRepo, Number: 93, Title: "the change", URL: fmt.Sprintf("https://github.com/%s/pull/93", timelineRepo),
		Author: "octocat", Sessions: []string{session},
	}); err != nil {
		t.Fatalf("UpsertPullRequest of %s#93 with the session %q: %v", timelineRepo, session, err)
	}
	var sessions []string
	if err := pool.QueryRow(ctx, `select sessions from delivery_pull_requests where repo = $1 and number = 93`, timelineRepo).Scan(&sessions); err != nil {
		t.Fatalf("read the sessions of %s#93: %v", timelineRepo, err)
	}
	if want := []string{"s1\uFFFDx"}; !slices.Equal(sessions, want) {
		t.Errorf("the sessions of %s#93 are %q, want %q", timelineRepo, sessions, want)
	}

	_, err = pool.Exec(ctx, `insert into delivery_pull_requests (repo, number, title, url, author) values ($1, 94, $2, $3, 'octocat')`,
		timelineRepo, searched, fmt.Sprintf("https://github.com/%s/pull/94", timelineRepo))
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "22021" {
		t.Errorf("a raw insert of the title %q answered %v, want SQLSTATE 22021 (the control)", searched, err)
	}
}
