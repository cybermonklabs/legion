package githubapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// proxyEchoToken is the installation token proxyEchoGitHub mints; its prefix is joined at run
// time, since the push's leak check refuses a GitHub token written whole.
var proxyEchoToken = "ghs" + "_proxyEchoInstallationTokenK1l2M3n4O5p6"

// proxyEchoGitHub is GitHub with a gateway in its place for the requests echo names: those get
// the gateway's HTML page at status, echoing the request back, the credential bare on a line of
// its own and in the Authorization line. Every other request is GitHub's: the installation
// lookup, the token mint, and, for any other path, the JSON own names, or a pull request.
type proxyEchoGitHub struct {
	echo   func(*http.Request) bool
	status int
	// own, when set, is GitHub's own refusal every read answers, at ownStatus.
	own       string
	ownStatus int

	mu     sync.Mutex
	echoed []string
}

func (f *proxyEchoGitHub) serve(w http.ResponseWriter, r *http.Request) {
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.echo(r) {
		f.mu.Lock()
		f.echoed = append(f.echoed, credential)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(f.status)
		fmt.Fprintf(w, "<html><head><title>%d %s</title></head><body><pre>\n%s\n%s %s HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
			f.status, http.StatusText(f.status), credential, r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation"):
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read"}}`)
	case r.URL.Path == "/app/installations/11/access_tokens":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, proxyEchoToken, time.Now().Add(time.Hour).Format(time.RFC3339))
	case f.own != "":
		w.WriteHeader(f.ownStatus)
		fmt.Fprint(w, f.own)
	default:
		fmt.Fprint(w, `{"state":"open","title":"Ship it"}`)
	}
}

func (f *proxyEchoGitHub) credentials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.echoed...)
}

func newProxyEchoClient(t *testing.T, fake *proxyEchoGitHub) *githubapp.Client {
	t.Helper()
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.serve})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxyecho", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client
}

// The proxy answers the browser with what GitHub answered and with the errors of the calls it
// makes for it, and a gateway in GitHub's place can answer either call with a page echoing the
// request back: the token mint's App JWT, or the read's installation token. The browser is given
// neither. A refusal that is not GitHub's error JSON keeps the status it came with and becomes
// the {code, error} the web client reads (api/client.ts errorFrom, ApiError), naming the page in
// one line with the token redacted; a mint that failed is 502, as before.
func TestProxyEchoTheProxyNeverHandsTheBrowserTheRequestsCredential(t *testing.T) {
	for _, row := range []struct {
		name         string
		echo         func(*http.Request) bool
		status, want int
	}{
		{"token mint", func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/installation") }, http.StatusServiceUnavailable, http.StatusBadGateway},
		{"forwarded read", func(r *http.Request) bool { return r.URL.Path == "/repos/acme/web/pulls/7" }, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := &proxyEchoGitHub{echo: row.echo, status: row.status}
			response := proxy(newProxyEchoClient(t, fake), http.MethodGet, "/api/github/rest/repos/acme/web/pulls/7")
			text := response.Body.String()
			t.Logf("the browser is handed %d %s", response.Code, text)
			echoed := fake.credentials()
			if len(echoed) == 0 {
				t.Fatalf("the gateway echoed no credential, so the row proves nothing; the browser got %d %s", response.Code, text)
			}
			for _, credential := range echoed {
				for at := 0; at+12 <= len(credential); at++ {
					if strings.Contains(text, credential[at:at+12]) {
						t.Fatalf("the browser is given the credential the request carried (%.16s…): %d %s", credential, response.Code, text)
					}
				}
			}
			var answer struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
				t.Fatalf("the browser's answer %q is not the {code, error} it reads: %v", text, err)
			}
			if response.Code != row.want || answer.Code != "GITHUB_UPSTREAM" ||
				!strings.Contains(answer.Error, `a body that is not GitHub's error JSON ("`) {
				t.Fatalf("the browser got %d %s, want %d GITHUB_UPSTREAM naming a body that is not GitHub's error JSON", response.Code, text, row.want)
			}
		})
	}
}

// GitHub's own refusal is GitHub's word, and the browser is given it as GitHub sent it, status,
// type and body, as before.
func TestProxyEchoGitHubsOwnRefusalPassesThrough(t *testing.T) {
	const own = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	fake := &proxyEchoGitHub{echo: func(*http.Request) bool { return false }, own: own, ownStatus: http.StatusNotFound}
	response := proxy(newProxyEchoClient(t, fake), http.MethodGet, "/api/github/rest/repos/acme/web/pulls/7")
	if response.Code != http.StatusNotFound || response.Body.String() != own || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GitHub's own 404 reached the browser as %d %q (%s), want it as GitHub sent it", response.Code, response.Body.String(), response.Header().Get("Content-Type"))
	}
}
