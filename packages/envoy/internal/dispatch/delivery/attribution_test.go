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
// pull request is in, and each links[key] URL as that issue's external link. Idempotent, so
// tests sharing the database can seed the same keys.
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

func resolvedKey(t *testing.T, ctx context.Context, pool *store.Pool, facts attributionFacts) (string, attributionSource) {
	t.Helper()
	key, source, err := resolveStoredIssueKey(ctx, pool, facts.Repo, facts.URL, attributionInputsFrom(facts))
	if err != nil {
		t.Fatalf("resolveStoredIssueKey(%+v): %v", facts, err)
	}
	if key == nil {
		return "", source
	}
	return *key, source
}

// storedIssueKey is the issue delivery_pull_requests credits one stored pull request to, as a
// failure message names it: "none" where the row credits nobody, which AttributePullRequests
// writes as a null issue_key.
func storedIssueKey(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	var issue *string
	if err := pool.QueryRow(ctx,
		`select issue_key from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&issue); err != nil {
		t.Fatalf("read the stored pull request %s#%d: %v", repo, number, err)
	}
	if issue == nil {
		return "none"
	}
	return *issue
}

// TestAttributionTakesAnExternalLinkNamingThePullRequest: source 1, an issue whose external
// link is the pull request's own URL.
func TestAttributionTakesAnExternalLinkNamingThePullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-101"}, map[string][]string{
		"ATTR-101": {"https://github.com/acme/widgets/pull/101"},
	})
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
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
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
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
		// citation_boundary_test.go holds.
		{"userinfo before github.com", "See https://user@github.com/acme/widgets/issues/77 for context.", "ATTR-111"},
		// An issue path with no host is the path of no URL at all: the finder matches the path
		// rather than the host, so this is the row that says so.
		{"an issue path with no host", "See /acme/widgets/issues/77 for context.", ""},
		{"first linked citation in body order", "Unlinked #12, then #88, then https://github.com/acme/other/issues/5.", "ATTR-113"},
		{"a pull request URL is not an issue", "Reverts https://github.com/acme/widgets/pull/77.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, source := resolvedKey(t, ctx, pool, attributionFacts{
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

// TestCitedIssuesCreditsTheURLsOwnPath drives citedIssues, which needs no database, over the
// shapes that tell a citation from the same bytes standing inside another URL. Each row names
// what the body cites, whole: an extra citation is a defect of its own, which a membership test
// would pass over.
func TestCitedIssuesCreditsTheURLsOwnPath(t *testing.T) {
	const cited = "https://github.com/acme/widgets/issues/7"
	const citedAB = "https://github.com/a/b/issues/7"
	const citedCD = "https://github.com/c/d/issues/9"
	const cited12 = "https://github.com/acme/widgets/issues/12"
	for _, row := range []struct {
		name string
		body string
		want []string
	}{
		// The matched path must be the URL's own path from its start. A host check alone credits
		// any path a github.com URL carries, which is how /blob/, /tree/ and /wiki/ URLs were
		// read as citations of an issue nobody wrote.
		{"a blob URL deep in a repository", "See https://github.com/acme/widgets/blob/main/docs/issues/12 here.", nil},
		{"a tree URL", "See https://github.com/acme/widgets/tree/fix/issues/123 here.", nil},
		{"a wiki URL", "See https://github.com/acme/widgets/wiki/issues/3 here.", nil},
		{"an issue path repeated inside a longer path", "See https://github.com/acme/widgets/issues/7/x/y/issues/8 here.", []string{cited}},
		// A citation the prose wraps is still a citation: the host opens a word of its own,
		// whatever stands beside it, and a space is not the only thing that can end a word.
		{"in parentheses", "See (github.com/acme/widgets/issues/7) here.", []string{cited}},
		{"in backticks", "See `github.com/acme/widgets/issues/7` here.", []string{cited}},
		{"in a Markdown link", "See [the issue](github.com/acme/widgets/issues/7) here.", []string{cited}},
		{"between bold markers", "See **github.com/acme/widgets/issues/7** here.", []string{cited}},
		{"after a non-breaking space", "See\u00a0github.com/acme/widgets/issues/7 here.", []string{cited}},
		{"after a line separator", "See\u2028github.com/acme/widgets/issues/7 here.", []string{cited}},
		{"in text with no spaces at all", "参照github.com/acme/widgets/issues/7です", []string{cited}},
		{"after a key and a colon", "Refs:github.com/acme/widgets/issues/7", []string{cited}},
		// A separator or a closing bracket left of the host ends whatever stood before it, so the
		// host opens a URL of its own even where that earlier text carries a `/` or a `#`: a
		// citation before this one, a bare #N, or a Markdown link's own text.
		{"two citations joined by a comma", "github.com/a/b/issues/7,github.com/c/d/issues/9", []string{citedAB, citedCD}},
		{"two citations joined by a semicolon", "github.com/a/b/issues/7;github.com/c/d/issues/9", []string{citedAB, citedCD}},
		{"two citations joined by an ampersand", "github.com/a/b/issues/7&github.com/c/d/issues/9", []string{citedAB, citedCD}},
		{"two citations joined by a plus", "github.com/a/b/issues/7+github.com/c/d/issues/9", []string{citedAB, citedCD}},
		{"a schemed citation before a bare one", "https://github.com/a/b/issues/7,github.com/c/d/issues/9", []string{citedAB, citedCD}},
		{"two citations each in parentheses", "(github.com/a/b/issues/7)(github.com/c/d/issues/9)", []string{citedAB, citedCD}},
		{"after a bare reference and a comma", "#12,github.com/acme/widgets/issues/7", []string{cited12, cited}},
		{"after a bare reference and a colon", "#12:github.com/acme/widgets/issues/7", []string{cited12, cited}},
		{"after a bracketed bare reference", "Fixes (#12)github.com/acme/widgets/issues/7", []string{cited12, cited}},
		{"in a Markdown link whose text names the issue", "See [acme/widgets#7](github.com/acme/widgets/issues/7)", []string{cited}},
		{"in a Markdown link whose text is a bare reference", "See [#7](github.com/acme/widgets/issues/7)", []string{cited}},
		{"after bold markers and a colon", "**#7**:github.com/acme/widgets/issues/7", []string{cited}},
		{"after a word of prose carrying a slash", "Related issues/PRs:github.com/acme/widgets/issues/7", []string{cited}},
		{"a scheme-relative URL in parentheses", "(//github.com/acme/widgets/issues/7)", []string{cited}},
		{"a scheme-relative URL in brackets", "[//github.com/acme/widgets/issues/7]", []string{cited}},
		// What stands to the left of the authority decides nothing, here as on the default
		// branch: a github.com issue path inside another URL's query, fragment or path is
		// credited, because the bytes the path hangs off name github.com. Whether such a URL
		// ought to credit at all is a question of its own, asked of the person this work is for
		// and not answered by this change, whose subject is a host that is not github.com.
		{"inside another host's query", "See https://example.test/r?u=github.com/acme/widgets/issues/7 here.", []string{cited}},
		{"inside another host's fragment", "See https://example.test/r#github.com/acme/widgets/issues/7 here.", []string{cited}},
		{"inside another host's path behind a tilde", "See https://example.test/~github.com/acme/widgets/issues/7 here.", []string{cited}},
		{"inside another host's path behind a bracket", "See https://example.test/(github.com/acme/widgets/issues/7) here.", []string{cited}},
		// An `@` before the host leaves github.com the authority the path hangs off, so the
		// citation is credited; `github.com@evil.test` is the other way round, with evil.test the
		// authority, and is credited to nobody (citation_boundary_test.go holds that pair).
		{"userinfo carrying another host's name", "See https://example.test/x@github.com/acme/widgets/issues/7 here.", []string{cited}},
		// Whatever digits stand between a colon and the path are read as the port, however many:
		// they name no host, so a port no URL may carry leaves the citation where it was rather
		// than dropping it for a typo. Past a host name's length the digits are no port, and what
		// then runs into the path is no host name either.
		{"a port no URL may carry", "See https://github.com:99999/acme/widgets/issues/7 here.", []string{cited}},
		{"digits where a port should be", "See https://github.com:443443443/acme/widgets/issues/7 here.", []string{cited}},
		{"more digits than a host name is long", "See https://github.com:" + strings.Repeat("4", 300) + "/acme/widgets/issues/7 here.", nil},
		// The host is the run of host bytes before the path and nothing further, so a long run of
		// bytes no host name may hold leaves the citation where it was, while a run of host bytes
		// longer than a host name is no host at all.
		{"a citation after a long run of bytes no host holds", strings.Repeat("~", 2100) + "github.com/acme/widgets/issues/7", []string{cited}},
		{"a host-byte run longer than a host name", strings.Repeat("a", 300) + ".github.com/acme/widgets/issues/7", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := citedIssues("acme/widgets", row.body); !slices.Equal(got, row.want) {
				t.Errorf("citedIssues(%q) = %v, want %v", row.body, got, row.want)
			}
		})
	}
}

