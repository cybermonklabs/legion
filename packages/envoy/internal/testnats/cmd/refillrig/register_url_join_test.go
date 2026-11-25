package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestARegistrationJoinsTheRouteOntoTheListenerURL: --url with a path and a query (path and
// query): each registration posts to /v1/interests/subscribe joined onto that path with the query
// kept (urljoin.Join), and the line register prints names that URL. Where register wrote the
// route after --url's text, the route went inside the query and the stand-in listener was asked
// /prefix. A plain origin (origin) is asked the route, as before.
func TestARegistrationJoinsTheRouteOntoTheListenerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, asked string }{
		{"path and query", "/prefix?key=k3y-made-up", "/prefix/v1/interests/subscribe?key=k3y-made-up"},
		{"origin", "", "/v1/interests/subscribe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
			}))
			t.Cleanup(listener.Close)
			given := listener.URL + c.suffix
			printed, err := stdoutOf(t, func() error {
				return runRegister([]string{"--url", given, "--sets", "joined=2", "--concurrency", "1"})
			})
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"POST " + c.asked, "POST " + c.asked}; err != nil || !slices.Equal(asked, want) {
				t.Errorf("register --url %s = %v and asked %q; want %q", given, err, asked, want)
			}
			if want := "registered 2 sessions through " + listener.URL + c.asked + " in "; !strings.HasPrefix(printed, want) {
				t.Errorf("register --url %s printed %q; want a line beginning %q", given, printed, want)
			}
		})
	}
}

// TestARegistrationURLWithNoHostIsRefusedQuotingNoneOfIt: a --url that is no absolute URL with a
// host is refused as urljoin.ErrBase before any registration, in words that quote none of it,
// since the URL can carry a credential. Where register wrote the route after --url's text,
// net/http's error for it quoted the URL whole.
func TestARegistrationURLWithNoHostIsRefusedQuotingNoneOfIt(t *testing.T) {
	err := runRegister([]string{"--url", "127.0.0.1:9?key=k3y-made-up", "--sets", "joined=1", "--concurrency", "1"})
	if !errors.Is(err, urljoin.ErrBase) || strings.Contains(err.Error(), "k3y-made-up") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("register of a --url with no scheme = %v; want urljoin.ErrBase, quoting none of the URL", err)
	}
}

// stdoutOf runs run with os.Stdout read into the text it answers.
func stdoutOf(t *testing.T, run func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("a pipe for stdout: %v", err)
	}
	defer read.Close()
	stdout := os.Stdout
	os.Stdout = write
	runErr := run()
	os.Stdout = stdout
	if err := write.Close(); err != nil {
		t.Fatalf("close the pipe's write end: %v", err)
	}
	printed, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("read the pipe: %v", err)
	}
	return string(printed), runErr
}
