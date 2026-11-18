package githubrest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A proxy in GitHub's place can answer with a page of its own that echoes the request back, the
// token included. That body is not GitHub's error JSON, so the Answer holds it, and its Error
// prints it, as one line with the token and any Authorization or Bearer value redacted, while
// GitHub's own error JSON is held as GitHub sent it, for the callers that decide by its message.
func TestAnAnswerHoldsAProxysPageWithoutTheToken(t *testing.T) {
	const token = "ghs_Q7vK2mXr9LtW4nBz8YcH1jPe"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/own" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Reference already exists","status":"422"}`+"\n")
			return
		}
		authorization := r.Header.Get("Authorization")
		_, sent, _ := strings.Cut(authorization, " ")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "<html><body><h1>502 Bad Gateway</h1><pre>Authorization: %s\nseen=%s</pre></body></html>", authorization, sent)
	}))
	defer server.Close()
	client := Client{API: server.URL + "/repos/acme/widgets", Token: token}

	var answer *Answer
	if err := client.Get(context.Background(), "/pulls", nil); !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want an *Answer", err)
	}
	page := `a body that is not GitHub's error JSON ("502 Bad Gateway Authorization: Bearer [redacted] seen=[redacted]")`
	if answer.Status != http.StatusBadGateway || answer.Body != page {
		t.Fatalf("Answer = %d %q, want 502 and the page %q", answer.Status, answer.Body, page)
	}
	if got, want := answer.Error(), "GitHub answered GET /pulls with 502: "+page; got != want || strings.Contains(got, token) {
		t.Fatalf("Error = %q, want %q", got, want)
	}

	var own *Answer
	if err := client.Get(context.Background(), "/own", nil); !errors.As(err, &own) {
		t.Fatalf("Get = %v, want an *Answer", err)
	}
	if want := `{"message":"Reference already exists","status":"422"}`; own.Body != want {
		t.Fatalf("GitHub's own answer is held as %q, want it as GitHub sent it, %q", own.Body, want)
	}
}
