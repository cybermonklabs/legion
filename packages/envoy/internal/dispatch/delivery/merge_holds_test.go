package delivery

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The commits these tests name: a merge commit of each pull request and its final head, 40 hex
// each, so every one is a commit a row can hold.
const (
	mergeOfFive  = "5555555555555555555555555555555555555555"
	mergeOfSix   = "6666666666666666666666666666666666666666"
	mergeOfSeven = "7777777777777777777777777777777777777777"
	headOfFive   = "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5"
	headOfSix    = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"
	headOfSeven  = "f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7"
)

// mergeTestTime reads an RFC 3339 time a test writes as a literal.
func mergeTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("read the time %q: %v", value, err)
	}
	return at
}

// storedMerge is what a row holds of its merge, as a test reads it: the title too, so a test can
// tell that a later write applied at all, and the rule the row carries (attribution_rule).
type storedMerge struct {
	title          string
	mergeCommitSHA string
	mergedAt       *time.Time
	rule           int
}

func readMerge(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) storedMerge {
	t.Helper()
	var merge storedMerge
	if err := pool.QueryRow(ctx, `
		select title, merge_commit_sha, merged_at, attribution_rule
		from pull_request_heads where repo = $1 and number = $2
	`, repo, number).Scan(&merge.title, &merge.mergeCommitSHA, &merge.mergedAt, &merge.rule); err != nil {
		t.Fatalf("read the merge of %s#%d: %v", repo, number, err)
	}
	return merge
}

// numbersOf is the pull request numbers of merged, in its order, which is what a failure names.
func numbersOf(merged []MergedPullRequest) []int {
	numbers := make([]int, len(merged))
	for i, pull := range merged {
		numbers[i] = pull.Number
	}
	return numbers
}

// mergeHead is one pull request row a test stores, with the texts of a read of GitHub that read
// every source: the statement credits it to the issue its head branch names (attributedTerms), as
// a writer's read leaves it. An empty issue is a row the statement credits to nobody.
func mergeHead(repo string, number int, issue, head, headRef, base, state string) PullRequestHead {
	title := "Change " + headRef
	if issue != "" {
		title = issue + " the work"
	}
	return PullRequestHead{
		Repo: repo, Number: number, HeadSHA: head, HeadRef: headRef, BaseRef: base, State: state,
		Title: title, URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(number),
		Texts: &PullRequestTexts{HeadRepo: repo, BaseRef: base, Title: title, CommitMessages: []string{}, MessagesRead: true},
	}
}

// A merged head keeps the commit GitHub merged it as and the time it merged, and a later write
// of the same pull request that carries neither, such as an edit delivered by a listener of the
// release before this one, keeps both. An open pull request's answer names a test merge commit,
// which is no merge, so its row stores none.
func TestAMergedHeadRowKeepsItsMergeCommitAndItsTime(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 54)
	mergedAt := mergeTestTime(t, "2026-03-01T10:00:00Z")
	merged := mergeHead("acme/widgets", 5, issue, headOfFive, "legion/"+issue, "main", "merged")
	merged.EventAt, merged.MergeCommitSHA, merged.MergedAt = &mergedAt, mergeOfFive, &mergedAt
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, merged); err != nil {
		t.Fatalf("store the merged head: %v", err)
	}
	if got := readMerge(t, ctx, pool, "acme/widgets", 5); got.mergeCommitSHA != mergeOfFive || !sameTime(got.mergedAt, mergedAt) {
		t.Fatalf("#5 after its merge = %+v, want merge commit %s merged at %s", got, mergeOfFive, mergedAt)
	}

	edited := mergeTestTime(t, "2026-03-02T10:00:00Z")
	later := merged
	later.Title, later.EventAt, later.MergeCommitSHA, later.MergedAt = "HEAD-54 the work, renamed", &edited, "", nil
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, later); err != nil {
		t.Fatalf("store the later write: %v", err)
	}
	got := readMerge(t, ctx, pool, "acme/widgets", 5)
	if got.title != later.Title {
		t.Fatalf("#5's title = %q, want %q: the later write did not apply, so it proves nothing about what it keeps", got.title, later.Title)
	}
	if got.mergeCommitSHA != mergeOfFive || !sameTime(got.mergedAt, mergedAt) {
		t.Fatalf("#5 after a write that carries no merge = %+v, want merge commit %s merged at %s kept", got, mergeOfFive, mergedAt)
	}

	open := mergeHead("acme/widgets", 6, issue, headOfSix, "legion/"+issue, "main", "open")
	open.EventAt, open.MergeCommitSHA = &mergedAt, mergeOfSix
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, open); err != nil {
		t.Fatalf("store the open head: %v", err)
	}
	if got := readMerge(t, ctx, pool, "acme/widgets", 6); got.mergeCommitSHA != "" || got.mergedAt != nil {
		t.Fatalf("the open #6 = %+v, want no merge commit and no merge time: GitHub's test merge is no merge", got)
	}
}

