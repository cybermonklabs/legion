package delivery

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The attribution stops at the first of the five sources that names an issue Dispatch holds
// (attribution.go): of that source's issues it takes the first that resolves to the pull
// request's repository, and where there is none it credits nobody rather than an issue a later
// source names. A key that names no issue names nothing, so the walk goes on past it, and the
// commit messages are read only where no earlier source names an issue.
//
// The pull requests here are in acme/gadgets: GDGT-<n> resolves there (seedGadgetsIssue) and
// HEAD-<n> to acme/widgets (seedHeadIssue).

// firstSourcePull is what one pull request says about itself: its title and body, its head
// branch, its commit messages in commit order, and the issue whose external link is its URL, if
// any.
type firstSourcePull struct {
	title, body, branch string
	commits             []string
	linkedBy            string
}

// firstSourcePick is what one of the three places the pick is made concluded about a pull request:
// the issue ("" for nobody) and the source that named it.
type firstSourcePick struct {
	key    string
	source attributionSource
}

// creditedTo is the issue a stored row credits, "" for nobody.
func creditedTo(issue *string) string {
	if issue == nil {
		return ""
	}
	return *issue
}

// serveFirstSourcePull answers GitHub's two reads of pull request number in acme/gadgets, the pull
// request and its commits, from pull, open and headed in acme/gadgets itself, and counts the
// commit reads.
func serveFirstSourcePull(t *testing.T, fake *fakeGitHub, number int, pull firstSourcePull) *atomic.Int32 {
	t.Helper()
	path := "/repos/acme/gadgets/pulls/" + strconv.Itoa(number)
	fake.handle("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": number, "title": pull.title, "body": pull.body,
			"html_url": "https://github.com/acme/gadgets/pull/" + strconv.Itoa(number),
			"state":    "open", "merged": false, "updated_at": "2026-11-28T12:57:00Z",
			"head": map[string]any{
				"ref": pull.branch, "sha": "402840284028402840284028402840284028" + strconv.Itoa(1000+number),
				"repo": map[string]any{"full_name": "acme/gadgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	var commitReads atomic.Int32
	fake.handle("GET "+path+"/commits", func(w http.ResponseWriter, _ *http.Request) {
		commitReads.Add(1)
		commits := []any{}
		for _, message := range pull.commits {
			commits = append(commits, map[string]any{"commit": map[string]any{"message": message}})
		}
		mustEncode(t, w, commits)
	})
	return &commitReads
}

// attributeEachWay runs pull, as pull request number of acme/gadgets, through each of the three
// places the pick is made, which must answer alike: the head row the reconcile's refresh writes
// (RefreshPullRequestHead, through headAttribution and attributeHead, the one path that reads the
// commit messages from GitHub, and only where it must), one stored pull request's pick
// (resolveStoredIssueKey), and the stored rows' pick in SQL (AttributePullRequests). It answers the
// three picks by place, and how many times the refresh read the commits.
func attributeEachWay(t *testing.T, ctx context.Context, pool *store.Pool, number int, pull firstSourcePull) (map[string]firstSourcePick, int32) {
	t.Helper()
	url := "https://github.com/acme/gadgets/pull/" + strconv.Itoa(number)
	if pull.linkedBy != "" {
		if _, err := pool.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
			on conflict do nothing`, pull.linkedBy, url); err != nil {
			t.Fatalf("link %s to %s: %v", pull.linkedBy, url, err)
		}
	}
	picks := map[string]firstSourcePick{}

	fake := newFakeGitHub(t)
	commitReads := serveFirstSourcePull(t, fake, number, pull)
	if err := unrenamed().RefreshPullRequestHead(ctx, pool, fake.newTestClient(), nil, "acme/gadgets", number, time.Now()); err != nil {
		t.Fatalf("refresh #%d: %v", number, err)
	}
	head := readHead(t, ctx, pool, "acme/gadgets", number)
	picks["head row"] = firstSourcePick{key: creditedTo(head.IssueKey), source: attributionSource(head.AttributionSource)}

	facts := attributionFacts{
		Repo: "acme/gadgets", URL: url, Title: pull.title, Body: pull.body, HeadRef: pull.branch,
		CommitMessages: pull.commits,
	}
	key, source := resolvedKey(t, ctx, pool, facts)
	picks["one stored pull request"] = firstSourcePick{key: key, source: source}

	inputs := unrenamed().attributionInputsFrom(facts)
	merged := time.Date(2026, 11, 28, 12, 57, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/gadgets", Number: number, Title: pull.title, URL: url,
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &inputs,
	}); err != nil {
		t.Fatalf("store #%d: %v", number, err)
	}
	if _, err := unrenamed().AttributePullRequests(ctx, pool, AttributionResolveBatch); err != nil {
		t.Fatalf("attribute the stored rows: %v", err)
	}
	stored := storedIssueKey(t, ctx, pool, "acme/gadgets", number)
	if stored == "none" {
		stored = ""
	}
	// The batch stores the issue alone, not the source that named it.
	picks["the stored rows' batch"] = firstSourcePick{key: stored}
	return picks, commitReads.Load()
}

// TestAttributionStopsAtTheFirstSourceThatNamesAnIssue: each case the rule makes, through each of
// the three places the pick is made.
func TestAttributionStopsAtTheFirstSourceThatNamesAnIssue(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	other := seedHeadIssue(t, ctx, pool, 47)
	own := seedGadgetsIssue(t, ctx, pool, 48)
	bystander := seedGadgetsIssue(t, ctx, pool, 647)

	for _, tc := range []struct {
		name   string
		number int
		pull   firstSourcePull
		want   string
		from   attributionSource
		// readsCommits is whether the refresh reads the commit messages: only where no source
		// above them names an issue.
		readsCommits bool
	}{
		{
			name:   "the first source names an issue of this repository, which wins",
			number: 301,
			pull: firstSourcePull{
				title: "feat: the work", body: "Dispatch: " + own, branch: "topic",
				commits: []string{"Keep the " + bystander + " rows", "Dispatch: " + own},
			},
			want: own, from: sourceTitleBody,
		},
		{
			name:   "the first source names only an issue of another repository, so nobody is credited and the walk stops",
			number: 302,
			pull: firstSourcePull{
				title: "feat: the work", body: "Dispatch: " + other, branch: "topic",
				commits: []string{"Keep the " + bystander + " rows", "Dispatch: " + other},
			},
			want: "", from: sourceNone,
		},
		{
			name:   "an issue of another repository links the pull request, above a title naming this repository's issue",
			number: 303,
			pull:   firstSourcePull{title: own + ": the work", branch: "topic", linkedBy: other},
			want:   "", from: sourceNone,
		},
		{
			name:   "the branch names an issue of another repository, above a title naming this repository's issue",
			number: 304,
			pull:   firstSourcePull{title: own + ": the work", branch: "legion/head-47"},
			want:   "", from: sourceNone,
		},
		{
			name:   "the first sources' keys name no issue, so the walk goes on to the commit messages",
			number: 305,
			pull: firstSourcePull{
				title: "Handle UTF-8 and SHA-256 for GDGT-9999", body: "See HEAD-9999.", branch: "legion/gdgt-9998",
				commits: []string{"wip", "Dispatch: " + own},
			},
			want: own, from: sourceCommitMessage, readsCommits: true,
		},
		{
			name:   "the branch's key names no issue, so the walk goes on to the title",
			number: 306,
			pull: firstSourcePull{
				title: own + ": the work", branch: "legion/head-9999",
				commits: []string{"Keep the " + bystander + " rows"},
			},
			want: own, from: sourceTitleBody,
		},
		{
			name:   "one source names an issue of another repository and then one of this repository, which wins",
			number: 307,
			pull: firstSourcePull{
				title: other + ": the other repository's issue, and " + own + " (this one)", branch: "topic",
				commits: []string{"Keep the " + bystander + " rows"},
			},
			want: own, from: sourceTitleBody,
		},
		{
			name:   "no source names an issue",
			number: 308,
			pull: firstSourcePull{
				title: "chore: tidy", body: "No key here.", branch: "topic",
				commits: []string{"wip", "Dispatch: NOPE-1"},
			},
			want: "", from: sourceNone, readsCommits: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			picks, commitReads := attributeEachWay(t, ctx, pool, tc.number, tc.pull)
			for place, pick := range picks {
				wantSource := tc.from
				if place == "the stored rows' batch" {
					wantSource = ""
				}
				if pick.key != tc.want || pick.source != wantSource {
					t.Errorf("%s: #%d is credited to %q from %q, want %q from %q", place, tc.number, pick.key, pick.source, tc.want, wantSource)
				}
			}
			if reads := commitReads > 0; reads != tc.readsCommits {
				t.Errorf("the refresh read #%d's commits %d times, want reads %v: they are read only where no source above them names an issue",
					tc.number, commitReads, tc.readsCommits)
			}
		})
	}
}

// refreshMergeMention writes the head row of pull request 4028 of acme/gadgets through the
// reconcile's refresh (RefreshPullRequestHead), in the shape of a production pull request that was
// credited to an issue only its merge commit mentions. Its body ends with `Dispatch: <declared>`
// and so does its last commit. One commit, a merge of the base branch, names <mentioned> in prose.
func refreshMergeMention(t *testing.T, ctx context.Context, pool *store.Pool, declared, mentioned string) storedHead {
	t.Helper()
	const head = "4028402840284028402840284028402840284028"
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/gadgets/pulls/4028", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 4028, "title": "gadgets: one scan exception row for copied licence notices",
			"body":     "All seven rows added by #66 remain.\n\nThis pull request delivers the licence-header lint precondition.\n\nDispatch: " + declared,
			"html_url": "https://github.com/acme/gadgets/pull/4028",
			"state":    "open", "merged": false, "updated_at": "2026-11-28T12:57:00Z",
			"head": map[string]any{
				"ref": "agent/licence-rows", "sha": head,
				"repo": map[string]any{"full_name": "acme/gadgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/gadgets/pulls/4028/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "Add scan exception rows for the copied licence headers"}},
			map[string]any{"commit": map[string]any{"message": "Merge main into the licence scan exceptions\n\nKeep every main exception row, including the " + mentioned + " rows from #66."}},
			map[string]any{"commit": map[string]any{"message": "Correct the round-trip lint change ownership\n\nDispatch: " + declared}},
		})
	})
	if err := unrenamed().RefreshPullRequestHead(ctx, pool, fake.newTestClient(), nil, "acme/gadgets", 4028, time.Now()); err != nil {
		t.Fatalf("refresh #4028: %v", err)
	}
	return readHead(t, ctx, pool, "acme/gadgets", 4028)
}

// holdsMergeMention reports whether issue's head set (HeadsAttributedTo, what the results read
// compares a before_merge check with) holds #4028.
func holdsMergeMention(t *testing.T, ctx context.Context, pool *store.Pool, issue string) bool {
	t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(ctx, pool, issue, HeadAnswers{})
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	for _, h := range heads {
		if h.Number == 4028 {
			return true
		}
	}
	return false
}

// TestAPullRequestIsNotCreditedToAnIssueOnlyItsMergeCommitMentions: the body and the last commit
// declare an issue of another repository, and a merge commit's prose mentions an issue of this
// one. Rules 1 to 8 passed the declared issue over for its repository and credited the pull
// request to the mentioned one, so every before_merge check of that unrelated issue needed a pass
// at the head of another issue's work. The body names an issue, so the walk stops there.
func TestAPullRequestIsNotCreditedToAnIssueOnlyItsMergeCommitMentions(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	declared := seedHeadIssue(t, ctx, pool, 47)
	bystander := seedGadgetsIssue(t, ctx, pool, 647)
	stored := refreshMergeMention(t, ctx, pool, declared, bystander)
	if stored.IssueKey != nil && *stored.IssueKey == bystander {
		t.Errorf("#4028 is credited to %s from source %q: only a merge commit's prose names %s, while the body and the last commit's trailer name %s",
			bystander, stored.AttributionSource, bystander, declared)
	}
	if stored.IssueKey != nil || stored.AttributionSource != string(sourceNone) {
		t.Errorf("#4028 = %s from %q, want nobody: the body names %s, of acme/widgets, and no issue of acme/gadgets",
			creditedTo(stored.IssueKey), stored.AttributionSource, declared)
	}
	if holdsMergeMention(t, ctx, pool, bystander) {
		t.Fatalf("%s's head set holds #4028, so each before_merge check of %s needs a pass at the head of another issue's work", bystander, bystander)
	}
}

// The first control: the same pull request with no issue in its merge commit's prose. Nobody is
// credited, before this rule and after it, so the test above fails on the mention and on nothing
// else.
func TestAPullRequestDeclaringAnIssueOfAnotherRepositoryWithNoMentionCreditsNobody(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	declared := seedHeadIssue(t, ctx, pool, 47)
	bystander := seedGadgetsIssue(t, ctx, pool, 647)
	stored := refreshMergeMention(t, ctx, pool, declared, "the main")
	if stored.IssueKey != nil || stored.AttributionSource != string(sourceNone) {
		t.Fatalf("#4028 = %s from %q, want nobody: its declared issue %s is of acme/widgets", creditedTo(stored.IssueKey), stored.AttributionSource, declared)
	}
	if holdsMergeMention(t, ctx, pool, bystander) {
		t.Fatalf("%s's head set holds #4028 with no mention of it", bystander)
	}
}

// The second control: the declared issue resolves to the pull request's own repository. It wins
// from the body, before this rule and after it: the mention took the pull request only because the
// declared issue was passed over for its repository.
func TestAPullRequestDeclaringAnIssueOfItsOwnRepositoryIsCreditedFromItsBody(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	declared := seedGadgetsIssue(t, ctx, pool, 48)
	bystander := seedGadgetsIssue(t, ctx, pool, 647)
	stored := refreshMergeMention(t, ctx, pool, declared, bystander)
	if stored.IssueKey == nil || *stored.IssueKey != declared || stored.AttributionSource != string(sourceTitleBody) {
		t.Fatalf("#4028 = %s from %q, want %s from %s", creditedTo(stored.IssueKey), stored.AttributionSource, declared, sourceTitleBody)
	}
	if holdsMergeMention(t, ctx, pool, bystander) {
		t.Fatalf("%s's head set holds #4028 though %s is held here", bystander, declared)
	}
}

// TestTheReconcileTakesAStoredPullRequestFromAnIssueOnlyItsMergeCommitMentions: a row rule 8
// credited to the issue a merge commit mentions is credited to nobody by the next reconcile pass,
// and the move is recorded on the issue it left. An open row is read again at every pass once
// PullRequestHeadStaleAfter has gone by, whatever rule judged it; a merged row is read again only
// because the rule that judged it is older than AttributionRuleVersion (ListStalePullRequestHeads).
func TestTheReconcileTakesAStoredPullRequestFromAnIssueOnlyItsMergeCommitMentions(t *testing.T) {
	for _, state := range []string{"open", "merged"} {
		t.Run(state, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			declared := seedHeadIssue(t, ctx, pool, 47)
			bystander := seedGadgetsIssue(t, ctx, pool, 647)
			const number = 4028
			const sha = "4028402840284028402840284028402840284028"
			pull := firstSourcePull{
				title: "gadgets: one scan exception row", body: "Dispatch: " + declared,
				branch:  "agent/licence-rows",
				commits: []string{"Merge main\n\nKeep every main exception row, including the " + bystander + " rows.", "Dispatch: " + declared},
			}
			fake := newFakeGitHub(t)
			fake.handle("GET /repos/acme/gadgets/pulls/4028", func(w http.ResponseWriter, _ *http.Request) {
				mustEncode(t, w, map[string]any{
					"number": number, "title": pull.title, "body": pull.body,
					"html_url": "https://github.com/acme/gadgets/pull/4028",
					"state":    map[string]string{"open": "open", "merged": "closed"}[state], "merged": state == "merged",
					"updated_at": "2026-11-28T12:57:00Z",
					"head": map[string]any{
						"ref": pull.branch, "sha": sha, "repo": map[string]any{"full_name": "acme/gadgets"},
					},
					"base": map[string]any{"ref": "main"},
				})
			})
			fake.handle("GET /repos/acme/gadgets/pulls/4028/commits", func(w http.ResponseWriter, _ *http.Request) {
				commits := []any{}
				for _, message := range pull.commits {
					commits = append(commits, map[string]any{"commit": map[string]any{"message": message}})
				}
				mustEncode(t, w, commits)
			})

			// The row as rule 8 left it: credited to the mentioned issue, read a day ago.
			read := time.Now().Add(-24 * time.Hour)
			broker := events.NewBroker()
			if err := UpsertPullRequestHead(ctx, pool, broker, PullRequestHead{
				Repo: "acme/gadgets", Number: number, HeadSHA: sha, HeadRef: pull.branch, BaseRef: "main",
				State: state, IssueKey: &bystander, AttributionSource: string(sourceCommitMessage), AttributionComplete: true,
				Title: pull.title, URL: "https://github.com/acme/gadgets/pull/4028", CheckedAt: &read,
			}); err != nil {
				t.Fatalf("store the row rule 8 left: %v", err)
			}
			if _, err := pool.Exec(ctx, `update pull_request_heads set attribution_rule = 8 where repo = 'acme/gadgets' and number = $1`, number); err != nil {
				t.Fatalf("stamp the row with rule 8: %v", err)
			}

			NewReconcile(pool, fake.newTestClient(), broker, unrenamed()).runOnce(ctx)

			head := readHead(t, ctx, pool, "acme/gadgets", number)
			if head.IssueKey != nil || head.AttributionSource != string(sourceNone) || head.attributionRule != AttributionRuleVersion {
				t.Fatalf("the %s row after a pass = %s from %q under rule %d, want nobody from %q under rule %d",
					state, creditedTo(head.IssueKey), head.AttributionSource, head.attributionRule, sourceNone, AttributionRuleVersion)
			}
			if holdsMergeMention(t, ctx, pool, bystander) {
				t.Fatalf("%s's head set still holds #4028 after the pass", bystander)
			}
			left := attributionEvents(t, ctx, pool, bystander)
			if len(left) != 2 || left[1].Payload.From == nil || *left[1].Payload.From != bystander || left[1].Payload.To != nil {
				t.Fatalf("%s carries %+v, want the event that credited it and one that leaves it credited to nobody", bystander, left)
			}
		})
	}
}
