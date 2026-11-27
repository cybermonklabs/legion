package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What one read of an issue's results spends on GitHub when many of its results stand at commits
// no stored head names: one question for all of them, whatever their number. Before the batched
// read, learn read each such commit through in a call of its own (results_comparison.go), and a
// production read of an issue with 18 such results took longer than two minutes where the same
// read with ?at= took 8.3 s, since ?at= reads nothing through.
//
// This row reads an issue of eighteen results at eighteen such commits and counts both what one
// read asks GitHub about — every one of the eighteen commits, since no bound leaves any of them
// unread — and how many questions it asks for them: one, since the commits of one repository go
// in one query (delivery.ReadThroughCommitsPulls). A read that asks a question per commit, as the
// read before this change did, fails it, and so does a read that leaves a commit unasked.
func TestOneResultsReadAsksGitHubOnceForEveryCommitItLearns(t *testing.T) {
	t.Parallel()
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESRT", "acme/widgets", "Many results at many commits")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// Each result stands at a commit of its own, and GitHub names a pull request for none of
	// them, so no record and no read stores a head and every commit stays unread.
	const results = 18
	for at := range results {
		commit := fmt.Sprintf("%040x", at+1)
		if response := recordResult(t, handler, issue.Key, resultBody("k1", "pass", commit, nil)); response.Code != 201 {
			t.Fatalf("record the result at %s: %d %s", commit, response.Code, response.Body.String())
		}
	}
	// The records' own read-throughs, which the read-time read neither counts nor is bound by.
	spent, queried := fake.asks("acme/widgets"), fake.commitQueries()

	read := readResults(t, handler, issue.Key)
	asked, queries := fake.asks("acme/widgets")-spent, fake.commitQueries()-queried
	if asked != results || queries != 1 {
		t.Fatalf("one read of %d results at %d commits asked GitHub about %d commits in %d queries, want every commit in one question",
			results, results, asked, queries)
	}
	// The answer is whole: every result is read, and each stands at another commit, which is what
	// the stored rows say of a commit no pull request carries.
	check := checkOf(t, read, "k1")
	if len(check.Results) != results {
		t.Fatalf("the check answers %d results, want %d", len(check.Results), results)
	}
	for _, result := range check.Results {
		if result.CommitStands.Answer != model.CommitOtherCommit {
			t.Fatalf("the result at %s stands %q, want %q: no pull request carries it",
				result.CommitSHA, result.CommitStands.Answer, model.CommitOtherCommit)
		}
	}
}

// A pass at a commit that only a read-time read-through can learn stands on the FIRST read,
// however many results of the issue stand at commits before it. The issue's one pass is at
// headCommit, recorded before any pull request carried it; then a pull request of the issue opens
// there and no event of it reaches Dispatch, so only this read can learn it
// (TestAResultStandsAtItsCommitOnceAPullRequestOpensThere). After the pass, fails are recorded at
// commits GitHub names no pull request for, and results are read newest first, so those commits
// are the ones the read learns first.
//
// This is the measurement that took a bound on the commits one read may learn out of this branch.
// With that bound at twelve, a read of an issue with eleven such fails reached the pass and
// answered pass_stands `yes`, and a read with twelve did not reach it and answered `no` — on the
// first read, on a second inside delivery.CommitPullReadStaleAfter, and on a read after that span,
// which asked about the same twelve commits again. A gate reading that `no` would refuse an
// approval for a pass that stands, so the bound is gone: every commit of one repository goes in
// one question. Each case below answers `yes` on its first read, and the fails' count is no part
// of the answer.
func TestAPassAtACommitOnlyAReadThroughLearnsStandsOnTheFirstRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		fails  int
		stored bool
	}{
		{"a few results before the pass, no stored head", 11, false},
		{"a few results before the pass, a stored head", 11, true},
		{"more results before the pass than the bound allowed, no stored head", 12, false},
		{"more results before the pass than the bound allowed, a stored head", 12, true},
		{"scores of results before the pass", 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			issue := issueInRepository(t, handler, "RESBD", "acme/widgets", "A pass past the bound")
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			if tc.stored {
				// A merged pull request of the issue whose head Dispatch holds, at another commit:
				// a basis the check compares with, and no subject, since an open pull request is
				// (headBases). A second OPEN head would be a second subject, and a pass at one
				// subject alone does not make a check's pass stand.
				writeHead(t, database, "acme/widgets", 90, newHeadCommit, "merged", issue.Key)
			}
			pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
			for at := range tc.fails {
				commit := fmt.Sprintf("%040x", at+1)
				if response := recordResult(t, handler, issue.Key, resultBody("k1", "fail", commit, nil)); response.Code != 201 {
					t.Fatalf("record the fail at %s: %d %s", commit, response.Code, response.Body.String())
				}
			}
			// The pull request opens at headCommit, and GitHub names it for that commit alone.
			opened := openPullAt(93, issue.Key)
			fake.answers(func(repo, sha string) string {
				if sha != headCommit {
					return `[]`
				}
				return opened(repo, sha)
			})

			answered := func(when string) string {
				spent := fake.asks("acme/widgets")
				check := checkOf(t, readResults(t, handler, issue.Key), "k1")
				stands := "none"
				for _, result := range check.Results {
					if result.ID == pass.Result.ID && result.CommitStands != nil {
						stands = result.CommitStands.Answer + "/" + result.CommitStands.Reason
					}
				}
				t.Logf("%s: pass_stands %q, the pass %s, compared with %d bases, compared nothing %q, asked GitHub %d times",
					when, check.PassStands.Answer, stands, len(check.ComparedWith), check.ComparedNothing,
					fake.asks("acme/widgets")-spent)
				return check.PassStands.Answer
			}
			first := answered("first read")
			second := answered("second read")
			rewindCommitPullReads(t, served)
			third := answered("read after the span")
			if first != model.PassStandsYes {
				t.Fatalf("the first read of a pass at a commit only this read can learn answers pass_stands %q, want yes "+
					"(second read %q, after the span %q): %d results stand at commits before it, and no number of them may change the answer",
					first, second, third, tc.fails)
			}
			for _, answer := range []string{second, third} {
				if answer != model.PassStandsYes {
					t.Fatalf("a later read answers pass_stands %q, want yes: the head the first read learned is stored", answer)
				}
			}
		})
	}
}