// The newest merge of an issue is the pull request that merged last, not the one whose newest
// event came last: an edit of a pull request merged earlier moves its event_at past a later
// merge. #6 merges first, #5 after it, and #6 is then edited, so the order by event_at and the
// order by number both answer #6, and only the time of each merge answers #5. Both writers carry
// that time: a head a reader of GitHub stores, and the intake's own read of the pull request an
// envelope names, which is where this build's intake takes every fact from.
func TestTheNewestMergeIsTheLastToMergeNotTheLastEdited(t *testing.T) {
	t.Parallel()
	first, second, edited := "2026-03-01T10:00:00Z", "2026-03-02T10:00:00Z", "2026-03-03T10:00:00Z"
	check := func(t *testing.T, ctx context.Context, pool *store.Pool, issue string) {
		t.Helper()
		merged, err := unrenamed().MergeOfIssue(ctx, pool, issue, HeadAnswers{})
		if err != nil {
			t.Fatalf("MergeOfIssue(%s): %v", issue, err)
		}
		if len(merged) != 2 || merged[0].Number != 5 || merged[1].Number != 6 {
			t.Fatalf("MergeOfIssue(%s) = %v, want #5 then #6: #5 merged last", issue, numbersOf(merged))
		}
		if merged[0].MergeCommitSHA != mergeOfFive || merged[0].BaseRef != "release" || merged[0].HeadSHA != headOfFive ||
			!sameTime(merged[0].MergedAt, mergeTestTime(t, second)) {
			t.Fatalf("MergeOfIssue(%s)[0] = %+v, want #5's merge commit, base, final head and time", issue, merged[0])
		}
	}

	t.Run("written as heads", func(t *testing.T) {
		t.Parallel()
		pool, ctx := deliveryTestPool(t)
		issue := seedHeadIssue(t, ctx, pool, 50)
		write := func(number int, head, base, mergeCommit, mergedAt, eventAt string) {
			t.Helper()
			merged, event := mergeTestTime(t, mergedAt), mergeTestTime(t, eventAt)
			row := mergeHead("acme/widgets", number, issue, head, "legion/"+issue, base, "merged")
			row.EventAt, row.MergeCommitSHA, row.MergedAt = &event, mergeCommit, &merged
			if err := unrenamed().UpsertPullRequestHead(ctx, pool, row); err != nil {
				t.Fatalf("store #%d: %v", number, err)
			}
		}
		write(6, headOfSix, "main", mergeOfSix, first, first)
		write(5, headOfFive, "release", mergeOfFive, second, second)
		write(6, headOfSix, "main", mergeOfSix, first, edited)
		check(t, ctx, pool, issue)
	})

	t.Run("delivered as envelopes", func(t *testing.T) {
		t.Parallel()
		pool, ctx := deliveryTestPool(t)
		issue := seedHeadIssue(t, ctx, pool, 51)
		fake := newFakeGitHub(t)
		github := serveGitHubPulls(t, fake, "acme/widgets")
		intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
		deliver := func(number int, head, base, mergeCommit, mergedAt, updatedAt string) {
			t.Helper()
			moment := mergeTestTime(t, updatedAt)
			github.set("acme/widgets", number, "closed", head, "legion/"+issue, "", moment)
			github.mu.Lock()
			github.pulls[number]["merged"] = true
			github.pulls[number]["merged_at"] = mergedAt
			github.pulls[number]["merge_commit_sha"] = mergeCommit
			github.pulls[number]["base"] = map[string]any{"ref": base}
			github.pulls[number]["title"] = issue + " the work"
			github.mu.Unlock()
			payload := map[string]string{
				"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
				"number": strconv.Itoa(number), "title": issue + " the work", "body": "",
				"head_ref": "legion/" + issue, "head_repo": "acme/widgets", "head_sha": head,
				"base_ref": base, "url": "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
				"updated_at": updatedAt,
			}
			if _, err := intake.recordPullRequestHead(ctx, payload, number); err != nil {
				t.Fatalf("record #%d from its envelope: %v", number, err)
			}
		}
		deliver(6, headOfSix, "main", mergeOfSix, first, first)
		deliver(5, headOfFive, "release", mergeOfFive, second, second)
		deliver(6, headOfSix, "main", mergeOfSix, first, edited)
		check(t, ctx, pool, issue)
	})
}

