// Package policytest builds a live policy for tests: secrets held by a secrets.Local, under one
// namespace and encrypted with one agent-secrets key, loaded by the real policy.Loader.
package policytest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

const (
	// Prefix is the namespace every Secret sits under.
	Prefix = "example/agent-secrets/"
	// KeyARN is the agent-secrets key every Secret is encrypted with (an AWS documentation
	// account).
	KeyARN = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// Secret is the namespace secret a session asks for as name (an environment-variable name such as
// DEEL_API_KEY), tagged with owner and tier, encrypted with KeyARN, holding value.
func Secret(name, owner, tier, value string) secrets.LocalSecret {
	return secrets.LocalSecret{
		Name:     ID(name),
		KmsKeyID: KeyARN,
		Tags:     map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
		Value:    value,
	}
}

// FixtureGitHubOwners is the github-owners tag KindSecret gives a github-app secret, in the
// canonical form ParseGitHubOwners answers.
const FixtureGitHubOwners = "acme ada"

// FixtureMaxGrant is the max-grant tag KindSecret gives a google-service-account secret
// (policy.TagRules refuses such a secret without a cap, and one longer than an hour).
const FixtureMaxGrant = "1h"

// KindSecret is Secret with a kind tag: a secret whose release the broker mints from value. A
// kind that needs github-owners (policy.KindNeedsGitHubOwners) also carries them
// (FixtureGitHubOwners), without which the policy refuses it; a test of that refusal deletes the
// tag. A google-service-account secret carries the requesters its own rules demand — its owner
// alone — and the max-grant its kind must set (FixtureMaxGrant), so the fixture loads for an owner
// who is a person; a test of those refusals edits the tags.
func KindSecret(name, owner, tier, kind, value string) secrets.LocalSecret {
	secret := Secret(name, owner, tier, value)
	secret.Tags[policy.TagKind] = kind
	if policy.KindNeedsGitHubOwners(kind) {
		secret.Tags[policy.TagGitHubOwners] = FixtureGitHubOwners
	}
	if kind == policy.KindGoogleServiceAccount {
		secret.Tags[policy.TagRequesters] = owner
		secret.Tags[policy.TagMaxGrant] = FixtureMaxGrant
	}
	return secret
}

// KindValue is a value of kind that secrets.ParseValue decodes, so the policy serves a KindSecret
// holding it: the policy reads and decodes each kind secret's value (policy.Current), and refuses
// one it cannot decode (policy.ReasonValueMalformed). A github-app value names App 123456 and a
// private key made once for the test binary (fixtureAppKey), built at run time rather than written
// here. No field holds a credential. It panics for a kind ParseValue reads no value of, which is a
// test bug.
func KindValue(kind string) string {
	fields := map[string]string{}
	switch kind {
	case policy.KindGitHubApp:
		fields["app_id"] = "123456"
		fields["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(fixtureAppKey())}))
	case policy.KindGoogleLogin:
		fields["type"], fields["client_id"], fields["client_secret"], fields["refresh_token"] = "authorized_user", "client", "secret", "refresh"
	case policy.KindGoogleServiceAccount:
		fields["service_account"] = "robot-1@acme-agent-infra.iam.gserviceaccount.com"
	case policy.KindAzureLogin:
		fields["tenant_id"], fields["client_id"] = "tenant", "client"
	default:
		panic(fmt.Sprintf("policytest.KindValue: no value of kind %q", kind))
	}
	value, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(value)
}

// fixtureAppKey is the GitHub App key KindValue's github-app value holds: one for the test binary,
// since making a 2048-bit key costs more than most tests that store one.
var fixtureAppKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// UnsignableAppKey is a 512-bit RSA key in PEM, made at run time from two fresh 256-bit primes:
// x509 parses it, and crypto/rsa refuses to sign with it, since Go refuses every RSA key shorter
// than 1024 bits (crypto/rsa's minimum key size). It is the stored key a github-app value can hold
// that parses and still cannot sign. No fixture writes the key out: rsa.GenerateKey refuses that
// size, and a key written into the source is a key the push guard refuses.
func UnsignableAppKey(t testing.TB) string {
	t.Helper()
	one := big.NewInt(1)
	for {
		p, err := rand.Prime(rand.Reader, 256)
		if err != nil {
			t.Fatal(err)
		}
		q, err := rand.Prime(rand.Reader, 256)
		if err != nil {
			t.Fatal(err)
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
		d := new(big.Int).ModInverse(big.NewInt(65537), phi)
		if p.Cmp(q) == 0 || d == nil {
			continue
		}
		key := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Mul(p, q), E: 65537}, D: d, Primes: []*big.Int{p, q}}
		key.Precompute()
		return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	}
}

// ListedSecret is Secret with a requesters tag naming requesters, beside a max-grant of
// FixtureMaxGrant: the shape of a secret whose list says who may ask for it, with a cap short
// enough that the kind and admin rules which demand one (policy.TagRules) are met as well. A test
// of a refusal those rules answer edits the tags of what this returns.
func ListedSecret(name, owner, tier, requesters, value string) secrets.LocalSecret {
	secret := Secret(name, owner, tier, value)
	secret.Tags[policy.TagRequesters] = requesters
	secret.Tags[policy.TagMaxGrant] = FixtureMaxGrant
	return secret
}

// ID is the Secrets Manager name of the secret a session asks for as name.
func ID(name string) string {
	return Prefix + mustSlug(name)
}

// mustSlug is name's slug under the prefix; a fixture naming no valid secret name is a test bug.
func mustSlug(name string) string {
	slug, err := policy.NameToSlug(name)
	if err != nil {
		panic(fmt.Sprintf("policytest: %q: %v", name, err))
	}
	return slug
}

// Store is the secrets.AWS over store, keyed on KeyARN: what Loader and every rig read with.
func Store(store *secrets.Local) secrets.AWS {
	return secrets.AWS{Client: store, KMS: store, KeyARN: KeyARN}
}

// StoreThrough is Store with client, a wrapper over store that counts, delays or fails its Secrets
// Manager calls, in place of store itself.
func StoreThrough(store *secrets.Local, client secrets.SecretsManagerAPI) secrets.AWS {
	return secrets.AWS{Client: client, KMS: store, KeyARN: KeyARN}
}

// CountingDescriber counts the DescribeSecret calls a reread or a reload makes through it.
type CountingDescriber struct {
	secrets.SecretsManagerAPI
	Calls atomic.Int64
}

func (c *CountingDescriber) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	c.Calls.Add(1)
	return c.SecretsManagerAPI.DescribeSecret(ctx, in, opts...)
}

