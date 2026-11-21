package reviewer

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// standInTokens is what the stand-in reports a text costs in tokens (modeltest's own count).
func standInTokens(text string) int { return max(utf8.RuneCountInString(text)/4, 1) }

// A reply that is not the object its schema names is asked again, and the call holds every try:
// their number, their tokens and their cost summed, and the last reply's model.
func TestAMalformedReplyIsAskedAgainAndEachTryIsRecorded(t *testing.T) {
	const notJSON = "I will review the text first, then write the object."
	good := reviewReplyOf(t, RuleNoslop)
	stand := modeltest.New(t,
		modeltest.Rule{Contains: reviewMarker, Once: true, Reply: modeltest.Reply{Text: notJSON}},
		modeltest.Rule{Contains: reviewMarker, Once: true, Reply: modeltest.Reply{Text: good}},
	)
	m := chatModel(stand.URL, "reviewer")
	m.Model.Price = agentloop.Price{Input: 2, Output: 10}
	reviewed, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord())
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Findings) != 1 || reviewed.Rewrite == nil {
		t.Errorf("the second try's review = %+v, want its finding and its rewrite", reviewed)
	}
	requests := stand.Requests()
	if len(requests) != 2 {
		t.Fatalf("%d requests, want the malformed try and one more", len(requests))
	}
	input := standInTokens(requests[0].Text) + standInTokens(requests[1].Text)
	output := standInTokens(notJSON) + standInTokens(good)
	want := Call{Purpose: PurposeReview, Deployment: "reviewer", Attempts: 2, Served: "reviewer",
		Usage: agentloop.Usage{Input: input, Output: output, Metered: true}}
	if len(reviewed.Calls) != 1 {
		t.Fatalf("calls = %+v, want the one review call", reviewed.Calls)
	}
	got := reviewed.Calls[0]
	cost := got.Usage.CostUSD
	got.Usage.CostUSD = 0
	if got != want {
		t.Errorf("call = %+v, want %+v", got, want)
	}
	if wantCost := float64(2*input+10*output) / 1e6; math.Abs(cost-wantCost) > 1e-12 {
		t.Errorf("the call cost %v, want both tries' %v", cost, wantCost)
	}
}

