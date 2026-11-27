package cistore_test

//ci:reads packages/contracts/fixtures/checks-settlement/* readSettlementFixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// fixtureName is a check name as packages/contracts/fixtures/checks-settlement writes it: a string,
// or {repeat, times, then}, repeat written times over and then then.
type fixtureName string

func (n *fixtureName) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*n = fixtureName(name)
		return nil
	}
	var built struct {
		Repeat string `json:"repeat"`
		Times  int    `json:"times"`
		Then   string `json:"then"`
	}
	if err := json.Unmarshal(data, &built); err != nil {
		return err
	}
	*n = fixtureName(strings.Repeat(built.Repeat, built.Times) + built.Then)
	return nil
}

// fixtureStatus is a status of a settlement as a fixture writes it, and as the listener's payload
// carries it: decoded apart from cistore.StatusRef, so a field the listener leaves out of its
// payload is one this test misses.
type fixtureStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// fixtureRun is a check_runs entry of a settlement as a fixture writes it, and as the listener's
// payload carries it: the check's name, its run's id and GitHub's conclusion word of that run,
// decoded apart from cistore.CheckRunRef for the reason a status is (fixtureStatus).
type fixtureRun struct {
	Name       fixtureName `json:"name"`
	ID         uint64      `json:"id"`
	Conclusion string      `json:"conclusion"`
}

// settlementFixture is a fixture of packages/contracts/fixtures/checks-settlement: GitHub's check
// runs and commit statuses on one head, and the settlement the listener publishes for them. Each
// group is a list of check names. Passed is compared only where the fixture writes it, since
// listener-rules.json leaves it out; an absent failed, cancelled or skipped group is an empty one.
type settlementFixture struct {
	ReviewerAppID string                       `json:"reviewerAppId"`
	CheckRuns     []map[string]json.RawMessage `json:"checkRuns"`
	Statuses      []map[string]any             `json:"statuses"`
	Settlement    struct {
		CheckRuns []fixtureRun    `json:"check_runs"`
		Failed    []fixtureName   `json:"failed"`
		Cancelled []fixtureName   `json:"cancelled"`
		Skipped   []fixtureName   `json:"skipped"`
		Passed    *[]fixtureName  `json:"passed"`
		Statuses  []fixtureStatus `json:"statuses"`
	} `json:"settlement"`
}

// readSettlementFixture reads the named file of packages/contracts/fixtures/checks-settlement.
func readSettlementFixture(t *testing.T, file string) settlementFixture {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", file))
	if err != nil {
		t.Fatalf("read the fixture %s: %v", file, err)
	}
	var fixture settlementFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the fixture %s: %v", file, err)
	}
	return fixture
}

// fixtureHead is the pull request head a fixture's check runs and statuses are delivered on.
type fixtureHead struct {
	owner, repo, number, sha string
}

