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
