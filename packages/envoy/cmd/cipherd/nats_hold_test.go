package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
)

// calloutSeedEnv writes the auth service's three seeds of keys to files of t's own and answers
// the settings that name them.
func calloutSeedEnv(t *testing.T, keys testnats.CalloutKeys) []string {
	t.Helper()
	dir := t.TempDir()
	var env []string
	for _, seed := range []struct{ setting, name, seed string }{
		{"BROKER_NATS_SEED_FILE", "service.seed", keys.ServiceSeed},
		{"BROKER_NATS_ISSUER_SEED_FILE", "issuer.seed", keys.IssuerSeed},
		{"BROKER_NATS_XKEY_SEED_FILE", "xkey.seed", keys.XKeySeed},
	} {
		path := filepath.Join(dir, seed.name)
		if err := os.WriteFile(path, []byte(seed.seed), 0o600); err != nil {
			t.Fatal(err)
		}
		env = append(env, seed.setting+"="+path)
	}
	return env
}

// holdKeyText is a fresh operator signing key and its public half as
// BROKER_NATS_HOLD_SIGNING_KEYS writes it.
func holdKeyText(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private, base64.RawURLEncoding.EncodeToString(public)
}

// TestTheStartRefusesTheHoldReadWithoutItsOperator: with BROKER_NATS_HOLD_READ true, the start
// is refused without an operator entry in BROKER_NATS_SERVICES or without
// BROKER_NATS_HOLD_SIGNING_KEYS, naming the switch and each setting that lacks what it needs, and
// the REAL compiled binary refuses before it reaches its database. With the switch false, or with
// both, nothing is refused. Fails if the refusal goes: the binary fails on its database instead.
func TestTheStartRefusesTheHoldReadWithoutItsOperator(t *testing.T) {
	t.Parallel()
	const (
		listener = "listener/example-hub/*=system:serviceaccount:example-ns:example-listener"
		operator = "operator/-/*=system:serviceaccount:envoy-operator:envoy-operator"
	)
	key := []ed25519.PublicKey{make(ed25519.PublicKey, ed25519.PublicKeySize)}
	parse := func(raw string) []config.NATSService {
		services, err := config.ParseNATSServices(raw)
		if err != nil {
			t.Fatal(err)
		}
		return services
	}
	for _, row := range []struct {
		name     string
		nats     config.NATSCallout
		names    []string
		refused  bool
		notNames []string
	}{
		{"no operator entry", config.NATSCallout{HoldRead: true, Services: parse(listener), HoldSigningKeys: key},
			[]string{"BROKER_NATS_HOLD_READ", "BROKER_NATS_SERVICES"}, true, []string{"BROKER_NATS_HOLD_SIGNING_KEYS"}},
		{"no signing key", config.NATSCallout{HoldRead: true, Services: parse(listener + " " + operator)},
			[]string{"BROKER_NATS_HOLD_READ", "BROKER_NATS_HOLD_SIGNING_KEYS"}, true, []string{"BROKER_NATS_SERVICES"}},
		{"neither", config.NATSCallout{HoldRead: true},
			[]string{"BROKER_NATS_HOLD_READ", "BROKER_NATS_SERVICES", "BROKER_NATS_HOLD_SIGNING_KEYS"}, true, nil},
		{"both", config.NATSCallout{HoldRead: true, Services: parse(operator), HoldSigningKeys: key}, nil, false, nil},
		{"the switch off", config.NATSCallout{Services: parse(listener)}, nil, false, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := refuseHoldReadWithoutOperator(config.Config{NATS: row.nats})
			if (err != nil) != row.refused {
				t.Fatalf("refuseHoldReadWithoutOperator = %v, want refused %v", err, row.refused)
			}
			for _, name := range row.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the refusal %q does not name %s", err, name)
				}
			}
			for _, name := range row.notNames {
				if strings.Contains(err.Error(), name) {
					t.Errorf("the refusal %q names %s, which is not what is missing", err, name)
				}
			}
		})
	}

	t.Run("the binary", func(t *testing.T) {
		_, signingKey := holdKeyText(t)
		out := runRefusedBroker(t, buildBroker(t), append(calloutSeedEnv(t, testnats.NewCalloutKeys(t)),
			"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
			"BROKER_PUBLIC_URL=https://secrets.internal.example",
			"BROKER_NATS_URLS=nats://hub.example:4222",
			"BROKER_NATS_SERVICES="+listener,
			"BROKER_NATS_HOLD_READ=true",
			"BROKER_NATS_HOLD_SIGNING_KEYS="+signingKey,
		)...)
		refusals := recordsWith(out, "broker: fatal", nil)
		if len(refusals) != 1 {
			t.Fatalf("the broker logged %d %q lines, want one:\n%s", len(refusals), "broker: fatal", out)
		}
		refusal, _ := refusals[0]["error"].(string)
		if !strings.Contains(refusal, "BROKER_NATS_HOLD_READ") || !strings.Contains(refusal, "BROKER_NATS_SERVICES") {
			t.Fatalf("the broker refused with %q, want a refusal naming BROKER_NATS_HOLD_READ and BROKER_NATS_SERVICES", refusal)
		}
	})
}

