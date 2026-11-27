package tlsalert

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/tlsalert/tlsalerttest"
)

// TestInReadsATLSAlertAnywhereInTheTree: In finds the alert a TCP TLS client returns, the far
// side's ("remote error") and its own ("local error"), through net/http's *url.Error, inside a
// joined error that holds a refused dial ahead of it (where errors.As stops at the dial's
// *net.OpError), and QUIC's tls.AlertError. Controls: a refused dial alone, and an error whose
// text alone reads as an alert, are none.
func TestInReadsATLSAlertAnywhereInTheTree(t *testing.T) {
	remote := tlsalerttest.Get(t, tlsalerttest.Remote(t))
	local := tlsalerttest.Get(t, tlsalerttest.Local(t))
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, row := range []struct {
		name string
		err  error
		want bool
	}{
		{"the far side's alert over TCP", remote, true},
		{"the client's own alert over TCP", local, true},
		{"an alert behind a refused dial in a joined error", errors.Join(refused, fmt.Errorf("the second address: %w", remote)), true},
		{"QUIC's tls.AlertError", fmt.Errorf("handshake: %w", tls.AlertError(40)), true},
		{"control: a refused dial alone", refused, false},
		{"control: an alert's text with no alert", errors.New("remote error: tls: handshake failure"), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := In(row.err); got != row.want {
				t.Fatalf("In(%v) = %v, want %v", row.err, got, row.want)
			}
		})
	}
}
