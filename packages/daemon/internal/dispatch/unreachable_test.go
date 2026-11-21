package dispatch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// Unreachable is the predicate daemon.go's boot gate judges every reconcile failure with: a boot
// reading Dispatch for admission must wait out an outage and refuse a misconfiguration loud.
func TestUnreachableClassifiesEachErrorShape(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"a 503 with no JSON body, the shape a raw HTML error page from in front of Dispatch takes",
			&Error{Status: 503, Message: "<html>503 Service Temporarily Unavailable</html>"}, true},
		{"a codeless 4xx from whatever sits in front of Dispatch, such as a wrong host's own 404 page",
			&Error{Status: 404, Message: "not found"}, true},
		{"408 Request Timeout", &Error{Status: 408, Code: oauth2calltest.Said("TIMEOUT"), Message: "come back later"}, true},
		{"429 Too Many Requests", &Error{Status: 429, Code: oauth2calltest.Said("RATE_LIMITED"), Message: "slow down"}, true},
		{"401, a credential answer PermanentRefusal rides out for a queued write and Unreachable rides out here too",
			&Error{Status: 401, Code: oauth2calltest.Said("UNAUTHORIZED"), Message: "bad token"}, true},
		{"403, the same as 401", &Error{Status: 403, Code: oauth2calltest.Said("FORBIDDEN"), Message: "denied"}, true},
		{"409 PARENT_CONTENDED: a concurrent reparent moved the parent at every attempt and nothing was applied",
			&Error{Status: 409, Code: oauth2calltest.Said(ParentContended), Message: "read the issue and retry"}, true},
		{"a genuine coded 409: any conflict but PARENT_CONTENDED is Dispatch refusing the write",
			&Error{Status: 409, Code: oauth2calltest.Said("ISSUE_CLOSED"), Message: "issue is closed"}, false},
		{"a genuine coded 404: the project or route named is not there, no wait fixes that",
			&Error{Status: 404, Code: oauth2calltest.Said("NOT_FOUND"), Message: "no such project"}, false},
		{"a genuine coded 400", &Error{Status: 400, Code: oauth2calltest.Said("BAD_REQUEST"), Message: "malformed"}, false},
		{"a TransientError wrapping a dial refusal", &TransientError{Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, true},
		{"a decode error on a 2xx response: not Dispatch answering, not a transport failure — a schema mismatch",
			errors.New("decode Dispatch GET /api/v1/issues response: unexpected end of JSON input"), false},
		{"a TLS alert on one read joined with a TransientError of another: the refusal wins over the outage",
			errors.Join(&TransientError{Err: context.DeadlineExceeded}, fmt.Errorf("read Dispatch GET /api/v1/issues/ACME-1/events: %w",
				&net.OpError{Op: "remote error", Err: errors.New("tls: certificate required")})), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Unreachable(testCase.err); got != testCase.want {
				t.Fatalf("Unreachable(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

// transportFailure's five shapes, each reproduced against a real server rather than a synthetic
// error: a certificate the client does not trust, a TLS alert (tlsalert.In reads one on the
// handshake or on a later record alike; this row's ends the handshake), an https:// URL pointed
// at a plain-HTTP port, and an https:// URL pointed at a port that answers in neither TLS nor
// HTTP/1.x all stay loud refusals no wait fixes, each one TransportRefusal reads; a dial the
// network refuses is wrapped as a TransientError, exactly what a real outage produces, so
// Unreachable rides it out and TransportRefusal does not read it. dispatchBase's scheme check
// (internal/config) catches a scheme that is not http or https at load time, but not a correct
// https:// whose port does not serve TLS, which only a real connection attempt reveals.
func TestTransportFailureClassifiesEachRealServerShape(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		client        func(t *testing.T) *HTTPClient
		wantTransient bool
	}{
		{
			name: "a certificate the client does not trust",
			client: func(t *testing.T) *HTTPClient {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("[]"))
				}))
				t.Cleanup(server.Close)
				// The default client trusts no certificate the test server minted for itself.
				return New(server.URL, "test-token")
			},
			wantTransient: false,
		},
		{
			name: "a TLS alert: a front door that demands a client certificate the client does not hold",
			client: func(t *testing.T) *HTTPClient {
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				server.Config.ErrorLog = log.New(io.Discard, "", 0)
				// Pinned to TLS 1.2, where the server's alert ends the handshake itself; at TLS 1.3
				// the server decides once the client's handshake is done, and the client meets the
				// alert at whichever write or read reaches it first.
				server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MaxVersion: tls.VersionTLS12}
				server.StartTLS()
				t.Cleanup(server.Close)
				client := New(server.URL, "test-token")
				// The client trusts the server's certificate, so the alert is all that fails.
				client.httpClient = server.Client()
				return client
			},
			wantTransient: false,
		},
		{
			name: "an https:// URL pointed at a plain-HTTP port",
			client: func(t *testing.T) *HTTPClient {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(server.Close)
				return New("https://"+strings.TrimPrefix(server.URL, "http://"), "test-token")
			},
			wantTransient: false,
		},
		{
			// net/http turns the client's tls.RecordHeaderError into http.ErrSchemeMismatch only
			// where the answer starts "HTTP/" (net/http/client.go). A port that answers in neither
			// TLS nor HTTP/1.x, such as an SSH server, leaves the record error itself, and no wait
			// fixes that either. Python's http.server, which answers the TLS client hello as an
			// HTTP/0.9 request with no status line, gave the real `legion start` the same error:
			// "tls: first record does not look like a TLS handshake".
			name: "an https:// URL pointed at a port that answers in neither TLS nor HTTP/1.x",
			client: func(t *testing.T) *HTTPClient {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				t.Cleanup(func() { listener.Close() })
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						go func() {
							defer conn.Close()
							if _, err := conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")); err == nil {
								_, _ = io.Copy(io.Discard, conn)
							}
						}()
					}
				}()
				return New("https://"+listener.Addr().String(), "test-token")
			},
			wantTransient: false,
		},
		{
			name: "a dial the network refuses",
			client: func(t *testing.T) *HTTPClient {
				reserved, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("reserve a port: %v", err)
				}
				addr := reserved.Addr().String()
				reserved.Close() // nothing answers here now.
				return New("http://"+addr, "test-token")
			},
			wantTransient: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := testCase.client(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.ListIssues(ctx, "ACME", nil)
			if err == nil {
				t.Fatal("ListIssues must fail")
			}
			var transient *TransientError
			if got := errors.As(err, &transient); got != testCase.wantTransient {
				t.Fatalf("errors.As(err, &TransientError{}) = %t, want %t (err: %v)", got, testCase.wantTransient, err)
			}
			if got := Unreachable(err); got != testCase.wantTransient {
				t.Fatalf("Unreachable(%v) = %t, want %t", err, got, testCase.wantTransient)
			}
			if got := TransportRefusal(err); got == testCase.wantTransient {
				t.Fatalf("TransportRefusal(%v) = %t, want %t", err, got, !testCase.wantTransient)
			}
		})
	}
}

// PermanentRefusal reads a 409 by its code: PARENT_CONTENDED applied nothing and is taken on a
// retry, while any other 409 is Dispatch refusing the write itself, so dropping the code half of
// the rule fails here.
func TestPermanentRefusalTellsAConflictByItsCode(t *testing.T) {
	if _, permanent := PermanentRefusal(&Error{Status: 409, Code: oauth2calltest.Said(ParentContended), Message: "retry"}); permanent {
		t.Fatal("409 PARENT_CONTENDED read as permanent")
	}
	if _, permanent := PermanentRefusal(&Error{Status: 409, Code: oauth2calltest.Said("ISSUE_CLOSED"), Message: "issue is closed"}); !permanent {
		t.Fatal("409 ISSUE_CLOSED read as an outage to ride out")
	}
}
