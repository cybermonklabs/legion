package oauthrefusal_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
)

// countingTransport sends each request through http.DefaultTransport and counts it: how many
// requests the client Record was given carried.
type countingTransport struct{ sent int }

func (t *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.sent++
	return http.DefaultTransport.RoundTrip(request)
}

// refusingTransport is the transport of a client no request may go through: it fails the test
// that sends one.
type refusingTransport struct{ t *testing.T }

func (x refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	x.t.Error("the request went through the client the context carries; Record sends through the client it is given")
	return nil, errors.New("this client sends nothing")
}

// TestRecordSendsThroughTheClientItIsGiven pins whose client a recorded call goes out on: the one
// its caller resolved and handed Record, never one the context carries, which the caller has
// already read where it reads one (cipherclient.clientOf). The client's own rules travel with it -
// here a redirect rule that answers a 302 as it stands - so one record layer carries the call: the
// Recorder reads the redirect's own status and the body it came with, and the target of the
// Location is sent nothing.
func TestRecordSendsThroughTheClientItIsGiven(t *testing.T) {
	const body = `{"error":"invalid_grant"}`
	moved := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			moved++
			return
		}
		w.Header().Set("Location", "/moved")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	sent := &countingTransport{}
	given := &http.Client{Transport: sent, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	carried := &http.Client{Transport: refusingTransport{t}}
	answered := &oauthrefusal.Recorder{}
	ctx := oauthrefusal.Record(context.WithValue(context.Background(), oauth2.HTTPClient, carried), given, answered)
	client, isClient := ctx.Value(oauth2.HTTPClient).(*http.Client)
	if !isClient {
		t.Fatalf("the context Record answered carries %T under oauth2.HTTPClient, want an *http.Client", ctx.Value(oauth2.HTTPClient))
	}
	response, err := client.Get(server.URL + "/token")
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(read) != body {
		t.Fatalf("the caller read %q, %v; want the whole body", read, err)
	}
	if sent.sent != 1 || moved != 0 {
		t.Fatalf("the client Record was given carried %d requests and the Location was asked %d times; want 1 and 0", sent.sent, moved)
	}
	if answered.Status != http.StatusFound || answered.Host != response.Request.URL.Host || string(answered.Answer()) != body {
		t.Fatalf("the recorder read (%d, %q, %q); want (%d, %q, %q)",
			answered.Status, answered.Host, answered.Answer(), http.StatusFound, response.Request.URL.Host, body)
	}
}
