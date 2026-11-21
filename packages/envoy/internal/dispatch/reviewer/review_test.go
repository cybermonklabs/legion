package reviewer

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// ruleSetMarkers are words of each rule set's own text, which a request carries only where that
// rule set is on: its section of the system prompt, and its inputs in the user message.
var ruleSetMarkers = map[RuleSet][]string{
	RuleNoslop:      {`### Rule set "noslop"`, "**Mechanism.**", "# Pattern catalog", "# Terminology"},
	RulePhrases:     {`### Rule set "phrases"`, "| land the change, land the PR |"},
	RuleReaderWords: {`### Rule set "reader_words"`, "<reader_words>\n- store\n</reader_words>"},
	RuleAskParts:    {`### Rule set "ask_parts"`, "2. Why the decision is necessary now."},
	RuleStructure:   {`### Rule set "structure"`, "<structure_findings>", "Rule phrasal-verb."},
	RuleDetector:    {`### Rule set "detector"`, "<detector_findings>", "Rule vacuous-verb-phrase."},
}

// keptRewrite is a rewrite of askRecord's ask that keeps every rule of what a rewrite keeps, as
// the schema has a reply write it: options with no id.
func keptRewrite() Text {
	return Text{
		Kind: KindAsk,
		Body: "Finding from tester\nWe start the node. The test fails on the node's first call.\nRef: dispatch://PRJ-1/comment/c1",
		Options: []model.AskOption{
			{Label: "Ship", Description: "Release it today, with the failing test"},
			{Label: "Hold", Description: "Wait until the test passes"},
		},
	}
}

// reviewReplyOf is a review's reply that finds one problem of set, with keptRewrite.
func reviewReplyOf(t *testing.T, set RuleSet) string {
	t.Helper()
	rewrite := keptRewrite()
	return asJSON(t, map[string]any{
		"findings": []Finding{{RuleSet: set, Quote: "This names the problem.", Why: "It states no fact.", Fix: "The test fails on the node's first call."}},
		"rewrite":  map[string]any{"body": rewrite.Body, "options": rewrite.Options, "multiple": false},
	})
}

// Each rule set's text reaches the model only where the rule set is on, and the model's findings
// and rewrite come back as the review's.
func TestEachRuleSetIsInTheRequestOnlyWhenItIsOn(t *testing.T) {
	for _, set := range AllRuleSets {
		t.Run(string(set), func(t *testing.T) {
			stand := modeltest.New(t,
				modeltest.Rule{Contains: detectorMarker, Reply: modeltest.Reply{Text: `{"vacuous":[{"id":"c1","rewrite":"Say the problem."}]}`}},
				modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: reviewReplyOf(t, set)}},
			)
			reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{set}, askRecord())
			if err != nil {
				t.Fatalf("Review with %s on: %v", set, err)
			}
			want := []Finding{{RuleSet: set, Quote: "This names the problem.", Why: "It states no fact.", Fix: "The test fails on the node's first call."}}
			if !reflect.DeepEqual(reviewed.Findings, want) {
				t.Errorf("findings = %+v, want %+v", reviewed.Findings, want)
			}
			if rewrite := keptRewrite(); reviewed.Rewrite == nil || !reflect.DeepEqual(*reviewed.Rewrite, rewrite) || reviewed.NotApplied != nil {
				t.Errorf("rewrite = %+v, not applied %v, want %+v applied", reviewed.Rewrite, reviewed.NotApplied, rewrite)
			}
			reviews := requestsHolding(stand, reviewMarker)
			if len(reviews) != 1 {
				t.Fatalf("%d review requests, want 1", len(reviews))
			}
			for other, markers := range ruleSetMarkers {
				for _, marker := range markers {
					if held := strings.Contains(reviews[0].Text, marker); held != (other == set) {
						t.Errorf("with only %s on, the request holds %q (of %s): %v", set, marker, other, held)
					}
				}
			}
			if called := len(requestsHolding(stand, detectorMarker)); called != map[bool]int{true: 1, false: 0}[set == RuleDetector] {
				t.Errorf("with only %s on, the detector's reader was called %d times", set, called)
			}

			// Every other rule set on, and this one off.
			others := slices.DeleteFunc(slices.Clone(AllRuleSets), func(s RuleSet) bool { return s == set })
			stand = modeltest.New(t,
				modeltest.Rule{Contains: detectorMarker, Reply: modeltest.Reply{Text: `{"vacuous":[{"id":"c1","rewrite":"Say the problem."}]}`}},
				modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: reviewReplyOf(t, others[0])}},
			)
			if _, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), others, askRecord()); err != nil {
				t.Fatalf("Review with all but %s on: %v", set, err)
			}
			reviews = requestsHolding(stand, reviewMarker)
			if len(reviews) != 1 {
				t.Fatalf("%d review requests, want 1", len(reviews))
			}
			for other, markers := range ruleSetMarkers {
				for _, marker := range markers {
					if held := strings.Contains(reviews[0].Text, marker); held != (other != set) {
						t.Errorf("with all but %s on, the request holds %q (of %s): %v", set, marker, other, held)
					}
				}
			}
		})
	}
}

