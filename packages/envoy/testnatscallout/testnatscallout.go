// Package testnatscallout is a test fixture: a hub that puts the real NATS auth service
// (internal/cipher/natscallout, deciding with its own Decide and answering with its own grants) in
// front of an in-process nats-server (internal/testnats), for a module outside
// github.com/sjawhar/envoy (packages/daemon's tests of the connection its service token makes),
// which Go's internal-package rule keeps from importing either. It is natscred's rig
// (natscred/rig_test.go, startRig) made public: the hub is configured as the production hub renders
// it (infra/core's nats_callout.go, AuthCallout: config mode, no accounts block, so the service
// places every client it admits in the global account, $G, beside the static users), and a test
// owns only the services the service lists, in BROKER_NATS_SERVICES' form, and the verifier of a
// service's platform token. Everything else the service decides as the broker does.
//
// It is a package of its own rather than part of testnatsinprocess because of what it links. Seven
// of the daemon's test packages link testnatsinprocess (through packages/daemon/internal/testnats),
// and packages/envoy/scripts/forced-order.sh builds it with the odatrace tag, while natscallout's
// import closure is the broker's (authelia's oauth2 provider, the AWS SDK, GCP Secret Manager,
// go-oidc): only the tests that need the auth service import this package, and with it that
// closure. internal/testnats stays the one place the server is made, and natscallout the one place
// a connection is decided.
package testnatscallout

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Verifier is the test's own verifier of a service's platform identity token, in the broker's
// place (natscallout.Deps.ServiceToken): it answers the account the raw token names, whether that
// is a Google account, and when the token was issued and expires, or the error that refuses the
// token, which the service counts under service_token_not_verified (under budget instead when ctx,
// which carries the decision's budget, ended first). The service holds the answer to its own rules
// after it, as the broker's verifier's answer is held: a token that lives longer than an hour is
// refused (token_too_long), one whose expiry has passed (token_not_live), and an account no listed
// service names (service_not_listed). It is called on the service's goroutines, once per CONNECT
// that presents a service's user and password, the keeper's probe dials among them, and may be
// called by several at once.
type Verifier func(ctx context.Context, raw string) (account string, google bool, issued, expiry time.Time, err error)

// Hub is a NATS server running in the test process with the real auth service in front of it
// (Start): every client that is not one of the server's two static users is admitted, or refused,
// by the service, as the deployed hub's clients are.
type Hub struct {
	url    string
	server *testnats.Server
	// adminKey and adminSign are the admin's public key and the signer of its nonce (Admin);
	// serviceKey is the public key of the auth service's own user, which the server admits by its
	// configuration as it admits the admin.
	adminKey, serviceKey string
	adminSign            nats.SignatureHandler
	// log is the service's log, which its reporter writes while a test reads it, and registry
	// counts its decisions (natscallout.NewMetrics), which a test reads to see why a connection was
	// refused: the log has only one line a minute of them.
	log      *logtest.Buffer
	registry *metrics.Registry
	// settings and deps are the service's, which StartService starts again after a StopService.
	settings natscallout.Settings
	deps     natscallout.Deps

	// mu guards the running service, nil once StopService stopped it, and the tokens the
	// verifier was given, which the service's goroutines append to while a test reads them.
	mu      sync.Mutex
	service *natscallout.Service
	tokens  []string
}