// Loader is the policy.Loader over store, with services registered.
func Loader(store *secrets.Local, services ...string) policy.Loader {
	return policy.Loader{Store: Store(store), Prefix: Prefix, Services: services}
}

// Current loads store's policy, as the broker does at boot; Refresh rereads it after store
// changes. It stops reloading when t ends.
func Current(t testing.TB, store *secrets.Local, services ...string) *policy.Current {
	t.Helper()
	return current(t, Loader(store, services...))
}

// MeteredCurrent is Current with the policy's two metrics on registry, as cmd/broker/main.go
// registers them: broker_policy_store_reads_total counted from the first load on
// (policy.NewStoreReads), and broker_unservable_secrets once that load answered
// (policy.NewUnservableSecrets).
func MeteredCurrent(t testing.TB, store *secrets.Local, registry *metrics.Registry, services ...string) *policy.Current {
	t.Helper()
	loader := Loader(store, services...)
	loader.StoreReads = policy.NewStoreReads(registry)
	cur := current(t, loader)
	policy.NewUnservableSecrets(registry, cur)
	return cur
}

// current is loader's policy as NewCurrent first loads it, reloading hourly until t ends.
func current(t testing.TB, loader policy.Loader) *policy.Current {
	t.Helper()
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	return cur
}

// CaptureLog points the default slog handler, which the broker logs through, at a buffer with no
// timestamp until t ends, so a test reads the exact lines the broker writes; the reload ticker's
// goroutine may write while the test reads. The log output is global, so a goroutine that outlives
// t and logs writes into whichever capture is live then: another test's. synctest.Test returns only
// once every goroutine it started has returned, which is how the policy package's reload tests keep
// their ticker's lines in their own capture.
func CaptureLog(t testing.TB) fmt.Stringer {
	t.Helper()
	logged := &lockedBuffer{}
	flags, output := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(output) })
	return logged
}

type lockedBuffer = logtest.Buffer
