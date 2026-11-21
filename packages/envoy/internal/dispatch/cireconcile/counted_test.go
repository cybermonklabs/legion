package cireconcile

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The head the tests below read is cybermonklabs/legion#3513 at a368687e, read on 2026-11-21.
//
// testdata/graphql-head-3513.json is that commit as headsQuery asks for it, workflowRun { event }
// of each check suite included, captured with `gh api graphql` against the real API. The commit
// was read by its oid, since the pull request's head has moved on since, and its answer is set
// under the one pull request node of headsQuery's own envelope; it was read again at 06:57Z and
// answered byte for byte the same. Its suite 121419707580 is the pull_request run 44860489650,
// whose ci-ok failed in check run 136722779787. Its suite 121419789924 is a workflow_dispatch run
// of the pull request's branch, run 44860519340, whose ci-ok passed in check run 136723497213, a
// higher id, and whose jobs share every name of the pull_request run's.
//
// testdata/graphql-rollup-3513.json is GitHub's own rollup of the pull request at that commit,
// captured the same way at 06:57Z: statusCheckRollup with each check run's id, name, conclusion,
// whether the pull request requires it, and its suite's workflow run event. It answers FAILURE,
// ci-ok is check run 136722779787, and each of its 74 check runs is of a pull_request run or of
// an App other than GitHub Actions.
const (
	countedNumber        = 3513
	countedNumberText    = "3513"
	countedSHA           = "a368687e3a5a316f04533f1e83715eaf5e5663f7"
	countedCheck         = "ci-ok"
	countedPullCheck     = 136722779787
	countedDispatchCheck = 136723497213
	countedDispatchSuite = "121419789924"
)

// rollupCheck is one check run of GitHub's rollup of a pull request.
type rollupCheck struct {
	ID         uint64
	Conclusion string
}

// countedRollup is GitHub's rollup of #3513 at countedSHA (testdata/graphql-rollup-3513.json): the
// check runs GitHub counts for the pull request, by name.
func countedRollup(t *testing.T) map[string]rollupCheck {
	t.Helper()
	checks := rollupOf(t, "graphql-rollup-3513.json", "FAILURE")
	if got := checks[countedCheck]; got.ID != countedPullCheck || got.Conclusion != "failure" {
		t.Fatalf("the captured rollup's ci-ok is %+v, want check run %d's failure", got, uint64(countedPullCheck))
	}
	return checks
}