// Start runs a hub in the test process: callout keys of its own (testnats.NewCalloutKeys), a
// nats-server with JetStream configured with them (testnats.StartWithCalloutServer) that admits two
// users by its own configuration, the auth service's and an admin granted every subject both ways
// (Admin), and sends every other client to the service; and the service itself (natscallout.Start)
// on the three seeds, each written 0600 under t.TempDir(), listing services, which takes
// BROKER_NATS_SERVICES' form (config.ParseNATSServices: for example
// "daemon/-/*=system:serviceaccount:example-ns:example-daemon"), and verifying each platform token
// with verify, after it records the token (Tokens). The service's log and decision counter are the
// hub's own (ServiceLog, Refused). Start returns once the service's subscription has reached the
// server (natscallout.Service.Ready), waiting as long as the test may run (testnats.Context), and
// the service and the server are closed when the test ends.
//
// The service's Decide and its grants are natscallout's own and cannot be replaced: what a client
// is admitted to is what the deployed hub admits it to. The broker behind the service introspects
// no token, so a test presents it no session's credential (a token beside a proof, with no user).
func Start(t testing.TB, services string, verify Verifier) *Hub {
	t.Helper()
	if verify == nil {
		t.Fatal("testnatscallout: a hub verifies each service token with the test's Verifier, and Start was given none")
	}
	listed, err := config.ParseNATSServices(services)
	if err != nil {
		t.Fatalf("testnatscallout: read the services the hub lists: %v", err)
	}
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	admin, err := nkeys.FromSeed([]byte(adminSeed))
	if err != nil {
		t.Fatalf("testnatscallout: read the admin's seed: %v", err)
	}
	everything := []string{">"}
	server, url := testnats.StartWithCalloutServer(t, keys,
		testnats.StaticUser{NKey: adminKey, Publish: everything, Subscribe: everything})
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("testnatscallout: write the auth service's %s: %v", name, err)
		}
		return path
	}
	log := &logtest.Buffer{}
	registry := metrics.New()
	h := &Hub{
		url: url, server: server,
		adminKey: adminKey, serviceKey: keys.ServicePublic, adminSign: admin.Sign,
		log: log, registry: registry,
		settings: natscallout.Settings{
			URLs:     []string{url},
			SeedFile: write("service.seed", keys.ServiceSeed), IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
			XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: listed,
		},
	}
	h.deps = natscallout.Deps{
		ServiceToken: func(ctx context.Context, raw string) (string, bool, time.Time, time.Time, error) {
			h.mu.Lock()
			h.tokens = append(h.tokens, raw)
			h.mu.Unlock()
			return verify(ctx, raw)
		},
		Log:     slog.New(slog.NewTextHandler(log, nil)),
		Metrics: natscallout.NewMetrics(registry),
	}
	h.StartService(t)
	return h
}

// URL is the hub's client URL, the same across every StopServer and StartServer (testnats.Server.URL).
func (h *Hub) URL() string { return h.url }

