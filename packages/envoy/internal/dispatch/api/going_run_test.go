package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingDecisionModel is the measured model the decision tests configure: a name the committed
// record holds, whose thresholds every expectation below reads from the record rather than
// writing a number.
const goingDecisionModel = "jev-latest"

// decisionStandIn is a System One endpoint that answers each decision with the answer the test
// last set, counts the calls and keeps each body it was sent. It calls nothing and holds no
// credential.
type decisionStandIn struct {
	url   string
	calls atomic.Int32

	mu     sync.Mutex
	reply  string
	bodies []map[string]json.RawMessage
}

func newDecisionStandIn(t *testing.T) *decisionStandIn {
	t.Helper()
	standIn := &decisionStandIn{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		standIn.calls.Add(1)
		standIn.mu.Lock()
		standIn.bodies = append(standIn.bodies, body)
		reply := standIn.reply
		standIn.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	standIn.url = server.URL
	return standIn
}

// decidedAnswer is what the stand-in answers the three questions with: the action chosen with
// its confidence and the probability of each choice, what the words are about, and the display
// with its confidence.
type decidedAnswer struct {
	action            string
	confidence        float64
	probabilities     map[string]float64
	about             string
	display           string
	displayConfidence float64
}

// answer sets what the stand-in answers every decision with from now on.
func (s *decisionStandIn) answer(t *testing.T, answer decidedAnswer) {
	t.Helper()
	reply, err := json.Marshal(map[string]any{
		"model": goingDecisionModel,
		"answers": map[string]any{
			going.QuestionAction:  map[string]any{"type": "choice", "choice": answer.action, "confidence": answer.confidence, "probabilities": answer.probabilities},
			going.QuestionContext: map[string]any{"type": "choice", "choice": answer.about, "confidence": 0.9},
			going.QuestionDisplay: map[string]any{"type": "choice", "choice": answer.display, "confidence": answer.displayConfidence},
		},
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.reply = string(reply)
	s.mu.Unlock()
}

// decider is the measured decider at this stand-in, its token answered by token.
func (s *decisionStandIn) decider(t *testing.T, token func(context.Context) (string, error)) *Decider {
	t.Helper()
	decider, measured := NewDecider(agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.SystemOne, BaseURL: s.url, Token: token},
		ID:       goingDecisionModel,
	})
	if !measured {
		t.Fatalf("NewDecider refused %s, which the record holds", goingDecisionModel)
	}
	return decider
}

// offeredActions is every action choice the last decision put to the model, none among them.
func (s *decisionStandIn) offeredActions(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("no decision was asked")
	}
	var questions map[string]agentloop.Question
	if err := json.Unmarshal(s.bodies[len(s.bodies)-1]["questions"], &questions); err != nil {
		t.Fatalf("decode the decision's questions: %v", err)
	}
	var criteria map[string]string
	if err := json.Unmarshal(questions[going.QuestionAction].Criteria, &criteria); err != nil {
		t.Fatalf("decode the action question's criteria: %v", err)
	}
	return slices.Sorted(maps.Keys(criteria))
}

// measuredThresholds is the record's two thresholds for the decision tests' model.
func measuredThresholds(t *testing.T) (action float64, chart *float64) {
	t.Helper()
	record, held := going.DecisionRecord(goingDecisionModel)
	if !held || record.ActionThreshold == nil {
		t.Fatalf("the record holds no action threshold for %s", goingDecisionModel)
	}
	return *record.ActionThreshold, record.ChartThreshold
}

// storedGoingFrames waits for the run to end and answers every frame it stored, in order.
func storedGoingFrames(t *testing.T, rig goingRoutesRig, id string) []goingEvent {
	t.Helper()
	rig.awaitTerminal(t, id)
	frames, _, err := rig.server.readGoingStream(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("read the run's frames: %v", err)
	}
	return frames
}

// frameNames is each stored frame's name, in order.
func frameNames(frames []goingEvent) []string {
	names := make([]string, len(frames))
	for index, frame := range frames {
		names[index] = frame.Event.Name
	}
	return names
}

// frameData decodes the first stored frame of that name into value, and reports whether the run
// stored one.
func frameData(t *testing.T, frames []goingEvent, name string, value any) bool {
	t.Helper()
	for _, frame := range frames {
		if frame.Event.Name != name {
			continue
		}
		raw, err := json.Marshal(frame.Event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode the %s frame %s: %v", name, raw, err)
		}
		return true
	}
	return false
}

