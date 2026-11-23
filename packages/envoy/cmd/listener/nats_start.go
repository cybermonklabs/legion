package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
)

// listenerCredentialWait is how long the listener's start waits out a refusal of its NATS
// credential (bus.WithCredentialWait), counted from its first try to connect, across its connects:
// the main client's and the stream's reconcile on it, then the role lane's and the session
// consumers' (natsStart.option). main passes it to runListener, which makes the start with it
// (newNATSStart). A server refuses a new nkey until it reloads a configuration
// that names it, and a hub's servers reload only once the kubelet has refreshed each one's mounted
// configuration. On 2026-11-22 the last of a hub's servers reloaded a session listener's new nkey
// 64 s after its configuration changed and 71.4 s after the listener's Deployment was created, and
// the listener, which then exited at the first refusal, exited three times meanwhile. 180 s holds
// that reading two and a half times over, and an estimate of a kubelet refresh of up to 90 s per
// server, through one Service address, with about 70 s to spare. It is a chosen tolerance, not a
// proved bound. While the start waits, /livez answers 204 and /healthz 503 (registerProbeRoutes),
// so a Deployment's probes must not restart the listener inside it: one whose startup and liveness
// probes read /livez restarts none.
const listenerCredentialWait = 180 * time.Second

// natsStart is the listener's start on NATS as its probe routes report it: its connects, the main
// client's (Phase 5, connectListenerBus), an owning client's or, on an edge listener, the client
// bus.Connect returns, then the role lane's (startRoleLane) and the session-consumer connection's
// (startSessionDelivery), each on a connection of its own and each ending the start when it fails.
// wait is the credential wait of the whole start, counted from began, the first connect's first
// try (option), and refusal the last refusal of the credential a connect is waiting out: nil
// before the first refusal and from the moment that connect gets in (admitted), whatever the step
// it belongs to does after.
type natsStart struct {
	wait    time.Duration
	once    sync.Once
	began   time.Time
	refusal atomic.Pointer[string]
}

// newNATSStart is a start that waits out a refusal of the credential for up to wait.
func newNATSStart(wait time.Duration) *natsStart { return &natsStart{wait: wait} }

// option is the option each connect of the start takes: what is left of the start's wait, counted
// from the first connect's first try, which the first call marks, handing each refusal to refused
// and the connect's getting in to admitted as the bus meets them (bus.WithCredentialWait). Behind a
// hub whose servers reload a new nkey one at a time, the main client can get in through the one
// server that has reloaded while the role lane or the session-consumer connection meets one that
// has not, so each waits out that refusal under the same bound. A connect made once the bound has
// passed gets no wait (a bound of zero or less): it tries ten times, a second apart, whatever the
// error, as every connect did before the wait. A dial that fails for another reason than a refusal
// gets those ten tries whatever is left of the bound.
func (s *natsStart) option() bus.ConnectOption {
	s.once.Do(func() { s.began = time.Now() })
	return bus.WithCredentialWait(s.wait-time.Since(s.began), s.refused, s.admitted)
}

// refused records err as the last refusal of the credential.
func (s *natsStart) refused(err error) {
	text := err.Error()
	s.refusal.Store(&text)
}

// admitted forgets the refusals of a connect that has just got in: the server took the credential,
// so /healthz answers as it does for a start that met none, and a step that fails after its connect
// (a subscribe, a flush, a check of the server's version) names no credential, until a later
// connect meets a refusal.
func (s *natsStart) admitted() { s.refusal.Store(nil) }

// refusedCredential is the attribute a line that ends the start adds when the start met a refusal
// of its credential that no connect has got in past since (admitted): nats_credential, the
// credential the environment names (bus.CredentialName), an nkey user by its public key. A start
// whose connects met no refusal, or got in past it, adds none, since its credential is not what
// ended it.
func (s *natsStart) refusedCredential(environment func(string) (string, bool)) []slog.Attr {
	if s.refusal.Load() == nil {
		return nil
	}
	return []slog.Attr{slog.String("nats_credential", bus.CredentialName(environment))}
}

