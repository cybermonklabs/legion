package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

const (
	bundleModel = "reader-model-2026"
	deployment  = "reader-deployment"
	apiVersion  = "2026-01-01"
	token       = "stub-token"
)

// stub is a Chat Completions endpoint that records every request and answers each by its
// answer function: a status, the model id its stream names, the reply's text and a delay.
type stub struct {
	server *httptest.Server
	answer func(sent sentInput) (status int, model, text string, delay time.Duration)

	mu  sync.Mutex
	got []recorded
}

type recorded struct {
	path, query, auth string
	body              map[string]any
	user              sentInput
}

// sentInput is the user message a call carried, decoded.
type sentInput struct {
	Candidates []input `json:"candidates"`
}

func newStub(t *testing.T, answer func(sentInput) (int, string, string, time.Duration)) *stub {
	t.Helper()
	s := &stub{answer: answer}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := recorded{path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
	_ = json.Unmarshal(raw, &rec.body)
	if messages, ok := rec.body["messages"].([]any); ok {
		for _, m := range messages {
			if msg, _ := m.(map[string]any); msg["role"] == "user" {
				_ = json.Unmarshal([]byte(contentText(msg["content"])), &rec.user)
			}
		}
	}
	s.mu.Lock()
	s.got = append(s.got, rec)
	s.mu.Unlock()

	status, model, text, delay := s.answer(rec.user)
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"the stub refuses","type":"invalid_request_error"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(choices []any, usage any) {
		c := map[string]any{"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": choices}
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	delta := func(d map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
	}
	chunk(delta(map[string]any{"role": "assistant", "content": ""}, nil), nil)
	chunk(delta(map[string]any{"content": text}, nil), nil)
	chunk(delta(map[string]any{}, "stop"), nil)
	chunk([]any{}, map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// contentText is a chat message's content as text, whether it was sent as a string or as parts.
func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if p, _ := part.(map[string]any); p["type"] == "text" {
				b.WriteString(fmt.Sprint(p["text"]))
			}
		}
		return b.String()
	}
	return ""
}

func (s *stub) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.got...)
}

// verdictsFor answers every sent candidate with vacuous, as the model's reply text.
func verdictsFor(sent sentInput, vacuous bool) string {
	var reply struct {
		Verdicts []Verdict `json:"verdicts"`
	}
	for _, c := range sent.Candidates {
		v := Verdict{ID: c.ID, Vacuous: vacuous, Reason: "The sentence states a fact."}
		if vacuous {
			v.Rewrite, v.Reason = "Say what the problem is.", "The verb phrase states no fact."
		}
		reply.Verdicts = append(reply.Verdicts, v)
	}
	b, _ := json.Marshal(reply)
	return string(b)
}

