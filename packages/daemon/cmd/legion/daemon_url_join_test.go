package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// daemonURLJoins are the daemon URLs a pane can be told (LEGION_DAEMON_URL, which set by hand can
// carry a path, a query, user information and a fragment), each by what follows the stand-in's
// scheme, host and port, with the path and the query a call of route must reach the daemon with.
var daemonURLJoins = []struct {
	name, user, suffix, dir, query string
}{
	{"query", "legion-reader", "/daemon?sig=s1g-7Qz4k", "/daemon", "sig=s1g-7Qz4k"},
	{"query-trailing-slash", "", "/daemon/?sig=s1g-7Qz4k", "/daemon", "sig=s1g-7Qz4k"},
	{"fragment", "", "/daemon#pane", "/daemon", ""},
	{"no-query", "", "/daemon", "/daemon", ""},
}

// askedDaemon is what a stand-in daemon recorded of the one request it was asked: its URL and the
// user name of its Basic authorization, which net/http sends from a URL's user information.
type askedDaemon struct {
	url  url.URL
	user string
}

// urlRecordingDaemon is a stand-in daemon that answers every request 200 with body and records
// what it was asked, which asked answers once the command has run.
func urlRecordingDaemon(t *testing.T, body string) (*httptest.Server, func(t *testing.T) askedDaemon) {
	t.Helper()
	requests := make(chan askedDaemon, 8)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		requests <- askedDaemon{*r.URL, user}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(daemon.Close)
	return daemon, func(t *testing.T) askedDaemon {
		t.Helper()
		select {
		case asked := <-requests:
			if len(requests) != 0 {
				t.Errorf("the daemon was asked %d more requests, want one", len(requests))
			}
			return asked
		default:
			t.Fatal("no request reached the daemon")
			return askedDaemon{}
		}
	}
}

// standInURL is the stand-in's URL with user before its host where user is not "", and suffix
// after its port.
func standInURL(daemon *httptest.Server, user, suffix string) string {
	base := daemon.URL
	if user != "" {
		base = strings.Replace(base, "://", "://"+user+"@", 1)
	}
	return base + suffix
}

// TestAnOperatorCallGoesToTheRouteJoinedOntoTheDaemonURL: an operator call (operator.do, here
// `legion claims list` in a pane) goes to the daemon URL's path with its route joined onto it, the
// query kept, the user information and port kept, and the fragment dropped:
// http://legion-reader@host:port/daemon?sig=... is asked /daemon/legion/v1/operator/claims?sig=...
// (query), and so is http://host:port/daemon/?sig=... (query-trailing-slash), where the route
// appended to the URL's text sat inside the query (/daemon?sig=.../legion/v1/operator/claims, the
// daemon asked /daemon with the query's value changed); http://host:port/daemon#pane is asked
// /daemon/legion/v1/operator/claims (fragment), where the route sat inside the fragment and the
// daemon was asked /daemon. A URL with neither is asked as it was (no-query). A stand-in daemon
// records the path and the query the call reaches it with.
func TestAnOperatorCallGoesToTheRouteJoinedOntoTheDaemonURL(t *testing.T) {
	tokenFile := writeFile(t, "operator-token", claimsOperatorToken+"\n")
	for _, c := range daemonURLJoins {
		t.Run(c.name, func(t *testing.T) {
			daemon, asked := urlRecordingDaemon(t, `{"claims":[]}`)
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_DAEMON_URL", standInURL(daemon, c.user, c.suffix))
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "claims", "list", "--operator-token-file", tokenFile}, &out, &errb)
			got := asked(t)
			if want := c.dir + claimsRoute; got.url.Path != want || got.url.RawQuery != c.query {
				t.Errorf("a daemon URL ending %q was asked path %q, query %q; want path %q, query %q", c.suffix, got.url.Path, got.url.RawQuery, want, c.query)
			}
			if code != 0 {
				t.Errorf("legion claims list = %d, stderr %q; want the daemon's list taken", code, errb.String())
			}
		})
	}
}

// TestAStateReadGoesToTheRouteJoinedOntoTheDaemonURL: `legion state` in a pane (get) reads the
// daemon URL's path with /legion/v1/state joined onto it, under the same cases as an operator call
// (TestAnOperatorCallGoesToTheRouteJoinedOntoTheDaemonURL); the read carries no bearer, so a URL's
// user name reaches the daemon as the Basic user net/http sends from it, kept by the join.
func TestAStateReadGoesToTheRouteJoinedOntoTheDaemonURL(t *testing.T) {
	for _, c := range daemonURLJoins {
		t.Run(c.name, func(t *testing.T) {
			daemon, asked := urlRecordingDaemon(t, `{}`)
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_DAEMON_URL", standInURL(daemon, c.user, c.suffix))
			t.Setenv("LEGION_ISSUE", "")
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "state", "--json"}, &out, &errb)
			got := asked(t)
			if want := c.dir + "/legion/v1/state"; got.url.Path != want || got.url.RawQuery != c.query || got.user != c.user {
				t.Errorf("a daemon URL ending %q was asked path %q, query %q, as user %q; want path %q, query %q, user %q",
					c.suffix, got.url.Path, got.url.RawQuery, got.user, want, c.query, c.user)
			}
			if code != 0 {
				t.Errorf("legion state --json = %d, stderr %q; want the daemon's state printed", code, errb.String())
			}
		})
	}
}

