// Package tlsalert is envoy's one reading of a TLS alert, the far side's or the client's own, at
// any point of the connection: proof that a peer took the connection and answered, as against a
// dial that reached nothing.
//
// The daemon module gets the same reading as packages/daemon/internal/tlsalert with pull request
// #4574, which is open as this is written; until it merges, main's daemon reads an alert in
// internal/agentsecrets' daemonSideFault by a rule of its own, the "remote error" Op alone. Once
// both are on main they are copies, not one package imported twice, for the reason
// packages/envoy/internal/testtmp gives: Go's internal rule keeps each module from importing the
// other's. A change to one is then a change to the other.
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
// answered but the exchange failed), internal/broker/secrets' transportFailure (an upstream a mint
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
