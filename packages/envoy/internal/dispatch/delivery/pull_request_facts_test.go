package delivery

// What a read of GitHub stores for a pull request: its facts, the results row and its texts row,
// and no issue. Every reader computes the issue from the stored facts at each read
// (PullRequestIssues). A writer pages the commit messages only where no higher source names a held
// issue, and a failed read of them leaves them not read, so the answer is not known until a later
// read stores them.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// setMessages makes GitHub answer the commits of pull request number with one commit for each
// of messages.
func (g *githubPulls) setMessages(number int, messages ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.messages == nil {
		g.messages = map[int][]string{}
	}
	g.messages[number] = messages
}

// failCommits makes GitHub answer every read of the commits with a 500.
func (g *githubPulls) failCommits() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commitsFailing = true
}

// edit changes GitHub's answer for pull request number, which set made.
func (g *githubPulls) edit(number int, change func(pull map[string]any)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g.pulls[number])
}

// serveCommitPulls answers GET /repos/{repo}/commits/{sha}/pulls with each pull request g holds
// whose head is sha, as GitHub names the pull requests of a commit.
func serveCommitPulls(t *testing.T, fake *fakeGitHub, g *githubPulls, repo string) {
	t.Helper()
	fake.handle("GET /repos/"+repo+"/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		pulls := []any{}
		for _, number := range slices.Sorted(maps.Keys(g.pulls)) {
			pull := g.pulls[number]
			if head, _ := pull["head"].(map[string]any); head["sha"] == r.PathValue("sha") {
				pulls = append(pulls, maps.Clone(pull))
			}
		}
		g.mu.Unlock()
		mustEncode(t, w, pulls)
	})
}

// storedTexts is one pull_request_texts row as a test reads it. messagesRead is false where
// commit_messages is null.
type storedTexts struct {
	headSHA, headRepo, baseRef, title, body    string
	messages                                   []string
	messagesRead                               bool
	updatedAt                                  *time.Time
	titleKeys, citedIssues, branchKeys, commit []string
}

// storedTextsOf reads the texts row of repo#number, and found false where there is none.
func storedTextsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) (texts storedTexts, found bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `
		select head_sha, head_repo, base_ref, title, body, coalesce(commit_messages, '{}'), commit_messages is not null,
			updated_at, title_keys, cited_issues, branch_keys, commit_keys
		from pull_request_texts where repo = $1 and number = $2
	`, repo, number).Scan(&texts.headSHA, &texts.headRepo, &texts.baseRef, &texts.title, &texts.body, &texts.messages,
		&texts.messagesRead, &texts.updatedAt, &texts.titleKeys, &texts.citedIssues, &texts.branchKeys, &texts.commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedTexts{}, false
	}
	if err != nil {
		t.Fatalf("read the texts of %s#%d: %v", repo, number, err)
	}
	return texts, true
}

