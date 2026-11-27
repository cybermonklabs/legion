package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
)

// The judge's reply schema reaches its model with reason before verdict, so the model writes why
// before it decides. That is the order the judge's requests carried on its default protocol,
// anthropic-messages, while the SDK's Anthropic and Responses adapters sent every schema with its
// keys sorted. agentloop now sends a schema in the order its caller writes it (its doc.go, "The
// structured reply"), and under structured output the model writes its reply's keys in that order
// (proof run 44858316560 of the Legion answer loop: a reply form sent in another order changed
// what the model wrote), so the order the judge declares is the order its model decides in.
func TestTheJudgesReplySchemaReachesTheModelWithReasonBeforeVerdict(t *testing.T) {
	m := newModel(t, verdictReply(Grant, "The reason names the issue."))
	model := Model{ID: testModel, Endpoint: agentloop.Endpoint{
		Kind: agentloop.OpenAIChat, BaseURL: m.server.URL,
		Token: func(context.Context) (string, error) { return "gateway-token", nil },
	}}
	verdict, _, _, err := model.Ask(t.Context(), tokenData())
	if err != nil || verdict.Word != Grant {
		t.Fatalf("Ask = %+v, %v; want the model's grant", verdict, err)
	}
	m.mu.Lock()
	bodies := append([][]byte(nil), m.bodies...)
	m.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("the model got %d calls, want 1", len(bodies))
	}
	value := json.RawMessage(bodies[0])
	for _, key := range []string{"response_format", "json_schema", "schema", "properties"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(value, &object); err != nil {
			t.Fatalf("the model call at %q: %v", key, err)
		}
		if value = object[key]; value == nil {
			t.Fatalf("the model call holds no %q on its schema's path:\n%s", key, bodies[0])
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(keys, []string{"reason", "verdict"}) {
		t.Errorf("the judge's reply schema reaches its model with its properties as %v, want [reason verdict]: the reason is written before the verdict", keys)
	}
}
