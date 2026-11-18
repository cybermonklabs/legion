package brokertest

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// The credentials the rows choose themselves; the proofs and the signed requests are minted per
// request, and a row reads them back from what the gateway saw. Each is long enough that excerpt
// redacts every run of eight of its characters, and none holds a word, so no piece of one can
// turn up in a message's own prose.
const (
	echoUIToken      = "ui-93c7e1a4b6d2f805"
	echoMachineCode  = "mc-0f5b9e3d7a2c4816"
	echoDeviceCode   = "dc-7a3e1c9b5f0d2864"
	echoRefresh      = "rt-e5b07c3a9d1f4682"
	echoUnusedAccess = "at-unused"
	echoDPoPProof    = "dp-1b9f3e7c5a0d8246"
)

// credentialFields are the fields of a JSON or form body, and of a query, that carry a credential
// in a request this package sends: a projected token, a signed request object, an authorization,
// device or machine-login code, a refresh token, a page token and a sign-in's state.
var credentialFields = []string{"token", "pod_token", "request", "code", "device_code", "refresh_token", "state"}

// gateway is a gateway standing in the broker's place, as a misrouted proxy or a captive portal
// can: it answers each request echo gives a status with a page of its own that echoes the request
// back (echoPage), and leaves every other to next, the broker's own answer. It keeps every
// credential the requests it echoed carried, and writes each to seenFile as well when one is set,
// for the parent of a child test process (TestProxyEchoHelpers).
type gateway struct {
	srv      *httptest.Server
	echo     func(r *http.Request) int
	next     http.HandlerFunc
	seenFile string

	mu   sync.Mutex
	seen []string
}

func newGateway(t *testing.T, echo func(r *http.Request) int, next http.HandlerFunc) *gateway {
	t.Helper()
	g := &gateway{echo: echo, next: next}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// echoAt is an echo that answers method on path at status, and leaves every other request alone.
func echoAt(status int, method, path string) func(r *http.Request) int {
	return func(r *http.Request) int {
		if r.Method == method && r.URL.Path == path {
			return status
		}
		return 0
	}
}

// echoAll is an echo that answers every request at status.
func echoAll(status int) func(r *http.Request) int {
	return func(*http.Request) int { return status }
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	status := g.echo(r)
	if status == 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.next(w, r)
		return
	}
	carried := carriedBy(r, body)
	g.mu.Lock()
	for _, c := range carried {
		if !slices.Contains(g.seen, c) {
			g.seen = append(g.seen, c)
		}
	}
	if g.seenFile != "" {
		_ = os.WriteFile(g.seenFile, []byte(strings.Join(g.seen, "\n")), 0o600)
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, echoPage(r, body, carried))
}

// credentials is every credential the requests the gateway echoed carried.
func (g *gateway) credentials() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seen)
}

// carriedBy is every credential r carries: its Authorization's value after the scheme, its Proof
// and DPoP headers, each cookie's value, and each of credentialFields in its query and in its
// JSON or form body.
func carriedBy(r *http.Request, body []byte) []string {
	var carried []string
	for _, name := range []string{"Authorization", "Proof", "DPoP"} {
		value := r.Header.Get(name)
		if _, rest, ok := strings.Cut(value, " "); ok && name == "Authorization" {
			value = rest
		}
		if value != "" {
			carried = append(carried, value)
		}
	}
	for _, cookie := range r.Cookies() {
		carried = append(carried, cookie.Value)
	}
	fields := r.URL.Query()
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		for name, value := range object {
			if s, ok := value.(string); ok {
				fields.Add(name, s)
			}
		}
	} else if form, err := url.ParseQuery(string(body)); err == nil {
		for name, values := range form {
			fields[name] = append(fields[name], values...)
		}
	}
	for _, name := range credentialFields {
		for _, value := range fields[name] {
			if value != "" {
				carried = append(carried, value)
			}
		}
	}
	return carried
}

// echoPage is the gateway's page for r: each credential r carried bare on a line of its own first,
// so excerpt's Authorization, Bearer and DPoP patterns cannot hide it and only the redaction of
// the credentials a message is handed can, then r's request line, headers and body as they came.
func echoPage(r *http.Request, body []byte, carried []string) string {
	var page strings.Builder
	page.WriteString("<!DOCTYPE html>\n<html><head><title>Gateway</title><style>p{margin:0}</style></head><body>\n<h1>Gateway</h1>\n")
	for _, c := range carried {
		page.WriteString("<p>\n" + c + "\n</p>\n")
	}
	fmt.Fprintf(&page, "<pre>\n%s %s %s\n", r.Method, r.URL.RequestURI(), r.Proto)
	for _, name := range slices.Sorted(maps.Keys(r.Header)) {
		for _, value := range r.Header[name] {
			fmt.Fprintf(&page, "%s: %s\n", name, value)
		}
	}
	page.WriteString("\n" + string(body) + "\n</pre>\n</body></html>\n")
	return page.String()
}