// The structure rules and the detector run before the review, and their findings are inputs the
// review reports beside its own.
func TestTheStructureRulesAndTheDetectorAreInputs(t *testing.T) {
	stand := modeltest.New(t,
		modeltest.Rule{Contains: detectorMarker, Reply: modeltest.Reply{Text: `{"vacuous":[{"id":"c1","rewrite":"Say the problem."}]}`}},
		modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: noFindings}},
	)
	reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleStructure, RuleDetector}, askRecord())
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, check := range reviewed.Checks {
		rules = append(rules, check.Rule+": "+check.Text)
	}
	// writingcheck reads a paragraph's soft line break as a space, so the first line joins the
	// sentence after it.
	if want := []string{"phrasal-verb: Finding from tester We spin up the node.", "vacuous-verb-phrase: This names the problem."}; !slices.Equal(rules, want) {
		t.Errorf("checks = %q, want %q", rules, want)
	}
	review := requestsHolding(stand, reviewMarker)[0].Text
	for _, want := range []string{"<structure_findings>\n- Rule phrasal-verb. The sentence: \"Finding from tester We spin up the node.\"", "<detector_findings>\n- Rule vacuous-verb-phrase. The sentence: \"This names the problem.\". The fix: Say the problem."} {
		if !strings.Contains(review, want) {
			t.Errorf("the review's request does not hold %q", want)
		}
	}
	if len(reviewed.Findings) != 0 || reviewed.Rewrite != nil {
		t.Errorf("a review that found nothing = %+v", reviewed)
	}
	if len(reviewed.Calls) != 2 || reviewed.Calls[0].Purpose != PurposeDetector || reviewed.Calls[1].Purpose != PurposeReview {
		t.Errorf("calls = %+v, want the detector's, then the review's", reviewed.Calls)
	}
}

// ask_parts applies to an ask and a decision block alone: a comment's review neither shows it nor
// lets a finding name it, and a review no rule set applies to makes no call.
func TestAskPartsAppliesToAnAskAlone(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: noFindings}})
	comment := Record{Text: Text{Kind: KindComment, Body: "This names the problem."}}
	if _, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleNoslop, RuleAskParts}, comment); err != nil {
		t.Fatal(err)
	}
	request := requestsHolding(stand, reviewMarker)[0]
	if strings.Contains(request.Text, `### Rule set "ask_parts"`) || !strings.Contains(string(request.Body), `"enum":["noslop"]`) {
		t.Errorf("a comment's review shows ask_parts, or lets a finding name it: %s", request.Body)
	}
	reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleAskParts}, comment)
	if err != nil || len(reviewed.Calls) != 0 || len(stand.Requests()) != 1 {
		t.Errorf("a review no rule set applies to = %+v, %v, after %d requests; want no call", reviewed, err, len(stand.Requests()))
	}
}