// rollupOf is the captured rollup of a pull request in file, which must answer state with every
// context: the check runs GitHub counts for the pull request, by name. Where the rollup holds two
// counted runs of one name, the one of higher id stands for it, as the CI store keeps the newest
// run of each name.
func rollupOf(t *testing.T, file, state string) map[string]rollupCheck {
	t.Helper()
	var answer struct {
		Data struct {
			Repository struct {
				Object struct {
					StatusCheckRollup struct {
						State    string `json:"state"`
						Contexts struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								Typename   string `json:"__typename"`
								DatabaseID uint64 `json:"databaseId"`
								Name       string `json:"name"`
								Conclusion string `json:"conclusion"`
							} `json:"nodes"`
						} `json:"contexts"`
					} `json:"statusCheckRollup"`
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(captured(t, file)), &answer); err != nil {
		t.Fatalf("the captured rollup %s is not GitHub's envelope: %v", file, err)
	}
	rollup := answer.Data.Repository.Object.StatusCheckRollup
	if rollup.State != state || rollup.Contexts.TotalCount != len(rollup.Contexts.Nodes) {
		t.Fatalf("the captured rollup %s answers %s with %d of %d contexts, want %s with every context",
			file, rollup.State, len(rollup.Contexts.Nodes), rollup.Contexts.TotalCount, state)
	}
	checks := map[string]rollupCheck{}
	for _, node := range rollup.Contexts.Nodes {
		if node.Typename != "CheckRun" {
			continue
		}
		if held, twice := checks[node.Name]; twice && held.ID > node.DatabaseID {
			continue
		}
		checks[node.Name] = rollupCheck{ID: node.DatabaseID, Conclusion: strings.ToLower(node.Conclusion)}
	}
	return checks
}

// differFromRollup is each check of a record that differs from GitHub's rollup of its pull request,
// and each check of the rollup the record lacks, one line each in name order.
func differFromRollup(state *cistore.State, rollup map[string]rollupCheck) []string {
	var differ []string
	for name, want := range rollup {
		got, held := state.Checks[name]
		if !held || uint64(got.CheckRunID) != want.ID || got.Conclusion != want.Conclusion {
			differ = append(differ, fmt.Sprintf("%s: record %d %q (held %v), rollup %d %q", name, uint64(got.CheckRunID), got.Conclusion, held, want.ID, want.Conclusion))
		}
	}
	for name, got := range state.Checks {
		if _, counted := rollup[name]; !counted {
			differ = append(differ, fmt.Sprintf("%s: record %d %q, not in the rollup", name, uint64(got.CheckRunID), got.Conclusion))
		}
	}
	slices.Sort(differ)
	return differ
}

// countedCiOKRuns is each ci-ok check run of the captured answer for #3513's head, as the REST
// listing and a check_run webhook carry one, with no App and no suite: what a check of a record
// written by a listener of an earlier release holds, which kept neither.
func countedCiOKRuns(t *testing.T) []object {
	t.Helper()
	var answer graphQLAnswer
	if err := json.Unmarshal([]byte(captured(t, "graphql-head-3513.json")), &answer); err != nil {
		t.Fatalf("the captured answer is not GitHub's envelope: %v", err)
	}
	var runs []object
	for _, suite := range answer.Data.Repository.PullRequests.Nodes[0].Commits.Nodes[0].Commit.CheckSuites.Nodes {
		for _, run := range suite.CheckRuns.Nodes {
			if run.Name != countedCheck {
				continue
			}
			runs = append(runs, object{
				"id": run.DatabaseID, "name": run.Name, "head_sha": countedSHA,
				"status": strings.ToLower(run.Status), "conclusion": strings.ToLower(run.Conclusion),
				"started_at": run.StartedAt, "completed_at": run.CompletedAt, "html_url": run.Permalink,
				"pull_requests": []any{object{"number": countedNumber}},
			})
		}
	}
	if len(runs) != 2 {
		t.Fatalf("the captured answer holds %d ci-ok runs, want the pull_request run's and the dispatched run's", len(runs))
	}
	return runs
}

// countedWebhook is the check_run webhook of #3513 carrying run.
func countedWebhook(t *testing.T, run object) string {
	t.Helper()
	body, err := json.Marshal(object{
		"action":    "completed",
		"check_run": run,
		"sender":    object{"login": "github-actions[bot]", "type": "Bot"},
		"repository": object{
			"name": capturedName, "owner": object{"login": capturedOwner},
			"full_name": capturedRepository, "default_branch": "main",
		},
	})
	if err != nil {
		t.Fatalf("build the check_run webhook: %v", err)
	}
	return string(body)
}

// countedSettled is #3513's record once the summary loop has settled it and stopped writing it.
func countedSettled(t *testing.T, h *harness) cistore.State {
	t.Helper()
	return settledAt(t, h, countedNumber, countedSHA)
}

// settledAt is the record of pull request number of the captured repository at sha once the
// summary loop has settled it and stopped writing it.
func settledAt(t *testing.T, h *harness, number int64, sha string) cistore.State {
	t.Helper()
	var revision uint64
	var state *cistore.State
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		next, nextRevision := h.recordAt(capturedRepository, number, sha)
		if next != nil && next.SettledEmitted && next.Claim == nil && nextRevision == revision {
			return *next
		}
		state, revision = next, nextRevision
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the head's record did not settle: %+v", state)
	return cistore.State{}
}

// lastSettlement is the last settlement of the head published to sub within a short wait, which
// must be one at least.
func lastSettlement(t *testing.T, sub *nats.Subscription, why string) cistore.Summary {
	t.Helper()
	var last *nats.Msg
	for {
		message, err := sub.NextMsg(time.Second)
		if err != nil {
			break
		}
		last = message
	}
	if last == nil {
		t.Fatalf("%s: no settlement was published", why)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(last.Data, &envelope); err != nil {
		t.Fatalf("%s: the settlement is not an envelope: %v", why, err)
	}
	var summary cistore.Summary
	if err := json.Unmarshal([]byte(envelope.Payload), &summary); err != nil {
		t.Fatalf("%s: the settlement's payload is not JSON: %v", why, err)
	}
	return summary
}

// settledCiOK is the ci-ok run a settlement names, and whether it names ci-ok failing.
func settledCiOK(summary cistore.Summary) (id uint64, failing bool) {
	for _, run := range summary.CheckRuns {
		if run.Name == countedCheck {
			id = run.ID
		}
	}
	return id, slices.Contains(summary.Failed.Checks, countedCheck)
}

// A record a listener of an earlier release wrote -- it kept the newest check run of each name
// whatever started its run, so the workflow_dispatch run's ci-ok success stands for ci-ok -- is
// corrected by a pass over GitHub's answer for the head, which carries the event that started each
// suite's workflow run. The head's checks are then GitHub's own rollup of the pull request, check
// run for check run, ci-ok the pull_request run's failure among them; the dispatched run's suite
// is not in the record; no suite is named as one whose event the pass could not read; and the
// settlement the listener publishes next names ci-ok failing and names the dispatched run's
// ci-ok, which the earlier settlement named, among its excluded check runs.
func TestAPassCountsAHeadsCheckRunsAsGitHubsRollupOfThePullRequestDoes(t *testing.T) {
	rollup := countedRollup(t)
	h := newHarness(t, reviewerAppID)
	h.mapRepository(capturedRepository)
	h.github.setRepository(capturedRepository, capturedRepository, 555)
	h.github.setPullRequests(capturedRepository, fakePull{number: countedNumber, sha: countedSHA})
	h.github.answerQueryWith(capturedRepository, captured(t, "graphql-head-3513.json"))

	subject := "notifications.github." + capturedOwner + "." + capturedName + ".pr." + countedNumberText + ".checks"
	settlements, err := h.bus.Conn.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = settlements.Unsubscribe() })

	for i, run := range countedCiOKRuns(t) {
		h.post("check_run", countedWebhook(t, run), fmt.Sprintf("earlier-release-%d", i))
	}
	countedSettled(t, h)
	if id, failing := settledCiOK(lastSettlement(t, settlements, "the earlier release's record")); id != countedDispatchCheck || failing {
		t.Fatalf("before the pass the settlement names ci-ok %d (failing %v), want the dispatched run's success %d, which a record of an earlier release holds",
			id, failing, uint64(countedDispatchCheck))
	}

	h.pass()

	state := countedSettled(t, h)
	if differ := differFromRollup(&state, rollup); len(differ) > 0 {
		t.Errorf("after the pass %d of the record's %d checks differ from GitHub's rollup of %d:\n  %s",
			len(differ), len(state.Checks), len(rollup), strings.Join(differ, "\n  "))
	}
	if check := state.Checks[countedCheck]; uint64(check.CheckRunID) != countedPullCheck || check.Conclusion != "failure" {
		t.Errorf("after the pass the head's ci-ok is %+v, want check run %d, the pull_request run's failure, as GitHub's rollup answers",
			check, uint64(countedPullCheck))
	}
	if suite, held := state.Suites[countedDispatchSuite]; held {
		t.Errorf("the record holds the workflow_dispatch run's suite %+v", suite)
	}
	if lines := append(h.lines(eventlessQueriedLine), h.lines(eventlessRESTLine)...); len(lines) != 0 {
		t.Errorf("the pass named suites whose event the record does not hold, in an answer that carries every GitHub Actions suite's event: %v", lines)
	}
	summary := lastSettlement(t, settlements, "after the pass")
	if id, failing := settledCiOK(summary); id != countedPullCheck || !failing {
		t.Errorf("after the pass the settlement names ci-ok %d (failing %v), want the pull_request run's failure %d",
			id, failing, uint64(countedPullCheck))
	}
	if !slices.Contains(summary.ExcludedCheckRuns, cistore.CheckRunRef{Name: countedCheck, ID: countedDispatchCheck}) {
		t.Errorf("after the pass the settlement names excluded check runs %+v, want the dispatched run's ci-ok %d, which the earlier settlement named",
			summary.ExcludedCheckRuns, uint64(countedDispatchCheck))
	}
}

