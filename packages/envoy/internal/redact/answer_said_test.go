package redact_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestSaidHoldsTextReadFromTheAnswerWithItsRecord: a caller reads a value from an answer's bytes
// by its own grammar and hands it to Said. The value echoes the bearer the request carried: it
// prints as the rule reads it, the bearer redacted, and compares as it came. The zero Answer's
// Said has no record. Captures redact.answer-said.
func TestSaidHoldsTextReadFromTheAnswerWithItsRecord(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://dispatch.example/api/v1/projects/PRIV", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+redacttest.Token)
	const value = "token " + redacttest.Token + " refused"
	body := `{"project":"` + value + `"}`
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	said := answer.Said(value)
	if !said.Equal(value) {
		t.Errorf("Said(%q) does not compare as it came", value)
	}
	if got := said.String(); !strings.Contains(got, "token [redacted] refused") {
		t.Errorf("Said(%q) prints %q, want the bearer redacted and the rest as it came", value, got)
	}
	redacttest.RequireNone(t, "redact.answer-said", said.String())
	if got := (redact.Answer{}).Said("plain words").String(); got != "plain words" {
		t.Errorf("the zero Answer's Said prints %q, want the words as they came", got)
	}
}
