//go:build e2e

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A session can ask the gateway up to eight times, inclusive of retries. The proxy accepts only
// requests carrying this fixture's task, so host side turns cannot spend or enlarge this bound.
const promptInputsGatewayCalls = 8
const promptInputsReadsModel = "claude-opus-5-5"

func (r *promptInputsRun) writeSpend() {
	s := r.spend
	s.mu.Lock()
	defer s.mu.Unlock()
	r.writeJSON("usage/total.json", map[string]any{
		"cap": s.cap, "accounted": s.accounted, "calls": s.calls, "reported": s.usage,
	})
	fmt.Printf("gateway usage: %d calls, %d input, %d cache read, %d cache write, %d output tokens; %d accounted of %d cap\n",
		s.calls, s.usage.Input, s.usage.CacheRead, s.usage.CacheWrite, s.usage.Output, s.accounted, s.cap)
}

type promptInputsGateway struct {
	target       *url.URL
	client       *http.Client
	spend        *promptInputsSpend
	mu           sync.Mutex
	usage        promptInputsUsage
	accounted    int
	auth         string
	firstNonRead string
	calls        int
	accepted     int
	stopped      bool
}

func promptInputsNewGateway(t *testing.T, spend *promptInputsSpend, auth string) *promptInputsGateway {
	t.Helper()
	target, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL"))
	if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Scheme != "http" && target.Scheme != "https") {
		t.Fatal("ANTHROPIC_BASE_URL must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	return &promptInputsGateway{target: target, spend: spend, auth: auth, client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (g *promptInputsGateway) serve(w http.ResponseWriter, req *http.Request, raw []byte, model *promptInputsModel) {
	if !bytes.Contains(raw, []byte(model.task[:24])) {
		streamAnthropicText(w, model.name, "WAITING on the fixture task.")
		return
	}
	// A pane sends one request at a time. Hold this slot until the request ends so a
	// next request cannot enter between the last SSE event and settlement.
	g.mu.Lock()
	if g.stopped || g.calls >= promptInputsGatewayCalls {
		g.mu.Unlock()
		streamAnthropicText(w, model.name, "WAITING: the fixture measurement has ended.")
		return
	}
	defer g.mu.Unlock()

	finish := func(err error) {
		g.stopped = true
		model.mu.Lock()
		defer model.mu.Unlock()
		model.complete(err)
	}
	var payload struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Model != promptInputsReadsModel || payload.MaxTokens <= 0 || payload.MaxTokens > 8192 {
		finish(fmt.Errorf("gateway request did not select %s with max_tokens in 1..8192", promptInputsReadsModel))
		http.Error(w, "model or output bound outside fixture", http.StatusBadRequest)
		return
	}
	target := g.target.JoinPath("v1", "messages")
	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		finish(err)
		http.Error(w, "failed to construct gateway request", http.StatusBadGateway)
		return
	}
	// Use a token per request wire byte and allow for provider framing. Reserve
	// the full output bound in addition to this conservative input estimate.
	inputEstimate := len(raw) + (16 << 10)
	reserved := inputEstimate + payload.MaxTokens
	if !g.spend.reserve(reserved) {
		finish(fmt.Errorf("gateway request would exceed the %d-token run limit", g.spend.cap))
		http.Error(w, "fixture token cap", http.StatusBadRequest)
		return
	}
	g.calls++
	var usage promptInputsUsage
	var streamedOutput int
	var complete, terminal bool
	var endErr error
	defer func() {
		g.accounted += g.spend.settle(reserved, inputEstimate, streamedOutput, usage, complete)
		g.usage.add(usage)
		switch {
		case endErr != nil:
			finish(endErr)
		case terminal:
			finish(nil)
		case g.calls >= promptInputsGatewayCalls:
			finish(fmt.Errorf("the session reached its %d-call budget before finishing its reads", promptInputsGatewayCalls))
		}
	}()
	for _, key := range []string{"Anthropic-Beta", "Anthropic-Version", "Content-Type"} {
		out.Header[key] = req.Header.Values(key)
	}
	// Only the proxy has the short-lived workload identity. It records no headers.
	out.Header.Set("Authorization", "Bearer "+g.auth)
	resp, err := g.client.Do(out)
	if err != nil {
		endErr = fmt.Errorf("gateway request failed: %w", err)
		http.Error(w, "gateway request failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		endErr = fmt.Errorf("gateway answered HTTP %d", resp.StatusCode)
		http.Error(w, "gateway refused the fixture call", resp.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	g.accepted++
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			var event struct {
				Type    string `json:"type"`
				Message struct {
					Usage promptInputsUsage `json:"usage"`
				} `json:"message"`
				Content struct{ Type, Name string } `json:"content_block"`
				Delta   struct {
					Stop        string `json:"stop_reason"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Usage struct {
					Output *int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &event) == nil {
				switch event.Type {
				case "message_start":
					usage = event.Message.Usage
				case "content_block_start":
					if event.Content.Type == "tool_use" && !promptInputsIsReadTool(event.Content.Name) {
						terminal = true
					}
				case "content_block_delta":
					streamedOutput += len(event.Delta.Text) + len(event.Delta.Thinking) + len(event.Delta.PartialJSON)
				case "message_delta":
					// A final delta includes output usage and a stop reason. A stream
					// can end after that delta without a separate message_stop.
					if event.Usage.Output != nil {
						usage.Output = *event.Usage.Output
					}
					if event.Delta.Stop != "" {
						complete = event.Usage.Output != nil && usage.inputTotal() > 0
						terminal = terminal || event.Delta.Stop != "tool_use"
					}
				}
			}
		}
		if _, err := fmt.Fprintln(w, string(line)); err != nil {
			break
		}
		if flusher != nil && len(line) == 0 {
			flusher.Flush()
		}
	}
	endErr = scanner.Err()
}

// Write usage even if the pane never reported a read boundary. The endpoint is closed first,
// so all its handlers have settled their reservations before this cleanup reads the ledger.
func (r *promptInputsRun) writePaneSpend(p *promptInputsPane) {
	g := p.model.gateway
	g.mu.Lock()
	defer g.mu.Unlock()
	r.writeJSON("usage/"+promptInputsSlug(p.name)+".json", map[string]any{
		"usage": g.usage, "accounted": g.accounted, "calls": g.calls, "first_non_read": g.firstNonRead,
	})
	fmt.Printf("   %s: %d gateway calls, %d input, %d cache read, %d cache write, %d output tokens, %d accounted\n",
		p.name, g.calls, g.usage.Input, g.usage.CacheRead, g.usage.CacheWrite, g.usage.Output, g.accounted)
}

func (g *promptInputsGateway) unreachable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls == 1 && g.accepted == 0
}

func (r *promptInputsRun) recordReads(a *liveAgentProof, p *promptInputsPane, session string) []promptInputsRead {
	if session == "" {
		// For the operator's controller there is no claim from which to read SessionFile. Its
		// fresh profile has a dedicated --session-dir, with just one top-level transcript.
		dir := filepath.Join(r.work, "interactive-reads-session")
		a.await("the interactive transcript", 10*time.Second, func() bool {
			files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
			if len(files) != 1 {
				return false
			}
			session = files[0]
			return true
		})
	}
	var data []byte
	var reads []promptInputsReadRecord
	var stop string
	a.await(p.name+" to persist its read results", 10*time.Second, func() bool {
		var err error
		data, err = os.ReadFile(session)
		if err != nil {
			return false
		}
		reads, stop, err = promptInputsTranscriptReads(data)
		if err != nil {
			return false
		}
		if stop != "" {
			return true
		}
		// A single end-turn after the last read is also a measurement boundary.
		return strings.Contains(string(data), `"stopReason":"stop"`) && len(reads) > 0
	})
	r.writeEvidence("reads-transcripts/"+promptInputsSlug(p.name)+".jsonl", data)
	g := p.model.gateway
	g.mu.Lock()
	g.firstNonRead = stop
	g.mu.Unlock()
	out := promptInputsToReads(reads)
	for i := range out {
		if rel, err := filepath.Rel(r.fixture, out[i].Path); err == nil && filepath.IsLocal(rel) {
			out[i].Path = rel
		} else if filepath.IsLocal(out[i].Path) && !strings.Contains(out[i].Path, "://") {
			out[i].Path = filepath.Clean(out[i].Path)
		}
	}
	return out
}
