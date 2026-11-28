package api

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestEveryRouteResolvesItsViewer (plan D3, R3): every route of the table runs under the viewer of
// the request's own caller, which one layer of the chain register builds for every row, withViewer,
// resolves once. The server is mounted on a recording mux, which keeps every pattern register
// mounts, and each row of the table among them is served one request, GET and writes alike, as a
// signed-in person, naming keys nobody holds and carrying an empty body. withViewer runs before
// the id prefixes are resolved and before any handler's own refusal, so the server's count of
// viewer resolutions (server.viewerResolutions) must rise by exactly one for each request; the test
// lists each route where it did not. The two websockets mounted beside the table resolve their
// viewer at the upgrade (plan D11) and are no row of it.
func TestEveryRouteResolvesItsViewer(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedPeople(t, database, rlsOwner)
	s := newRLSServer(t, database, rlsServerConfig{})
	rows := map[string]bool{}
	for _, route := range s.served.routes() {
		rows[route.Method+" "+route.Pattern] = true
	}
	served := 0
	var missed []string
	for _, pattern := range s.mux.mounted() {
		if !rows[pattern] {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		request := withPerson(httptest.NewRequest(method, routeProbePath(path), nil), rlsOwner)
		before := s.served.viewerResolutions.Load()
		s.serve(t, request)
		if resolved := s.served.viewerResolutions.Load() - before; resolved != 1 {
			missed = append(missed, fmt.Sprintf("%s (%d)", pattern, resolved))
		}
		served++
	}
	if served != len(rows) {
		t.Errorf("served %d rows of the %d the table holds: the recording mux holds a mount of each", served, len(rows))
	}
	if len(missed) > 0 {
		t.Errorf("%d of %d routes did not resolve their viewer exactly once for one request (resolutions in brackets):\n  %s",
			len(missed), served, strings.Join(missed, "\n  "))
	}
}
