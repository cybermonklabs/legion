package cireconcile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The head the captured answers below are of: the oldest open pull request of
// cybermonklabs/legion when they were captured, its head commit, and the 21 check runs and 9
// check suites of that commit all the answers carry.
//
// testdata/graphql-page.json is one page of headsQuery's own answer, captured with
// `gh api graphql` against the real API; the only thing changed from the query this pass sends
// is that it asked for one open pull request rather than twenty, which keeps the fixture
// readable and changes no field of what GitHub answers about a head.
// testdata/rest-check-runs.json and testdata/rest-check-suites.json are the same commit's two
// REST listings, captured the same way (`filter=all` for the runs, as the pass reads them),
// each object kept down to the fields an observation reads and the two it is keyed by.
//
// testdata/graphql-page-statuses.json and testdata/rest-commit-statuses.json are one head's
// commit statuses as headsQueryWithStatuses and the REST listing of the commit's combined status
// answer them. They are not captured from GitHub: no repository a test here may name carried a
// commit status that the pod could read when they were written. They are built from the fields
// GitHub documents for a StatusContext (context, state, createdAt, targetUrl and description, the
// state one of ERROR, EXPECTED, FAILURE, PENDING and SUCCESS) and for each status of the combined
// status of a reference (context, state, created_at, target_url and description), under the
// placeholder repository and head the harness reconciles (Acme/Widgets#7). They hold a status in
// each state either read answers, a link and a description answered null, and the head's one
// check suite and its one run.
const (
	capturedRepository = "cybermonklabs/legion"
	capturedOwner      = "cybermonklabs"
	capturedName       = "legion"
	capturedNumber     = 1827
	capturedNumberText = "1827"
	capturedSHA        = "5ec911e4babe60fcb3dff684f6faf71826e4b76a"
	capturedRunID      = 112521222144
	capturedSuiteID    = 101684831035
	capturedCheckName  = "test"
)

// captured is one file of testdata, read whole.
func captured(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read the captured answer %s: %v", name, err)
	}
	return string(body)
}

// capturedObjects is one captured REST listing as the webhook's own bodies carry its objects.
func capturedObjects(t *testing.T, name string) []object {
	t.Helper()
	var items []object
	if err := json.Unmarshal([]byte(captured(t, name)), &items); err != nil {
		t.Fatalf("the captured listing %s is not JSON: %v", name, err)
	}
	if len(items) == 0 {
		t.Fatalf("the captured listing %s is empty", name)
	}
	return items
}

// capturedPull is the one open pull request of the captured GraphQL page, as GitHub answered
// it: the node the pass maps a head out of.
func capturedPull(t *testing.T) *graphQLPull {
	t.Helper()
	var answer graphQLAnswer
	if err := json.Unmarshal([]byte(captured(t, "graphql-page.json")), &answer); err != nil {
		t.Fatalf("the captured GraphQL answer is not GitHub's envelope: %v", err)
	}
	if answer.Data.Repository == nil || len(answer.Data.Repository.PullRequests.Nodes) == 0 {
		t.Fatalf("the captured GraphQL answer holds no open pull request: %+v", answer)
	}
	return answer.Data.Repository.PullRequests.Nodes[0]
}

// capturedHead is the pass's own mapping of the captured GraphQL page: the head GitHub answered
// and the check objects it holds, through the code the pass runs.
func capturedHead(t *testing.T) queryHead {
	t.Helper()
	head := headOf(capturedPull(t), false, nil)
	if head.overflow != "" {
		t.Fatalf("the captured answer's head is one the pass would read over REST: %s", head.overflow)
	}
	return head
}

// observationsOf is each object of a listing as the observation the CI record is written from,
// keyed by the check run id or the suite id it is recorded under.
func observationsOf(t *testing.T, event string, items []object) map[string]contracts.CIObservation {
	t.Helper()
	mapped := map[string]contracts.CIObservation{}
	for _, item := range items {
		observation, ok := contracts.GithubCIObservation(event, item, capturedOwner, capturedName, capturedNumberText)
		if !ok {
			t.Fatalf("a captured %s maps to no observation: %v", event, item)
		}
		key := observation.SuiteID
		if event == "check_run" {
			key = fmt.Sprint(observation.CheckRunID)
		}
		mapped[key] = observation
	}
	return mapped
}