func newReader(s *stub, cfg Config) *Reader {
	cfg.Model = AzureModel(s.server.URL+"/", deployment, apiVersion, func(context.Context) (string, error) { return token, nil })
	if cfg.BundleModelID == "" {
		cfg.BundleModelID = bundleModel
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return New(cfg)
}

// cands is the candidates of body.
func cands(t *testing.T, body string) []candidates.Candidate {
	t.Helper()
	found := candidates.Find(prose.ProseSentences("description", body), candidates.Options{})
	if len(found) == 0 {
		t.Fatalf("no candidates in %q", body)
	}
	return found
}

const twoVacuous = "The cache holds 900 entries. This names the problem. The fix shows the gap."

func TestAGoodAnswerIsOneVerdictPerCandidate(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, bundleModel, verdictsFor(sent, true), 0
	})
	r := newReader(s, Config{})
	verdicts, outcome := r.Read(context.Background(), cands(t, twoVacuous))
	if outcome != OutcomeRead {
		t.Fatalf("outcome %q, want a read", outcome)
	}
	want := []Verdict{
		{ID: "c1", Vacuous: true, Rewrite: "Say what the problem is.", Reason: "The verb phrase states no fact."},
		{ID: "c2", Vacuous: true, Rewrite: "Say what the problem is.", Reason: "The verb phrase states no fact."},
	}
	if !reflect.DeepEqual(verdicts, want) {
		t.Errorf("verdicts %+v, want %+v", verdicts, want)
	}
	got := s.requests()
	if len(got) != 1 || r.Calls() != 1 {
		t.Fatalf("%d requests, %d calls counted, want one of each", len(got), r.Calls())
	}
	req := got[0]
	if req.path != "/chat/completions" || req.query != "api-version="+apiVersion || req.auth != "Bearer "+token {
		t.Errorf("request to %s?%s with %q, want the Azure chat path, api-version and bearer token", req.path, req.query, req.auth)
	}
	if req.body["model"] != deployment || req.body["reasoning_effort"] != "none" {
		t.Errorf("model %v, reasoning_effort %v, want the deployment and no reasoning", req.body["model"], req.body["reasoning_effort"])
	}
	if _, ok := req.body["response_format"]; !ok {
		t.Error("the request carries no response_format, want the verdict schema")
	}
	wantUser := sentInput{Candidates: []input{
		{ID: "c1", Field: "description", Text: "This names the problem.",
			Before: []string{"The cache holds 900 entries."}, After: []string{"The fix shows the gap."}},
		{ID: "c2", Field: "description", Text: "The fix shows the gap.",
			Before: []string{"The cache holds 900 entries.", "This names the problem."}, After: []string{}},
	}}
	if !reflect.DeepEqual(req.user, wantUser) {
		t.Errorf("user message %+v, want %+v", req.user, wantUser)
	}
	system := contentText(req.body["messages"].([]any)[0].(map[string]any)["content"])
	if system != Prompt || !strings.Contains(Prompt, "data") {
		t.Error("the system message is not the embedded prompt")
	}
}

func TestEachFaultLetsTheWriteThroughWithItsOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(sentInput) (int, string, string, time.Duration)
		want   string
	}{
		{"late", func(s sentInput) (int, string, string, time.Duration) {
			return http.StatusOK, bundleModel, verdictsFor(s, true), 2 * time.Second
		}, OutcomeTimeout},
		{"malformed", func(sentInput) (int, string, string, time.Duration) {
			return http.StatusOK, bundleModel, `{"verdicts": [{"id": "c1", "vacuous": "maybe"`, 0
		}, OutcomeMalformed},
		{"unknown id", func(sentInput) (int, string, string, time.Duration) {
			return http.StatusOK, bundleModel, `{"verdicts": [{"id": "c9", "vacuous": true, "rewrite": "", "reason": ""}]}`, 0
		}, OutcomeMalformed},
		{"error", func(sentInput) (int, string, string, time.Duration) {
			return http.StatusBadRequest, bundleModel, "", 0
		}, OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, tc.answer)
			r := newReader(s, Config{Timeout: 300 * time.Millisecond})
			verdicts, outcome := r.Read(context.Background(), cands(t, twoVacuous))
			if outcome != tc.want || verdicts != nil {
				t.Errorf("verdicts %+v, outcome %q, want none and %q", verdicts, outcome, tc.want)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("%d requests, want exactly one", n)
			}
		})
	}
}

// A reply served by another model is a mismatch, and the reader then stays off: no later Read
// calls the model.
func TestAModelMismatchTurnsTheReaderOff(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, "another-model", verdictsFor(sent, true), 0
	})
	r := newReader(s, Config{})
	for i := range 3 {
		verdicts, outcome := r.Read(context.Background(), cands(t, twoVacuous))
		if outcome != OutcomeModelMismatch || verdicts != nil {
			t.Fatalf("read %d: verdicts %+v, outcome %q, want none and %q", i, verdicts, outcome, OutcomeModelMismatch)
		}
	}
	if n := len(s.requests()); n != 1 || r.Calls() != 1 {
		t.Errorf("%d requests, %d calls counted, want the one call before the mismatch and none after", n, r.Calls())
	}
}

