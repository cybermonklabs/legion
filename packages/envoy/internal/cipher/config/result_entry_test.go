package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// withResultsAirlock is e with the airlock's four settings: the rules file a result entry's
// grants need and the three the proxy cannot run without. Load reads none of the files.
func withResultsAirlock(e map[string]string) map[string]string {
	e["BROKER_AIRLOCK_RULES_FILE"] = "/run/airlock/airlock.json"
	e["BROKER_AIRLOCK_PROXY_URL"] = "https://secrets.internal.example:13381"
	e["BROKER_AIRLOCK_CA_CERT_FILE"] = "/run/airlock/ca.crt"
	e["BROKER_AIRLOCK_CA_KEY_FILE"] = "/run/airlock/ca.key"
	return e
}

// resultsEntry is the rules entry a result entry's grants are held to: DISPATCH_RESULTS_TOKEN,
// kind dpop, bound to Dispatch's host, carried in Authorization alone, with an allow list.
func resultsEntry() proxyrules.SecretRule {
	return proxyrules.SecretRule{
		Name: "DISPATCH_RESULTS_TOKEN", Hosts: []string{"dispatch.example"}, Headers: []string{"Authorization"},
		Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/projects/"}}, Kind: proxyrules.KindDPoP,
	}
}

// TestAResultEntryNeedsTheDPoPEntryAtStart: BROKER_EXCHANGE_RESULT_WORKFLOWS with no rules file
// refuses the start naming BROKER_AIRLOCK_RULES_FILE, and ResultEntryRules refuses rules with no
// DISPATCH_RESULTS_TOKEN entry, one that binds no host, one not of kind dpop, one with no allow
// list or an empty one, and one carried in any header but Authorization alone, each refusal
// naming what is missing. The entry whole, and the one with the default header (Authorization),
// are taken.
func TestAResultEntryNeedsTheDPoPEntryAtStart(t *testing.T) {
	e := withResultsAirlock(validEnv())
	e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
	e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
	e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = "example-org/example-repo=.github/workflows/ci-results.yaml=refs/heads/*=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err != nil {
		t.Fatalf("result workflows with the airlock: %v", err)
	}
	delete(e, "BROKER_AIRLOCK_RULES_FILE")
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") || !strings.Contains(err.Error(), "BROKER_AIRLOCK_RULES_FILE") {
		t.Errorf("result workflows with no rules file: err = %v, want a refusal naming BROKER_EXCHANGE_RESULT_WORKFLOWS and BROKER_AIRLOCK_RULES_FILE", err)
	}
	if err := ResultEntryRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{resultsEntry()}}); err != nil {
		t.Fatalf("the whole entry: %v", err)
	}
	defaultHeader := resultsEntry()
	defaultHeader.Headers = nil
	if err := ResultEntryRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{defaultHeader}}); err != nil {
		t.Fatalf("the entry with the default header: %v", err)
	}
	for _, c := range []struct {
		name, says string
		spoil      func(*proxyrules.SecretRule)
	}{
		{"no such entry", "holds no entry", func(s *proxyrules.SecretRule) { s.Name = "OTHER_TOKEN" }},
		{"no host", "binds no host", func(s *proxyrules.SecretRule) { s.Hosts = nil }},
		{"no kind", "kind dpop", func(s *proxyrules.SecretRule) { s.Kind = "" }},
		{"another kind", "kind dpop", func(s *proxyrules.SecretRule) { s.Kind = proxyrules.KindAWS }},
		{"no allow list", "allow list", func(s *proxyrules.SecretRule) { s.Allow = nil }},
		{"an empty allow list", "allow list", func(s *proxyrules.SecretRule) { s.Allow = []proxyrules.Allow{} }},
		{"another header", "Authorization", func(s *proxyrules.SecretRule) { s.Headers = []string{"X-Token"} }},
		{"a second header", "Authorization", func(s *proxyrules.SecretRule) { s.Headers = []string{"Authorization", "X-Token"} }},
		{"no header", "Authorization", func(s *proxyrules.SecretRule) { s.Headers = []string{} }},
	} {
		entry := resultsEntry()
		c.spoil(&entry)
		err := ResultEntryRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{entry}})
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_RESULTS_TOKEN") || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want a refusal naming DISPATCH_RESULTS_TOKEN and %q", c.name, err, c.says)
		}
	}
}

// TestLoadReadsTheResultGrantTestSeconds pins BROKER_RESULT_GRANT_TEST_SECONDS: unset it is 0,
// the production grant lifetime; a whole number of seconds from 1 to 900 is read; anything else
// refuses the start naming the variable.
func TestLoadReadsTheResultGrantTestSeconds(t *testing.T) {
	if cfg, err := Load(env(validEnv())); err != nil || cfg.ResultGrantTestSeconds != 0 {
		t.Fatalf("Load with BROKER_RESULT_GRANT_TEST_SECONDS unset = (%d, %v), want 0", cfg.ResultGrantTestSeconds, err)
	}
	e := validEnv()
	e["BROKER_RESULT_GRANT_TEST_SECONDS"] = "2"
	if cfg, err := Load(env(e)); err != nil || cfg.ResultGrantTestSeconds != 2 {
		t.Fatalf("Load(BROKER_RESULT_GRANT_TEST_SECONDS=2) = (%d, %v), want 2", cfg.ResultGrantTestSeconds, err)
	}
	for _, bad := range []string{"0", "901", "two", "-1", "2.5"} {
		e := validEnv()
		e["BROKER_RESULT_GRANT_TEST_SECONDS"] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_RESULT_GRANT_TEST_SECONDS") {
			t.Errorf("BROKER_RESULT_GRANT_TEST_SECONDS = %q: err = %v, want a refusal naming the variable", bad, err)
		}
	}
}