// askRun asks one question as login and answers the run's id.
func askRun(t *testing.T, rig goingRoutesRig, login string, body map[string]any) string {
	t.Helper()
	asked := rig.ask(t, login, body)
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	return decodeBody[askedRun](t, asked).ID
}

// A run with a measured decision model decides first: the decision is the stream's first frame,
// and the one answer call is told the two actions the decision offered with their argument
// schemas, which it could only have been told once the decision was in hand. Its arguments come
// back as the proposal, the last frame before the ending: each offered action's list of
// invocations as the answer wrote them, one per item, and nothing for an action the decision
// did not offer, which is never attempted. The run's record keeps the decision and the proposal
// the stream carried, and reads finished, with no write.
func TestGoingRunDecidesBeforeItAnswersAndProposesBothOfferedActions(t *testing.T) {
	threshold, _ := measuredThresholds(t)
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{
		action: "dispatch_issue_update", confidence: min(threshold+0.2, 1),
		probabilities: map[string]float64{"dispatch_issue_update": 0.7, "dispatch_comment": 0.2, going.ActionNone: 0.1},
		about:         going.ContextNamed, display: going.DisplayProse, displayConfidence: 0.9,
	})
	arguments := `{"dispatch_issue_update":[{"issue":"DEMO-1","status":"done"},{"issue":"DEMO-2","status":"done"}],` +
		`"dispatch_comment":[{"issue":"DEMO-1","body":"Closing both."}],` +
		`"dispatch_not_an_action":[{"issue":"DEMO-1"}]}`
	answerer, chat := newGoingModelAnswerer(t, goingtest.Reply{Final: &goingtest.Final{Answer: "Both close.", Arguments: arguments}})
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		answerer: answerer, answerModel: "going-test-deployment", decider: standIn.decider(t, staticToken),
	})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}

	id := askRun(t, rig, "alice", map[string]any{"words": "close DEMO-1 and DEMO-2"})
	frames := storedGoingFrames(t, rig, id)
	names := frameNames(frames)
	if names[0] != contracts.GoingEventDecision || frames[0].Sequence != 1 {
		t.Fatalf("the run stored %v, want the decision first", names)
	}
	if len(names) < 3 || names[len(names)-2] != contracts.GoingEventProposal || names[len(names)-1] != contracts.GoingEventEnd {
		t.Fatalf("the run stored %v, want the proposal just before the end", names)
	}
	if calls := standIn.calls.Load(); calls != 1 {
		t.Fatalf("the decision model was called %d times, want once", calls)
	}

	var decision goingDecision
	frameData(t, frames, contracts.GoingEventDecision, &decision)
	if len(decision.Actions) != 2 || decision.Actions[0].Action != "dispatch_issue_update" || decision.Actions[1].Action != "dispatch_comment" {
		t.Fatalf("the decision ranks %+v, want dispatch_issue_update then dispatch_comment", decision.Actions)
	}
	if decision.Actions[0].Confidence == nil || decision.Actions[1].Confidence != nil || !decision.Act {
		t.Errorf("the decision is %+v, want the first action's own confidence, none for the runner-up, and act", decision)
	}

	requests := chat.Requests()
	if len(requests) != 1 {
		t.Fatalf("the answer made %d calls, want one", len(requests))
	}
	var told string
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			told = message.Content
		}
	}
	for _, want := range []string{"name: dispatch_issue_update", "name: dispatch_comment", `"name":"status"`, `"name":"body"`} {
		if !strings.Contains(told, want) {
			t.Errorf("the answer call was told %q, want %q in it: the offered actions with their schemas", told, want)
		}
	}
	if strings.Contains(told, "dispatch_not_an_action") || strings.Contains(told, "name: dispatch_claim") {
		t.Errorf("the answer call was told %q, want only the two offered actions", told)
	}

	var proposal goingProposal
	frameData(t, frames, contracts.GoingEventProposal, &proposal)
	// The words name DEMO-1 and DEMO-2, so every invocation on them may run at once.
	want := goingProposal{Offers: []goingOffer{
		{Action: "dispatch_issue_update", Offered: true, Invocations: []goingInvocation{
			{Arguments: map[string]any{"issue": "DEMO-1", "status": "done"}, Automatic: true},
			{Arguments: map[string]any{"issue": "DEMO-2", "status": "done"}, Automatic: true},
		}},
		{Action: "dispatch_comment", Offered: true, Invocations: []goingInvocation{
			{Arguments: map[string]any{"issue": "DEMO-1", "body": "Closing both."}, Automatic: true},
		}},
	}}
	if !reflect.DeepEqual(proposal, want) {
		t.Fatalf("the proposal is %+v, want %+v", proposal, want)
	}

	run, err := rig.server.readGoingRun(context.Background(), id, model.CanonicalLogin("alice"))
	if err != nil {
		t.Fatalf("read the run's record: %v", err)
	}
	if !run.Finished || len(run.Writes) != 0 || run.StructuredOutput == nil || *run.StructuredOutput != going.StructuredOutputAccepted {
		t.Errorf("the record reads finished=%v writes=%v structured_output=%v, want finished, no write, accepted", run.Finished, run.Writes, run.StructuredOutput)
	}
	var recordedDecision goingDecision
	var recordedProposal goingProposal
	if json.Unmarshal(run.Decision, &recordedDecision) != nil || !reflect.DeepEqual(recordedDecision, decision) {
		t.Errorf("the record keeps the decision %s, want the frame's %+v", run.Decision, decision)
	}
	if json.Unmarshal(run.Proposal, &recordedProposal) != nil || !reflect.DeepEqual(recordedProposal, proposal) {
		t.Errorf("the record keeps the proposal %s, want the frame's %+v", run.Proposal, proposal)
	}
}

