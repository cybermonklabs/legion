package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// natsSeeds writes a user, an account and a curve seed to files of the test's own and returns
// their paths, and the seeds themselves, which no refusal may print.
func natsSeeds(t *testing.T) (user, account, curve string, seeds []string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, create func() (nkeys.KeyPair, error)) string {
		pair, err := create()
		if err != nil {
			t.Fatal(err)
		}
		seed, err := pair.Seed()
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, string(seed))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(seed, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return write("user.seed", nkeys.CreateUser), write("issuer.seed", nkeys.CreateAccount), write("xkey.seed", nkeys.CreateCurveKeys), seeds
}

// The NATS auth service's settings: none set is no service; any set needs the hub's URLs and the
// three seed files, each holding a seed of its own kind; an audience for a kind of platform token
// needs that kind's issuer; and the shared Envoy bearer is gone, its variables refused naming the
// account token file that replaces it.
func TestNATSCalloutSettings(t *testing.T) {
	user, account, curve, seeds := natsSeeds(t)
	full := func() map[string]string {
		e := validEnv()
		e["BROKER_NATS_URLS"] = "nats://hub-a:4222, nats://hub-b:4222"
		e["BROKER_NATS_SEED_FILE"] = user
		e["BROKER_NATS_ISSUER_SEED_FILE"] = account
		e["BROKER_NATS_XKEY_SEED_FILE"] = curve
		e["BROKER_NATS_SERVICES"] = "listener/dev-stack/*=system:serviceaccount:example-ns:example-listener"
		return e
	}

	t.Run("none set is no service", func(t *testing.T) {
		cfg, err := Load(env(validEnv()))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.NATS.Enabled() || len(cfg.NATS.URLs) != 0 || cfg.NATS.SeedFile != "" {
			t.Fatalf("an environment naming no NATS setting configured %+v", cfg.NATS)
		}
	})
	t.Run("every setting", func(t *testing.T) {
		e := full()
		e["BROKER_K8S_OIDC_ISSUER"], e["BROKER_K8S_OIDC_AUDIENCE"] = "https://issuer.example", "broker"
		e["BROKER_NATS_K8S_AUDIENCE"] = "envoy-nats"
		e["BROKER_NATS_GOOGLE_AUDIENCE"] = "envoy-nats-google"
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		want := NATSCallout{
			URLs: []string{"nats://hub-a:4222", "nats://hub-b:4222"}, SeedFile: user, IssuerSeedFile: account, XKeySeedFile: curve,
			Services: e["BROKER_NATS_SERVICES"], K8sAudience: "envoy-nats", GoogleAudience: "envoy-nats-google",
		}
		if !cfg.NATS.Enabled() || strings.Join(cfg.NATS.URLs, " ") != strings.Join(want.URLs, " ") ||
			cfg.NATS.SeedFile != want.SeedFile || cfg.NATS.IssuerSeedFile != want.IssuerSeedFile || cfg.NATS.XKeySeedFile != want.XKeySeedFile ||
			cfg.NATS.Services != want.Services || cfg.NATS.K8sAudience != want.K8sAudience || cfg.NATS.GoogleAudience != want.GoogleAudience {
			t.Fatalf("read %+v, want %+v", cfg.NATS, want)
		}
	})

	refusals := []struct {
		name  string
		edit  func(map[string]string)
		names string
	}{
		{"no URLs", func(e map[string]string) { delete(e, "BROKER_NATS_URLS") }, "BROKER_NATS_URLS"},
		{"no seed file", func(e map[string]string) { delete(e, "BROKER_NATS_SEED_FILE") }, "BROKER_NATS_SEED_FILE"},
		{"no issuer seed file", func(e map[string]string) { delete(e, "BROKER_NATS_ISSUER_SEED_FILE") }, "BROKER_NATS_ISSUER_SEED_FILE"},
		{"no xkey seed file", func(e map[string]string) { delete(e, "BROKER_NATS_XKEY_SEED_FILE") }, "BROKER_NATS_XKEY_SEED_FILE"},
		{"only the services", func(e map[string]string) {
			for _, name := range []string{"BROKER_NATS_URLS", "BROKER_NATS_SEED_FILE", "BROKER_NATS_ISSUER_SEED_FILE", "BROKER_NATS_XKEY_SEED_FILE"} {
				delete(e, name)
			}
		}, "BROKER_NATS_URLS"},
		{"a user seed in the issuer file", func(e map[string]string) { e["BROKER_NATS_ISSUER_SEED_FILE"] = user }, "BROKER_NATS_ISSUER_SEED_FILE"},
		{"a user seed in the xkey file", func(e map[string]string) { e["BROKER_NATS_XKEY_SEED_FILE"] = user }, "BROKER_NATS_XKEY_SEED_FILE"},
		{"an account seed in the seed file", func(e map[string]string) { e["BROKER_NATS_SEED_FILE"] = account }, "BROKER_NATS_SEED_FILE"},
		{"a seed file that is not there", func(e map[string]string) { e["BROKER_NATS_SEED_FILE"] = user + ".missing" }, "BROKER_NATS_SEED_FILE"},
		{"a Kubernetes audience with no Kubernetes issuer", func(e map[string]string) { e["BROKER_NATS_K8S_AUDIENCE"] = "envoy-nats" }, "BROKER_K8S_OIDC_ISSUER"},
		{"the shared Envoy bearer", func(e map[string]string) { e["BROKER_ENVOY_TOKEN"] = "shared" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
		{"the shared Envoy bearer's file", func(e map[string]string) { e["BROKER_ENVOY_TOKEN_FILE"] = "/run/token" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
		{"an Envoy URL with no account token", func(e map[string]string) { e["BROKER_ENVOY_URL"] = "https://listener.example" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
	}
	for _, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			e := full()
			row.edit(e)
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), row.names) {
				t.Fatalf("want a refusal naming %s, got %v", row.names, err)
			}
			for _, seed := range seeds {
				if strings.Contains(err.Error(), seed) {
					t.Fatalf("the refusal prints a seed: %v", err)
				}
			}
		})
	}
	// A hub URL that carries user info is refused at load, by its place in the list and never its
	// text: the service connects as the user its seed names, so user info is a credential it does
	// not use, and the broker writes the URLs into its start line and the service's connect error.
	// nats.go dials an entry with no scheme as nats://<entry> (nats.go, parseServerURL), so such an
	// entry is read the same way, and an entry that does not parse as a URL, whose parse error
	// would quote it whole, is refused too. Each URL is built from a random secret of this run, so
	// no credential-shaped URL is written in the source, and the refusal must hold none of it.
	secret := rand.Text()
	userAndPassword := (&url.URL{Scheme: "nats", User: url.UserPassword("svc", secret), Host: "hub-b:4222"}).String()
	for _, row := range []struct{ name, urls, names string }{
		{"a user and password", "nats://hub-a:4222, " + userAndPassword, "BROKER_NATS_URLS entry 2"},
		{"a token as the user", (&url.URL{Scheme: "nats", User: url.User(secret), Host: "hub-a:4222"}).String(), "BROKER_NATS_URLS entry 1"},
		{"user info with no scheme", strings.TrimPrefix(userAndPassword, "nats://"), "BROKER_NATS_URLS entry 1"},
		{"no URL at all", strings.Replace(userAndPassword, secret, secret+"%zz", 1), "BROKER_NATS_URLS entry 1"},
	} {
		t.Run("a hub URL with "+row.name, func(t *testing.T) {
			e := full()
			e["BROKER_NATS_URLS"] = row.urls
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), row.names) {
				t.Fatalf("want a refusal naming %s, got %v", row.names, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the refusal prints the URL's user info: %v", err)
			}
		})
	}
	t.Run("an Envoy URL and its account token file", func(t *testing.T) {
		e := validEnv()
		e["BROKER_ENVOY_URL"] = "https://listener.example"
		e["BROKER_ENVOY_ACCOUNT_TOKEN_FILE"] = "/var/run/secrets/tokens/envoy"
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.EnvoyAccountTokenFile != "/var/run/secrets/tokens/envoy" {
			t.Fatalf("read %q", cfg.EnvoyAccountTokenFile)
		}
	})
}

