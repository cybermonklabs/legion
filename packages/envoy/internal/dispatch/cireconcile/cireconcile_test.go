package cireconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// A head whose checks reached the listener through no webhook at all settles after one pass: the
// pass reads GitHub's own listings of that head, records the completed run and the completed
// suite, and the listener's summary loop publishes the settlement the daemon reads. A newer run
// at the same head, which again no webhook carries, settles again.
func TestAPassSettlesAHeadNoWebhookReached(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "failure", otherApp, "2026-11-05T03:01:00Z"),
		checkRun(2, "pr-title", "completed", "success", otherApp, "2026-11-05T03:01:10Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "failure", otherApp, "2026-11-05T03:01:20Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after the first pass")
	if settled.Number != pullNumber || settled.SHA != headSHA {
		t.Fatalf("the settlement is of %s at %s, want %s at %s", settled.Number, settled.SHA, pullNumber, headSHA)
	}
	if settled.Failed.Count != 1 || len(settled.FailingChecks) != 1 || settled.FailingChecks[0].Name != "ci-ok" {
		t.Fatalf("settlement = %+v, want ci-ok among its failing checks", settled)
	}
	if settled.Passed.Count != 1 || len(settled.Passed.Checks) != 1 || settled.Passed.Checks[0] != "pr-title" {
		t.Fatalf("settlement = %+v, want pr-title passing", settled)
	}

	// GitHub runs ci-ok again at the same head and it passes. The pass reads the listing again,
	// finds an ETag it does not match, and records the newer run over the older.
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(3, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:05:00Z"),
		checkRun(2, "pr-title", "completed", "success", otherApp, "2026-11-05T03:01:10Z"),
	)

	h.pass()

	settled = h.settlement(settlements, "after the second pass")
	if settled.Failed.Count != 0 || len(settled.FailingChecks) != 0 {
		t.Fatalf("settlement = %+v, want no failing check once ci-ok passed", settled)
	}
	if settled.Passed.Count != 2 {
		t.Fatalf("settlement = %+v, want both checks passing", settled)
	}
}

// A head whose completion webhook GitHub never delivered settles after one pass. The listener
// took the run's in_progress webhook and recorded a running check, and cistore settles no head
// while a check its record holds is queued or running (settlementReady), so until something
// writes the end of that run the head cannot settle and the Legion daemon waits on an answer
// that never comes. The pass reads GitHub's own listing, finds the run ended, writes it over the
// running one, and the listener publishes the settlement the daemon was waiting for.
func TestAPassSettlesAHeadWhoseCompletionWebhookNeverCame(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	settlements := h.subscribe()

	// The run GitHub started and never reported the end of, as its own delivery carries it.
	running := checkRun(1, "ci-ok", "in_progress", "", otherApp, "2026-11-05T03:01:00Z")
	// GitHub leaves completed_at null until a run ends, so this observation is timed by the
	// run's start, and the completed one the pass reads is later than it.
	delete(running, "completed_at")
	// The delivery names no pull request, so the listener attributes it by its check suite's
	// head branch, which is this branch's own attribution rule: refs/pull/<n>/head is the ref
	// that pull request's head is fetched by.
	running["pull_requests"] = []any{}
	running["check_suite"] = object{"id": 900, "head_branch": "refs/pull/" + pullNumber + "/head"}
	body, err := json.Marshal(object{
		// GitHub's action for a run it has begun, which is not the "completed" every other
		// webhook in these tests carries.
		"action":    "created",
		"check_run": running,
		"sender":    object{"login": "ci", "type": "Bot"},
		"repository": object{
			"name":           gitHubName,
			"owner":          object{"login": gitHubOwner},
			"full_name":      gitHubRepository,
			"default_branch": "main",
		},
	})
	if err != nil {
		t.Fatalf("build the in_progress webhook body: %v", err)
	}

	h.post("check_run", string(body), "d1")

	state, _, ok := h.record()
	if !ok {
		t.Fatal("the in_progress webhook left the head no record")
	}
	if held := state.Checks["ci-ok"]; held.Status != "in_progress" {
		t.Fatalf("the record holds %+v, want ci-ok in_progress from the webhook", state.Checks)
	}
	h.noSettlement(settlements, "while the record holds a running ci-ok")

	// GitHub holds what the completion webhook would have carried: the same run, ended, beside
	// the suite that ran it.
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "failure", otherApp, "2026-11-05T03:05:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "failure", otherApp, "2026-11-05T03:05:10Z"),
	)

	h.pass()

	settled := h.settlement(settlements, "after a pass over the run whose completion never arrived")
	if settled.Number != pullNumber || settled.SHA != headSHA {
		t.Fatalf("the settlement is of %s at %s, want %s at %s", settled.Number, settled.SHA, pullNumber, headSHA)
	}
	if settled.Failed.Count != 1 || len(settled.FailingChecks) != 1 || settled.FailingChecks[0].Name != "ci-ok" {
		t.Fatalf("settlement = %+v, want ci-ok among its failing checks", settled)
	}
	state, _, ok = h.record()
	if !ok {
		t.Fatal("the head has no record after the pass")
	}
	held, has := state.Checks["ci-ok"]
	if !has {
		t.Fatalf("the record holds %+v, want ci-ok", state.Checks)
	}
	if held.Status != "completed" || held.Conclusion != "failure" {
		t.Fatalf("the record holds ci-ok as %+v, want it completed with conclusion failure", held)
	}
}

