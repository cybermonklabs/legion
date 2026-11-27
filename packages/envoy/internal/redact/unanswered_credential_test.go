package redact

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// The texts the rule prints for a URL with credentials, written out here rather than named, so
// this file builds against a tree that does not hold them.
const (
	credentialMark         = "[a URL with credentials]"
	credentialHostNotASCII = "(the request failed; its error is not printed: it names the host of an address that holds a credential, and that host is not ASCII)"
	credentialAddress      = "(the request failed; its error is not printed: the address holds a credential, and the dialer refused it as no valid host and port)"
)

// credentialHead is a made-up part of a credential that url.Parse reads as a host, holding the
// gate's marker: a password's tail after its unencoded "@", or a credential's head before its "/"
// or "?".
const credentialHead = "q7vn2-" + redacttest.Token + "-4c1e"

// credentialClient is a client that follows no redirect and whose resolver answers no query, so a
// request to a name fails at its lookup, which names the host the request dialled, without the
// machine's DNS.
func credentialClient() *http.Client {
	resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("this test's resolver answers no query")
	}}
	return redirects.Refused(&http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Resolver: resolver}).DialContext}})
}

// unansweredOf is Unanswered of the error of a request to raw, which must get no answer, and
// net/http's own error of it, the twin.
func unansweredOf(t *testing.T, client *http.Client, raw string) (printed, twin string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("%s answered %d: the row needs a request that gets no answer", raw, response.StatusCode)
	}
	var failed *url.Error
	if !errors.As(err, &failed) {
		t.Fatalf("net/http's error %#v holds no *url.Error", err)
	}
	named := Unanswered(err, request, "")
	if !errors.Is(named, err) {
		t.Errorf("Unanswered = %#v, which no longer finds net/http's error", named)
	}
	return named.Error(), err.Error()
}

// TestUnansweredPrintsNoHostOrPortOfAURLWithCredentials: a request to a URL that holds user
// information or an "@" past its host, which gets no answer, is named with the URL's authority as
// the mark of a URL with credentials, and the transport's words with its host and its port as the
// mark, or as fixed words where neither can be found in them. Each row's URL holds a made-up
// credential where url.Parse reads the host: a password's tail after its unencoded "@", ASCII
// (at-tail) or not, dialled by its `xn--` form (at-tail-not-ascii); a credential holding a "/"
// whose head is read as the host and whose digits after its ":" are read as the port
// (digits-as-port, `http://user:4821/<tail>@host`); and one whose digits past the port's range
// the dialer refuses, quoting them (digits-out-of-range). The resolver's or the dialer's own words
// name that host or port, and each row's twin, net/http's own error, holds a run of the
// credential. Captures redact.unanswered-credential/<case>.
func TestUnansweredPrintsNoHostOrPortOfAURLWithCredentials(t *testing.T) {
	const path = "/repos/acme/widgets"
	for _, c := range []struct {
		name, url, want string
		// exact is whether want is the whole text, not its head.
		exact  bool
		secret string
	}{
		{
			name: "at-tail", url: "http://reader:p@" + credentialHead + ".invalid" + path, secret: "p@" + credentialHead + ".invalid",
			want: `Get "http://` + credentialMark + path + `": dial tcp: lookup ` + credentialMark,
		},
		{
			name: "at-tail-not-ascii", url: "http://reader:p@" + credentialHead + "\u00f6rd.invalid" + path, secret: "p@" + credentialHead + "\u00f6rd.invalid",
			want: `Get "http://` + credentialMark + path + `": ` + credentialHostNotASCII, exact: true,
		},
		{
			name: "digits-as-port", url: "http://" + redacttest.User + ":4821/" + credentialHead + "@widgets.invalid" + path, secret: redacttest.User + ":4821/" + credentialHead,
			want: `Get "http://` + credentialMark + `": dial tcp: lookup ` + credentialMark,
		},
		{
			name: "digits-out-of-range", url: "http://reader:48213579/" + credentialHead + "@widgets.invalid" + path, secret: "48213579/" + credentialHead,
			want: `Get "http://` + credentialMark + `": ` + credentialAddress, exact: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			printed, twin := unansweredOf(t, credentialClient(), c.url)
			if redacttest.Piece(twin, c.secret) == "" {
				t.Fatalf("the twin %q holds no run of %q, so the row cannot see a leak", twin, c.secret)
			}
			redacttest.RequireNoPiece(t, "redact.unanswered-credential/"+c.name, printed, c.secret)
			if strings.Contains(strings.ToLower(printed), "xn--") {
				t.Errorf("Unanswered = %q, which names the host's ASCII form", printed)
			}
			if c.exact && printed != c.want || !strings.HasPrefix(printed, c.want) {
				t.Errorf("Unanswered = %q, want %q", printed, c.want)
			}
		})
	}
}

