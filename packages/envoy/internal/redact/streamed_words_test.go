package redact_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestStreamedWordsHoldsTextWithItsAnswersRecord: a client reads a name out of an answer it
// streams as data (a tar entry's) and holds it with StreamedWords. The name echoes the bearer the
// request carried: it prints as the rule reads it, the bearer redacted, under %s and %q alike, and
// compares as it came. An answer with no request has no record. Captures redact.streamed-words.
func TestStreamedWordsHoldsTextWithItsAnswersRecord(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://cache.example/tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+redacttest.Token)
	const name = "refs/heads/" + redacttest.Token
	words := redact.StreamedWords(&http.Response{StatusCode: http.StatusOK, Request: request}, name)
	if !words.Equal(name) {
		t.Errorf("StreamedWords(%q) does not compare as it came", name)
	}
	if got, want := fmt.Sprintf("%s", words), "refs/heads/"+redact.Redacted; got != want {
		t.Errorf("StreamedWords(%q) prints %q under %%s, want %q", name, got, want)
	}
	if got, want := fmt.Sprintf("%q", words), `"refs/heads/`+redact.Redacted+`"`; got != want {
		t.Errorf("StreamedWords(%q) prints %s under %%q, want %s", name, got, want)
	}
	redacttest.RequireNone(t, "redact.streamed-words", fmt.Sprintf("%s %q %v", words, words, words))
	if got := redact.StreamedWords(&http.Response{StatusCode: http.StatusOK}, "plain words").String(); got != "plain words" {
		t.Errorf("StreamedWords of an answer with no request prints %q, want the words as they came", got)
	}
}
