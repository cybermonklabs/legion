package config

import (
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/natsgrant"
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

// No audience a token of the cluster's issuer is accepted at may be the hub-admission audience
// (natsgrant.HubAdmissionAudience): the auth service verifies a token of that audience as
// Dispatch's hub-admission proof alone, so a setting equal to it would make a proof token
// spendable there, or a token minted for there a proof. Each of the four is refused at start,
// naming it and the audience; the control is the same environment with each audience its own,
// which loads.
func TestNoAudienceIsTheHubAdmissionAudience(t *testing.T) {
	user, account, curve, _ := natsSeeds(t)
	audiences := func(set map[string]string) map[string]string {
		e := withK8sIssuer()
		e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
		e["BROKER_NATS_URLS"] = "nats://hub-a:4222"
		e["BROKER_NATS_SEED_FILE"], e["BROKER_NATS_ISSUER_SEED_FILE"], e["BROKER_NATS_XKEY_SEED_FILE"] = user, account, curve
		e["BROKER_NATS_K8S_AUDIENCE"] = "envoy-nats"
		e["BROKER_POD_REGISTRATION_AUDIENCE"] = "legion-broker-pod-registration"
		for name, value := range set {
			e[name] = value
		}
		return e
	}
	if _, err := Load(env(audiences(nil))); err != nil {
		t.Fatalf("the control, each audience its own, was refused: %v", err)
	}
	for _, setting := range []string{"BROKER_K8S_OIDC_AUDIENCE", "BROKER_EXCHANGE_K8S_AUDIENCE", "BROKER_NATS_K8S_AUDIENCE", "BROKER_POD_REGISTRATION_AUDIENCE"} {
		t.Run(setting, func(t *testing.T) {
			_, err := Load(env(audiences(map[string]string{setting: natsgrant.HubAdmissionAudience})))
			if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), natsgrant.HubAdmissionAudience) {
				t.Fatalf("Load = %v, want a refusal naming %s and %s", err, setting, natsgrant.HubAdmissionAudience)
			}
		})
	}
}
