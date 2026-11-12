package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/api"
)

// decisionEnvironment is a server configured with the decision model: the measured model, its
// https endpoint and a token file's path, with overrides on top.
func decisionEnvironment(overrides map[string]string) map[string]string {
	return withOverrides(map[string]string{
		"DISPATCH_DECISION_MODEL":            "jev-latest",
		"DISPATCH_DECISION_MODEL_ENDPOINT":   "https://decide.example",
		"DISPATCH_DECISION_MODEL_TOKEN_FILE": "/var/run/secrets/decision/token",
	}, overrides)
}

// systemOneStandIn is a System One endpoint a decider is pointed at: it answers every call with
// the three answers and records the bearer each call presented. It calls nothing and holds no
// credential.
type systemOneStandIn struct {
	url     string
	mu      sync.Mutex
	bearers []string
}

func newSystemOneStandIn(t *testing.T) *systemOneStandIn {
	t.Helper()
	standIn := &systemOneStandIn{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		standIn.mu.Lock()
		standIn.bearers = append(standIn.bearers, r.Header.Get("Authorization"))
		standIn.mu.Unlock()
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-latest","answers":{`+
			`"action":{"type":"choice","choice":"none","confidence":0.9},`+
			`"context":{"type":"choice","choice":"none","confidence":0.8},`+
			`"display":{"type":"choice","choice":"prose","confidence":0.9}},`+
			`"usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	t.Cleanup(server.Close)
	standIn.url = server.URL
	return standIn
}

// calls are the bearers the stand-in was presented, one per call it received.
func (s *systemOneStandIn) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bearers...)
}

// decide makes one decision through decider with the questions the server asks.
func decide(t *testing.T, decider *api.Decider) error {
	t.Helper()
	questions := going.DecisionQuestions([]going.Action{{Name: "dispatch_claim", Description: "Claim a Dispatch issue, or release it."}})
	result, err := decider.Decide(t.Context(), questions, going.DecisionState("what is open?", going.PageContext{Path: "/issues/DEMO-12"}))
	if err == nil && result.Fault != "" {
		t.Fatalf("the decision ended %s: %s", result.Fault, result.Detail)
	}
	return err
}

// With none of the three set the server boots, builds no decider, and says so in one line.
func TestDecisionModelUnsetBootsAndDecidesNothing(t *testing.T) {
	boot := resolveWith(t, nil)
	if boot.Decision != (decisionConfig{}) {
		t.Fatalf("no decision settings: Decision = %+v, want none", boot.Decision)
	}
	logged := capturedLog(t)
	if decider := newDecider(boot.Decision); decider != nil {
		t.Fatalf("no decision settings: newDecider built %q, want none", decider.Model())
	}
	want := `level=WARN msg="dispatch: no decision model configured, so the command bar decides nothing" setting=DISPATCH_DECISION_MODEL` + "\n"
	if line := logged.String(); !strings.HasSuffix(line, want) || strings.Count(line, "\n") != 1 {
		t.Fatalf("logged %q, want the one line %q", line, want)
	}
}

// The endpoint and the token file are set together or not at all, the refusal naming the one
// missing; the model may be left out; and the endpoint is an absolute https base URL.
func TestDecisionModelRefusesAShapeItCannotUse(t *testing.T) {
	refusedWith(t, decisionEnvironment(map[string]string{"DISPATCH_DECISION_MODEL_TOKEN_FILE": ""}),
		"DISPATCH_DECISION_MODEL_TOKEN_FILE is required when DISPATCH_DECISION_MODEL_ENDPOINT is set")
	refusedWith(t, decisionEnvironment(map[string]string{"DISPATCH_DECISION_MODEL_ENDPOINT": ""}),
		"DISPATCH_DECISION_MODEL_ENDPOINT is required when DISPATCH_DECISION_MODEL_TOKEN_FILE is set")
	for _, endpoint := range []string{"http://decide.example", "decide.example", "https://", "https://decide.example?key=1", "https://decide.example#top"} {
		refusedWith(t, decisionEnvironment(map[string]string{"DISPATCH_DECISION_MODEL_ENDPOINT": endpoint}),
			`DISPATCH_DECISION_MODEL_ENDPOINT="`+endpoint+`" (expected an absolute https URL`)
	}
	if boot := resolveWith(t, decisionEnvironment(map[string]string{"DISPATCH_DECISION_MODEL": ""})); boot.Decision.Endpoint != "https://decide.example" || boot.Decision.Model != "" {
		t.Errorf("endpoint and token file with no model: Decision = %+v, want the two with no model", boot.Decision)
	}
	if boot := resolveWith(t, map[string]string{"DISPATCH_DECISION_MODEL": "jev-latest"}); boot.Decision.Model != "jev-latest" || boot.Decision.Endpoint != "" {
		t.Errorf("a model with no endpoint: Decision = %+v, want the model alone", boot.Decision)
	}
}

// The bearer every call presents is what the token file holds at that call: a token rewritten
// between two calls is the one the second presents.
func TestDecisionModelReadsItsTokenFileOnEveryCall(t *testing.T) {
	standIn := newSystemOneStandIn(t)
	tokenFile := writeFile(t, "decision-token", "first-bearer\n")
	decider := newDecider(decisionConfig{Model: "jev-latest", Endpoint: standIn.url, TokenFile: tokenFile})
	if decider == nil {
		t.Fatal("newDecider built no decider for the measured model")
	}
	if err := decide(t, decider); err != nil {
		t.Fatalf("the first decision: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("second-bearer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := decide(t, decider); err != nil {
		t.Fatalf("the second decision: %v", err)
	}
	if calls := standIn.calls(); len(calls) != 2 || calls[0] != "Bearer first-bearer" || calls[1] != "Bearer second-bearer" {
		t.Fatalf("the endpoint was presented %q, want the file's first bearer and then its rewritten one", calls)
	}
}

// A token file that is absent or empty boots, reads as unreachable, and fails each call as the
// model that cannot be reached with no request made; once the file is written the next call is
// made with it.
func TestDecisionModelWithNoTokenFileIsUnreachable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "decision-token")
	boot := resolveWith(t, decisionEnvironment(map[string]string{"DISPATCH_DECISION_MODEL_TOKEN_FILE": missing}))
	if boot.Decision.TokenFile != missing {
		t.Fatalf("TokenFile = %q, want %q: a file not written yet boots", boot.Decision.TokenFile, missing)
	}
	standIn := newSystemOneStandIn(t)
	decider := newDecider(decisionConfig{Model: boot.Decision.Model, Endpoint: standIn.url, TokenFile: missing})
	if decider == nil {
		t.Fatal("newDecider built no decider for the measured model")
	}
	// In order: the file is absent first, then written empty.
	for _, name := range []string{"absent", "empty"} {
		if name == "empty" {
			writeTokenFile(t, missing, " \n")
		}
		if decider.Reachable(t.Context()) {
			t.Errorf("%s token file: Reachable = true, want unreachable", name)
		}
		if err := decide(t, decider); !errors.Is(err, api.ErrDecisionModelUnreachable) {
			t.Errorf("%s token file: Decide = %v, want %v", name, err, api.ErrDecisionModelUnreachable)
		}
	}
	if calls := standIn.calls(); len(calls) != 0 {
		t.Fatalf("the endpoint was called %d times with no token to present, want none", len(calls))
	}

	writeTokenFile(t, missing, "written-later")
	if !decider.Reachable(t.Context()) {
		t.Error("written token file: Reachable = false")
	}
	if err := decide(t, decider); err != nil {
		t.Fatalf("written token file: Decide = %v", err)
	}
	if calls := standIn.calls(); len(calls) != 1 || calls[0] != "Bearer written-later" {
		t.Fatalf("the endpoint was presented %q, want the one call with the file written later", calls)
	}
}

// A model the committed record does not hold is no decision model: no decider, one line naming
// it, and no call to its endpoint. A model named with no endpoint, and an endpoint with no model,
// build none either.
func TestDecisionModelUnmeasuredBuildsNoDecider(t *testing.T) {
	standIn := newSystemOneStandIn(t)
	tokenFile := writeFile(t, "decision-token", "bearer")
	logged := capturedLog(t)
	if decider := newDecider(decisionConfig{Model: "unmeasured-model", Endpoint: standIn.url, TokenFile: tokenFile}); decider != nil {
		t.Fatalf("an unmeasured model built a decider of %q", decider.Model())
	}
	line := logged.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "not one the committed measurement holds") || !strings.Contains(line, "model=unmeasured-model") {
		t.Fatalf("logged %q, want one line naming the unmeasured model", line)
	}
	if calls := standIn.calls(); len(calls) != 0 {
		t.Fatalf("the endpoint was called %d times for an unmeasured model", len(calls))
	}
	if decider := newDecider(decisionConfig{Model: "jev-latest"}); decider != nil {
		t.Error("a model with no endpoint built a decider")
	}
	if decider := newDecider(decisionConfig{Endpoint: standIn.url, TokenFile: tokenFile}); decider != nil {
		t.Error("an endpoint with no model built a decider")
	}
}

// A measured model builds a decider carrying that model's own entry of the record: its two fitted
// thresholds, which the boot line names.
func TestDecisionModelMeasuredBuildsOneWithItsThresholds(t *testing.T) {
	want, measured := going.DecisionRecord("jev-latest")
	if !measured || want.ActionThreshold == nil {
		t.Fatalf("the record holds jev-latest = %v with action threshold %v, want a measured entry", measured, want.ActionThreshold)
	}
	cfg := resolveWith(t, decisionEnvironment(nil)).Decision
	logged := capturedLog(t)
	decider := newDecider(cfg)
	if decider == nil {
		t.Fatal("newDecider built no decider for jev-latest")
	}
	got := decider.Record()
	if decider.Model() != "jev-latest" || got.Model != "jev-latest" ||
		*got.ActionThreshold != *want.ActionThreshold || (got.ChartThreshold == nil) != (want.ChartThreshold == nil) ||
		(got.ChartThreshold != nil && *got.ChartThreshold != *want.ChartThreshold) {
		t.Fatalf("the decider is %q with %+v, want jev-latest with the record's thresholds %+v", decider.Model(), got, want)
	}
	line := logged.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "model=jev-latest host=decide.example") || strings.Contains(line, cfg.TokenFile) {
		t.Fatalf("logged %q, want one line naming the model and the host and no credential", line)
	}
}

// writeTokenFile writes contents to path.
func writeTokenFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