// A finding of a rule set that is off fits no reply's schema, so the call gave no usable answer.
func TestAFindingOfARuleSetThatIsOffIsMalformed(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: reviewReplyOf(t, RulePhrases)}})
	_, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleNoslop}, askRecord())
	var failure *ModelError
	if !errors.As(err, &failure) || failure.Fault != agentloop.FaultMalformed {
		t.Fatalf("err = %v, want a malformed reply", err)
	}
}

// A finding whose quote is not in the text is dropped and kept with its rule set and its quote as
// the model wrote it: the quote is looked up as the harness's quote check reads it, so case,
// emphasis and white space do not count, and a quote of the record is not the text's. A review
// whose every finding is dropped found nothing, and its rewrite is not kept.
func TestAFindingWhoseQuoteIsNotInTheTextIsDropped(t *testing.T) {
	rewrite := keptRewrite()
	reply := func(findings ...Finding) string {
		return asJSON(t, map[string]any{
			"findings": findings,
			"rewrite":  map[string]any{"body": rewrite.Body, "options": rewrite.Options, "multiple": false},
		})
	}
	ofText := Finding{RuleSet: RuleNoslop, Quote: "we  **spin up**\nthe node.", Why: "A phrasal verb.", Fix: "We start the node."}
	ofRecord := Finding{RuleSet: RulePhrases, Quote: "The store keeps rows.", Why: "It says no more.", Fix: "The store keeps each row."}
	empty := Finding{RuleSet: RulePhrases, Quote: " ", Why: "It says no more.", Fix: "The store."}
	stand := modeltest.New(t,
		modeltest.Rule{Contains: reviewMarker, Once: true, Reply: modeltest.Reply{Text: reply(ofRecord, ofText, empty)}},
		modeltest.Rule{Contains: reviewMarker, Once: true, Reply: modeltest.Reply{Text: reply(ofRecord)}},
	)
	rules := []RuleSet{RuleNoslop, RulePhrases}
	reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), rules, askRecord())
	if err != nil {
		t.Fatal(err)
	}
	dropped := []DroppedFinding{{RuleSet: RulePhrases, Quote: ofRecord.Quote}, {RuleSet: RulePhrases, Quote: empty.Quote}}
	if !reflect.DeepEqual(reviewed.Findings, []Finding{ofText}) || !slices.Equal(reviewed.Dropped, dropped) {
		t.Errorf("findings %+v, dropped %+v; want the text's finding kept and the two others dropped with their quotes", reviewed.Findings, reviewed.Dropped)
	}
	if reviewed.Rewrite == nil {
		t.Error("a review with a finding kept lost its rewrite")
	}

	reviewed, err = Review(t.Context(), chatModel(stand.URL, "reviewer"), rules, askRecord())
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Findings) != 0 || reviewed.Rewrite != nil || !slices.Equal(reviewed.Dropped, dropped[:1]) {
		t.Errorf("a review whose one finding quotes the record = %+v, want no finding, no rewrite and one dropped", reviewed)
	}
}

