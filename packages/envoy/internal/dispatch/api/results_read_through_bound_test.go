package api

import (
	"fmt"
	"testing"

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
