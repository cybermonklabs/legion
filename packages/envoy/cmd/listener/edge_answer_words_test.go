package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEdgeRepositoryReadNamesTheBrokersWordsAsTheRuleReadsThem: the broker, or whatever answers
// in its place, answers the edge's read of a project's repositories with words that echo the
// bearer the read carried: as a refusal's body, as the project it answers about, and as a
// repository that is no owner/name. The read's error, which the refresh logs, names those words
// as the rule reads them, the bearer redacted, and still says what was wrong. Captures
// listener.edge-repositories-answer-words/<case>.
func TestAnEdgeRepositoryReadNamesTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	account := githubtest.InstallationToken(t.Name())
	for _, c := range []struct {
		name   string
		status int
		body   func(echoed string) string
		want   []string
	}{
		{"refusal", http.StatusServiceUnavailable, func(echoed string) string {
			return `{"code":"UNAVAILABLE","error":"token ` + echoed + ` refused"}`
		}, []string{"the broker answered 503: ", "token " + redact.Redacted + " refused"}},
		{"project", http.StatusOK, func(echoed string) string {
			return `{"project":"token ` + echoed + `","repositories":[]}`
		}, []string{`answered for project "token ` + redact.Redacted + `", not for PRIV`}},
		{"repository", http.StatusOK, func(echoed string) string {
			return `{"project":"PRIV","repositories":["token ` + echoed + `"]}`
		}, []string{`names "token ` + redact.Redacted + `", which is no owner/name`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body(echoed))
			}))
			t.Cleanup(server.Close)
			reader := newEdgeRepositories(server.URL, listenerBrokerAccount(account), []string{"PRIV"},
				newEdgeWithheldCounter(metrics.New()), logging.New("test"))
			got, err := reader.read(t.Context(), "PRIV")
			if err == nil || got != nil || !errors.Is(err, errRepositoriesUnknown) {
				t.Fatalf("read = %q, %v; want no list and errRepositoriesUnknown", got, err)
			}
			redacttest.RequireNoPiece(t, "listener.edge-repositories-answer-words/"+c.name, err.Error(), account)
			// Control: the error still names what was wrong, and where.
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("read's error is %q, want %q in it", err, want)
				}
			}
		})
	}
}