// A merged row stored with no merge time is read again by the results read that must order it
// (RefreshMergedPullRequest), not by the reconcile: this build's reconcile offers no merged row
// at all (ListStalePullRequestHeads), since the facts line reads every row whose texts are not
// current and a merged row's facts never change again. The refresh stores the merge commit and
// the time GitHub answers; a row whose pull request GitHub no longer answers is stamped read and
// keeps no time.
func TestAMergedRowWithNoMergeTimeIsRefreshedByTheResultsReadNotTheReconcile(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 52)
	mergedAt := mergeTestTime(t, "2026-03-01T10:00:00Z")
	stored := mergeTestTime(t, "2026-03-01T09:00:00Z")
	for _, number := range []int{31, 32} {
		row := mergeHead("acme/widgets", number, issue, headOfFive, "legion/"+issue, "main", "merged")
		row.EventAt, row.CheckedAt = &stored, &stored
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, row); err != nil {
			t.Fatalf("store #%d: %v", number, err)
		}
		// The rule the release before this one stamped every row it read with.
		if _, err := pool.Exec(ctx, `
			update pull_request_heads set attribution_rule = 2 where repo = 'acme/widgets' and number = $1
		`, number); err != nil {
			t.Fatalf("make #%d a row of rule 2: %v", number, err)
		}
	}

	stale, err := unrenamed().ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	for _, ref := range stale {
		if ref.Number == 31 || ref.Number == 32 {
			t.Fatalf("the reconcile offers %+v, want no merged row: its pass reads the open rows and the facts line", stale)
		}
	}

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/31", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 31, "title": issue + " the work", "body": "", "html_url": "https://github.com/acme/widgets/pull/31",
			"state": "closed", "merged": true, "merged_at": "2026-03-01T10:00:00Z", "merge_commit_sha": mergeOfFive,
			"updated_at": "2026-03-01T10:00:00Z",
			"head":       map[string]any{"ref": "legion/" + issue, "sha": headOfFive, "repo": map[string]any{"full_name": "acme/widgets"}},
			"base":       map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/32", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		mustEncode(t, w, map[string]any{"message": "Not Found"})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})
	client := fake.newTestClient()
	now := time.Now()
	for _, number := range []int{31, 32} {
		if err := unrenamed().RefreshMergedPullRequest(ctx, pool, client, nil, "acme/widgets", number, now); err != nil {
			t.Fatalf("refresh #%d: %v", number, err)
		}
	}

	if got := readMerge(t, ctx, pool, "acme/widgets", 31); got.mergeCommitSHA != mergeOfFive || !sameTime(got.mergedAt, mergedAt) {
		t.Fatalf("#31 after its refresh = %+v, want merge commit %s merged at %s", got, mergeOfFive, mergedAt)
	}
	if got := readMerge(t, ctx, pool, "acme/widgets", 32); got.mergedAt != nil {
		t.Fatalf("#32, which GitHub answers 404 for, = %+v, want no merge time", got)
	}
	if got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", 32); got.CheckedAt == nil || !got.CheckedAt.After(stored) {
		t.Fatalf("#32 after a refusal = %+v, want stamped read at the refresh's own moment", got)
	}
}

