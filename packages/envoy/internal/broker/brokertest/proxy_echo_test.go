package brokertest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The rows of this file drive Pods (requestseed's client) and the approval page's Browser through
// the gateway answertext_test.go stands in the broker's place; the credentials they choose
// themselves are below, beside the ones that file holds. Each is long enough that excerpt redacts
// every run of eight of its characters, and none holds a word.
const (
	echoPodToken      = "pt-4c8a1e6f0b9d2735"
	echoSessionCookie = "sc-2f6a9d0c4e8b1735"
	echoPageToken     = "pg-d41b7e5c09a3f628"
	echoCode          = "cd-6e2c9a1f7b40d385"
	echoState         = "st-b18f4d6a2e9c0573"
	echoBrokerRefusal = `{"code":"ENROLLMENT_UNKNOWN","error":"the broker holds no enrollment enr-1"}`
)

// stepTB is the TB requestseed gives a Pods (its command), keeping what it was told: each log
// line, and the failure that ends the step, which ends the step's goroutine as a failed command
// ends its process.
type stepTB struct {
	mu    sync.Mutex
	lines []string
	fatal string
}

func (*stepTB) Helper() {}

func (s *stepTB) Logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *stepTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.fatal = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	runtime.Goexit()
}

// run runs step and answers everything it printed, its failure last; failed is false unless the
// step failed.
func (s *stepTB) run(step func()) (printed string, failed bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		step()
	}()
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(append(slices.Clone(s.lines), s.fatal), "\n"), s.fatal != ""
}

// TestProxyEchoPods: every step of a Pods, requestseed's client, names a gateway's page in the
// exchange's log line and in its failure with each credential its request carried redacted: the
// Proof header, and the projected token and signed request object its body carries.
func TestProxyEchoPods(t *testing.T) {
	podKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	launcherKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pod := Pod{RuntimeID: "pod-a", Key: podKey, EnrollmentID: "enr-1"}
	launcher := Launcher{Key: launcherKey, CredentialID: "lc-1"}
	for _, row := range []struct {
		name   string
		status int
		step   func(b Pods)
	}{
		{"pods.go LogIn", http.StatusForbidden, func(b Pods) { b.LogIn("svc-a", "acct-a") }},
		{"pods.go Enroll", http.StatusForbidden, func(b Pods) { b.Enroll(launcher, "pod-a", "acct-a") }},
		{"pods.go EnrollRefused answered 201", http.StatusCreated, func(b Pods) { b.EnrollRefused(launcher, "pod-a", "acct-a") }},
		{"pods.go AskWithReason", http.StatusForbidden, func(b Pods) { b.Ask(pod, "APP_KEY") }},
		{"pods.go AskWithReason answered 200", http.StatusOK, func(b Pods) { b.Ask(pod, "APP_KEY") }},
		{"pods.go State", http.StatusForbidden, func(b Pods) { b.State(pod, "req-1") }},
		{"pods.go Cancel", http.StatusForbidden, func(b Pods) { b.Cancel(pod, "req-1") }},
		{"pods.go Values", http.StatusForbidden, func(b Pods) { b.Values(pod, "grant-1") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := newGateway(t, echoAll(row.status), nil)
			tb := &stepTB{}
			printed, failed := tb.run(func() {
				row.step(Pods{T: tb, Base: g.srv.URL, Token: func(string, string) string { return echoPodToken }})
			})
			if !failed {
				t.Fatalf("the step took the gateway's page:\n%s", printed)
			}
			requireRedacted(t, row.name, printed, g.credentials(), `a body that is not the broker's error JSON (`)
		})
	}
}

