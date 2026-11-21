package reviewer

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// sentSystemPrompt is the system prompt of a request the stand-in model got on the Anthropic
// Messages protocol: the request's system field, a string or the text of each of its blocks.
func sentSystemPrompt(t *testing.T, request modeltest.Request) string {
	t.Helper()
	var body struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("read a request: %v", err)
	}
	var system string
	if json.Unmarshal(body.System, &system) == nil {
		return system
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body.System, &blocks); err != nil {
		t.Fatalf("read a request's system prompt %s: %v", body.System, err)
	}
	var b strings.Builder
	for _, block := range blocks {
		b.WriteString(block.Text)
	}
	return b.String()
}

// LeastSystemPromptRunes of purposes is the shortest system prompt the calls of those purposes
// send, a review, a rewrite and a definition of every kind of text, as the stand-in model gets
// them, and 0 where they make no call: a review by no rule set. The boot weighs each model group's
// quota against the charge of it (cmd/dispatch), so a floor taken from another purpose's prompts
// would start a group whose calls the pacer refuses, or refuse one whose calls it serves.
func TestTheLeastSystemPromptIsTheShortestTheCallsOfItsPurposesSend(t *testing.T) {
	markers := map[string]string{PurposeReview: reviewMarker, PurposeRewrite: rewriteMarker, PurposeDefine: defineMarker}
	for name, rules := range map[string][]RuleSet{
		"the default rule sets": {RuleNoslop, RulePhrases, RuleReaderWords, RuleAskParts},
		"ask_parts":             {RuleAskParts},
		"structure":             {RuleStructure},
		"no rule set":           {},
	} {
		t.Run(name, func(t *testing.T) {
			stand := modeltest.New(t,
				modeltest.Rule{Contains: reviewMarker, Reply: modeltest.Reply{Text: noFindings}},
				modeltest.Rule{Contains: rewriteMarker, Reply: modeltest.Reply{Text: `{"body":"Ship it."}`}},
				modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"ship","meaning":"Release.","links":[]}]}`}},
			)
			m := anthropicModel(stand.URL, "m")
			for _, kind := range Kinds {
				record := Record{Text: Text{Kind: kind, Title: "Ship", Body: "Ship it."}}
				_, _ = Review(t.Context(), m, rules, record)
				_, _ = Rewrite(t.Context(), m, rules, record, Answer{})
				_, _ = Define(t.Context(), m, rules, record, []string{"ship"}, Answer{})
			}
			sent := map[string]int{}
			for _, request := range stand.Requests() {
				for purpose, marker := range markers {
					if !strings.Contains(request.Text, marker) {
						continue
					}
					n := utf8.RuneCountInString(sentSystemPrompt(t, request))
					if least, seen := sent[purpose]; !seen || n < least {
						sent[purpose] = n
					}
				}
			}
			if _, reviewed := sent[PurposeReview]; reviewed == (len(rules) == 0) || sent[PurposeRewrite] == 0 || sent[PurposeDefine] == 0 {
				t.Fatalf("the shortest system prompt each purpose sent = %v", sent)
			}
			for _, purposes := range [][]string{{PurposeReview}, {PurposeRewrite, PurposeDefine}, {PurposeReview, PurposeRewrite, PurposeDefine}} {
				want := 0
				for _, purpose := range purposes {
					if n, seen := sent[purpose]; seen && (want == 0 || n < want) {
						want = n
					}
				}
				if got, err := LeastSystemPromptRunes(rules, purposes...); err != nil || got != want {
					t.Errorf("LeastSystemPromptRunes(%v) = %d, %v; want %d, the shortest the calls sent", purposes, got, err, want)
				}
			}
		})
	}
}