// A repository neither a mapping nor an issue names is not read, and a repository two rows name
// -- the name it had and the name it has -- is read once, because GitHub answers both rows with
// one id.
func TestAPassReadsEachCoveredRepositoryOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRepository("acme/widgets-old")
	h.listHead()
	// Both rows resolve to the one repository GitHub holds, under the name it has now.
	h.github.setRepository("acme/widgets-old", gitHubRepository, repositoryID)
	// A repository of the same App that no mapping and no issue names.
	h.github.setRepository("acme/unmapped", "Acme/Unmapped", 777)
	h.github.setPullRequests("Acme/Unmapped", fakePull{number: 3, sha: headSHA})
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

	h.pass()

	for _, request := range h.github.received() {
		if strings.Contains(strings.ToLower(request.url), "unmapped") {
			t.Fatalf("the pass read a repository no mapping and no issue names: %s %s", request.method, request.url)
		}
	}
	if asked := h.queriesFor(gitHubRepository); asked != 1 {
		t.Fatalf("%s was queried %d times, want once for two rows naming it", gitHubRepository, asked)
	}
	if sent := h.github.countPath(fmt.Sprintf("/repos/%s/commits/%s/check-runs", gitHubRepository, headSHA)); sent != 0 {
		t.Fatalf("the head's check runs were read over REST %d times, want none: the query answered the head whole", sent)
	}
	if line := h.passLine(); number(t, line, "heads") != 1 {
		t.Fatalf("the pass line reports %d heads, want 1: %v", number(t, line, "heads"), line)
	}
}

// A check suite of another App that is still queued, with no run of its own, is not recorded: a
// record holding it would never settle, since cistore holds a head while any suite it holds is
// not completed.
func TestAQueuedSuiteIsNotRecordedAndTheHeadStillSettles(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z"),
		checkSuite(901, "queued", "", 4321, "2026-11-05T03:01:06Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "with a queued suite at the head")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record")
	}
	if _, held := state.Suites["901"]; held {
		t.Fatalf("the queued suite is in the record: %+v", state.Suites)
	}
	if _, held := state.Suites["900"]; !held {
		t.Fatalf("the completed suite is not in the record: %+v", state.Suites)
	}
}

// A run that is queued or in progress is not recorded either: nothing takes a check out of a
// record, so one recorded while it is running would hold the head for ever if it never finished.
func TestARunningRunIsNotRecordedAndTheHeadStillSettles(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
		checkRun(2, "flaky", "queued", "", otherApp, ""),
		checkRun(3, "slow", "in_progress", "", otherApp, "2026-11-05T03:01:02Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "with a queued and a running run at the head")
	if settled.SHA != headSHA || settled.Passed.Count != 1 || settled.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want the head settled on its one completed check", settled)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record")
	}
	if len(state.Checks) != 1 {
		t.Fatalf("the record holds %+v, want the completed check alone", state.Checks)
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want ci-ok", state.Checks)
	}
}

