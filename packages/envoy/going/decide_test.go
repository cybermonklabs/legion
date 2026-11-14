package going_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
)

// The decision asks three choice questions: every action offered plus none, each carrying its
// description whole as what it is chosen on; the page, items named or none; and chart or prose. An
// action missing from the choices is one the model can never pick.
func TestTheDecisionOffersEveryActionAndNone(t *testing.T) {
	actions := []going.Action{
		{Name: "dispatch_issue", Description: "Create a native Dispatch issue."},
		{Name: "dispatch_claim", Description: "Claim a Dispatch issue, or release it."},
	}
	questions := going.DecisionQuestions(actions)
	if got := slices.Sorted(maps.Keys(questions)); !slices.Equal(got, []string{going.QuestionAction, going.QuestionContext, going.QuestionDisplay}) {
		t.Fatalf("the decision asks %v", got)
	}
	for name, want := range map[string][]string{
		going.QuestionAction:  {"dispatch_claim", "dispatch_issue", going.ActionNone},
		going.QuestionContext: {going.ContextNamed, going.ContextNone, going.ContextPage},
		going.QuestionDisplay: {going.DisplayChart, going.DisplayProse},
	} {
		question := questions[name]
		var criteria map[string]string
		if err := json.Unmarshal(question.Criteria, &criteria); err != nil {
			t.Fatalf("%s: the criteria are no object of strings: %v", name, err)
		}
		if question.Type != "choice" || !slices.Equal(slices.Sorted(maps.Keys(criteria)), want) {
			t.Errorf("%s is a %s question over %v, want a choice over %v", name, question.Type, slices.Sorted(maps.Keys(criteria)), want)
		}
		if name != going.QuestionAction {
			continue
		}
		for _, action := range actions {
			if criteria[action.Name] != action.Description {
				t.Errorf("%s is chosen on %q, want its own description %q", action.Name, criteria[action.Name], action.Description)
			}
		}
	}
}

// The state carries the words as typed and every part of the page they were typed on.
func TestTheDecisionStateCarriesTheWordsAndThePage(t *testing.T) {
	page := going.PageContext{Path: "/issues/DEMO-12/artifacts/spec-md", Project: "DEMO", Issue: "DEMO-12", Block: "b-4f1a"}
	var state struct {
		Words string            `json:"words"`
		Page  going.PageContext `json:"page"`
	}
	if err := json.Unmarshal(going.DecisionState(`close "this" one`, page), &state); err != nil {
		t.Fatal(err)
	}
	if state.Words != `close "this" one` || state.Page != page {
		t.Errorf("the state reads %+v, want the words and the page as given", state)
	}
}

// systemOneAnswer is what the System One stand-in answers the three questions with: a choice and
// its confidence for each, and for the two-way display question the confidence the API reports
// for a two-way choice, the gap between its two probabilities.
const systemOneAnswer = `{
  "model": "systemone-standin",
  "answers": {
    "action": {"type":"choice","choice":"dispatch_claim","confidence":0.82,"probabilities":{"dispatch_claim":0.86,"dispatch_issue":0.04,"none":0.1}},
    "context": {"type":"choice","choice":"page","confidence":0.61,"probabilities":{"page":0.75,"named":0.14,"none":0.11}},
    "display": {"type":"choice","choice":"prose","confidence":0.8,"probabilities":{"prose":0.9,"chart":0.1}}
  },
  "usage": {"input_tokens": 512, "output_tokens": 24}
}`

// TestDecideThroughSystemOne: a real agentloop.Decide call built from DecisionQuestions and
// DecisionState reaches a System One endpoint as the questions and the state the server and the
// bake-off build, under the endpoint's bearer, and the three answers come back with their choices
// and their confidences (c11's decision leg).
func TestDecideThroughSystemOne(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]json.RawMessage
		paths    []string
		bearers  []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		received = append(received, body)
		paths = append(paths, r.URL.Path)
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, systemOneAnswer)
	}))
	t.Cleanup(server.Close)

	actions := []going.Action{
		{Name: "dispatch_issue", Description: "Create a native Dispatch issue."},
		{Name: "dispatch_claim", Description: "Claim a Dispatch issue, or release it."},
	}
	page := going.PageContext{Path: "/issues/DEMO-12", Project: "DEMO", Issue: "DEMO-12"}
	questions := going.DecisionQuestions(actions)
	state := going.DecisionState("claim this", page)
	result, err := agentloop.Decide(t.Context(), agentloop.DecideRequest{
		Model: agentloop.Model{
			Endpoint: agentloop.Endpoint{
				Kind:    agentloop.SystemOne,
				BaseURL: server.URL,
				Token:   func(context.Context) (string, error) { return "decision-bearer", nil },
			},
			ID: "systemone-standin",
		},
		State:     state,
		Questions: questions,
		Bounds:    agentloop.Bounds{Time: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if result.Fault != "" {
		t.Fatalf("Decide ended %s: %s", result.Fault, result.Detail)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || paths[0] != "/v1/systemone" || bearers[0] != "Bearer decision-bearer" {
		t.Fatalf("the endpoint was called %d times, at %v with %v, want one call to /v1/systemone under the bearer", len(received), paths, bearers)
	}
	var sentQuestions, wantQuestions, sentState, wantState any
	asJSON(t, received[0]["questions"], &sentQuestions)
	asJSON(t, questions, &wantQuestions)
	asJSON(t, received[0]["state"], &sentState)
	asJSON(t, state, &wantState)
	if !reflect.DeepEqual(sentQuestions, wantQuestions) {
		t.Errorf("the call put the questions %v, want DecisionQuestions' %v", sentQuestions, wantQuestions)
	}
	if !reflect.DeepEqual(sentState, wantState) {
		t.Errorf("the call put the state %v, want DecisionState's %v", sentState, wantState)
	}

	if got := slices.Sorted(maps.Keys(result.Answers)); !slices.Equal(got, []string{going.QuestionAction, going.QuestionContext, going.QuestionDisplay}) {
		t.Fatalf("the answers are to %v, want the three questions", got)
	}
	for name, want := range map[string]struct {
		choice     string
		confidence float64
	}{
		going.QuestionAction:  {"dispatch_claim", 0.82},
		going.QuestionContext: {going.ContextPage, 0.61},
		going.QuestionDisplay: {going.DisplayProse, 0.8},
	} {
		got := result.Answers[name]
		if got.Type != "choice" || got.Choice != want.choice || got.Confidence != want.confidence {
			t.Errorf("the %s answer is %+v, want %s at confidence %v", name, got, want.choice, want.confidence)
		}
	}
	if !result.Usage.Metered || result.Usage.Input != 512 || result.Usage.Output != 24 {
		t.Errorf("usage = %+v, want the answer's own counts", result.Usage)
	}
}

// asJSON reads v back through JSON, so the test compares what the wire carries rather than a Go
// value.
func asJSON(t *testing.T, v any, into any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}
