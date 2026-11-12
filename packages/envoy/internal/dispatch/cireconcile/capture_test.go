package cireconcile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	head := headOf(capturedPull(t), false)
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

// The runs and the completed suites the listener recorded from GitHub's own webhooks, read
// again by a pass whose answer is GitHub's own captured GraphQL page, change no record and
// publish no settlement: the pass writes the observations the webhooks already wrote, and
// cistore applies an unchanged observation as no change. It is k5 end to end.
func TestAPassOverACapturedAnswerChangesNothingTheWebhooksRecorded(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(capturedRepository)
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
	if _, err := settlements.NextMsg(15 * time.Second); err != nil {
		t.Fatalf("the webhooks published no settlement: %v", err)
	}
	before := capturedRevision(t, h)

	h.pass()

	if message, err := settlements.NextMsg(2 * time.Second); err == nil {
		t.Fatalf("the pass published a settlement over what the webhooks recorded: %s", message.Data)
	}
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

// capturedRevision is the head's record revision once the summary loop has stopped writing it:
// publishing a settlement is itself two writes of the record, so a revision read the moment a
// settlement arrives still moves afterwards.
func capturedRevision(t *testing.T, h *harness) uint64 {
	t.Helper()
	var revision uint64
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		state, next := h.recordAt(capturedRepository, capturedNumber, capturedSHA)
		if state != nil && state.SettledEmitted && state.Claim == nil && next == revision {
			return revision
		}
		revision = next
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the head's record did not settle: %+v", h.stateAt(capturedRepository, capturedNumber, capturedSHA))
	return 0
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
