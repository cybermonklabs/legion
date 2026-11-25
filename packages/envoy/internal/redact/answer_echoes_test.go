package redact_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoesAnswer is an answer to a read of a plan's blocks that carried the bearer and the query
// value `true` (?blocks=true), the two kinds of value the answer's record holds.
func echoesAnswer(t *testing.T) redact.Answer {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://dispatch.example/api/v1/artifacts/plan-1/versions/3?blocks=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+redacttest.Token)
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// handedOn is what a caller that hands on a word Echoes passes, as the plan route does with a
// block's id, would hand on: the word, or a refusal of it.
func handedOn(answer redact.Answer, word string) string {
	if redact.Echoes(answer, word) {
		return "refused: the word holds a run of a credential the read carried"
	}
	return word
}

// TestEchoesFindsARunOfACredentialTheReadCarried: a word that holds a run of TokenPiece characters
// of the bearer the read carried, whole or cut, echoes it, and a caller that hands on only words
// Echoes passes hands on none of the bearer (captures redact.echoes/bearer); the twin shows the
// word itself holds the bearer. A word equal to the read's short query value `true` is no echo:
// the value is shorter than TokenPiece, so no run of it is one (the short-query-value subtest).
// The zero Answer carries nothing, so nothing echoes it.
func TestEchoesFindsARunOfACredentialTheReadCarried(t *testing.T) {
	answer := echoesAnswer(t)
	t.Run("bearer", func(t *testing.T) {
		for _, word := range []string{redacttest.Token, "task-" + redacttest.Token[3:15]} {
			if redacttest.Piece(word, redacttest.Token) == "" {
				t.Fatalf("twin: the word %q holds no run of the bearer, so this row proves nothing", word)
			}
			redacttest.RequireNoPiece(t, "redact.echoes/bearer", handedOn(answer, word), redacttest.Token)
		}
	})
	t.Run("short-query-value", func(t *testing.T) {
		for _, word := range []string{"true", "plan", "versions"} {
			if redact.Echoes(answer, word) {
				t.Errorf("Echoes(%q) = true, want false: no run of TokenPiece characters of a credential the read carried is in it", word)
			}
		}
	})
	if redact.Echoes(redact.Answer{}, redacttest.Token) {
		t.Error("the zero Answer's Echoes = true, want false: it carried nothing")
	}
}
