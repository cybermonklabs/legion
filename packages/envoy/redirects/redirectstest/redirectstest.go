// Package redirectstest is the stand-ins the redirect rows of the envoy and daemon modules answer
// with: a server in a client's server's place that answers every request with a redirect whose
// Location quotes the credential the request carried, as whoever answers there can, and a target
// that counts what it is asked, or keeps it, which a client that follows no redirect never asks.
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
