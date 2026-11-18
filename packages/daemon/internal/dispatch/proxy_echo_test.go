package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body that is not Dispatch's error JSON was written by something in Dispatch's place, such as a
// proxy's page, which can echo the request back, its bearer included. The refusal keeps its status
// and its empty code, and names that body in one line with the bearer and any Authorization or
// Bearer value redacted (excerpt.BodyForText), after the status line.
func TestARefusalNamesAProxysPageWithoutTheBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		_, bearer, _ := strings.Cut(authorization, " ")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "<html><body><h1>502 Bad Gateway</h1><pre>Authorization: %s\nseen=%s</pre></body></html>", authorization, bearer)
	}))
	defer server.Close()

	_, err := New(server.URL, dispatchToken).GetIssue(context.Background(), "LEGION-12")
	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("get issue error = %T %v, want *dispatch.Error", err, err)
	}
	want := `502 Bad Gateway with a body that is not Dispatch's error JSON ("502 Bad Gateway Authorization: Bearer [redacted] seen=[redacted]")`
	if refusal.Status != http.StatusBadGateway || refusal.Code != "" || refusal.Message != want {
		t.Fatalf("Dispatch error = %#v, want 502, no code and the message %q", refusal, want)
	}
	if strings.Contains(err.Error(), dispatchToken) {
		t.Fatalf("the error prints the bearer: %q", err)
	}
}

// answering is a transport that answers every request with what answer builds, for an answer no
// real server writes, such as a reason phrase that names the bearer.
type answering func(*http.Request) *http.Response

func (a answering) RoundTrip(request *http.Request) (*http.Response, error) { return a(request), nil }

// The reason phrase is text the answerer chose too, so it is named through the same excerpt.
func TestARefusalNamesAReasonPhraseWithoutTheBearer(t *testing.T) {
	client := New("http://dispatch.test", dispatchToken)
	client.httpClient = &http.Client{Transport: answering(func(request *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusBadGateway, Status: "502 Denied for " + dispatchToken,
			Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"message":"upstream"}`)),
			Request: request,
		}
	})}

	_, err := client.GetIssue(context.Background(), "LEGION-13")
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusBadGateway || refusal.Message != "502 Denied for [redacted]" {
		t.Fatalf("get issue error = %#v, want 502 with the reason phrase redacted", err)
	}
}
