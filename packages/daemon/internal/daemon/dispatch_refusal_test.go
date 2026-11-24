package daemon

import (
	"net/http"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// dispatchRefusal is Dispatch's refusal as the client decodes one (dispatch's dispatchError):
// status, code and sentence, the sentence both as it came (Said), which a decision on it reads,
// and as it prints (Message).
func dispatchRefusal(status int, code, said string) *dispatch.Error {
	return &dispatch.Error{Status: status, Code: oauth2calltest.Said(code), Message: said, Said: oauth2calltest.Said(said)}
}

// noRouteError is a Dispatch router's answer that it has no route for the request
// (dispatch.NoRoute): 404 NOT_FOUND, its sentence starting "no route for ".
func noRouteError(said string) *dispatch.Error {
	return dispatchRefusal(http.StatusNotFound, "NOT_FOUND", said)
}
