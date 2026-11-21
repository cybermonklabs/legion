package cistore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The head these tests are about is cybermonklabs/legion#3513 at a368687e, read on 2026-11-21. Its
// pull_request run 44860489650 (check suite 121419707580) failed ci-ok in check run 136722779787.
// A workflow_dispatch run of the pull request's branch, run 44860519340 (suite 121419789924),
// passed ci-ok in check run 136723497213, a higher id. GitHub's rollup of the pull request answers
// ci-ok FAILURE: a workflow_dispatch run's checks are not evaluated for a pull request. Both suites
// are GitHub Actions' and list the pull request; only the event that started each run tells them
// apart, and only the workflow_run webhook and Dispatch's GraphQL read carry it.
//
// Each test drives the listener's own webhook handler with GitHub's webhook bodies, so what the
// store holds is what the listener records from GitHub's own deliveries.
const (
	countedOwner    = "example-org"
	countedRepo     = "example-repo"
	countedNumber   = 3513
	countedSHA      = "a368687e3a5a316f04533f1e83715eaf5e5663f7"
	countedSecret   = "s"
	pullRunSuite    = 121419707580
	pullRunCheck    = 136722779787
	dispatchSuite   = 121419789924
	dispatchCheck   = 136723497213
	countedCheckRun = "ci-ok"
)

// unpublished is the publisher of a handler whose deliveries publish nothing: a pull request's CI
// events are recorded, never published.
type unpublished struct{ t *testing.T }

func (p unpublished) PublishWithin(_ context.Context, envelope contracts.Envelope) error {
	p.t.Errorf("a CI delivery published %s", envelope.Topic)
	return nil
}

// countedPost sends the store's webhook handler one GitHub delivery, signed as GitHub signs it.
func countedPost(t *testing.T, handler http.Handler, event string, body map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the %s body: %v", event, err)
	}
	mac := hmac.New(sha256.New, []byte(countedSecret))
	mac.Write(raw)
	request := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(raw))
	request.Header.Set("X-GitHub-Delivery", fmt.Sprintf("%s-%d", event, time.Now().UnixNano()))
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the %s delivery was answered %d: %s", event, recorder.Code, recorder.Body.String())
	}
}

func countedRepository() map[string]any {
	return map[string]any{"name": countedRepo, "full_name": countedOwner + "/" + countedRepo, "owner": map[string]any{"login": countedOwner}, "default_branch": "main"}
}

func countedPulls(number int) []any {
	return []any{map[string]any{"number": number}}
}

// workflowRunBody is GitHub's workflow_run webhook for a run of check suite suite, started by
// event, of the pull request number's head.
func workflowRunBody(number int, suite int64, event string) map[string]any {
	return map[string]any{
		"action": "requested",
		"workflow_run": map[string]any{
			"id": suite + 1, "name": "CI", "path": ".github/workflows/ci.yaml", "event": event,
			"check_suite_id": suite, "head_sha": countedSHA, "head_branch": "feature",
			"status": "queued", "conclusion": nil, "pull_requests": countedPulls(number),
		},
		"repository": countedRepository(),
		"sender":     map[string]any{"login": "octocat", "type": "User"},
	}
}

// checkRunBody is GitHub's check_run webhook for a GitHub Actions job of check suite suite.
func checkRunBody(number int, id, suite int64, name, status, conclusion, at string) map[string]any {
	action := "completed"
	if status != "completed" {
		action = "created"
	}
	run := map[string]any{
		"id": id, "name": name, "status": status, "head_sha": countedSHA,
		"started_at": at, "html_url": "https://github.com/" + countedOwner + "/" + countedRepo + "/runs/" + strconv.FormatInt(id, 10),
		"app":           map[string]any{"id": 15368, "slug": "github-actions"},
		"check_suite":   map[string]any{"id": suite, "head_branch": "feature"},
		"pull_requests": countedPulls(number),
	}
	if status == "completed" {
		run["conclusion"] = conclusion
		run["completed_at"] = at
	}
	return map[string]any{"action": action, "check_run": run, "repository": countedRepository(), "sender": map[string]any{"login": "github-actions[bot]", "type": "Bot"}}
}