// leaked is the first piece of credential that out holds: a run of eight of its characters, the
// shortest run excerpt redacts wherever it stands, or credential whole where it is shorter; ""
// where out holds none.
func leaked(out, credential string) string {
	length := min(8, len(credential))
	for at := 0; length > 0 && at+length <= len(credential); at++ {
		if piece := credential[at : at+length]; strings.Contains(out, piece) {
			return piece
		}
	}
	return ""
}

// requireRedacted fails t unless out, what a helper printed after the gateway's page, holds no
// piece of any of credentials and names the page in excerpt's form, form ("" for a page the
// boundary's rule may name as not readable rather than as an excerpt).
func requireRedacted(t *testing.T, what, out string, credentials []string, form string) {
	t.Helper()
	if len(credentials) == 0 {
		t.Fatalf("%s: the gateway echoed no request carrying a credential, so nothing was checked:\n%s", what, out)
	}
	for _, c := range credentials {
		if piece := leaked(out, c); piece != "" {
			t.Errorf("%s printed %q, a piece of a credential its request carried:\n%s", what, piece, out)
		}
	}
	if !strings.Contains(out, form) {
		t.Errorf("%s does not name the gateway's page as %q:\n%s", what, form, out)
	}
	t.Logf("%s printed:\n%s", what, out)
}

// The rows below drive helpers that take a *testing.T and end it on a failure, which a test cannot
// watch happen to itself. Each runs in a child test process (TestProxyEchoChild) named by
// childCaseEnv, whose gateway writes every credential it saw to the file childSeenEnv names, and
// what the child printed is what the helper printed.
const (
	childCaseEnv = "BROKERTEST_PROXY_ECHO_CASE"
	childSeenEnv = "BROKERTEST_PROXY_ECHO_SEEN"
)

// childCase is one child: what it drives, and the form its failure must name the page in.
type childCase struct {
	form  string
	drive func(t *testing.T, seenFile string)
}

// newChildGateway is a gateway in a child process, writing what it sees to seenFile.
func newChildGateway(t *testing.T, seenFile string, echo func(r *http.Request) int, next http.HandlerFunc) *gateway {
	t.Helper()
	g := newGateway(t, echo, next)
	g.seenFile = seenFile
	return g
}

// fakeDPoP is a Client's DPoP wrapper that puts one fixed proof on every request, so a row knows
// the proof a message must not print.
func fakeDPoP(_ *ecdsa.PrivateKey, base http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("DPoP", echoDPoPProof)
		return base.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// echoClient is a Client pinned to g, presenting echoUIToken on the UI routes and fakeDPoP's
// proof on the OAuth steps.
func echoClient(t *testing.T, g *gateway) *Client {
	t.Helper()
	c := NewClient(t, "http://broker.example", strings.TrimPrefix(g.srv.URL, "http://"))
	c.UIToken, c.DPoP = echoUIToken, fakeDPoP
	return c
}

// pendingLookup answers a machine login's lookup as the broker does for a pending one.
func pendingLookup(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, `{"record_id":"rec-1","state":"pending"}`)
}