// An ask's option may be quoted as the model reads it, in JSON, or as an ask block writes it,
// "Label: Description", and both quotes are the ask's. A quote that puts an option's label with
// words that no option holds is dropped, and a quote of the question is kept as before. A
// comment's quote is looked up as before: the JSON form of words the comment holds is not in it.
func TestAQuoteOfAnAsksOptionIsLookedUpInTheFormsTheModelReadsAndWrites(t *testing.T) {
	quoting := func(quotes ...string) []Finding {
		findings := make([]Finding, len(quotes))
		for i, quote := range quotes {
			findings[i] = Finding{RuleSet: RuleNoslop, Quote: quote, Why: "It names no fact.", Fix: "Say the fact."}
		}
		return findings
	}
	review := func(record Record, findings []Finding) Reviewed {
		t.Helper()
		stand := modeltest.New(t, modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"findings": findings, "rewrite": nil})}})
		reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleNoslop}, record)
		if err != nil {
			t.Fatal(err)
		}
		return reviewed
	}
	dropped := func(findings []Finding) []DroppedFinding {
		out := make([]DroppedFinding, len(findings))
		for i, finding := range findings {
			out[i] = DroppedFinding{RuleSet: finding.RuleSet, Quote: finding.Quote}
		}
		return out
	}

	kept := quoting(
		`"Ship","description":"Release it today"`,
		`{"label":"Hold","description":"Wait for the store"}`,
		`"Ship","description":"Release it today"},{"label":"Hold"`,
		"Ship: Release it today",
		"we  **spin up**\nthe node.",
	)
	mixed := quoting(
		`"Ship","description":"Release it next week"`,
		"Ship: Release it next week",
		"Ship: Wait for the store",
	)
	reviewed := review(askRecord(), append(slices.Clone(kept), mixed...))
	if !reflect.DeepEqual(reviewed.Findings, kept) || !slices.Equal(reviewed.Dropped, dropped(mixed)) {
		t.Errorf("an ask's findings %+v, dropped %+v; want the option's and the question's quotes kept and the mixed ones dropped", reviewed.Findings, reviewed.Dropped)
	}

	comment := Record{Text: Text{Kind: KindComment, Body: "We keep two options.\nShip: Release it today"}}
	ownWords, jsonForm := quoting("Ship: Release it today"), quoting(`"Ship","description":"Release it today"`)
	reviewed = review(comment, append(slices.Clone(ownWords), jsonForm...))
	if !reflect.DeepEqual(reviewed.Findings, ownWords) || !slices.Equal(reviewed.Dropped, dropped(jsonForm)) {
		t.Errorf("a comment's findings %+v, dropped %+v; want its own words kept and the JSON form dropped", reviewed.Findings, reviewed.Dropped)
	}
}

// An ask reaches the model with <, > and & as written, in its question and in its options' JSON,
// so a finding that quotes them as the model read them is kept.
func TestAnAsksMarksReachTheModelAsWritten(t *testing.T) {
	const marks = "a < b && c > d"
	record := askRecord()
	record.Text.Body = "Finding from tester\nWe ship once " + marks + "."
	record.Text.Options[0].Description = "Release it once " + marks
	findings := []Finding{
		{RuleSet: RuleNoslop, Quote: "We ship once " + marks + ".", Why: "It names no fact.", Fix: "Say the fact."},
		{RuleSet: RuleNoslop, Quote: `"Ship","description":"Release it once ` + marks + `"`, Why: "It names no fact.", Fix: "Say the fact."},
	}
	stand := modeltest.New(t, modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"findings": findings, "rewrite": nil})}})
	reviewed, err := Review(t.Context(), chatModel(stand.URL, "reviewer"), []RuleSet{RuleNoslop}, record)
	if err != nil {
		t.Fatal(err)
	}
	request := requestsHolding(stand, reviewMarker)[0].Text
	for _, want := range []string{"We ship once " + marks + ".", `{"label":"Ship","description":"Release it once ` + marks + `"}`} {
		if !strings.Contains(request, want) {
			t.Errorf("the review's request does not hold %q:\n%s", want, request)
		}
	}
	if !reflect.DeepEqual(reviewed.Findings, findings) || len(reviewed.Dropped) != 0 {
		t.Errorf("findings %+v, dropped %+v; want both findings kept", reviewed.Findings, reviewed.Dropped)
	}
}

