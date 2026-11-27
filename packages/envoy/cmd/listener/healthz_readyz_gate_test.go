package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// /readyz, the path a load balancer's health check reads, takes no credential and answers 503
// "starting" while the /v1 gate is closed, and while it is open but its dependencies are not yet
// published, and 200 "serving" once both are and the bus answers: a starting listener is unhealthy
// to the balancer until /v1 serves, as /healthz answers it 503 "starting" until the pod can serve.
func TestHealthzReadyzAnswers503UntilTheV1GateOpens(t *testing.T) {
	t.Parallel()
	gate := newStartingGate("v1", logging.NewWithWriter("test-machine", io.Discard))
	var served atomic.Pointer[listenerDeps]
	handler := readyzHandler(gate, &served)
	get := func() readyzAnswer {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return readReadyz(t, recorder.Result())
	}
	if got := get(); got != (readyzAnswer{code: http.StatusServiceUnavailable, status: "starting"}) {
		t.Errorf("/readyz while /v1 is closed = %+v, want 503 starting", got)
	}
	gate.open(http.NewServeMux())
	if got := get(); got != (readyzAnswer{code: http.StatusServiceUnavailable, status: "starting"}) {
		t.Errorf("/readyz once the gate is open, before /v1's dependencies are published = %+v, want 503 starting", got)
	}
	client, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	served.Store(&listenerDeps{client: client})
	if got := get(); got != (readyzAnswer{code: http.StatusOK, status: "serving"}) {
		t.Errorf("/readyz once /v1 is open and its bus answers = %+v, want 200 serving", got)
	}
}