// A batch GitHub refuses holds the claims of its own commits and of no other. The issue's pass is at
// headCommit, which only a read-time read-through can learn (as in
// TestAPassAtACommitOnlyAReadThroughLearnsStandsOnTheFirstRead), and thirty fails stand at commits
// GitHub names no pull request for, so one read asks about 31 commits in two batches, the fails'
// first. GitHub answers 502 to every query that does not name headCommit: the first batch.
//
// The read stops at that failure, as every read of GitHub stops at one. Its first batch's commits
// were asked and GitHub failed, so their claims stand for delivery.CommitPullReadStaleAfter, as the
// read of one commit's claim stands when GitHub fails it. The second batch was never asked, so no
// answer of GitHub's is behind its claims: the next read, inside that span, asks about exactly
// those commits, learns the pull request at headCommit and answers pass_stands `yes`. A read whose
// claims stand for commits no query named asks nothing on the second read and answers `no` for a
// pass that stands until the span ends.
func TestAFailedBatchLeavesTheCommitsNoQueryNamedToTheNextRead(t *testing.T) {
	t.Parallel()
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESFB", "acme/widgets", "A batch GitHub refuses")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	const fails = 30
	every := []string{headCommit}
	for at := range fails {
		commit := fmt.Sprintf("%040x", at+1)
		every = append(every, commit)
		if response := recordResult(t, handler, issue.Key, resultBody("k1", "fail", commit, nil)); response.Code != 201 {
			t.Fatalf("record the fail at %s: %d %s", commit, response.Code, response.Body.String())
		}
	}
	opened := openPullAt(93, issue.Key)
	fake.answers(func(repo, sha string) string {
		if sha != headCommit {
			return `[]`
		}
		return opened(repo, sha)
	})
	fake.refusesCommitQueries(func(shas []string) int {
		if slices.Contains(shas, headCommit) {
			return 0
		}
		return http.StatusBadGateway
	})
	named := func(queries [][]string) []string {
		var commits []string
		for _, query := range queries {
			for _, sha := range query {
				if !slices.Contains(commits, sha) {
					commits = append(commits, sha)
				}
			}
		}
		slices.Sort(commits)
		return commits
	}
	answer := func(check model.CheckBlock) string {
		for _, result := range check.Results {
			if result.ID == pass.Result.ID && result.CommitStands != nil {
				return check.PassStands.Answer + ", the pass " + result.CommitStands.Answer + "/" + result.CommitStands.Reason
			}
		}
		return check.PassStands.Answer + ", the pass not answered"
	}

	before := len(fake.commitsNamedByQueries(0))
	first := checkOf(t, readResults(t, handler, issue.Key), "k1")
	firstQueries := fake.commitsNamedByQueries(before)
	refused := named(firstQueries)
	if len(firstQueries) == 0 || slices.Contains(refused, headCommit) {
		t.Fatalf("the first read's queries named %v, want the fails' batch alone, which GitHub refuses: "+
			"the pass's commit must be in a batch after it for this row to test anything", firstQueries)
	}
	t.Logf("first read: %d queries naming %d commits, GitHub answering 502; %s", len(firstQueries), len(refused), answer(first))

	between := len(fake.commitsNamedByQueries(0))
	second := checkOf(t, readResults(t, handler, issue.Key), "k1")
	asked := named(fake.commitsNamedByQueries(between))
	var want []string
	for _, sha := range every {
		if !slices.Contains(refused, sha) {
			want = append(want, sha)
		}
	}
	slices.Sort(want)
	t.Logf("second read: asked about %d commits; %s", len(asked), answer(second))
	if !slices.Equal(asked, want) {
		t.Fatalf("the second read asked GitHub about %d commits, want the %d that no query of the first read named "+
			"(its refused batch's claims stand, and no answer is behind the rest): asked %v, want %v",
			len(asked), len(want), asked, want)
	}
	if second.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the second read answers %s, want pass_stands yes: it asked about the pass's commit and GitHub named the pull request there",
			answer(second))
	}
}

