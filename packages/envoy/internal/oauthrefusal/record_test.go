package oauthrefusal_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
)

// TestRecorderKeepsTheBodyOfAnAnswerThatIsNoSuccessAlone pins what a Recorder keeps of an answer's
// body: a 2xx answer's body - a token answer, which holds a person's tokens - is not kept, so no
// copy of it stays in memory, while every other answer's body is kept for a reader that prints its
// registered code (internal/redact, Answered). Whatever it keeps, it records the status, and a body
// read whole leaves ReadErr nil.
func TestRecorderKeepsTheBodyOfAnAnswerThatIsNoSuccessAlone(t *testing.T) {
	const body = `{"access_token":"a-token","error":"invalid_grant"}`
	for _, c := range []struct {
		status int
		kept   bool
	}{
		{http.StatusOK, false},
		{http.StatusCreated, false},
		{http.StatusFound, true},
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusServiceUnavailable, true},
	} {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			answered := &oauthrefusal.Recorder{}
			client := oauthrefusal.Record(context.Background(), answered).Value(oauth2.HTTPClient).(*http.Client)
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			read, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(read) != body {
				t.Fatalf("the caller read %q, %v; want the whole body", read, err)
			}
			if answered.Status != c.status || answered.ReadErr() != nil {
				t.Fatalf("Status = %d, ReadErr = %v; want %d and nil", answered.Status, answered.ReadErr(), c.status)
			}
			want := ""
			if c.kept {
				want = body
			}
			if got := string(answered.Answer()); got != want {
				t.Fatalf("Answer after a %d = %q, want %q", c.status, got, want)
			}
		})
	}
}
