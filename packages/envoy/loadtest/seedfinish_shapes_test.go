package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// shapeCoverageBody is a coverage body as the server writes it (model.SearchCoverage), covering
// four issues and lacking nothing, with each embedder member as embedder names it, which is a
// JSON null where it is nil.
func shapeCoverageBody(embedder map[string]any) map[string]any {
	none := map[string]any{"items": 0, "current": 0, "lacking": 0, "failed": 0, "oldest_lacking_seconds": nil}
	issue := map[string]any{"items": 4, "current": 4, "lacking": 0, "failed": 0, "oldest_lacking_seconds": nil}
	body := map[string]any{"total": issue, "kinds": map[string]any{"issue": issue, "document_block": none,
		"comment_block": none, "message_block": none, "ask_part": none}}
	for member, value := range embedder {
		body[member] = value
	}
	return body
}

// shapeCoverageReader reads the coverage report of a stand-in that answers the embedder members
// it names of the read, with the member rows' bearer; f is a gate reading through it, writing no
// record and logging to logged.
func shapeCoverageReader(t *testing.T, embedder func(r *http.Request) map[string]any, logged *bytes.Buffer) (dispatchCoverage, *seedFinisher) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONAnswer(w, http.StatusOK, shapeCoverageBody(embedder(r)))
	}))
	t.Cleanup(srv.Close)
	cov := dispatchCoverage{a: newAPI(srv.URL, memberBearer, 2)}
	f, _ := fakeEmbedFinisher(time.Hour, cov, "", logged)
	return cov, f
}

// TestTheCoverageReadNamesTheEmbedderAsTheRuleReadsItAndNamesNoneWhereTheReportDoes: the gate
// reads the embedder's identity, place and model as Dispatch's words, held as the rule reads them
// with the read's record, and the report it answers holds each as that reading; a report that
// names none of the three (the deployment with no embedder) answers none of them, and the gate
// refuses it at its first poll rather than waiting out its bound. The counts are Dispatch's own
// numbers, read as they came.
func TestTheCoverageReadNamesTheEmbedderAsTheRuleReadsItAndNamesNoneWhereTheReportDoes(t *testing.T) {
	t.Run("named", func(t *testing.T) {
		var logged bytes.Buffer
		cov, _ := shapeCoverageReader(t, func(r *http.Request) map[string]any {
			return map[string]any{"identity": echoed(r), "place": "azure", "model": redacttest.Code}
		}, &logged)
		coverage, err := cov.readCoverage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if coverage.Identity == nil || coverage.Place == nil || coverage.Model == nil {
			t.Fatalf("the report names identity %v, place %v, model %v; want all three", coverage.Identity, coverage.Place, coverage.Model)
		}
		for member, shown := range map[string]string{"identity": *coverage.Identity, "model": *coverage.Model} {
			if !strings.Contains(shown, redacttest.CodeShown) {
				t.Errorf("%s reads %q, want the URL it quotes as the rule reads it", member, shown)
			}
			if strings.Contains(shown, memberBearer) || strings.Contains(shown, redacttest.Password) {
				t.Errorf("%s reads %q, which holds a credential the read carried", member, shown)
			}
		}
		if *coverage.Place != "azure" {
			t.Errorf("place reads %q, want the word the report named", *coverage.Place)
		}
		if coverage.Kinds.Issue.Items != 4 || coverage.Total.Items != 4 {
			t.Errorf("the report counts %d issue items of %d, want the body's 4 of 4", coverage.Kinds.Issue.Items, coverage.Total.Items)
		}
	})
	t.Run("none", func(t *testing.T) {
		var logged bytes.Buffer
		cov, f := shapeCoverageReader(t, func(*http.Request) map[string]any {
			return map[string]any{"identity": nil, "place": nil, "model": nil}
		}, &logged)
		coverage, err := cov.readCoverage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if coverage.Identity != nil || coverage.Place != nil || coverage.Model != nil {
			t.Fatalf("the report names identity %v, place %v, model %v; want none of the three", coverage.Identity, coverage.Place, coverage.Model)
		}
		if err := f.waitEmbedded(context.Background()); err == nil || !strings.Contains(err.Error(), "the deployment has no embedder") {
			t.Fatalf("the gate ended %v, want the refusal of a deployment that embeds nothing", err)
		}
	})
}