// A read that stops inside a query GitHub answered leaves the commits after the stop to the next
// read. The issue's pass is at headCommit, which only a read-time read-through can learn, and one
// fail stands after it at a commit GitHub names eleven pull requests for, so one query asks about
// both, the fail's commit first, and reads the fail's commit over REST, since the query reads ten
// of a commit's pull requests. GitHub answers that REST read 502.
//
// The read stops there. The fail's commit keeps its claim, as the read of one commit GitHub failed
// keeps it. GitHub answered headCommit in the query, but the read stopped before storing that
// answer, so no answer of GitHub's is behind its claim: the next read, inside
// delivery.CommitPullReadStaleAfter, asks about headCommit alone, learns the pull request there
// and answers pass_stands `yes`. A read that keeps headCommit's claim asks nothing on the second
// read and answers `no` for a pass that stands until the span ends.
func TestAReadThatStopsInsideAnAnsweredQueryLeavesTheCommitsAfterItToTheNextRead(t *testing.T) {
	t.Parallel()
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESAQ", "acme/widgets", "A REST read GitHub refuses inside a batch")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	crowded := fmt.Sprintf("%040x", 1)
	if response := recordResult(t, handler, issue.Key, resultBody("k1", "fail", crowded, nil)); response.Code != 201 {
		t.Fatalf("record the fail at %s: %d %s", crowded, response.Code, response.Body.String())
	}
	opened := openPullAt(93, issue.Key)
	fake.answers(func(repo, sha string) string {
		switch sha {
		case headCommit:
			return opened(repo, sha)
		case crowded:
			pulls := make([]string, 0, delivery.CommitPullsPerCommit+1)
			for number := 100; number <= 100+delivery.CommitPullsPerCommit; number++ {
				pulls = append(pulls, strings.Trim(openPullAt(number, "OTHER-1")(repo, sha), "[]"))
			}
			return "[" + strings.Join(pulls, ",") + "]"
		}
		return `[]`
	})
	fake.refusesCommitPullsOverREST(crowded, http.StatusBadGateway)
	answer := func(check model.CheckBlock) string {
		for _, result := range check.Results {
			if result.ID == pass.Result.ID && result.CommitStands != nil {
				return check.PassStands.Answer + ", the pass " + result.CommitStands.Answer + "/" + result.CommitStands.Reason
			}
		}
		return check.PassStands.Answer + ", the pass not answered"
	}

	first := checkOf(t, readResults(t, handler, issue.Key), "k1")
	firstQueries := fake.commitsNamedByQueries(0)
	if len(firstQueries) != 1 || !slices.Equal(firstQueries[0], []string{crowded, headCommit}) {
		t.Fatalf("the first read's queries named %v, want one query naming the fail's commit and then the pass's: "+
			"the pass's commit must come after the stop in the same answer for this row to test anything", firstQueries)
	}
	t.Logf("first read: one query naming both commits, the REST read of the crowded one refused 502; %s", answer(first))

	between := len(fake.commitsNamedByQueries(0))
	second := checkOf(t, readResults(t, handler, issue.Key), "k1")
	secondQueries := fake.commitsNamedByQueries(between)
	t.Logf("second read: queries naming %v; %s", secondQueries, answer(second))
	if len(secondQueries) != 1 || !slices.Equal(secondQueries[0], []string{headCommit}) {
		t.Fatalf("the second read's queries named %v, want one naming the pass's commit alone: GitHub failed the crowded commit's "+
			"read, so its claim stands, and the read stopped before storing GitHub's answer about the pass's commit, so its claim was given back",
			secondQueries)
	}
	if second.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("the second read answers %s, want pass_stands yes: it asked about the pass's commit and GitHub named the pull request there",
			answer(second))
	}
}

