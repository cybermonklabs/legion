package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The places of today's fixed phases where a step runs (workflow's stepOf), which a start
// records its agent under.
var (
	implementingPlace = record.StepPlace{Task: "default", Kind: "step", Place: 2}
	testingPlace      = record.StepPlace{Task: "default", Kind: "step", Place: 3}
	acceptingPlace    = record.StepPlace{Task: "default", Kind: "step", Place: 5}
)

// streamWait is one wait for an agent's stream (Options.AwaitStream): the claim, and how many
// facts had applied when it began.
type streamWait struct {
	claim   claim.Token
	applied int
}

// awaitRig is a server whose facts land in a recorder, never in the workflow, and whose waits for
// an agent's stream are kept in the order they were made.
type awaitRig struct {
	h        *harness
	facts    *factRecorder
	statuses *statusRecorder
	mu       sync.Mutex
	waits    []streamWait
}

func newAwaitRig(t *testing.T) *awaitRig {
	t.Helper()
	h := newHarness(t)
	r := &awaitRig{h: h, facts: &factRecorder{}, statuses: &statusRecorder{issues: map[string]dispatch.Issue{"LEGION-208": {Key: "LEGION-208", LastSeq: 7}}}}
	h.handoffs = r.statuses
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{r.facts}, Dispatch: r.statuses, PluginRelaunchable: true, RoleFor: phase.RoleFor,
		Releaser: h.runtime, Trees: h.store, Tokens: handoffTokens{}, GitHubAPI: newHandoffGitHub(t).url,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AwaitStream: func(_ context.Context, token claim.Token) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.waits = append(r.waits, streamWait{claim: token, applied: len(r.facts.recorded())})
		},
	}).Handler
	return r
}

// made is every wait made so far.
func (r *awaitRig) made() []streamWait {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits)
}

// runs records agent's start of the step at place on issue as the daemon's start writes it
// (workflow's startStep: one more start counted, with the agent and the moment), so the record
// names agent the agent of that step's open run.
func (r *awaitRig) runs(t *testing.T, issue string, place record.StepPlace, agent claim.Token) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := r.h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		_, err := record.NewStore().RaiseStepStarts(context.Background(), tx, issue, place, func(starts int64) record.StepRecord {
			return record.StepRecord{Task: place.Task, Step: place.Place, Kind: place.Kind, Agent: string(agent), StartedAt: &at}
		})
		return err
	}); err != nil {
		t.Fatalf("start the step of %s at %+v under %s: %v", issue, place, agent, err)
	}
}

// agentRequest is one request of an agent's: the agent's claim and the request.
type agentRequest struct {
	agent claim.Token
	send  func() *httptest.ResponseRecorder
}

// agentRoute is what one route of the table does with a fact of an agent's own request. waits,
// for a route whose fact names an issue an agent can run a step of, sets up the record naming
// the requesting agent the agent of a run on the fact's issue that is not ended — for a route
// whose fact ends the requester's own step, the request that ends it — and answers the requests
// to send. none says why the route applies no such fact.
type agentRoute struct {
	waits func(t *testing.T, r *awaitRig) []agentRequest
	none  string
}

