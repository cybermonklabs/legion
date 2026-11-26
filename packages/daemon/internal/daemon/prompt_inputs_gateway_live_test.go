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
const promptInputsTokenCap = 1_850_000

// All real-model sessions of the run share this ledger. Reserve before a request leaves the
// proxy. Release the unused portion only on a complete usage report. If a stream drops, keep the
// reservation: a missing report does not mean the provider did no work.
type promptInputsSpend struct {
	mu        sync.Mutex
	accounted int
	calls     int
	usage     promptInputsUsage
	auth      string
}

func (s *promptInputsSpend) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > promptInputsTokenCap-s.accounted {
		return false
	}
	s.accounted += n
	s.calls++
	return true
}

func (s *promptInputsSpend) settle(reservation int, usage promptInputsUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounted += usage.Input + usage.CacheRead + usage.CacheWrite + usage.Output - reservation
	s.usage.add(usage)
}

func (r *promptInputsRun) writeSpend() {
	s := r.spend
	s.mu.Lock()
	defer s.mu.Unlock()
	r.writeJSON("usage/total.json", map[string]any{
		"cap": promptInputsTokenCap, "accounted": s.accounted, "calls": s.calls, "reported": s.usage,
	})
	fmt.Printf("gateway usage: %d calls, %d input, %d cache read, %d cache write, %d output tokens; %d accounted of %d cap\n",
		s.calls, s.usage.Input, s.usage.CacheRead, s.usage.CacheWrite, s.usage.Output, s.accounted, promptInputsTokenCap)
}

type promptInputsUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}

func (u *promptInputsUsage) add(v promptInputsUsage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
}

type promptInputsGateway struct {
	target   *url.URL
	client   *http.Client
	spend    *promptInputsSpend
	mu       sync.Mutex
	usage    promptInputsUsage
	calls    int
	accepted int
	stopped  bool
}

func promptInputsNewGateway(t *testing.T, spend *promptInputsSpend) *promptInputsGateway {
	t.Helper()
	target, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL"))
	if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Scheme != "http" && target.Scheme != "https") {
		t.Fatal("ANTHROPIC_BASE_URL must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	return &promptInputsGateway{target: target, spend: spend, client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (g *promptInputsGateway) serve(w http.ResponseWriter, req *http.Request, raw []byte, model *promptInputsModel) {
	if !bytes.Contains(raw, []byte(model.task[:24])) {
		streamAnthropicText(w, model.name, "WAITING on the fixture task.")
		return
	}
	g.mu.Lock()
	if g.stopped || g.calls >= promptInputsGatewayCalls {
		g.mu.Unlock()
		streamAnthropicText(w, model.name, "WAITING: the fixture measurement has ended.")
		return
	}
	g.mu.Unlock()

	finish := func(err error) {
		g.mu.Lock()
		g.stopped = true
		g.mu.Unlock()
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
	// Reserve a token per wire byte, an allowance for provider framing, and the full
	// output budget. Cache hits still count as input against the run's cap.
	reserved := len(raw) + (16 << 10) + payload.MaxTokens
	if !g.spend.reserve(reserved) {
		finish(fmt.Errorf("gateway request would exceed the %d-token run limit", promptInputsTokenCap))
		http.Error(w, "fixture token cap", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	target := g.target.JoinPath("v1", "messages")
	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		finish(err)
		http.Error(w, "failed to construct gateway request", 502)
		return
	}
	for _, key := range []string{"Anthropic-Beta", "Anthropic-Version", "Content-Type"} {
		out.Header[key] = req.Header.Values(key)
	}
	// Only the proxy has the short-lived workload identity. It records no headers.
	out.Header.Set("Authorization", "Bearer "+g.spend.auth)
	resp, err := g.client.Do(out)
	if err != nil {
		finish(fmt.Errorf("gateway request failed: %w", err))
		http.Error(w, "gateway request failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		finish(fmt.Errorf("gateway answered HTTP %d", resp.StatusCode))
		http.Error(w, "gateway refused the fixture call", resp.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	g.mu.Lock()
	g.accepted++
	g.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var usage promptInputsUsage
	started, ended := false, false
	terminal := false
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
					Stop string `json:"stop_reason"`
				} `json:"delta"`
				Usage promptInputsUsage `json:"usage"`
			}
			if json.Unmarshal(data, &event) == nil {
				switch event.Type {
				case "message_start":
					usage = event.Message.Usage
					started = true
				case "content_block_start":
					if event.Content.Type == "tool_use" && !promptInputsIsReadTool(event.Content.Name) {
						terminal = true
					}
				case "message_delta":
					usage.Output = event.Usage.Output
					terminal = terminal || event.Delta.Stop != "tool_use"
				case "message_stop":
					ended = true
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
	if started && ended && usage.Input+usage.CacheRead+usage.CacheWrite > 0 {
		g.spend.settle(reserved, usage)
	}
	g.mu.Lock()
	g.usage.add(usage)
	exhausted := g.calls >= promptInputsGatewayCalls
	g.mu.Unlock()
	if err := scanner.Err(); err != nil {
		finish(err)
	} else if terminal {
		finish(nil)
	} else if exhausted {
		finish(fmt.Errorf("the session reached its %d-call budget before finishing its reads", promptInputsGatewayCalls))
	}
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
	usage, calls := g.usage, g.calls
	g.mu.Unlock()
	r.writeJSON("usage/"+promptInputsSlug(p.name)+".json", map[string]any{"usage": usage, "calls": calls, "first_non_read": stop})
	fmt.Printf("   %s: %d gateway calls, %d input, %d cache read, %d cache write, %d output tokens\n", p.name, calls, usage.Input, usage.CacheRead, usage.CacheWrite, usage.Output)
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
