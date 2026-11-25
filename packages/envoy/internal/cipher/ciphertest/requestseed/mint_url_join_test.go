package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestTheMintJoinsItsRouteOntoTheIssuerURL: --issuer with a path and a query (path and query):
// each token is minted by a POST to /_dev/mint joined onto that path with the query kept
// (urljoin.Join). Where requestseed wrote the route after --issuer's text, the route went inside
// the query and the stand-in issuer was asked /prefix. A plain origin (origin) is asked the
// route, as before.
func TestTheMintJoinsItsRouteOntoTheIssuerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, asked string }{
		{"path and query", "/prefix?key=k3y-made-up", "/prefix/_dev/mint?key=k3y-made-up"},
		{"origin", "", "/_dev/mint"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
				mu.Unlock()
				fmt.Fprint(w, `{"token":"minted-token"}`)
			}))
			t.Cleanup(issuer.Close)
			given := issuer.URL + c.suffix
			var stderr strings.Builder
			token := devIssuerTokens(&command{stderr: &stderr}, given)(podAccount, "pod-1")
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"POST " + c.asked}; token != "minted-token" || !slices.Equal(asked, want) {
				t.Errorf("a mint at --issuer %s = %q and asked %q (stderr %q); want minted-token, asking %q", given, token, asked, stderr.String(), want)
			}
		})
	}
}

// TestAFailedMintNamesTheJoinedURL: a mint at an --issuer with a path and a query that the issuer
// refuses (refused), whose answer is cut short (cut short), or whose issuer is gone (unreachable)
// ends the command with a line naming the URL it asked: /_dev/mint joined onto the --issuer's
// path with the query kept. Where requestseed wrote the route after --issuer's text, the line
// named that text. A failed mint ends the process, so each runs in a child (TestTheMintChild).
func TestAFailedMintNamesTheJoinedURL(t *testing.T) {
	for _, c := range []struct {
		name    string
		respond func(w http.ResponseWriter)
		want    func(asked string) string
	}{
		{"refused", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "the issuer is down")
		}, func(asked string) string {
			return "requestseed: POST " + asked + " for " + podAccount + " = 503 the issuer is down, want 200 with a token"
		}},
		{"cut short", func(w http.ResponseWriter) {
			conn, buffered, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack the stand-in's connection: %v", err)
				return
			}
			defer conn.Close()
			fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"token\":")
			_ = buffered.Flush()
		}, func(asked string) string { return "requestseed: read the answer to POST " + asked + ": " }},
		{"unreachable", nil, func(asked string) string { return "requestseed: POST " + asked + ": " }},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.respond(w) }))
			if c.respond == nil {
				issuer.Close()
			} else {
				t.Cleanup(issuer.Close)
			}
			given := issuer.URL + "/prefix?key=k3y-made-up"
			printed, exited := mintInChild(t, given)
			if want := c.want(issuer.URL + "/prefix/_dev/mint?key=k3y-made-up"); exited != 1 || !strings.Contains(printed, want) {
				t.Errorf("a mint at --issuer %s exited %d printing %q; want 1, printing %q", given, exited, printed, want)
			}
		})
	}
}

// TestAnIssuerURLWithNoHostIsRefusedQuotingNoneOfIt: an --issuer that is no absolute URL with a
// host ends the command before any mint, in urljoin's words, which quote none of it, since the URL
// can carry a credential. Where requestseed wrote the route after --issuer's text, net/http's
// error for it quoted the URL whole.
func TestAnIssuerURLWithNoHostIsRefusedQuotingNoneOfIt(t *testing.T) {
	printed, exited := mintInChild(t, "127.0.0.1:9?key=k3y-made-up")
	if want := "requestseed: --issuer: " + urljoin.ErrBase.Error(); exited != 1 || !strings.Contains(printed, want) || strings.Contains(printed, "k3y-made-up") {
		t.Errorf("a mint at an --issuer with no scheme exited %d printing %q; want 1, printing %q and none of the URL", exited, printed, want)
	}
}

// mintChildEnv names the --issuer TestTheMintChild mints at.
const mintChildEnv = "REQUESTSEED_MINT_CHILD_ISSUER"

// TestTheMintChild mints one token at the --issuer mintChildEnv names, as ask does for a pod, in
// a child process of a row: a failed mint ends the process, 1, printing its line to stderr.
func TestTheMintChild(t *testing.T) {
	issuer := os.Getenv(mintChildEnv)
	if issuer == "" {
		t.Skip("the child of TestAFailedMintNamesTheJoinedURL and TestAnIssuerURLWithNoHostIsRefusedQuotingNoneOfIt")
	}
	devIssuerTokens(&command{stderr: os.Stderr}, issuer)(podAccount, "pod-1")
}

// mintInChild runs TestTheMintChild for given and answers what it printed and its exit code.
func mintInChild(t *testing.T, given string) (string, int) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestTheMintChild$", "-test.count=1")
	child.Env = append(os.Environ(), mintChildEnv+"="+given)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(output), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("run the child: %v", err)
	}
	return string(output), 0
}