// GitHub's own GraphQL answer and GitHub's own REST listings, for one real head, map to the
// very same observations: field for field, object for object, the observations the webhook's
// own bodies make. It is k5's mapping, read from answers captured from the real API rather
// than from a stand-in's idea of them.
func TestGitHubsQueryAndItsRESTListingsMapToOneSetOfObservations(t *testing.T) {
	head := capturedHead(t)
	if head.sha != capturedSHA {
		t.Fatalf("the captured answer is of commit %s, want %s", head.sha, capturedSHA)
	}

	for _, listing := range []struct {
		event   string
		queried []object
		file    string
	}{
		{event: "check_run", queried: head.runs, file: "rest-check-runs.json"},
		{event: "check_suite", queried: head.suites, file: "rest-check-suites.json"},
	} {
		fromQuery := observationsOf(t, listing.event, listing.queried)
		fromREST := observationsOf(t, listing.event, capturedObjects(t, listing.file))
		if len(fromQuery) != len(fromREST) {
			t.Errorf("the query answered %d %s objects and the REST listing %d", len(fromQuery), listing.event, len(fromREST))
		}
		for key, listed := range fromREST {
			queried, held := fromQuery[key]
			if !held {
				t.Errorf("the query's answer holds no %s %s, which its REST listing does", listing.event, key)
				continue
			}
			if queried != listed {
				t.Errorf("%s %s maps from the query to\n  %+v\nand from the REST listing to\n  %+v", listing.event, key, queried, listed)
			}
		}
	}

	// The one run and the one suite k5 names, read out of the same answers by hand, so what
	// the comparison above is about is named once in full.
	queried := observationsOf(t, "check_run", head.runs)[fmt.Sprint(capturedRunID)]
	if queried.CheckName != capturedCheckName || queried.Status != "completed" || queried.Conclusion != "success" {
		t.Errorf("run %d maps from the query to %+v, want the completed, passing %q the webhook recorded",
			capturedRunID, queried, capturedCheckName)
	}
	suite := observationsOf(t, "check_suite", head.suites)[fmt.Sprint(capturedSuiteID)]
	if suite.Status != "completed" || suite.Conclusion != "success" || suite.AppID != "15368" {
		t.Errorf("suite %d maps from the query to %+v, want the completed, passing suite of App 15368", capturedSuiteID, suite)
	}
}

// statusPagePull is the one open pull request of the GraphQL page that carries commit statuses
// (testdata/graphql-page-statuses.json), built from the fields GitHub documents for a
// StatusContext rather than captured.
func statusPagePull(t *testing.T) *graphQLPull {
	t.Helper()
	var answer graphQLAnswer
	if err := json.Unmarshal([]byte(captured(t, "graphql-page-statuses.json")), &answer); err != nil {
		t.Fatalf("the GraphQL answer with statuses is not GitHub's envelope: %v", err)
	}
	if answer.Data.Repository == nil || len(answer.Data.Repository.PullRequests.Nodes) != 1 {
		t.Fatalf("the GraphQL answer with statuses holds no one open pull request: %+v", answer)
	}
	return answer.Data.Repository.PullRequests.Nodes[0]
}

// statusObservationsOf is each status object as the observation the CI record is written from,
// keyed by its context, leaving out every object the mapping maps to none.
func statusObservationsOf(items []object) map[string]contracts.StatusObservation {
	mapped := map[string]contracts.StatusObservation{}
	for _, item := range items {
		if observation, ok := contracts.GithubStatusObservation(item, gitHubOwner, gitHubName, pullNumber, headSHA); ok {
			mapped[observation.Context] = observation
		}
	}
	return mapped
}

// One head's commit statuses in the shape of GitHub's query and in that of its REST listing, both
// built from the fields GitHub documents for a StatusContext and for the combined status of a
// reference (testdata/graphql-page-statuses.json, testdata/rest-commit-statuses.json), map to the
// very same status observations, field for field, and to no more than the statuses in success,
// failure or error: a pending status, and GraphQL's EXPECTED, map to none either way. A status
// field GitHub refused costs the head its statuses alone: its check objects map as before.
func TestGitHubsQueryAndItsRESTListingMapAHeadsStatusesToOneSetOfObservations(t *testing.T) {
	head := headOf(statusPagePull(t), false, nil)
	if head.overflow != "" || head.sha != headSHA {
		t.Fatalf("the answer's head is %q, read over REST for %q; want %s answered whole", head.sha, head.overflow, headSHA)
	}
	if len(head.statuses) != 3 {
		t.Fatalf("the query's head carries %d statuses to record, want the three in success, failure and error: %v", len(head.statuses), head.statuses)
	}
	fromQuery := statusObservationsOf(head.statuses)
	fromREST := statusObservationsOf(capturedObjects(t, "rest-commit-statuses.json"))
	want := map[string]string{"ci/build": "success", "ci/lint": "failure", "ci/license": "error"}
	for _, observations := range []map[string]contracts.StatusObservation{fromQuery, fromREST} {
		if len(observations) != len(want) {
			t.Fatalf("the statuses map to %v, want %v and no pending or expected one", observations, want)
		}
		for context, state := range want {
			if observations[context].State != state {
				t.Fatalf("status %s maps to %+v, want state %s", context, observations[context], state)
			}
		}
	}
	for context, listed := range fromREST {
		if queried := fromQuery[context]; queried != listed {
			t.Errorf("status %s maps from the query to\n  %+v\nand from the REST listing to\n  %+v", context, queried, listed)
		}
	}
	if license := fromQuery["ci/license"]; license.URL != "" || license.Description != "" || license.CreatedAt != "2026-11-17T05:03:00Z" {
		t.Errorf("a status with no link and no description maps to %+v, want both empty and its time kept", license)
	}

	entries := []graphQLError{{Type: "FORBIDDEN", Message: "Resource not accessible by integration",
		Path: []any{"repository", "pullRequests", "nodes", float64(0), "commits", "nodes", float64(0), "commit", "status"}}}
	refused := headOf(statusPagePull(t), false, entries)
	if refused.overflow != "" || len(refused.statuses) != 0 {
		t.Fatalf("a head whose status field GitHub refused is read over REST for %q with statuses %v; want it answered whole with none", refused.overflow, refused.statuses)
	}
	if !reflect.DeepEqual(refused.statusesRefused, entries) {
		t.Fatalf("a head whose status field GitHub refused keeps the entries %+v, want %+v for the pass to name at WARN", refused.statusesRefused, entries)
	}
	if len(refused.runs) != len(head.runs) || len(refused.suites) != len(head.suites) {
		t.Fatalf("a refused status field left %d runs and %d suites, want the head's %d and %d", len(refused.runs), len(refused.suites), len(head.runs), len(head.suites))
	}
}

