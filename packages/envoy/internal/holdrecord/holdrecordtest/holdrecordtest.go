// Package holdrecordtest serves the secrets broker's real /v1/nats-hold routes, mounted by
// api.Register, on a test server over a migrated Postgres schema of the test's own (storetest),
// with a verifier that admits one operator token and an Ed25519 hold signing key. A test of the
// hold's client, or of a command that drives the hold through it, runs against the routes as the
// broker serves them rather than against a stub of them. Like storetest, Start skips the test when
// BROKER_TEST_DATABASE_URL is unset.
package holdrecordtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The operator entry the routes admit, and the one token the verifier takes for it: the token a
// client of Config presents.
const (
	OperatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"
	OperatorToken   = "holdrecordtest-operator-token"
)

// Broker is the hold's routes on a test server, until the test ends.
type Broker struct {
	// URL is the server's base URL, a client's holdrecord.Config.BrokerURL.
	URL string
	// Pool is a pool on the test's schema, for a test that writes the hold's tables as a database
	// writer other than the operator would (a forged row, a deleted row).
	Pool *pgxpool.Pool
	// SigningKey is the operator's hold signing key, and VerifyKey its public half, the one key
	// the routes check every write against.
	SigningKey ed25519.PrivateKey
	VerifyKey  ed25519.PublicKey

	deps         api.Deps
	routes       atomic.Pointer[http.ServeMux]
	hook         atomic.Pointer[func(http.ResponseWriter, *http.Request) bool]
	recordWrites atomic.Int64
}

// Start serves the hold's routes over a fresh migrated schema (storetest.URL), skipping t when
// BROKER_TEST_DATABASE_URL is unset, and stops them when t ends. The routes admit
// OperatorToken as the operator entry OperatorAccount, and refuse every other token
// not_operator. Every request comes from the test's one address, so the broker's per-address
// account bucket is opened wide: a test of the hold is not a test of its rate limit.
func Start(t testing.TB) *Broker {
	t.Helper()
	dsn := storetest.URL(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("holdrecordtest: parse the test schema's DSN: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("holdrecordtest: open the test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("holdrecordtest: generate the hold signing key: %v", err)
	}
	b := &Broker{Pool: pool, SigningKey: key, VerifyKey: pub, deps: api.Deps{
		AccountLimit: &ratelimit.Limit{Every: time.Microsecond, Burst: 1 << 20},
		NATSHold: api.NATSHold{
			Pool:            pool,
			Schema:          parsed.Query().Get("search_path"),
			Keys:            []ed25519.PublicKey{pub},
			OperatorToken:   verifyOperatorToken,
			OperatorAccount: OperatorAccount,
		},
	}}
	b.Restart()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut && req.URL.Path == "/v1/nats-hold/records" {
			b.recordWrites.Add(1)
		}
		if hook := b.hook.Load(); hook != nil && (*hook)(w, req) {
			return
		}
		b.routes.Load().ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	b.URL = srv.URL
	return b
}

// Restart mounts the hold's routes afresh on the same tables, as a broker process that starts
// again does: what the routes kept in memory, each record's floor among it, is gone, and every
// later request is served by the new routes at the same URL.
func (b *Broker) Restart() {
	mux := http.NewServeMux()
	api.Register(mux, b.deps)
	b.routes.Store(mux)
}

// verifyOperatorToken is the routes' platform-token verifier: OperatorToken is the operator
// entry's account, issued now and living ten minutes, and any other token is refused.
func verifyOperatorToken(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	if raw != OperatorToken {
		return "", false, time.Time{}, time.Time{}, errors.New("holdrecordtest: not the operator's token")
	}
	now := time.Now()
	return OperatorAccount, false, now, now.Add(10 * time.Minute), nil
}

// Config is the operator's client configuration for b: its URL, OperatorToken and both halves of
// the hold signing key.
func (b *Broker) Config() holdrecord.Config {
	return holdrecord.Config{
		BrokerURL:  b.URL,
		Token:      func(context.Context) (string, error) { return OperatorToken, nil },
		SigningKey: b.SigningKey,
		VerifyKeys: []ed25519.PublicKey{b.VerifyKey},
	}
}

// Client is a client of b with Config, failing t when New refuses it.
func (b *Broker) Client(t testing.TB) *holdrecord.Client {
	t.Helper()
	c, err := holdrecord.New(b.Config())
	if err != nil {
		t.Fatalf("holdrecordtest: %v", err)
	}
	return c
}

// Intercept sets hook, which sees each request before the routes do and answers it itself when it
// returns true; nil removes it. A test puts a write between a client's read and its write that
// way, or loses a request or its answer. A request the hook makes to b itself passes through the
// hook again.
func (b *Broker) Intercept(hook func(http.ResponseWriter, *http.Request) bool) {
	if hook == nil {
		b.hook.Store(nil)
		return
	}
	b.hook.Store(&hook)
}

// Routes are the hold's routes without the hook, for a hook that serves a request itself.
func (b *Broker) Routes() http.Handler { return b.routes.Load() }

// RecordWrites is how many PUTs of /v1/nats-hold/records b was sent, refused ones included.
func (b *Broker) RecordWrites() int64 { return b.recordWrites.Load() }
