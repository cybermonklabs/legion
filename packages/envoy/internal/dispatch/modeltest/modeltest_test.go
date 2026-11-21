package modeltest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

func post(t *testing.T, url string, body map[string]any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(read)
}

// The stand-in answers each protocol in its own shape, whole or streamed as the request asks,
// from the first rule that matches, and keeps every request with its model and its text.
func TestTheStandInAnswersBothProtocolsByItsRules(t *testing.T) {
	stand := modeltest.New(t,
		modeltest.Rule{Model: "reviewer", Contains: "This names the problem.", Reply: modeltest.Reply{Text: `{"findings":[]}`}},
		modeltest.Rule{Model: "records", Once: true, Reply: modeltest.Reply{Text: "first"}},
		modeltest.Rule{Model: "records", Once: true, Reply: modeltest.Reply{Text: "second"}},
	)
	anthropic := map[string]any{
		"model": "reviewer", "max_tokens": 100,
		"system":   []any{map[string]any{"type": "text", "text": "You review."}},
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "This names the problem."}}}},
	}
	if status, body := post(t, stand.URL+"/v1/messages", anthropic); status != http.StatusOK || !strings.Contains(body, `"text":"{\"findings\":[]}"`) || !strings.Contains(body, `"type":"message"`) {
		t.Errorf("Anthropic, whole: %d %s", status, body)
	}
	anthropic["stream"] = true
	if status, body := post(t, stand.URL+"/v1/messages", anthropic); status != http.StatusOK || !strings.Contains(body, "event: content_block_delta") || !strings.Contains(body, "event: message_stop") {
		t.Errorf("Anthropic, streamed: %d %s", status, body)
	}
	chat := map[string]any{"model": "records", "messages": []any{map[string]any{"role": "user", "content": "Is this a question about words?"}}}
	if status, body := post(t, stand.URL+"/models/chat/completions?api-version=2024-05-01-preview", chat); status != http.StatusOK || !strings.Contains(body, `"content":"first"`) || !strings.Contains(body, `"object":"chat.completion"`) {
		t.Errorf("Chat, whole: %d %s", status, body)
	}
	chat["stream"] = true
	if status, body := post(t, stand.URL+"/chat/completions", chat); status != http.StatusOK || !strings.Contains(body, `"content":"second"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("Chat, streamed: %d %s", status, body)
	}
	// Both Once rules are used up, and no rule answers a model nobody scripted.
	if status, body := post(t, stand.URL+"/chat/completions", chat); status != http.StatusInternalServerError || !strings.Contains(body, "no reply scripted") {
		t.Errorf("Chat, unscripted: %d %s", status, body)
	}
	requests := stand.Requests()
	if len(requests) != 5 {
		t.Fatalf("%d requests kept, want 5", len(requests))
	}
	if first := requests[0]; first.Protocol != modeltest.Anthropic || first.Model != "reviewer" || first.Text != "You review.\nThis names the problem." || first.Stream {
		t.Errorf("the first request = %+v", first)
	}
	if third := requests[2]; third.Protocol != modeltest.Chat || third.Path != "/models/chat/completions" || third.Query != "api-version=2024-05-01-preview" || third.Model != "records" {
		t.Errorf("the third request = %+v", third)
	}
}

// A rule's reply may be an error status, which each protocol answers with its own error object.
func TestTheStandInAnswersAScriptedErrorInEachProtocolsShape(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Status: http.StatusTooManyRequests, Text: "slow down"}})
	if status, body := post(t, stand.URL+"/v1/messages", map[string]any{"model": "m", "messages": []any{}}); status != http.StatusTooManyRequests || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "slow down") {
		t.Errorf("Anthropic: %d %s", status, body)
	}
	if status, body := post(t, stand.URL+"/chat/completions", map[string]any{"model": "m", "messages": []any{}}); status != http.StatusTooManyRequests || !strings.Contains(body, `"error":{`) || !strings.Contains(body, "slow down") {
		t.Errorf("Chat: %d %s", status, body)
	}
}