// The listener settles a head's check runs and commit statuses as the shared fixture
// packages/contracts/fixtures/checks-settlement/listener-rules.json says (settlesAsTheFixtureSays):
// the settlement names each check's highest-id run with GitHub's conclusion word of that run, an
// unknown future word passed on as it is, its name cut as the store keys it, the failed, cancelled
// and skipped checks, a tester or architect run only from the reviewer App, and each terminal
// status with its state and its created_at, apart from the check runs. The Legion daemon's pass
// that built a settlement from this fixture went with its own periodic reads of GitHub
// (dispatch://INFRA-714 R1). A rule changed here and not in the listener fails this test, and a
// reader looking for the daemon's half of these rules will not find one.
func TestTheListenerSettlesCheckRunsAsTheSharedFixtureSays(t *testing.T) {
	fixture := readSettlementFixture(t, "listener-rules.json")
	if len(fixture.Settlement.Statuses) == 0 {
		t.Fatal("the shared fixture's settlement names no status")
	}
	// The reconcile's mapping keeps no pending status, so the fixture's pending one is never
	// recorded.
	settlesAsTheFixtureSays(t, fixture, fixtureHead{owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567890abcdef1234567890abcdef12"},
		[]string{"pending"})
}

// The listener settles a real head of this repository as
// packages/contracts/fixtures/checks-settlement/held-back-suites-skip.json says: the 54 check runs
// GitHub holds on cybermonklabs/legion's head 4198cda5 (pull request #4464), captured with the
// command the fixture's about names, go through the handler, store and summary loop the shared
// fixture's do (settlesAsTheFixtureSays). The head's held-back suites skipped, so the settlement
// names 28 checks skipped, envoy-go-race and dispatch-memory among them, each in the skipped group
// and each with the word skipped, the others passed and none failed or cancelled; pr-title ran
// twice and is named once, by its later run. A skipped check is what GitHub sends for a suite this
// repository holds back, so a rule that read one as anything else would fail here first.
func TestTheListenerSettlesACapturedHeadAsItsFixtureSays(t *testing.T) {
	fixture := readSettlementFixture(t, "held-back-suites-skip.json")
	if len(fixture.Settlement.Skipped) == 0 || len(fixture.Settlement.CheckRuns) >= len(fixture.CheckRuns) {
		t.Fatalf("the captured fixture's settlement names %d skipped checks and %d runs of its %d; want skipped checks and a name that ran twice",
			len(fixture.Settlement.Skipped), len(fixture.Settlement.CheckRuns), len(fixture.CheckRuns))
	}
	settlesAsTheFixtureSays(t, fixture, fixtureHead{owner: "cybermonklabs", repo: "legion", number: "4464", sha: "4198cda5fd0ae76e055089846f40f7be0d10dcf4"}, nil)
}

// settlesAsTheFixtureSays delivers a fixture's check runs and statuses on head and fails unless
// the listener settles them as the fixture says. Each run is delivered as its check_run webhook
// through the real handler, configured with the fixture's reviewer App, into the real CI store;
// each status the reconcile's mapping keeps is recorded by a writer of the same bucket, and the
// states of the statuses it keeps none of are wantUnrecorded. The summary loop's settlement must
// name the fixture's check_runs entries, each with its name, its run's id and GitHub's conclusion
// word of that run, the fixture's failed, cancelled and skipped checks, its passed ones where it
// writes them, and its statuses.
func settlesAsTheFixtureSays(t *testing.T, fixture settlementFixture, head fixtureHead, wantUnrecorded []string) {
	t.Helper()
	names := func(list []fixtureName) []string {
		out := []string{}
		for _, name := range list {
			out = append(out, string(name))
		}
		return out
	}
	wantRuns := fixture.Settlement.CheckRuns
	wantFailed, wantCancelled := names(fixture.Settlement.Failed), names(fixture.Settlement.Cancelled)
	wantSkipped := names(fixture.Settlement.Skipped)
	var wantPassed []string
	if fixture.Settlement.Passed != nil {
		wantPassed = names(*fixture.Settlement.Passed)
	}
	wantStatuses := fixture.Settlement.Statuses
	if slices.ContainsFunc(wantStatuses, func(status fixtureStatus) bool { return status.CreatedAt == "" }) {
		t.Fatalf("the fixture's settlement names a status with no created_at: %+v", wantStatuses)
	}

	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store, err := cistore.Open(t.Context(), bustest.Fixture(client.Conn), logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("cache ready: %v", err)
	}
	// The reconcile's writer, which records the statuses Dispatch reads of GitHub into the same
	// bucket the listener's store settles from.
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("reconcile"))
	if err != nil {
		t.Fatalf("open the reconcile's writer: %v", err)
	}
	const secret = "s"
	handler := webhook.GitHubHandler(secret, "@legion", fixture.ReviewerAppID, "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 300*time.Millisecond, 20*time.Millisecond, metrics.New().NewGauge("held", ""), logging.New("fixture"))
	sub, err := client.Conn.SubscribeSync(contracts.GithubSubject(head.owner, head.repo, "pr."+head.number+".checks"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	postEvent := func(event, body, delivery string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	repository := fmt.Sprintf(`{"name": %q, "owner": {"login": %q}}`, head.repo, head.owner)
	postEvent("pull_request", fmt.Sprintf(`{
		"action": "opened", "number": %s,
		"pull_request": {"head": {"sha": %q}, "updated_at": "2026-10-28T08:00:00Z"},
		"repository": %s
	}`, head.number, head.sha, repository), "d0")
	// Each status goes through the reconcile's mapping into its writer, as Dispatch's CI reconcile
	// records what it reads of GitHub: the listener drops every status webhook. They are recorded
	// before the check runs, so the settlement that names every run already holds them.
	var unrecorded []string
	for _, status := range fixture.Statuses {
		observation, ok := contracts.GithubStatusObservation(status, head.owner, head.repo, head.number, head.sha)
		if !ok {
			unrecorded = append(unrecorded, fmt.Sprint(status["state"]))
			continue
		}
		if changed, err := writer.RecordStatus(ctx, observation); err != nil || !changed {
			t.Fatalf("record the status %s: changed %v, %v", observation.Context, changed, err)
		}
	}
	if !slices.Equal(unrecorded, wantUnrecorded) {
		t.Fatalf("the fixture's statuses the mapping recorded none of are in states %v, want %v", unrecorded, wantUnrecorded)
	}
	for i, run := range fixture.CheckRuns {
		var name fixtureName
		if err := json.Unmarshal(run["name"], &name); err != nil {
			t.Fatalf("decode a run's name: %v", err)
		}
		run["name"], _ = json.Marshal(string(name))
		run["head_sha"], _ = json.Marshal(head.sha)
		run["pull_requests"] = json.RawMessage(`[{"number": ` + head.number + `}]`)
		checkRun, _ := json.Marshal(run)
		postEvent("check_run", `{"action": "completed", "check_run": `+string(checkRun)+`,
			"sender": {"login": "ci", "type": "Bot"},
			"repository": `+repository+`}`, fmt.Sprintf("d%d", i+1))
	}

	var last cistore.Summary
	// settled is the same payload's check runs and statuses, read apart from cistore.CheckRunRef
	// and cistore.StatusRef (fixtureRun, fixtureStatus).
	var settled struct {
		CheckRuns []fixtureRun    `json:"check_runs"`
		Statuses  []fixtureStatus `json:"statuses"`
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		msg, err := sub.NextMsg(time.Until(deadline))
		if err != nil {
			break
		}
		var env contracts.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			t.Fatalf("checks envelope not JSON: %v", err)
		}
		last, settled.CheckRuns, settled.Statuses = cistore.Summary{}, nil, nil
		if err := json.Unmarshal([]byte(env.Payload), &last); err != nil {
			t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
		}
		if err := json.Unmarshal([]byte(env.Payload), &settled); err != nil {
			t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
		}
		if len(settled.CheckRuns) == len(wantRuns) {
			break
		}
	}
	passedDiffers := fixture.Settlement.Passed != nil && !slices.Equal(last.Passed.Checks, wantPassed)
	if !slices.Equal(settled.CheckRuns, wantRuns) || !slices.Equal(last.Failed.Checks, wantFailed) || !slices.Equal(last.Cancelled.Checks, wantCancelled) ||
		!slices.Equal(last.Skipped.Checks, wantSkipped) || passedDiffers || !slices.Equal(settled.Statuses, wantStatuses) {
		t.Fatalf("the listener's settlement\nruns %v\nfailed %v\ncancelled %v\nskipped %v\npassed %v\nstatuses %v\n"+
			"want the fixture's\nruns %v\nfailed %v\ncancelled %v\nskipped %v\npassed %v (compared only where the fixture writes it)\nstatuses %v",
			settled.CheckRuns, last.Failed.Checks, last.Cancelled.Checks, last.Skipped.Checks, last.Passed.Checks, settled.Statuses,
			wantRuns, wantFailed, wantCancelled, wantSkipped, wantPassed, wantStatuses)
	}
}
