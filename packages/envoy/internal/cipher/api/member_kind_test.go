package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode: the API port answers a refusal the broker
// made because it could not learn a person's member kind 503 under projectaccess.UncheckableCode,
// the code the airlock proxy answers the same refusal with, in the refusal's fixed words and
// never the failure's, which can name Dispatch's address. An error that is no such refusal is left
// to the caller's next case.
func TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode(t *testing.T) {
	t.Parallel()
	failure := errors.New("dial tcp 10.20.0.9:8766: connect: connection refused")
	uncheckable := &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: failure}
	w := httptest.NewRecorder()
	if !writeMemberKindUncheckable(w, "the release", fmt.Errorf("read grant: %w", uncheckable)) {
		t.Fatal("writeMemberKindUncheckable did not answer a wrapped MemberKindUncheckable")
	}
	var body struct{ Code, Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer %q is no refusal body: %v", w.Body.String(), err)
	}
	if w.Code != http.StatusServiceUnavailable || body.Code != projectaccess.UncheckableCode || body.Error != uncheckable.Error() {
		t.Fatalf("the answer is %d %+v; want 503 %s %q", w.Code, body, projectaccess.UncheckableCode, uncheckable.Error())
	}
	if strings.Contains(w.Body.String(), "10.20.0.9") {
		t.Fatalf("the answer %q names the failure", w.Body.String())
	}
	if writeMemberKindUncheckable(httptest.NewRecorder(), "the release", failure) {
		t.Fatal("writeMemberKindUncheckable answered an error that is no MemberKindUncheckable")
	}
}

// TestACallerThatEndedIsNoDispatchFailure: a member-kind read that failed because its caller gave
// up (context.Canceled, a client that closed its connection) is still refused 503 in the same
// fixed words, but it writes no line and is counted cancelled, as every route counts a caller that
// hung up (answer.Recorder.NoteCallerEnded): it says nothing of Dispatch. A failure to ask
// Dispatch is logged at ERROR and counted failed, as before.
func TestACallerThatEndedIsNoDispatchFailure(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, row := range []struct {
		name    string
		failure error
		outcome string
		errors  int
	}{
		{"the caller hung up", fmt.Errorf("projectaccess: ask dispatch: %w", context.Canceled), "cancelled", 0},
		{"Dispatch did not answer", errors.New("dial tcp 10.20.0.9:8766: connect: connection refused"), "failed", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			registry := metrics.New()
			handler := answer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeMemberKindUncheckable(w, "the release", &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: row.failure})
			}), answer.NewAnswers(registry))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/grants/g/values", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("the refusal for %v answered %d, want 503", row.failure, w.Code)
			}
			var errorLines int
			for _, rec := range capture.Records() {
				if rec.Level >= slog.LevelError {
					errorLines++
				}
			}
			if errorLines != row.errors {
				t.Errorf("the refusal for %v wrote %d ERROR lines, want %d", row.failure, errorLines, row.errors)
			}
			scrape := httptest.NewRecorder()
			registry.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if want := `broker_answers_total{outcome="` + row.outcome + `",code="HTTP_503"`; !strings.Contains(scrape.Body.String(), want) {
				t.Errorf("the refusal for %v was not counted %s:\n%s", row.failure, row.outcome, scrape.Body.String())
			}
		})
	}
}