// checkSuiteBody is GitHub's check_suite webhook for GitHub Actions' suite suite.
func checkSuiteBody(number int, suite int64, status, conclusion, at string) map[string]any {
	object := map[string]any{
		"id": suite, "status": status, "head_sha": countedSHA, "head_branch": "feature", "updated_at": at,
		"app": map[string]any{"id": 15368, "slug": "github-actions"}, "pull_requests": countedPulls(number),
	}
	if conclusion != "" {
		object["conclusion"] = conclusion
	}
	return map[string]any{"action": "completed", "check_suite": object, "repository": countedRepository(), "sender": map[string]any{"login": "github-actions[bot]", "type": "Bot"}}
}

// countedDeliveries are the four deliveries of #3513's ci-ok, by the name a test orders them by.
func countedDeliveries(number int) map[string]func() (string, map[string]any) {
	return map[string]func() (string, map[string]any){
		"the pull_request run's event": func() (string, map[string]any) {
			return "workflow_run", workflowRunBody(number, pullRunSuite, "pull_request")
		},
		"the pull_request run's failing ci-ok": func() (string, map[string]any) {
			return "check_run", checkRunBody(number, pullRunCheck, pullRunSuite, countedCheckRun, "completed", "failure", "2026-11-21T04:30:00Z")
		},
		"the workflow_dispatch run's event": func() (string, map[string]any) {
			return "workflow_run", workflowRunBody(number, dispatchSuite, "workflow_dispatch")
		},
		"the workflow_dispatch run's passing ci-ok": func() (string, map[string]any) {
			return "check_run", checkRunBody(number, dispatchCheck, dispatchSuite, countedCheckRun, "completed", "success", "2026-11-21T04:40:00Z")
		},
	}
}

// requireCountedCheck fails unless the record of the head of number holds ci-ok as GitHub's rollup
// does: the pull_request run's failure.
func requireCountedCheck(t *testing.T, store *Store, number int, why string) {
	t.Helper()
	state := getState(t, store, countedOwner, countedRepo, strconv.Itoa(number), countedSHA)
	check, held := state.Checks[countedCheckRun]
	if !held || uint64(check.CheckRunID) != pullRunCheck || check.Conclusion != "failure" {
		t.Fatalf("%s: the head's ci-ok is %+v (held %v), want check run %d, the pull_request run's failure, as GitHub's rollup answers; checks %+v",
			why, check, held, uint64(pullRunCheck), state.Checks)
	}
}

// The pull_request run's ci-ok failure, and then a ci-ok success with a higher id from a
// workflow_dispatch run of the branch, leave the head's ci-ok a failure, and the settlement the
// listener publishes names it failing: GitHub does not evaluate the dispatched run's checks for the
// pull request. The deliveries come in the order GitHub sends them, each run's workflow_run event
// at its start, before its jobs.
func TestAWorkflowDispatchRunsCheckDoesNotReplaceThePullRequestRunsCheck(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	deliveries := countedDeliveries(countedNumber)
	for _, name := range []string{
		"the pull_request run's event",
		"the pull_request run's failing ci-ok",
		"the workflow_dispatch run's event",
		"the workflow_dispatch run's passing ci-ok",
	} {
		event, body := deliveries[name]()
		countedPost(t, handler, event, body)
	}
	requireCountedCheck(t, store, countedNumber, "after the dispatched run's success")

	number := strconv.Itoa(countedNumber)
	waitCacheChecks(t, store, countedOwner, countedRepo, number, countedSHA, 1)
	setLastEventAt(t, store, countedOwner, countedRepo, number, countedSHA, 0)
	pub := &recPub{}
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("settlements = %d, want 1", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode the settlement: %v", err)
	}
	assertCheckRuns(t, summary.CheckRuns, map[string]uint64{countedCheckRun: pullRunCheck})
	if strings.Join(summary.Failed.Checks, ",") != countedCheckRun || summary.Passed.Count != 0 {
		t.Fatalf("the settlement passed %v and failed %v, want ci-ok failing alone", summary.Passed.Checks, summary.Failed.Checks)
	}
	// The dispatched run's event came before its run, so the record never counted the run, and no
	// settlement can have named it: none is excluded.
	if len(summary.ExcludedCheckRuns) != 0 {
		t.Fatalf("the settlement names excluded check runs %+v, want none: the record never counted the dispatched run", summary.ExcludedCheckRuns)
	}
}

