// Package policytest builds a live policy for tests: secrets held by a memory store, under one
// namespace, loaded by the real policy.Loader.
package policytest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// Prefix is the namespace every Secret sits under.
const Prefix = "example/agent-secrets/"

// Secret is the namespace secret a session asks for as name (an environment-variable name such as
// DEEL_API_KEY), tagged with owner and tier, holding value, on the agent-secrets key.
func Secret(name, owner, tier, value string) memory.Secret {
	return memory.Secret{
		Name:  ID(name),
		Tags:  map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
		Value: value,
	}
}

// Judged is s with a judge tag of seconds (policy.TagJudge), which makes it a key the model judge
// decides for that long, and a guidance tag of guidance (policy.TagGuidance) unless guidance is "".
func Judged(s memory.Secret, seconds int, guidance string) memory.Secret {
	s.Tags[policy.TagJudge] = strconv.Itoa(seconds)
	if guidance != "" {
		s.Tags[policy.TagGuidance] = guidance
	}
	return s
}

// JudgeTags puts a judge tag of seconds on the secret store holds as name, and a guidance tag of
// guidance unless it is "", as its owner's `agent-secrets secret retag --judge --guidance` does,
// and has cur read the namespace again, so the policy a test's broker holds gives the judge that
// key from here on.
func JudgeTags(t testing.TB, store *memory.Store, cur *policy.Current, name string, seconds int, guidance string) {
	t.Helper()
	secret, ok := store.Get(ID(name))
	if !ok {
		t.Fatalf("tag %s with judge %d: the store holds no %s", name, seconds, ID(name))
	}
	store.Put(Judged(secret, seconds, guidance))
	if err := cur.Refresh(t.Context()); err != nil {
		t.Fatalf("policy.Current.Refresh after tagging %s: %v", name, err)
	}
	if got, judged := cur.Get().JudgeSeconds(name); !judged || got != uint32(seconds) {
		t.Fatalf("after tagging %s with judge %d the policy judges it %d, %v", name, seconds, got, judged)
	}
}

// FixtureGitHubOwners is the github-owners tag TypeSecret gives a github-app secret, in the
// canonical form ParseGitHubOwners answers.
const FixtureGitHubOwners = "acme ada"

// FixtureMaxGrant is the max-grant tag TypeSecret gives a google-service-account secret
// (policy.TagRules refuses such a secret without a cap, and one longer than an hour).
const FixtureMaxGrant = "1h"

// TypeSecret is Secret with a type tag: a secret whose release the broker mints from value. A
// type that needs github-owners (policy.TypeNeedsGitHubOwners) also carries them
// (FixtureGitHubOwners), without which the policy refuses it; a test of that refusal deletes the
// tag. A google-service-account or anthropic-federation secret carries the requesters its own
// rules demand — its owner alone — and the max-grant its type must set (FixtureMaxGrant), so the
// fixture loads for an owner who is a person or a group; a test of those refusals edits the
// tags. An anthropic-federation secret is human tier only, so it loads only for tier human.
func TypeSecret(name, owner, tier, secretType, value string) memory.Secret {
	secret := Secret(name, owner, tier, value)
	secret.Tags[policy.TagType] = secretType
	if policy.TypeNeedsGitHubOwners(secretType) {
		secret.Tags[policy.TagGitHubOwners] = FixtureGitHubOwners
	}
	if secretType == policy.TypeGoogleServiceAccount || secretType == policy.TypeAnthropicFederation {
		secret.Tags[policy.TagRequesters] = owner
		secret.Tags[policy.TagMaxGrant] = FixtureMaxGrant
	}
	return secret
}