// A pass over what the webhooks already recorded writes nothing: the observation it maps is the
// one the webhook made, so the record's revision does not move and no second settlement is
// published.
func TestAPassOverRecordedChecksChangesNothing(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")
	suite := checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.github.setCheckSuites(gitHubRepository, headSHA, suite)
	settlements := h.subscribe()

	// The listener's own webhooks carry the very objects GitHub's listings answer with.
	h.post("check_run", webhookBody("check_run", run), "d1")
	h.post("check_suite", webhookBody("check_suite", suite), "d2")
	settled := h.settlement(settlements, "from the webhooks alone")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the webhook's own settlement of the head", settled)
	}
	_, before := h.settledRecord()

	h.pass()

	if asked := h.queriesFor(gitHubRepository); asked != 1 {
		t.Fatalf("the repository was queried %d times, want once: the pass must read GitHub", asked)
	}
	// The summary loop settles on its own clock, so the record is given the same room a second
	// settlement would have needed before its revision is read again.
	h.noSettlement(settlements, "after a pass over what the webhooks recorded")
	_, after, ok := h.record()
	if !ok {
		t.Fatal("the record is gone after the pass")
	}
	if after != before {
		t.Fatalf("the record's revision moved from %d to %d; the pass rewrote what the webhooks recorded", before, after)
	}
	// The pass wrote both observations through to the store, which applied each as no change:
	// records counts the records a pass changed, so a pass that mended nothing reports none.
	if line := h.passLine(); number(t, line, "records") != 0 {
		t.Fatalf("the pass line reports %d records, want 0: it changed no record: %v", number(t, line, "records"), line)
	}
}

// A verdict check says a review passed or failed, so it is recorded only when the review App
// created it, and not at all when Dispatch holds no review App id.
func TestOnlyTheReviewAppsVerdictIsRecorded(t *testing.T) {
	runs := []object{
		checkRun(1, "build", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
		checkRun(2, "tester", "completed", "success", reviewerApp, "2026-11-05T03:01:01Z"),
		checkRun(3, "tester", "completed", "failure", otherApp, "2026-11-05T03:01:02Z"),
	}

	t.Run("with the review App's id", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.mapRepository(rowRepository)
		h.listHead()
		h.github.setCheckRuns(gitHubRepository, headSHA, runs...)

		h.pass()

		state, _, ok := h.record()
		if !ok {
			t.Fatal("the head has no record")
		}
		tester, held := state.Checks["tester"]
		if !held {
			t.Fatalf("the review App's verdict is not in the record: %+v", state.Checks)
		}
		if tester.CheckRunID != 2 || tester.Conclusion != "success" {
			t.Fatalf("the recorded tester is run %d concluding %q, want run 2 concluding success: another App's verdict was recorded",
				tester.CheckRunID, tester.Conclusion)
		}
		if !h.logged("ci reconcile ignored a verdict check another App created") {
			t.Fatalf("the dropped verdict was not warned about:\n%s", h.logs.String())
		}
	})

	t.Run("with no review App id", func(t *testing.T) {
		h := newHarness(t, "")
		h.mapRepository(rowRepository)
		h.listHead()
		h.github.setCheckRuns(gitHubRepository, headSHA, runs...)

		h.pass()

		state, _, ok := h.record()
		if !ok {
			t.Fatal("the head has no record")
		}
		if _, held := state.Checks["tester"]; held {
			t.Fatalf("a verdict was recorded with no review App id: %+v", state.Checks)
		}
		if _, held := state.Checks["build"]; !held {
			t.Fatalf("the ordinary check was not recorded: %+v", state.Checks)
		}
		if !h.logged("DISPATCH_REVIEWER_APP_ID is not set") {
			t.Fatalf("the dropped verdicts were not warned about:\n%s", h.logs.String())
		}
	})
}

// A head whose record has gone -- the bucket's TTL, or a delete -- is read in full again rather
// than conditionally: a 304 would hide from the pass exactly the observations the record needs
// back, and the head would never settle again.
//
// Only a head read over REST holds ETags at all, since a query carries none, so the head here
// is one the query cannot answer: GitHub refuses its Checks fields and the pass reads it over
// REST, which is where the ETags and this rule live.
func TestAHeadWhoseRecordIsGoneIsReadInFullAgain(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.refuseChecks(gitHubRepository, headSHA)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

	h.pass()
	if _, _, ok := h.record(); !ok {
		t.Fatal("the first pass left no record")
	}

	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	if err := kv.Delete(cistore.Key(gitHubOwner, gitHubName, pullNumber, headSHA)); err != nil {
		t.Fatalf("delete the head's record: %v", err)
	}
	sentBefore := h.github.count()

	h.pass()

	read := 0
	for _, request := range h.github.received()[sentBefore:] {
		if !strings.Contains(request.url, "check-runs") {
			continue
		}
		read++
		if request.ifNoneMatch != "" {
			t.Fatalf("the head's check runs were read conditionally although its record had gone: %s", request.ifNoneMatch)
		}
	}
	if read != 1 {
		t.Fatalf("the head's check runs were read %d times, want once", read)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the pass did not write the head's record again")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want ci-ok back", state.Checks)
	}
}