// A model that fails gives a *ModelError the server posts the text as written on, with the call
// it made.
func TestAModelFailureIsAModelError(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply modeltest.Reply
		fault agentloop.Fault
	}{
		{"a provider error", modeltest.Reply{Status: http.StatusInternalServerError, Text: "the deployment is down"}, agentloop.FaultProviderError},
		{"a reply that is no JSON", modeltest.Reply{Text: "I found two problems."}, agentloop.FaultMalformed},
		{"a reply past the time bound", modeltest.Reply{Text: noFindings, Delay: 5 * time.Second}, agentloop.FaultTime},
	} {
		t.Run(test.name, func(t *testing.T) {
			stand := modeltest.New(t, modeltest.Rule{Reply: test.reply})
			m := chatModel(stand.URL, "reviewer")
			m.Timeout = 500 * time.Millisecond
			reviewed, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord())
			var failure *ModelError
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want a *ModelError", err)
			}
			if failure.Fault != test.fault && !(test.fault == agentloop.FaultTime && errors.Is(err, context.DeadlineExceeded)) {
				t.Errorf("err = %v (fault %q), want fault %q", err, failure.Fault, test.fault)
			}
			if len(reviewed.Calls) != 1 || reviewed.Calls[0].Deployment != "reviewer" || reviewed.Calls[0].Purpose != PurposeReview {
				t.Errorf("calls = %+v, want the failed review's", reviewed.Calls)
			}
			if len(reviewed.Findings) != 0 || reviewed.Rewrite != nil {
				t.Errorf("a failed review = %+v, want no findings and no rewrite", reviewed)
			}
		})
	}
}

// Each call names the deployment its request named and the model the provider served, on both
// protocols a model group speaks, the detector's call included.
func TestEachCallNamesItsDeploymentAndTheModelServed(t *testing.T) {
	for _, protocol := range []string{modeltest.Chat, modeltest.Anthropic} {
		t.Run(protocol, func(t *testing.T) {
			stand := modeltest.New(t,
				modeltest.Rule{Contains: detectorMarker, Reply: modeltest.Reply{Text: `{"vacuous":[]}`}},
				modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: reviewReplyOf(t, RuleNoslop)}},
			)
			url := servedAs(t, stand, "reviewer", "reviewer-2026-11-01")
			m := chatModel(url, "reviewer")
			if protocol == modeltest.Anthropic {
				m = anthropicModel(url, "reviewer")
			}
			reviewed, err := Review(t.Context(), m, []RuleSet{RuleNoslop, RuleDetector}, askRecord())
			if err != nil {
				t.Fatal(err)
			}
			want := []Call{
				{Purpose: PurposeDetector, Deployment: "reviewer", Served: "reviewer-2026-11-01"},
				{Purpose: PurposeReview, Deployment: "reviewer", Served: "reviewer-2026-11-01"},
			}
			if len(reviewed.Calls) != len(want) {
				t.Fatalf("calls = %+v, want %+v", reviewed.Calls, want)
			}
			for i, call := range reviewed.Calls {
				if call.Purpose != want[i].Purpose || call.Deployment != want[i].Deployment || call.Served != want[i].Served || call.Fault != "" {
					t.Errorf("call %d = %+v, want %+v", i, call, want[i])
				}
			}
			for _, call := range reviewed.Calls {
				if !call.Usage.Metered {
					t.Errorf("the %s call reports no usage: %+v", call.Purpose, call)
				}
			}
			for _, request := range stand.Requests() {
				if request.Protocol != protocol || request.Model != "reviewer" {
					t.Errorf("a request went by %s to %q, want %s to the deployment", request.Protocol, request.Model, protocol)
				}
			}
		})
	}
}

// Every call waits for the caller's share of its deployment's pacer: a call the quota cannot
// serve in the time it has is refused before it reaches the model.
func TestACallWaitsForThePacer(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Text: noFindings}})
	m := chatModel(stand.URL, "reviewer")
	m.Pace = modelpace.New(modelpace.Config{Quotas: map[string]int{"reviewer": 100}}).Join(modelpace.Share{
		Deployment: modelpace.Deployment{Kind: "azure", Host: "stand-in", Name: "reviewer"},
	})
	_, err := Review(t.Context(), m, []RuleSet{RuleNoslop}, askRecord())
	if !errors.Is(err, modelpace.ErrPastDeadline) {
		t.Fatalf("err = %v, want the pacer's refusal", err)
	}
	if got := len(stand.Requests()); got != 0 {
		t.Errorf("%d requests reached the model past the pacer's refusal", got)
	}
}
