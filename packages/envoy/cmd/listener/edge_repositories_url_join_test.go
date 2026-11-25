package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEdgeRepositoryReadGoesToTheRouteJoinedOntoTheBrokerURL: the broker URL the edge listener is
// given can carry a path and a query. A read of a project's repositories asks edgeRepositoriesPath
// joined onto the URL's path, the URL's query kept and the read's own project=<KEY> after it:
// http://host:port/broker?key=... is asked /broker/v1/project-repositories?key=...&project=PRIV
// (query), and so is http://host:port/broker/?key=... (query-trailing-slash), where the route
// appended to the URL's text sat inside the query (/broker?key=.../v1/project-repositories?project=PRIV:
// the broker was asked /broker with the query's value changed and no project of its own). A URL
// with no query is asked where it was, with a trailing slash (trailing-slash) or without
// (no-query). A stand-in records the path and the query the read reaches it with, and answers the
// project's list, so the read keeps it.
func TestAnEdgeRepositoryReadGoesToTheRouteJoinedOntoTheBrokerURL(t *testing.T) {
	const route = "/broker" + edgeRepositoriesPath
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/broker?key=k3y-7Qz4k", "key=k3y-7Qz4k&project=PRIV"},
		{"query-trailing-slash", "/broker/?key=k3y-7Qz4k", "key=k3y-7Qz4k&project=PRIV"},
		{"trailing-slash", "/broker/", "project=PRIV"},
		{"no-query", "/broker", "project=PRIV"},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"project":"PRIV","repositories":["example-owner/example-repo"]}`))
			}))
			t.Cleanup(broker.Close)
			reader := newEdgeRepositories(broker.URL+c.suffix, listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
				newEdgeWithheldCounter(metrics.New()), logging.New("test"))
			got, err := reader.read(t.Context(), "PRIV")
			select {
			case request := <-asked:
				if request.Path != route || request.RawQuery != c.query {
					t.Errorf("a broker URL ending %q was asked at path %q, query %q; want path %q, query %q", c.suffix, request.Path, request.RawQuery, route, c.query)
				}
			default:
				t.Fatalf("no read reached the broker; read answered %v", err)
			}
			if err != nil || !slices.Equal(got, []string{"example-owner/example-repo"}) {
				t.Errorf("read = %q, %v; want the list the stand-in answered", got, err)
			}
		})
	}
}

// TestAnEdgeRepositoryReadAtABrokerURLThatDoesNotParsePrintsNoneOfIt: a broker URL url.Parse refuses
// (an invalid escape in its path), with redacttest's credentials in its user information and its
// query. The read is errRepositoriesUnknown, and its error, which the refresh logs, names the URL
// by its class, never url.Parse's error, which quotes it whole (unparsed); the twin shows
// url.Parse's error holds the credentials, so the row can see them printed. Captures
// listener.edge-url/unparsed.
func TestAnEdgeRepositoryReadAtABrokerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse(%q) = %v; want an error that quotes the credentials, so the row can see them", unparsed, err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		reader := newEdgeRepositories(unparsed, listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
			newEdgeWithheldCounter(metrics.New()), logging.New("test"))
		got, err := reader.read(t.Context(), "PRIV")
		if err == nil || got != nil || !errors.Is(err, errRepositoriesUnknown) {
			t.Fatalf("read = %q, %v; want no list and errRepositoriesUnknown", got, err)
		}
		redacttest.RequireNoPiece(t, "listener.edge-url/unparsed", err.Error())
		if !strings.HasSuffix(err.Error(), ": not a valid URL") {
			t.Errorf("read = %q; want the broker URL refused by its class", err)
		}
	})
}