// storedIssueOf is the issue_key column of the results row of repo#number: what a write or a
// fixture stored, which no reader answers for a pull request with current texts.
func storedIssueOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) *string {
	t.Helper()
	var issue *string
	if err := pool.QueryRow(ctx, `select issue_key from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&issue); err != nil {
		t.Fatalf("read the stored issue of %s#%d: %v", repo, number, err)
	}
	return issue
}

// describeIssue is issue as a failure names it, "nobody" for nil.
func describeIssue(issue *string) string {
	if issue == nil {
		return "nobody"
	}
	return *issue
}

// refreshOrFail reads repo#number through RefreshPullRequestHead, and stops the test where the
// read fails or GitHub refuses it.
func refreshOrFail(t *testing.T, ctx context.Context, pool *store.Pool, client *githubapp.Client, repo string, number int) *FetchedFacts {
	t.Helper()
	fetched, refused, err := RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now())
	if err != nil || refused != nil {
		t.Fatalf("refresh %s#%d: refused %v, error %v", repo, number, refused, err)
	}
	return fetched
}

// Each writer of GitHub's answer stores the pull request's texts and no issue: the envelope's
// handler (here one whose envelope names no head branch), the reconcile's refresh and a result's
// read-through. #31's branch is legion/ACME-2 and its body names ACME-1 first. Its row holds
// ACME-1, the verdict of an older rule. After each writer the texts row holds GitHub's body, the
// branch's keys are {ACME-2}, and the row's issue_key is still ACME-1. The statement answers
// ACME-2 from the branch, and no writer read a page of commits: the branch decides.
func TestEveryWriterStoresGitHubsTextsAndNoIssue(t *testing.T) {
	const number = 31
	body := "This follows ACME-1 and finishes ACME-2."
	head := fakeSHA("c", number)
	for _, writer := range []struct {
		name  string
		write func(ctx context.Context, pool *store.Pool, client *githubapp.Client, second time.Time) error
	}{
		{name: "an_envelope_that_names_no_head_branch", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, second time.Time) error {
			envelope := prEnvelope(statementRepo, number, "edited", head, "", "", second)
			delete(envelope, "head_ref")
			_, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil, envelope)
			return err
		}},
		{name: "the_refresh", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ time.Time) error {
			_, _, err := RefreshPullRequestHead(ctx, pool, client, nil, statementRepo, number, time.Now())
			return err
		}},
		{name: "the_read_through", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ time.Time) error {
			_, _, err := ReadThroughCommitPulls(ctx, pool, client, statementRepo, head, time.Now())
			return err
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			statementIssues(t, ctx, pool, "ACME-1", "ACME-2")
			earlier := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
			if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: statementRepo, Number: number,
				HeadSHA: fakeSHA("b", number), HeadRef: "legion/ACME-2", BaseRef: "main", State: "open", EventAt: &earlier}); err != nil {
				t.Fatalf("store the row of an older release: %v", err)
			}
			seedStoredIssue(t, ctx, pool, statementRepo, number, new("ACME-1"), string(sourceTitleBody))
			second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, statementRepo)
			github.set(statementRepo, number, "open", head, "legion/ACME-2", body, second)
			serveCommitPulls(t, fake, github, statementRepo)
			if err := writer.write(ctx, pool, fake.newTestClient(), second); err != nil {
				t.Fatalf("the write: %v", err)
			}
			texts, found := storedTextsOf(t, ctx, pool, statementRepo, number)
			if !found || texts.headSHA != head || texts.body != body {
				t.Errorf("the texts of #%d = %+v (found %v), want GitHub's body %q at %.8s", number, texts, found, body, head)
			}
			if fmt.Sprint(texts.branchKeys) != "[ACME-2]" {
				t.Errorf("the branch keys of #%d = %v, want [ACME-2]", number, texts.branchKeys)
			}
			if stored := storedIssueOf(t, ctx, pool, statementRepo, number); describeIssue(stored) != "ACME-1" {
				t.Errorf("issue_key of #%d = %s after the write, want ACME-1 as the row held it: a writer names no issue", number, describeIssue(stored))
			}
			wantAnswer(t, ctx, pool, statementRepo, number, "ACME-2 from branch")
			wantKnown(t, ctx, pool, statementRepo, number, true)
			if pages := github.commitReads.Load(); pages != 0 {
				t.Errorf("the write read %d pages of commits, want 0: the branch names a held issue", pages)
			}
		})
	}
}

// A writer pages the commit messages only where no higher source names a held issue. #33's only
// key is in its commit messages: a new head pages them once, and the same head again pages none,
// since the stored texts hold the messages of that head and base. A later head pages them once
// more. #34's branch names a held issue, so its messages are never paged.
func TestAWriterReadsTheMessagesOnlyWhereNoHigherSourceDecides(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-4", "ACME-5")
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set(statementRepo, 33, "open", fakeSHA("a", 33), "work/none", "", second)
	github.setMessages(33, "Work.\n\nDispatch: ACME-4")
	github.set(statementRepo, 34, "open", fakeSHA("a", 34), "legion/ACME-5", "", second)
	github.setMessages(34, "Work.\n\nDispatch: ACME-4")
	client := fake.newTestClient()
	pagesOf := func(number int) int32 {
		before := github.commitReads.Load()
		refreshOrFail(t, ctx, pool, client, statementRepo, number)
		return github.commitReads.Load() - before
	}
	if pages := pagesOf(33); pages != 1 {
		t.Errorf("the first read of #33 read %d pages of commits, want 1", pages)
	}
	wantAnswer(t, ctx, pool, statementRepo, 33, "ACME-4 from commit_message")
	if pages := pagesOf(33); pages != 0 {
		t.Errorf("a second read of #33 at the same head read %d pages of commits, want 0: the stored texts hold its messages", pages)
	}
	wantAnswer(t, ctx, pool, statementRepo, 33, "ACME-4 from commit_message")
	github.set(statementRepo, 33, "open", fakeSHA("b", 33), "work/none", "", second.Add(time.Minute))
	if pages := pagesOf(33); pages != 1 {
		t.Errorf("the read of #33 at a new head read %d pages of commits, want 1", pages)
	}
	wantAnswer(t, ctx, pool, statementRepo, 33, "ACME-4 from commit_message")
	if pages := pagesOf(34); pages != 0 {
		t.Errorf("the read of #34 read %d pages of commits, want 0: its branch names a held issue", pages)
	}
	wantAnswer(t, ctx, pool, statementRepo, 34, "ACME-5 from branch")
}

// A pull request with no key in its branch, title or body is known in the first answer after its
// event: the intake writes the texts and then pages the commit messages in the same handling.
// acme/widgets#39 opens on work/no-key, and its commit message names ACME-4, which is held. After
// the intake handles the envelope of its open, the first answer is ACME-4 from the commit message,
// known, and one page of commits was read. A second envelope moves the head to a commit of the same
// kind: the same answer after that handling, and one more page. Control in the same test: where
// GitHub answers 500 to the commits, the answer is not known after each event.
func TestAKeylessPullRequestIsKnownInTheFirstAnswerAfterItsEvent(t *testing.T) {
	const repo, number = "acme/widgets", 39
	for _, failing := range []bool{false, true} {
		name := map[bool]string{false: "the_commits_answer", true: "control_the_commits_answer_500"}[failing]
		t.Run(name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			seedProjectRepository(t, ctx, pool, "ACME", "ACME", repo)
			insertProjectIssue(t, ctx, pool, "ACME", "ACME-4", 4)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, repo)
			if failing {
				github.failCommits()
			}
			intake := newIntake(nil, pool, nil, fake.newTestClient(), nil, nil)
			handle := func(action, head string, at time.Time) int32 {
				github.set(repo, number, "open", head, "work/no-key", "", at)
				github.setMessages(number, "Work.\n\nDispatch: ACME-4")
				before := github.commitReads.Load()
				if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope(repo, number, action, head, "work/no-key", "", at)); err != nil {
					t.Fatalf("handle the %s envelope of #%d: %v", action, number, err)
				}
				return github.commitReads.Load() - before
			}
			opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			for i, event := range []struct {
				action, head string
				at           time.Time
			}{{"opened", fakeSHA("a", number), opened}, {"synchronize", fakeSHA("b", number), opened.Add(time.Minute)}} {
				pages := handle(event.action, event.head, event.at)
				if failing {
					wantAnswer(t, ctx, pool, repo, number, "nobody from none, no facts")
					wantKnown(t, ctx, pool, repo, number, false)
					continue
				}
				wantAnswer(t, ctx, pool, repo, number, "ACME-4 from commit_message")
				wantKnown(t, ctx, pool, repo, number, true)
				if pages != 1 {
					t.Errorf("event %d (%s) read %d pages of commits, want 1", i+1, event.action, pages)
				}
			}
		})
	}
}

// A fork's head reads no commits: a pull request whose head is in another repository is nobody's
// whatever its texts name, so its messages are stored as read and empty, and it is known as
// nobody's. Its branch, body and commit message all name a held issue.
func TestAForksHeadReadsNoCommits(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-6")
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set(statementRepo, 41, "open", fakeSHA("a", 41), "legion/ACME-6", "Fixes ACME-6.", second)
	github.edit(41, func(pull map[string]any) {
		pull["head"].(map[string]any)["repo"] = map[string]any{"full_name": "fork/stmt"}
	})
	github.setMessages(41, "Work.\n\nDispatch: ACME-6")
	refreshOrFail(t, ctx, pool, fake.newTestClient(), statementRepo, 41)
	if pages := github.commitReads.Load(); pages != 0 {
		t.Errorf("the read of a fork's pull request read %d pages of commits, want 0", pages)
	}
	texts, found := storedTextsOf(t, ctx, pool, statementRepo, 41)
	if !found || !texts.messagesRead || len(texts.messages) != 0 || texts.headRepo != "fork/stmt" {
		t.Errorf("the texts of the fork's pull request = %+v (found %v), want the head repository fork/stmt and messages read and empty", texts, found)
	}
	wantAnswer(t, ctx, pool, statementRepo, 41, "nobody from none")
	wantKnown(t, ctx, pool, statementRepo, 41, true)
	wantListed(t, ctx, pool, "ACME-6")
}

// A failed read of the commit messages stores every other part of the facts (B4). GitHub answers
// 500 to every read of the commits. #31 has a row before, which stores ACME-3, and its only key is
// in its commit messages. A read of its new head writes the new head and a texts row of that head
// with the messages not read. Its texts are current and no higher source decides, so the answer is
// not known: nobody, no facts, and not the stored ACME-3, which stays on the row. #32 has no row
// before, and its branch names ACME-2: it is ACME-2's from the branch, and its read pages nothing.
func TestACommitReadFailureStoresEveryOtherPartOfTheFacts(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-2", "ACME-3")
	earlier := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: statementRepo, Number: 31,
		HeadSHA: fakeSHA("a", 31), HeadRef: "work/none", BaseRef: "main", State: "open", EventAt: &earlier}); err != nil {
		t.Fatalf("store #31 before: %v", err)
	}
	seedStoredIssue(t, ctx, pool, statementRepo, 31, new("ACME-3"), string(sourceCommitMessage))
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	github.failCommits()
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newHead := fakeSHA("b", 31)
	github.set(statementRepo, 31, "open", newHead, "work/none", "", second)
	github.setMessages(31, "Work.\n\nDispatch: ACME-3")
	github.set(statementRepo, 32, "open", fakeSHA("a", 32), "legion/ACME-2", "", second)
	client := fake.newTestClient()

	refreshOrFail(t, ctx, pool, client, statementRepo, 31)
	if got := readHead(t, ctx, pool, statementRepo, 31); got.HeadSHA != newHead {
		t.Errorf("the results row of #31 holds %.8s after the read, want the new head %.8s", got.HeadSHA, newHead)
	}
	texts, found := storedTextsOf(t, ctx, pool, statementRepo, 31)
	if !found || texts.headSHA != newHead || texts.messagesRead {
		t.Errorf("the texts of #31 = %+v (found %v), want the new head %.8s with commit_messages null", texts, found, newHead)
	}
	wantAnswer(t, ctx, pool, statementRepo, 31, "nobody from none, no facts")
	wantKnown(t, ctx, pool, statementRepo, 31, false)
	if stored := storedIssueOf(t, ctx, pool, statementRepo, 31); describeIssue(stored) != "ACME-3" {
		t.Errorf("issue_key of #31 = %s, want ACME-3 as the row held it", describeIssue(stored))
	}
	wantListed(t, ctx, pool, "ACME-3")

	before := github.commitReads.Load()
	refreshOrFail(t, ctx, pool, client, statementRepo, 32)
	if pages := github.commitReads.Load() - before; pages != 0 {
		t.Errorf("the first read of #32 read %d pages of commits, want 0: its branch names a held issue", pages)
	}
	wantAnswer(t, ctx, pool, statementRepo, 32, "ACME-2 from branch")
	wantKnown(t, ctx, pool, statementRepo, 32, true)
}

// An edited body replaces the stored texts: GitHub moves updated_at, and the next read writes the
// new body. The keys of the title and the body follow it, and so does the answer.
func TestAnEditedBodyReplacesTheStoredTexts(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-1", "ACME-2")
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	client := fake.newTestClient()
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	head := fakeSHA("a", 31)
	for i, edit := range []struct {
		body string
		at   time.Time
		keys string
	}{{"Fixes ACME-1.", first, "[ACME-1]"}, {"Fixes ACME-2.", first.Add(time.Minute), "[ACME-2]"}} {
		github.set(statementRepo, 31, "open", head, "work/none", edit.body, edit.at)
		refreshOrFail(t, ctx, pool, client, statementRepo, 31)
		texts, found := storedTextsOf(t, ctx, pool, statementRepo, 31)
		if !found || texts.body != edit.body || fmt.Sprint(texts.titleKeys) != edit.keys ||
			texts.updatedAt == nil || !texts.updatedAt.Equal(edit.at) {
			t.Errorf("read %d: the texts of #31 = %+v (found %v), want the body %q, title keys %s and updated_at %s",
				i+1, texts, found, edit.body, edit.keys, edit.at.Format(time.RFC3339))
		}
	}
	wantAnswer(t, ctx, pool, statementRepo, 31, "ACME-2 from title_body")
}

// RefreshPullRequestHead answers what it fetched and stored: for #31 a FetchedFacts of GitHub's
// answer, whose texts equal the stored texts row. A read GitHub refuses answers no facts and the
// refusal.
func TestARefreshAnswersWhatItFetchedAndStored(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-4")
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set(statementRepo, 31, "open", fakeSHA("a", 31), "work/none", "A body.", second)
	github.setMessages(31, "Work.\n\nDispatch: ACME-4")
	client := fake.newTestClient()

	fetched := refreshOrFail(t, ctx, pool, client, statementRepo, 31)
	if fetched == nil || fetched.Texts == nil {
		t.Fatalf("the refresh of #31 answered %+v, want the facts it fetched", fetched)
	}
	if fetched.Payload.Number != 31 || fetched.Canonical != statementRepo || fetched.Owner != "acme" || fetched.Repo != "stmt" || fetched.Token == "" {
		t.Errorf("the refresh of #31 answered the read %s (%s/%s) #%d with token %q, want %s #31 with a token",
			fetched.Canonical, fetched.Owner, fetched.Repo, fetched.Payload.Number, fetched.Token, statementRepo)
	}
	stored, found := storedTextsOf(t, ctx, pool, statementRepo, 31)
	answered := storedTexts{headRepo: fetched.Texts.HeadRepo, baseRef: fetched.Texts.BaseRef, title: fetched.Texts.Title,
		body: fetched.Texts.Body, messages: fetched.Texts.CommitMessages, messagesRead: fetched.Texts.MessagesRead}
	kept := storedTexts{headRepo: stored.headRepo, baseRef: stored.baseRef, title: stored.title, body: stored.body,
		messages: stored.messages, messagesRead: stored.messagesRead}
	if !found || fmt.Sprint(answered) != fmt.Sprint(kept) || !answered.messagesRead {
		t.Errorf("the refresh answered the texts %+v, and the texts row holds %+v (found %v): want one value, with the messages read", answered, kept, found)
	}

	fetched, refused, err := RefreshPullRequestHead(ctx, pool, client, nil, statementRepo, 404, time.Now())
	if err != nil || refused == nil || fetched != nil {
		t.Errorf("the refresh of #404, which GitHub does not have, answered %+v, refused %v, error %v; want no facts and the refusal", fetched, refused, err)
	}
}

// A write that the event-time guard refuses writes no texts (review 3, note): the stored row of
// #35 is GitHub's answer of a later updated_at than the payload's. A refresh answers that older
// payload with another head and title, and both rows stay as they were. Control in the same test:
// the same refresh with a later updated_at replaces both.
func TestAWriteThatTheGuardRefusesWritesNoTexts(t *testing.T) {
	for _, later := range []bool{false, true} {
		name := map[bool]string{false: "an_older_answer", true: "control_a_later_answer"}[later]
		t.Run(name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			statementIssues(t, ctx, pool, "ACME-7")
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, statementRepo)
			client := fake.newTestClient()
			stamped := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			first, other := fakeSHA("a", 35), fakeSHA("b", 35)
			github.set(statementRepo, 35, "open", first, "legion/ACME-7", "", stamped)
			refreshOrFail(t, ctx, pool, client, statementRepo, 35)

			at := stamped.Add(-time.Minute)
			if later {
				at = stamped.Add(time.Minute)
			}
			github.set(statementRepo, 35, "open", other, "legion/ACME-7", "", at)
			github.edit(35, func(pull map[string]any) { pull["title"] = "Another title" })
			refreshOrFail(t, ctx, pool, client, statementRepo, 35)
			head := readHead(t, ctx, pool, statementRepo, 35)
			texts, found := storedTextsOf(t, ctx, pool, statementRepo, 35)
			want, title, updated := first, "Change 35", stamped
			if later {
				want, title, updated = other, "Another title", at
			}
			if head.HeadSHA != want || !found || texts.headSHA != want || texts.title != title || texts.updatedAt == nil || !texts.updatedAt.Equal(updated) {
				t.Errorf("after an answer of %s the results row holds %.8s and the texts %+v (found %v); want %.8s, the title %q and updated_at %s",
					at.Format(time.RFC3339), head.HeadSHA, texts, found, want, title, updated.Format(time.RFC3339))
			}
		})
	}
}

// A new results row starts with the issue of its timeline row (review 3, note), the answer every
// reader gave before the row existed (R4): the first value a move reads as where it came from
// (T7). #36's timeline row says ATTR-1, and no results row exists. A first write of texts that name
// nothing held, whose commits answer 500, stores ATTR-1 in pull_request_heads.issue_key. The
// texts are current and the messages are not read, so no reader answers that issue: the answer is
// not known. The same holds with the timeline row under the old name of a renamed repository. And
// after a rename where a results row under the old name says ATTR-2 and the timeline row says
// ATTR-1, the re-key starts the row under the new name with ATTR-2, the old row's answer.
func TestANewResultsRowStartsWithItsTimelineIssue(t *testing.T) {
	const repo, old, number = "acme/attr", "acme/attr-old", 36
	for _, kase := range []struct {
		name string
		// setUp stores the rows before the write; readUnder is the name the write reads.
		setUp     func(t *testing.T, ctx context.Context, pool *store.Pool)
		readUnder string
		want      string
	}{
		{name: "a_timeline_row", readUnder: repo, want: "ATTR-1", setUp: func(t *testing.T, ctx context.Context, pool *store.Pool) {
			storeStatementRow(t, ctx, pool, statementRow{repo: repo, number: number, title: "Change 36", noHead: true,
				timeline: &statementTimeline{issue: new("ATTR-1")}})
		}},
		{name: "a_timeline_row_under_the_old_name", readUnder: repo, want: "ATTR-1", setUp: func(t *testing.T, ctx context.Context, pool *store.Pool) {
			storeStatementRow(t, ctx, pool, statementRow{repo: old, number: number, title: "Change 36", noHead: true,
				timeline: &statementTimeline{issue: new("ATTR-1")}})
		}},
		{name: "a_rekey_of_a_results_row_under_the_old_name", readUnder: old, want: "ATTR-2", setUp: func(t *testing.T, ctx context.Context, pool *store.Pool) {
			storeStatementRow(t, ctx, pool, statementRow{repo: old, number: number, branch: "work/none", title: "Change 36",
				stored: new("ATTR-2"), timeline: &statementTimeline{issue: new("ATTR-1")}})
		}},
	} {
		t.Run(kase.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			seedProjectRepository(t, ctx, pool, "ATTR", "ATTR", repo)
			mapRepository(t, ctx, pool, old, "ATTR", false, new(repo))
			insertProjectIssue(t, ctx, pool, "ATTR", "ATTR-1", 1)
			insertProjectIssue(t, ctx, pool, "ATTR", "ATTR-2", 2)
			kase.setUp(t, ctx, pool)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, repo)
			github.failCommits()
			github.set(repo, number, "open", fakeSHA("a", number), "work/none", "", time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
			refreshOrFail(t, ctx, pool, fake.newTestClient(), kase.readUnder, number)
			if stored := storedIssueOf(t, ctx, pool, repo, number); describeIssue(stored) != kase.want {
				t.Errorf("the new results row of %s#%d stores %s, want %s", repo, number, describeIssue(stored), kase.want)
			}
			var oldRows int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1`, old).Scan(&oldRows); err != nil || oldRows != 0 {
				t.Errorf("results rows under %s after the write = %d (%v), want 0", old, oldRows, err)
			}
			wantAnswer(t, ctx, pool, repo, number, "nobody from none, no facts")
			wantKnown(t, ctx, pool, repo, number, false)
			wantListed(t, ctx, pool, "ATTR-1")
			wantListed(t, ctx, pool, "ATTR-2")
		})
	}
}