// The pass's one line carries the counts an alarm reads, under field names that do not move:
// the counts it has always carried, and the GraphQL and REST counts beside them.
func TestThePassLineCountsWhatThePassDid(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z"),
	)

	h.pass()

	line := h.passLine()
	// The head's completed run and its completed suite are two observations of the one record
	// that head has, and records counts one for each head whose record the pass changed, so the
	// pass reports 1. One query answered the one page of the one repository, which cost one
	// point, and the only REST read the pass made is the repository's own identity: no listing
	// of its open pull requests, and no read of its head.
	for field, want := range map[string]int{
		"list_pages":         0,
		"heads":              1,
		"head_requests":      0,
		"heads_not_modified": 0,
		"records":            1,
		"mappings_skipped":   0,
		"heads_too_large":    0,
		"graphql_queries":    1,
		"graphql_points":     1,
		"rest_requests":      1,
		"rest_not_modified":  0,
		"heads_over_rest":    0,
	} {
		if got := number(t, line, field); got != want {
			t.Errorf("the first pass's %s = %d, want %d: %v", field, got, want, line)
		}
	}
	if got, want := number(t, line, "requests"), 2; got != want {
		t.Errorf("the first pass's requests = %d, want %d: the query and the repository's identity: %v", got, want, line)
	}
	if line["rate_limited"] != false {
		t.Errorf("the first pass reports rate_limited = %v, want false", line["rate_limited"])
	}
	if _, ok := line["duration"].(string); !ok {
		t.Errorf("the pass line has no duration: %v", line)
	}

	// Nothing has changed, so the second pass's one REST read -- the repository's identity --
	// is answered 304, and its query reads the head again and records nothing.
	h.pass()

	line = h.passLine()
	if got := number(t, line, "rest_not_modified"); got != 1 {
		t.Errorf("the second pass's rest_not_modified = %d, want 1: the repository's identity has not changed: %v", got, line)
	}
	if got := number(t, line, "records"); got != 0 {
		t.Errorf("the second pass's records = %d, want 0: GitHub holds what the first pass recorded: %v", got, line)
	}
}