// TestProxyEchoPodsKeepTheBrokersOwnRefusal: the broker's own error JSON is printed as its error
// and its code, in the log line and in the failure, never as an excerpt of a foreign page. #4368's
// answerText reads that JSON's two fields by the rule (excerpt.ErrorJSON) and writes them again,
// so the row holds the words, not the bytes the broker wrote.
func TestProxyEchoPodsKeepTheBrokersOwnRefusal(t *testing.T) {
	podKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, echoBrokerRefusal+"\n")
	}))
	t.Cleanup(broker.Close)
	tb := &stepTB{}
	printed, failed := tb.run(func() {
		Pods{T: tb, Base: broker.URL}.State(Pod{RuntimeID: "pod-a", Key: podKey, EnrollmentID: "enr-1"}, "req-1")
	})
	const words, code = `"error":"the broker holds no enrollment enr-1"`, `"code":"ENROLLMENT_UNKNOWN"`
	if !failed || strings.Count(printed, words) != 2 || strings.Count(printed, code) != 2 || strings.Contains(printed, "a body that is not") {
		t.Fatalf("the step printed %q, want the broker's error and code after 401 in its log line and its failure", printed)
	}
}

// signInFlow answers a browser's sign-in as the broker and its provider do, up to the step the
// gateway answers instead: a cookie for /seed, a record's page that sends the browser to the
// provider, the provider's return to the callback with a code and the state, and a record's page
// with its decision form once signed in (decided).
func signInFlow(decided bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch {
		case r.URL.Path == "/seed":
			http.SetCookie(w, &http.Cookie{Name: "broker_session", Value: echoSessionCookie, Path: "/"})
		case r.URL.Path == "/approve/rec-1" && decided:
			_, _ = io.WriteString(w, `<form method="post" action="/approve"><input type="hidden" name="record" value="rec-1">`+
				`<input type="hidden" name="token" value="`+echoPageToken+`"><button name="decision" value="approve">Approve</button></form>`)
		case r.URL.Path == "/approve/rec-1":
			http.Redirect(w, r, base+"/authorize?state="+echoState, http.StatusFound)
		case r.URL.Path == "/authorize":
			http.Redirect(w, r, base+"/signin/callback?code="+echoCode+"&state="+echoState, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}
}

// echoBrowser is a Browser on g holding the broker's session cookie, whose provider's code flow is
// armed so a row can sign in.
func echoBrowser(t *testing.T, g *gateway) *Browser {
	t.Helper()
	issuer := oidctest.New(t)
	SignIn(issuer, oidctest.NewKey(t, "echo"))
	b := NewBrowser(t, g.srv.URL, http.DefaultTransport, issuer)
	b.Get(t, "/seed")
	if b.Cookie(t, "broker_session") != echoSessionCookie {
		t.Fatal("the browser holds no session cookie, so no row would carry one")
	}
	return b
}

// The approval page's Browser steps, run as child processes by TestProxyEchoHelpers: each names a
// gateway's page with the session cookie, the page token, the code and the state redacted.
func init() {
	for name, c := range map[string]childCase{
		"approval.go Location": {`a body that is not a redirect (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodGet, "/elsewhere"), signInFlow(false))
			echoBrowser(t, g).Get(t, "/elsewhere").Location(t)
		}},
		"approval.go StartSignIn": {`a body that is not a redirect to the provider (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodGet, "/approve/rec-1"), signInFlow(false))
			echoBrowser(t, g).StartSignIn(t, "/approve/rec-1")
		}},
		"approval.go Authorize": {`a body that is not a redirect to the callback (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodGet, "/authorize"), signInFlow(false))
			authorize, err := url.Parse(g.srv.URL + "/authorize?state=" + echoState)
			if err != nil {
				t.Fatal(err)
			}
			echoBrowser(t, g).Authorize(t, authorize, Person("ada@"+SignInDomain))
		}},
		"approval.go SignIn": {`a body that is not a redirect to the record's page (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodGet, "/signin/callback"), signInFlow(false))
			echoBrowser(t, g).SignIn(t, "rec-1", Person("ada@"+SignInDomain))
		}},
		"approval.go Page": {`a body that is not the record's page (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodGet, "/approve/rec-1"), signInFlow(false))
			echoBrowser(t, g).Page(t, "rec-1")
		}},
		"approval.go Decide": {`a body that is not a redirect to the record's page (`, func(t *testing.T, seen string) {
			g := newChildGateway(t, seen, echoAt(http.StatusForbidden, http.MethodPost, "/approve"), signInFlow(true))
			echoBrowser(t, g).Decide(t, "rec-1", true)
		}},
	} {
		childCases[name] = c
	}
}