// The same deliveries in the reverse order leave the same ci-ok: the dispatched run's success
// comes first, before its event, so the store counts it as it counts any run whose suite's event it
// does not know; the pull_request run's failure, the lower id, then waits to stand in its place, and
// stands once the dispatched run's event says GitHub does not count that run. The settlement then
// names the dispatched run among its excluded check runs, since a settlement could have named it
// while it stood, and in no other part.
func TestTheReverseOrderLeavesThePullRequestRunsCheck(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	deliveries := countedDeliveries(countedNumber)
	for _, name := range []string{
		"the workflow_dispatch run's passing ci-ok",
		"the workflow_dispatch run's event",
		"the pull_request run's failing ci-ok",
		"the pull_request run's event",
	} {
		event, body := deliveries[name]()
		countedPost(t, handler, event, body)
	}
	requireCountedCheck(t, store, countedNumber, "in the reverse order")
	summary := renderSummary(getState(t, store, countedOwner, countedRepo, strconv.Itoa(countedNumber), countedSHA))
	if want := []CheckRunRef{{Name: countedCheckRun, ID: dispatchCheck}}; !slices.Equal(summary.ExcludedCheckRuns, want) {
		t.Fatalf("the settlement names excluded check runs %+v, want %+v, the dispatched run the record counted before its event", summary.ExcludedCheckRuns, want)
	}
	assertCheckRuns(t, summary.CheckRuns, map[string]uint64{countedCheckRun: pullRunCheck})
}

// Which ci-ok the head holds does not depend on the order the two check runs and the two runs'
// events arrive in: in every one of the 24 orders the head holds the pull_request run's failure.
// The orders where the dispatched run's success comes before its event are the ones a record
// cannot get right by refusing a run: the store first counts that run, as it counts any run whose
// suite's event it does not know, and the failure it refused as older waits to stand in its place.
func TestTheCountedCheckStandsWhateverOrderTheRunsAndTheirEventsArriveIn(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	short := map[string]string{
		"the pull_request run's event":              "pr-event",
		"the pull_request run's failing ci-ok":      "pr-failure",
		"the workflow_dispatch run's event":         "dispatch-event",
		"the workflow_dispatch run's passing ci-ok": "dispatch-success",
	}
	names := make([]string, 0, len(short))
	for name := range short {
		names = append(names, name)
	}
	slices.Sort(names)
	orders := permutations(names)
	if len(orders) != 24 {
		t.Fatalf("%d orders of four deliveries, want 24", len(orders))
	}
	for i, order := range orders {
		label := make([]string, len(order))
		for j, name := range order {
			label[j] = short[name]
		}
		t.Run(strings.Join(label, ","), func(t *testing.T) {
			// Each order is a head of its own, so no order's deliveries reach another's record.
			number := countedNumber + i
			deliveries := countedDeliveries(number)
			for _, name := range order {
				event, body := deliveries[name]()
				countedPost(t, handler, event, body)
			}
			requireCountedCheck(t, store, number, "in the order "+strings.Join(order, ", then "))
		})
	}
}

// A suite whose event the store never learns is counted, as every suite was before the store read
// events: with no workflow_run delivery at all, the dispatched run's success, the higher id, stands.
// It is the rule the record keeps where the event cannot be read, and it holds at main too.
func TestASuiteWhoseEventNeverArrivesIsCountedAsBefore(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	deliveries := countedDeliveries(countedNumber)
	for _, name := range []string{"the pull_request run's failing ci-ok", "the workflow_dispatch run's passing ci-ok"} {
		event, body := deliveries[name]()
		countedPost(t, handler, event, body)
	}
	state := getState(t, store, countedOwner, countedRepo, strconv.Itoa(countedNumber), countedSHA)
	if check := state.Checks[countedCheckRun]; uint64(check.CheckRunID) != dispatchCheck || check.Conclusion != "success" {
		t.Fatalf("with no event known the head's ci-ok is %+v, want check run %d, the higher id, as before the store read events", check, uint64(dispatchCheck))
	}
}

