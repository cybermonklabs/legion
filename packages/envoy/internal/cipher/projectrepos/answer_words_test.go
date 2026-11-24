package projectrepos_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARefusedRepositoryAnswerNamesDispatchsWordsAsTheRuleReadsThem: Dispatch, or whatever answers
// in its place, answers the repositories of a project with words that echo the broker's account
// token the read carried: as a repository that is no owner/name, as a key that is no member of the
// answer, as the project it answers about, and as a refusal's code. Each refusal names those words
// as the rule reads them, the token redacted, and still names their place. Captures
// projectrepos.answer-words/<case>.
func TestARefusedRepositoryAnswerNamesDispatchsWordsAsTheRuleReadsThem(t *testing.T) {
	account := githubtest.InstallationToken(t.Name())
	for _, c := range []struct {
		name   string
		status int
		body   func(echoed string) string
		want   string
	}{
		{"repository", http.StatusOK, func(echoed string) string {
			return `{"project":"PRIV","repositories":["token ` + echoed + `"]}`
		}, `lists "token [redacted]", which is no owner/name`},
		{"key", http.StatusOK, func(echoed string) string {
			return `{"project":"PRIV","repositories":[],"` + echoed + `":1}`
		}, `holds the key "[redacted]"`},
		{"project", http.StatusOK, func(echoed string) string {
			return `{"project":"` + echoed + `","repositories":[]}`
		}, `answered about "[redacted]"`},
		{"code", http.StatusServiceUnavailable, func(echoed string) string {
			return `{"code":"token ` + echoed + ` refused","error":"unavailable"}`
		}, `with the code "token [redacted] refused"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				echoed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body(echoed))
			}))
			t.Cleanup(server.Close)
			got, err := projectrepos.New(server.URL, projectaccesstest.StaticAccount(account), nil).Repositories(t.Context(), "PRIV")
			// Control: the refusal is the one that names the echoed words' place.
			if err == nil || got != nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Repositories = %q, %v; want no list and an error with %s in it", got, err, c.want)
			}
			redacttest.RequireNoPiece(t, "projectrepos.answer-words/"+c.name, err.Error(), account)
		})
	}
}
