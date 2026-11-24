//go:build linux

package helper

import (
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// waitRevokes returns once no revoke goRevoke fired off is in flight: at once when none is, and
// otherwise when the count next falls to zero, read through testwait.Receive so a revoke that never
// returns fails t naming the wait, at testwait's margin before the test binary's deadline. Only
// tests wait for the revokes a path fires off.
func (s *Server) waitRevokes(t *testing.T) {
	t.Helper()
	s.revokesMu.Lock()
	var idle <-chan struct{} = s.revokesIdle
	inFlight := s.revokesInFlight
	s.revokesMu.Unlock()
	if inFlight > 0 {
		testwait.Receive(t, "the revokes the helper fired off to return", idle)
	}
}
