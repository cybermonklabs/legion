package delivery

import (
	"context"
	"net/http"
	"strconv"
	"strings"
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

// firstSourceIssues are the three issues the pull requests here name: other resolves to
// acme/widgets, and own and bystander to acme/gadgets, the pull requests' own repository.
type firstSourceIssues struct {
	other, own, bystander string
}

func seedFirstSourceIssues(t *testing.T, ctx context.Context, pool *store.Pool) firstSourceIssues {
	t.Helper()
	return firstSourceIssues{
		other:     seedHeadIssue(t, ctx, pool, 47),
		own:       seedGadgetsIssue(t, ctx, pool, 48),
		bystander: seedGadgetsIssue(t, ctx, pool, 647),
	}
}

// firstSourcePull is what one pull request says about itself: its title and body, its head
// branch, its commit messages in commit order, the issue whose external link is its URL, if any,
// and whether it is merged (open where it is not).
type firstSourcePull struct {
	title, body, branch string
	commits             []string
	linkedBy            string
	merged              bool
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

// firstSourceHead is the head commit GitHub answers for pull request number of acme/gadgets
// (serveFirstSourcePull).
func firstSourceHead(number int) string {
	return "402840284028402840284028402840284028" + strconv.Itoa(1000+number)
}

// serveFirstSourcePull answers GitHub's two reads of pull request number in acme/gadgets, the pull
// request and its commits, from pull, headed in acme/gadgets itself at firstSourceHead, and counts
// the commit reads. A merged pull request is answered closed and merged, as GitHub answers one.
func serveFirstSourcePull(t *testing.T, fake *fakeGitHub, number int, pull firstSourcePull) *atomic.Int32 {
	t.Helper()
	path := "/repos/acme/gadgets/pulls/" + strconv.Itoa(number)
	state := "open"
	if pull.merged {
		state = "closed"
	}
	fake.handle("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": number, "title": pull.title, "body": pull.body,
			"html_url": "https://github.com/acme/gadgets/pull/" + strconv.Itoa(number),
			"state":    state, "merged": pull.merged, "updated_at": "2026-11-28T12:57:00Z",
			"head": map[string]any{
				"ref": pull.branch, "sha": firstSourceHead(number),
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

// firstSourcePicks is what the three places the pick is made concluded about one pull request
// (attributeEachWay): the head row the reconcile's refresh writes, one stored pull request's pick,
// and the issue the stored rows' batch credits ("" for nobody), which stores no source; and how
// many times the refresh read the pull request's commits.
type firstSourcePicks struct {
	head, stored firstSourcePick
	batch        string
	commitReads  int32
}

// attributeEachWay runs pull, as pull request number of acme/gadgets, through each of the three
// places the pick is made, which must answer alike: the head row the reconcile's refresh writes
// (RefreshPullRequestHead, through headAttribution and attributeHead, the one path that reads the
// commit messages from GitHub, and only where it must), one stored pull request's pick
// (resolveStoredIssueKey), and the stored rows' pick in SQL (AttributePullRequests).
func attributeEachWay(t *testing.T, ctx context.Context, pool *store.Pool, number int, pull firstSourcePull) firstSourcePicks {
	t.Helper()
	url := "https://github.com/acme/gadgets/pull/" + strconv.Itoa(number)
	if pull.linkedBy != "" {
		if _, err := pool.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
			on conflict do nothing`, pull.linkedBy, url); err != nil {
			t.Fatalf("link %s to %s: %v", pull.linkedBy, url, err)
		}
	}
	var picks firstSourcePicks

	fake := newFakeGitHub(t)
	commitReads := serveFirstSourcePull(t, fake, number, pull)
	if err := unrenamed().RefreshPullRequestHead(ctx, pool, fake.newTestClient(), nil, "acme/gadgets", number, time.Now()); err != nil {
		t.Fatalf("refresh #%d: %v", number, err)
	}
	head := readHead(t, ctx, pool, "acme/gadgets", number)
	picks.head = firstSourcePick{key: creditedTo(head.IssueKey), source: attributionSource(head.AttributionSource)}
	picks.commitReads = commitReads.Load()

	facts := attributionFacts{
		Repo: "acme/gadgets", URL: url, Title: pull.title, Body: pull.body, HeadRef: pull.branch,
		CommitMessages: pull.commits,
	}
	key, source := resolvedKey(t, ctx, pool, facts)
	picks.stored = firstSourcePick{key: key, source: source}

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
	var batch *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = 'acme/gadgets' and number = $1`,
		number).Scan(&batch); err != nil {
		t.Fatalf("read the stored pull request #%d: %v", number, err)
	}
	picks.batch = creditedTo(batch)
	return picks
}

// headSetHolds reports whether issue's head set (HeadsAttributedTo, what the results read compares
// a before_merge check with) holds pull request number of acme/gadgets.
func headSetHolds(t *testing.T, ctx context.Context, pool *store.Pool, issue string, number int) bool {
	t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(ctx, pool, issue, HeadAnswers{})
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	for _, h := range heads {
		if h.Repo == "acme/gadgets" && h.Number == number {
			return true
		}
	}
	return false
}

// firstSourceCase is one pull request, as pull request number of acme/gadgets, and what each of
// the three places the pick is made must conclude about it: the issue ("" for nobody) and the
// source that named it, and whether the refresh reads the commit messages, which it does only
// where no source above them names an issue.
type firstSourceCase struct {
	name         string
	number       int
	pull         firstSourcePull
	want         string
	from         attributionSource
	readsCommits bool
}

// wantAttributedEachWay runs tc through the three places the pick is made (attributeEachWay) and
// fails where any of them credits another issue than tc.want, or names another source than tc.from
// (the batch stores no source), or where the refresh reads the commit messages when
// tc.readsCommits says it must not, or the other way round. It also fails where the head set of an
// issue of acme/gadgets (headSetHolds) holds the pull request though the issue is not credited, or
// lacks it though it is: that set is what a before_merge check of the issue is compared with. An
// issue of another repository is left out, since its head set also holds a pull request of
// acme/gadgets that it links (linkedPullRequestTerms).
func wantAttributedEachWay(t *testing.T, ctx context.Context, pool *store.Pool, issues firstSourceIssues, tc firstSourceCase) {
	t.Helper()
	picks := attributeEachWay(t, ctx, pool, tc.number, tc.pull)
	want := firstSourcePick{key: tc.want, source: tc.from}
	if picks.head != want {
		t.Errorf("head row: #%d is credited to %q from %q, want %q from %q", tc.number, picks.head.key, picks.head.source, want.key, want.source)
	}
	if picks.stored != want {
		t.Errorf("one stored pull request: #%d is credited to %q from %q, want %q from %q", tc.number, picks.stored.key, picks.stored.source, want.key, want.source)
	}
	if picks.batch != tc.want {
		t.Errorf("the stored rows' batch: #%d is credited to %q, want %q", tc.number, picks.batch, tc.want)
	}
	if reads := picks.commitReads > 0; reads != tc.readsCommits {
		t.Errorf("the refresh read #%d's commits %d times, want reads %v: they are read only where no source above them names an issue",
			tc.number, picks.commitReads, tc.readsCommits)
	}
	for _, issue := range []string{issues.own, issues.bystander} {
		if holds, credited := headSetHolds(t, ctx, pool, issue, tc.number), issue == tc.want; holds != credited {
			t.Errorf("%s's head set holds #%d: %v, want %v, since #%d is credited to %q", issue, tc.number, holds, credited, tc.number, tc.want)
		}
	}
}

// TestAttributionStopsAtTheFirstSourceThatNamesAnIssue: each case the rule makes, through each of
// the three places the pick is made.
//
// The rows from 401 hold the rule's second half: a `Dispatch:` trailer line is the pull request's
// own statement of its issue, so within one source it outranks a key the same source only
// mentions. Stopping at the first source that names an issue left the mention free to win inside
// that source: a body whose trailer names an issue of another repository and whose prose names an
// issue of this one was credited to the issue the prose names, which is row 302's failure inside
// one source, and the commit messages did the same with a merge commit's prose above the last
// commit's trailer.
func TestAttributionStopsAtTheFirstSourceThatNamesAnIssue(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issues := seedFirstSourceIssues(t, ctx, pool)
	other, own, bystander := issues.other, issues.own, issues.bystander

	for _, tc := range []firstSourceCase{
		{
			name:   "the first source names an issue of this repository, which wins",
			number: 301,
			pull: firstSourcePull{
				title: "feat: the work", body: "Dispatch: " + own, branch: "topic",
				commits: []string{"Keep the " + bystander + " rows", "Do the work\n\nDispatch: " + own},
			},
			want: own, from: sourceTitleBody,
		},
		{
			// The shape of a production pull request rules 1 to 8 credited to an issue only its
			// merge commit mentions: they passed the declared issue over for its repository.
			name:   "the first source names only an issue of another repository, so nobody is credited and the walk stops",
			number: 302,
			pull: firstSourcePull{
				title: "feat: the work", body: "Dispatch: " + other, branch: "topic",
				commits: []string{"Keep the " + bystander + " rows", "Do the work\n\nDispatch: " + other},
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
				commits: []string{"wip", "Do the work\n\nDispatch: " + own},
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
		{
			name:   "the body's trailer names an issue of another repository, its prose one of this repository",
			number: 401,
			pull: firstSourcePull{
				title: "feat: the work", body: "Dispatch: " + other + "\n\nKeeps the " + bystander + " rows.", branch: "topic",
			},
			want: "", from: sourceNone,
		},
		{
			name:   "a merge commit's prose names an issue of this repository, the last commit's trailer the pull request's own",
			number: 407,
			pull: firstSourcePull{
				title: "feat: the work", branch: "topic",
				commits: []string{"Merge main\n\nKeep the " + bystander + " rows.", "Do the work\n\nDispatch: " + own},
			},
			want: own, from: sourceCommitMessage, readsCommits: true,
		},
		{
			name:   "a merge commit's prose names an issue of this repository, the last commit's trailer one of another repository",
			number: 408,
			pull: firstSourcePull{
				title: "feat: UTF-8", body: "See GDGT-9999.", branch: "topic",
				commits: []string{"Keep the " + bystander + " rows.", "Do the work\n\nDispatch: " + other},
			},
			want: "", from: sourceNone, readsCommits: true,
		},
		{
			name:   "a commit subject that begins with the word dispatch is no trailer",
			number: 403,
			pull: firstSourcePull{
				title: "feat: the work", branch: "topic",
				commits: []string{"dispatch: keep the " + bystander + " rows", "Do the work\n\nDispatch: " + own},
			},
			want: own, from: sourceCommitMessage, readsCommits: true,
		},
		{
			name:   "the title mentions an unrelated issue of this repository, the body's trailer names the pull request's own",
			number: 402,
			pull:   firstSourcePull{title: "Follow " + bystander, body: "Dispatch: " + own, branch: "topic"},
			want:   own, from: sourceTitleBody,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantAttributedEachWay(t, ctx, pool, issues, tc)
		})
	}
}

// TestATruncatedEnvelopeBodyLeavesTheAttributionToTheReconcile: a `pull_request` envelope carries
// at most 2048 runes of the body and says so in body_truncated, so a trailer past the cut is not in
// it. Under the first-source walk a mention before the cut would decide for good: an unrelated
// issue of this repository would be credited, or an issue of another repository would credit
// nobody, and the write would be stamped read at this build's rule, so no later pass would look
// again. The envelope therefore attributes nothing from a cut body and leaves the row unread, and
// the reconcile's own read of the whole pull request decides.
func TestATruncatedEnvelopeBodyLeavesTheAttributionToTheReconcile(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issues := seedFirstSourceIssues(t, ctx, pool)
	for _, tc := range []struct {
		name    string
		number  int
		mention string
	}{
		{"the kept part mentions an unrelated issue of this repository", 411, issues.bystander},
		{"the kept part mentions an issue of another repository", 412, issues.other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := "Keeps the " + tc.mention + " rows.\n\n" + strings.Repeat("Evidence. ", 300) + "\n\nDispatch: " + issues.own
			kept := string([]rune(full)[:2048])
			url := "https://github.com/acme/gadgets/pull/" + strconv.Itoa(tc.number)
			fake := newFakeGitHub(t)
			serveFirstSourcePull(t, fake, tc.number, firstSourcePull{title: "feat: the work", body: full, branch: "topic"})
			client := fake.newTestClient()
			intake := newIntake(tuningFor(t), nil, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), nil, unrenamed())
			if _, err := intake.recordPullRequestHead(ctx, map[string]string{
				"kind": "pr", "action": "opened", "merged": "false", "repo": "acme/gadgets", "number": strconv.Itoa(tc.number),
				"title": "feat: the work", "body": kept, "body_truncated": "true", "head_ref": "topic",
				"head_repo": "acme/gadgets", "head_sha": firstSourceHead(tc.number), "base_ref": "main", "url": url,
				"updated_at": "2026-11-28T12:57:00Z",
			}, tc.number); err != nil {
				t.Fatalf("record the head the envelope reports: %v", err)
			}
			fromEnvelope := readHead(t, ctx, pool, "acme/gadgets", tc.number)
			if fromEnvelope.IssueKey != nil || fromEnvelope.CheckedAt != nil {
				t.Errorf("the envelope wrote #%d credited to %q and read at %v, want nobody and no read time: its body was cut before its trailer",
					tc.number, creditedTo(fromEnvelope.IssueKey), fromEnvelope.CheckedAt)
			}
			NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
			if after := readHead(t, ctx, pool, "acme/gadgets", tc.number); creditedTo(after.IssueKey) != issues.own {
				t.Fatalf("after a reconcile pass #%d is credited to %q from %q, want %s from the whole body's trailer",
					tc.number, creditedTo(after.IssueKey), after.AttributionSource, issues.own)
			}
		})
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
			issues := seedFirstSourceIssues(t, ctx, pool)
			const number = 4028
			pull := firstSourcePull{
				title: "gadgets: one scan exception row", body: "Dispatch: " + issues.other,
				branch: "agent/licence-rows",
				commits: []string{
					"Merge main\n\nKeep every main exception row, including the " + issues.bystander + " rows.",
					"Do the work\n\nDispatch: " + issues.other,
				},
				merged: state == "merged",
			}
			fake := newFakeGitHub(t)
			serveFirstSourcePull(t, fake, number, pull)

			// The row as rule 8 left it: credited to the mentioned issue, read a day ago.
			read := time.Now().Add(-24 * time.Hour)
			broker := events.NewBroker()
			if err := UpsertPullRequestHead(ctx, pool, broker, PullRequestHead{
				Repo: "acme/gadgets", Number: number, HeadSHA: firstSourceHead(number), HeadRef: pull.branch, BaseRef: "main",
				State: state, IssueKey: &issues.bystander, AttributionSource: string(sourceCommitMessage), AttributionComplete: true,
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
			if headSetHolds(t, ctx, pool, issues.bystander, number) {
				t.Fatalf("%s's head set still holds #4028 after the pass", issues.bystander)
			}
			left := attributionEvents(t, ctx, pool, issues.bystander)
			if len(left) != 2 || left[1].Payload.From == nil || *left[1].Payload.From != issues.bystander || left[1].Payload.To != nil {
				t.Fatalf("%s carries %+v, want the event that credited it and one that leaves it credited to nobody", issues.bystander, left)
			}
		})
	}
}