// The pass line tells its own counts apart, which only a pass whose numbers differ can show:
// two repositories queried, three heads, one of them read over REST and answered 304
// throughout, and one record changed. It is the pass k12 describes.
//
// Each of the three heads is there for one of the counts. Pull request 7's Checks fields are
// refused to the query, so the pass reads that head over REST; the pass before this one read
// it in full and nothing at it has changed since, so both its listings answer 304 and it is
// the one head GitHub had nothing new for and the one head read over REST. Pull request 9 the
// query answers whole, and everything it holds to record is in the record already, so it
// changes nothing. Pull request 8, of the other repository, was opened since, and its
// completed run is the one record this pass changes.
func TestThePassLineTellsItsPagesHeadsAndRecordsApart(t *testing.T) {
	// The second mapped repository, and the two heads beside the harness's own: the one whose
	// every observation the record holds already, and the one with a run to record.
	const (
		otherRow          = "acme/tools"
		otherRepository   = "Acme/Tools"
		otherRepositoryID = 777

		unchangedHeadSHA = "c0ffee1234567890abcdef1234567890abcdef12"
		changedHeadSHA   = "b1c0ffee1234567890abcdef1234567890abcdef"
	)
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRepository(otherRow)
	h.github.setRepository(rowRepository, gitHubRepository, repositoryID)
	h.github.setRepository(otherRow, otherRepository, otherRepositoryID)
	h.github.setPullRequests(gitHubRepository,
		fakePull{number: 7, sha: headSHA},
		fakePull{number: 9, sha: unchangedHeadSHA},
	)
	h.github.refuseChecks(gitHubRepository, headSHA)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z"),
	)
	h.github.setCheckRuns(gitHubRepository, unchangedHeadSHA,
		atHead(gitHubRepository, unchangedHeadSHA, checkRun(2, "build", "completed", "success", otherApp, "2026-11-05T03:01:10Z")),
	)

	// The pass before the one this test is about is what earns the 304s: it reads both heads of
	// the first repository and records the three observations they hold, changing each head's
	// record, and the second repository has no open pull request yet.
	h.pass()

	if got := number(t, h.passLine(), "records"); got != 2 {
		t.Fatalf("the pass before changed %d records, want the records of its two heads, the one holding a run and a suite and the one holding a run: %v",
			got, h.passLine())
	}

	// Since that pass, pull request 8 has been opened in the other repository, at a head
	// carrying the one completed run this pass has to record.
	h.github.setPullRequests(otherRepository, fakePull{number: 8, sha: changedHeadSHA})
	h.github.setCheckRuns(otherRepository, changedHeadSHA,
		atHead(otherRepository, changedHeadSHA, checkRun(3, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:06:00Z")),
	)
	sentBefore := h.github.count()

	h.pass()

	line := h.passLine()
	for field, want := range map[string]int{
		// No page of open pull requests is listed over REST: every page of both repositories
		// was answered by a query.
		"list_pages":         0,
		"heads":              3,
		"heads_not_modified": 1,
		"records":            1,
		"mappings_skipped":   0,
		"heads_too_large":    0,
		"graphql_queries":    2,
		"graphql_points":     2,
		"heads_over_rest":    1,
		// The two repositories' identity reads and the refused head's two listings, all four
		// of them answered 304.
		"rest_requests":     4,
		"rest_not_modified": 4,
	} {
		if got := number(t, line, field); got != want {
			t.Errorf("the pass's %s = %d, want %d: %v", field, got, want, line)
		}
	}
	// head_requests is the requests the one head read over REST sent, which the stand-in's own
	// log is the only honest count of.
	sent := 0
	for _, request := range h.github.received()[sentBefore:] {
		if strings.Contains(request.url, "check-runs") || strings.Contains(request.url, "check-suites") {
			sent++
		}
	}
	if got := number(t, line, "head_requests"); got != sent {
		t.Errorf("the pass reports %d head requests, want the %d the stand-in was sent: %v", got, sent, line)
	}
	if line["rate_limited"] != false {
		t.Errorf("the pass reports rate_limited = %v, want false: no limit stopped it", line["rate_limited"])
	}
	if _, ok := line["duration"].(string); !ok {
		t.Errorf("the pass line has no duration: %v", line)
	}
}

// atHead is one check object of a head other than the harness's own: checkRun and checkSuite
// answer for headSHA of gitHubRepository alone, and an observation is keyed by the head_sha its
// object carries, so a run at another repository's head has to say where it ran itself.
func atHead(repository, sha string, item object) object {
	item["head_sha"] = sha
	item["html_url"] = fmt.Sprintf("https://github.com/%s/runs/%v", repository, item["id"])
	return item
}

// A limit already in force on the App ends a pass before it sends GitHub anything, and the pass
// line names the scope and when the limit ends.
func TestALimitInForceEndsThePassBeforeItReadsGitHub(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	if err := h.reconciler.Limits.Record(h.ctx, "app", 10*time.Minute); err != nil {
		t.Fatalf("record a limit on the App: %v", err)
	}

	h.pass()

	if sent := h.github.count(); sent != 0 {
		t.Fatalf("the pass sent GitHub %d requests under a limit in force: %+v", sent, h.github.received())
	}
	line := h.passLine()
	if line["rate_limited"] != true {
		t.Fatalf("the pass line reports rate_limited = %v, want true: %v", line["rate_limited"], line)
	}
	if line["rate_limit_scope"] != "app" {
		t.Fatalf("the pass line names scope %v, want app: %v", line["rate_limit_scope"], line)
	}
	if _, ok := line["rate_limit_until"].(string); !ok {
		t.Fatalf("the pass line says when the limit ends: %v", line)
	}
}

// GitHub's own rate-limited answer ends the pass where it stands: nothing further is read or
// recorded, and the head the limit cut short is left exactly as the pass found it, for the next
// pass to read from the start (the cursor names it). A rate limit is not a repository the App
// cannot read, which the pass would have skipped quietly and gone on from.
func TestARateLimitedAnswerStopsThePass(t *testing.T) {
	t.Run("at a head's REST read", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.mapRepository(rowRepository)
		h.listHead()
		// The query cannot answer this head, so the pass reads it over REST, which is where
		// GitHub's REST limit answers it.
		h.github.refuseChecks(gitHubRepository, headSHA)
		h.github.setCheckRuns(gitHubRepository, headSHA,
			checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
		)
		h.github.rateLimitNext(fmt.Sprintf("/repos/%s/commits/%s/check-runs", gitHubRepository, headSHA))

		h.pass()

		if _, _, ok := h.record(); ok {
			t.Fatal("the pass recorded something although GitHub rate-limited its first head read")
		}
		if sent := h.github.countPath(fmt.Sprintf("/repos/%s/commits/%s/check-suites", gitHubRepository, headSHA)); sent != 0 {
			t.Fatalf("the pass read on after the rate limit: %d check-suites requests", sent)
		}
		line := h.passLine()
		if line["rate_limited"] != true {
			t.Fatalf("the pass line reports rate_limited = %v, want true: %v", line["rate_limited"], line)
		}
		if state, kept := h.reconciler.heads[gitHubRepository+"#"+pullNumber+"@"+headSHA]; kept {
			// The limit cut this head's first read short, so the pass kept nothing of a head it
			// never read whole; a head that had ETags from an earlier pass keeps them
			// (TestAHeadCutShortKeepsItsETags).
			t.Fatalf("the pass kept %+v of a head it never read whole", state)
		}
	})

	t.Run("at the first request of all", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.mapRepository(rowRepository)
		h.listHead()
		h.github.rateLimitEverything()

		h.pass()

		line := h.passLine()
		if line["rate_limited"] != true {
			t.Fatalf("the pass line reports rate_limited = %v, want true: %v", line["rate_limited"], line)
		}
		if got := number(t, line, "mappings_skipped"); got != 0 {
			t.Fatalf("the pass skipped %d mappings; a rate limit stops the pass, it does not skip a repository: %v", got, line)
		}
		if got := number(t, line, "heads"); got != 0 {
			t.Fatalf("the pass read %d heads under a rate limit: %v", got, line)
		}
	})
}

