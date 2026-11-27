package tlstimeout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"
)

// shortHandshake is the TLSHandshakeTimeout of the client transports these tests fail on a peer
// that never answers (tlstimeouttest.Silent), so net/http's own timeout ends their handshake soon.
const shortHandshake = 200 * time.Millisecond

// handshakeTimeout is the error a Go client gets from a peer that takes its connection and never
// answers its ClientHello: net/http's own, wrapped in the *url.Error http.Client returns.
func handshakeTimeout(t *testing.T) error {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: shortHandshake}}
	_, err := client.Get("https://" + tlstimeouttest.Silent(t) + "/")
	if err == nil {
		t.Fatal("a peer that never answers answered")
	}
	return err
}

// A TLS handshake timeout is net/http's own error, `net/http: TLS handshake timeout`, wherever an
// error holds it: as it comes from a transport, inside the *url.Error http.Client wraps it in,
// under a caller's %w, inside errors.Join, and under the proxyconnect error a transport makes of
// one met on the way to an HTTPS proxy; and an error that only says it at the end of its text, as
// gh's line does once it is an error of another program's. Nothing else is one: no error, an HTTP
// status, a name that does not resolve, a refused dial, a context's end, the client's own Timeout,
// an i/o timeout, and the words with more after them.
func TestIsTimeoutIsNetHTTPsOwnHandshakeTimeoutAlone(t *testing.T) {
	t.Parallel()
	timedOut := handshakeTimeout(t)
	var asURL *url.Error
	if !errors.As(timedOut, &asURL) || asURL.Err == nil {
		t.Fatalf("the client's error %v is no *url.Error over the transport's own", timedOut)
	}
	refused := func() error {
		_, err := (&http.Client{Transport: &http.Transport{}}).Get("https://" + testport.Refused(t) + "/")
		return err
	}()
	clientTimeout := func() error {
		client := &http.Client{Timeout: shortHandshake, Transport: &http.Transport{}}
		_, err := client.Get("https://" + tlstimeouttest.Silent(t) + "/")
		return err
	}()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"net/http's own, as http.Client returns it", timedOut, true},
		{"net/http's own, as a transport returns it", asURL.Err, true},
		{"under a caller's %w", fmt.Errorf("renew the lease: %w", timedOut), true},
		{"inside errors.Join", errors.Join(errors.New("a first failure"), timedOut), true},
		{"on the way to an HTTPS proxy", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: asURL.Err}, true},
		{"another program's line", errors.New(`Post "https://api.github.com/graphql": net/http: TLS handshake timeout`), true},
		{"no error", nil, false},
		{"an HTTP 500", errors.New("HTTP 500: Internal Server Error (https://api.github.com/graphql)"), false},
		{"a name that does not resolve", &url.Error{Op: "Get", URL: "https://broker.invalid/", Err: &net.DNSError{Err: "no such host", Name: "broker.invalid", IsNotFound: true}}, false},
		{"a refused dial", refused, false},
		{"a context's deadline", context.DeadlineExceeded, false},
		{"the client's own Timeout", clientTimeout, false},
		{"an i/o timeout", errors.New(`Get "https://api.github.com/": dial tcp 140.82.112.5:443: i/o timeout`), false},
		{"the words with more after them", errors.New(`Get "https://api.github.com/": net/http: TLS handshake timeout (retrying)`), false},
	} {
		if got := IsTimeout(tc.err); got != tc.want {
			t.Errorf("%s: IsTimeout(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if refused == nil || clientTimeout == nil {
		t.Fatalf("the refused dial answered %v and the client's own Timeout %v, want two failures", refused, clientTimeout)
	}
}

// HostInLine reads the host gh's line names, with its port where it has one, and nothing of its
// path, query, fragment or user: the last line of the text of the form `<Method> "<https URL>":
// net/http: TLS handshake timeout`, the line Go's *url.Error prints, which is what gh prints for a
// request whose handshake never finished. Any other line, and a host that is no plain host, are
// none.
func TestHostInLineReadsTheHostOfGhsLineAlone(t *testing.T) {
	t.Parallel()
	line := func(method, address string) string {
		return method + ` "` + address + `": net/http: TLS handshake timeout`
	}
	// The URL with a user is built at run time, so no line of this file reads as a URL that holds
	// a credential to the shared leak check that scans every pushed line.
	withUser := (&url.URL{Scheme: "https", User: url.UserPassword("someone", "invented-password"), Host: "example.com:444", Path: "/v1/secret-path", RawQuery: "token=invented-token", Fragment: "part"}).String()
	for _, tc := range []struct {
		name, text, host string
	}{
		{"gh's REST read", line("Get", "https://api.github.com/repos/acme/widgets"), "api.github.com"},
		{"a GraphQL call to a port", line("Post", "https://127.0.0.1:8443/graphql"), "127.0.0.1:8443"},
		{"ended by a line break", line("Get", "https://api.github.com/user") + "\r\n", "api.github.com"},
		{"the last of several", "warning: one\n" + line("Get", "https://first.example/") + "\n" + line("Post", "https://second.example:444/graphql") + "\nretrying\n", "second.example:444"},
		{"a URL with a user, a path, a query and a fragment", line("Get", withUser), "example.com:444"},
		{"an IPv6 address", line("Post", "https://[::1]:8443/graphql"), "[::1]:8443"},
		{"no text", "", ""},
		{"gh's DNS failure", "error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com\n", ""},
		{"an http URL", line("Get", "http://api.github.com/"), ""},
		{"another cause", `Get "https://api.github.com/": dial tcp 140.82.112.5:443: i/o timeout`, ""},
		{"on the way to a proxy", `Get "https://api.github.com/": proxyconnect tcp: net/http: TLS handshake timeout`, ""},
		{"more after the words", line("Get", "https://api.github.com/") + " (retrying)", ""},
		{"an unquoted URL", `Get https://api.github.com/: net/http: TLS handshake timeout`, ""},
		{"no method", `"https://api.github.com/": net/http: TLS handshake timeout`, ""},
		{"a method that is no word", line("GET /x", "https://api.github.com/"), ""},
		{"no host", line("Get", "https:///repos/acme/widgets"), ""},
		{"a host that is no plain host", line("Get", `https://api.github.com\u0020x/`), ""},
	} {
		host, ok := HostInLine(tc.text)
		if host != tc.host || ok != (tc.host != "") {
			t.Errorf("%s: HostInLine(%q) = %q, %v; want %q, %v", tc.name, tc.text, host, ok, tc.host, tc.host != "")
		}
	}
}

// roundTripperFunc is a base transport of a test's own.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// idleCloser is a base transport that counts the CloseIdleConnections it is handed.
type idleCloser struct {
	roundTripperFunc
	closed *int
}

func (c idleCloser) CloseIdleConnections() { *c.closed++ }

// netHTTPsWords is an error whose whole text is net/http's own words for a handshake timeout, as a
// base transport of a test's own returns one.
type netHTTPsWords struct{}

func (netHTTPsWords) Error() string { return "net/http: TLS handshake timeout" }

// quickRecorder is a recorder whose record ends at once: its curl a path with no file, so no curl
// runs, and its stamps in a directory of the test's own.
func quickRecorder(t *testing.T, stderr io.Writer) *Recorder {
	t.Helper()
	dir := t.TempDir()
	return &Recorder{Client: "agent-secrets", Stderr: stderr, Curl: dir + "/no-curl", StampDir: dir + "/stamps", Bound: time.Second}
}

// Check c-tls-timeout-recorded, point 5: Transport hands the caller the base's own answer, the
// very response and the very error, whether or not it records. A real handshake timeout is
// recorded and returned as the base returned it; the base's success, its failure that is no
// handshake timeout and an answer that is both are returned untouched, with no record; a nil base
// is http.DefaultTransport; and CloseIdleConnections reaches the base.
func TestTransportReturnsTheBasesOwnAnswerUnchanged(t *testing.T) {
	t.Parallel()
	request := func(t *testing.T, address string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	t.Run("a real handshake timeout", func(t *testing.T) {
		t.Parallel()
		var stderr logtest.Buffer
		real := &http.Transport{TLSHandshakeTimeout: shortHandshake}
		var gotResponse *http.Response
		var gotErr error
		base := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotResponse, gotErr = real.RoundTrip(r)
			return gotResponse, gotErr
		})
		silent := tlstimeouttest.Silent(t)
		response, err := Transport(base, quickRecorder(t, &stderr)).RoundTrip(request(t, "https://"+silent+"/"))
		if response != gotResponse || err != gotErr || !IsTimeout(err) {
			t.Errorf("Transport answered %v, %v; want the base's own %v, %v, a handshake timeout", response, err, gotResponse, gotErr)
		}
		if lines := strings.Count(stderr.String(), "agent-secrets: tls handshake timeout recorded: host="+silent+" "); lines != 1 {
			t.Errorf("the timeout wrote %d record lines, want 1:\n%s", lines, stderr.String())
		}
	})
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
		records  bool
	}{
		{name: "a handshake timeout of the base's own", err: netHTTPsWords{}, records: true},
		{name: "a success", response: &http.Response{StatusCode: http.StatusOK}},
		{name: "a failure that is no handshake timeout", err: errors.New("connection reset by peer")},
		{name: "a response and a handshake timeout at once", response: &http.Response{StatusCode: http.StatusBadGateway}, err: netHTTPsWords{}, records: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr logtest.Buffer
			base := roundTripperFunc(func(*http.Request) (*http.Response, error) { return tc.response, tc.err })
			response, err := Transport(base, quickRecorder(t, &stderr)).RoundTrip(request(t, "https://"+testport.Refused(t)+"/"))
			if response != tc.response || err != tc.err {
				t.Errorf("Transport answered %v, %v; want the base's own %v, %v", response, err, tc.response, tc.err)
			}
			if recorded := strings.Contains(stderr.String(), "tls handshake timeout recorded"); recorded != tc.records {
				t.Errorf("Transport recorded %v, want %v:\n%s", recorded, tc.records, stderr.String())
			}
		})
	}
	t.Run("a nil base", func(t *testing.T) {
		t.Parallel()
		var stderr logtest.Buffer
		// http.DefaultTransport dials the refused port and answers its refusal.
		response, err := Transport(nil, quickRecorder(t, &stderr)).RoundTrip(request(t, "https://"+testport.Refused(t)+"/"))
		var dial *net.OpError
		if response != nil || !errors.As(err, &dial) || dial.Op != "dial" {
			t.Errorf("Transport(nil) answered %v, %v; want http.DefaultTransport's refused dial", response, err)
		}
		if stderr.Len() != 0 {
			t.Errorf("a refused dial was recorded:\n%s", stderr.String())
		}
	})
	t.Run("CloseIdleConnections", func(t *testing.T) {
		t.Parallel()
		closed := 0
		transport := Transport(idleCloser{closed: &closed}, nil)
		closer, ok := transport.(interface{ CloseIdleConnections() })
		if !ok {
			t.Fatalf("Transport is a %T, which has no CloseIdleConnections", transport)
		}
		closer.CloseIdleConnections()
		if closed != 1 {
			t.Errorf("the base was asked to close its idle connections %d times, want 1", closed)
		}
	})
}

