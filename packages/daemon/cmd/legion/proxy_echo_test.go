package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoingProxy is a proxy in the daemon's place that answers every request with a page of its own
// echoing the request back: its Authorization header, the credential after the scheme on a line of
// its own, and its body. grants, when set, answers the grants route first instead, with that body
// and a 200.
func echoingProxy(t *testing.T, status int, grants string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if grants != "" && r.URL.Path == "/legion/v1/grants" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, grants)
			return
		}
		body, _ := io.ReadAll(r.Body)
		authorization := r.Header.Get("Authorization")
		_, sent, _ := strings.Cut(authorization, " ")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html><body><h1>Proxy</h1><pre>Authorization: %s\nseen=%s\n%s</pre></body></html>", authorization, sent, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// An operator command's refusal from a proxy's page names the page in one line with the operator
// bearer and any Authorization or Bearer value redacted: issueStatus fails the row where either
// stream holds the bearer.
func TestAnOperatorRefusalNamesAProxysPageWithoutTheBearer(t *testing.T) {
	t.Setenv("LEGION_DAEMON_URL", echoingProxy(t, http.StatusBadGateway, "").URL)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")

	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile)
	want := `legion issue status: the daemon answered 502 Bad Gateway: a body that is not the daemon's error JSON ("Proxy Authorization: Bearer [redacted] seen=[redacted] {}")` + "\n"
	if code != 1 || errb != want {
		t.Fatalf("legion status = %d, stderr %q; want 1 and %q", code, errb, want)
	}
}

// A 2xx that is not the daemon's JSON is named the same way and printed nowhere as it came, since
// what answered in the daemon's place can echo the bearer in a 2xx too; a JSON 2xx that holds no
// grant is named as no grant answer.
func TestAGrantAnswerThatIsNoGrantIsNamedWithoutTheBearer(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	proxy := echoingProxy(t, http.StatusOK, "")
	t.Setenv("LEGION_DAEMON_URL", proxy.URL)
	code, out, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile)
	want := "legion issue status: " + proxy.URL + `/legion/v1/grants answered with a body that is not the daemon's JSON ("Proxy Authorization: Bearer [redacted] seen=[redacted] {}")` + "\n"
	if code != 1 || out != "" || errb != want {
		t.Fatalf("legion status = %d, stdout %q, stderr %q; want 1, nothing on stdout and %q", code, out, errb, want)
	}

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":%q}`, r.Header.Get("Authorization"))
	}))
	defer echo.Close()
	t.Setenv("LEGION_DAEMON_URL", echo.URL)
	code, _, errb = issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile)
	if want := `no grant in it: a body that is not a grant answer ("{\"echo\":\"Bearer [redacted]\"}")`; code != 1 || !strings.Contains(errb, want) {
		t.Fatalf("legion status = %d, stderr %q; want 1 and a failure naming %q", code, errb, want)
	}
}

// The status request authenticates with the grant in its body, so the grant is what a proxy's
// echo of that body would print, and it is redacted as a bearer is.
func TestAStatusRefusalNamesAProxysPageWithoutTheGrant(t *testing.T) {
	const grant = "grant-Q7vK2mXr9LtW4nBz8YcH"
	t.Setenv("LEGION_DAEMON_URL", echoingProxy(t, http.StatusBadGateway, `{"grantId":"`+grant+`","expiresAt":"2026-11-18T12:00:00Z"}`).URL)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")

	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile)
	// The request carries no Authorization header, so the rule for its value takes the next word.
	if code != 1 || strings.Contains(errb, grant) || !strings.Contains(errb, `a body that is not the daemon's error JSON ("Proxy Authorization: [redacted] {\"grantId\":\"[redacted]\"`) {
		t.Fatalf("legion status = %d, stderr %q; want 1 and the page named with the grant redacted", code, errb)
	}
}

// `legion controller start`'s two calls name a proxy's page without what they carried: the
// operator bearer on the secret's fetch, and the controller's capability secret in the
// credential's body.
func TestTheControllerCallsNameAProxysPageWithoutWhatTheyCarried(t *testing.T) {
	proxy := echoingProxy(t, http.StatusBadGateway, "")
	const secret = "capability-Q7vK2mXr9LtW4nBz8YcH"

	_, _, fetched := fetchControllerSecret(context.Background(), proxy.URL, controllerOperatorToken, 55)
	_, _, credential := fetchControllerCredential(context.Background(), proxy.URL, secret, "")
	for name, refused := range map[string]struct {
		err     error
		carried string
	}{"secret fetch": {fetched, controllerOperatorToken}, "credential fetch": {credential, secret}} {
		if refused.err == nil || strings.Contains(refused.err.Error(), refused.carried) ||
			!strings.Contains(refused.err.Error(), `the daemon answered 502 Bad Gateway: a body that is not the daemon's error JSON ("Proxy Authorization: `) {
			t.Errorf("the %s = %v, want the page named without %q", name, refused.err, refused.carried)
		}
	}
}