// TestCitedIssuesCostsNoMoreThanTheBodyIsLong holds that the reading is linear in the body: the
// finder walks it once, and each match reads at most a host name's length of digits and host
// bytes before its path. The body is 1 MiB with no whitespace in it and an issue path every
// thirteen bytes, which is the shape a reading that walks back to the nearest space pays the
// square of: such a walk costs about 1.5 s at 64 KiB and so about 380 s at 1 MiB, where this one
// costs about a third of a second, nearly all of it the finder's own pass. The deadline is some
// thirty times that, so a loaded machine stays green and a walking reader cannot pass.
func TestCitedIssuesCostsNoMoreThanTheBodyIsLong(t *testing.T) {
	const cited = "https://github.com/acme/widgets/issues/7"
	const deadline = 10 * time.Second
	body := strings.Repeat("/a/b/issues/1", 1<<20/13) + ",github.com/acme/widgets/issues/7"
	began := time.Now()
	got := citedIssues("acme/widgets", body)
	if took := time.Since(began); took > deadline {
		t.Errorf("citedIssues over %d bytes took %s, want under %s", len(body), took, deadline)
	}
	if !slices.Equal(got, []string{cited}) {
		t.Fatalf("a body of %d bytes cites %v, want %s and nothing else", len(body), got, cited)
	}
	whole := "https://github.com/acme/widgets/issues/7" + strings.Repeat("x", 4096)
	if got := citedIssues("acme/widgets", whole); len(got) != 0 {
		t.Fatalf("a path running past the issue number cites %v, want nothing", got)
	}
}

