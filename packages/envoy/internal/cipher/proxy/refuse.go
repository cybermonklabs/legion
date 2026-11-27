// packages/envoy/internal/cipher/proxy/refuse.go
// How a refusal of a grant read is answered: the 407 that asks a command for its credential
// again, the one rule a failed read of a grant is answered by (grantRefusal), and the codes of
// those answers. The writer of every refusal of this port is refuse (bound.go).
package proxy

import (
	"net/http"
)

// refuseCredential answers 407 and asks for the credential again (refuse, with the challenge a
// client sends its credential again on). Nothing is tunnelled and nothing is dialled: a request
// that reaches here never left the broker.
func (s *Server) refuseCredential(w http.ResponseWriter, r *http.Request, c call, code, why string) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="airlock"`)
	s.refuse(w, r, c, http.StatusProxyAuthRequired, code, why)
}

// refuseRead answers a proxy request whose grant read failed, by the one rule for a failed read
// (grantRefusal). A 407 is the broker's own words for why this credential opens nothing - an
// unknown or ended grant, an enrollment whose lease lapsed, a grant every one of whose names is
// withheld - and never the placeholder itself, which a command's own proxy error line would
// otherwise print and keep. A 503 is a read that did not happen, which grantRefusal has logged: a
// failure of the broker's, and no refusal of the caller. Any other status is the broker's own
// refusal of this grant, in its own words.
func (s *Server) refuseRead(w http.ResponseWriter, r *http.Request, c call, read string, err error) {
	status, code, why := s.grantRefusal(read, err)
	switch status {
	case http.StatusProxyAuthRequired:
		s.refuseCredential(w, r, c, code, why)
	case http.StatusServiceUnavailable:
		fail(w, http.StatusServiceUnavailable, code, why)
	default:
		s.refuse(w, r, c, status, code, why)
	}
}