// The runs and the completed suites the listener recorded from GitHub's own webhooks, read
// again by a pass whose answer is GitHub's own captured GraphQL page, change no record and
// publish no settlement: the pass writes the observations the webhooks already wrote, and
// cistore applies an unchanged observation as no change. It is k5 end to end.
func TestAPassOverACapturedAnswerChangesNothingTheWebhooksRecorded(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(capturedRepository)
	h.github.setRepository(capturedRepository, capturedRepository, 555)
	// The one open pull request the captured page answers, and the captured page itself as the
	// stand-in's answer to the pass's query about it.
	h.github.setPullRequests(capturedRepository, fakePull{number: capturedNumber, sha: capturedSHA})
	h.github.answerQueryWith(capturedRepository, captured(t, "graphql-page.json"))

	subject := "notifications.github." + capturedOwner + "." + capturedName + ".pr." + capturedNumberText + ".checks"
	settlements, err := h.bus.Conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = settlements.Unsubscribe() })

	// The listener's own webhooks, carrying the very objects GitHub's REST listings answer
	// with: every run of the head, and every suite of it that has completed.
	delivery := 0
	for _, item := range capturedObjects(t, "rest-check-runs.json") {
		delivery++
		h.post("check_run", capturedWebhook(t, "check_run", item), fmt.Sprintf("run-%d", delivery))
	}
	for _, item := range capturedObjects(t, "rest-check-suites.json") {
		if item["status"] != "completed" {
			continue
		}
		delivery++
		h.post("check_suite", capturedWebhook(t, "check_suite", item), fmt.Sprintf("suite-%d", delivery))
	}
	h.settlement(settlements, "the webhooks' settlement of the captured head")
	_, before := h.settledRecordAt(capturedRepository, capturedNumber, capturedSHA)

	h.pass()

	// The summary loop publishes a settlement only through two writes of the record, so the
	// record still at the revision it settled at, read below, is the proof that the pass made it
	// publish none.
	h.noSettlement(settlements, "after the pass over what the webhooks recorded")
	state, after := h.recordAt(capturedRepository, capturedNumber, capturedSHA)
	if state == nil {
		t.Fatal("the head has no record after the pass")
	}
	if after != before {
		t.Fatalf("the record's revision moved from %d to %d; the pass rewrote what the webhooks recorded: %+v", before, after, state)
	}
	if check, held := state.Checks[capturedCheckName]; !held || uint64(check.CheckRunID) != uint64(capturedRunID) {
		t.Fatalf("the record holds %+v, want the run the webhook recorded", state.Checks)
	}
	if _, held := state.Suites[fmt.Sprint(capturedSuiteID)]; !held {
		t.Fatalf("the record holds suites %+v, want the one the webhook recorded", state.Suites)
	}
	if got := number(t, h.passLine(), "records"); got != 0 {
		t.Fatalf("the pass changed %d records, want none: %v", got, h.passLine())
	}
}

// capturedWebhook is one GitHub webhook body carrying a captured object as its event, named for
// the repository and the pull request the capture is of.
func capturedWebhook(t *testing.T, event string, payload object) string {
	t.Helper()
	attributed := object{"pull_requests": []any{object{"number": capturedNumber}}}
	for key, value := range payload {
		attributed[key] = value
	}
	body, err := json.Marshal(object{
		"action": "completed",
		event:    attributed,
		"sender": object{"login": "ci", "type": "Bot"},
		"repository": object{
			"name":           capturedName,
			"owner":          object{"login": capturedOwner},
			"full_name":      capturedRepository,
			"default_branch": "main",
		},
	})
	if err != nil {
		t.Fatalf("build the captured webhook body: %v", err)
	}
	return string(body)
}
