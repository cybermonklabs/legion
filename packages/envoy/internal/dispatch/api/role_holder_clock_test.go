package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

func TestRoleHolderLookupUsesTheServersClock(t *testing.T) {
	t.Parallel()
	answered := make(chan struct{}, 1)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/roles/reviewer" {
			http.NotFound(w, r)
			return
		}
		answered <- struct{}{}
		_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s2","title":"reviewer","capabilities":["steer"]}`))
	}))
	t.Cleanup(listener.Close)
	fake := clocktest.Held()
	s := &server{deps: Deps{
		Clock: fake,
		Envoy: envoy.New(listener.URL,
			envoy.WithClock(fake),
			envoy.WithAccountTokenFile(testEnvoyAccountTokenFile(t, listener.URL))),
	}}
	if s.roleHolderMayBe(context.Background(), "reviewer", "s1") {
		t.Fatal("the answered role lookup names s2, so it cannot hold a send to s1")
	}
	select {
	case <-answered:
	default:
		t.Fatal("the role lookup returned without the listener's answer")
	}
}
