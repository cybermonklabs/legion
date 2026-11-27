package api

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What one read of an issue's results may spend on GitHub when many of its results stand at
// commits no stored head names. learn reads each such commit through, one call after another
// (results_comparison.go), and a production read of dispatch://INFRA-3904 took longer than two
// minutes where the same read with ?at= took 8.3 s, since ?at= reads nothing through: an issue
// with scores of results pays a round trip for each of their commits before the first check is
// answered.
//
// The bound is readThroughsPerRead. The rest of the commits keep the answer the stored rows give
// this read, and the next read of the results — the Verification tab refetches on every
// result.recorded, and a reader's own refresh is another — reads more of them, since each read
// through of this read is claimed and the unread ones are not.
//
// Red before the bound: the read asks GitHub once per commit, which this row reads as the whole
// of its results, and the count is the measurement the production finding asked for.
func TestOneResultsReadReadsThroughNoMoreCommitsThanItsBound(t *testing.T) {
	t.Parallel()
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESRT", "acme/widgets", "Many results at many commits")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// Each result stands at a commit of its own, and GitHub names a pull request for none of
	// them, so no record and no read stores a head and every commit stays unread.
	const results = readThroughsPerRead + 6
	for at := range results {
		commit := fmt.Sprintf("%040x", at+1)
		if response := recordResult(t, handler, issue.Key, resultBody("k1", "pass", commit, nil)); response.Code != 201 {
			t.Fatalf("record the result at %s: %d %s", commit, response.Code, response.Body.String())
		}
	}
	// The records' own read-throughs, which the read-time bound neither counts nor is bound by.
	spent := fake.asks("acme/widgets")

	read := readResults(t, handler, issue.Key)
	if asked := fake.asks("acme/widgets") - spent; asked > readThroughsPerRead {
		t.Fatalf("one read of %d results at %d commits asked GitHub for a commit's pull requests %d times, want at most %d",
			results, results, asked, readThroughsPerRead)
	}
	// The answer is whole however many commits the read left unread: every result is read, and
	// each stands at another commit, which is what the stored rows say of an unread one.
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

// What the bound costs a reader: whether a read that stopped short answers a check wrongly, or
// only less completely. The issue's one pass is at headCommit, recorded before any pull request
// carried it; then a pull request of the issue opens there and no event of it reaches Dispatch,
// so only a read-time read-through of headCommit can learn it (TestAResultStandsAtItsCommitOnceAPullRequestOpensThere).
// After the pass, fails are recorded at commits GitHub names no pull request for. Results are read
// newest first, so those fails' commits are the ones learn reads through first.
//
// With one fail fewer than the bound the read reaches headCommit, learns the pull request, and
// the pass stands: the fixture can answer yes. With as many fails as the bound it cannot reach
// headCommit, and the answer it gives there is what this row measures, on a first read, on a
// second read inside delivery.CommitPullReadStaleAfter, and on a read after that span.
func TestABoundedReadAnswersAPassAtAnUnreadCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		fails  int
		stored bool
	}{
		{"the read reaches the pass, no stored head", readThroughsPerRead - 1, false},
		{"the read reaches the pass, a stored head", readThroughsPerRead - 1, true},
		{"the bound is spent before the pass, no stored head", readThroughsPerRead, false},
		{"the bound is spent before the pass, a stored head", readThroughsPerRead, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			issue := issueInRepository(t, handler, "RESBD", "acme/widgets", "A pass past the bound")
			uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
			if tc.stored {
				// An open pull request of the issue whose head Dispatch holds, at another commit.
				writeHead(t, database, "acme/widgets", 90, newHeadCommit, "open", issue.Key)
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
			if tc.fails < readThroughsPerRead {
				if first != model.PassStandsYes {
					t.Fatalf("a read that reaches the pass answers pass_stands %q, want yes: the fixture cannot measure the bound", first)
				}
				return
			}
			for _, answer := range []string{first, second, third} {
				if answer == model.PassStandsNo {
					t.Fatalf("a read whose bound is spent before the pass answers pass_stands no (first %q, second %q, after the span %q), where a read that reaches it answers yes: the bound answers wrongly, not less completely",
						first, second, third)
				}
			}
		})
	}
}