// The scope the pass line names is the one GitHub's answer was budgeted under. A repository's
// own read is made with that repository's installation token, so a limit GitHub answers it with
// is recorded in the shared ledger under that installation; a line naming the App instead would
// send an alarm looking for a limit nobody recorded.
func TestTheStopNamesTheScopeTheReadWasBudgetedUnder(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.rateLimitNext("/repos/" + rowRepository)

	h.pass()

	line := h.passLine()
	if line["rate_limited"] != true {
		t.Fatalf("the pass line reports rate_limited = %v, want true: %v", line["rate_limited"], line)
	}
	want := githubapp.InstallationScope(h.github.installationID())
	if line["rate_limit_scope"] != want {
		t.Fatalf("the pass line names scope %v, want the installation the read was budgeted under, %s: %v",
			line["rate_limit_scope"], want, line)
	}
	// The line carries the reset time of that same answer beside its scope, which is the other
	// half of what k12 asks a stopped pass's line for: an alarm reads it to say when GitHub
	// takes a request under the scope again.
	until, ok := line["rate_limit_until"].(string)
	if !ok {
		t.Fatalf("the pass line says when the limit GitHub answered with ends: %v", line)
	}
	if _, err := time.Parse(time.RFC3339, until); err != nil {
		t.Fatalf("the pass line's rate_limit_until %q is not RFC3339: %v", until, err)
	}
	wait, _, err := h.reconciler.Limits.Wait(h.ctx, want)
	if err != nil || wait <= 0 {
		t.Fatalf("the shared limit on %s has %s left (%v), want GitHub's own wait recorded under the scope the line names", want, wait, err)
	}
}

// readRecorder is a githubreads.Recorder that keeps each attempt it is handed, in the order the
// passes recorded them, so a test reads what a pass gave GET /healthz to answer.
type readRecorder struct {
	mu       sync.Mutex
	recorded []recordedRead
}

// recordedRead is one attempt a readRecorder was handed.
type recordedRead struct {
	read      githubreads.Read
	succeeded bool
	cause     string
}

func (r *readRecorder) record(_ context.Context, read githubreads.Read, succeeded bool, cause string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, recordedRead{read: read, succeeded: succeeded, cause: cause})
	return nil
}

// take answers the attempts recorded since the last take, and forgets them.
func (r *readRecorder) take() []recordedRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.recorded
	r.recorded = nil
	return taken
}

// recordReads makes the harness's Reconciler record each pass's attempt at reading GitHub into
// the recorder it answers.
func (h *harness) recordReads() *readRecorder {
	reads := &readRecorder{}
	h.reconciler.GitHubReads = reads.record
	return reads
}