// TestUnansweredPrintsNoPortOfAURLWithCredentials: a request to a URL with a user name and a
// password whose host resolves and whose port nothing listens at fails at its dial, whose words
// name the address and the port, which can be a password's digits: the host and the port print as
// the mark, as written and as the dialer writes the port back, with no leading zeros. Its twin,
// net/http's own error, names the port.
func TestUnansweredPrintsNoPortOfAURLWithCredentials(t *testing.T) {
	dead := redirectstest.Dead(t)
	_, port, err := net.SplitHostPort(dead)
	if err != nil {
		t.Fatal(err)
	}
	const want = `Get "http://` + credentialMark + `/repos/acme/widgets": dial tcp ` + credentialMark + ":" + credentialMark + ": connect: connection refused"
	for name, raw := range map[string]string{
		"as-written":    "http://reader:" + redacttest.Password + "@" + dead + "/repos/acme/widgets",
		"leading-zeros": "http://reader:" + redacttest.Password + "@127.0.0.1:00" + port + "/repos/acme/widgets",
	} {
		t.Run(name, func(t *testing.T) {
			printed, twin := unansweredOf(t, redirects.Refused(&http.Client{}), raw)
			if !strings.Contains(twin, ":"+port+":") {
				t.Fatalf("the twin %q names no port %s, so the row cannot see it printed", twin, port)
			}
			redacttest.RequireNone(t, "redact.unanswered-credential/port-"+name, printed)
			if printed != want || strings.Contains(printed, port) {
				t.Errorf("Unanswered = %q, want %q", printed, want)
			}
		})
	}
}

// TestAskedURLNamesTheAuthorityOfAURLWithCredentialsByTheMark: a line about a request that got an
// answer names the URL it asked as Unanswered does, so a URL that holds user information or an "@"
// past its host is named with its authority as the mark. A user name and a password beside a host
// (user-password-host) print as the mark with the host, which url.Parse reads alike from
// `user:password@host` and from a password holding an "@" and no host after it (`user:p@ssword`),
// so the row's host holds a made-up credential as such a tail would; a credential holding a "?"
// whose head url.Parse reads as the host (past-host) prints as the scheme and the mark alone. Each
// twin, the URL as it came, holds the credential. Captures redact.asked-url-credential/<case>.
func TestAskedURLNamesTheAuthorityOfAURLWithCredentialsByTheMark(t *testing.T) {
	for _, c := range []struct {
		name, url, want string
		secrets         []string
	}{
		{
			name: "user-password-host", url: "https://" + redacttest.User + ":" + redacttest.Password + "@" + credentialHead + ".invalid:8418/widgets/healthz?sig=1",
			want: "https://" + credentialMark + "/widgets/healthz", secrets: []string{redacttest.User, redacttest.Password, credentialHead},
		},
		{
			name: "past-host", url: "https://" + credentialHead + "?" + redacttest.Password + "@cache.invalid/widgets/healthz",
			want: "https://" + credentialMark, secrets: []string{credentialHead, redacttest.Password},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.url)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range c.secrets {
				if redacttest.Piece(u.String(), secret) == "" {
					t.Fatalf("the URL %q holds no run of %q, so the row cannot see a leak", u, secret)
				}
			}
			named := AskedURL(u)
			redacttest.RequireNoPiece(t, "redact.asked-url-credential/"+c.name, named, c.secrets...)
			if named != c.want {
				t.Errorf("AskedURL(%q) = %q, want %q", c.url, named, c.want)
			}
		})
	}
}
