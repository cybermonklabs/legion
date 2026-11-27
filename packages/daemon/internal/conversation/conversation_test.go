package conversation

import (
	"encoding/json"
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