// A call makes three tries at most: a reply malformed on each ends the call on that fault, with
// the three tries recorded.
func TestAMalformedReplyIsAskedAgainTwiceAtMost(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Text: "No object here."}})
	reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleNoslop}, askRecord())
	var failure *ModelError
	if !errors.As(err, &failure) || failure.Fault != agentloop.FaultMalformed {
		t.Fatalf("err = %v, want a malformed reply", err)
	}
	if got := len(stand.Requests()); got != 3 {
		t.Errorf("%d requests, want 3: the try and two more", got)
	}
	if len(reviewed.Calls) != 1 || reviewed.Calls[0].Attempts != 3 || reviewed.Calls[0].Fault != agentloop.FaultMalformed {
		t.Errorf("calls = %+v, want one call of 3 tries that ended malformed", reviewed.Calls)
	}

	// A reply that fits the schema and that the caller still cannot use is malformed too.
	stand = modeltest.New(t,
		modeltest.Rule{Contains: defineMarker, Once: true, Reply: modeltest.Reply{Text: `{"definitions":[]}`}},
		modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"store","meaning":"The database.","links":[]}]}`}},
	)
	defined, err := Define(t.Context(), chatModel(stand.URL, "definer"), []RuleSet{RuleNoslop}, askRecord(), []string{"store"}, Answer{})
	if err != nil || len(defined.Definitions) != 1 {
		t.Fatalf("Define = %+v, %v, want the second try's definition", defined, err)
	}
	if len(defined.Calls) != 1 || defined.Calls[0].Attempts != 2 || defined.Calls[0].Fault != "" {
		t.Errorf("calls = %+v, want one call of 2 tries that answered", defined.Calls)
	}
}

// Only a malformed reply is asked again: a provider's error, a refusal, a reply cut at its length
// or a call past its bound ends the call on the first try.
func TestOnlyAMalformedReplyIsAskedAgain(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply modeltest.Reply
		fault agentloop.Fault
	}{
		{"a request the provider refuses", modeltest.Reply{Status: http.StatusBadRequest, Text: "the request is refused"}, agentloop.FaultProviderError},
		{"a reply past the time bound", modeltest.Reply{Text: noFindings, Delay: 5 * time.Second}, agentloop.FaultTime},
	} {
		t.Run(test.name, func(t *testing.T) {
			stand := modeltest.New(t, modeltest.Rule{Reply: test.reply})
			m := chatModel(stand.URL, "rewriter")
			m.Timeout = 500 * time.Millisecond
			rewritten, err := Rewrite(t.Context(), m, []RuleSet{RuleNoslop}, askRecord(), Answer{})
			var failure *ModelError
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want a *ModelError", err)
			}
			if len(rewritten.Calls) != 1 || rewritten.Calls[0].Attempts != 1 {
				t.Errorf("calls = %+v, want one call of one try", rewritten.Calls)
			}
			if got := len(stand.Requests()); got != 1 {
				t.Errorf("%d requests, want 1", got)
			}
		})
	}
}

// Every try runs inside the call's one time bound: a second try gets what time the first left,
// and the call ends at the bound.
func TestATryAgainEndsAtTheCallsBound(t *testing.T) {
	stand := modeltest.New(t,
		modeltest.Rule{Contains: reviewMarker, Once: true, Reply: modeltest.Reply{Text: "No object here.", Delay: 600 * time.Millisecond}},
		modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: noFindings, Delay: 5 * time.Second}},
	)
	m := chatModel(stand.URL, "reviewer")
	m.Timeout = time.Second
	started := time.Now()
	reviewed, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord())
	took := time.Since(started)
	var failure *ModelError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a *ModelError", err)
	}
	if took > m.Timeout+500*time.Millisecond {
		t.Errorf("the call took %s, past its bound of %s", took, m.Timeout)
	}
	if len(reviewed.Calls) != 1 || reviewed.Calls[0].Attempts != 2 {
		t.Errorf("calls = %+v, want one call of 2 tries", reviewed.Calls)
	}
}

// A try again that the pacer cannot serve inside the call's bound is not made: the call ends on
// the malformed reply before it, and only the try made is recorded.
func TestATryAgainThatCannotStartInsideTheBoundIsNotMade(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Text: "No object here."}})
	m := chatModel(stand.URL, "reviewer")
	m.Timeout, m.OutputTokens = 5*time.Second, 1000
	p, err := prepare(t.Context(), m, "review", []RuleSet{RuleNoslop}, askRecord(), extra{})
	if err != nil {
		t.Fatal(err)
	}
	// A quota of one and a half tries a minute serves the first try at once, and the second only
	// about 20 s later, well past the call's bound.
	tokens := modelpace.Charge(utf8.RuneCountInString(p.system)+utf8.RuneCountInString(p.input), m.OutputTokens)
	m.Pace = modelpace.New(modelpace.Config{Quotas: map[string]int{"reviewer": tokens + tokens/2}}).Join(modelpace.Share{
		Deployment: modelpace.Deployment{Kind: "azure", Host: "stand-in", Name: "reviewer"},
	})
	reviewed, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord())
	var failure *ModelError
	if !errors.As(err, &failure) || failure.Fault != agentloop.FaultMalformed {
		t.Fatalf("err = %v, want the first try's malformed reply", err)
	}
	if got := len(stand.Requests()); got != 1 {
		t.Errorf("%d requests, want the one try the pacer served", got)
	}
	if len(reviewed.Calls) != 1 || reviewed.Calls[0].Attempts != 1 {
		t.Errorf("calls = %+v, want one call of one try", reviewed.Calls)
	}
}

// On openai-chat every call sends its schema in strict mode, the review's with the rewrite
// required and null for none; on Anthropic's protocol the schema rides as it always did.
func TestTheSchemaIsStrictOnOpenAIChatAlone(t *testing.T) {
	type format struct {
		Strict *bool `json:"strict"`
		Schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schema"`
	}
	read := func(t *testing.T, request modeltest.Request) (strict *bool, required []string, rewrite string) {
		t.Helper()
		var body struct {
			ResponseFormat struct {
				JSONSchema *format `json:"json_schema"`
			} `json:"response_format"`
			OutputConfig struct {
				Format *format `json:"format"`
			} `json:"output_config"`
		}
		if err := json.Unmarshal(request.Body, &body); err != nil {
			t.Fatal(err)
		}
		f := body.ResponseFormat.JSONSchema
		if f == nil {
			f = body.OutputConfig.Format
		}
		if f == nil {
			t.Fatalf("the request sends no schema: %s", request.Body)
		}
		return f.Strict, f.Schema.Required, string(f.Schema.Properties["rewrite"])
	}
	for _, protocol := range []string{modeltest.Chat, modeltest.Anthropic} {
		t.Run(protocol, func(t *testing.T) {
			stand := modeltest.New(t,
				modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: reviewReplyOf(t, RuleNoslop)}},
				modeltest.Rule{Contains: rewriteMarker, Reply: modeltest.Reply{Text: rewriteReply(t, keptRewrite())}},
				modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"store","meaning":"The database.","links":[]}]}`}},
			)
			m := chatModel(stand.URL, "model")
			if protocol == modeltest.Anthropic {
				m = anthropicModel(stand.URL, "model")
			}
			if _, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord()); err != nil {
				t.Fatal(err)
			}
			if _, err := Rewrite(t.Context(), m, []RuleSet{RuleNoslop}, askRecord(), Answer{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Define(t.Context(), m, []RuleSet{RuleNoslop}, askRecord(), []string{"store"}, Answer{}); err != nil {
				t.Fatal(err)
			}
			requests := stand.Requests()
			if len(requests) != 3 {
				t.Fatalf("%d requests, want the review's, the rewrite's and the definition's", len(requests))
			}
			for i, request := range requests {
				strict, required, rewrite := read(t, request)
				switch {
				case protocol == modeltest.Chat && (strict == nil || !*strict):
					t.Errorf("request %d on openai-chat is not strict: %s", i, request.Body)
				case protocol == modeltest.Anthropic && strict != nil:
					t.Errorf("request %d on Anthropic's protocol sends strict %v", i, *strict)
				}
				if i > 0 {
					continue
				}
				wantRequired, nullable := []string{"findings"}, false
				if protocol == modeltest.Chat {
					wantRequired, nullable = []string{"findings", "rewrite"}, true
				}
				if !slices.Equal(required, wantRequired) {
					t.Errorf("the review's schema requires %v, want %v", required, wantRequired)
				}
				var anyOf struct {
					AnyOf []map[string]any `json:"anyOf"`
				}
				_ = json.Unmarshal([]byte(rewrite), &anyOf)
				if takesNull := len(anyOf.AnyOf) == 2 && anyOf.AnyOf[1]["type"] == "null"; takesNull != nullable {
					t.Errorf("the review's rewrite is %s, want null taken: %v", rewrite, nullable)
				}
			}
		})
	}
}