// TestAttributionTakesTheHeadBranch: source 3, a key in the branch name, matched whatever its
// case.
func TestAttributionTakesTheHeadBranch(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-121"}, nil)
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
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
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
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
	all := attributionFacts{
		Repo: "acme/widgets", URL: prURL, Title: "feat: ATTR-141", Body: "Fixes #143.",
		HeadRef: "attr-144-branch", CommitMessages: []string{"ATTR-145"},
	}
	steps := []struct {
		drop   func(*attributionFacts)
		want   string
		source attributionSource
	}{
		{func(*attributionFacts) {}, "ATTR-142", sourceExternalLink},
		{func(f *attributionFacts) { f.URL = "https://github.com/acme/widgets/pull/149" }, "ATTR-143", sourceGitHubIssueLink},
		{func(f *attributionFacts) { f.Body = "" }, "ATTR-144", sourceBranch},
		{func(f *attributionFacts) { f.HeadRef = "main" }, "ATTR-141", sourceTitleBody},
		{func(f *attributionFacts) { f.Title = "feat: none" }, "ATTR-145", sourceCommitMessage},
		{func(f *attributionFacts) { f.CommitMessages = nil }, "", sourceNone},
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
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
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
		facts attributionFacts
		want  string
		from  attributionSource
	}{
		{
			name: "the issue linking the pull request",
			facts: attributionFacts{
				Repo: "acme/widgets", URL: prURL, Title: "feat: follows ATTR-241",
			},
			want: "ATTR-242", from: sourceExternalLink,
		},
		{
			name: "the issue holding the GitHub issue the body cites",
			facts: attributionFacts{
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
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/150",
		Title: "feat: ATTR-9150", Body: "ATTR-9151, #9152", HeadRef: "attr-9153-x", CommitMessages: []string{"ATTR-9154"},
	})
	if key != "" || source != sourceNone {
		t.Fatalf("resolved %q from %q, want no issue: no source names a stored issue", key, source)
	}
}

// TestReconcileBackfillsAttributionInputsOnce: a complete row stored before its attribution
// inputs existed is fetched once, stores them and is credited (here from its head branch); a
// row whose inputs are stored is never fetched again, on that pass or the next. The backfill
// reads only what the attribution needs: the pull request once and its commits once (one page),
// never FetchPullRequest's first-commit read, which a complete row already answered.
func TestReconcileBackfillsAttributionInputsOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-161"}, nil)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, number := range []int{160, 162} {
		if _, err := pool.Exec(ctx, `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, partial)
			values ('acme/widgets', $1, 'feat: history', $2, 'octocat', $3, $3, false)`,
			number, "https://github.com/acme/widgets/pull/"+strconv.Itoa(number), merged); err != nil {
			t.Fatalf("seed stored row #%d: %v", number, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		update delivery_pull_requests
		set attribution_title_keys = '{}', attribution_cited_issues = '{}', attribution_branch_keys = '{}', attribution_commit_keys = '{}'
		where number = 162`); err != nil {
		t.Fatalf("store #162's inputs: %v", err)
	}

	var fetches160, commits160, firstCommit160, fetches162 atomic.Int32
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/160", func(w http.ResponseWriter, r *http.Request) {
		fetches160.Add(1)
		mustEncode(t, w, map[string]any{
			"number": 160, "title": "feat: history", "html_url": "https://github.com/acme/widgets/pull/160",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
			"head": map[string]any{"ref": "fix/attr-161-history"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/160/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			firstCommit160.Add(1)
		} else {
			commits160.Add(1)
		}
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: history\n\nOmp-Session: ses-160", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/162", func(w http.ResponseWriter, r *http.Request) {
		fetches162.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	noRuns(t, fake)

	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())
	reconcile.runOnce(ctx)
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 160`))
	if err != nil {
		t.Fatalf("read #160: %v", err)
	}
	if pr.IssueKey == nil || *pr.IssueKey != "ATTR-161" || pr.Attribution == nil || len(pr.Attribution.BranchKeys) != 1 {
		t.Fatalf("#160 issue = %v, inputs = %+v; want ATTR-161 from its stored head branch keys", pr.IssueKey, pr.Attribution)
	}
	if len(pr.Sessions) != 1 || pr.Sessions[0] != "ses-160" {
		t.Fatalf("#160 sessions = %v, want the trailer its commits name", pr.Sessions)
	}
	if pull, commits, first := fetches160.Load(), commits160.Load(), firstCommit160.Load(); pull != 1 || commits != 1 || first != 0 {
		t.Fatalf("#160 over two passes: %d pull reads, %d commits pages, %d first-commit reads; want 1, 1, 0", pull, commits, first)
	}
	if n := fetches162.Load(); n != 0 {
		t.Fatalf("#162, whose inputs are stored, was fetched %d times; want 0", n)
	}
}