// A free-form text with a NUL gets facts (BL2, BL3 of review 5). Only the commit message of #37
// names a held issue. Its title, its body and its message each hold a NUL between two characters
// that are not word characters, and GitHub's JSON answers each as \u0000. #38 has the same texts
// without the NULs. After the write no higher source names a held issue, so the writer pages the
// messages: one page for each. Each NUL is stored as U+FFFD, both are ACME-3's from the commit
// message, and the four key lists of #37 equal #38's. Control in the same test: Postgres refuses
// #37's message with its NUL (SQLSTATE 22021). Control in the same test: #40's NUL is inside the
// key, so its commit keys are empty and the comparison of lists can fail.
func TestAFreeFormTextWithANulGetsFacts(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	statementIssues(t, ctx, pool, "ACME-3")
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	client := fake.newTestClient()
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, pull := range []struct {
		number               int
		title, body, message string
	}{
		{37, "Fix:\x00 the notes", "The change.\x00 Read the notes.", "Work.\x00 Dispatch: ACME-3"},
		{38, "Fix: the notes", "The change. Read the notes.", "Work. Dispatch: ACME-3"},
		{40, "Fix: the notes", "The change. Read the notes.", "Work. Dispatch: ACME\x00-3"},
	} {
		github.set(statementRepo, pull.number, "open", fakeSHA("a", pull.number), "work/notes", pull.body, second)
		github.edit(pull.number, func(answer map[string]any) { answer["title"] = pull.title })
		github.setMessages(pull.number, pull.message)
		before := github.commitReads.Load()
		refreshOrFail(t, ctx, pool, client, statementRepo, pull.number)
		if pages := github.commitReads.Load() - before; pages != 1 {
			t.Errorf("the read of #%d read %d pages of commits, want 1", pull.number, pages)
		}
	}
	nul37, found := storedTextsOf(t, ctx, pool, statementRepo, 37)
	if !found || nul37.title != "Fix:\uFFFD the notes" || nul37.body != "The change.\uFFFD Read the notes." ||
		fmt.Sprint(nul37.messages) != "[Work.\uFFFD Dispatch: ACME-3]" {
		t.Errorf("the texts of #37 = %+v (found %v), want U+FFFD where each NUL was", nul37, found)
	}
	if head := readHead(t, ctx, pool, statementRepo, 37); head.HeadSHA != fakeSHA("a", 37) {
		t.Errorf("the results row of #37 holds %.8s, want its head", head.HeadSHA)
	}
	plain38, _ := storedTextsOf(t, ctx, pool, statementRepo, 38)
	wantAnswer(t, ctx, pool, statementRepo, 37, "ACME-3 from commit_message")
	wantAnswer(t, ctx, pool, statementRepo, 38, "ACME-3 from commit_message")
	keysOf := func(texts storedTexts) string {
		return fmt.Sprint(texts.titleKeys, texts.citedIssues, texts.branchKeys, texts.commit)
	}
	if keysOf(nul37) != keysOf(plain38) || fmt.Sprint(nul37.commit) != "[ACME-3]" {
		t.Errorf("the key lists of #37 are %s and of #38 %s, want one value with the commit keys [ACME-3]", keysOf(nul37), keysOf(plain38))
	}
	_, err := pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages)
		values ($1, 37, $2, 'work/notes', $1, 'main', 'Fix:', '', array[$3::text])
		on conflict (repo, number) do update set commit_messages = excluded.commit_messages`,
		statementRepo, fakeSHA("a", 37), "Work.\x00 Dispatch: ACME-3")
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "22021" {
		t.Errorf("an insert of #37's message with its NUL answered %v, want SQLSTATE 22021 (the control)", err)
	}
	inside, _ := storedTextsOf(t, ctx, pool, statementRepo, 40)
	if len(inside.commit) != 0 {
		t.Errorf("the commit keys of #40, whose NUL is inside the key, = %v, want none (the control)", inside.commit)
	}
	wantAnswer(t, ctx, pool, statementRepo, 40, "nobody from none")
}

// A value of identity with a NUL is refused (BL2): a payload whose head branch holds a NUL, and
// one whose head repository does. Each write answers errNoRowHolds, and neither stores a results
// row or a texts row.
func TestAValueOfIdentityWithANulIsRefused(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, statementRepo)
	client := fake.newTestClient()
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	github.set(statementRepo, 42, "open", fakeSHA("a", 42), "work/a\x00b", "", second)
	github.set(statementRepo, 43, "open", fakeSHA("a", 43), "work/none", "", second)
	github.edit(43, func(pull map[string]any) {
		pull["head"].(map[string]any)["repo"] = map[string]any{"full_name": statementRepo + "\x00"}
	})
	for _, number := range []int{42, 43} {
		_, _, err := RefreshPullRequestHead(ctx, pool, client, nil, statementRepo, number, time.Now())
		if !errors.Is(err, errNoRowHolds) {
			t.Errorf("the write of #%d answered %v, want errNoRowHolds", number, err)
		}
		var heads, texts int
		if err := pool.QueryRow(ctx, `select (select count(*) from pull_request_heads where repo = $1 and number = $2),
			(select count(*) from pull_request_texts where repo = $1 and number = $2)`, statementRepo, number).Scan(&heads, &texts); err != nil {
			t.Fatal(err)
		}
		if heads != 0 || texts != 0 {
			t.Errorf("#%d has %d results rows and %d texts rows after the refused write, want none", number, heads, texts)
		}
	}
}
