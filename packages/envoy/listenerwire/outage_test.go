package listenerwire_test

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// A listener outage is every status httpoutage.Is names but FaultStatus, the listener's own
// lasting 500, and nothing else: no success, no refusal of the request, and not the 500.
func TestAListenerOutageIsEveryOutageButFaultStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusNotImplemented,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusHTTPVersionNotSupported, 599,
	} {
		if !listenerwire.Outage(status) {
			t.Errorf("Outage(%d) = false, want true", status)
		}
	}
	for _, status := range []int{
		listenerwire.FaultStatus, http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
	} {
		if listenerwire.Outage(status) {
			t.Errorf("Outage(%d) = true, want false", status)
		}
	}
}
