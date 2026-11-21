package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// The reply form's properties reach the provider in the order the form declares them: kind, then
// text, then needs (replySchema). Under a JSON-schema output the model writes the keys in the
// schema's order, so a schema sent in another order has the model decide needs before it has
// written its answer. Proof run 44858316560 sent kind, needs, text on every request, Anthropic's
// and OpenAI's alike, and its last call on A4 replied not_answered with needs "" after the last
// call's message had named the rule.
func TestTheReplyFormReachesTheProviderInTheOrderItDeclares(t *testing.T) {
	const answered = `{"kind":"answered","text":"PR 7 is green.","needs":""}`
	for _, c := range []struct {
		name string
		// schema is the path of the reply form's schema in the provider's request body.
		schema  []string
		request func(*testing.T, *standIn, GH, int) Request
		reply   reply
	}{
		{name: "Anthropic Messages", schema: []string{"output_config", "format", "schema"}, request: anthropicRequest,
			reply: anthropicStream("end_turn", anthropicUsage{Output: 1}, anthropicText(answered))},
		{name: "OpenAI Responses", schema: []string{"text", "format", "schema"}, request: openaiRequest,
			reply: openaiStream(openaiUsage{Output: 1}, "", openaiMessage("msg_o", answered))},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.reply)
			got, err := Answer(context.Background(), c.request(t, s, &stubGH{}, 1))
			if err != nil || got.Fault != "" || got.Kind != Answered {
				t.Fatalf("reply %+v, %v; want answered", got, err)
			}
			sent := s.requests()
			if len(sent) != 1 {
				t.Fatalf("%d model calls, want 1", len(sent))
			}
			value := json.RawMessage(sent[0].body)
			for _, key := range append(c.schema, "properties") {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(value, &object); err != nil {
					t.Fatalf("the request body at %q: %v", key, err)
				}
				if value = object[key]; value == nil {
					t.Fatalf("the request body holds no %q on the path %v", key, c.schema)
				}
			}
			if keys := objectKeys(t, value); !slices.Equal(keys, []string{"kind", "text", "needs"}) {
				t.Errorf("the reply form's properties reach the provider as %v, want [kind text needs], the order replySchema declares", keys)
			}
		})
	}
}

// objectKeys is the keys of the JSON object value in the order its bytes hold them.
func objectKeys(t *testing.T, value json.RawMessage) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(value))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("not an object: %s", value)
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
	return keys
}