// TypeValue is a value of type that secrets.ParseValue decodes, so the policy serves a TypeSecret
// holding it: the policy reads and decodes each type secret's value (policy.Current), and refuses
// one it cannot decode (policy.ReasonValueMalformed). A github-app value names App 123456 and a
// private key made once for the test binary (fixtureAppKey), built at run time rather than written
// here. A github-installations value holds a token of each account FixtureGitHubOwners names
// (GitHubInstallationsValue). No field holds a credential. It panics for a type ParseValue reads no
// value of, which is a test bug.
func TypeValue(secretType string) string {
	if secretType == policy.TypeGitHubInstallations {
		return GitHubInstallationsValue()
	}
	fields := map[string]string{}
	switch secretType {
	case policy.TypeGitHubApp:
		fields["app_id"] = "123456"
		fields["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(fixtureAppKey())}))
	case policy.TypeGoogleLogin:
		fields["type"], fields["client_id"], fields["client_secret"], fields["refresh_token"] = "authorized_user", "client", "secret", "refresh"
	case policy.TypeGoogleServiceAccount:
		fields["service_account"] = "robot-1@acme-deployment.iam.gserviceaccount.com"
	case policy.TypeAzureLogin:
		fields["tenant_id"], fields["client_id"] = "tenant", "client"
	case policy.TypeAnthropicFederation:
		fields["organization_id"], fields["federation_rule_id"] = "00000000-0000-4000-8000-000000000000", "fdrl_01examplerule"
		fields["service_account_id"], fields["service_account"] = "svac_01exampletarget", "robot-1@acme-deployment.iam.gserviceaccount.com"
	default:
		panic(fmt.Sprintf("policytest.TypeValue: no value of type %q", secretType))
	}
	value, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(value)
}

// GitHubInstallationsValue is a github-installations value as a minter outside the broker writes
// one, at shapeVersion 1: for each account FixtureGitHubOwners names, acme and ada, its
// installation (41, 42), a token that is no credential ("fixture-token-<account>"), which lapses
// fifty minutes from now (the least a minter that runs every ten minutes leaves on a token GitHub
// gives an hour, well past secrets.MintedTokenMargin), contents and metadata at read, and every
// repository of the installation.
func GitHubInstallationsValue() string {
	installations := map[string]any{}
	for i, account := range strings.Fields(FixtureGitHubOwners) {
		installations[account] = map[string]any{
			"installation_id": 41 + i, "token": "fixture-token-" + account,
			"expires_at":  time.Now().Add(50 * time.Minute).UTC().Format(time.RFC3339),
			"permissions": map[string]string{"contents": "read", "metadata": "read"}, "repository_selection": "all",
		}
	}
	value, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": installations})
	if err != nil {
		panic(err)
	}
	return string(value)
}

// fixtureAppKey is the GitHub App key TypeValue's github-app value holds: one for the test binary,
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
// enough that the type and admin rules which demand one (policy.TagRules) are met as well. A test
// of a refusal those rules answer edits the tags of what this returns.
func ListedSecret(name, owner, tier, requesters, value string) memory.Secret {
	secret := Secret(name, owner, tier, value)
	secret.Tags[policy.TagRequesters] = requesters
	secret.Tags[policy.TagMaxGrant] = FixtureMaxGrant
	return secret
}

// ID is the name in the store of the secret a session asks for as name.
func ID(name string) string {
	return Prefix + mustSlug(name)
}

// Source is the source the policy serves the secret a session asks for as name by: the memory
// store's own (memory.Source) of its ID.
func Source(name string) string {
	return memory.Source(ID(name))
}

// mustSlug is name's slug under the prefix; a fixture naming no valid secret name is a test bug.
func mustSlug(name string) string {
	slug, err := policy.NameToSlug(name)
	if err != nil {
		panic(fmt.Sprintf("policytest: %q: %v", name, err))
	}
	return slug
}

// Loader is the policy.Loader over store, with services registered.
func Loader(store secretstore.Store, services ...string) policy.Loader {
	return policy.Loader{Store: store, Prefix: Prefix, Services: services}
}

// Current loads store's policy, as the broker does at boot; Refresh rereads it after store
// changes. It stops reloading when t ends.
func Current(t testing.TB, store secretstore.Store, services ...string) *policy.Current {
	t.Helper()
	return current(t, Loader(store, services...))
}

// Profiles is file, a profiles file's text, as the broker's boot reads it: parsed by
// policy.ParseProfiles and its requesters read against services (Profiles.CheckRequesters). A file
// either refuses is a test bug.
func Profiles(t testing.TB, file string, services ...string) policy.Profiles {
	t.Helper()
	profiles, err := policy.ParseProfiles([]byte(file))
	if err != nil {
		t.Fatalf("policy.ParseProfiles: %v", err)
	}
	if err := profiles.CheckRequesters(services); err != nil {
		t.Fatalf("Profiles.CheckRequesters: %v", err)
	}
	return profiles
}

// ProfiledCurrent is Current with file as the deployment's profiles file (Profiles), so the
// policy it loads serves those profiles and refuses each that fails against its source.
func ProfiledCurrent(t testing.TB, store secretstore.Store, file string, services ...string) *policy.Current {
	t.Helper()
	loader := Loader(store, services...)
	loader.Profiles = Profiles(t, file, services...)
	return current(t, loader)
}

// AppProfile is the profile that serves the github-app secret name by its own name, as the
// broker serves every App it releases (policy.ReasonGitHubAppNoProfile refuses one no profile
// names): a github block on the accounts TypeSecret's github-owners tag names
// (FixtureGitHubOwners), with the metadata read GitHub gives every App, and no proxy block.
func AppProfile(name string) string {
	return `{"name": "` + name + `", "github": {"accounts": ["acme", "ada"], "permissions": {"metadata": "read"}}}`
}

// ProfilesFile is the profiles file holding profiles, each one entry's JSON, in order.
func ProfilesFile(profiles ...string) string {
	return `{"profiles": [` + strings.Join(profiles, ", ") + `]}`
}

// Kinds is a kinds list a test's profiles file declares (KindsFile), one line of JSON: example, on
// api.example.com, whose ways take a stored value, a minted token and an exchange alike, so a
// secret of any type is served behind it; github, on GitHub's three hosts; gcp, on Google's APIs;
// slack; aws, which signs a stored key pair; and anthropic as the spec's anthropic kind declares
// it, on api.anthropic.com, with its three allow rows under /v1/organizations/ and a max_grant of
// an hour. No kind but anthropic sets an allow or a max_grant, so a secret behind one of the
// others is held to nothing a test has not set itself.
const Kinds = `[` +
	`{"name": "example", "hosts": ["api.example.com"], "ways": ["swap", "mint", "exchange"]}, ` +
	`{"name": "github", "hosts": ["api.github.com", "github.com", "uploads.github.com"], "headers": ["Authorization"], "variables": ["GH_TOKEN", "GITHUB_TOKEN"], "ways": ["swap", "mint"]}, ` +
	`{"name": "gcp", "hosts": ["*.googleapis.com", "*.pkg.dev"], "headers": ["Authorization"], "ways": ["mint"]}, ` +
	`{"name": "slack", "hosts": ["slack.com"], "headers": ["Authorization"], "variables": ["SLACK_BOT_TOKEN"], "ways": ["swap"]}, ` +
	`{"name": "aws", "hosts": ["*.amazonaws.com"], "ways": ["sign"]}, ` +
	`{"name": "anthropic", "hosts": ["api.anthropic.com"], "headers": ["Authorization"], "variables": ["ANTHROPIC_ADMIN_TOKEN"], "ways": ["mint"], ` +
	`"allow": [{"method": "GET", "pathPrefix": "/v1/organizations/"}, {"method": "POST", "pathPrefix": "/v1/organizations/"}, {"method": "DELETE", "pathPrefix": "/v1/organizations/"}], "max_grant": "1h"}` +
	`]`

// KindsFile is ProfilesFile with Kinds declared after its profiles, on the line they end on, so
// a profile may name a kind of Kinds as its proxy string and a secret's proxy tag may name one.
func KindsFile(profiles ...string) string {
	return KindsFileOf(Kinds, profiles...)
}

// KindsFileOf is the profiles file holding profiles, each one entry's JSON, in order, and kinds, a
// JSON list of kinds, after them on the line they end on.
func KindsFileOf(kinds string, profiles ...string) string {
	return `{"profiles": [` + strings.Join(profiles, ", ") + `], "kinds": ` + kinds + `}`
}

// Proxied is s with a proxy tag naming kind (policy.TagProxy), which puts it behind Cipher's proxy
// under that kind of the profiles file.
func Proxied(s memory.Secret, kind string) memory.Secret {
	s.Tags[policy.TagProxy] = kind
	return s
}

// Narrowed is s with the proxy-narrowed tag (policy.TagProxyNarrowed), which has the policy serve it
// through exactly one profile, and one with an allow.
func Narrowed(s memory.Secret) memory.Secret {
	s.Tags[policy.TagProxyNarrowed] = policy.ProxyNarrowedTrue
	return s
}

// MeteredCurrent is Current with the policy's two metrics on registry, as cmd/cipherd/main.go
// registers them: broker_policy_store_reads_total counted from the first load on
// (policy.NewStoreReads), and broker_unservable_secrets once that load answered
// (policy.NewUnservableSecrets).
func MeteredCurrent(t testing.TB, store secretstore.Store, registry *metrics.Registry, services ...string) *policy.Current {
	t.Helper()
	return MeteredProfiledCurrent(t, store, registry, "", services...)
}

// MeteredProfiledCurrent is MeteredCurrent with file as the deployment's profiles file (Profiles),
// or none for "", as BROKER_PROFILES_FILE is set or not.
func MeteredProfiledCurrent(t testing.TB, store secretstore.Store, registry *metrics.Registry, file string, services ...string) *policy.Current {
	t.Helper()
	loader := Loader(store, services...)
	if file != "" {
		loader.Profiles = Profiles(t, file, services...)
	}
	return Metered(t, loader, registry)
}

// Metered is loader's policy with the policy's two metrics on registry, as MeteredCurrent says,
// for a caller that sets more of the loader than Loader does, as cmd/cipherd/main.go sets its
// ProjectChecks.
func Metered(t testing.TB, loader policy.Loader, registry *metrics.Registry) *policy.Current {
	t.Helper()
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
