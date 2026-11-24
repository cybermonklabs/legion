package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEdgeRepositoryReadPrintsNoLocationOfARedirect: whatever answers at the broker's address
// answers the edge's read of a project's repositories, which carries the listener's own bearer,
// with a redirect whose Location quotes that bearer. One net/http cannot parse (unparsed) is
// net/http's error whatever the client's redirect policy, since Go parses a Location before it
// asks CheckRedirect; one that parses names an address on another host nothing listens at
// (elsewhere), whose failure net/http names by the Location when it follows it. The read's error,
// which the refresh logs, holds no run of the bearer and no word of the Location. The twin shows
// the stand-in's echo reaches net/http's own error. Captures listener.edge-redirect/<case>.
func TestAnEdgeRepositoryReadPrintsNoLocationOfARedirect(t *testing.T) {
	account := githubtest.InstallationToken(t.Name()) + "-RDX-"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	echo := func(location func(bearer string) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	unparsed := func(bearer string) string { return "/moved%zz" + bearer }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, echo(unparsed)+edgeRepositoriesPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+account)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), account) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", unparsed},
		{"elsewhere", func(bearer string) string { return "http://" + dead + "/moved?access_token=" + bearer }},
	} {
		t.Run(c.name, func(t *testing.T) {
			reader := newEdgeRepositories(echo(c.location), listenerBrokerAccount(account), []string{"PRIV"},
				newEdgeWithheldCounter(metrics.New()), logging.New("test"))
			got, err := reader.read(t.Context(), "PRIV")
			if err == nil || got != nil || !errors.Is(err, errRepositoriesUnknown) {
				t.Fatalf("read = %q, %v; want no list and errRepositoriesUnknown", got, err)
			}
			redacttest.RequireNoPiece(t, "listener.edge-redirect/"+c.name, err.Error(), account)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("read's error names the Location: %q", err)
			}
		})
	}
}
