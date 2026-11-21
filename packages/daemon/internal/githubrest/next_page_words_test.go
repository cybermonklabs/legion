package githubrest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestANextPageGitHubNamesPrintsNoTokenItEchoes: GitHub, or whatever answers in its place, names
// in its Link header a next page whose URL echoes the installation token the read carried and the
// probe's made-up credentials: on another origin, which is refused (elsewhere), or on the API's own
// origin, whose read GitHub answers 403 (refused) or which gets no answer at all (unanswered). The
// error holds none of the token or the probe's credentials, and still names its place: the origin
// the token is sent to, or the page by its number under the caller's path. The error named the
// next page's URL as GitHub's header wrote it: in the refusal, in GitHub's answer's path and in
// net/http's error. Captures githubrest.answer-words/next-page-<case>.
func TestANextPageGitHubNamesPrintsNoTokenItEchoes(t *testing.T) {
	token := githubtest.InstallationToken(t.Name())
	const path = "/rules/branches/main"
	echoed := "/repositories/42/" + token + "/not-a-secret-user:not-a-secret-password?page=2&access_token=not-a-secret-token"
	for _, tc := range []struct {
		name string
		// next is the next page's URL, given the server's own.
		next func(server string) string
		// laterPage answers the read of the next page on the API's own origin.
		laterPage func(w http.ResponseWriter)
		// names is what the error must still say of its place, given the server's own URL.
		names func(server string) string
	}{
		{
			name:  "elsewhere",
			next:  func(string) string { return "https://not-a-secret-user:not-a-secret-password@issuer.example" + echoed },
			names: func(server string) string { return "is not on " + server },
		},
		{
			name: "refused",
			next: func(server string) string { return server + echoed },
			laterPage: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			},
			names: func(string) string { return "GitHub answered GET " + path + " (page 2) with 403" },
		},
		{
			name: "unanswered",
			next: func(server string) string { return server + echoed },
			laterPage: func(w http.ResponseWriter) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			},
			names: func(string) string { return path + " (page 2)" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var server *httptest.Server
			server = githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/acme/widgets"+path {
					w.Header().Set("Link", "<"+tc.next(server.URL)+`>; rel="next"`)
					_, _ = io.WriteString(w, `["first"]`)
					return
				}
				if tc.laterPage == nil {
					githubtest.Unexpected(t, w, r)
					return
				}
				tc.laterPage(w)
			}})
			client := Client{Token: token, API: server.URL + "/repos/acme/widgets"}

			got, err := GetPages[string](context.Background(), client, path)
			if err == nil || got != nil {
				t.Fatalf("GetPages = %v, %v; want the read refused", got, err)
			}
			printed := err.Error()
			var answer *Answer
			if errors.As(err, &answer) {
				printed += " " + answer.Path
			}
			if want := tc.names(server.URL); !strings.Contains(printed, want) {
				t.Errorf("the read's error is %q, want it to name %q", printed, want)
			}
			oauth2calltest.RequireNone(t, "githubrest.answer-words/next-page-"+tc.name, printed)
			for at := 0; at+8 <= len(token); at++ {
				if strings.Contains(printed, token[at:at+8]) {
					t.Fatalf("the read's error holds a run of the token the read carried: %q", printed)
				}
			}
		})
	}
}