// The actions a run offers are the ones the caller's credential may run, read from the route
// table's own auth class: a person at a browser is never offered a handoff, which
// only a bearer may write, while the same person's own token is.
func TestGoingDecisionOffersOnlyTheActionsTheCallersCredentialMayRun(t *testing.T) {
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayProse, displayConfidence: 1})
	answerer := &fakeAnswerer{frames: []going.Event{going.End(0, 0)}}
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, decider: standIn.decider(t, staticToken)})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}

	rig.awaitTerminal(t, askRun(t, rig, "alice", map[string]any{"words": "what is open?"}))
	browser := standIn.offeredActions(t)
	if slices.Contains(browser, "dispatch_handoff_write") || !slices.Contains(browser, "dispatch_claim") || !slices.Contains(browser, going.ActionNone) {
		t.Fatalf("a browser's run was offered %v, want the claim and none and no handoff", browser)
	}

	token := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	asked := rig.askUnder(t, token, map[string]any{"words": "what is open?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask under alice's token: status=%d body=%s", asked.Code, asked.Body.String())
	}
	rig.awaitTerminal(t, decodeBody[askedRun](t, asked).ID)
	if bearer := standIn.offeredActions(t); !slices.Contains(bearer, "dispatch_handoff_write") {
		t.Fatalf("a bearer's run was offered %v, want the handoff among them", bearer)
	}
}

// act is the bar running the first action at once on submit: true at or above the measured action
// threshold, false below it, false for none, and false for the two writes that always ask however
// sure the decision is.
func TestGoingDecisionActsOnlyAtOrAboveTheThresholdAndNeverForTheTwoThatAlwaysAsk(t *testing.T) {
	threshold, _ := measuredThresholds(t)
	standIn := newDecisionStandIn(t)
	server := directServer(Deps{Decider: standIn.decider(t, staticToken)})
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	start := goingRunStart{actions: catalogue.runnableBy(goingCredential{cookie: true, credential: personCredential})}
	for _, tc := range []struct {
		action     string
		confidence float64
		act        bool
	}{
		{"dispatch_claim", threshold, true},
		{"dispatch_claim", 1, true},
		{"dispatch_claim", threshold - 0.01, false},
		{going.ActionNone, 1, false},
		{"dispatch_issue", 1, false},
		{"dispatch_send_to_agent", 1, false},
	} {
		standIn.answer(t, decidedAnswer{action: tc.action, confidence: tc.confidence, about: going.ContextPage, display: going.DisplayProse, displayConfidence: 1})
		decision, decided := server.goingDecide(t.Context(), "the words", start)
		if !decided {
			t.Fatalf("%s at %v: no decision", tc.action, tc.confidence)
		}
		if decision.Act != tc.act {
			t.Errorf("%s at %v (threshold %v): act=%v, want %v", tc.action, tc.confidence, threshold, decision.Act, tc.act)
		}
	}
}