// workflowRunWebhook is GitHub's workflow_run webhook for a run of check suite suite, started by
// event, of pull request number of repository (owner/name) at sha.
func workflowRunWebhook(t *testing.T, repository string, number int64, sha string, suite int64, event string) string {
	t.Helper()
	owner, name, _ := strings.Cut(repository, "/")
	body, err := json.Marshal(object{
		"action": "requested",
		"workflow_run": object{
			"id": suite + 1, "name": "CI", "path": ".github/workflows/ci.yaml", "event": event,
			"check_suite_id": suite, "head_sha": sha, "status": "queued",
			"pull_requests": []any{object{"number": number}},
		},
		"sender":     object{"login": "octocat", "type": "User"},
		"repository": object{"name": name, "owner": object{"login": owner}, "full_name": repository, "default_branch": "main"},
	})
	if err != nil {
		t.Fatalf("build the workflow_run webhook: %v", err)
	}
	return string(body)
}

// lineSuites is the suites a line names, as text.
func lineSuites(line map[string]any) []string {
	var suites []string
	if listed, ok := line["suites"].([]any); ok {
		for _, suite := range listed {
			suites = append(suites, fmt.Sprint(suite))
		}
	}
	return suites
}

// capturedActionsSuites are the seven GitHub Actions suites of the captured page of #1827 that
// have completed, in id order; its two other suites are other Apps' and queued.
var capturedActionsSuites = []int64{101684831035, 101684831052, 101684831161, 101684831624, 101684831635, 101684831665, 101693986931}

