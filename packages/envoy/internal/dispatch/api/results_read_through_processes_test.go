package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Two envoy-dispatch processes on one database read a result's commit through once a span between
// them (delivery.CommitPullReadStaleAfter). A read of the issue's results in the first process
// asks GitHub for the pull requests of the result's commit. A read in the second process within
// the span asks GitHub nothing, since the claim every process shares (commit_pull_reads) holds
// the commit, and the first process's read within the span asks nothing either. Once the span has
// passed, a read in the second process reads the commit through. A claim that each process keeps
// in its own memory alone makes the second process ask GitHub again inside the span.
func TestTwoProcessesReadAResultsCommitThroughOnceASpan(t *testing.T) {
	t.Parallel()
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

// A claim the first process gives back is read again by the second process at once. Process A's
// read of the issue's results claims all 31 of its commits, in the database and in A's memory,
// and asks GitHub about the first batch, which GitHub answers 502. While that query is held open,
// process B reads the same results: every claim is A's, so B asks GitHub nothing, and B's memory
// keeps the moment A's claims hold. A's read then stops at the failure: the first batch keeps its
// claims, and the commits no query named, the pass's among them, are given back, in the database
// and in A's memory, which is all a give-back can reach.
//
// B's next read must ask GitHub about exactly the commits A gave back, learn the pull request at
// the pass's commit and answer pass_stands `yes`. A read that trusts the moment B's memory took
// from A's claim asks nothing, finds no claim of a failed read in the database for those commits,
// and reads the pass as `other_commit` with pass_stands `no` for the rest of the span, behind a
// load balancer of two replicas.
func TestAClaimGivenBackInOneProcessIsReadAgainInAnother(t *testing.T) {
	t.Parallel()
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets")
	otherHandler, _ := secondReadThroughServer(t, database, fake)
	issue := issueInRepository(t, handler, "TWOGB", "acme/widgets", "A claim given back in another process")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	every := []string{headCommit}
	for at := range 30 {
		commit := fmt.Sprintf("%040x", at+1)
		every = append(every, commit)
		if response := recordResult(t, handler, issue.Key, resultBody("k1", "fail", commit, nil)); response.Code != http.StatusCreated {
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
	arrived, release := holdFirstCommitQuery(t, fake)
	first := readResultsInBackground(handler, issue.Key)
	awaitHeld(t, arrived, "process A's first query")

	held := len(fake.commitsNamedByQueries(0))
	readResults(t, otherHandler, issue.Key)
	if asked := fake.commitsNamedByQueries(held); len(asked) != 0 {
		t.Fatalf("process B asked GitHub %v while process A's claims held every commit, want nothing", asked)
	}
	release()
	if response := <-first; response.Code != http.StatusOK {
		t.Fatalf("process A's read of the results: %d %s", response.Code, response.Body.String())
	}
	refused := namedCommits(fake.commitsNamedByQueries(0))
	if len(refused) == 0 || slices.Contains(refused, headCommit) {
		t.Fatalf("process A's queries named %v, want the fails' batch alone, which GitHub refuses: the pass's commit "+
			"must be among the commits A gave back for this row to test anything", refused)
	}
	var givenBack []string
	for _, sha := range every {
		if !slices.Contains(refused, sha) {
			givenBack = append(givenBack, sha)
		}
	}
	slices.Sort(givenBack)

	between := len(fake.commitsNamedByQueries(0))
	second := checkOf(t, readResults(t, otherHandler, issue.Key), "k1")
	asked := namedCommits(fake.commitsNamedByQueries(between))
	t.Logf("process B's read after A's: asked about %d commits; %s", len(asked), passAnswer(second, pass.Result.ID))
	if !slices.Equal(asked, givenBack) {
		t.Fatalf("process B's read after A's asked GitHub about %d commits, want the %d A gave back: asked %v, want %v",
			len(asked), len(givenBack), asked, givenBack)
	}
	if second.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("process B's read after A's answers %s, want pass_stands yes: it asked about the pass's commit and GitHub named the pull request there",
			passAnswer(second, pass.Result.ID))
	}
}

// A read refused by a claim that no answer of GitHub's stands behind yet reads the commit as not
// known, in any process. Process A's read of the results claims the pass's commit and asks GitHub,
// and its query is held open. Process B's read meanwhile is refused by A's claim and asks nothing:
// what A's read will learn is not stored, so B reads the pass as unknown for read_failed, never as
// `other_commit` from rows A's read has not written yet. Once A's read has stored GitHub's answer,
// B's next read, inside the span, asks nothing and reads the pass at its basis from those rows.
func TestAReadRefusedByAClaimNoAnswerStandsBehindReadsTheCommitAsNotKnown(t *testing.T) {
	t.Parallel()
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets")
	otherHandler, _ := secondReadThroughServer(t, database, fake)
	issue := issueInRepository(t, handler, "TWOIF", "acme/widgets", "A read under way in another process")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	pass := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	fake.answers(openPullAt(93, issue.Key))
	arrived, release := holdFirstCommitQuery(t, fake)
	first := readResultsInBackground(handler, issue.Key)
	awaitHeld(t, arrived, "process A's query")

	held := len(fake.commitsNamedByQueries(0))
	during := checkOf(t, readResults(t, otherHandler, issue.Key), "k1")
	if asked := fake.commitsNamedByQueries(held); len(asked) != 0 {
		t.Fatalf("process B asked GitHub %v while process A's claim held the pass's commit, want nothing", asked)
	}
	t.Logf("process B's read while A's read is under way: %s", passAnswer(during, pass.Result.ID))
	stands := passStands(during, pass.Result.ID)
	if stands == nil || stands.Answer != model.CommitUnknown || stands.Reason != model.ReasonReadFailed {
		t.Fatalf("process B's read while A's read is under way answers %s, want the pass unknown/read_failed: "+
			"no answer of GitHub's stands behind the claim that refused it", passAnswer(during, pass.Result.ID))
	}
	release()
	if response := <-first; response.Code != http.StatusOK {
		t.Fatalf("process A's read of the results: %d %s", response.Code, response.Body.String())
	}

	between := len(fake.commitsNamedByQueries(0))
	after := checkOf(t, readResults(t, otherHandler, issue.Key), "k1")
	if asked := fake.commitsNamedByQueries(between); len(asked) != 0 {
		t.Fatalf("process B's read after A's asked GitHub %v, want nothing: A's claim stands, and GitHub's answer is behind it", asked)
	}
	if after.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("process B's read after A's answers %s, want pass_stands yes from the rows A's read stored",
			passAnswer(after, pass.Result.ID))
	}
}

// holdFirstCommitQuery holds the first batched query of commits' pull requests GitHub is asked from
// now on: arrived is closed once the query has reached GitHub, and GitHub answers it once release
// is called. Later queries are answered at once. A test that never calls release releases it at
// its end.
func holdFirstCommitQuery(t *testing.T, fake *commitPullsFake) (<-chan struct{}, func()) {
	t.Helper()
	arrived, released := make(chan struct{}), make(chan struct{})
	var first, done sync.Once
	fake.holdsCommitQueries(func([]string) {
		first.Do(func() {
			close(arrived)
			<-released
		})
	})
	release := func() { done.Do(func() { close(released) }) }
	t.Cleanup(release)
	return arrived, release
}

// awaitHeld waits for a held query to reach GitHub, for a minute at most.
func awaitHeld(t *testing.T, arrived <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(time.Minute):
		t.Fatalf("%s did not reach GitHub within a minute", what)
	}
}

// readResultsInBackground reads the results of issue through handler on a goroutine of its own, as
// alice, and hands back the answer once the read ends.
func readResultsInBackground(handler http.Handler, issue string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+issue+"/results", nil)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		done <- response
	}()
	return done
}

// namedCommits is every commit the queries name, each once, in order.
func namedCommits(queries [][]string) []string {
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

// passStands is where the result id of check stands, or nil where the read placed it nowhere.
func passStands(check model.CheckBlock, id string) *model.ResultCommitStands {
	for _, result := range check.Results {
		if result.ID == id {
			return result.CommitStands
		}
	}
	return nil
}

// passAnswer is the check's pass_stands and where the result id stands, for a failure message.
func passAnswer(check model.CheckBlock, id string) string {
	stands := passStands(check, id)
	if stands == nil {
		return check.PassStands.Answer + ", the pass not answered"
	}
	return check.PassStands.Answer + ", the pass " + stands.Answer + "/" + stands.Reason
}