// A chart is drawn unasked only at or above the measured chart-display threshold, and never while
// the display-fit test hook is on, which is a deployment whose model has no display fit; turned
// off, the record's threshold holds again. A body that names no off is refused and changes
// nothing.
func TestGoingDecisionDrawsNoChartUnaskedWithTheDisplayFitHookOn(t *testing.T) {
	_, chartThreshold := measuredThresholds(t)
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayChart, displayConfidence: 1})
	answerer := &fakeAnswerer{frames: []going.Event{going.Text("Counts per week."), going.End(3, 0)}}
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		answerer: answerer, decider: standIn.decider(t, staticToken), testHooks: true,
	})
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
	chartOf := func() bool {
		t.Helper()
		var decision goingDecision
		if !frameData(t, storedGoingFrames(t, rig, askRun(t, rig, "alice", map[string]any{"words": "issues closed per week"})), contracts.GoingEventDecision, &decision) {
			t.Fatal("the run stored no decision")
		}
		return decision.Display.Chart
	}
	setFit := func(off bool) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/going/display-fit/_test", map[string]any{"off": off}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("display-fit hook: status=%d body=%s", response.Code, response.Body.String())
		}
	}

	setFit(true)
	if chartOf() {
		t.Fatal("with the display-fit hook on, a chart answered at full confidence draws unasked")
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/going/display-fit/_test", map[string]any{}, "alice"); response.Code != http.StatusBadRequest {
		t.Fatalf("display-fit hook with no off: status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if chartOf() {
		t.Fatal("a display-fit body with no off turned the hook off")
	}
	setFit(false)
	if got, want := chartOf(), chartThreshold != nil; got != want {
		t.Fatalf("with the hook off, chart=%v, want %v (the record's chart threshold is %v)", got, want, chartThreshold)
	}
	if chartThreshold != nil && *chartThreshold > 0 {
		standIn.answer(t, decidedAnswer{action: going.ActionNone, confidence: 1, about: going.ContextNone, display: going.DisplayChart, displayConfidence: *chartThreshold - 0.01})
		if chartOf() {
			t.Fatalf("a chart answered below the threshold %v draws unasked", *chartThreshold)
		}
	}
}

