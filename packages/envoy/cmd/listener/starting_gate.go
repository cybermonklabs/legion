package main

// starting_gate.go is the gate a route waits behind until its dependencies are open, /v1 as main
// mounts it behind that gate (v1API), and /readyz, which reads that gate and the bus.

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/logging"
)

// startingGate answers 503 "service starting" until open hands it the mux to serve, and logs each
// request it refuses: that line is the listener's only record of a caller it turned away while it
// started. main builds each gate's mux only once every dependency of its handlers is open, so a
// request never reaches a handler over a dependency that is not there. The two list reads, GET
// /v1/sessions and GET /v1/interests/, get the refilling answer the routes give while their copies
// cannot be served (writeListRefilling), since before /v1 opens no copy has been read: a caller
// reads one answer for "the list is not ready" whether the gate or the route gives it.
type startingGate struct {
	name   string
	logger *logging.Logger
	mux    atomic.Pointer[http.ServeMux]
}

// newStartingGate returns a closed gate whose refusals log under name.
func newStartingGate(name string, logger *logging.Logger) *startingGate {
	return &startingGate{name: name, logger: logger}
}

func (g *startingGate) open(mux *http.ServeMux) { g.mux.Store(mux) }

// route names the /v1 route r reaches, envoy_listener_requests_total's path label: the pattern of
// the route that serves it ((*http.ServeMux).Handler, which routes as ServeHTTP does and changes
// nothing on r), and for a path no route matches, the pattern that routed it to the gate (/v1 or
// /v1/). It is always a pattern and never the path, so a session id, or any other text a caller
// writes into a path, never becomes a series of its own. Before the gate opens, v1API refuses a
// request before apiAuth counts it.
func (g *startingGate) route(r *http.Request) string {
	if mux := g.mux.Load(); mux != nil {
		if _, pattern := mux.Handler(r); pattern != "" {
			return pattern
		}
	}
	return r.Pattern
}

func (g *startingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := g.mux.Load()
	if mux == nil {
		g.refuse(w, r)
		return
	}
	mux.ServeHTTP(w, r)
}

// refuse is the closed gate's answer to r, and the log line that records it.
func (g *startingGate) refuse(w http.ResponseWriter, r *http.Request) {
	g.logger.Warn("request refused while starting",
		slog.String("gate", g.name),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
	if isListRead(r) {
		writeListRefilling(w)
		return
	}
	writeJSONError(w, http.StatusServiceUnavailable, "service starting")
}

// ahead is next behind the gate's own answer: while the gate is closed, a request gets the closed
// gate's 503 before next reads anything of it, and once the gate is open, next serves every
// request. It is what keeps a starting listener from spending a request's DPoP proof: in front of
// apiAuth (v1API), the 503 a closed /v1 answers comes before any credential is judged, so the one
// retry Google's load balancer sends of a body-less request after a 503, with the same proof, is
// not refused as a replay (TestEdgeRetryOfAProofAfterAPreAuth503IsServed). A request it refuses
// is admitted by no credential, so envoy_listener_requests_total counts none of them; the gate's
// own log line records each.
func (g *startingGate) ahead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.mux.Load() == nil {
			g.refuse(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isListRead reports whether r is one of the two list reads, GET /v1/sessions or GET
// /v1/interests/, whose closed-gate answer is the refilling one.
func isListRead(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/v1/sessions" || r.URL.Path == "/v1/interests/")
}

// v1API is /v1 as main mounts it: the closed gate's answer first (startingGate.ahead), then apiAuth
// in front of the gate's routes.
func v1API(auth listenerAPIAuth, gate *startingGate) http.Handler {
	return gate.ahead(apiAuth(auth, gate))
}

// readyzHandler is the listener's /readyz. `GET /readyz` answers whether /v1 can serve, and a load
// balancer's health check reads it: 503 {"status":"starting"} until the /v1 gate opens and its
// dependencies are published to served (openV1Routes); after that, 503
// {"status":"unavailable","error":"nats unavailable"} while the bus does not answer, the check
// that turns /healthz 503 (busAnswers, within healthzBudget), since /v1's routes then answer
// bus-unavailable 503s; and 200 {"status":"serving"} otherwise, again as soon as the bus answers.
// It takes no credential and reads no store: the gate, and one core PING on the listener's
// connection.
//
// The balancer reads this path because the pod's readiness does not take a replica out of its
// backends. GKE's standalone NEGs keep a NOT-READY endpoint: ingress-gce's
// EndpointsDataFromEndpointSlices drops only terminating endpoints and records Ready without
// reading it (pkg/neg/types/types.go:394-417 at ba5195c), and toZoneNetworkEndpointMap adds every
// address with no Ready check (pkg/neg/syncers/utils.go:254). So a replica whose /healthz answers
// 503 for a cut bus stays in the NEG, and only this answer stops the balancer sending it /v1
// requests. /healthz does not serve here: it answers 200 "starting" from the listener's first
// answer (healthzHandler), since the probes that read it must not take a starting listener out of
// service.
func readyzHandler(gate *startingGate, served *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := served.Load()
		if gate.mux.Load() == nil || d == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), healthzBudget)
		defer cancel()
		if err := busAnswers(ctx, d); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "nats unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "serving"})
	}
}
