package cireconcile

import (
	"fmt"
	"strings"
	"testing"
)

// The second mapped repository of the tests below, whose second page of open pull requests
// GitHub refuses the query for.
const (
	refusedRow          = "acme/tools"
	refusedRepository   = "Acme/Tools"
	refusedRepositoryID = 777
)

// inSuite is one check run under the check suite that created it, which is how GitHub hangs a
// run: checkRun writes every run under one suite, and a test of the suites of a head has to say
// which suite each of its runs belongs to.
func inSuite(run object, suite int64) object {
	run["check_suite"] = object{"id": suite, "head_branch": "feature/x"}
	return run
}

// suitesOfHead is count completed check suites of one head, ids from first upwards: a head with
// more of them than one query reads is a head the pass must read over REST.
func suitesOfHead(sha string, first int64, count int) []object {
	made := make([]object, 0, count)
	for i := range count {
		suite := checkSuite(first+int64(i), "completed", "success", otherApp, "2026-11-05T03:01:00Z")
		suite["head_sha"] = sha
		made = append(made, suite)
	}
	return made
}

// runsOfSuite is count completed check runs of one suite, named run-001 upwards: a suite with
// more of them than one query reads is a head the pass must read over REST.
func runsOfSuite(repository, sha string, suite int64, count int) []object {
	made := make([]object, 0, count)
	for i := range count {
		run := checkRun(suite*1000+int64(i)+1, fmt.Sprintf("run-%03d", i+1), "completed", "success", otherApp, "2026-11-05T03:01:00Z")
		made = append(made, inSuite(atHead(repository, sha, run), suite))
	}
	return made
}

// batched is the GitHub of the tests below: one repository of twenty open pull requests, four of
// which one query cannot answer whole, and a second repository of twenty-five whose second page
// GitHub refuses the query for.
//
// The four heads of the first repository are the four ways a query falls short, one each:
//
//   - pull request 1 has 31 check suites, one more than a query reads;
//   - pull request 2 has one check suite of 61 check runs, one more than a query reads;
//   - pull request 3 is the head a query answers whole, and the one that proves the query asks
//     for every check type: its `flaky` check ran, passed, and was started again, so GitHub's
//     default answer -- the latest run of each name -- holds only the re-run, which is queued
//     and recorded by nothing, and the head never settles. Only a read of every check type
//     holds the attempt that passed;
//   - pull request 4's Checks fields GitHub refuses, answering them null with an errors entry
//     against that head's own path.
type batched struct {
	h *harness
	// manySuites, manyRuns, reRun and refused are the four heads above, and their numbers.
	manySuites, manyRuns, reRun, refused fakePull
}

