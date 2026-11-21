package holdtest

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/holdrecord/holdrecordtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// The accounts the stack's auth service lists, as BROKER_NATS_SERVICES writes each one: the
// operator (holdrecordtest's, so one token serves the broker's routes and the hub), the hold's
// probe, and the listeners a test connects as writers.
const (
	OperatorAccount = holdrecordtest.OperatorAccount
	ProbeAccount    = "system:serviceaccount:envoy-operator:envoy-hold-probe"
	// ListenerMachines are the machine ids of the listener entries the stack lists; a test
	// connects a listener of each through ConnectListener.
	listenerAccountPrefix = "system:serviceaccount:holdtest:listener-"
)

// ListenerMachines are the listener entries the stack's auth service lists, by machine id.
var ListenerMachines = []string{"hub-a", "hub-b", "hub-c"}

// Stack is the writer hold's whole surrounding, real but for the platform tokens: a test Postgres
// schema with the broker's migrations, the broker's own /v1/nats-hold routes on it
// (holdrecordtest), a cluster of nats-servers whose every client is decided by the NATS auth
// service (testnats.StartClusterWithCallout), and that auth service itself (natscallout.Start),
// reading the hold row from the schema at every decision, so it refuses every client but the
// operator while the row holds held. Its hold row starts as the bootstrap leaves it: released at
// gen 1, signed by Broker.SigningKey.
//
// Like holdrecordtest, Start skips the test when BROKER_TEST_DATABASE_URL is unset.
type Stack struct {
	Cluster *testnats.Cluster
	Keys    testnats.CalloutKeys
	Broker  *holdrecordtest.Broker
	// Records is the operator's client of Broker's routes, signing with Broker.SigningKey.
	Records *holdrecord.Client
	// Auth is the auth service, started with the stack and closed when the test ends.
	Auth *natscallout.Service
	// Kube is the clustered hub's Kubernetes objects over Cluster, with the chart's render
	// (InstallRender): what the hold reads and evicts.
	Kube *Kube

	t      testing.TB
	tokens *tokenTable
}

// tokenTable is the stack's platform tokens: each raw token the hub's clients present, the account
// it names, and how long a token lives from its issue (TokenLife).
type tokenTable struct {
	mu       sync.Mutex
	accounts map[string]string
	life     time.Duration
}

func (tt *tokenTable) verify(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	account, ok := tt.accounts[raw]
	if !ok {
		return "", false, time.Time{}, time.Time{}, fmt.Errorf("holdtest: %q is no token of the stack", raw)
	}
	issued := time.Now().Add(-5 * time.Second)
	return account, false, issued, issued.Add(tt.life), nil
}

// The raw tokens of the stack's probe and listeners. The operator's is holdrecordtest's.
const probeToken = "holdtest-probe-token"

func listenerToken(machine string) string { return "holdtest-listener-" + machine + "-token" }

// Options change what Start runs.
type Options struct {
	// NoHoldRead starts the auth service as a broker whose BROKER_NATS_HOLD_READ is false: it reads
	// no hold row and decides every client as if the row held released, the control that shows
	// what the hold rule refuses.
	NoHoldRead bool
}

// Start runs the stack, stopped when t ends.
func Start(t testing.TB) *Stack {
	t.Helper()
	return StartWith(t, Options{})
}

// StartWith runs the stack with opts, stopped when t ends.
func StartWith(t testing.TB, opts Options) *Stack {
	t.Helper()
	broker := holdrecordtest.Start(t)
	var schema string
	if err := broker.Pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("holdtest: read the test schema's name: %v", err)
	}
	tokens := &tokenTable{accounts: map[string]string{
		holdrecordtest.OperatorToken: OperatorAccount,
		probeToken:                   ProbeAccount,
	}, life: 50 * time.Minute}
	entries := []string{"operator/-/*=" + OperatorAccount, "probe/-/*=" + ProbeAccount}
	for _, machine := range ListenerMachines {
		account := listenerAccountPrefix + machine
		tokens.accounts[listenerToken(machine)] = account
		entries = append(entries, "listener/"+machine+"/*="+account)
	}
	services, err := natscallout.ParseServices(strings.Join(entries, " "))
	if err != nil {
		t.Fatalf("holdtest: the stack's services: %v", err)
	}

	keys := testnats.NewCalloutKeys(t)
	cluster := testnats.StartClusterWithCallout(t, 3, keys)
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
			t.Fatalf("holdtest: write %s: %v", name, err)
		}
		return path
	}
	pool := broker.Pool
	holdRead := func(ctx context.Context) (natshold.Reading, bool, error) {
		return natshold.Read(ctx, pool, schema)
	}
	if opts.NoHoldRead {
		holdRead = nil
	}
	auth, err := natscallout.Start(context.Background(), natscallout.Settings{
		URLs:           cluster.URLs(),
		SeedFile:       write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile:   write("xkey.seed", keys.XKeySeed),
		Services:       services,
	}, natscallout.Deps{
		ServiceToken: tokens.verify,
		HoldRead:     holdRead,
		HoldKeys:     []ed25519.PublicKey{broker.VerifyKey},
	})
	if err != nil {
		t.Fatalf("holdtest: start the auth service: %v", err)
	}
	t.Cleanup(auth.Close)
	ready, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := auth.Ready(ready); err != nil {
		t.Fatalf("holdtest: the auth service's subscription never reached the hub: %v", err)
	}

	records := broker.Client(t)
	if _, err := records.RestoreRow(context.Background(), 1, natsgrant.OperatorPhaseReleased, "", "holdtest bootstrap"); err != nil {
		t.Fatalf("holdtest: bootstrap the hold row: %v", err)
	}
	kube := NewKube(t, cluster)
	kube.InstallRender(t, keys)
	return &Stack{Cluster: cluster, Keys: keys, Broker: broker, Records: records, Auth: auth, Kube: kube, t: t, tokens: tokens}
}

// SetTokenLife sets how long every platform token the stack verifies from now on lives from its
// issue: a test of an expiry shortens it. The auth service answers a service's user with its
// token's expiry, and the operator's with at most 10 minutes past its decision.
func (s *Stack) SetTokenLife(life time.Duration) {
	s.tokens.mu.Lock()
	defer s.tokens.mu.Unlock()
	s.tokens.life = life
}

// OperatorCredential is the operator's CONNECT, user "service" with the operator's platform
// token, as natscred.Service sends it, and the operator's inbox prefix.
func (s *Stack) OperatorCredential() []nats.Option {
	return []nats.Option{nats.UserInfo("service", holdrecordtest.OperatorToken), nats.CustomInboxPrefix(natsgrant.OperatorInbox)}
}

// ProbeCredential is the hold probe's CONNECT.
func (s *Stack) ProbeCredential() nats.Option { return nats.UserInfo("service", probeToken) }

// ListenerCredential is the CONNECT of the listener entry of machine, one of ListenerMachines.
func (s *Stack) ListenerCredential(machine string) nats.Option {
	return nats.UserInfo("service", listenerToken(machine))
}

// Connect connects to url with opts, its connect bounded, closed when the test ends, and answers
// the server's refusal as the error.
func (s *Stack) Connect(url string, opts ...nats.Option) (*nats.Conn, error) {
	s.t.Helper()
	nc, err := nats.Connect(url, append([]nats.Option{nats.Timeout(10 * time.Second)}, opts...)...)
	if err == nil {
		s.t.Cleanup(nc.Close)
	}
	return nc, err
}
