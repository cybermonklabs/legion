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

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode: the API port answers a refusal the broker
// made because it could not learn a person's member kind 503 under projectaccess.UncheckableCode,
// the code the airlock proxy answers the same refusal with, in the refusal's fixed words and
// never the failure's, which can name Dispatch's address. An error that is no such refusal is left
// to the caller's next case.
func TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode(t *testing.T) {
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
// fixed words, but it is logged below ERROR: it says nothing of Dispatch. A failure to ask
// Dispatch is logged at ERROR, as before.
func TestACallerThatEndedIsNoDispatchFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	for _, row := range []struct {
		failure error
		level   slog.Level
	}{
		{fmt.Errorf("projectaccess: ask dispatch: %w", context.Canceled), slog.LevelInfo},
		{errors.New("dial tcp 10.20.0.9:8766: connect: connection refused"), slog.LevelError},
	} {
		uncheckable := &projectaccess.MemberKindUncheckable{Person: "uma@example.com", Err: row.failure}
		before := len(capture.Records())
		w := httptest.NewRecorder()
		if !writeMemberKindUncheckable(w, "the release", uncheckable) || w.Code != http.StatusServiceUnavailable {
			t.Fatalf("the refusal for %v answered %d, want 503", row.failure, w.Code)
		}
		records := capture.Records()[before:]
		if len(records) != 1 || records[0].Level != row.level {
			t.Fatalf("the refusal for %v logged %d records, the first at %v; want one at %v", row.failure, len(records), levels(records), row.level)
		}
	}
}

// levels is each record's level, for a failure message.
func levels(records []slog.Record) []slog.Level {
	var out []slog.Level
	for _, record := range records {
		out = append(out, record.Level)
	}
	return out
}
