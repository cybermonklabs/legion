// Package tlsalert is the daemon's one reading of a TLS alert, the far side's or the client's own,
// at any point of the connection: an answer that ends the connection and that no wait fixes, as
// against a network that is not there yet, which the next attempt can find.
package tlsalert

import (
	"crypto/tls"
	"net"
	"slices"
)

// In reports whether err carries a TLS alert, the far side's or the client's own, at any point of
// the connection, anywhere in its tree. Over TCP, Go's TLS client returns a fatal alert the far side
// sent as a *net.OpError whose Op is "remote error", and one it sent itself, on a record or a
// handshake message of the far side's it could not take, a record that fails to decrypt among
// them, as a *net.OpError whose Op is "local error", each holding crypto/tls's own unexported alert
// type (crypto/tls/conn.go). Over QUIC, crypto/tls returns a tls.AlertError (crypto/tls/alert.go).
// So the alert is read by that Op or that type, never by the error's text. Each error of the tree
// is read, through Unwrap() error and through each error of Unwrap() []error, where errors.As would
// stop at the first *net.OpError: an error that joins what several addresses or attempts answered
// can hold a refused dial ahead of the alert.
//
// Three of its callers keep an alert out of the failures they wait out: internal/store's
// OpenRetryable (the start's attempts to open its store), internal/natsauth's Unreachable (a NATS
// that is not up yet) and internal/dispatch's TransportRefusal (a failure of Dispatch's transport
// no wait fixes). internal/cipher's daemonSideFault still waits an alert out, but on the
// token login's long schedule, a fault on the daemon's own side, in place of the short one of a
// broker out of reach.
func In(err error) bool {
	switch err := err.(type) {
	case *net.OpError:
		if err.Op == "remote error" || err.Op == "local error" {
			return true
		}
	case tls.AlertError:
		return true
	}
	switch err := err.(type) {
	case interface{ Unwrap() error }:
		return In(err.Unwrap())
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(err.Unwrap(), In)
	}
	return false
}
