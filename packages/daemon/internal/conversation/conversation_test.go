package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAppendValidateRefusesATurnWithNoContent(t *testing.T) {
	for _, tc := range []struct {
		api  string
		turn Turn
	}{
		{api: AnthropicMessages, turn: Turn{}},
		{api: OpenAIResponses, turn: Turn{
			Content: []Block{},
			Native:  []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)},
		}},
	} {
		t.Run(tc.api, func(t *testing.T) {
			a := Append{
				QuestionID: "q-1", Question: "What does the note say?",
				Provider: "test", API: tc.api, Model: "test-model",
				Turns: []Turn{tc.turn}, NoAnswer: "The model returned nothing.",
			}
			if err := a.Validate(); err == nil {
				t.Fatal("Validate accepted a turn with no content blocks")
			}
		})
	}
}

func TestAppendValidateRefusesReadOnlyBlockFields(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*Append)
	}{
		{"Phase", func(a *Append) { a.Turns[1].Content[0].Phase = "commentary" }},
		{"Detail", func(a *Append) { a.Turns[0].Results[0].Content[0].Detail = "low" }},
		{"ItemID", func(a *Append) { a.Turns[0].Content[0].ItemID = "fc_read" }},
		{"result wrapper", func(a *Append) { a.Turns[0].Results[0].ItemID = "fc_read" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			a := Append{
				QuestionID: "q-1", Question: "What does the note say?",
				Provider: "test", API: OpenAIResponses, Model: "test-model",
				Turns: []Turn{
					{
						Content: []Block{{Type: ToolCall, ToolCallID: "call_read", ToolName: "read"}},
						Results: []Block{{Type: ToolResult, ToolCallID: "call_read", Content: []Block{{Type: Image, MediaType: "image/png", ImageData: "eA=="}}}},
					},
					{Content: []Block{{Type: Text, Text: "Plum."}}},
				},
			}
			if err := a.Validate(); err != nil {
				t.Fatalf("without metadata: %v", err)
			}
			tc.set(&a)
			if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Fatalf("Validate = %v, want a read-only field error", err)
			}
		})
	}
}

func TestAppendValidateRefusesSavedToolChanges(t *testing.T) {
	for _, kind := range []BlockType{ToolAddition, ToolRemoval} {
		t.Run(string(kind), func(t *testing.T) {
			a := Append{
				QuestionID: "q-1", Question: "What does the note say?",
				Provider: "anthropic", API: AnthropicMessages, Model: "claude-test",
				Turns: []Turn{{Content: []Block{{Type: kind, ToolName: "read"}, {Type: Text, Text: "Plum."}}}},
			}
			if err := a.Validate(); err == nil || !strings.Contains(err.Error(), string(kind)+" block in a model turn") {
				t.Fatalf("Validate = %v, want a refusal of the saved %s block", err, kind)
			}
		})
	}
}
