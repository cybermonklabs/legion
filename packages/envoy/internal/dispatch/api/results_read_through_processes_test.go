package api

import (
	"testing"
)

// Two envoy-dispatch processes on one database read a result's commit through once a span between
// them (delivery.CommitPullReadStaleAfter). A read of the issue's results in the first process
// asks GitHub for the pull requests of the result's commit. A read in the second process within
// the span asks GitHub nothing, since the claim every process shares (commit_pull_reads) holds
// the commit, and the first process's read within the span asks nothing either. Once the span has
// passed, a read in the second process reads the commit through. A claim that each process keeps
// in its own memory alone makes the second process ask GitHub again inside the span.
func TestTwoProcessesReadAResultsCommitThroughOnceASpan(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	otherHandler, other := secondReadThroughServer(t, database, fake)
	issue := issueInRepository(t, handler, "TWOP", "acme/widgets", "Read by two processes")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))

	spent := fake.asks("acme/widgets")
	readResults(t, handler, issue.Key)
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("the first process's read asked GitHub for the commit's pull requests %d times, want once", asked)
	}
	readResults(t, otherHandler, issue.Key)
	readResults(t, handler, issue.Key)
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("after a read in each process within one span GitHub was asked %d times, want once: the second process reads the claim the first one made", asked)
	}

	// The span passes for both processes.
	rewindCommitPullReads(t, served)
	rewindCommitPullReads(t, other)
	readResults(t, otherHandler, issue.Key)
	if asked := fake.asks("acme/widgets") - spent; asked != 2 {
		t.Fatalf("after the span GitHub was asked %d times in all, want twice: the second process reads the commit through once the span has passed", asked)
	}
}
