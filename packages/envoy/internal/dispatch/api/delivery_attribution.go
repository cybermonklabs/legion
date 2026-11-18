package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// getDeliveryAttribution answers what the attribution of the stored pull requests still waits
// on, counted at the request (delivery.ReadAttributionCounts): the pull requests a reconcile pass
// still reads the facts of, the part with no facts, the part whose last read was refused, the
// reading that runs now and the stored keys of another, the moves that wait for their events, and
// the URLs that two issues link in two letter cases. It reads no delivery setting and no GitHub,
// so a deployment with no delivery settings and no App key answers it too.
func (s *server) getDeliveryAttribution(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	counts, err := delivery.ReadAttributionCounts(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, counts)
}