// TestCompletingAPullRequestWhoseCommitsFailLeavesItPartial: a completion whose commit read fails
// (a 500 here, which readGitHubPage does not retry) writes nothing, so the row stays partial with
// no sessions and no attribution inputs rather than recording them short for good, and the next
// pass completes it once the commits answer.
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
	fake.handle("GET /repos/acme/widgets/pulls/180", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 180, "title": "feat: commits fail once", "html_url": "https://github.com/acme/widgets/pull/180",
			"user": map[string]any{"login": "octocat"}, "labels": []any{},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
			"head": map[string]any{"ref": "feature"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/180/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "x", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}}})
			return
		}
		// A 500, which readGitHubPage does not retry (it retries only timeouts and gateway
		// timeouts), for the whole first pass: the completion's read and the backfill's.
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
	if !first.Partial || first.Attribution != nil || first.IssueKey != nil || len(first.Sessions) != 0 {
		t.Fatalf("#180 after its commits failed: partial = %v, inputs = %+v, issue = %v, sessions = %v; want still partial with nothing written",
			first.Partial, first.Attribution, first.IssueKey, first.Sessions)
	}

	commitsFailing.Store(false)
	reconcile.runOnce(ctx)
	second, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 180`))
	if err != nil {
		t.Fatalf("read #180 after the second pass: %v", err)
	}
	if second.Partial || second.Attribution == nil || len(second.Attribution.CommitKeys) != 1 || second.IssueKey == nil || *second.IssueKey != "ATTR-181" {
		t.Fatalf("#180 after the commits answer: partial = %v, inputs = %+v, issue = %v; want complete with ATTR-181 from its commit message",
			second.Partial, second.Attribution, second.IssueKey)
	}
}

// TestAttributingStoredPullRequestsWalksTheTableInBatches: each pass resolves at most its batch of
// stored rows, those checked longest ago first, so the walk comes round to every row and an
// issue that links a pull request credits it within one walk of the table.
func TestAttributingStoredPullRequestsWalksTheTableInBatches(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-191"}, nil)
	if _, err := pool.Exec(ctx, `delete from issue_external_links where url like 'https://github.com/acme/widgets/pull/19_'`); err != nil {
		t.Fatalf("clear an earlier run's links: %v", err)
	}
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	for number := 190; number < 195; number++ {
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/widgets", Number: number, Title: "feat: walked", URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
			Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &AttributionInputs{},
		}); err != nil {
			t.Fatalf("seed #%d: %v", number, err)
		}
	}
	checked := func() int {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where attribution_checked_at is not null`).Scan(&n); err != nil {
			t.Fatalf("count checked rows: %v", err)
		}
		return n
	}
	if _, err := AttributePullRequests(ctx, pool, 2); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if n := checked(); n != 2 {
		t.Fatalf("after one pass of 2: %d rows checked, want 2", n)
	}
	if _, err := AttributePullRequests(ctx, pool, 2); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n := checked(); n != 4 {
		t.Fatalf("after two passes of 2: %d rows checked, want 4 (the walk takes the unchecked rows next)", n)
	}

	// A link added now credits #194 within the walk: one more pass takes the last unchecked row.
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-191"}, map[string][]string{
		"ATTR-191": {"https://github.com/acme/widgets/pull/194"},
	})
	changed, err := AttributePullRequests(ctx, pool, 2)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	var issue *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where number = 194`).Scan(&issue); err != nil {
		t.Fatalf("read #194: %v", err)
	}
	if changed != 1 || issue == nil || *issue != "ATTR-191" {
		t.Fatalf("third pass changed %d rows, #194 issue = %v; want 1 and ATTR-191", changed, issue)
	}
}

// TestReconcileCreditsAPullRequestAnIssueLinksLater: a pull request stored with no issue is
// credited once an issue's external link names it, by the next pass and with no GitHub call.
func TestReconcileCreditsAPullRequestAnIssueLinksLater(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-171"}, nil)
	// deliveryTestPool clears only the delivery tables: drop the link an earlier run added.
	if _, err := pool.Exec(ctx, `delete from issue_external_links where url = 'https://github.com/acme/widgets/pull/170'`); err != nil {
		t.Fatalf("clear an earlier run's link: %v", err)
	}
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 170, Title: "feat: unlinked", URL: "https://github.com/acme/widgets/pull/170",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &AttributionInputs{},
	}); err != nil {
		t.Fatalf("seed #170: %v", err)
	}

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
	issueOf := func() *string {
		var issue *string
		if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where number = 170`).Scan(&issue); err != nil {
			t.Fatalf("read #170: %v", err)
		}
		return issue
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
		t.Fatalf("pull request fetched %d times; want 0 (stored inputs need no GitHub call)", n)
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
