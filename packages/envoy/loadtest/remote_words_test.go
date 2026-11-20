package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASeedErrorPrintsDispatchsCodeAsTheRuleReadsIt: Dispatch refuses a seed step with a code that
// quotes a URL carrying a credential. The seedError made from its body prints the step, the status
// and the code as the rule for remote text reads it, under every verb and in a value that holds
// it, and none of the credentials.
func TestASeedErrorPrintsDispatchsCodeAsTheRuleReadsIt(t *testing.T) {
	for _, row := range []struct{ name, code, shown string }{
		{"user information and a query value", redacttest.Words, redacttest.Shown},
		{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
		{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
	} {
		t.Run(row.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"code": row.code, "error": "refused"})
			if err != nil {
				t.Fatal(err)
			}
			refused := newSeedError("issue-read", "DSP-1", 400, errorCode(body))
			redacttest.RequireNoneFormatted(t, "loadtest.seedError", refused)
			if want := "issue-read DSP-1: status 400 code " + strconv.Quote(row.shown); refused.Error() != want {
				t.Errorf("seedError = %q, want %q", refused, want)
			}
		})
	}
}

// TestARefusedQuoteLogsDispatchsCodeAsTheRuleReadsIt: Dispatch refuses every anchor the seed tries
// for a comment, with a code that quotes a URL carrying a credential. The seed's log line for each
// refused quote names the code as the rule reads it, and none of the credentials.
func TestARefusedQuoteLogsDispatchsCodeAsTheRuleReadsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": redacttest.Code, "error": "the anchor does not match"})
	}))
	t.Cleanup(srv.Close)
	var logged bytes.Buffer
	s := &replaySeeder{api: newAPI(srv.URL, "t", 1), lg: log.New(&logged, "", 0), counts: map[string]int{}, marks: map[string]int{}, b: &replayBundle{}}
	d := &issueDoc{a: &replayArtifact{ID: "d1", Kind: "doc"}, local: "local-d1", text: "Intro paragraph.\n\nA sentence the comment quotes.\n"}
	_, err := s.anchored(context.Background(), "comment", "PRJ-1", d, "c1", &replayAnchor{ArtifactID: "d1", Quote: "A sentence the comment quotes."}, who{}, "/api/v1/issues/PRJ-1/comments", map[string]any{"body": "x"})
	if err == nil {
		t.Fatal("anchored answered no error for a refused anchor")
	}
	redacttest.RequireNone(t, "loadtest.quote-refused-log", logged.String())
	if want := "quote refused"; !strings.Contains(logged.String(), want) || !strings.Contains(logged.String(), strconv.Quote(redacttest.CodeShown)) {
		t.Errorf("the seed logged %q, want a %q line naming the code as the rule reads it", logged.String(), want)
	}
}

// TestTheReportsPrintDispatchsCodeAsTheRuleReadsIt: a code that quotes a credential's URL reaches
// each report the load test writes - a route's codes, a loop class's codes, a loop refusal's code -
// only as the rule reads it, and a code is still compared as it came.
func TestTheReportsPrintDispatchsCodeAsTheRuleReadsIt(t *testing.T) {
	body, err := json.Marshal(map[string]string{"code": redacttest.Words, "error": "refused"})
	if err != nil {
		t.Fatal(err)
	}
	res := callResult{status: 400, body: body}
	if !res.errorCode().Equal(redacttest.Words) || !errorCode([]byte(`{"code":"KEY_IN_FLIGHT"}`)).Equal("KEY_IN_FLIGHT") {
		t.Fatal("a code is no longer compared as it came")
	}
	routes := summarizeRequests([]reqRec{{route: "r", status: 400, code: res.errorCode()}}, 0, 0, 1)
	w := &writer{reqs: map[string]*loopRequests{}, lat: map[string][]float64{}}
	w.record("c", res)
	reports := map[string]string{"a route's codes": fmtJSON(t, routes.Routes["r"].Codes), "a loop class's codes": fmtJSON(t, w.reqs["c"].Codes), "a loop refusal's code": loopAnswerCode(res)}
	for report, text := range reports {
		redacttest.RequireNone(t, report, text)
	}
	if _, ok := routes.Routes["r"].Codes["400 "+redacttest.Shown]; !ok || loopAnswerCode(res) != redacttest.Shown {
		t.Errorf("the reports name the code as %v and %q, want %q", routes.Routes["r"].Codes, loopAnswerCode(res), redacttest.Shown)
	}
}

func fmtJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
