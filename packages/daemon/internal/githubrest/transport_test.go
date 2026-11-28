package githubrest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// answeringTransport answers every request itself, counting them, so a test sees which transport
// a call went through; with err set it answers none, as a transport that reached no server.
type answeringTransport struct {
	calls atomic.Int32
	err   error
}

func (a *answeringTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	a.calls.Add(1)
	if a.err != nil {
		return nil, a.err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
}

// A Client call goes through the transport SetTransport set, which HTTPClient carries, and
// SetTransport(nil) gives every call back to http.DefaultClient, which is all a process that never
// calls it uses.
func TestSetTransportCarriesEveryClientCallUntilItIsSetBack(t *testing.T) {
	t.Cleanup(func() { SetTransport(nil) })
	if HTTPClient() != http.DefaultClient {
		t.Fatalf("HTTPClient before any SetTransport = %v, want http.DefaultClient", HTTPClient())
	}
	through := &answeringTransport{}
	SetTransport(through)
	if HTTPClient().Transport != through {
		t.Fatalf("HTTPClient's transport = %v, want the one SetTransport set", HTTPClient().Transport)
	}
	// No server listens at github.invalid: only the set transport can answer this call.
	if err := (Client{Token: "token", API: "http://github.invalid/repos/acme/widgets"}).Get(context.Background(), "", nil); err != nil {
		t.Fatalf("a call through the set transport: %v", err)
	}
	if calls := through.calls.Load(); calls != 1 {
		t.Fatalf("the set transport carried %d calls, want the one", calls)
	}

	SetTransport(nil)
	if HTTPClient() != http.DefaultClient {
		t.Fatalf("HTTPClient after SetTransport(nil) = %v, want http.DefaultClient", HTTPClient())
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer server.Close()
	if err := (Client{Token: "token", API: server.URL}).Get(context.Background(), "/repos/acme/widgets", nil); err != nil {
		t.Fatalf("a call after SetTransport(nil): %v", err)
	}
	if calls := through.calls.Load(); calls != 1 {
		t.Fatalf("the transport set before SetTransport(nil) carried %d calls, want still 1", calls)
	}
}

// GetHeaders returns the headers of an answer other than 2xx beside the *Answer, since a refusal
// names the rate limit too, and no headers when no answer arrived.
func TestGetHeadersReturnsARefusalsHeadersAndNoneWithoutAnAnswer(t *testing.T) {
	t.Cleanup(func() { SetTransport(nil) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("X-RateLimit-Used", "5000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	headers, err := client.GetHeaders(context.Background(), "/actions/workflows?per_page=1", nil)
	var refused *Answer
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("GetHeaders of a 403 answered %v, want the *Answer naming 403", err)
	}
	if used := headers.Get("X-RateLimit-Used"); used != "5000" {
		t.Fatalf("GetHeaders of a 403 returned X-RateLimit-Used %q, want the answer's 5000", used)
	}

	cut := errors.New("the connection was refused")
	SetTransport(&answeringTransport{err: cut})
	headers, err = client.GetHeaders(context.Background(), "/actions/workflows?per_page=1", nil)
	if !errors.Is(err, cut) {
		t.Fatalf("GetHeaders with no answer = %v, want the transport's error", err)
	}
	if headers != nil {
		t.Fatalf("GetHeaders with no answer returned headers %v, want none", headers)
	}
}

// APIRoot is the REST root a call that names no repository goes to: GitHub's own for an endpoint
// that is empty once its trailing slashes are trimmed, and the endpoint given with no trailing
// slash, so a path under it never doubles one. appauth's token manager takes its base from it too.
func TestAPIRootIsGitHubsRootOrTheEndpointTrimmed(t *testing.T) {
	for endpoint, want := range map[string]string{
		"":                           "https://api.github.com",
		"/":                          "https://api.github.com",
		"http://github.example":      "http://github.example",
		"http://github.example/":     "http://github.example",
		"http://github.example/api/": "http://github.example/api",
	} {
		if got := APIRoot(endpoint); got != want {
			t.Errorf("APIRoot(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
