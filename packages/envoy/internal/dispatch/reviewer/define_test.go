package reviewer

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// Define says what each selection means, from the record and the writer's answer, with the links
// of the record, and the call that defined them.
func TestDefineDefinesEachSelection(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Contains: defineMarker, Reply: modeltest.Reply{Text: `{"definitions":[` +
		`{"words":"store","meaning":"The database that keeps the rows.","links":["dispatch://PRJ-1/spec#b-1"]},` +
		`{"words":"the node","meaning":"The test server.","links":[]}]}`}})
	url := servedAs(t, stand, "definer", "definer-2026-11-01")
	question, err := DefineQuestion("a-1", KindAsk, []string{"store", "the node"})
	if err != nil {
		t.Fatal(err)
	}
	defined, err := Define(t.Context(), chatModel(url, "definer"), []RuleSet{RuleNoslop, RuleReaderWords, RuleAskParts}, askRecord(),
		[]string{" store ", "the node", "  "}, Answer{Question: question, Text: "By the node I mean the test server."})
	if err != nil {
		t.Fatal(err)
	}
	want := []Definition{
		{Words: "store", Meaning: "The database that keeps the rows.", Links: []string{"dispatch://PRJ-1/spec#b-1"}},
		{Words: "the node", Meaning: "The test server.", Links: []string{}},
	}
	if !reflect.DeepEqual(defined.Definitions, want) {
		t.Errorf("definitions = %+v, want %+v", defined.Definitions, want)
	}
	if len(defined.Calls) != 1 || defined.Calls[0].Purpose != PurposeDefine || defined.Calls[0].Deployment != "definer" || defined.Calls[0].Served != "definer-2026-11-01" {
		t.Errorf("calls = %+v, want the definition's call to the definer, served by definer-2026-11-01", defined.Calls)
	}
	request := stand.Requests()[0].Text
	for _, want := range []string{
		"<words>\n- store\n- the node\n</words>",
		"<question_to_writer>\nA person reads your ask a-1. They selected these words in it and asked what they mean:\n\n- store\n- the node",
		"<writer_answer>\nBy the node I mean the test server.\n</writer_answer>",
		"<reader_words>\n- store\n</reader_words>",
	} {
		if !strings.Contains(request, want) {
			t.Errorf("the definition's request does not hold %q", want)
		}
	}
	// ask_parts is no rule of a definition.
	if strings.Contains(request, `### Rule set "ask_parts"`) {
		t.Error("a definition's request holds the ask_parts rule set")
	}
}

// Define of no words makes no call, and a reply that defines nothing gave no usable answer.
func TestDefineOfNothing(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Text: `{"definitions":[]}`}})
	if _, err := Define(t.Context(), chatModel(stand.URL, "definer"), []RuleSet{RuleNoslop}, askRecord(), []string{" ", ""}, Answer{}); !errors.Is(err, ErrNoWords) {
		t.Errorf("Define of no words: %v, want ErrNoWords", err)
	}
	if got := len(stand.Requests()); got != 0 {
		t.Errorf("Define of no words made %d requests", got)
	}
	defined, err := Define(t.Context(), chatModel(stand.URL, "definer"), []RuleSet{RuleNoslop}, askRecord(), []string{"store"}, Answer{})
	var failure *ModelError
	if !errors.As(err, &failure) || failure.Fault != agentloop.FaultMalformed {
		t.Errorf("a reply that defines nothing: %v, want a malformed reply", err)
	}
	if len(defined.Calls) != 1 || defined.Calls[0].Fault != agentloop.FaultMalformed {
		t.Errorf("calls = %+v, want the malformed call", defined.Calls)
	}
}