// TestTheStateHeadingNamesTheDaemonURLWithNoCredential: `legion state` names the daemon it read
// in its heading on stdout, which, unlike its stderr, no writer passes through the URL rule
// (newRoot). A daemon_url, which the daemon tells every pane as LEGION_DAEMON_URL, can carry user
// information, made-up credentials here: the heading names the daemon through redact.URL, by its
// scheme, its authority as the mark of a URL with credentials, and its path (heading). The twin
// shows the daemon URL holds the credentials. Captures legion.daemon-url/state-heading.
func TestTheStateHeadingNamesTheDaemonURLWithNoCredential(t *testing.T) {
	const credentials = "not-a-secret-user:not-a-secret-password"
	daemon, _ := urlRecordingDaemon(t, `{}`)
	told := standInURL(daemon, credentials, "/daemon")
	t.Run("twin", func(t *testing.T) {
		if !strings.Contains(told, credentials) {
			t.Fatalf("%q holds no %q, so the row cannot see a leak", told, credentials)
		}
	})
	t.Run("heading", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("LEGION_DAEMON_URL", told)
		t.Setenv("LEGION_ISSUE", "")
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
			t.Fatalf("legion state = %d, stderr %q; want the state printed", code, errb.String())
		}
		heading, _, _ := strings.Cut(out.String(), "\n")
		oauth2calltest.RequireNoPiece(t, "legion.daemon-url/state-heading", heading)
		if want := " on http://[a URL with credentials]/daemon — "; !strings.Contains(heading, want) {
			t.Errorf("the heading is %q, want it to name the daemon as %q", heading, want)
		}
	})
}

// TestADaemonURLThatDoesNotParsePrintsNoneOfIt: a daemon URL url.Parse refuses (an invalid escape
// in its path), with made-up credentials in its user information and its query, fails an operator
// call (operator-unparsed) and a state read (state-unparsed) in the CLI's own words, which hold
// none of the URL; so does one url.Parse takes whose joined URL, as url.URL.String writes it,
// url.Parse refuses again (an IPv6 zone holding a byte that is not ASCII, which String escapes as
// %FD; operator-reparsed, state-reparsed), whose failure prints through oauth2call.Unanswered.
// http.NewRequest's error for either quotes the URL whole (the twins). Captures
// legion.daemon-url/<case>.
func TestADaemonURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		token    = "c8q2v-not-a-secret-token-4h7m"
	)
	unparsed := "http://" + user + ":" + password + "@127.0.0.1:1/daemon%zz?sig=" + token
	reparsed := "http://" + user + ":" + password + "@[::%25\xfd]/daemon?sig=" + token
	holds := func(t *testing.T, text string) {
		t.Helper()
		for _, credential := range []string{user, password, token} {
			if !strings.Contains(text, credential) {
				t.Fatalf("%q holds no %q, so the row cannot see a leak", text, credential)
			}
		}
	}
	t.Run("unparsed-twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, unparsed+claimsRoute, nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		joined, err := urljoin.Join(reparsed, claimsRoute)
		if err != nil {
			t.Fatalf("urljoin.Join refused %q (%v); the row needs a daemon URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodGet, joined.String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	tokenFile := writeFile(t, "operator-token", claimsOperatorToken+"\n")
	const refused = "the daemon's URL is not an absolute URL with a host (it is not printed)"
	for _, c := range []struct {
		name, base string
		argv       []string
		want       string
	}{
		{"operator-unparsed", unparsed, []string{"legion", "claims", "list", "--operator-token-file", tokenFile}, "legion claims list: " + refused},
		{"state-unparsed", unparsed, []string{"legion", "state"}, "legion state: " + refused},
		{"operator-reparsed", reparsed, []string{"legion", "claims", "list", "--operator-token-file", tokenFile}, "legion claims list: parse "},
		{"state-reparsed", reparsed, []string{"legion", "state"}, "legion state: read http://[a URL with credentials]/daemon/legion/v1/state: parse "},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_DAEMON_URL", c.base)
			t.Setenv("LEGION_ISSUE", "")
			var out, errb bytes.Buffer
			if code := run(context.Background(), c.argv, &out, &errb); code != 1 {
				t.Errorf("%s = %d, want 1", strings.Join(c.argv[:3], " "), code)
			}
			said := out.String() + errb.String()
			oauth2calltest.RequireNoPiece(t, "legion.daemon-url/"+c.name, said, token)
			if !strings.HasPrefix(said, c.want) {
				t.Errorf("the command printed %q, want it to begin %q", said, c.want)
			}
		})
	}
}

