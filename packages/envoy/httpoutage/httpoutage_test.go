package httpoutage

import (
	"net/http"
	"testing"
)

// An outage is any 5xx, a 408 and a 429, and nothing else: no success, no redirect and no other
// 4xx. The 4xx on either side of 408 and of 429 are listed so a range written one off fails here.
func TestAnOutageIsEvery5xxA408AndA429(t *testing.T) {
	for _, status := range []int{
		http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusHTTPVersionNotSupported, 599,
	} {
		if !Is(status) {
			t.Errorf("Is(%d) = false, want true", status)
		}
	}
	for _, status := range []int{
		0, http.StatusOK, http.StatusNoContent, http.StatusMovedPermanently, http.StatusBadRequest,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusProxyAuthRequired, http.StatusConflict, http.StatusUnprocessableEntity,
		http.StatusPreconditionRequired, 430, 499,
	} {
		if Is(status) {
			t.Errorf("Is(%d) = true, want false", status)
		}
	}
}