// A head a query answered names, at WARN and once, the GitHub Actions suites whose workflow run
// event neither GitHub's answer nor the CI record holds after the pass wrote them: the record
// counts them as every suite was counted before it read events. The captured page of #1827 was
// read before the query asked for workflowRun, so GitHub's answer carries no event for any of its
// seven Actions suites. A suite whose event the listener took from the run's own workflow_run
// webhook is named in no line, and a head whose every Actions suite's event the record holds gets
// no line at all. Another App's suite (101684758258) is never named.
func TestAQueriedHeadNamesAtWarnTheSuitesTheRecordHoldsNoEventFor(t *testing.T) {
	all := make([]string, 0, len(capturedActionsSuites))
	for _, suite := range capturedActionsSuites {
		all = append(all, strconv.FormatInt(suite, 10))
	}
	for _, c := range []struct {
		name  string
		known []int64
		want  []string
	}{
		{name: "the record holds no event", want: all},
		{name: "the listener recorded one suite's event", known: capturedActionsSuites[:1], want: all[1:]},
		{name: "the listener recorded every suite's event", known: capturedActionsSuites},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.mapRepository(capturedRepository)
			h.github.setRepository(capturedRepository, capturedRepository, 555)
			h.github.setPullRequests(capturedRepository, fakePull{number: capturedNumber, sha: capturedSHA})
			h.github.answerQueryWith(capturedRepository, captured(t, "graphql-page.json"))
			for i, suite := range c.known {
				h.post("workflow_run", workflowRunWebhook(t, capturedRepository, capturedNumber, capturedSHA, suite, "pull_request"), fmt.Sprintf("run-event-%d", i))
			}

			h.pass()

			if rest := h.lines(eventlessRESTLine); len(rest) != 0 {
				t.Errorf("a head a query answered was named as one read over REST: %v", rest)
			}
			lines := h.lines(eventlessQueriedLine)
			if c.want == nil {
				if len(lines) != 0 {
					t.Fatalf("the pass named suites whose every event the record holds: %v", lines)
				}
			} else {
				if len(lines) != 1 {
					t.Fatalf("the pass logged %d lines naming suites the record holds no event for, want one:\n%s", len(lines), h.logs.String())
				}
				line := lines[0]
				if line["level"] != "WARN" || line["repository"] != capturedRepository || line["pr"] != capturedNumberText || line["sha"] != capturedSHA {
					t.Errorf("the line names %v %v#%v at %v, want WARN for %s#%s at %s", line["level"], line["repository"], line["pr"], line["sha"],
						capturedRepository, capturedNumberText, capturedSHA)
				}
				if got := lineSuites(line); !slices.Equal(got, c.want) {
					t.Errorf("the line names suites %v, want %v", got, c.want)
				}
			}
			state := h.stateAt(capturedRepository, capturedNumber, capturedSHA)
			if state == nil {
				t.Fatal("the head has no record after the pass")
			}
			if check, held := state.Checks[capturedCheckName]; !held || uint64(check.CheckRunID) != uint64(capturedRunID) {
				t.Errorf("the record holds %s as %+v (held %v), want run %d, counted", capturedCheckName, check, held, uint64(capturedRunID))
			}
		})
	}
}