func newBatched(t *testing.T) *batched {
	t.Helper()
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRepository(refusedRow)
	h.github.setRepository(refusedRow, refusedRepository, refusedRepositoryID)
	b := &batched{h: h, manySuites: head(1), manyRuns: head(2), reRun: head(3), refused: head(4)}

	// Twenty open pull requests, which is one whole page of a query.
	h.listHeads(heads(pullsPerQuery)...)

	h.github.setCheckSuites(gitHubRepository, b.manySuites.sha, suitesOfHead(b.manySuites.sha, 9001, suitesPerQuery+1)...)
	h.github.setCheckSuites(gitHubRepository, b.manyRuns.sha,
		atHead(gitHubRepository, b.manyRuns.sha, checkSuite(9100, "completed", "success", otherApp, "2026-11-05T03:01:00Z")))
	h.github.setCheckRuns(gitHubRepository, b.manyRuns.sha, runsOfSuite(gitHubRepository, b.manyRuns.sha, 9100, runsPerQuery+1)...)
	h.github.setCheckRuns(gitHubRepository, b.reRun.sha,
		atHead(gitHubRepository, b.reRun.sha, checkRun(31, "flaky", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		atHead(gitHubRepository, b.reRun.sha, checkRun(32, "flaky", "queued", "", otherApp, "")),
	)
	h.github.refuseChecks(gitHubRepository, b.refused.sha)
	h.github.setCheckRuns(gitHubRepository, b.refused.sha,
		atHead(gitHubRepository, b.refused.sha, checkRun(41, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:02:00Z")),
	)
	// The sixteen ordinary heads, each with one completed run.
	for _, pull := range heads(pullsPerQuery)[4:] {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:03:00Z")),
		)
	}

	// The second repository: twenty-five open pull requests, which is two pages, and GitHub
	// refuses the query for the second of them.
	h.github.setPullRequests(refusedRepository, heads(25)...)
	for _, pull := range heads(25) {
		h.github.setCheckRuns(refusedRepository, pull.sha,
			atHead(refusedRepository, pull.sha, checkRun(200+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:04:00Z")),
		)
	}
	h.github.refusePage(refusedRepository, 1)
	return b
}

// A pass reads every open head of every mapped repository in batched queries, and records every
// run and every suite of every one of them: from the query where the query could answer the
// head whole, and from the paged REST reads of before this change where it could not. The pass
// line counts the queries it asked, the points they cost and the heads it had to read over
// REST. It is the pass k17 describes.
func TestAPassRecordsEveryRunOfEveryHeadFromQueriesAndTheRESTItCannotAnswer(t *testing.T) {
	b := newBatched(t)
	h := b.h

	h.pass()

	// The head with more check suites than one query reads: every one of its 31 suites is in
	// the record, the 31st included, which only the REST read of that head can hold.
	state := h.recordOf(b.manySuites, b.manySuites.number)
	if state == nil {
		t.Fatal("the head with 31 check suites has no record")
	}
	if len(state.Suites) != suitesPerQuery+1 {
		t.Errorf("the record holds %d suites, want the %d GitHub has: %v", len(state.Suites), suitesPerQuery+1, state.Suites)
	}
	if _, held := state.Suites[fmt.Sprint(9001+suitesPerQuery)]; !held {
		t.Errorf("the record does not hold the %dst suite, the one past what a query reads: %v", suitesPerQuery+1, state.Suites)
	}

	// The head with more check runs in one suite than a query reads: every one of its 61 runs
	// is in the record, the 61st included.
	state = h.recordOf(b.manyRuns, b.manyRuns.number)
	if state == nil {
		t.Fatal("the head with 61 check runs has no record")
	}
	if len(state.Checks) != runsPerQuery+1 {
		t.Errorf("the record holds %d checks, want the %d runs GitHub has", len(state.Checks), runsPerQuery+1)
	}
	if _, held := state.Checks[fmt.Sprintf("run-%03d", runsPerQuery+1)]; !held {
		t.Errorf("the record does not hold run-%03d, the run past what a query reads: %v", runsPerQuery+1, state.Checks)
	}

	// The head whose `flaky` check passed and was started again: the query asks for every check
	// type, so the attempt that passed is recorded although a newer run of that name is queued.
	state = h.recordOf(b.reRun, b.reRun.number)
	if state == nil {
		t.Fatal("the head with a re-run check has no record")
	}
	flaky, held := state.Checks["flaky"]
	if !held {
		t.Fatalf("the record does not hold the attempt that passed, which only a read of every check type answers: %v", state.Checks)
	}
	if flaky.CheckRunID != 31 || flaky.Status != "completed" || flaky.Conclusion != "success" {
		t.Errorf("the record holds flaky as %+v, want run 31 completed with conclusion success", flaky)
	}

	// The head whose Checks fields GitHub refused the query: read over REST, and its run
	// recorded.
	state = h.recordOf(b.refused, b.refused.number)
	if state == nil {
		t.Fatal("the head whose Checks fields GitHub refused has no record")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Errorf("the record of the refused head holds %v, want the run GitHub's REST listing answers", state.Checks)
	}

	// The sixteen ordinary heads of that repository, every one answered by the one query.
	for _, pull := range heads(pullsPerQuery)[4:] {
		state := h.recordOf(pull, pull.number)
		if state == nil {
			t.Fatalf("pull request %d, which the query answered whole, has no record", pull.number)
		}
		if _, held := state.Checks["ci-ok"]; !held {
			t.Errorf("pull request %d's record holds %v, want its run", pull.number, state.Checks)
		}
	}

	// The repository whose second page GitHub refused: the pass read its open pull requests
	// over REST and every head of that page the same way, so every run of all twenty-five is
	// recorded.
	for _, pull := range heads(25) {
		state := h.stateAt(refusedRepository, pull.number, pull.sha)
		if state == nil {
			t.Fatalf("pull request %d of %s has no record", pull.number, refusedRepository)
		}
		if _, held := state.Checks["ci-ok"]; !held {
			t.Errorf("pull request %d of %s holds %v, want its run", pull.number, refusedRepository, state.Checks)
		}
	}
	if !h.logged("ci reconcile read a repository over REST") {
		t.Errorf("the page GitHub refused was not named at WARN:\n%s", h.logs.String())
	}

	// The line: one query for the first repository's one page, two for the second repository's
	// two, each charged a point; eight heads read over REST, the three of the first repository
	// a query could not answer whole and the five of the page GitHub refused; and the second
	// repository's open pull requests listed over REST in one page.
	line := h.passLine()
	for field, want := range map[string]int{
		"graphql_queries": 3,
		"graphql_points":  3,
		"heads_over_rest": 8,
		"list_pages":      1,
		"heads":           pullsPerQuery + 25,
	} {
		if got := number(t, line, field); got != want {
			t.Errorf("the pass's %s = %d, want %d: %v", field, got, want, line)
		}
	}
	if got := number(t, line, "rest_requests"); got <= 0 {
		t.Errorf("the pass reports %d REST requests, want the reads the query could not answer: %v", got, line)
	}
}

// With nothing changed between two passes, the second reads each head it must read over REST
// with the ETags it kept and GitHub answers 304 to every one of them: a head one query cannot
// answer is read over REST at every pass for as long as it is open, and those reads cost the
// App's budget nothing. It is k6.
func TestTheSecondPassReadsEveryOverflowHeadConditionally(t *testing.T) {
	b := newBatched(t)
	h := b.h
	// The page GitHub refuses makes five more heads of the second repository REST reads, which
	// is what the pass's own line counts them as; this test is about the ETags all of them keep.

	h.pass()

	sentBefore := h.github.count()

	h.pass()

	line := h.passLine()
	if got, want := number(t, line, "heads_over_rest"), 8; got != want {
		t.Fatalf("the second pass read %d heads over REST, want the %d the first pass did: %v", got, want, line)
	}
	read := 0
	for _, request := range h.github.received()[sentBefore:] {
		if !strings.Contains(request.url, "/check-runs") && !strings.Contains(request.url, "/check-suites") {
			continue
		}
		read++
		if want := h.github.etagOf(request.url); request.ifNoneMatch != want {
			t.Errorf("the second pass read %s with If-None-Match %q, want the ETag the first pass was answered with, %q",
				request.url, request.ifNoneMatch, want)
		}
	}
	if read == 0 {
		t.Fatal("the second pass read no head over REST at all")
	}
	if got := number(t, line, "rest_not_modified"); got < read {
		t.Errorf("the second pass had %d REST reads answered 304, want all %d of its head reads: %v", got, read, line)
	}
	if got, want := number(t, line, "heads_not_modified"), 8; got != want {
		t.Errorf("the second pass had %d heads answered 304 throughout, want the %d it read over REST: %v", got, want, line)
	}
	if got := number(t, line, "records"); got != 0 {
		t.Errorf("the second pass changed %d records, want none: nothing changed on GitHub: %v", got, line)
	}
}
