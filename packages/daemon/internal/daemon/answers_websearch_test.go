package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// The boot's `answers: tools` line, which an after_merge check reads on INFRA, names web_search
// among the tools the loop offers where answers.web_search configures it and answers.providers
// names anthropic, and otherwise names it by its own key with why: no web_search block, or no
// Anthropic provider. `legion config check` prints the same in its one line. A daemon whose
// answers offers the tool hands the loop a web_search tool, which opens each answer's tool; one
// whose answers offers none hands it none.
func TestTheBootLineNamesWebSearchOfferedOrWhyNot(t *testing.T) {
	t.Parallel()
	machine := filepath.Join(t.TempDir(), "public-leak-check.sh")
	anthropic := config.AnswerEndpoint{BaseURL: "https://gateway.internal.example/anthropic", Credential: config.AnswerCredential{File: "/run/legion/anthropic-key"}}
	openai := config.AnswerEndpoint{BaseURL: "https://gateway.internal.example/openai", Credential: config.AnswerCredential{Command: "printf openai-key"}}
	opus := config.AnswerPrice{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	search := &config.AnswerWebSearch{Model: "anthropic/claude-opus-5-5", PricePerSearch: 0.01}
	bounds := config.AnswerBounds{OutputTokens: 4000, GHCall: 30 * time.Second, GHOutputChars: 60000, GHCalls: 20}
	const (
		noDispatch = "no dispatch_url, so no Dispatch to run it against"
		noBlock    = "no answers.web_search block (set its model and price_per_search)"
		noProvider = "answers.providers names no anthropic, the provider every search runs on"
	)
	for _, c := range []struct {
		name    string
		answers config.Answers
		tools   []any
		why     string
		line    string
	}{
		{"offered", config.Answers{
			Providers: map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: anthropic},
			Prices:    map[string]config.AnswerPrice{"anthropic/claude-opus-5-5": opus}, WebSearch: search, Bounds: bounds,
		}, []any{"gh", "web_search"}, "", "answers: tools [gh web_search]; dispatch: " + noDispatch},
		{"no web_search block", config.Answers{
			Providers: map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: anthropic},
			Prices:    map[string]config.AnswerPrice{"anthropic/claude-opus-5-5": opus}, Bounds: bounds,
		}, []any{"gh"}, noBlock, "answers: tools [gh]; dispatch: " + noDispatch + "; web_search: " + noBlock},
		{"no Anthropic provider", config.Answers{
			Providers: map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerOpenAI: openai},
			Prices:    map[string]config.AnswerPrice{"openai/gpt-6.1": {Input: 1.25, Output: 10}}, WebSearch: search, Bounds: bounds,
		}, []any{"gh"}, noProvider, "answers: tools [gh]; dispatch: " + noDispatch + "; web_search: " + noProvider},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{StateDir: t.TempDir(), Answers: &c.answers}
			var logs bytes.Buffer
			w := &workflowRuntime{log: slog.New(slog.NewJSONHandler(&logs, nil)), answers: &answering{}, dispatch: dispatch.New("https://dispatch.internal.example", dispatch.BearerTransport("t", nil))}
			p := plan{answers: answersReads{leakCheck: machine, dispatch: dispatchFound{absent: noDispatch}}}
			stop, err := w.attachAnswers(context.Background(), cfg, p, &supervision{}, nil)
			if err != nil {
				t.Fatalf("attachAnswers: %v", err)
			}
			defer stop()
			var lines []map[string]any
			for _, raw := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var line map[string]any
				if err := json.Unmarshal(raw, &line); err != nil {
					t.Fatal(err)
				}
				if line["msg"] == "answers: tools" {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("boot logged %d answers: tools lines, want 1: %s", len(lines), logs.String())
			}
			if tools, _ := lines[0]["tools"].([]any); !slices.Equal(tools, c.tools) {
				t.Errorf("the line's tools are %v, want %v", lines[0]["tools"], c.tools)
			}
			if reason, named := lines[0]["web_search"]; named != (c.why != "") || named && reason != c.why {
				t.Errorf("the line names web_search as %v (%t), want %q", reason, named, c.why)
			}
			if line, ok := AnswersToolsLine(cfg, environLookup(nil)); !ok || line != c.line {
				t.Errorf("legion config check's line is %q, %t; want %q", line, ok, c.line)
			}

			tools := w.answersLoop(cfg, p, &supervision{}, nil, nil, ghTools{}).WebSearchTools
			if c.why != "" {
				if tools != nil {
					t.Fatalf("a daemon whose answers offers no web_search hands the loop the web_search tools %v; want none", tools)
				}
				return
			}
			if tools == nil {
				t.Fatal("a daemon whose answers offers web_search hands the loop no web_search tool")
			}
			if opened, err := tools.Open("dispatch:m-1"); err != nil || opened == nil {
				t.Fatalf("open the answer's web_search tool: %v, %v", opened, err)
			}
		})
	}
}