// Every route of the table is in this test's table, and each request of an agent whose fact names
// an issue where the record names that agent the agent of a run that is not ended waits for the
// agent's stream before its fact applies: the tester's completion, a phase worker's and the
// architect's moves back, the architect's sign-off and its park of a child in accepting, each
// ending its own step, and the architect's retry of an issue its own failure held in accepting
// and its gate registration on a root in accepting. So the record of a step an agent's request
// ends names the model whose answer its stream read ahead of the request. The routes are read
// from the server's own table (routes), so a route added later fails here by name until it is
// put in this one.
func TestEveryAgentRouteWaitsForItsStreamWhereTheRecordNamesItTheAgentOfARun(t *testing.T) {
	noFact := "it applies no fact"
	noRequester := "the operator's bearer redeems no grant, so no requester is noted and a fact it applies waits for nothing (TestAFactNoAgentRequestedWaitsForNothing)"
	controller := "the controller's grant notes no requester (redeem): the controller runs no step"
	table := map[string]agentRoute{
		"GET /legion/v1/state":                         {none: "a read of the daemon's state; " + noFact},
		"GET /healthz":                                 {none: "the liveness answer; " + noFact},
		"GET /readyz":                                  {none: "the readiness answer; " + noFact},
		"POST /legion/v1/claims/register":              {none: "a launch's registration of its session; " + noFact},
		"POST /legion/v1/claims/ready":                 {none: "the claim's report to its own machine; " + noFact},
		"POST /legion/v1/claims/ready-for-update":      {none: "the claim's report to its own machine; " + noFact},
		"POST /legion/v1/claims/exit":                  {none: "the claim's report to its own machine; " + noFact},
		"POST /legion/v1/claims/turn-outcome":          {none: "the claim's report to its own machine; " + noFact},
		"POST /legion/v1/claims/activity":              {none: "the claim's report to its own machine; " + noFact},
		"POST /legion/v1/claims/release-records":       {none: "a read of the claim's release records; " + noFact},
		"POST /legion/v1/grants":                       {none: "a grant's mint; " + noFact},
		"POST /legion/v1/controller/secret":            {none: "the controller's secret; " + noFact},
		"POST /legion/v1/controller/github-credential": {none: "the controller's GitHub credential; " + noFact},
		"POST /legion/v1/issues/status":                {none: controller},
		"POST /legion/v1/claims/fresh-session":         {none: controller},
		"POST /legion/v1/claims/compact":               {none: controller},
		"POST /legion/v1/waves/release": {none: "its own status writes name children not in the workflow yet, which no run is on, so they wait for nothing; " +
			"its adoption of children recorded in other trees names no one issue (intake.IssueOf), so apply asks RunsAStep, the read TestTheRecordNamesTheAgentOfARunThatIsNotEnded holds, and this table drives no adoption"},
		"POST /legion/v1/roots/close":    {none: "its root has started no phase: admitted and awaiting a person run no step (workflow's stepOf)"},
		"POST /legion/v1/children/rerun": {none: "its child is out of the workflow, at phase done, which runs no step"},
		"POST /legion/v1/handoff/complete": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			seedIssueAt(t, r.h, "LEGION-208", phase.Testing)
			tester := newLiveClaim(t, r.h, "LEGION-208", claim.RoleTester)
			r.runs(t, "LEGION-208", testingPlace, tester.token)
			written := tester.writeHandoff("test", 1)
			return []agentRequest{{tester.token, func() *httptest.ResponseRecorder {
				return r.h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1})
			}}}
		}},
		"POST /legion/v1/phase/backward": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			seedIssueAt(t, r.h, "LEGION-208", phase.Testing)
			tester := newLiveClaim(t, r.h, "LEGION-208", claim.RoleTester)
			architect := newLiveClaim(t, r.h, "LEGION-208", claim.RoleArchitect)
			r.runs(t, "LEGION-208", testingPlace, tester.token)
			return []agentRequest{{tester.token, func() *httptest.ResponseRecorder {
				return r.h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
					"grantId": tester.grant(t), "to": "implementing", "reason": "the suite needs a fix"}, nil)
			}}, {architect.token, func() *httptest.ResponseRecorder {
				seedIssueAt(t, r.h, "LEGION-208", phase.Accepting)
				r.runs(t, "LEGION-208", acceptingPlace, architect.token)
				return r.h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
					"grantId": architect.grant(t), "issue": "LEGION-208", "to": "implementing", "reason": "the acceptance found a gap"}, nil)
			}}}
		}},
		"POST /legion/v1/signoff": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			seedIssueAt(t, r.h, "LEGION-208", phase.Accepting)
			architect := newLiveClaim(t, r.h, "LEGION-208", claim.RoleArchitect)
			r.runs(t, "LEGION-208", acceptingPlace, architect.token)
			return []agentRequest{{architect.token, func() *httptest.ResponseRecorder {
				return r.h.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-208"}, nil)
			}}}
		}},
		"POST /legion/v1/children/park": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			seedTree(t, r.h, "LEGION-208", "LEGION-209")
			setIssue(t, r.h, "LEGION-209", func(issue *record.Issue) { issue.Phase = phase.Accepting })
			architect := newLiveClaim(t, r.h, "LEGION-208", claim.RoleArchitect)
			r.runs(t, "LEGION-209", acceptingPlace, architect.token)
			return []agentRequest{{architect.token, func() *httptest.ResponseRecorder {
				return r.h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-209"}, nil)
			}}}
		}},
		"POST /legion/v1/phase/retry": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			// A failure of the architect's claim holds an issue in accepting, whose role is the
			// architect's (workflow's claimFailed), and the held run is the step of the phase its
			// hold is from (record.StandsAt).
			seedTree(t, r.h, "LEGION-208", "LEGION-209")
			setIssue(t, r.h, "LEGION-209", func(issue *record.Issue) {
				issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Accepting}
			})
			architect := newLiveClaim(t, r.h, "LEGION-208", claim.RoleArchitect)
			r.runs(t, "LEGION-209", acceptingPlace, architect.token)
			return []agentRequest{{architect.token, func() *httptest.ResponseRecorder {
				return r.h.request(http.MethodPost, "/legion/v1/phase/retry", map[string]any{
					"grantId": architect.grant(t), "issue": "LEGION-209", "decision": "retry"}, nil)
			}}}
		}},
		"POST /legion/v1/gates/register": {waits: func(t *testing.T, r *awaitRig) []agentRequest {
			const artifact = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093"
			r.statuses.documents = map[string]string{artifact: "LEGION-208"}
			r.statuses.approvals = map[string]dispatch.Approval{artifact: {State: "none", LatestVersion: 7}}
			seedTree(t, r.h, "LEGION-208")
			setIssue(t, r.h, "LEGION-208", func(issue *record.Issue) { issue.Phase = phase.Accepting })
			architect := newLiveClaim(t, r.h, "LEGION-208", claim.RoleArchitect)
			r.runs(t, "LEGION-208", acceptingPlace, architect.token)
			return []agentRequest{{architect.token, func() *httptest.ResponseRecorder {
				return r.h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
					"grantId": architect.grant(t), "issue": "LEGION-208", "artifactId": artifact, "version": 7}, nil)
			}}}
		}},
		"POST /legion/v1/operator/claims":                        {none: noRequester},
		"GET /legion/v1/operator/claims":                         {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/deliver":        {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/suspend":        {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/resume":         {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/stop":           {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/retry":          {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/compact":        {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/forget-session": {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/release-names":  {none: noRequester},
		"POST /legion/v1/operator/claims/{token}/close":          {none: noRequester},
		"/": {none: "the catch-all answers no route; " + noFact},
	}

	served := (&server{}).routes()
	if len(served) == 0 {
		t.Fatal("the server's table holds no route: the test read nothing")
	}
	patterns := make([]string, 0, len(served))
	for _, route := range served {
		patterns = append(patterns, route.pattern)
		if _, listed := table[route.pattern]; !listed {
			t.Errorf("route %s is not in this test's table: give the request of an agent whose fact names an issue the agent runs a step of, or say why it applies no such fact", route.pattern)
		}
	}
	for pattern := range table {
		if !slices.Contains(patterns, pattern) {
			t.Errorf("this test's table holds %s, which the server does not serve", pattern)
		}
	}
	for _, pattern := range patterns {
		entry := table[pattern]
		if entry.waits == nil {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			r := newAwaitRig(t)
			for i, request := range entry.waits(t, r) {
				applied, waited := len(r.facts.recorded()), len(r.made())
				if recorder := request.send(); recorder.Code != http.StatusOK {
					t.Fatalf("request %d of %s = %d: %s", i+1, pattern, recorder.Code, recorder.Body)
				}
				if len(r.facts.recorded()) == applied {
					t.Fatalf("request %d of %s applied no fact", i+1, pattern)
				}
				made := r.made()[waited:]
				if len(made) == 0 || made[0] != (streamWait{claim: request.agent, applied: applied}) {
					t.Fatalf("request %d of %s made the waits %+v, want the first a wait for %s's stream before any fact of the request applied (%d before it)",
						i+1, pattern, made, request.agent, applied)
				}
			}
		})
	}
}

// A request no agent's grant made applies its fact with no wait: the operator's retry of a phase
// worker, whose run the record holds open, goes through apply as every fact does and waits for
// no stream, since the operator is no requester whose stream the record reads. A request an
// agent made waits only where the record names that agent the agent of a run on the issue its
// fact names: the architect's park of a child in implementing, whose run is its implementer's,
// waits for nothing, though the architect runs the accepting step of another child. A request
// refused before it applies a fact waits for nothing: the tester's completion with no verdict,
// and the architect's move back of an issue of another tree.
func TestAFactNoAgentRequestedWaitsForNothing(t *testing.T) {
	r := newAwaitRig(t)
	worker, _ := r.h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	r.h.ready(worker, "ses_implementer", 1)
	workingIssue(t, r.h, phase.Implementing)
	r.runs(t, "LEGION-209", implementingPlace, worker)
	if recorder := r.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(worker)+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if recorder := r.h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(worker)+"/retry", nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("the operator's retry = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	if applied := r.facts.recorded(); len(applied) != 1 {
		t.Fatalf("the operator's retry applied %#v, want its one fact", applied)
	}

	seedTree(t, r.h, "LEGION-300", "LEGION-301", "LEGION-302")
	setIssue(t, r.h, "LEGION-301", func(issue *record.Issue) { issue.Phase = phase.Accepting })
	setIssue(t, r.h, "LEGION-302", func(issue *record.Issue) { issue.Phase = phase.Implementing })
	architect := newLiveClaim(t, r.h, "LEGION-300", claim.RoleArchitect)
	implementer, _ := r.h.launchIn("LEGION-300", "LEGION-302", claim.RoleImplementer)
	r.runs(t, "LEGION-301", acceptingPlace, architect.token)
	r.runs(t, "LEGION-302", implementingPlace, implementer)
	if park := r.h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": "LEGION-302"}, nil); park.Code != http.StatusOK {
		t.Fatalf("park = %d: %s", park.Code, park.Body)
	}

	seedIssueAt(t, r.h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, r.h, "LEGION-208", claim.RoleTester)
	r.runs(t, "LEGION-208", testingPlace, tester.token)
	written := tester.writeHandoff("test", 1)
	refused(t, r.h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "ran the suite", HandoffID: written, HandoffVersion: 1}),
		http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")
	seedIssueAt(t, r.h, "LEGION-400", phase.Implementing)
	other := newLiveClaim(t, r.h, "LEGION-400", claim.RoleArchitect)
	refused(t, r.h.request(http.MethodPost, "/legion/v1/phase/backward", map[string]any{
		"grantId": other.grant(t), "issue": "LEGION-301", "to": "implementing", "reason": "not this architect's tree",
	}, nil), http.StatusForbidden, "ISSUE_OUTSIDE_TREE")

	if made := r.made(); len(made) != 0 {
		t.Fatalf("the waits made = %+v, want none", made)
	}
	if applied := r.facts.recorded(); len(applied) != 2 {
		t.Fatalf("the facts applied = %#v, want the operator's retry and the park", applied)
	}
}