// TestTheBrokerStartsWithNoVerifiedRow drives the REAL compiled broker with the hold rule on
// (BROKER_NATS_HOLD_READ=true) against an in-process hub whose auth service it runs, on a real
// Postgres holding no hold row: the broker starts; a listener entry is refused while no row
// verifies, counted hold_unknown; the secrets broker serves a pod a grant meanwhile;
// /v1/nats-hold/restore writes the operator's signed released; and the listener is admitted by
// the first decision after it, which reads that row from the broker's schema with the operator's
// key. Fails if the broker refuses to start without a row: nothing listens and /restore cannot
// answer.
func TestTheBrokerStartsWithNoVerifiedRow(t *testing.T) {
	t.Parallel()
	const (
		podAudience     = "example-broker-pod"
		natsAudience    = "envoy-nats"
		service         = "example-service"
		loginAccount    = "system:serviceaccount:example-system:example-service-login"
		podAccount      = "system:serviceaccount:example:example-worker"
		operatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"
		listenerAccount = "system:serviceaccount:example-ns:example-listener"
		secretName      = "SERVICE_KEY"
		secretValue     = "service-value"
	)
	issuer := oidctest.New(t)
	signer := issuer.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": [
		{"name": "`+testSecretsPrefix+`service-key", "kms_key_id": "`+testSecretsKMSKeyARN+`", "tags": {"owner": "`+service+`", "tier": "agent"}, "value": "`+secretValue+`"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := testnats.NewCalloutKeys(t)
	hub := testnats.StartWithCallout(t, keys)
	operatorKey, signingKey := holdKeyText(t)
	broker := startBroker(t, brokerOptions{Env: append(calloutSeedEnv(t, keys),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount,
		"BROKER_SERVICES="+service+"="+podAccount,
		"BROKER_NATS_URLS="+hub,
		"BROKER_NATS_SERVICES=operator/-/*="+operatorAccount+" listener/example-hub/*="+listenerAccount,
		"BROKER_NATS_K8S_AUDIENCE="+natsAudience,
		"BROKER_NATS_HOLD_READ=true",
		"BROKER_NATS_HOLD_SIGNING_KEYS="+signingKey,
	)})
	base := "http://" + broker.Addr
	broker.AwaitRecord(t, "broker: the NATS auth service is answering", nil)
	natsToken := func(account string) string {
		claims := issuer.Claims(account, natsAudience)
		now := time.Now()
		claims["iat"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(10*time.Minute).Unix()
		return issuer.Mint(t, signer, claims)
	}
	connectListener := func() (*nats.Conn, error) {
		return nats.Connect(hub, nats.UserInfo(natscred.ServiceUser, natsToken(listenerAccount)), nats.NoReconnect(), nats.Timeout(5*time.Second))
	}

	t.Log("--- with no hold row, a listener entry is refused hold_unknown")
	if conn, err := connectListener(); err == nil {
		conn.Close()
		t.Fatal("a listener entry connected while the broker holds no hold row that verifies; want it refused hold_unknown")
	}
	holdUnknown := func() float64 {
		var n float64
		for labels, value := range scrapeMetric(t, broker.MetricsAddr(t), "broker_nats_callout_decisions_total") {
			if strings.Contains(labels, `reason="hold_unknown"`) {
				n += value
			}
		}
		return n
	}
	if n := holdUnknown(); n < 1 {
		t.Fatalf("broker_nats_callout_decisions_total counts %v hold_unknown refusals after the listener's, want at least 1", n)
	}

	t.Log("--- the secrets broker serves a pod its grant meanwhile")
	pods := ciphertest.Pods{T: t, Base: base, Token: ciphertest.IssuerTokens(t, issuer, signer, podAudience)}
	pod := pods.Enroll(pods.LogIn(service, loginAccount), "pod-a", podAccount)
	reply := pods.Ask(pod, secretName)
	if reply.State != "granted" || reply.GrantID == nil {
		t.Fatalf("the pod's request = %+v, want granted at once", reply)
	}
	if values := pods.Values(pod, *reply.GrantID); values[secretName] != secretValue {
		t.Fatalf("the pod's grant values = %v, want %s=%s", values, secretName, secretValue)
	}

	t.Log("--- the operator restores the first row, released at gen 1")
	signedAt := time.Now().UTC().Truncate(time.Microsecond)
	sig, err := signedrecord.Sign(operatorKey, signedrecord.HoldRow(1, "released", "", signedAt))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"gen": 1, "phase": "released", "hold_id": "", "reason": "the first row",
		"signed_at": signedAt.Format(time.RFC3339Nano), "sig": sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, base+"/v1/nats-hold/restore", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+natsToken(operatorAccount))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/nats-hold/restore = %d %s, want 200", response.StatusCode, answer)
	}

	t.Log("--- the listener is admitted by the first decision after the row verifies")
	var lastErr error
	if !testwait.Holds(10*time.Second, 100*time.Millisecond, func() bool {
		conn, err := connectListener()
		if err != nil {
			lastErr = err
			return false
		}
		conn.Close()
		return true
	}) {
		t.Fatalf("the listener is still refused after the operator's signed released was restored: %v", lastErr)
	}
}
