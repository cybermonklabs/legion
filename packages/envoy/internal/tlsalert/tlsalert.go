// Package tlsalert is envoy's one reading of a TLS alert, the far side's or the client's own, at
// any point of the connection: proof that a peer took the connection and answered, as against a
// dial that reached nothing.
//
// The daemon module keeps the same reading in packages/daemon/internal/tlsalert once #4574
// merges; until then its internal/cipher' daemonSideFault reads an alert by a rule of its
// own, the "remote error" Op alone. The two packages are copies, not one package imported twice,
// for the reason packages/envoy/internal/testtmp gives: Go's internal rule keeps each module from
// importing the other's. Whichever lands first, a change to the reading here is a change to the
// daemon's, wherever the daemon reads it.
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
// Its callers word an alert as the peer answering: agent-secrets' peerAnswered (a broker that
// answered but the exchange failed), internal/cipher/secrets' transportFailure (an upstream a mint
// called that answered), and internal/redact's tlsFailure (the class "TLS failed").
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
