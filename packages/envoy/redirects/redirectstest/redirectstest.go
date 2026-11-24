// Package redirectstest is the stand-ins the redirect rows of the envoy and daemon modules answer
// with: a server in a client's server's place that answers every request with a redirect whose
// Location quotes the credential the request carried, as whoever answers there can, and a target
// that counts what it is asked, or keeps it, which a client that follows no redirect never asks.
// It is also the rows' twins, which show the leak each row keeps out is real: net/http's own
// client, which follows redirects, prints the echo in its error (RequireEcho) or carries the
// credential on to the target (RequireSentOn).
package redirectstest

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Unparsed is a Location net/http cannot parse (an invalid escape) that quotes credential: Go
// parses a redirect's Location before a client's CheckRedirect is asked, so its error for the
// answer quotes this whole Location whatever the client's redirect policy.
func Unparsed(credential string) string { return "/moved%zz" + credential }

// To is a Location sending the client to host's /moved with credential in its query.
func To(host, credential string) string {
	return "http://" + host + "/moved?access_token=" + credential
}

// MovedTo is a Location header line, for oauth2calltest.StatusLineServer, sending the client to
// host, carrying the made-up credentials of oauth2calltest's probe in its user information and its
// query.
func MovedTo(host string) string {
	return "Location: http://not-a-secret-user:not-a-secret-password@" + host + "/moved?access_token=not-a-secret-token\r\n"
}

// Echo serves, until t ends, a stand-in that answers every request 302 Found with the Location
// location makes of the value the request carried in header (its Authorization, say), and
// answers its base URL.
func Echo(t testing.TB, header string, location func(credential string) string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location(r.Header.Get(header)))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// EchoBearer is Echo of the request's Authorization, with location made of the bearer it carried,
// its "Bearer " scheme cut off.
func EchoBearer(t testing.TB, location func(bearer string) string) string {
	t.Helper()
	return Echo(t, "Authorization", func(authorization string) string {
		return location(strings.TrimPrefix(authorization, "Bearer "))
	})
}

// Target serves, until t ends, a stand-in that answers every request 500 and counts it, and
// answers its address (host:port) and asked, the count of requests it has been asked since asked
// was last called, so each row reads its own.
func Target(t testing.TB) (host string, asked func() int64) {
	t.Helper()
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), func() int64 { return count.Swap(0) }
}

// Sent serves, until t ends, a stand-in that answers every request 500 and keeps what each one
// carried: its method, its request URI and every header it was sent, a line for each request. It
// answers its address (host:port) and sent, the text of the requests it has been asked since sent
// was last called, so each row reads its own. A client that follows a redirect To it carries the
// credential on, in the Location's query and in the headers net/http copies onto the redirect's
// request (Authorization among them, on to the same host name), and a row captures it from that
// text; a client that follows no redirect sends it nothing, and sent answers "".
func Sent(t testing.TB) (host string, sent func() string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked strings.Builder
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fmt.Fprintf(&asked, "%s %s %v\n", r.Method, r.RequestURI, r.Header)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		text := asked.String()
		asked.Reset()
		return text
	}
}

// Dead is an address (host:port) nothing listens at: a port just closed.
func Dead(t testing.TB) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// RequireEcho is the twin of the rows that print a request's failure: it sends request, which
// carries credential, to a stand-in that answers it with an echo of credential (an Unparsed
// Location, a To one on a Dead address, a status line no client reads), with net/http's own
// client, which follows redirects, and fails t unless net/http's own error holds credential. A row
// whose output holds none of it has kept the echo out.
func RequireEcho(t testing.TB, request *http.Request, credential string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("net/http answered %s %s with status %d and no error: the rows cannot see one", request.Method, request.URL.Redacted(), response.StatusCode)
	}
	if !strings.Contains(err.Error(), credential) {
		t.Fatalf("net/http's own error holds no echo of the credential, so the rows cannot see a leak: %q", err)
	}
}

// RequireSentOn is the twin of the rows that capture what Sent's stand-in was sent: it sends
// request, which carries credential, to a stand-in that answers it with a redirect To Sent's
// stand-in, with net/http's own client, which follows it, and fails t unless the client is
// answered and sent (Sent's) then holds credential. A row whose stand-in is sent none of it has
// kept the credential from going on.
func RequireSentOn(t testing.TB, request *http.Request, credential string, sent func() string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("a client that follows the redirect of %s %s failed: %v", request.Method, request.URL.Redacted(), err)
	}
	_ = response.Body.Close()
	if text := sent(); !strings.Contains(text, credential) {
		t.Fatalf("a client that follows the redirect of %s %s sent the stand-in no credential, so the rows cannot see one sent on: %q",
			request.Method, request.URL.Redacted(), text)
	}
}