// Each pass records its attempt at reading GitHub, which GET /healthz answers: a pass whose first
// request GitHub answered 503 records failed, with that answer as its cause; the next pass, which
// GitHub answered, records succeeded; and a pass that asked GitHub nothing -- it bound no CI
// record writer, so it read nothing -- records nothing at all.
func TestEachPassRecordsItsAttemptAtReadingGitHub(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	reads := h.recordReads()
	h.mapRepository(rowRepository)
	h.listHead()
	// The first request of a pass looks the installation of the mapped repository up, for the
	// token every read of it is made with.
	installation := "/repos/" + rowRepository + "/installation"
	h.github.answerStatus(installation, http.StatusServiceUnavailable)

	h.pass()

	if sent := h.github.received(); len(sent) == 0 || sent[0].url != installation {
		t.Fatalf("the pass sent %+v, want the installation lookup first", sent)
	}
	got := reads.take()
	if len(got) != 1 || got[0].read != githubreads.CIReconcile || got[0].succeeded ||
		!strings.Contains(got[0].cause, installation) || !strings.Contains(got[0].cause, "status 503") {
		t.Fatalf("the pass GitHub answered 503 recorded %+v, want one failed %s attempt whose cause is that 503:\n%s", got, githubreads.CIReconcile, h.logs.String())
	}

	h.github.answerStatus(installation, 0)
	h.pass()
	if got := reads.take(); len(got) != 1 || got[0].read != githubreads.CIReconcile || !got[0].succeeded || got[0].cause != "" {
		t.Fatalf("the pass GitHub answered recorded %+v, want one succeeded %s attempt with no cause:\n%s", got, githubreads.CIReconcile, h.logs.String())
	}

	h.reconciler.Open = func() (Writer, error) { return nil, errors.New("NATS is not connected") }
	sent := h.github.count()
	h.pass()
	if got := reads.take(); len(got) != 0 {
		t.Fatalf("a pass that asked GitHub nothing recorded %+v, want nothing", got)
	}
	if more := h.github.count() - sent; more != 0 {
		t.Fatalf("the pass that bound no writer sent GitHub %d requests, want none", more)
	}
}