// Admin connects to the hub as its admin, a user the server admits by its own configuration with
// every subject granted both ways, for what a test does beside the client under test: making a
// stream, publishing a fixture, reading what the client sent. The connection reconnects without
// limit, so it is back once a StopServer's StartServer has run, and is closed when the test ends.
// Each call makes another connection.
func (h *Hub) Admin(t testing.TB) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(h.url, nats.Nkey(h.adminKey, h.adminSign), nats.Name(adminName),
		nats.Timeout(adminConnectTimeout), nats.MaxReconnects(-1), nats.ReconnectWait(adminReconnectWait))
	if err != nil {
		t.Fatalf("testnatscallout: connect to the hub at %s as its admin: %v", h.url, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// adminName is the name each Admin connection gives the server, which /connz lists it by.
const adminName = "testnatscallout admin"

// adminConnectTimeout bounds an Admin connect's dial and handshake: the server already admits the
// static users when Start returns, so it only leaves room for a loaded machine, as natscred's rig
// tests leave their connects. adminReconnectWait is the wait between an Admin connection's
// reconnects, short, so the admin is back soon after a StartServer.
const (
	adminConnectTimeout = 10 * time.Second
	adminReconnectWait  = 100 * time.Millisecond
)

// StaticKeys is the public key of each user the server admits by its own configuration, without
// asking the auth service: the admin (Admin) and the service's own user, which natscallout.Start
// connects as. The monitoring endpoint's /connz?auth=true reports such a connection's
// authorized_user as its key, and a connection the service admitted by the name its decision gave
// (daemon/<account> for a daemon entry), so an inventory of the hub's connections skips the static
// users' by these keys, and every other connection is one the service decided.
func (h *Hub) StaticKeys() []string { return []string{h.adminKey, h.serviceKey} }

// StopServer shuts the nats-server down, as stopping a server an operator restarts does
// (testnats.Server.Stop): every client's connection drops, the auth service's among them, and the
// URL's port refuses each new connection until StartServer. Stopping a stopped server does nothing.
func (h *Hub) StopServer(t testing.TB) {
	t.Helper()
	h.server.Stop(t)
}

// StartServer runs the nats-server again on the store and the configuration it had
// (testnats.Server.Start), at the same URL, and returns once it accepts connections and, while the
// auth service runs, once the service has reconnected and its subscription has reached the server
// again (natscallout.Service.Ready), so the next client the server sends it is decided rather than
// left unanswered. It waits as long as the test may run.
func (h *Hub) StartServer(t testing.TB) {
	t.Helper()
	h.server.Start(t)
	h.mu.Lock()
	service := h.service
	h.mu.Unlock()
	if service != nil {
		h.awaitReady(t, service)
	}
}

// MonitorURL is the nats-server's monitoring endpoint (testnats.Server.MonitorURL), where
// /connz?auth=true lists each open client connection with the user it was authorized as;
// state=closed lists the closed ones instead, and state=all both. The listener is the server's own,
// so a StopServer and a StartServer give it a new port, and a test reads this again after one; it
// is "" while the server is stopped. The server moves a closing connection from its open list to its
// closed one on a goroutine of its own, so a read just after a connection closed may find it on
// neither list, or on both (testnats.Server.Connections): read the inventory once the test's own
// connections are at rest.
func (h *Hub) MonitorURL() string { return h.server.MonitorURL() }

// ServerLog is everything the nats-server has logged so far (testnats.Server.Log), each line
// "[<level>] <line>": a client's publish or subscribe its grant does not allow is logged at ERR as
// a "Publish Violation" or a "Subscription Violation" naming the subject, as the client is sent
// "Permissions Violation".
func (h *Hub) ServerLog() string { return h.server.Log() }

// StopService stops the auth service, so the server has nobody to ask about a client and admits
// none but its two static users until StartService: each other client's CONNECT waits out the
// server's authorization timeout (testnats.CalloutAuthTimeout) and is refused. Stopping a stopped
// service does nothing.
func (h *Hub) StopService() {
	h.mu.Lock()
	service := h.service
	h.service = nil
	h.mu.Unlock()
	if service != nil {
		service.Close()
	}
}

// StartService starts the auth service on the hub's keys, services and verifier, closed when the
// test ends, and returns once its subscription has reached the server, so the next client the
// server sends it is decided; it waits as long as the test may run. Start calls it; a test calls it
// again after StopService. Its log and decision counter are the ones the earlier service wrote, so
// ServiceLog and Refused read across the stop.
func (h *Hub) StartService(t testing.TB) {
	t.Helper()
	h.mu.Lock()
	running := h.service != nil
	h.mu.Unlock()
	if running {
		t.Fatal("testnatscallout: start the hub's auth service while it runs")
	}
	service, err := natscallout.Start(t.Context(), h.settings, h.deps)
	if err != nil {
		t.Fatalf("testnatscallout: start the auth service: %v", err)
	}
	t.Cleanup(service.Close)
	h.awaitReady(t, service)
	h.mu.Lock()
	h.service = service
	h.mu.Unlock()
}

// awaitReady waits, as long as the test may run (testnats.Context), for service's subscription to
// reach the server.
func (h *Hub) awaitReady(t testing.TB, service *natscallout.Service) {
	t.Helper()
	if err := service.Ready(testnats.Context(t)); err != nil {
		t.Fatalf("testnatscallout: wait for the auth service to subscribe at the hub at %s: %v", h.url, err)
	}
}

// ServiceLog is everything the auth service has logged so far: a line for each kind of refusal,
// failed answer or drop at most once a minute, each with its first event's detail and never a
// credential (natscallout.Deps.Log). A test prints it beside a refusal it did not expect.
func (h *Hub) ServiceLog() string { return h.log.String() }

// Tokens is each raw token the verifier was given, in the order the service asked it, across every
// StopService and StartService: a copy, safe to read while the service runs.
func (h *Hub) Tokens() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tokens)
}

// Refused is how many connections the auth service has refused under label, of every credential
// kind, read from its decision counter (broker_nats_callout_decisions_total): label is one of
// natscallout's reasons, such as no_credential (a client that presented neither a service's user
// and password nor a session's token), service_token_not_verified (the Verifier's error),
// service_not_listed, token_not_live or token_too_long. The service counts a decision as it answers
// it, so a test that has just seen a refusal waits for its count. It may run off the test's
// goroutine, so a counter line it cannot read is reported with Errorf, never FailNow.
func (h *Hub) Refused(t testing.TB, label string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	total := 0
	for line := range strings.SplitSeq(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "broker_nats_callout_decisions_total{") || !strings.Contains(line, `outcome="refused",reason="`+label+`"}`) {
			continue
		}
		n, err := strconv.Atoi(line[strings.LastIndex(line, " ")+1:])
		if err != nil {
			t.Errorf("testnatscallout: a decision counter line %q: %v", line, err)
			continue
		}
		total += n
	}
	return total
}
