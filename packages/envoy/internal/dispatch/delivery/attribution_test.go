package delivery

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedAttributionIssues writes one issue per key (ATTR-<n>) in the project that owns
// acme/widgets (seedDeliveryProject), since an attribution holds an issue to the repository the
// pull request is in, and each links[key] URL as that issue's external link. Idempotent, as
// seedDeliveryProject is.
func seedAttributionIssues(t *testing.T, ctx context.Context, pool *store.Pool, keys []string, links map[string][]string) {
	t.Helper()
	seedDeliveryProject(t, ctx, pool)
	for _, key := range keys {
		number, err := strconv.Atoi(strings.TrimPrefix(key, "ATTR-"))
		if err != nil {
			t.Fatalf("issue key %q is not ATTR-<n>", key)
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank)
			values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}', $1)
			on conflict do nothing`, key, deliveryProject, attributionIssueBand+number); err != nil {
			t.Fatalf("seed issue %s: %v", key, err)
		}
		for _, url := range links[key] {
			if _, err := pool.Exec(ctx, `
				insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
				on conflict do nothing`, key, url); err != nil {
				t.Fatalf("seed link %s -> %s: %v", url, key, err)
			}
		}
	}
}

// ruleFacts is one pull request as a test gives it to the rule: its repository and URL, the
// texts of one read of GitHub, and the repository of its head branch. HeadRepo "" is the pull
// request's own repository. CommitMessages nil means that the read did not read the messages.
type ruleFacts struct {
	Repo, URL, Title, Body, HeadRef, HeadRepo string
	CommitMessages                            []string
}

// resolvedKey stores facts as the results row and the texts of a new pull request of facts.Repo
// (storeStatementRow), with facts.URL as its URL. The row takes a number above every stored one,
// so one test can store many sets of facts. resolvedKey answers the issue that PullRequestIssues
// gives, "" for nobody, and the source that named it.
func resolvedKey(t *testing.T, ctx context.Context, pool *store.Pool, facts ruleFacts) (string, attributionSource) {
	t.Helper()
	var number int
	if err := pool.QueryRow(ctx, `select greatest(100000, (select max(number) from pull_request_heads),
		(select max(number) from delivery_pull_requests)) + 1`).Scan(&number); err != nil {
		t.Fatalf("choose the number of %+v: %v", facts, err)
	}
	texts := &statementTexts{body: facts.Body, messages: facts.CommitMessages}
	if facts.HeadRepo != "" {
		texts.headRepo = &facts.HeadRepo
	}
	storeStatementRow(t, ctx, pool, statementRow{
		repo: facts.Repo, number: number, branch: facts.HeadRef, title: facts.Title, url: facts.URL, texts: texts,
	})
	key := PullRequestKey{Repo: facts.Repo, Number: number}
	answers, err := PullRequestIssues(ctx, pool, []PullRequestKey{key})
	if err != nil {
		t.Fatalf("PullRequestIssues(%+v): %v", facts, err)
	}
	answer, found := answers[key]
	if !found {
		t.Fatalf("PullRequestIssues gives no answer for %+v, stored as %s#%d", facts, facts.Repo, number)
	}
	if answer.IssueKey == nil {
		return "", attributionSource(answer.Source)
	}
	return *answer.IssueKey, attributionSource(answer.Source)
}

// TestAttributionTakesAnExternalLinkNamingThePullRequest: source 1, an issue whose external
// link is the pull request's own URL.
func TestAttributionTakesAnExternalLinkNamingThePullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-101"}, map[string][]string{
		"ATTR-101": {"https://github.com/acme/widgets/pull/101"},
	})
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/101", Title: "feat: no key here",
	})
	if key != "ATTR-101" || source != sourceExternalLink {
		t.Fatalf("resolved %q from %q, want ATTR-101 from %q", key, source, sourceExternalLink)
	}
}

// TestAttributionIgnoresAnExternalLinkNamingAnotherPullRequest: a link to pull/103 credits
// nothing to pull/102, though the URLs share a prefix.
func TestAttributionIgnoresAnExternalLinkNamingAnotherPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-103"}, map[string][]string{
		"ATTR-103": {"https://github.com/acme/widgets/pull/1030"},
	})
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/103",
		Body: "Follows https://github.com/acme/widgets/pull/1030.",
	})
	if key != "" {
		t.Fatalf("resolved %q from %q, want no issue: the link names another pull request", key, source)
	}
}

// TestAttributionTakesACitedGitHubIssue: source 2, the first GitHub issue the body cites, a
// full URL or a bare #N in the pull request's own repository, that an issue's external link
// names.
func TestAttributionTakesACitedGitHubIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-111", "ATTR-112", "ATTR-113"}, map[string][]string{
		"ATTR-111": {"https://github.com/acme/widgets/issues/77"},
		"ATTR-112": {"https://github.com/acme/other/issues/5"},
		"ATTR-113": {"https://github.com/acme/widgets/issues/88"},
	})
	for _, tc := range []struct {
		name, body, want string
	}{
		{"bare reference in the same repository", "Fixes #77.", "ATTR-111"},
		{"full URL to another repository", "See https://github.com/acme/other/issues/5 for context.", "ATTR-112"},
		// A citation is the whole path of a URL whose host is github.com: a URL at the very start
		// of the body is one, scheme or none, and a host whose name merely ends in github.com is
		// not.
		{"full URL at the start of the body", "https://github.com/acme/other/issues/5 is the context.", "ATTR-112"},
		{"scheme-less citation at the start of the body", "github.com/acme/other/issues/5 is the context.", "ATTR-112"},
		{"a lookalike host is not github.com", "See https://evilgithub.com/acme/widgets/issues/77 for context.", ""},
		{"a subdomain of github.com is not github.com", "See https://pages.github.com/acme/widgets/issues/77 for context.", ""},
		// www.github.com serves the same pages as github.com, so a reader writing a citation by
		// hand is likelier to type it than any other lookalike. What the parse does with it is
		// pinned here rather than left to be rediscovered: it is a subdomain, and another host.
		{"www.github.com is a subdomain too", "See https://www.github.com/acme/widgets/issues/77 for context.", ""},
		// The same string standing inside another URL is credited through the store as the parser
		// credits it, which is what the default branch does: the authority the issue path hangs
		// off is github.com, and what stands to its left decides nothing.
		{"github.com in another host's path", "See https://example.test/github.com/acme/widgets/issues/77 for context.", "ATTR-111"},
		{"github.com in another host's query", "See https://example.test/r?u=github.com/acme/widgets/issues/77 for context.", "ATTR-111"},
		{"github.com in another host's fragment", "See https://example.test/r#github.com/acme/widgets/issues/77 for context.", "ATTR-111"},
		// Userinfo before the host is a citation, because the host after it is github.com and
		// that is where the reader goes; the host of `github.com@evil.test` is evil.test, which
		// the corpus of the reading holds (testdata/attribution_reading.json).
		{"userinfo before github.com", "See https://user@github.com/acme/widgets/issues/77 for context.", "ATTR-111"},
		// An issue path with no host is the path of no URL at all: the finder matches the path
		// rather than the host, so this is the row that says so.
		{"an issue path with no host", "See /acme/widgets/issues/77 for context.", ""},
		{"first linked citation in body order", "Unlinked #12, then #88, then https://github.com/acme/other/issues/5.", "ATTR-113"},
		{"a pull request URL is not an issue", "Reverts https://github.com/acme/widgets/pull/77.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, source := resolvedKey(t, ctx, pool, ruleFacts{
				Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/110", Body: tc.body,
			})
			if key != tc.want {
				t.Fatalf("resolved %q from %q, want %q", key, source, tc.want)
			}
			if tc.want != "" && source != sourceGitHubIssueLink {
				t.Fatalf("source = %q, want %q", source, sourceGitHubIssueLink)
			}
		})
	}
}

// TestAttributionTakesTheHeadBranch: source 3, a key in the branch name, matched whatever its
// case.
func TestAttributionTakesTheHeadBranch(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-121"}, nil)
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/120", HeadRef: "fix/attr-121-guard-the-thing",
	})
	if key != "ATTR-121" || source != sourceBranch {
		t.Fatalf("resolved %q from %q, want ATTR-121 from %q", key, source, sourceBranch)
	}
}

// TestAttributionFallsBackToCommitMessages: source 5, the first known key across the commit
// messages in commit order, which is read only where the four sources above name nobody.
func TestAttributionFallsBackToCommitMessages(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-131", "ATTR-132"}, nil)
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/130",
		CommitMessages: []string{"wip", "refs ATTR-999 and ATTR-132", "closes ATTR-131"},
	})
	if key != "ATTR-132" || source != sourceCommitMessage {
		t.Fatalf("resolved %q from %q, want ATTR-132 from %q", key, source, sourceCommitMessage)
	}
}

// TestAttributionTakesTheSourcesInOrder: the external link, then the GitHub issue the body
// cites, then the head branch, then the title and the body, then the commit messages; each
// source is tried only when every earlier one found nothing.
func TestAttributionTakesTheSourcesInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	prURL := "https://github.com/acme/widgets/pull/140"
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-141", "ATTR-142", "ATTR-143", "ATTR-144", "ATTR-145"}, map[string][]string{
		"ATTR-142": {prURL},
		"ATTR-143": {"https://github.com/acme/widgets/issues/143"},
	})
	all := ruleFacts{
		Repo: "acme/widgets", URL: prURL, Title: "feat: ATTR-141", Body: "Fixes #143.",
		HeadRef: "attr-144-branch", CommitMessages: []string{"ATTR-145"},
	}
	steps := []struct {
		drop   func(*ruleFacts)
		want   string
		source attributionSource
	}{
		{func(*ruleFacts) {}, "ATTR-142", sourceExternalLink},
		{func(f *ruleFacts) { f.URL = "https://github.com/acme/widgets/pull/149" }, "ATTR-143", sourceGitHubIssueLink},
		{func(f *ruleFacts) { f.Body = "" }, "ATTR-144", sourceBranch},
		{func(f *ruleFacts) { f.HeadRef = "main" }, "ATTR-141", sourceTitleBody},
		{func(f *ruleFacts) { f.Title = "feat: none" }, "ATTR-145", sourceCommitMessage},
		{func(f *ruleFacts) { f.CommitMessages = nil }, "", sourceNone},
	}
	facts := all
	for _, step := range steps {
		step.drop(&facts)
		key, source := resolvedKey(t, ctx, pool, facts)
		if key != step.want || source != step.source {
			t.Fatalf("with %+v resolved %q from %q, want %q from %q", facts, key, source, step.want, step.source)
		}
	}
}

// TestAttributionTakesTheHeadBranchOverTheTitle: the first of the two moves R8 of INFRA-1522
// makes. A branch name is an act that names the issue — someone branched for it — while a key
// in a title is a mention, which a pull request that follows or reverts another issue's work
// carries as readily as one that does it. Rule 1 credited this pull request to the issue its
// title names.
func TestAttributionTakesTheHeadBranchOverTheTitle(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-231", "ATTR-232"}, nil)
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/230",
		Title: "feat: follows ATTR-231", HeadRef: "legion/attr-232-the-work",
	})
	if key != "ATTR-232" || source != sourceBranch {
		t.Fatalf("resolved %q from %q, want ATTR-232 from %q: the branch outranks the title", key, source, sourceBranch)
	}
}

// TestAttributionTakesALinkOverTheTitle: the second move R8 makes, for both link sources.
// Someone linking an issue to the pull request, or the pull request citing a GitHub issue an
// issue holds, names the issue as deliberately as a branch does.
func TestAttributionTakesALinkOverTheTitle(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	prURL := "https://github.com/acme/widgets/pull/240"
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-241", "ATTR-242", "ATTR-243"}, map[string][]string{
		"ATTR-242": {prURL},
		"ATTR-243": {"https://github.com/acme/widgets/issues/243"},
	})
	for _, tc := range []struct {
		name  string
		facts ruleFacts
		want  string
		from  attributionSource
	}{
		{
			name: "the issue linking the pull request",
			facts: ruleFacts{
				Repo: "acme/widgets", URL: prURL, Title: "feat: follows ATTR-241",
			},
			want: "ATTR-242", from: sourceExternalLink,
		},
		{
			name: "the issue holding the GitHub issue the body cites",
			facts: ruleFacts{
				Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/249",
				Title: "feat: follows ATTR-241", Body: "Fixes #243.",
			},
			want: "ATTR-243", from: sourceGitHubIssueLink,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, source := resolvedKey(t, ctx, pool, tc.facts)
			if key != tc.want || source != tc.from {
				t.Fatalf("resolved %q from %q, want %q from %q: a link outranks the title", key, source, tc.want, tc.from)
			}
		})
	}
}

// TestAttributionRefusesAKeyDispatchDoesNotHave: a key-shaped string in every source that names
// no stored issue credits nothing.
func TestAttributionRefusesAKeyDispatchDoesNotHave(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	key, source := resolvedKey(t, ctx, pool, ruleFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/150",
		Title: "feat: ATTR-9150", Body: "ATTR-9151, #9152", HeadRef: "attr-9153-x", CommitMessages: []string{"ATTR-9154"},
	})
	if key != "" || source != sourceNone {
		t.Fatalf("resolved %q from %q, want no issue: no source names a stored issue", key, source)
	}
}

// TestCompletingAPullRequestWhoseCommitsFailLeavesItPartial: a completion whose commit read fails
// (a 500 here, which readGitHubPage does not retry) writes nothing, so the row stays partial with
// no sessions rather than recording them short for good. Its texts row holds no commit messages,
// so the statement does not know its issue yet. The next pass completes the row once the commits
// answer, and the statement takes the issue from its commit message.
func TestCompletingAPullRequestWhoseCommitsFailLeavesItPartial(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-181"}, nil)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 180, Title: "feat: commits fail once", URL: "https://github.com/acme/widgets/pull/180",
		Author: "octocat", CreatedAt: &merged, Partial: true,
	}); err != nil {
		t.Fatalf("seed partial #180: %v", err)
	}

	var commitsFailing atomic.Bool
	commitsFailing.Store(true)
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	// The head branch lives in acme/widgets itself, so the rule reads the commit messages of #180.
	fake.handle("GET /repos/acme/widgets/pulls/180", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 180, "title": "feat: commits fail once", "html_url": "https://github.com/acme/widgets/pull/180",
			"user": map[string]any{"login": "octocat"}, "labels": []any{}, "state": "closed", "merged": true,
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z", "updated_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
			"head": map[string]any{"ref": "feature", "sha": headOf(180), "repo": map[string]any{"full_name": "acme/widgets"}},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/180/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "x", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}}})
			return
		}
		// A 500, which readGitHubPage does not retry (it retries only timeouts and gateway
		// timeouts), for each read of the commit messages in the first pass.
		if commitsFailing.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "fix ATTR-181: commits"}}})
	})
	noRuns(t, fake)

	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	reconcile.runOnce(ctx)
	first, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 180`))
	if err != nil {
		t.Fatalf("read #180 after the first pass: %v", err)
	}
	if !first.Partial || len(first.Sessions) != 0 {
		t.Fatalf("#180 after its commits failed: partial = %v, sessions = %v; want still partial with no sessions", first.Partial, first.Sessions)
	}
	if texts, found := storedTextsOf(t, ctx, pool, "acme/widgets", 180); !found || texts.messagesRead {
		t.Fatalf("#180 after its commits failed: texts row found = %v, messages read = %v; want a texts row with commit_messages null",
			found, texts.messagesRead)
	}
	if answer := answerOf(t, ctx, pool, "acme/widgets", 180); answer != "nobody from none, no facts" {
		t.Fatalf("#180 after its commits failed answers %q, want %q: no source names an issue yet", answer, "nobody from none, no facts")
	}

	commitsFailing.Store(false)
	reconcile.runOnce(ctx)
	second, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 180`))
	if err != nil {
		t.Fatalf("read #180 after the second pass: %v", err)
	}
	if second.Partial {
		t.Fatal("#180 after the commits answer is still partial, want complete")
	}
	if texts, found := storedTextsOf(t, ctx, pool, "acme/widgets", 180); !found || !texts.messagesRead || !slices.Equal(texts.commit, []string{"ATTR-181"}) {
		t.Fatalf("#180 after the commits answer: texts row found = %v, messages read = %v, commit_keys = %q; want read, with [ATTR-181]",
			found, texts.messagesRead, texts.commit)
	}
	if answer := answerOf(t, ctx, pool, "acme/widgets", 180); answer != "ATTR-181 from commit_message" {
		t.Fatalf("#180 after the commits answer answers %q, want %q", answer, "ATTR-181 from commit_message")
	}
}

// TestReconcileCreditsAPullRequestAnIssueLinksLater: a pull request whose stored facts name no
// issue gets the issue whose external link names it. The timeline gives that issue after the link
// and one pass, and no pass reads GitHub for the pull request.
func TestReconcileCreditsAPullRequestAnIssueLinksLater(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-171"}, nil)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 170, Title: "feat: unlinked", URL: "https://github.com/acme/widgets/pull/170",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged,
	}); err != nil {
		t.Fatalf("seed #170: %v", err)
	}
	// The results row and the texts of #170 name no key. Its messages are read, and they are empty.
	storeStatementRow(t, ctx, pool, statementRow{
		repo: "acme/widgets", number: 170, branch: "feature", title: "feat: unlinked", texts: readTexts(""),
	})

	var pullFetches atomic.Int32
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/", func(w http.ResponseWriter, r *http.Request) {
		pullFetches.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	noRuns(t, fake)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

	reconcile.runOnce(ctx)
	day := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	issueOf := func() *string {
		t.Helper()
		listed, err := ListPullRequestsInWindow(ctx, pool, day, day.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("ListPullRequestsInWindow over %s: %v", day.Format(time.DateOnly), err)
		}
		for _, pr := range listed {
			if pr.Repo == "acme/widgets" && pr.Number == 170 {
				return pr.IssueKey
			}
		}
		t.Fatalf("ListPullRequestsInWindow over %s lists no #170", day.Format(time.DateOnly))
		return nil
	}
	if issue := issueOf(); issue != nil {
		t.Fatalf("#170 issue = %s before any link names it, want none", *issue)
	}

	seedAttributionIssues(t, ctx, pool, []string{"ATTR-171"}, map[string][]string{
		"ATTR-171": {"https://github.com/acme/widgets/pull/170"},
	})
	reconcile.runOnce(ctx)
	if issue := issueOf(); issue == nil || *issue != "ATTR-171" {
		t.Fatalf("#170 issue = %v after ATTR-171 linked it, want ATTR-171", issue)
	}
	if n := pullFetches.Load(); n != 0 {
		t.Fatalf("pull request fetched %d times; want 0 (the statement reads the stored facts, with no GitHub call)", n)
	}
}

// noRuns answers both workflows' run listings with no runs.
func noRuns(t *testing.T, fake *fakeGitHub) {
	t.Helper()
	for _, workflow := range []string{"deploy.yml", "pr-checks.yml"} {
		fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2F"+workflow+"/runs", func(w http.ResponseWriter, r *http.Request) {
			mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
		})
	}
}
