package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADispatchAnswerPrintsThroughAnswerWordsWithNoBearerItEchoes: an answer in Dispatch's place
// refuses a call with a body that repeats the bearer the call carried. What every log line of a
// call's result prints of the answer (callResult.text, through answerWords) holds no run of the
// bearer (echo), while the answer as it came holds it whole (twin). Captures
// loadtest.answer-words/echo.
func TestADispatchAnswerPrintsThroughAnswerWordsWithNoBearerItEchoes(t *testing.T) {
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"the gateway refused %s","code":"BAD_GATEWAY"}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(stand.Close)
	res := newAPI(stand.URL, redacttest.Token, 1).call(context.Background(), http.MethodGet, "/api/v1/projects", who{}, nil)
	if res.err != nil || res.status != http.StatusBadGateway {
		t.Fatalf("the call answered %d, %v; want the stand-in's 502", res.status, res.err)
	}
	t.Run("twin", func(t *testing.T) {
		if !strings.Contains(string(res.body), redacttest.Token) {
			t.Errorf("the answer as it came is %q; want the bearer in it, so the row can see it printed", res.body)
		}
	})
	t.Run("echo", func(t *testing.T) {
		said := res.text()
		redacttest.RequireNoPiece(t, "loadtest.answer-words/echo", said, redacttest.Token)
		if !strings.Contains(said, "the gateway refused") {
			t.Errorf("the call's words are %q; want the answer's own words beside the bearer printed", said)
		}
	})
}