// Check c-tls-timeout-recorded, point 4: a call that ends any other way than in a handshake
// timeout writes no line, keeps no stamp and runs no curl: a success, an HTTP 500, a name that
// does not resolve (a .invalid host, through a resolver of the test's own that sends nothing), a
// refused dial, and a context's end while the handshake waits.
func TestNoRecordButForAHandshakeTimeout(t *testing.T) {
	t.Parallel()
	serving := func(status int) (string, http.RoundTripper) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		t.Cleanup(server.Close)
		return server.URL + "/", server.Client().Transport
	}
	succeeds, trustsIt := serving(http.StatusOK)
	fails, trustsThat := serving(http.StatusInternalServerError)
	noResolver := &http.Transport{DialContext: (&net.Dialer{Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("this test's resolver reaches no server")
		},
	}}).DialContext}
	for _, tc := range []struct {
		name, address string
		base          http.RoundTripper
		bound         time.Duration
		status        int
	}{
		{name: "a success", address: succeeds, base: trustsIt, status: http.StatusOK},
		{name: "an HTTP 500", address: fails, base: trustsThat, status: http.StatusInternalServerError},
		{name: "a name that does not resolve", address: "https://broker.invalid/", base: noResolver},
		{name: "a refused dial", address: "https://" + testport.Refused(t) + "/", base: &http.Transport{}},
		{name: "a context's end while the handshake waits", address: "https://" + tlstimeouttest.Silent(t) + "/", base: &http.Transport{}, bound: shortHandshake},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr logtest.Buffer
			curl, argv, _ := curlStandIn(t, "exit 0\n")
			recorder := quickRecorder(t, &stderr)
			recorder.Curl = curl
			ctx := context.Background()
			if tc.bound > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.bound)
				t.Cleanup(cancel)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, tc.address, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := Transport(tc.base, recorder).RoundTrip(req)
			switch {
			case tc.status != 0 && (err != nil || response.StatusCode != tc.status):
				t.Errorf("the call answered %v, %v; want HTTP %d", response, err, tc.status)
			case tc.status == 0 && err == nil:
				t.Errorf("the call answered %v, want a failure", response)
			}
			if response != nil {
				_ = response.Body.Close()
			}
			if stderr.Len() != 0 {
				t.Errorf("the call wrote %q, want nothing", stderr.String())
			}
			if _, err := os.Stat(recorder.StampDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the call left its stamp directory (%v), want none made", err)
			}
			if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("curl ran (%v), want no curl", err)
			}
		})
	}
}
