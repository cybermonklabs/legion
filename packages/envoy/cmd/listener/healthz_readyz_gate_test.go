package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
)

// /readyz, the path a load balancer's health check reads, takes no credential and answers 503
// "starting" while the /v1 gate is closed and 200 "serving" once it is open: a starting listener
// is unhealthy to the balancer until /v1 serves, where /healthz answers it 200 "starting" from its
// first answer.
func TestHealthzReadyzAnswers503UntilTheV1GateOpens(t *testing.T) {
	gate := newStartingGate("v1", logging.NewWithWriter("test-machine", io.Discard))
	handler := readyzHandler(gate)
	get := func() readyzAnswer {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return readReadyz(t, recorder.Result())
	}
	if got := get(); got != (readyzAnswer{code: http.StatusServiceUnavailable, status: "starting"}) {
		t.Errorf("/readyz while /v1 is closed = %+v, want 503 starting", got)
	}
	gate.open(http.NewServeMux())
	if got := get(); got != (readyzAnswer{code: http.StatusOK, status: "serving"}) {
		t.Errorf("/readyz once /v1 is open = %+v, want 200 serving", got)
	}
}
