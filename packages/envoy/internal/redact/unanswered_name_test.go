package redact

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// askedToken is a credential the URL a request asked carries, which holds the gate's marker.
const askedToken = "q7vn2-" + redacttest.Token + "-4c1e"

// TestUnansweredNamesTheURLItAskedByItsSchemeHostAndPath: where its caller gives no words of its
// own for the call, Unanswered names a request that got no answer by the URL it asked: its
// scheme, host and path, its whole query (a key alone as much as a value) and its fragment
// removed, its authority as URLWithCredentials where it holds user information, and NotPrinted
// where the URL rule refuses what remains. Each row asks an address nothing listens at, so the
// failure is net/http's own text of a refused connection, which names that address alone, and for
// a URL with user information that address's host and port as URLWithCredentials. A clean URL
// with a query (clean-query) prints its scheme, host and path. A URL with a credential in its user
// name, its password, a query value, a query key alone, its fragment, or a path the rule refuses
// (an escape, a ':') prints none of it, nor does one Unanswered reads off net/http's error with no
// request (no-request). Each twin shows the leak is real: net/http's own error holds the
// credential, or for a password, which net/http masks, the URL the request asked does. Captures
// redact.unanswered-name/<case>.
func TestUnansweredNamesTheURLItAskedByItsSchemeHostAndPath(t *testing.T) {
	refusing := redirects.Refused(&http.Client{})
	dead := redirectstest.Dead(t)
	asked := "http://" + dead + "/repos/acme/widgets"
	marked := "http://" + URLWithCredentials + "/repos/acme/widgets"
	for _, c := range []struct {
		name, url, want string
		secrets         []string
		// noRequest is whether Unanswered reads the URL off net/http's error, given no request.
		noRequest bool
		// masked is whether net/http's own error masks the secret (a password), so the twin reads
		// the URL the request asked.
		masked bool
	}{
		{name: "clean", url: asked, want: asked},
		{name: "clean-query", url: asked + "?per_page=100&page=2", want: asked},
		{name: "user", url: "http://" + redacttest.User + "@" + dead + "/repos/acme/widgets", want: marked, secrets: []string{redacttest.User}},
		{name: "password", url: "http://reader:" + redacttest.Password + "@" + dead + "/repos/acme/widgets", want: marked, secrets: []string{redacttest.Password}, masked: true},
		{name: "query-value", url: asked + "?per_page=100&sig=" + askedToken, want: asked, secrets: []string{askedToken}},
		{name: "query-key", url: asked + "?" + askedToken, want: asked, secrets: []string{askedToken}},
		{name: "fragment", url: asked + "#" + askedToken, want: asked, secrets: []string{askedToken}},
		{name: "escaped-path", url: asked + "/%2F" + askedToken, want: NotPrinted, secrets: []string{askedToken}},
		{name: "path-outside-allow-list", url: asked + "/sig:" + askedToken, want: NotPrinted, secrets: []string{askedToken}},
		{name: "no-request", url: "http://" + redacttest.User + "@" + dead + "/repos/acme/widgets?sig=" + askedToken + "#" + askedToken, want: marked, secrets: []string{redacttest.User, askedToken}, noRequest: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := refusing.Do(request)
			if err == nil {
				_ = response.Body.Close()
				t.Fatalf("%s answered %d: the row needs a request that gets no answer", c.url, response.StatusCode)
			}
			var failed *url.Error
			if !errors.As(err, &failed) {
				t.Fatalf("net/http's error %#v holds no *url.Error", err)
			}
			twin := err.Error()
			if c.masked {
				twin = request.URL.String()
			}
			for _, secret := range c.secrets {
				if redacttest.Piece(twin, secret) == "" {
					t.Fatalf("the twin %q holds no run of %q, so the row cannot see a leak", twin, secret)
				}
			}
			named := request
			if c.noRequest {
				named = nil
			}
			printed := Unanswered(err, named, "")
			redacttest.RequireNoPiece(t, "redact.unanswered-name/"+c.name, printed.Error(), c.secrets...)
			failure := failed.Err.Error()
			if c.want == marked {
				failure = "dial tcp " + URLWithCredentials + ":" + URLWithCredentials + ": connect: connection refused"
			}
			if want := "Get " + strconv.Quote(c.want) + ": " + failure; printed.Error() != want {
				t.Errorf("Unanswered = %q, want %q", printed, want)
			}
			if !errors.Is(printed, err) {
				t.Errorf("Unanswered = %#v, which no longer finds net/http's error", printed)
			}
		})
	}
}
