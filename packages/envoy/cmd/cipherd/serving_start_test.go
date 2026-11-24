package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestABrokerThatDiesBeforeServingRecordsNoServingBeat runs the REAL broker binary configured so
// that its boot fails after the bind but before srv.Serve: the test holds oauth_provider_secret
// locked in a transaction of its own, so the broker's read of its OAuth secret, one of the boot
// calls that run after the bind, waits out oauthSecretTimeout and fails, and the broker exits
// non-zero having served nothing. A beat is a record that a broker is serving, so this broker must
// leave broker_serving as it found it: a fleet crash-looping at such a step must read as no broker
// serving, or the leases that run out meanwhile are swept. The wait is the point of the lock: it
// gives a serving clock started before the failing step time to beat several times
// (BROKER_SWEEP_SECONDS=1), so a clock started too early fails this test on every run rather than
// on a race between its first beat and the exit. (The issuers' discovery, which once made this
// failure, runs before the bind since the identity registry opens with the other boot reads.)
func TestABrokerThatDiesBeforeServingRecordsNoServingBeat(t *testing.T) {
	databaseURL := storetest.URL(t)
	memorySecretsFile := filepath.Join(t.TempDir(), "memory-secrets.json")
	if err := os.WriteFile(memorySecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db := connectToBroker(t, databaseURL)
	serving := func() string {
		t.Helper()
		var rows string
		if err := db.QueryRow(ctx, `select coalesce(string_agg(process || ' ' || last_serving_at::text || ' ' || not_serving_after::text, ', ' order by process), '')
			from broker_serving`).Scan(&rows); err != nil {
			t.Fatalf("read broker_serving: %v", err)
		}
		return rows
	}
	before := serving()
	// Held until the test ends: nothing a correct broker writes marks the wait, so no gate the
	// test releases can take its place.
	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the OAuth secret: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `lock table oauth_provider_secret in access exclusive mode`); err != nil {
		t.Fatalf("lock oauth_provider_secret: %v", err)
	}

	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + memorySecretsFile,
		"BROKER_SWEEP_SECONDS=1",
	}})
	if err := broker.AwaitExit(t); err == nil {
		t.Fatalf("the broker exited 0, want a fatal exit at its OAuth secret's read:\n%s", broker.Log())
	}
	if log := broker.Log(); !strings.Contains(log, "broker: fatal") || !strings.Contains(log, "store OAuth provider secret") {
		t.Fatalf("the broker exited, but not on its OAuth secret's read:\n%s", log)
	}
	if after := serving(); after != before {
		t.Fatalf("broker_serving after a broker that never served = %q, want it left at %q:\n%s", after, before, broker.Log())
	}
}

// TestAnUndiscoverableExchangeIssuerRefusesTheBootBeforeTheBind runs the REAL broker binary with
// a GitHub exchange issuer whose discovery answers 404: the identity registry reads every issuer
// it trusts before the bind, so the broker exits non-zero naming the issuer, and never logs
// "broker listening". It is as fatal as it was when that read ran after the bind; only where it
// fails moved.
func TestAnUndiscoverableExchangeIssuerRefusesTheBootBeforeTheBind(t *testing.T) {
	issuer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(issuer.Close)
	memorySecretsFile := filepath.Join(t.TempDir(), "memory-secrets.json")
	if err := os.WriteFile(memorySecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{NoWaitForListening: true, Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + memorySecretsFile,
		"BROKER_EXCHANGE_GITHUB_ISSUER=" + issuer.URL,
		"BROKER_EXCHANGE_GITHUB_AUDIENCE=example-broker-exchange",
	}})
	if err := broker.AwaitExit(t); err == nil {
		t.Fatalf("the broker exited 0, want a fatal exit at its exchange issuer's discovery:\n%s", broker.Log())
	}
	log := broker.Log()
	if !strings.Contains(log, "broker: fatal") || !strings.Contains(log, "discovering issuer "+issuer.URL) {
		t.Fatalf("the broker exited, but not on its exchange issuer's discovery:\n%s", log)
	}
	if strings.Contains(log, "broker listening") {
		t.Fatalf("the broker bound its socket before reading its exchange issuer:\n%s", log)
	}
}