// Whether a commit holds a merge is read from GitHub once and kept for good, a no as a yes,
// since two commits never change. GitHub's compare of the commit with the merge commit answers
// it: `behind` is a yes, `diverged` a no. A read that fails tells nothing and is kept nowhere,
// so the next asks again. The answer is kept under the lowercased repository, as branch_commits
// keeps its own, so a caller that spells the repository with capitals reads it too.
func TestACommitHoldsAMergeIsReadOnceAndKeptForGood(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	const (
		merge  = "abababababababababababababababababababab"
		before = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
		after  = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
		unread = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	)
	reads := map[string]*atomic.Int32{after: {}, before: {}, unread: {}}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/compare/{comparison}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("comparison") {
		case after + "..." + merge:
			reads[after].Add(1)
			mustEncode(t, w, map[string]any{"status": "behind"})
		case before + "..." + merge:
			reads[before].Add(1)
			mustEncode(t, w, map[string]any{"status": "diverged"})
		case unread + "..." + merge:
			reads[unread].Add(1)
			w.WriteHeader(http.StatusBadGateway)
			mustEncode(t, w, map[string]any{"message": "Server Error"})
		default:
			t.Errorf("an unexpected comparison: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := fake.newTestClient()
	branches := &Branches{}
	now := time.Now()

	for _, row := range []struct {
		commit string
		want   HoldAnswer
	}{{after, Holds}, {before, DoesNotHold}, {unread, HoldUnknown}} {
		if got := branches.CommitHolds(ctx, pool, client, "acme/widgets", row.commit, merge, now, false); got != row.want {
			t.Fatalf("CommitHolds(%s, %s) = %v, want %v", row.commit, merge, got, row.want)
		}
	}
	kept := func(commit string) (holds, found bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `
			select holds from commit_holds where repo = 'acme/widgets' and commit_sha = $1 and merge_sha = $2
		`, commit, merge).Scan(&holds)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false
		}
		if err != nil {
			t.Fatalf("read the kept answer of %s: %v", commit, err)
		}
		return holds, true
	}
	if holds, found := kept(after); !found || !holds {
		t.Fatalf("the kept answer of %s = %v, %v; want a yes", after, holds, found)
	}
	if holds, found := kept(before); !found || holds {
		t.Fatalf("the kept answer of %s = %v, %v; want a no", before, holds, found)
	}
	if _, found := kept(unread); found {
		t.Fatalf("a read that failed kept an answer for %s, want none", unread)
	}

	later := now.Add(24 * time.Hour)
	if got := branches.CommitHolds(ctx, pool, client, "Acme/Widgets", after, merge, later, false); got != Holds {
		t.Fatalf("CommitHolds(%s) a day later = %v, want Holds", after, got)
	}
	if got := branches.CommitHolds(ctx, pool, client, "acme/widgets", before, merge, later, false); got != DoesNotHold {
		t.Fatalf("CommitHolds(%s) a day later = %v, want DoesNotHold", before, got)
	}
	if got := branches.CommitHolds(ctx, pool, client, "acme/widgets", unread, merge, later, false); got != HoldUnknown {
		t.Fatalf("CommitHolds(%s) again = %v, want HoldUnknown", unread, got)
	}
	for commit, want := range map[string]int32{after: 1, before: 1, unread: 2} {
		if got := reads[commit].Load(); got != want {
			t.Errorf("GitHub was asked about %s %d times, want %d", commit, got, want)
		}
	}
}

