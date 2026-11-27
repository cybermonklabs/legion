package natsconn

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/nats-io/nats.go"
)

// A connection's error prints as its class alone: a lookup's, a dial's through a SOCKS5 proxy, a
// certificate's name check and a port net cannot read name the server by its host or port, which
// can be a piece of a credential, and none of it prints. The system's words for a failed call, EOF,
// a time out, a TLS alert's words, the NATS client's own errors and a server's -ERR refusal as the
// client relays it print as they are; the client's TLS error keeps its words before the class
// beneath it. An error the client formatted from what the server sent - its parser's, built as
// nats.go v1.55.0 parser.go:417, :474 and :522 build them, and its CONNECT handshake's (nats.go:3306),
// and a -ERR the client names no refusal for - prints as ConnectionProtocolError, the made-up bytes
// in it unprinted; an error with no class prints as ConnectionUnprinted.
func TestConnectionClass(t *testing.T) {
	const host = "madeUpHostPiece0123456789"
	dial := func(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }
	socks := func(err error) error {
		return &net.OpError{Op: "socks connect", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1055}, Err: err}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "unknown"},
		{"a lookup", dial(&net.DNSError{Err: "no such host", Name: host, IsNotFound: true}), "name resolution failed"},
		{"a port net cannot read", dial(&net.AddrError{Err: "invalid port", Addr: host}), "the server's address is no host and port the system can dial"},
		{"a refused dial", dial(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), "connection refused"},
		{"a reset", dial(syscall.ECONNRESET), "connection reset by peer"},
		{"a deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), "timed out"},
		{"EOF", io.EOF, "EOF"},
		{"a certificate for another host", fmt.Errorf("%w: %w", nats.ErrTLS, x509.HostnameError{Host: host, Certificate: &x509.Certificate{}}), "nats: tls error: TLS failed"},
		{"a TLS alert the server sent", fmt.Errorf("%w: %w", nats.ErrTLS, &net.OpError{Op: "remote error", Err: errors.New("tls: certificate required")}),
			"nats: tls error: remote error: tls: certificate required"},
		{"a TLS error with nothing beneath it to class", nats.ErrTLS, "nats: tls error"},
		{"a proxy's reply", socks(errors.New("unknown error host unreachable")), "the SOCKS5 proxy answered host unreachable"},
		{"a proxy's other failure", socks(errors.New("unknown error " + host)), "the SOCKS5 proxy failed the connection"},
		{"a proxy nothing listens at", socks(dial(syscall.ECONNREFUSED)), "connection refused"},
		{"the NATS client's own error", nats.ErrNoServers, "nats: no servers available for connection"},
		{"a server's -ERR, as the client relays it", errors.New("nats: Authorization Violation"), "nats: Authorization Violation"},
		{"a server's -ERR the client names, as processErr relays it", errors.New("nats: Maximum Control Line Exceeded"), "nats: Maximum Control Line Exceeded"},
		// nats-server's other fixed refusals, each as nats.go relays it and in the server's case.
		{"nats-server's Authentication Timeout", errors.New("nats: Authentication Timeout"), "nats: Authentication Timeout"},
		{"nats-server's Maximum Payload Violation", errors.New("nats: Maximum Payload Violation"), "nats: Maximum Payload Violation"},
		{"nats-server's Failed Account Registration", errors.New("nats: Failed Account Registration"), "nats: Failed Account Registration"},
		{"nats-server's connection throttling refusal", errors.New("nats: Connection throttling is active. Please try again later."),
			"nats: Connection throttling is active. Please try again later."},
		{"nats-server's Secure Connection - TLS Required", errors.New("nats: Secure Connection - TLS Required"), "nats: Secure Connection - TLS Required"},
		{"nats-server's Invalid Subject", errors.New("nats: Invalid Subject"), "nats: Invalid Subject"},
		{"nats-server's Invalid Publish Subject", errors.New("nats: Invalid Publish Subject"), "nats: Invalid Publish Subject"},
		{"the parser's error, quoting the bytes the server sent", fmt.Errorf("nats: Parse Error [%d]: '%s'", 0, "XYZ "+host+"\r\n"), ConnectionProtocolError},
		{"processMsgArgs' error, quoting the MSG line", fmt.Errorf("nats: processMsgArgs Parse Error: '%s'", "legion.x "+host), ConnectionProtocolError},
		{"processHeaderMsgArgs' error, quoting the HMSG line", fmt.Errorf("nats: processHeaderMsgArgs Bad or Missing Sid: '%s'", "legion.x "+host+" 1 2"), ConnectionProtocolError},
		{"the CONNECT handshake's error, quoting the line in place of PONG", fmt.Errorf("nats: expected '%s', got '%s'", "PONG", "XYZ "+host), ConnectionProtocolError},
		{"a server's -ERR the client names no refusal for", errors.New("nats: Something Else " + host), ConnectionProtocolError},
		{"a server's refusal with more after it", errors.New("nats: Authorization Violation " + host), ConnectionProtocolError},
		{"the client's error naming a root certificate file", fmt.Errorf("nats: failed to parse root certificate from %q", "/madeUp/"+host), ConnectionRootCertificate},
		{"the JetStream consumer's asynchronous error", nats.ErrConsumerNotActive, "nats: consumer not active"},
		{"a server's permissions violation, as the client relays it", fmt.Errorf("%w: %s", nats.ErrPermissionViolation, `Permissions Violation for Subscription to "legion.denied"`),
			`nats: permissions violation: Permissions Violation for Subscription to "legion.denied"`},
		{"the client's own error under words of another's", fmt.Errorf("subscribe %s: %w", host, nats.ErrTimeout), "nats: timeout"},
		{"two errors joined behind the client's words", fmt.Errorf("nats: %w: %w", nats.ErrTimeout, errors.New(host)), "nats: timeout"},
		{"an error with no class", errors.New("websocket handshake with " + host + " failed"), ConnectionUnprinted},
		{"an error with no class behind the client's words", fmt.Errorf("nats: %w", errors.New(host)), ConnectionUnprinted},
	} {
		got := ConnectionClass(tc.err)
		if got != tc.want {
			t.Errorf("%s: ConnectionClass = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, host) {
			t.Errorf("%s: ConnectionClass = %q, which names the host", tc.name, got)
		}
	}
	// ConnectionError prints that class, and errors.As still reads the error beneath it.
	failed := ConnectionError(dial(&net.DNSError{Err: "no such host", Name: host, IsNotFound: true}))
	var lookup *net.DNSError
	if failed.Error() != "name resolution failed" || !errors.As(failed, &lookup) {
		t.Errorf("ConnectionError = %q, as a DNS error %t; want its class, and the error beneath it", failed.Error(), errors.As(failed, &lookup))
	}
	if ConnectionError(nil) != nil {
		t.Error("ConnectionError(nil) is not nil")
	}
}