// stepFailed writes the line that ends the start when a step after the main client's failed with
// err, the role lane's (startRoleLane) or the session-consumer connection's (startSessionDelivery):
// an ERROR of message on logger naming err and, when the start met a refusal of its credential that
// no connect has got past since, the credential (refusedCredential). main then exits 1.
func (s *natsStart) stepFailed(logger *logging.Logger, message string, err error, environment func(string) (string, bool)) {
	logger.Error(message, append([]slog.Attr{slog.String("error", err.Error())}, s.refusedCredential(environment)...)...)
}

// startFailureLine is the last line of a start on NATS that ended with err (Phase 5,
// connectListenerBus): err names the step that failed, and, when the start met a refusal of its
// credential that its connect did not get in past, the line ends naming that credential
// (bus.CredentialName), an nkey user by its public key, which an operator looks for in the hub's
// authorization. A start that met no refusal names none, since the credential is not what ended it.
func startFailureLine(err error, start *natsStart, environment func(string) (string, bool)) string {
	line := fmt.Sprintf("connect to NATS and reconcile the %s stream: %v", bus.Stream, err)
	if start.refusal.Load() != nil {
		line += "; the NATS credential: " + bus.CredentialName(environment)
	}
	return line
}

// startSessionDelivery opens the listener's session-consumer connection (connectSessionDelivery)
// as a connect of the start: on cfg's servers, with inactiveThreshold and the listener's main
// client, stream, and, on an edge listener, its inbox prefix (edgeGrant.connectOptions), waiting out
// a refusal of the credential under the start's bound (natsStart.option), which /healthz reports
// while it lasts, and forgotten once its connect gets in, before the check of the server's version
// that follows it.
func startSessionDelivery(cfg config.Service, inactiveThreshold time.Duration, stream *bus.Client, logger *logging.Logger, edge *edgeGrant, start *natsStart) (*sessionDelivery, error) {
	return connectSessionDelivery(cfg.NATSURLs, inactiveThreshold, stream, logger, append(edge.connectOptions(), start.option())...)
}

// healthz is next, answering /healthz, behind the start's own answer while it waits out a refusal
// of its credential: 503, "unhealthy", with an error naming the last refusal and, as every answer
// does, the fields issuers adds (writeHealth). A readiness probe that runs curl -f against /healthz
// so keeps a listener whose credential the hub refuses out of its Service, which a 200 "starting"
// would not. Before the first refusal and once the connect that met it has got in (admitted), next
// answers.
func (s *natsStart) healthz(next http.Handler, issuers func() map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusal := s.refusal.Load()
		if refusal == nil {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeHealth(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "error": "nats credential refused: " + *refusal}, issuers)
	})
}

// registerProbeRoutes registers the two routes a Deployment probes the listener on, which main
// serves from Phase 4 on, before any NATS work, and which the tests of the start serve the same
// way.
//
// /healthz is always reachable, before NATS setup as well as after it: start's answer while the
// start waits out a refusal of the credential (natsStart.healthz), and healthzHandler's otherwise,
// over deps. Every answer carries what issuers adds, the identity token issuers' fields
// (issuerHealthFields), which never decide its status.
//
// /livez answers 204 and touches nothing: no flush to NATS, no cache ping and no read of the
// durable consumer. It says only that the process answers, for the startup and liveness probes,
// which must not wait on or fail with NATS: a listener whose bus is down repairs itself, a start
// whose credential is refused waits for the hub to admit it, and a restart would only drop its
// warm caches. /healthz stays the readiness route, the one that reads the bus, because its HTTP
// status is what tells a starting pod from a serving one.
func registerProbeRoutes(mux *http.ServeMux, deps *atomic.Pointer[listenerDeps], start *natsStart, issuers func() map[string]any) {
	mux.Handle("/healthz", start.healthz(healthzHandler(deps, issuers), issuers))
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}