// The landing of a branch on the base it merged into is a merged pull request whose head is
// that branch: LandingOf lists each, newest merge first, with its merge commit and final head,
// whatever spelling of the repository its row carries. A pull request still open from that
// branch, one merged from another branch and one of another repository are none of it.
func TestTheLandingOfABranchIsItsNewestMergedPullRequestWhoseFinalHeadHoldsTheMerge(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	const branch = "legion/ACME-0"
	write := func(repo string, number int, head, headRef, state, mergeCommit, mergedAt, eventAt string) {
		t.Helper()
		event := mergeTestTime(t, eventAt)
		row := mergeHead(repo, number, "", head, headRef, "main", state)
		row.EventAt, row.MergeCommitSHA = &event, mergeCommit
		if mergedAt != "" {
			merged := mergeTestTime(t, mergedAt)
			row.MergedAt = &merged
		}
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, row); err != nil {
			t.Fatalf("store %s#%d: %v", repo, number, err)
		}
	}
	write("Acme/Widgets", 7, headOfSeven, branch, "merged", mergeOfSeven, "2026-03-01T10:00:00Z", "2026-03-01T10:00:00Z")
	write("acme/widgets", 6, headOfSix, branch, "merged", mergeOfSix, "2026-03-03T10:00:00Z", "2026-03-03T10:00:00Z")
	write("acme/widgets", 5, headOfFive, branch, "merged", mergeOfFive, "2026-03-02T10:00:00Z", "2026-03-04T10:00:00Z")
	write("acme/widgets", 8, headOfFive, branch, "open", "", "", "2026-03-05T10:00:00Z")
	write("acme/widgets", 9, headOfFive, "legion/ACME-9", "merged", mergeOfFive, "2026-03-05T10:00:00Z", "2026-03-05T10:00:00Z")
	write("acme/tools", 10, headOfFive, branch, "merged", mergeOfFive, "2026-03-05T10:00:00Z", "2026-03-05T10:00:00Z")

	landed, err := LandingOf(ctx, pool, "acme/widgets", branch, HeadAnswers{})
	if err != nil {
		t.Fatalf("LandingOf(%s): %v", branch, err)
	}
	if got := numbersOf(landed); len(got) != 3 || got[0] != 6 || got[1] != 5 || got[2] != 7 {
		t.Fatalf("LandingOf(%s) = %v, want #6, #5, #7: the order each merged", branch, got)
	}
	for i, want := range []struct{ merge, head string }{{mergeOfSix, headOfSix}, {mergeOfFive, headOfFive}, {mergeOfSeven, headOfSeven}} {
		if landed[i].MergeCommitSHA != want.merge || landed[i].HeadSHA != want.head || landed[i].BaseRef != "main" {
			t.Fatalf("LandingOf(%s)[%d] = %+v, want merge commit %s, final head %s, base main", branch, i, landed[i], want.merge, want.head)
		}
	}
}