// holdKey is a fresh Ed25519 public key and its text as BROKER_NATS_HOLD_SIGNING_KEYS writes it,
// the unpadded base64url of its 32 bytes.
func holdKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, base64.RawURLEncoding.EncodeToString(public)
}

// The writer hold's three settings: unset, the auth service reads no hold row, the routes trust no
// key and the hold's pool would be 2; set, the switch is true or false and nothing else, every key
// of the list is read in order whatever whitespace or commas part them, and the pool is a whole
// number of connections from 1 to 10. A key that is not the unpadded base64url of 32 bytes is
// refused by its place in the list and never its text, since a deployment that pasted a private
// key there must not find it in the broker's log.
func TestNATSHoldSettings(t *testing.T) {
	first, firstText := holdKey(t)
	second, secondText := holdKey(t)
	third, thirdText := holdKey(t)

	t.Run("unset", func(t *testing.T) {
		cfg, err := Load(env(validEnv()))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.NATS.HoldRead || cfg.NATS.HoldSigningKeys != nil || cfg.NATS.HoldPoolSize != 2 {
			t.Fatalf("an environment naming no hold setting read HoldRead %v, %d keys, pool %d; want false, none and 2",
				cfg.NATS.HoldRead, len(cfg.NATS.HoldSigningKeys), cfg.NATS.HoldPoolSize)
		}
	})
	t.Run("every setting", func(t *testing.T) {
		e := validEnv()
		e["BROKER_NATS_HOLD_READ"] = "true"
		e["BROKER_NATS_HOLD_SIGNING_KEYS"] = " " + firstText + ",\n" + secondText + " ,, " + thirdText + "\t"
		e["BROKER_NATS_HOLD_POOL_SIZE"] = "5"
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.NATS.HoldRead || cfg.NATS.HoldPoolSize != 5 {
			t.Fatalf("read HoldRead %v and pool %d, want true and 5", cfg.NATS.HoldRead, cfg.NATS.HoldPoolSize)
		}
		want := []ed25519.PublicKey{first, second, third}
		if len(cfg.NATS.HoldSigningKeys) != len(want) {
			t.Fatalf("read %d keys, want %d", len(cfg.NATS.HoldSigningKeys), len(want))
		}
		for i, key := range want {
			if !key.Equal(cfg.NATS.HoldSigningKeys[i]) {
				t.Fatalf("key %d is not the key the list names in that place", i+1)
			}
		}
		e["BROKER_NATS_HOLD_READ"] = "false"
		if cfg, err = Load(env(e)); err != nil || cfg.NATS.HoldRead {
			t.Fatalf("BROKER_NATS_HOLD_READ=false read HoldRead %v, %v; want false", cfg.NATS.HoldRead, err)
		}
	})

	padded := base64.URLEncoding.EncodeToString(first)
	short := base64.RawURLEncoding.EncodeToString(first[:31])
	// 32 bytes of 0xff, whose standard base64 is all '/' but its last character, which no base64url
	// text holds.
	standard := base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("\xff", ed25519.PublicKeySize)))
	for _, row := range []struct {
		name, setting, value, names string
		hidden                      []string
	}{
		{"a switch that is neither true nor false", "BROKER_NATS_HOLD_READ", "yes", "BROKER_NATS_HOLD_READ", nil},
		{"a key that is no base64url", "BROKER_NATS_HOLD_SIGNING_KEYS", firstText + " not*a*key", "BROKER_NATS_HOLD_SIGNING_KEYS entry 2", []string{"not*a*key"}},
		{"a padded key", "BROKER_NATS_HOLD_SIGNING_KEYS", padded, "BROKER_NATS_HOLD_SIGNING_KEYS entry 1", []string{padded}},
		{"a key of 31 bytes", "BROKER_NATS_HOLD_SIGNING_KEYS", secondText + "," + short, "BROKER_NATS_HOLD_SIGNING_KEYS entry 2", []string{short}},
		{"a key in standard base64", "BROKER_NATS_HOLD_SIGNING_KEYS", standard, "BROKER_NATS_HOLD_SIGNING_KEYS entry 1", []string{standard}},
		{"a pool of none", "BROKER_NATS_HOLD_POOL_SIZE", "0", "BROKER_NATS_HOLD_POOL_SIZE", nil},
		{"a pool above the broker's own", "BROKER_NATS_HOLD_POOL_SIZE", "11", "BROKER_NATS_HOLD_POOL_SIZE", nil},
		{"a pool that is no number", "BROKER_NATS_HOLD_POOL_SIZE", "two", "BROKER_NATS_HOLD_POOL_SIZE", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := validEnv()
			e[row.setting] = row.value
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), row.names) {
				t.Fatalf("want a refusal naming %s, got %v", row.names, err)
			}
			for _, text := range row.hidden {
				if strings.Contains(err.Error(), text) {
					t.Fatalf("the refusal prints the entry's text %q: %v", text, err)
				}
			}
		})
	}
}
