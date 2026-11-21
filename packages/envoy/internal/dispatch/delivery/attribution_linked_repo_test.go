package delivery

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// A pull request that an issue links by its URL is that issue's pull request in any repository
// (R19 of the spec of several open pull requests, by R1 of the spec of a pull request's issue: the
// issue is the one the current rule gives). ATTR-871 resolves to acme/widgets and links
// acme/tools#9. The statement gives #9 to ATTR-871 from the link, so the results and the timeline
// name one issue for it: PullRequestIssues answers ATTR-871 from external_link, HeadsAttributedTo
// lists #9 naming acme/tools, ListPullRequestsOfIssues lists its timeline row, and
// BaseBranchMergedIn answers the branch the merged #9 went into, where BaseBranchMergedFor, which
// reads the issue's own repository, answers none. Two controls of the rule's other halves: #10,
// whose title names ATTR-871 and which no link names, has no issue (a name is held to the pull
// request's repository), and #11, which ATTR-872 links and whose head is in a fork, has none either.
func TestAPullRequestAnIssueLinksInAnotherRepositoryIsItsOwnInTheResultsAndTheTimeline(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-871", "ATTR-872"}, map[string][]string{
		"ATTR-871": {"https://github.com/acme/tools/pull/9"},
		"ATTR-872": {"https://github.com/acme/tools/pull/11"},
	})
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	store := func(number int, state, title, headRepo, baseRef string) {
		t.Helper()
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: "acme/tools", Number: number, HeadSHA: fmt.Sprintf("%040x", 9000+number), HeadRef: "topic", BaseRef: baseRef,
			State: state, Title: title, URL: fmt.Sprintf("https://github.com/acme/tools/pull/%d", number), EventAt: &at,
			Texts: &PullRequestTexts{HeadRepo: headRepo, BaseRef: baseRef, Title: title, CommitMessages: []string{}, MessagesRead: true},
		}); err != nil {
			t.Fatalf("store acme/tools#%d: %v", number, err)
		}
		merged := at
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/tools", Number: number, Title: title, Author: "octocat",
			URL: fmt.Sprintf("https://github.com/acme/tools/pull/%d", number), MergedAt: &merged, CreatedAt: &merged,
		}); err != nil {
			t.Fatalf("store the timeline row of acme/tools#%d: %v", number, err)
		}
	}
	store(9, "merged", "A change", "acme/tools", "release")
	store(10, "merged", "ATTR-871 named in the title", "acme/tools", "main")
	store(11, "merged", "A forked change", "someone/tools", "main")

	keys := []PullRequestKey{{Repo: "acme/tools", Number: 9}, {Repo: "acme/tools", Number: 10}, {Repo: "acme/tools", Number: 11}}
	answers, err := PullRequestIssues(ctx, pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	issueOf := func(number int) string {
		answer := answers[PullRequestKey{Repo: "acme/tools", Number: number}]
		if answer.IssueKey == nil {
			return "none"
		}
		return *answer.IssueKey + " from " + answer.Source
	}
	t.Logf("#9=%s #10=%s #11=%s", issueOf(9), issueOf(10), issueOf(11))
	if got := issueOf(9); got != "ATTR-871 from external_link" {
		t.Errorf("acme/tools#9, which ATTR-871 links, answers %s; want ATTR-871 from external_link: a link holds in any repository", got)
	}
	if got := issueOf(10); got != "none" {
		t.Errorf("control: acme/tools#10, whose title names ATTR-871 of acme/widgets, answers %s; want none: a name in a title is held to the pull request's repository", got)
	}
	if got := issueOf(11); got != "none" {
		t.Errorf("control: acme/tools#11, which ATTR-872 links and whose head is in a fork, answers %s; want none", got)
	}

	heads, err := HeadsAttributedTo(ctx, pool, "ATTR-871")
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 1 || heads[0].Number != 9 || heads[0].LinkedRepo != "acme/tools" || !heads[0].HeadKnown {
		t.Errorf("HeadsAttributedTo(ATTR-871) = %+v; want #9 alone, known, naming acme/tools", heads)
	}
	timeline, err := ListPullRequestsOfIssues(ctx, pool, []string{"ATTR-871"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(timeline, func(pr DeliveryPullRequest) bool { return pr.Repo == "acme/tools" && pr.Number == 9 }) || len(timeline) != 1 {
		t.Errorf("the timeline of ATTR-871 lists %+v; want acme/tools#9 alone, the issue the results name", timeline)
	}
	if branch, err := BaseBranchMergedIn(ctx, pool, "ATTR-871", "acme/tools"); err != nil || branch != "release" {
		t.Errorf("BaseBranchMergedIn(ATTR-871, acme/tools) = %q, %v; want release, the branch #9 merged into", branch, err)
	}
	if branch, err := BaseBranchMergedFor(ctx, pool, "ATTR-871"); err != nil || branch != "" {
		t.Errorf("BaseBranchMergedFor(ATTR-871) = %q, %v; want none: it reads acme/widgets, the issue's own repository, alone", branch, err)
	}
}