// A head read over REST, whose listings carry no workflow run event, names at INFO and once, and
// never at WARN, the GitHub Actions suites the CI record holds no event for after the pass wrote
// them; once the listener has taken the suite's event from its workflow_run webhook, no line names
// it.
func TestAHeadReadOverRESTNamesAtInfoTheSuitesTheRecordHoldsNoEventFor(t *testing.T) {
	const suite = 900
	for _, c := range []struct {
		name  string
		known bool
	}{
		{name: "the record holds no event"},
		{name: "the listener recorded the event", known: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.mapRepository(rowRepository)
			h.listHead()
			h.github.refuseChecks(gitHubRepository, headSHA)
			h.github.setCheckRuns(gitHubRepository, headSHA, checkRun(11, "build", "completed", "success", reviewerApp, "2026-11-21T04:00:00Z"))
			h.github.setCheckSuites(gitHubRepository, headSHA, checkSuite(suite, "completed", "success", reviewerApp, "2026-11-21T04:00:01Z"))
			if c.known {
				h.post("workflow_run", workflowRunWebhook(t, gitHubRepository, 7, headSHA, suite, "pull_request"), "run-event")
			}

			h.pass()

			if got := number(t, h.passLine(), "heads_over_rest"); got != 1 {
				t.Fatalf("the pass read %d heads over REST, want the one whose Checks GitHub refused: %v", got, h.passLine())
			}
			if warned := h.lines(eventlessQueriedLine); len(warned) != 0 {
				t.Errorf("a head read over REST was named at WARN: %v", warned)
			}
			lines := h.lines(eventlessRESTLine)
			if c.known {
				if len(lines) != 0 {
					t.Fatalf("the pass named a suite whose event the record holds: %v", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("the pass logged %d lines naming suites of a head read over REST, want one:\n%s", len(lines), h.logs.String())
			}
			line := lines[0]
			if line["level"] != "INFO" || line["repository"] != gitHubRepository || line["pr"] != pullNumber || line["sha"] != headSHA {
				t.Errorf("the line names %v %v#%v at %v, want INFO for %s#%s at %s", line["level"], line["repository"], line["pr"], line["sha"],
					gitHubRepository, pullNumber, headSHA)
			}
			if got := lineSuites(line); !slices.Equal(got, []string{strconv.Itoa(suite)}) {
				t.Errorf("the line names suites %v, want [%d]", got, suite)
			}
		})
	}
}

// editedAnswer is the captured GraphQL answer in file with edit applied to each check suite of its
// one head, given the event of the suite's workflow run ("" for none), so a test can change what
// GitHub answered without a fixture of its own.
func editedAnswer(t *testing.T, file string, edit func(suite map[string]any, event string)) string {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal([]byte(captured(t, file)), &answer); err != nil {
		t.Fatalf("the captured answer %s is not JSON: %v", file, err)
	}
	repository := answer["data"].(map[string]any)["repository"].(map[string]any)
	pull := repository["pullRequests"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
	commit := pull["commits"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["commit"].(map[string]any)
	for _, raw := range commit["checkSuites"].(map[string]any)["nodes"].([]any) {
		suite := raw.(map[string]any)
		event := ""
		if run, ok := suite["workflowRun"].(map[string]any); ok {
			event, _ = run["event"].(string)
		}
		edit(suite, event)
	}
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode the edited answer: %v", err)
	}
	return string(body)
}

// The head cybermonklabs/legion#2803 holds at aa4cd30f carries a suite of CodeQL's default setup,
// whose workflow run's event is `dynamic` and which holds the required ci-ok, a pr-title and four
// Analyze jobs. GitHub's rollup of the pull request counts those runs, although GitHub's
// documented list of the events whose runs it evaluates for a pull request does not name `dynamic`
// (testdata/graphql-head-2803.json and testdata/graphql-rollup-2803.json, both captured on
// 2026-11-21 with `gh api graphql`). A pass over the captured answer leaves the head's record
// holding GitHub's rollup check run for check run, 39 names of 39 (pr-title has two counted runs,
// and the record keeps the newer). And where the dynamic suite's Analyze (go) fails, the record
// and its settlement name it failing.
func TestAPassCountsADynamicSuitesCheckRunsAsGitHubsRollupDoes(t *testing.T) {
	const (
		number    = 2803
		sha       = "aa4cd30f2cbf1eaa382489b91caca471b75683b2"
		planted   = "Analyze (go)"
		plantedID = 127271542899
	)
	for _, c := range []struct {
		name  string
		plant bool
	}{
		{name: "as captured"},
		{name: "with a planted Analyze (go) failure", plant: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rollup := rollupOf(t, "graphql-rollup-2803.json", "SUCCESS")
			answer := editedAnswer(t, "graphql-head-2803.json", func(suite map[string]any, event string) {
				if !c.plant || event != "dynamic" {
					return
				}
				for _, raw := range suite["checkRuns"].(map[string]any)["nodes"].([]any) {
					if run := raw.(map[string]any); run["name"] == planted {
						run["conclusion"] = "FAILURE"
					}
				}
			})
			if c.plant {
				rollup[planted] = rollupCheck{ID: plantedID, Conclusion: "failure"}
			}
			h := newHarness(t, reviewerAppID)
			h.mapRepository(capturedRepository)
			h.github.setRepository(capturedRepository, capturedRepository, 555)
			h.github.setPullRequests(capturedRepository, fakePull{number: number, sha: sha})
			h.github.answerQueryWith(capturedRepository, answer)
			subject := "notifications.github." + capturedOwner + "." + capturedName + ".pr." + strconv.Itoa(number) + ".checks"
			settlements, err := h.bus.Conn.SubscribeSync(subject)
			if err != nil {
				t.Fatalf("subscribe to %s: %v", subject, err)
			}
			t.Cleanup(func() { _ = settlements.Unsubscribe() })

			h.pass()

			state := settledAt(t, h, number, sha)
			if differ := differFromRollup(&state, rollup); len(differ) > 0 || len(rollup) != 39 {
				t.Errorf("after the pass %d of the record's %d checks differ from GitHub's rollup of %d names (want 39):\n  %s",
					len(differ), len(state.Checks), len(rollup), strings.Join(differ, "\n  "))
			}
			summary := lastSettlement(t, settlements, "after the pass")
			if failing := slices.Contains(summary.Failed.Checks, planted); failing != c.plant {
				t.Errorf("the settlement names %s failing %v, want %v: it fails %v", planted, failing, c.plant, summary.Failed.Checks)
			}
		})
	}
}

// A head the query cannot answer whole because one of its suites holds more check runs than one
// query asks for is read over REST, whose listings carry no event; the events of its suites that
// the query did answer are recorded first, so the store judges the listed runs by them. Here the
// #3513 answer's pull_request suite is said to hold more runs than the query read: the head is
// read over REST, its ci-ok is the pull_request run's failure and not the workflow_dispatch run's
// success of higher id, the dispatched run's suite is not in the record, and no line names a suite
// whose event the record does not hold.
func TestAHeadReadOverRESTIsJudgedByTheEventsItsQueryAnswered(t *testing.T) {
	const pullSuite, dispatchSuite = 121419707580, 121419789924
	answer := editedAnswer(t, "graphql-head-3513.json", func(suite map[string]any, _ string) {
		if suite["databaseId"] == float64(pullSuite) {
			suite["checkRuns"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
		}
	})
	h := newHarness(t, reviewerAppID)
	h.mapRepository(capturedRepository)
	h.github.setRepository(capturedRepository, capturedRepository, 555)
	h.github.setPullRequests(capturedRepository, fakePull{number: countedNumber, sha: countedSHA})
	h.github.answerQueryWith(capturedRepository, answer)
	listed := func(id int64, suite int64, conclusion, at string) object {
		return object{
			"id": id, "name": countedCheck, "status": "completed", "conclusion": conclusion, "head_sha": countedSHA,
			"started_at": at, "completed_at": at, "html_url": fmt.Sprintf("https://github.com/%s/runs/%d", capturedRepository, id),
			"app": object{"id": reviewerApp}, "check_suite": object{"id": suite},
		}
	}
	h.github.setCheckRuns(capturedRepository, countedSHA,
		listed(countedPullCheck, pullSuite, "failure", "2026-11-21T03:58:53Z"),
		listed(countedDispatchCheck, dispatchSuite, "success", "2026-11-21T04:02:35Z"))
	suiteOf := func(id int64, conclusion, at string) object {
		return object{"id": id, "status": "completed", "conclusion": conclusion, "head_sha": countedSHA, "app": object{"id": reviewerApp}, "updated_at": at}
	}
	h.github.setCheckSuites(capturedRepository, countedSHA,
		suiteOf(pullSuite, "failure", "2026-11-21T03:58:54Z"), suiteOf(dispatchSuite, "success", "2026-11-21T04:02:36Z"))

	h.pass()

	if got := number(t, h.passLine(), "heads_over_rest"); got != 1 {
		t.Fatalf("the pass read %d heads over REST, want the one whose suite holds more runs than a query reads: %v", got, h.passLine())
	}
	state := h.stateAt(capturedRepository, countedNumber, countedSHA)
	if state == nil {
		t.Fatal("the head has no record after the pass")
	}
	if check := state.Checks[countedCheck]; uint64(check.CheckRunID) != countedPullCheck || check.Conclusion != "failure" {
		t.Errorf("the head's ci-ok is %+v, want check run %d, the pull_request run's failure", check, uint64(countedPullCheck))
	}
	if suite, held := state.Suites[countedDispatchSuite]; held {
		t.Errorf("the record holds the workflow_dispatch run's suite %+v", suite)
	}
	if lines := append(h.lines(eventlessQueriedLine), h.lines(eventlessRESTLine)...); len(lines) != 0 {
		t.Errorf("the pass named suites whose event the record does not hold, though its query answered each one's event: %v", lines)
	}
}

// A record a listener of an earlier release wrote, which keeps no check's suite, can hold a
// workflow_dispatch run's check run that is still running, and the head then never settles: the
// suite's event reaches no check of no suite. A pass whose answer carries that run, still running,
// with its suite's event writes it all the same, since the store records no run of an event it
// does not count and the write can only take that run out of the record; the pull_request run's
// ci-ok then stands for its name, and the head settles. Nothing of the running dispatched run is
// recorded.
func TestAPassTakesOutARunningUncountedRunAnEarlierReleaseRecorded(t *testing.T) {
	answer := editedAnswer(t, "graphql-head-3513.json", func(suite map[string]any, event string) {
		if event != "workflow_dispatch" {
			return
		}
		suite["status"], suite["conclusion"] = "IN_PROGRESS", nil
		for _, raw := range suite["checkRuns"].(map[string]any)["nodes"].([]any) {
			run := raw.(map[string]any)
			run["status"], run["conclusion"], run["completedAt"] = "IN_PROGRESS", nil, nil
		}
	})
	h := newHarness(t, reviewerAppID)
	h.mapRepository(capturedRepository)
	h.github.setRepository(capturedRepository, capturedRepository, 555)
	h.github.setPullRequests(capturedRepository, fakePull{number: countedNumber, sha: countedSHA})
	h.github.answerQueryWith(capturedRepository, answer)
	for i, run := range countedCiOKRuns(t) {
		if run["id"] == int64(countedDispatchCheck) {
			run["status"], run["conclusion"], run["completed_at"] = "in_progress", nil, nil
		}
		h.post("check_run", countedWebhook(t, run), fmt.Sprintf("earlier-release-%d", i))
	}
	if state := h.stateAt(capturedRepository, countedNumber, countedSHA); state == nil ||
		uint64(state.Checks[countedCheck].CheckRunID) != countedDispatchCheck || state.Checks[countedCheck].Status != "in_progress" {
		t.Fatalf("before the pass the record holds %+v, want the dispatched run's running ci-ok, as a record of an earlier release holds it", state)
	}

	h.pass()

	state := settledAt(t, h, countedNumber, countedSHA)
	if check := state.Checks[countedCheck]; uint64(check.CheckRunID) != countedPullCheck || check.Conclusion != "failure" {
		t.Errorf("after the pass the head's ci-ok is %+v, want check run %d, the pull_request run's failure", check, uint64(countedPullCheck))
	}
	for name, check := range state.Checks {
		if check.Status != "completed" {
			t.Errorf("after the pass the record holds %s running: %+v", name, check)
		}
	}
	if _, held := state.Suites[countedDispatchSuite]; held {
		t.Errorf("the record holds the running workflow_dispatch suite")
	}
}