// The proposal's own rules, which need no model: on context page a target the words left out is
// filled from the chip; elsewhere it is the one argument the bar asks for, as an item the search
// finds; an action needing more than one argument, or one the bar cannot prompt for, is not
// offered; an element naming an argument the action does not take or holding the wrong type is
// dropped; with no structured reply an action is still offered on what the page fills.
func TestGoingRunProposalFillsFromThePageAndPromptsForOneMissingArgument(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	browser := catalogue.runnableBy(goingCredential{cookie: true, credential: personCredential})
	session := catalogue.runnableBy(goingCredential{credential: &model.Credential{Session: "0199e2a0-1111-7000-8000-000000000001"}})
	chip := &goingContext{Path: "/issues/DEMO-12", Project: "DEMO", Issue: "DEMO-12"}
	offer := func(proposal goingProposal, action string) goingOffer {
		t.Helper()
		for _, offer := range proposal.Offers {
			if offer.Action == action {
				return offer
			}
		}
		t.Fatalf("the proposal %+v holds no offer of %s", proposal, action)
		return goingOffer{}
	}

	claimOnPage := goingPropose(
		goingDecision{Context: going.ContextPage, offered: []string{"dispatch_claim", "dispatch_resolve_ask"}},
		&going.Final{Arguments: `{"dispatch_claim":[{}]}`},
		goingRunStart{chip: chip, actions: browser})
	if got := offer(claimOnPage, "dispatch_claim"); !got.Offered || len(got.Invocations) != 1 ||
		!reflect.DeepEqual(got.Invocations[0], goingInvocation{Arguments: map[string]any{"issue": "DEMO-12"}, Automatic: true}) {
		t.Errorf("on the page the claim is offered as %+v, want the chip's issue filled and nothing missing", got)
	}
	if got := offer(claimOnPage, "dispatch_resolve_ask"); got.Offered || len(got.Invocations) != 0 {
		t.Errorf("an ask resolved with three arguments missing is offered as %+v, want it not offered", got)
	}

	claimNamed := goingPropose(
		goingDecision{Context: going.ContextNamed, offered: []string{"dispatch_claim"}},
		&going.Final{Arguments: `{}`},
		goingRunStart{chip: chip, actions: browser})
	want := goingInvocation{Arguments: map[string]any{}, Missing: &goingMissingArgument{Field: "issue", Kind: goingPromptItem, Item: "issue"}, Automatic: true}
	if got := offer(claimNamed, "dispatch_claim"); !got.Offered || len(got.Invocations) != 1 || !reflect.DeepEqual(got.Invocations[0], want) {
		t.Errorf("about items the words name, the claim is offered as %+v, want the issue asked for as an item", got)
	}

	follow := goingPropose(
		goingDecision{Context: going.ContextNamed, offered: []string{"dispatch_follow"}},
		&going.Final{Arguments: `{"dispatch_follow":[{"ask":"0199e2a0-2222-7000-8000-000000000002"}]}`},
		goingRunStart{actions: session, session: "0199e2a0-1111-7000-8000-000000000001"})
	wantFollow := goingInvocation{
		Arguments: map[string]any{"ask": "0199e2a0-2222-7000-8000-000000000002"},
		Missing:   &goingMissingArgument{Field: "action", Kind: goingPromptEnum, Choices: []string{"follow", "unfollow"}},
	}
	if got := offer(follow, "dispatch_follow"); !got.Offered || !reflect.DeepEqual(got.Invocations, []goingInvocation{wantFollow}) {
		t.Errorf("a session's follow is offered as %+v, want its action asked for among follow and unfollow", got)
	}
	browserFollow := goingPropose(
		goingDecision{Context: going.ContextNamed, offered: []string{"dispatch_follow"}},
		&going.Final{Arguments: `{"dispatch_follow":[{"ask":"0199e2a0-2222-7000-8000-000000000002"}]}`},
		goingRunStart{actions: browser})
	if got := offer(browserFollow, "dispatch_follow"); got.Offered {
		t.Errorf("a browser's follow missing its action and its session is offered as %+v, want it not offered", got)
	}

	dropped := goingPropose(
		goingDecision{Context: going.ContextNamed, offered: []string{"dispatch_issue_update"}},
		&going.Final{Arguments: `{"dispatch_issue_update":[{"issue":12},{"issue":"DEMO-3","colour":"red"},{"issue":"DEMO-4","status":"done"}]}`},
		goingRunStart{actions: browser})
	if got := offer(dropped, "dispatch_issue_update"); !reflect.DeepEqual(got.Invocations, []goingInvocation{{Arguments: map[string]any{"issue": "DEMO-4", "status": "done"}}}) {
		t.Errorf("the update is offered as %+v, want the one element its schema accepts", got)
	}

	fallback := goingPropose(
		goingDecision{Context: going.ContextPage, offered: []string{"dispatch_claim"}},
		nil,
		goingRunStart{chip: chip, actions: browser})
	if got := offer(fallback, "dispatch_claim"); !got.Offered || !reflect.DeepEqual(got.Invocations, []goingInvocation{{Arguments: map[string]any{"issue": "DEMO-12"}, Automatic: true}}) {
		t.Errorf("with no structured reply the claim is offered as %+v, want it filled from the page", got)
	}
}

// With no decision model configured, and with one whose token cannot be read, the answer streams
// as it always did: no decision frame, no proposal, and no call to the decision endpoint.
func TestGoingRunWithNoDecisionModelOrAnUnreachableOneAnswersWithNoDecision(t *testing.T) {
	standIn := newDecisionStandIn(t)
	standIn.answer(t, decidedAnswer{action: "dispatch_claim", confidence: 1, about: going.ContextPage, display: going.DisplayProse, displayConfidence: 1})
	unreadable := func(context.Context) (string, error) { return "", errors.New("the token file is absent") }
	for name, decider := range map[string]*Decider{
		"no decision model": nil,
		"unreachable":       standIn.decider(t, unreadable),
	} {
		t.Run(name, func(t *testing.T) {
			answerer := &fakeAnswerer{frames: []going.Event{going.Text("The answer."), going.End(2, 0)}}
			handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, decider: decider})
			rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
			frames := storedGoingFrames(t, rig, askRun(t, rig, "alice", map[string]any{
				"words": "claim this", "context": map[string]any{"path": "/issues/DEMO-12", "issue": "DEMO-12", "project": "DEMO"},
			}))
			if got, want := frameNames(frames), []string{contracts.GoingEventText, contracts.GoingEventEnd}; !slices.Equal(got, want) {
				t.Fatalf("the run stored %v, want %v: the answer alone", got, want)
			}
		})
	}
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("the decision endpoint was called %d times, want none", calls)
	}
}