// A pass records failed when it gives up part of its read of GitHub on an answer the client takes
// as data and notes nothing of -- a 404, a 422, a name the pass cannot key a record by, a head's
// status field a query answers refused, or a limit recorded before any request -- so in each
// failed row only the pass's own note can make the record say so, and its cause names the
// repository it gave up (the limit in force at the start names the limit instead, the pass having
// reached no repository). A query GitHub answered 502 whose repository the REST listing then read
// is a read the pass made the other way, and it records succeeded; a pass its caller cancelled,
// which is this process shutting down, records nothing.
func TestAPassRecordsAReadItGaveUpOnAsFailed(t *testing.T) {
	for _, row := range []struct {
		name string
		// arrange sets up the mapping, GitHub's answers and the shared limits before the pass.
		arrange func(h *harness)
		// pass runs the one pass; nil is h.pass.
		pass func(h *harness)
		// nothing is a pass that records no attempt, and succeeded one that records it succeeded.
		nothing, succeeded bool
		// causeHas is what the cause of a failed attempt must name.
		causeHas []string
	}{{
		name: "a mapped repository the App is not installed on",
		arrange: func(h *harness) {
			// The stand-in knows no repository under the row, so the installation lookup is 404.
			h.mapRepository(rowRepository)
		},
		causeHas: []string{"not installed", rowRepository},
	}, {
		name: "a mapped repository the App is not installed on, on a pass that holds GitHub's earlier answer",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			// A pass before the one measured asks GitHub and holds its answer, so the measured pass
			// asks GitHub nothing about the repository and must still note the fault it stands for.
			measured := h.reconciler.GitHubReads
			h.reconciler.GitHubReads = nil
			h.pass()
			h.reconciler.GitHubReads = measured
		},
		causeHas: []string{"not installed", rowRepository},
	}, {
		name: "a repository only an issue names that the App is not installed on",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.issueInRepository(uninstalledRow)
			h.listHead()
			h.github.setCheckRuns(gitHubRepository, headSHA,
				checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
			)
		},
		succeeded: true,
	}, {
		name: "a repository whose identity GitHub answers 404",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.github.hideRepository(rowRepository)
		},
		causeHas: []string{"cannot read the repository", rowRepository},
	}, {
		name: "a repository GitHub names with no owner",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			// A full_name that is not owner/name: an empty one is no answer at all (identify).
			h.github.setRepository(rowRepository, gitHubName, repositoryID)
		},
		causeHas: []string{"not owner/name", rowRepository},
	}, {
		name: "open pull requests GitHub refuses after it refused the query",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.listHead()
			h.github.answerStatus("/graphql", http.StatusUnprocessableEntity)
			h.github.answerStatus("/repos/"+gitHubRepository+"/pulls", http.StatusUnprocessableEntity)
		},
		causeHas: []string{"/repos/" + gitHubRepository + "/pulls", "422"},
	}, {
		name: "a head whose check runs GitHub refuses",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.listHead()
			// The query cannot answer the head's checks, so the pass reads them over REST.
			h.github.refuseChecks(gitHubRepository, headSHA)
			h.github.answerStatus(checkRunsPath, http.StatusUnprocessableEntity)
		},
		causeHas: []string{checkRunsPath, "422"},
	}, {
		name: "a head whose commit statuses GitHub refuses",
		arrange: func(h *harness) {
			// The token holds Commit statuses, so the query asks for the head's status field, and
			// GitHub answers it with an errors entry: the head is answered whole without them.
			h.headWithStatuses(tokenWithStatuses, commitStatus(101, "ci/build", "failure", "2026-11-17T05:01:00Z"))
			h.github.refuseStatuses(gitHubRepository, headSHA)
		},
		causeHas: []string{"commit status", gitHubRepository},
	}, {
		name: "a limit in force when the pass starts",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.listHead()
			if err := h.reconciler.Limits.Record(h.ctx, githubapp.AppScope, 10*time.Minute); err != nil {
				h.t.Fatalf("record a limit on the App: %v", err)
			}
		},
		causeHas: []string{"rate limit on " + githubapp.AppScope, "before it read any repository"},
	}, {
		name: "a query GitHub gave up on whose repository the listing reads",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.listHead()
			h.github.answerStatus("/graphql", http.StatusBadGateway)
		},
		succeeded: true,
	}, {
		name: "a pass its caller cancelled",
		arrange: func(h *harness) {
			h.mapRepository(rowRepository)
			h.listHead()
			// GitHub holds its answer about the repository past the moment the caller cancels.
			h.github.stall("/repos/"+rowRepository, time.Minute)
		},
		pass: func(h *harness) {
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.reconciler.Pass(ctx)
			}()
			for until := time.Now().Add(20 * time.Second); h.github.countPath("/repos/"+rowRepository) == 0; {
				if time.Now().After(until) {
					cancel()
					<-done
					h.t.Fatalf("the pass never asked GitHub about the repository:\n%s", h.logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			<-done
		},
		nothing: true,
	}} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			reads := h.recordReads()
			row.arrange(h)

			if row.pass != nil {
				row.pass(h)
			} else {
				h.pass()
			}

			got := reads.take()
			if row.nothing {
				if len(got) != 0 {
					t.Fatalf("the pass recorded %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 || got[0].read != githubreads.CIReconcile {
				t.Fatalf("the pass recorded %+v, want one %s attempt:\n%s", got, githubreads.CIReconcile, h.logs.String())
			}
			if row.succeeded {
				if !got[0].succeeded || got[0].cause != "" {
					t.Fatalf("the pass recorded %+v, want succeeded with no cause:\n%s", got[0], h.logs.String())
				}
				return
			}
			if got[0].succeeded {
				t.Fatalf("the pass recorded succeeded, want failed:\n%s", h.logs.String())
			}
			for _, text := range row.causeHas {
				if !strings.Contains(got[0].cause, text) {
					t.Fatalf("the recorded cause %q does not name %q", got[0].cause, text)
				}
			}
		})
	}
}

// webhookBody is one GitHub webhook body carrying payload as its event, with the repository and
// the pull request GitHub names for it: the delivery the listener would have had.
func webhookBody(event string, payload object) string {
	// The webhook attributes the object to its pull request as GitHub's own delivery does; a
	// listing carries no such field, which is why the pass takes the number from the pull
	// request it read the head of. It is no part of the observation either body maps to, so the
	// webhook's and the listing's are still the same observation.
	attributed := object{"pull_requests": []any{object{"number": 7}}}
	for key, value := range payload {
		attributed[key] = value
	}
	body, err := json.Marshal(object{
		"action": "completed",
		event:    attributed,
		"sender": object{"login": "ci", "type": "Bot"},
		"repository": object{
			"name":           gitHubName,
			"owner":          object{"login": gitHubOwner},
			"full_name":      gitHubRepository,
			"default_branch": "main",
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}
