package natscred_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// daemonAccount is the service account the rig's service tokens name, listed as a daemon.
const daemonAccount = "system:serviceaccount:example-ns:example-daemon"

// operatorAccount is the service account the operator rig's service tokens name, listed as the
// operator.
const operatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"

// rig is a hub behind the real auth service, whose broker answers are the test's own.
type rig struct {
	url    string
	server *testnats.Server
	// log is the service's log, which its reporter writes while a test reads it.
	log *logtest.Buffer
	// registry counts the service's decisions (natscallout.NewMetrics), which a test reads to see
	// why a connection was refused: the log has only one line a minute of them.
	registry *metrics.Registry

	// settings and deps are the auth service's, which startService starts again after a stop.
	settings natscallout.Settings
	deps     natscallout.Deps
	service  *natscallout.Service
}

// newRig starts a callout hub and the auth service in front of it, deciding sessions with
// introspect and services with serviceToken, the one service listed being a daemon on
// daemonAccount.
func newRig(t *testing.T, introspect func(context.Context, string) (oauthwire.Introspection, error),
	serviceToken func(context.Context, string) (string, bool, time.Time, time.Time, error)) *rig {
	t.Helper()
	return startRig(t, "daemon/-/*="+daemonAccount, natscallout.Deps{Introspect: introspect, ServiceToken: serviceToken})
}

// newOperatorRig starts a callout hub and the auth service in front of it, deciding services with
// serviceToken, the one service listed being the operator on operatorAccount, and the writer
// hold's row reading held: the hold view's read answers a row signed with a key of the rig's own,
// whose public half it verifies against, so the operator is answered with
// natsgrant.Operator(OperatorPhaseHeld).
func newOperatorRig(t *testing.T, serviceToken func(context.Context, string) (string, bool, time.Time, time.Time, error)) *rig {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	// Cut to the microsecond, as the hold row's column keeps it, so the stub answers a row the
	// broker could have read back.
	signedAt := time.Now().UTC().Truncate(time.Microsecond)
	held := string(natsgrant.OperatorPhaseHeld)
	sig, err := signedrecord.Sign(private, signedrecord.HoldRow(1, held, "hold-1", signedAt))
	require.NoError(t, err)
	row := natshold.Reading{Gen: 1, Phase: held, HoldID: "hold-1", SignedAt: signedAt, Sig: sig}
	read := func(context.Context) (natshold.Reading, bool, error) { return row, true, nil }
	return startRig(t, "operator/-/*="+operatorAccount, natscallout.Deps{
		ServiceToken: serviceToken,
		Hold:         natscallout.NewHoldView(read, []ed25519.PublicKey{public}, nil),
	})
}

// startRig starts a callout hub and the auth service in front of it, listing services (in
// BROKER_NATS_SERVICES' form) and answering with deps, whose log and decision counter are the
// rig's own.
func startRig(t *testing.T, services string, deps natscallout.Deps) *rig {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	server, url := testnats.StartWithCalloutServer(t, keys)
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
		return path
	}
	listed, err := config.ParseNATSServices(services)
	require.NoError(t, err)
	log := &logtest.Buffer{}
	registry := metrics.New()
	deps.Log = slog.New(slog.NewTextHandler(log, nil))
	deps.Metrics = natscallout.NewMetrics(registry)
	r := &rig{
		url: url, server: server, log: log, registry: registry,
		settings: natscallout.Settings{
			URLs:     []string{url},
			SeedFile: write("service.seed", keys.ServiceSeed), IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
			XKeySeedFile: write("xkey.seed", keys.XKeySeed), Services: listed,
		},
		deps: deps,
	}
	r.startService(t)
	return r
}

// startService starts the auth service in front of the rig's hub, on the hub's keys and the
// rig's answers, closed when the test ends, and returns once the hub sends it each connection.
func (r *rig) startService(t *testing.T) {
	t.Helper()
	service, err := natscallout.Start(t.Context(), r.settings, r.deps)
	require.NoError(t, err)
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, service.Ready(ctx))
	r.service = service
}

// stopService stops the auth service, so the hub has nobody to ask about a connection and
// admits none but its own users until startService.
func (r *rig) stopService() { r.service.Close() }

// refused is how many connections the service has refused under label, of every credential kind,
// read from its decision counter. It may run off the test's goroutine (require.Eventually), so a
// line it cannot read is reported with Errorf, never FailNow.
func (r *rig) refused(t *testing.T, label string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	total := 0
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "broker_nats_callout_decisions_total{") || !strings.Contains(line, `outcome="refused",reason="`+label+`"}`) {
			continue
		}
		n, err := strconv.Atoi(line[strings.LastIndex(line, " ")+1:])
		if err != nil {
			t.Errorf("a counter line %q: %v", line, err)
			continue
		}
		total += n
	}
	return total
}

// thumbprint is the RFC 7638 thumbprint of key, which the broker binds a token to.
func thumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(sum)
}

// liveFor answers every token of tokens live for session ses_a, bound to jkt, for 15 minutes.
func liveFor(jkt string, tokens func(string) bool) func(context.Context, string) (oauthwire.Introspection, error) {
	return func(_ context.Context, token string) (oauthwire.Introspection, error) {
		if !tokens(token) {
			return oauthwire.Introspection{}, nil
		}
		return oauthwire.Introspection{
			Active: true, Session: "ses_a", Person: "person@example.com", ClientID: ciphertoken.ExchangeClientID,
			ProjectScope: ciphertoken.ProjectScopePerson, Thumbprint: jkt, ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
		}, nil
	}
}

// platformToken is an unsigned JWT naming account, issued now and lapsing at expiry: the stub
// ServiceToken below reads it, and natscred reads its expiry, neither verifying it.
func platformToken(account string, expiry time.Time) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": account, "iat": time.Now().Unix(), "exp": expiry.Unix()}) + ".unsigned"
}

// readPlatformToken is a stub ServiceToken: the account, issue and expiry raw names, unverified.
func readPlatformToken(raw string) (string, bool, time.Time, time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false, time.Time{}, time.Time{}, errors.New("no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	var claims struct {
		Sub      string `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false, time.Time{}, time.Time{}, fmt.Errorf("claims: %w", err)
	}
	return claims.Sub, false, time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0), nil
}