// More than CandidateCap candidates send the first CandidateCap and say the rest were not read.
func TestTheCandidateCap(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, bundleModel, verdictsFor(sent, false), 0
	})
	r := newReader(s, Config{})
	body := strings.Repeat("This names the problem. ", CandidateCap+6)
	verdicts, outcome := r.Read(context.Background(), cands(t, body))
	if outcome != OutcomeCandidateCap || len(verdicts) != CandidateCap {
		t.Fatalf("%d verdicts, outcome %q, want %d and %q", len(verdicts), outcome, CandidateCap, OutcomeCandidateCap)
	}
	got := s.requests()
	if len(got) != 1 || len(got[0].user.Candidates) != CandidateCap {
		t.Fatalf("%d requests, want one carrying %d candidates", len(got), CandidateCap)
	}
}

// A call past the minute's token budget is not made.
func TestABudgetSpentMakesNoCall(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, bundleModel, verdictsFor(sent, false), 0
	})
	r := newReader(s, Config{TokensPerMinute: 100})
	verdicts, outcome := r.Read(context.Background(), cands(t, twoVacuous))
	if outcome != OutcomeBudget || verdicts != nil {
		t.Errorf("verdicts %+v, outcome %q, want none and %q", verdicts, outcome, OutcomeBudget)
	}
	if n := len(s.requests()); n != 0 || r.Calls() != 0 {
		t.Errorf("%d requests, %d calls counted, want none", n, r.Calls())
	}
}

func TestNoCandidatesMakeNoCall(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, bundleModel, verdictsFor(sent, false), 0
	})
	r := newReader(s, Config{})
	if verdicts, outcome := r.Read(context.Background(), nil); verdicts != nil || outcome != OutcomeRead {
		t.Errorf("verdicts %+v, outcome %q, want none and a read", verdicts, outcome)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// NoEffort sends no reasoning_effort at all, and Observe sees each call's served model id.
func TestNoEffortSendsNoEffortAndObserveSeesTheServedModel(t *testing.T) {
	t.Parallel()
	s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
		return http.StatusOK, bundleModel, verdictsFor(sent, false), 0
	})
	var calls []Call
	r := newReader(s, Config{NoEffort: true, Observe: func(c Call) { calls = append(calls, c) }})
	if _, outcome := r.Read(context.Background(), cands(t, twoVacuous)); outcome != OutcomeRead {
		t.Fatalf("outcome %q, want a read", outcome)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests, want one", len(got))
	}
	if effort, ok := got[0].body["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort %v sent, want none", effort)
	}
	if len(calls) != 1 || calls[0].Model != bundleModel || calls[0].Fault != "" || calls[0].Err != nil {
		t.Errorf("observed %+v, want one clean call served by %s", calls, bundleModel)
	}
}

// Observe sees a refused call's fault, with no served model, and a mismatched model's id.
func TestObserveSeesFaultsAndAMismatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		model     string
		wantModel string
		wantFault bool
	}{
		{"refused", http.StatusBadRequest, bundleModel, "", true},
		{"mismatch", http.StatusOK, "another-model", "another-model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, func(sent sentInput) (int, string, string, time.Duration) {
				return tc.status, tc.model, verdictsFor(sent, false), 0
			})
			var calls []Call
			r := newReader(s, Config{Observe: func(c Call) { calls = append(calls, c) }})
			r.Read(context.Background(), cands(t, twoVacuous))
			if len(calls) != 1 {
				t.Fatalf("%d calls observed, want one", len(calls))
			}
			c := calls[0]
			if c.Model != tc.wantModel || (c.Fault != "" || c.Err != nil) != tc.wantFault {
				t.Errorf("observed %+v, want model %q and a fault %v", c, tc.wantModel, tc.wantFault)
			}
		})
	}
}
