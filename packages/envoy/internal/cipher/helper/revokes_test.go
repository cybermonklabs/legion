//go:build linux

package helper

// waitRevokes returns once no revoke goRevoke fired off is in flight: at once when none is, and
// otherwise when the count next falls to zero. Only tests wait for the revokes a path fires off.
func (s *Server) waitRevokes() {
	s.revokesMu.Lock()
	idle := s.revokesIdle
	inFlight := s.revokesInFlight
	s.revokesMu.Unlock()
	if inFlight > 0 {
		<-idle
	}
}