// A line about the server a connection reached names it by its position in the servers it was
// configured with, never by its URL: the client authenticates with a URL's userinfo, a user name
// alone as a token and user:password as a password, and a URL's host can be a piece of a
// credential a comma split. A URL that names no port matches on its host, a blank entry is no
// server, and a server the list does not hold is one the cluster advertised.
func TestServerPosition(t *testing.T) {
	// The URL with a user and password is built here, so no source line holds one.
	withUserinfo := "tls://user-marker-0:" + "pw-marker-1@nats.example.test:4223"
	urls := []string{"nats://127.0.0.1:4222", " ", withUserinfo, "127.0.0.1"}
	for _, tc := range []struct{ connected, want string }{
		{"nats://127.0.0.1:4222", "1 of 4"},
		{withUserinfo, "3 of 4"},
		{"nats://NATS.example.test:4223", "3 of 4"},
		{"nats://127.0.0.1:4224", "4 of 4"},
		{"nats://10.0.0.7:4222", "one the cluster advertised, beside the 4 configured"},
		{"", "one the cluster advertised, beside the 4 configured"},
	} {
		if got := ServerPosition(urls, tc.connected); got != tc.want {
			t.Errorf("ServerPosition(%q) = %q, want %q", tc.connected, got, tc.want)
		}
	}
}
