package delivery

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnInstallationLoginPrintsOnlyAsALogin: GitHub, or whatever answers in its place, lists the
// App's one installation under an account login, and that installation's search fails before it
// reads a page: its window start cannot be read (window-start), or its repositories cannot be
// listed (repositories). The search's error names the installation by its id and by its login
// where the login is one GitHub allows (login), and otherwise by a phrase that names the field,
// so a login that quotes a credential's URL prints none of it. Captures
// delivery.installation-login/<case>.
func TestAnInstallationLoginPrintsOnlyAsALogin(t *testing.T) {
	for _, c := range []struct {
		name, login, want string
		sinceFails        bool
	}{
		{"window-start", redacttest.Words, "installation 1 of (the installation login is not a login): ", true},
		{"repositories", redacttest.Words, "installation 1 of (the installation login is not a login): ", false},
		{"login", "acme-bots", "installation 1 of acme-bots: ", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
				mustEncode(t, w, []map[string]any{{
					"id": 1, "app_slug": "delivery-test", "account": map[string]any{"login": c.login},
					"permissions": map[string]string{"contents": "read", "pull_requests": "read"},
				}})
			})
			mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				mustEncode(t, w, map[string]any{"token": "ghs_installation_token_for_the_search", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
			})
			mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				mustEncode(t, w, map[string]any{"message": "Server Error"})
			})
			client := newAppClient(t, githubtest.Spec{Handle: mux.ServeHTTP})
			start, end := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
			since := func(int64) (time.Time, error) { return start, nil }
			if c.sinceFails {
				since = func(int64) (time.Time, error) { return time.Time{}, errors.New("the window start is unread") }
			}
			err := SearchMergedPullRequestsAcrossInstallation(t.Context(), client, []string{"alice"}, since, end, nil,
				func(githubapp.Installation, time.Time, []FetchedPullRequest) error { return nil })
			if err == nil {
				t.Fatal("the search answered no error for an installation it could not search")
			}
			redacttest.RequireNone(t, "delivery.installation-login/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the search's error is %q; want %q in it", err, c.want)
			}
		})
	}
}
