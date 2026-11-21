package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testEnv is what a test of this package holds: its own NATS server, the bus client a producer
// publishes through, the interest registry a fan-out reads, and the session registry a delivery
// resolves its target in. A delivery decision is the listener's, and `cmd/listener`'s own tests
// drive it.
type testEnv struct {
	t        *testing.T
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
}

type testEnvOption func(*testEnvOpts)

type testEnvOpts struct {
	sessionTTL   time.Duration
	sessionClock func() time.Time
}

func withSessionTTL(d time.Duration) testEnvOption {
	return func(o *testEnvOpts) { o.sessionTTL = d }
}

// withSessionClock gives the session registry the clock a test moves (session.WithSessionClock).
func withSessionClock(now func() time.Time) testEnvOption {
	return func(o *testEnvOpts) { o.sessionClock = now }
}

// sessionClock is the session registry's clock in a test that crosses a TTL by moving it rather
// than waiting the TTL out: the wall clock, plus every advance the test has made.
type sessionClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *sessionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *sessionClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func setupTestEnv(t *testing.T, options ...testEnvOption) *testEnv {
	t.Helper()
	opts := testEnvOpts{sessionTTL: 10 * time.Second, sessionClock: time.Now}
	for _, o := range options {
		o(&opts)
	}

	// Start real NATS with JetStream
	uri := testnats.URL(t)

	// Connect bus client
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(client.Close)

	// Set up interest registry (NATS KV)
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(opts.sessionTTL), session.WithSessionClock(opts.sessionClock))
	if err != nil {
		t.Fatalf("failed to create session registry: %v", err)
	}

	return &testEnv{
		t:        t,
		client:   client,
		registry: registry,
		sessions: sessions,
	}
}

// registeredSubjects is what a registration stores as the session's topics: its own agent subject
// and each topic it follows.
func registeredSubjects(sessionID string, topics []string) []string {
	return append([]string{contracts.LegacyAgentSubject(sessionID)}, topics...)
}

// registerInterest subscribes interest in the KV store (envoy_interests bucket).
func (env *testEnv) registerInterest(sessionID string, topics []string) {
	env.t.Helper()
	if _, err := env.registry.Upsert(store.Interest{
		SessionID: sessionID,
		MachineID: "test-machine",
		Dir:       "/test",
	}, registeredSubjects(sessionID, topics)); err != nil {
		env.t.Fatalf("register the interest of the session %s: %v", sessionID, err)
	}
}

// registerKVSession writes a session-liveness entry to the envoy_sessions KV bucket.
func (env *testEnv) registerKVSession(sessionID string) {
	env.t.Helper()
	if err := env.sessions.Put(sessionID, session.SessionEntry{
		SelfSubscribed: true,
		MachineID:      "test-machine",
		Dir:            "/test",
	}); err != nil {
		env.t.Fatal(err)
	}
}

// --- TESTS ---

func containsTopic(topics []string, want string) bool {
	for _, topic := range topics {
		if topic == want {
			return true
		}
	}
	return false
}

func TestRoleTransfer_ExactlyOneHolder(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)
	env.registerInterest("ses_role_a", nil)
	env.registerInterest("ses_role_b", nil)

	if _, err := env.registry.SetRole("ses_role_a", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for A failed: %v", err)
	}
	if _, err := env.registry.SetRole("ses_role_b", "test-machine", "legion-controller", false); err != nil {
		t.Fatalf("SetRole for B failed: %v", err)
	}

	waitUntil(t, 5*time.Second, "the role to name exactly one holder", func() bool {
		a, errA := env.registry.Get(context.Background(), "ses_role_a")
		b, errB := env.registry.Get(context.Background(), "ses_role_b")
		matches := env.registry.Match("test-machine", "notifications.role.legion-controller")
		return errA == nil && errB == nil &&
			!containsTopic(a.Topics, "notifications.role.legion-controller") &&
			containsTopic(b.Topics, "notifications.role.legion-controller") &&
			len(matches) == 1 && matches[0].SessionID == "ses_role_b"
	})
}

// A session's KV entry expires at its TTL while its interest record stays. The TTL is crossed on
// the session registry's own clock, which the test moves past it, as the registry's own tests do
// (session.WithSessionClock): at a TTL of 2 s waited out on the wall clock, the registration, the
// interest and the read that must still find the entry all had to land inside those 2 s, and a
// test slowed past them failed on a read that found the entry already gone.
func TestDelivery_KVEntryExpiresButInterestPersists(t *testing.T) {
	t.Parallel()
	const ttl = time.Hour
	now := &sessionClock{}
	env := setupTestEnv(t, withSessionTTL(ttl), withSessionClock(now.Now))

	env.registerKVSession("ses_expiry_check")
	env.registerInterest("ses_expiry_check", nil)

	if _, err := env.sessions.Get("ses_expiry_check"); err != nil {
		t.Fatalf("expected KV entry to exist: %v", err)
	}

	now.advance(ttl + time.Minute)
	if _, err := env.sessions.Get("ses_expiry_check"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the session entry a TTL past its registration: %v, want it expired (%v)", err, natsgo.ErrKeyNotFound)
	}
	interest, err := env.registry.Get(context.Background(), "ses_expiry_check")
	if err != nil {
		t.Fatalf("interest should persist after session expiry, got: %v", err)
	}
	if interest.SessionID != "ses_expiry_check" {
		t.Fatalf("wrong interest session ID: %s", interest.SessionID)
	}
}

func TestDelivery_RegistryListReturnsSortedInterests(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t)

	env.registerInterest("ses_charlie", []string{"notifications.test.>"})
	env.registerInterest("ses_alpha", []string{"notifications.test.>"})
	env.registerInterest("ses_bravo", []string{"notifications.test.>"})

	// The cache follows the bucket through its watcher: wait for the three upserts to reach it.
	waitUntil(t, 5*time.Second, "the three interests to reach the cache", func() bool {
		items, err := env.registry.List()
		return err == nil && len(items) == 3
	})

	items, err := env.registry.List()
	if err != nil {
		t.Fatalf("list interests: %v", err)
	}
	if items[0].SessionID != "ses_alpha" {
		t.Fatalf("expected first item to be ses_alpha, got %s", items[0].SessionID)
	}
	if items[1].SessionID != "ses_bravo" {
		t.Fatalf("expected second item to be ses_bravo, got %s", items[1].SessionID)
	}
	if items[2].SessionID != "ses_charlie" {
		t.Fatalf("expected third item to be ses_charlie, got %s", items[2].SessionID)
	}
}
