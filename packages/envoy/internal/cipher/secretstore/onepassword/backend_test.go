package onepassword_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// TestParseConfig: the 1Password store's entry requires its three variables, refuses each that
// is not of its form and a prefix that is no title prefix, each refusal naming its variable, and
// answers the vault as its settings field.
func TestParseConfig(t *testing.T) {
	good := map[string]string{
		"BROKER_ONEPASSWORD_VAULT":          testVault,
		"BROKER_ONEPASSWORD_TOKEN_FILE":     "/run/secrets/onepassword-token",
		"BROKER_ONEPASSWORD_DAILY_REQUESTS": "900",
	}
	with := func(name, value string) func(string) string {
		vars := maps.Clone(good)
		if value == "" {
			delete(vars, name)
		} else {
			vars[name] = value
		}
		return func(n string) string { return vars[n] }
	}
	for _, c := range []struct {
		name, value, prefix, want string
	}{
		{"BROKER_ONEPASSWORD_VAULT", "", testPrefix, "BROKER_ONEPASSWORD_VAULT is required with BROKER_SECRETS_STORE=onepassword"},
		{"BROKER_ONEPASSWORD_TOKEN_FILE", "", testPrefix, "BROKER_ONEPASSWORD_TOKEN_FILE is required with BROKER_SECRETS_STORE=onepassword"},
		{"BROKER_ONEPASSWORD_DAILY_REQUESTS", "", testPrefix, "BROKER_ONEPASSWORD_DAILY_REQUESTS is required with BROKER_SECRETS_STORE=onepassword: the account's type and its other service accounts decide it"},
		{"BROKER_ONEPASSWORD_VAULT", "Agent", testPrefix, `BROKER_ONEPASSWORD_VAULT must be a 1Password vault id, 26 lowercase letters and digits, got "Agent"`},
		{"BROKER_ONEPASSWORD_VAULT", testVault + "a", testPrefix, `BROKER_ONEPASSWORD_VAULT must be a 1Password vault id, 26 lowercase letters and digits, got "` + testVault + `a"`},
		{"BROKER_ONEPASSWORD_DAILY_REQUESTS", "9", testPrefix, `BROKER_ONEPASSWORD_DAILY_REQUESTS must be a whole number from 10 to 1000000, got "9"`},
		{"BROKER_ONEPASSWORD_DAILY_REQUESTS", "1000001", testPrefix, `BROKER_ONEPASSWORD_DAILY_REQUESTS must be a whole number from 10 to 1000000, got "1000001"`},
		{"BROKER_ONEPASSWORD_DAILY_REQUESTS", "900.5", testPrefix, `BROKER_ONEPASSWORD_DAILY_REQUESTS must be a whole number from 10 to 1000000, got "900.5"`},
	} {
		_, err := onepassword.Backend{}.ParseConfig(c.prefix, with(c.name, c.value))
		if err == nil || err.Error() != c.want {
			t.Errorf("ParseConfig with %s=%q = %v, want %q", c.name, c.value, err, c.want)
		}
	}
	for _, prefix := range []string{"", "cipher", "Cipher-", "cipher_", "production/agent-secrets/", "cipher -", strings.Repeat("a", 64) + "-"} {
		_, err := onepassword.Backend{}.ParseConfig(prefix, with("", ""))
		want := "BROKER_SECRETS_PREFIX must be a 1Password title prefix of lowercase letters, digits and hyphens ending in -, at most 64 bytes, such as cipher-, got " + strconv.Quote(prefix)
		if err == nil || err.Error() != want {
			t.Errorf("ParseConfig(prefix %q) = %v, want %q", prefix, err, want)
		}
	}
	for _, prefix := range []string{testPrefix, "agent-secrets-", strings.Repeat("a", 63) + "-"} {
		cfg, err := onepassword.Backend{}.ParseConfig(prefix, with("", ""))
		if err != nil {
			t.Fatalf("ParseConfig(prefix %q): %v", prefix, err)
		}
		if want := map[string]string{"onepassword_vault": testVault}; cfg.Store() != onepassword.Name || cfg.Prefix() != prefix || !maps.Equal(cfg.Settings(), want) {
			t.Errorf("Config = %s %s %v; want %s, %s and %v", cfg.Store(), cfg.Prefix(), cfg.Settings(), onepassword.Name, prefix, want)
		}
	}
}

// TestTheBackendIsTheOnePasswordStore: the registry entry names the store onepassword, keeps the
// SDK to its own package, is no development store, reads its three variables, and holds the fleet
// to one reread every 10 seconds with a burst of four.
func TestTheBackendIsTheOnePasswordStore(t *testing.T) {
	b := onepassword.Backend{}
	if b.Name() != "onepassword" || b.ID() != "onepassword" || b.Development() {
		t.Errorf("Backend is %s (id %s), development %t; want onepassword, onepassword, no development store", b.Name(), b.ID(), b.Development())
	}
	if got := b.ServiceSDKs(); !slices.Equal(got, []string{"github.com/1password/onepassword-sdk-go"}) {
		t.Errorf("ServiceSDKs = %v, want the 1Password SDK", got)
	}
	if got, want := b.Variables(), []string{"BROKER_ONEPASSWORD_VAULT", "BROKER_ONEPASSWORD_TOKEN_FILE", "BROKER_ONEPASSWORD_DAILY_REQUESTS"}; !slices.Equal(got, want) {
		t.Errorf("Variables = %v, want %v", got, want)
	}
	if got, want := b.RereadBudget(), (ratelimit.Limit{Every: 10 * time.Second, Burst: 4}); got != want {
		t.Errorf("RereadBudget = %+v, want %+v", got, want)
	}
	const item = "item000000000000000000000a"
	if got, want := onepassword.Source(testVault, item), onepassword.Name+":"+testVault+"/"+item; got != want {
		t.Errorf("Source = %s, want %s", got, want)
	}
}

// TestOpenStoreRefusesATokenFileItCannotUse: a token file that cannot be read, and one that holds
// only white space, are refused naming BROKER_ONEPASSWORD_TOKEN_FILE, before any 1Password call.
func TestOpenStoreRefusesATokenFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank")
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	for file, want := range map[string]string{
		missing: "read BROKER_ONEPASSWORD_TOKEN_FILE: open " + missing + ": no such file or directory",
		blank:   "BROKER_ONEPASSWORD_TOKEN_FILE names " + blank + ", which holds no token",
	} {
		vars := map[string]string{"BROKER_ONEPASSWORD_VAULT": testVault, "BROKER_ONEPASSWORD_TOKEN_FILE": file, "BROKER_ONEPASSWORD_DAILY_REQUESTS": "900"}
		cfg, err := onepassword.Backend{}.ParseConfig(testPrefix, func(n string) string { return vars[n] })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (onepassword.Backend{}).OpenStore(context.Background(), cfg, secretstore.OpenEnv{}); err == nil || err.Error() != want {
			t.Errorf("OpenStore with the token file %s = %v, want %q", file, err, want)
		}
	}
}