// A re-run of a counted run replaces the check of its name: a ci-ok success with a higher id in
// the pull_request run's own suite stands in place of that suite's failure.
func TestARerunInThePullRequestRunsSuiteReplacesItsCheck(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	countedPost(t, handler, "workflow_run", workflowRunBody(countedNumber, pullRunSuite, "pull_request"))
	countedPost(t, handler, "check_run", checkRunBody(countedNumber, pullRunCheck, pullRunSuite, countedCheckRun, "completed", "failure", "2026-11-21T04:30:00Z"))
	const rerun = pullRunCheck + 5000
	countedPost(t, handler, "check_run", checkRunBody(countedNumber, rerun, pullRunSuite, countedCheckRun, "completed", "success", "2026-11-21T04:50:00Z"))
	state := getState(t, store, countedOwner, countedRepo, strconv.Itoa(countedNumber), countedSHA)
	if check := state.Checks[countedCheckRun]; uint64(check.CheckRunID) != rerun || check.Conclusion != "success" {
		t.Fatalf("after the re-run the head's ci-ok is %+v, want the re-run %d's success", check, uint64(rerun))
	}
}

// A completed pull_request suite settles its head while a workflow_dispatch run of the branch is
// still running beside it: the dispatched run's suite and its running check runs are not the
// pull request's, so they hold the head no more than GitHub's rollup holds the pull request. When
// the dispatched run then finishes, the settled head is not settled again: its checks, its snapshot
// and its quiet period are as they were.
func TestARunningWorkflowDispatchRunDoesNotHoldThePullRequestsHead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	handler := webhook.GitHubHandler(countedSecret, "@legion", "", unpublished{t}, store)
	number := strconv.Itoa(countedNumber)
	countedPost(t, handler, "workflow_run", workflowRunBody(countedNumber, pullRunSuite, "pull_request"))
	countedPost(t, handler, "workflow_run", workflowRunBody(countedNumber, dispatchSuite, "workflow_dispatch"))
	countedPost(t, handler, "check_run", checkRunBody(countedNumber, pullRunCheck, pullRunSuite, countedCheckRun, "completed", "failure", "2026-11-21T04:30:00Z"))
	countedPost(t, handler, "check_suite", checkSuiteBody(countedNumber, pullRunSuite, "completed", "failure", "2026-11-21T04:30:01Z"))
	countedPost(t, handler, "check_suite", checkSuiteBody(countedNumber, dispatchSuite, "in_progress", "", "2026-11-21T04:31:00Z"))
	countedPost(t, handler, "check_run", checkRunBody(countedNumber, dispatchCheck, dispatchSuite, countedCheckRun, "in_progress", "", "2026-11-21T04:31:00Z"))

	waitCacheChecks(t, store, countedOwner, countedRepo, number, countedSHA, 1)
	setLastEventAt(t, store, countedOwner, countedRepo, number, countedSHA, 0)
	pub := &recPub{}
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("settlements = %d while the dispatched run runs, want 1: %+v", got, getState(t, store, countedOwner, countedRepo, number, countedSHA))
	}
	settled := getState(t, store, countedOwner, countedRepo, number, countedSHA)
	if _, held := settled.Suites[strconv.FormatInt(dispatchSuite, 10)]; held {
		t.Fatalf("the record holds the dispatched run's suite: %+v", settled.Suites)
	}

	countedPost(t, handler, "check_run", checkRunBody(countedNumber, dispatchCheck, dispatchSuite, countedCheckRun, "completed", "success", "2026-11-21T04:40:00Z"))
	countedPost(t, handler, "check_suite", checkSuiteBody(countedNumber, dispatchSuite, "completed", "success", "2026-11-21T04:40:01Z"))
	after := getState(t, store, countedOwner, countedRepo, number, countedSHA)
	if !after.SettledEmitted || after.Generation != settled.Generation || after.LastEventAt != settled.LastEventAt || after.Hash() != settled.Hash() {
		t.Fatalf("the dispatched run's completion moved the settled head: settled_emitted %v, generation %d then %d, last_event_at %d then %d",
			after.SettledEmitted, settled.Generation, after.Generation, settled.LastEventAt, after.LastEventAt)
	}
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("settlements = %d after the dispatched run finished, want the 1 before it", got)
	}
}

// permutations is every order of items.
func permutations(items []string) [][]string {
	if len(items) <= 1 {
		return [][]string{append([]string(nil), items...)}
	}
	var out [][]string
	for i := range items {
		rest := append(append([]string(nil), items[:i]...), items[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]string{items[i]}, tail...))
		}
	}
	return out
}
