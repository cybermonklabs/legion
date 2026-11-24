package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASeedRefusalPrintsNoBearerItsAnswerEchoes: an answer in Dispatch's place refuses a seed's
// read with a code that repeats the bearer the read carried. The seed's error, and the code a
// route's summary prints, hold none of it: the load test reads a refusal's code through the record
// of the request it sent (redact.Carried), as every client of this repository reads a refused
// answer. Captures loadtest.answer-echo and loadtest.answer-echo/code.
func TestASeedRefusalPrintsNoBearerItsAnswerEchoes(t *testing.T) {
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"code":%q,"error":"refused"}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(stand.Close)
	res := newAPI(stand.URL, redacttest.Token, 1).call(context.Background(), http.MethodGet, "/api/v1/issues/DSP-1", who{}, nil)
	if res.err != nil || res.status != http.StatusBadRequest {
		t.Fatalf("the read answered %d, %v; want the stand-in's 400", res.status, res.err)
	}
	redacttest.RequireNone(t, "loadtest.answer-echo", newSeedError("issue-read", "DSP-1", res.status, res.errorCode()).Error())
	redacttest.RequireNone(t, "loadtest.answer-echo/code", res.errorCode().String())
}
