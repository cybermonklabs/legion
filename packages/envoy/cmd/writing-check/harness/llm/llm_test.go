package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

func standInModel(url string) agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: url, Token: func(context.Context) (string, error) { return "stand-in", nil }},
		ID:       "claude-test", Effort: "off", Price: agentloop.Price{Input: 1, Output: 2},
	}
}

var schema = []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)

func TestARunRecordsItsTimeAndItsAttempts(t *testing.T) {
	stand := modeltest.New(t,
		modeltest.Rule{Once: true, Reply: modeltest.Reply{Text: "not an object"}},
		modeltest.Rule{Reply: modeltest.Reply{Text: `{"ok":true}`, Delay: 20 * time.Millisecond}})
	r := &Runner{Limit: NewLimit(1)}
	resp, err := r.Run(context.Background(), standInModel(stand.URL), Request{Purpose: "test", System: "s", User: "u", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Attempts != 2 || resp.Elapsed < 20*time.Millisecond || string(resp.Value) != `{"ok":true}` || resp.Usage.CostUSD <= 0 {
		t.Errorf("a run asked again after a reply out of its schema: %+v", resp)
	}
}

func TestAProviderErrorIsAskedAgainAfterABackoff(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Text: "busy", Status: http.StatusTooManyRequests}})
	var waits []time.Duration
	r := &Runner{Retries: 3, Backoff: time.Second, Sleep: func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}}
	_, err := r.Run(context.Background(), standInModel(stand.URL), Request{Purpose: "test", System: "s", User: "u"})
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Attempts != 4 {
		t.Fatalf("a call the provider kept refusing: %v", err)
	}
	if len(waits) != 3 || waits[0] != time.Second || waits[2] != 4*time.Second {
		t.Errorf("the waits: %v, want 1s, 2s, 4s", waits)
	}
}
