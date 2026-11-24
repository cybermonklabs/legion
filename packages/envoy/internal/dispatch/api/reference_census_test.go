package api

import (
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readerOfCaller is the reader caller's requests resolve to on the seeded server (readerOf).
func readerOfCaller(t *testing.T, fixture fragmentCensusFixture, caller privateCaller) model.Actor {
	t.Helper()
	recorder := httptest.NewRecorder()
	reader, ok := fixture.server.readerOf(recorder, fixture.readerRequest(t, caller))
	if !ok {
		t.Fatalf("resolve %s: %d %s", caller, recorder.Code, recorder.Body.String())
	}
	return reader
}