// A pass at a commit GitHub names more pull requests for than one page of its REST list holds
// stands at the newest of them. GitHub names forty pull requests for headCommit, oldest first:
// thirty-nine of another issue, each headed at a commit of its own past headCommit, and the
// newest, #140, the issue's own, headed at headCommit. The query reads ten of them and their
// count, so the read falls back to the REST list, which Dispatch reads 32 at a time
// (delivery's fetchCommitPullPayloads): #140 is on its second page. The read must ask that page,
// learn #140 and answer pass_stands `yes`, asking GitHub one query and two pages.
//
// Below 32 pull requests one page holds the whole list, so no row of fewer could tell a read of
// one page from a read of all of them, and a fake GitHub that answers every pull request on every
// page passes such a row while it answers a list of forty as one that never ends.
func TestAPassAtTheNewestOfMoreThanAPageOfPullRequestsStands(t *testing.T) {
	t.Parallel()
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESPG", "acme/widgets", "Forty pull requests at one commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	own := openPullAt(140, issue.Key)
	fake.answers(func(repo, sha string) string {
		if sha != headCommit {
			return `[]`
		}
		pulls := make([]string, 0, 40)
		for number := 101; number < 140; number++ {
			pulls = append(pulls, strings.Trim(openPullAt(number, "OTHER-1")(repo, fmt.Sprintf("%040x", number)), "[]"))
		}
		pulls = append(pulls, strings.Trim(own(repo, sha), "[]"))
		return "[" + strings.Join(pulls, ",") + "]"
	})

	spent := fake.asks("acme/widgets")
	read := readResults(t, handler, issue.Key)
	asked := fake.asks("acme/widgets") - spent
	check := checkOf(t, read, "k1")
	stands := resultRead(t, read, "k1", pass.Result.ID).CommitStands
	t.Logf("GitHub asked %d times; the pass %+v; pass_stands %q; compared with %d bases", asked, stands, check.PassStands.Answer, len(check.ComparedWith))
	if check.PassStands.Answer != model.PassStandsYes || stands == nil || stands.Answer != model.CommitAtBasis {
		t.Fatalf("the read answers pass_stands %q with the pass %+v, want yes at the basis of #140: #140 is on the second page of the list",
			check.PassStands.Answer, stands)
	}
	if asked != 3 {
		t.Fatalf("the read asked GitHub %d times, want 3: one query, and the two pages of forty pull requests at 32 a page", asked)
	}
}

// A pull request of more commits than one page of GitHub's list of them holds, whose title, body
// and branch name no issue, is credited by the commit message that names its issue, and its
// messages are read once each, page by page. GitHub names #94 at headCommit; its 150 commits are
// two pages of GitHub's list (100 and 50), and only the 130th names the issue. The read must
// credit #94 to the issue, so the pass at headCommit stands, and store the 150 messages after
// asking two pages. A fake that answered every commit on every page answered a first page of 150,
// so the read asked all five pages it may and stored each message five times.
func TestAPullRequestOfMoreThanAPageOfCommitsIsCreditedByItsMessagesReadOnce(t *testing.T) {
	t.Parallel()
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESCM", "acme/widgets", "A pull request of 150 commits")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	fake.answers(func(repo, sha string) string {
		if sha != headCommit {
			return `[]`
		}
		return fmt.Sprintf(`[{"number":94,"title":"Tidy the walk","body":"","html_url":"https://github.com/%s/pull/94",
			"state":"open","merged":false,"updated_at":"2026-11-27T19:30:00Z",
			"head":{"ref":"feature/unnamed","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`, repo, sha, repo)
	})
	commits := make([]string, 150)
	for index := range commits {
		message := fmt.Sprintf("work %03d", index+1)
		if index == 129 {
			message = "work 130\n\nDispatch: " + issue.Key
		}
		commits[index] = fmt.Sprintf(`{"sha":"%040d","commit":{"message":%q}}`, index+1, message)
	}
	fake.commitsAnswer(func(string, string) (int, string) { return http.StatusOK, "[" + strings.Join(commits, ",") + "]" })

	pages := fake.commitPagesRead()
	read := readResults(t, handler, issue.Key)
	pages = fake.commitPagesRead() - pages
	check := checkOf(t, read, "k1")
	stands := resultRead(t, read, "k1", pass.Result.ID).CommitStands
	var stored int
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(array_length(commit_messages, 1), 0) from pull_request_texts where repo = 'acme/widgets' and number = 94`).Scan(&stored); err != nil {
		t.Fatalf("read the stored messages of #94: %v", err)
	}
	t.Logf("GitHub asked %d pages of #94's commits; %d messages stored; the pass %+v; pass_stands %q", pages, stored, stands, check.PassStands.Answer)
	if check.PassStands.Answer != model.PassStandsYes || stands == nil || stands.Answer != model.CommitAtBasis {
		t.Fatalf("the read answers pass_stands %q with the pass %+v, want yes at #94's head: its 130th message names the issue",
			check.PassStands.Answer, stands)
	}
	if pages != 2 || stored != 150 {
		t.Fatalf("the read asked %d pages of #94's commits and stored %d messages, want 2 pages and the 150 messages", pages, stored)
	}
}
