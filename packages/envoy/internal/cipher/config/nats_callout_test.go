package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// entryTexts is each parsed entry's text, as the setting gave it, joined by single spaces.
func entryTexts(services []NATSService) string {
	texts := make([]string, len(services))
	for index, entry := range services {
		texts[index] = entry.Text
	}
	return strings.Join(texts, " ")
}

// The NATS auth service's settings: none set is no service, and neither is BROKER_NATS_SERVICES
// alone holding only edge listeners' entries; any other set needs the hub's URLs and the three
// seed files, each holding a seed of its own kind; an audience for a kind of platform token needs
// that kind's issuer, and an edge listener's account the issuer its identity token comes from; the
// hub account is $G unset, and refused naming its setting and never its value unless it is $G or
// one subject token of at most 64 characters that holds no nkey seed in any spelling, nor JETSTREAM;
// and the shared Envoy bearer is gone, its variables refused naming the account token file that
// replaces it.
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
			K8sAudience: "envoy-nats", GoogleAudience: "envoy-nats-google", Account: GlobalNATSAccount,
		}
		if !cfg.NATS.Enabled() || strings.Join(cfg.NATS.URLs, " ") != strings.Join(want.URLs, " ") ||
			cfg.NATS.SeedFile != want.SeedFile || cfg.NATS.IssuerSeedFile != want.IssuerSeedFile || cfg.NATS.XKeySeedFile != want.XKeySeedFile ||
			entryTexts(cfg.NATS.Services) != e["BROKER_NATS_SERVICES"] || cfg.NATS.K8sAudience != want.K8sAudience || cfg.NATS.GoogleAudience != want.GoogleAudience ||
			cfg.NATS.Account != want.Account {
			t.Fatalf("read %+v, want %+v with the services %s", cfg.NATS, want, e["BROKER_NATS_SERVICES"])
		}
	})
	// BROKER_NATS_ACCOUNT is the account every client the service admits is placed in: $G unset,
	// and as given when it is $G or one subject token of at most 64 characters, its surrounding
	// space trimmed as every other setting's is.
	longest := strings.Repeat("e", 64)
	for raw, want := range map[string]string{"$G": GlobalNATSAccount, "ENVOY": "ENVOY", " ENVOY ": "ENVOY", "envoy_clients-2": "envoy_clients-2", longest: longest} {
		t.Run("the account "+strconv.Quote(raw), func(t *testing.T) {
			e := full()
			e[NATSAccountVariable] = raw
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NATS.Account != want {
				t.Fatalf("%s %q read as account %q, want %q", NATSAccountVariable, raw, cfg.NATS.Account, want)
			}
		})
	}
	// BROKER_NATS_SERVICES alone runs no auth service, and is read for the edge listeners its
	// entries name (GET /v1/project-repositories): with every entry an edge-listener one, it loads
	// as it was given and the service stays off.
	t.Run("only edge-listener services is no service", func(t *testing.T) {
		e := validEnv()
		e["BROKER_K8S_OIDC_ISSUER"], e["BROKER_K8S_OIDC_AUDIENCE"] = "https://issuer.example", "broker"
		e["BROKER_NATS_SERVICES"] = "edge-listener/edge-a/PRIV=system:serviceaccount:example-ns:example-edge-listener"
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.NATS.Enabled() || entryTexts(cfg.NATS.Services) != e["BROKER_NATS_SERVICES"] || len(cfg.NATS.URLs) != 0 || cfg.NATS.SeedFile != "" {
			t.Fatalf("an environment naming only edge-listener services configured %+v, want them read and no service", cfg.NATS)
		}
	})
	// The cluster issuer a Kubernetes edge listener needs is BROKER_K8S_OIDC_ISSUER in either form:
	// named entries do as well as one URL, and the edge listener's entry names its own among them,
	// which the broker's API reads it with.
	t.Run("a Kubernetes edge listener beside named cluster issuers", func(t *testing.T) {
		e := namedIssuersEnv()
		e["BROKER_NATS_SERVICES"] = "edge-listener/edge-a/PRIV=system:serviceaccount:example-ns:example-edge-listener=" + secondIssuer.Name
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		if edges := cfg.NATS.EdgeListeners(); len(edges) != 1 || !slices.Equal(edges[0].Issuers, []string{secondIssuer.Name}) {
			t.Fatalf("an edge listener naming the issuer %s read as %+v, want that issuer alone", secondIssuer.Name, edges)
		}
	})
	// An edge listener is a caller of the broker's API, so its entry needs the issuer whose
	// identity token its account presents, as a BROKER_API_ACCOUNTS entry of the same kind does.
	// A Google entry is a Google caller: it alone lets BROKER_GOOGLE_OIDC_AUDIENCE be set, and it
	// is refused without that audience. A Kubernetes entry is refused without the cluster issuer.
	t.Run("a Google edge listener is the audience's Google caller", func(t *testing.T) {
		e := validEnv()
		e["BROKER_GOOGLE_OIDC_AUDIENCE"] = "https://broker.example/machines"
		e["BROKER_NATS_SERVICES"] = "edge-listener/edge-g/PRIV=google:example-edge@example-project.iam.gserviceaccount.com"
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GoogleOIDCAudience != e["BROKER_GOOGLE_OIDC_AUDIENCE"] || entryTexts(cfg.NATS.Services) != e["BROKER_NATS_SERVICES"] || cfg.NATS.Enabled() {
			t.Fatalf("a Google edge listener with its audience configured audience %q and %+v", cfg.GoogleOIDCAudience, cfg.NATS)
		}
	})
	for name, row := range map[string]struct {
		edit  map[string]string
		names string
	}{
		"a Google edge listener with no Google audience": {map[string]string{
			"BROKER_NATS_SERVICES": "edge-listener/edge-g/PRIV=google:example-edge@example-project.iam.gserviceaccount.com",
		}, "BROKER_GOOGLE_OIDC_AUDIENCE"},
		"a Kubernetes edge listener with no cluster issuer": {map[string]string{
			"BROKER_NATS_SERVICES": "edge-listener/edge-a/PRIV=system:serviceaccount:example-ns:example-edge-listener",
		}, "BROKER_K8S_OIDC_ISSUER"},
	} {
		t.Run(name, func(t *testing.T) {
			e := validEnv()
			for key, value := range row.edit {
				e[key] = value
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), row.names) || !strings.Contains(err.Error(), "edge-listener") {
				t.Fatalf("want a refusal naming %s and the edge-listener entry, got %v", row.names, err)
			}
		})
	}
	// An entry is parsed once, at load, and every rule that reads an entry's kind or account reads
	// that parse, so a malformed entry is refused for its form before any of them. An edge
	// listener's entry with no account, with no cluster issuer set, is refused as no entry, never
	// as a Kubernetes edge listener that needs the cluster's issuer; and a malformed entry beside the
	// auth service's settings is refused at load too, where it used to wait for the boot.
	for name, row := range map[string]struct {
		env   func() map[string]string
		entry string
	}{
		"an edge listener's entry with no account, and no cluster issuer": {validEnv, "edge-listener/edge-a/PRIV"},
		"a listener's entry with no account, beside the auth service":     {full, "listener/dev-stack/*"},
		"an edge listener's entry with two machine ids":                   {validEnv, "edge-listener/edge-a/edge-b/PRIV=system:serviceaccount:example-ns:example-edge-listener"},
	} {
		t.Run(name, func(t *testing.T) {
			e := row.env()
			e["BROKER_NATS_SERVICES"] = row.entry
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), "BROKER_NATS_SERVICES must be <kind>/") || !strings.Contains(err.Error(), strconv.Quote(row.entry)) {
				t.Fatalf("want the entry form's refusal quoting %q, got %v", row.entry, err)
			}
			if strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
				t.Fatalf("the refusal of an entry that names no account asks for the cluster issuer: %v", err)
			}
		})
	}

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
		{"only the services, an edge listener among them", func(e map[string]string) {
			for _, name := range []string{"BROKER_NATS_URLS", "BROKER_NATS_SEED_FILE", "BROKER_NATS_ISSUER_SEED_FILE", "BROKER_NATS_XKEY_SEED_FILE"} {
				delete(e, name)
			}
			e["BROKER_NATS_SERVICES"] = "edge-listener/edge-a/PRIV=system:serviceaccount:example-ns:example-edge-listener " + e["BROKER_NATS_SERVICES"]
		}, "BROKER_NATS_URLS"},
		{"edge-listener services and a seed file", func(e map[string]string) {
			for _, name := range []string{"BROKER_NATS_URLS", "BROKER_NATS_ISSUER_SEED_FILE", "BROKER_NATS_XKEY_SEED_FILE"} {
				delete(e, name)
			}
			e["BROKER_NATS_SERVICES"] = "edge-listener/edge-a/PRIV=system:serviceaccount:example-ns:example-edge-listener"
		}, "BROKER_NATS_URLS"},
		{"a user seed in the issuer file", func(e map[string]string) { e["BROKER_NATS_ISSUER_SEED_FILE"] = user }, "BROKER_NATS_ISSUER_SEED_FILE"},
		{"a user seed in the xkey file", func(e map[string]string) { e["BROKER_NATS_XKEY_SEED_FILE"] = user }, "BROKER_NATS_XKEY_SEED_FILE"},
		{"an account seed in the seed file", func(e map[string]string) { e["BROKER_NATS_SEED_FILE"] = account }, "BROKER_NATS_SEED_FILE"},
		{"a seed file that is not there", func(e map[string]string) { e["BROKER_NATS_SEED_FILE"] = user + ".missing" }, "BROKER_NATS_SEED_FILE"},
		{"a Kubernetes audience with no Kubernetes issuer", func(e map[string]string) { e["BROKER_NATS_K8S_AUDIENCE"] = "envoy-nats" }, "BROKER_K8S_OIDC_ISSUER"},
		{"the shared Envoy bearer", func(e map[string]string) { e["BROKER_ENVOY_TOKEN"] = "shared" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
		{"the shared Envoy bearer's file", func(e map[string]string) { e["BROKER_ENVOY_TOKEN_FILE"] = "/run/token" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
		{"an Envoy URL with no account token", func(e map[string]string) { e["BROKER_ENVOY_URL"] = "https://listener.example" }, "BROKER_ENVOY_ACCOUNT_TOKEN_FILE"},
		{"only the account", func(e map[string]string) {
			for _, name := range []string{"BROKER_NATS_URLS", "BROKER_NATS_SEED_FILE", "BROKER_NATS_ISSUER_SEED_FILE", "BROKER_NATS_XKEY_SEED_FILE", "BROKER_NATS_SERVICES"} {
				delete(e, name)
			}
			e[NATSAccountVariable] = "ENVOY"
		}, "BROKER_NATS_URLS"},
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
	// A malformed BROKER_NATS_ACCOUNT refuses the start naming the setting and never the value,
	// so a credential set there by mistake reaches neither the refusal nor, accepted, the boot
	// record: a value of two tokens, a space, a wildcard, the system account, one past 64
	// characters, an nkey seed in any spelling that stays one subject token of at most 64
	// characters (as nkeys spells it, in lower case, behind a prefix, and split by '-'), each of
	// which the token rule admits and bus.HoldsSeed finds, and a URL carrying a password.
	// JETSTREAM, the account a private render keeps JetStream in, is refused by name: the rule
	// names it.
	password := rand.Text()
	seed := seeds[0]
	accountRefusals := map[string]string{
		"an account of two tokens":                     "a.b",
		"an account with a space":                      "ENVOY X",
		"an account that is a wildcard":                ">",
		"the system account":                           "$SYS",
		"an account past 64 characters":                longest + "e",
		"an account that is an nkey seed":              seed,
		"an account that is a lower-case nkey seed":    strings.ToLower(seed),
		"an account that is a prefixed nkey seed":      "acct_" + seed,
		"an account that is an nkey seed split by '-'": seed[:29] + "-" + seed[29:],
		"an account that is a URL":                     (&url.URL{Scheme: "nats", User: url.UserPassword("svc", password), Host: "hub-b:4222"}).String(),
		"the account JetStream sits in":                JetStreamNATSAccount,
	}
	for name, raw := range accountRefusals {
		t.Run(name, func(t *testing.T) {
			e := full()
			e[NATSAccountVariable] = raw
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), NATSAccountVariable) {
				t.Fatalf("want a refusal naming %s, got %v", NATSAccountVariable, err)
			}
			if raw != JetStreamNATSAccount && strings.Contains(err.Error(), raw) {
				t.Fatalf("the refusal prints the value it refused: %v", err)
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