// An issue's merge in a repository it links a pull request of is the newest of the merged pull
// requests it links there: MergeIn lists them, newest merge first, with each one's base. Asked of
// the issue's own repository, which no link of it can name, it answers nothing.
func TestTheLinkedMergeIsTheNewestLinkedPullRequestsMerge(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 53)
	for _, url := range []string{"https://github.com/acme/tools/pull/9", "https://github.com/acme/tools/pull/10"} {
		if _, err := pool.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
		`, issue, url); err != nil {
			t.Fatalf("link %s to %s: %v", issue, url, err)
		}
	}
	write := func(number int, head, base, mergeCommit, mergedAt, eventAt string) {
		t.Helper()
		merged, event := mergeTestTime(t, mergedAt), mergeTestTime(t, eventAt)
		row := mergeHead("acme/tools", number, "", head, "legion/"+issue, base, "merged")
		row.EventAt, row.MergeCommitSHA, row.MergedAt = &event, mergeCommit, &merged
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, row); err != nil {
			t.Fatalf("store acme/tools#%d: %v", number, err)
		}
	}
	// #10 merged first and was edited last, so the order by event_at and the order by number
	// both answer #10.
	write(10, headOfSix, "release", mergeOfSix, "2026-03-01T10:00:00Z", "2026-03-03T10:00:00Z")
	write(9, headOfFive, "main", mergeOfFive, "2026-03-02T10:00:00Z", "2026-03-02T10:00:00Z")

	linked, err := unrenamed().MergeIn(ctx, pool, issue, "acme/tools", HeadAnswers{})
	if err != nil {
		t.Fatalf("MergeIn(%s, acme/tools): %v", issue, err)
	}
	if got := numbersOf(linked); len(got) != 2 || got[0] != 9 || got[1] != 10 {
		t.Fatalf("MergeIn(%s, acme/tools) = %v, want #9 then #10: #9 merged last", issue, got)
	}
	if linked[0].MergeCommitSHA != mergeOfFive || linked[0].BaseRef != "main" || linked[0].HeadSHA != headOfFive {
		t.Fatalf("MergeIn(%s, acme/tools)[0] = %+v, want #9's merge commit, base and final head", issue, linked[0])
	}
	own, err := unrenamed().MergeIn(ctx, pool, issue, "acme/widgets", HeadAnswers{})
	if err != nil || len(own) != 0 {
		t.Fatalf("MergeIn(%s, acme/widgets) = %v, %v; want none: it is the issue's own repository", issue, numbersOf(own), err)
	}
}

// One merged pull request is read by its repository, compared without case, and its number, with
// what MergeOfIssue reads of each: the read a results read makes of a row it has just refreshed,
// to learn the merge commit and the time the refresh stored. An open pull request, a number with
// no row and a pull request of another repository are none.
func TestAMergedPullRequestIsReadByItsRepositoryAndNumber(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	mergedAt := mergeTestTime(t, "2026-03-01T10:00:00Z")
	merged := mergeHead("acme/widgets", 5, "", headOfFive, "legion/HEAD-5", "release", "merged")
	merged.EventAt, merged.MergeCommitSHA, merged.MergedAt = &mergedAt, mergeOfFive, &mergedAt
	open := mergeHead("acme/widgets", 6, "", headOfSix, "legion/HEAD-6", "main", "open")
	open.EventAt = &mergedAt
	other := mergeHead("acme/tools", 7, "", headOfSeven, "legion/HEAD-7", "main", "merged")
	other.EventAt, other.MergeCommitSHA, other.MergedAt = &mergedAt, mergeOfSeven, &mergedAt
	for _, row := range []PullRequestHead{merged, open, other} {
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, row); err != nil {
			t.Fatalf("store %s#%d: %v", row.Repo, row.Number, err)
		}
	}
	pull, found, err := MergedPullRequestOf(ctx, pool, "Acme/Widgets", 5, HeadAnswers{})
	if err != nil || !found {
		t.Fatalf("MergedPullRequestOf(Acme/Widgets, 5) = %+v, %v, %v; want acme/widgets#5", pull, found, err)
	}
	if pull.Repo != "acme/widgets" || pull.Number != 5 || pull.BaseRef != "release" || pull.HeadSHA != headOfFive ||
		pull.MergeCommitSHA != mergeOfFive || !sameTime(pull.MergedAt, mergedAt) {
		t.Fatalf("MergedPullRequestOf(Acme/Widgets, 5) = %+v, want #5's base, final head, merge commit and time", pull)
	}
	for _, none := range []struct {
		repo   string
		number int
	}{{"acme/widgets", 6}, {"acme/widgets", 7}, {"acme/tools", 5}} {
		if pull, found, err := MergedPullRequestOf(ctx, pool, none.repo, none.number, HeadAnswers{}); err != nil || found {
			t.Fatalf("MergedPullRequestOf(%s, %d) = %+v, %v, %v; want no merged pull request", none.repo, none.number, pull, found, err)
		}
	}
}