// TestALineNamesTheDaemonURLAsTheRequestAskedIt: a line that names the URL a request of the daemon
// asked names the URL the request asked, its route joined onto the daemon URL's path
// (operator.urlOf), through the URL rule (operator.shown), which removes the daemon URL's user
// information and query, made-up credentials here: the --wait bound running out on an operator
// call (wait-bound), an answer `legion claims list` cannot read (list-unread), the list `legion
// claims retry --failed` (retry-list-unread) and `legion claims suspend` (suspend-list-unread)
// cannot read, and `legion controller start`'s refused reads of the state (controller-state), the
// capability (controller-secret) and the GitHub credential (controller-credential), whose
// daemon_url refuses a query and can carry user information. The route appended to the URL's text
// sat inside its query, so a claims line, whose stderr passes the URL rule (newRoot), named
// /daemon; and a controller read's error, which its stderr and its refresh log pass through the
// rule after it, held the user information itself. The twin shows the URL the request asked holds
// the credentials. Captures legion.daemon-url/<case>.
func TestALineNamesTheDaemonURLAsTheRequestAskedIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		token    = "c8q2v-not-a-secret-token-4h7m"
	)
	withCredentials := func(daemon *httptest.Server, query bool) string {
		base := standInURL(daemon, user+":"+password, "/daemon")
		if query {
			base += "?sig=" + token
		}
		return base
	}
	t.Run("twin", func(t *testing.T) {
		daemon, _ := urlRecordingDaemon(t, `{}`)
		joined, err := urljoin.Join(withCredentials(daemon, true), claimsRoute)
		if err != nil {
			t.Fatal(err)
		}
		for _, credential := range []string{user, password, token} {
			if !strings.Contains(joined.String(), credential) {
				t.Fatalf("%q holds no %q, so the rows cannot see a leak", joined, credential)
			}
		}
	})
	tokenFile := writeFile(t, "operator-token", claimsOperatorToken+"\n")
	// claims runs `legion claims <args>` against handler, told the daemon by a URL with the
	// credentials, and answers what it printed.
	claims := func(t *testing.T, handler http.HandlerFunc, args ...string) string {
		t.Helper()
		daemon := httptest.NewServer(handler)
		t.Cleanup(daemon.Close)
		t.Chdir(t.TempDir())
		t.Setenv("LEGION_DAEMON_URL", withCredentials(daemon, true))
		var out, errb bytes.Buffer
		argv := append(append([]string{"legion", "claims"}, args...), "--operator-token-file", tokenFile)
		if code := run(context.Background(), argv, &out, &errb); code != 1 {
			t.Errorf("legion claims %s = %d, stderr %q; want 1", args[0], code, errb.String())
		}
		return out.String() + errb.String()
	}
	unreadable := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not a claim") }
	// controller runs call against a daemon refusing every request with status, told it by a
	// daemon_url with the user information alone, and answers its error.
	controller := func(t *testing.T, status int, call func(daemonURL string) error) string {
		t.Helper()
		daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"refused"}`)
		}))
		t.Cleanup(daemon.Close)
		err := call(withCredentials(daemon, false))
		if err == nil {
			t.Fatal("the call answered no error")
		}
		return err.Error()
	}
	for _, c := range []struct {
		name, names string
		printed     func(t *testing.T) string
	}{
		{"wait-bound", "/daemon/legion/v1/operator/claims was not answered within the --wait bound", func(t *testing.T) string {
			return claims(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, "list", "--wait", "300ms")
		}},
		{"list-unread", "/daemon/legion/v1/operator/claims served", func(t *testing.T) string {
			return claims(t, unreadable, "list")
		}},
		{"retry-list-unread", "/daemon/legion/v1/operator/claims served", func(t *testing.T) string {
			return claims(t, unreadable, "retry", "--failed")
		}},
		{"suspend-list-unread", "/daemon/legion/v1/operator/claims served", func(t *testing.T) string {
			return claims(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{}`)
					return
				}
				unreadable(w, r)
			}, "suspend", "--claim", string(architectClaim))
		}},
		{"controller-state", "/daemon/legion/v1/state: the daemon answered 500", func(t *testing.T) string {
			return controller(t, http.StatusInternalServerError, func(daemonURL string) error {
				return checkControllerRepositories(context.Background(), daemonURL)
			})
		}},
		{"controller-secret", "/daemon/legion/v1/controller/secret: the daemon answered 403", func(t *testing.T) string {
			return controller(t, http.StatusForbidden, func(daemonURL string) error {
				_, _, err := fetchControllerSecret(context.Background(), daemonURL, claimsOperatorToken, 1)
				return err
			})
		}},
		{"controller-credential", "/daemon/legion/v1/controller/github-credential: the daemon answered 500", func(t *testing.T) string {
			return controller(t, http.StatusInternalServerError, func(daemonURL string) error {
				_, _, err := fetchControllerCredential(context.Background(), daemonURL, "capability-secret", "")
				return err
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			printed := c.printed(t)
			oauth2calltest.RequireNoPiece(t, "legion.daemon-url/"+c.name, printed, token)
			if !strings.Contains(printed, c.names) {
				t.Errorf("the line is %q, want it to name %q", printed, c.names)
			}
		})
	}
}