// childCases are the helpers a child runs, by name. Another file of the package adds its own
// helpers' cases in an init.
var childCases = map[string]childCase{
	"client.go EnrollSession refused": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodPost, "/v1/enrollments"), nil)).
			EnrollSession(t, NewSigningKey(t), "lc-1", "session-a")
	}},
	"client.go EnrollSession answered 201": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAt(http.StatusCreated, http.MethodPost, "/v1/enrollments"), nil)).
			EnrollSession(t, NewSigningKey(t), "lc-1", "session-a")
	}},
	"client.go AskForAgentSecret answered 200": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusOK), nil)).
			AskForAgentSecret(t, NewSigningKey(t), "enr-1", "session-a needs it", "APP_KEY")
	}},
	"client.go RequestAgentSecret refused": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusForbidden), nil)).
			RequestAgentSecret(t, NewSigningKey(t), "enr-1", "session-a needs it", "APP_KEY")
	}},
	"client.go GrantValues refused": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusForbidden), nil)).GrantValues(t, NewSigningKey(t), "enr-1", "grant-1")
	}},
	"client.go GrantValues answered 200": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusOK), nil)).GrantValues(t, NewSigningKey(t), "enr-1", "grant-1")
	}},
	"client.go DeviceLogin refused": {"", func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusForbidden), nil)).DeviceLogin(t, NewSigningKey(t), "ada", "host-a")
	}},
	"client.go ApproveMachineLogin lookup refused": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusForbidden), nil)).ApproveMachineLogin(t, "ada", echoMachineCode)
	}},
	"client.go ApproveMachineLogin lookup answered 200": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		echoClient(t, newChildGateway(t, seen, echoAll(http.StatusOK), nil)).ApproveMachineLogin(t, "ada", echoMachineCode)
	}},
	"client.go ApproveMachineLogin decision refused": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodPost, "/v1/credential-requests/rec-1/approve"), pendingLookup)
		echoClient(t, g).ApproveMachineLogin(t, "ada", echoMachineCode)
	}},
	"client.go ApproveMachineLogin decision answered 200": {`a body that is not the broker's error JSON (`, func(t *testing.T, seen string) {
		g := newChildGateway(t, seen, echoAt(http.StatusOK, http.MethodPost, "/v1/credential-requests/rec-1/approve"), pendingLookup)
		echoClient(t, g).ApproveMachineLogin(t, "ada", echoMachineCode)
	}},
}

// TestProxyEchoChild is one of childCases, run only as a child process of TestProxyEchoHelpers.
func TestProxyEchoChild(t *testing.T) {
	name := os.Getenv(childCaseEnv)
	if name == "" {
		t.Skip("a child process of TestProxyEchoHelpers, run only by it")
	}
	childCases[name].drive(t, os.Getenv(childSeenEnv))
}

// TestProxyEchoHelpers: every helper of a Client, its machine-login decision among them, names a
// gateway's page in its failure with each credential its request carried redacted: the UI bearer,
// a session or launcher proof, the signed request object and the machine-login code its body
// carries, and a DPoP proof. The OAuth step's page is named by the OAuth boundary
// (redact.OAuth2), which may print the rule's not-readable words in its place.
func TestProxyEchoHelpers(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(childCases)) {
		t.Run(name, func(t *testing.T) {
			seenFile := filepath.Join(t.TempDir(), "seen")
			child := exec.Command(os.Args[0], "-test.run=^TestProxyEchoChild$", "-test.v", "-test.count=1")
			child.Env = append(os.Environ(), childCaseEnv+"="+name, childSeenEnv+"="+seenFile)
			output, err := child.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "--- FAIL: TestProxyEchoChild") {
				t.Fatalf("the child did not fail on the gateway's page (%v):\n%s", err, output)
			}
			seen, _ := os.ReadFile(seenFile)
			requireRedacted(t, name, string(output), strings.Fields(string(seen)), childCases[name].form)
		})
	}
}

// TestProxyEchoClientOAuth: a device-code exchange and a refresh a gateway refuses answer an
// error that holds no credential its request carried, the code or the refresh token and the DPoP
// proof among them, and still unwraps to the *oauth2.RetrieveError a row reads its status from.
func TestProxyEchoClientOAuth(t *testing.T) {
	for _, row := range []struct {
		name     string
		exchange func(t *testing.T, c *Client) error
	}{
		{"client.go DeviceToken", func(t *testing.T, c *Client) error {
			_, err := c.DeviceToken(t, NewSigningKey(t), &oauth2.DeviceAuthResponse{DeviceCode: echoDeviceCode, Expiry: time.Now().Add(time.Minute)})
			return err
		}},
		{"client.go Refresh", func(t *testing.T, c *Client) error {
			_, err := c.Refresh(t, NewSigningKey(t), &oauth2.Token{AccessToken: echoUnusedAccess, RefreshToken: echoRefresh, Expiry: time.Now().Add(time.Hour)})
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := newGateway(t, echoAll(http.StatusForbidden), nil)
			err := row.exchange(t, echoClient(t, g))
			if err == nil {
				t.Fatal("the exchange took the gateway's page")
			}
			requireRedacted(t, row.name, err.Error(), g.credentials(), "")
			var refused *oauth2.RetrieveError
			if !errors.As(err, &refused) || refused.Response.StatusCode != http.StatusForbidden {
				t.Errorf("the error %v no longer unwraps to the RetrieveError with the gateway's 403", err)
			}
		})
	}
}