// A cancelled run stops at its next frame, wherever its answer is being written: the model, which
// streams slowly, sees its reply abandoned; nothing the answer writes after the cancel is stored;
// a frame appended after it is refused; and the run's record keeps what it reached and reads
// unfinished. "this process" cancels through the route, which ends the run here at once; "another
// process" marks the record alone, as a cancel landing on another pod does, and the run's next
// frame write is what stops it.
func TestGoingCancelStopsARunAtItsNextFrame(t *testing.T) {
	for _, where := range []string{"this process", "another process"} {
		t.Run(where, func(t *testing.T) {
			answer := strings.Repeat("The pelicans arrive in spring and leave in autumn. ", 12)
			answerer, chat := newGoingModelAnswerer(t, goingtest.Reply{
				Final: &goingtest.Final{Answer: answer, Arguments: "{}"}, Chunk: 4, Pause: 100 * time.Millisecond,
			})
			rig := newGoingRoutesRig(t, answerer)
			id := askRun(t, rig, "alice", map[string]any{"words": "when do the pelicans arrive?"})

			awaitGoing(t, "the answer's first text frame", func() bool {
				frames, _, err := rig.server.readGoingStream(context.Background(), id, 0)
				return err == nil && slices.Contains(frameNames(frames), contracts.GoingEventText)
			})
			switch where {
			case "this process":
				cancelled := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests/"+id+"/cancel", nil, "alice")
				if cancelled.Code != http.StatusOK {
					t.Fatalf("cancel: status=%d body=%s", cancelled.Code, cancelled.Body.String())
				}
				if body := decodeBody[goingCancellation](t, cancelled); body.ID != id || body.CancelledAt.IsZero() {
					t.Fatalf("cancel answered %+v, want the run and its moment", body)
				}
			default:
				if _, err := rig.server.cancelGoingRun(context.Background(), id); err != nil {
					t.Fatalf("mark the run cancelled: %v", err)
				}
			}
			atCancel, _, err := rig.server.readGoingStream(context.Background(), id, 0)
			if err != nil {
				t.Fatal(err)
			}

			awaitGoing(t, "the model to see its reply abandoned", func() bool { return chat.Abandoned() == 1 })
			// Past the model's own next frame, so a frame the answer still had in hand has had
			// every chance to be stored.
			time.Sleep(300 * time.Millisecond)
			after, _, err := rig.server.readGoingStream(context.Background(), id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(atCancel) {
				t.Fatalf("the run stored %v by the cancel and %v after it, want nothing more", frameNames(atCancel), frameNames(after))
			}
			if chat.Calls() != 1 {
				t.Errorf("the model was called %d times, want the one call it abandoned", chat.Calls())
			}
			if _, err := rig.server.appendGoingEvent(context.Background(), id, going.Text("late")); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("a frame appended after the cancel: %v, want it refused (pgx.ErrNoRows)", err)
			}
			run, err := rig.server.readGoingRun(context.Background(), id, model.CanonicalLogin("alice"))
			if err != nil {
				t.Fatalf("read the run's record: %v", err)
			}
			if run.Finished {
				t.Errorf("the cancelled run's record reads finished, want unfinished")
			}
		})
	}
}

// Only a run's author may cancel it, and a stranger is answered as the stream answers one: 404,
// the run left running. An id that is no run, and one that is not a uuid, are answered alike.
func TestGoingCancelOfAnotherCallersRunIsNotFound(t *testing.T) {
	rig := newGoingRoutesRig(t, &fakeAnswerer{frames: []going.Event{going.End(0, 0)}})
	id := askRun(t, rig, "alice", map[string]any{"words": "whose run is this?"})
	rig.awaitTerminal(t, id)
	for name, target := range map[string]string{
		"another caller's run": id,
		"no run":               "0199e2a0-3333-7000-8000-000000000003",
		"no uuid":              "not-a-run",
	} {
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests/"+target+"/cancel", nil, "bob")
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s, want 404", name, response.Code, response.Body.String())
		}
		if refusal := decodeBody[refusalAnswer](t, response); refusal.Code != codeGoingRequestNotFound {
			t.Fatalf("%s: code=%q, want %s", name, refusal.Code, codeGoingRequestNotFound)
		}
	}
	var cancelled *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select cancelled_at from going_requests where id = $1::uuid`, id).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != nil {
		t.Fatalf("a stranger's cancel marked alice's run cancelled at %v", *cancelled)
	}
}

// awaitGoing polls until done holds, failing the test after ten seconds.
func awaitGoing(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
