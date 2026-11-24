package config

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// withResultsAirlock is e with the airlock's four settings: the profiles file a result entry's
// grants need and the three the proxy cannot run without. Load reads none of the files.
func withResultsAirlock(e map[string]string) map[string]string {
	e["BROKER_PROFILES_FILE"] = "/run/broker/profiles.json"
	e["BROKER_AIRLOCK_PROXY_URL"] = "https://secrets.internal.example:13381"
	e["BROKER_AIRLOCK_CA_CERT_FILE"] = "/run/airlock/ca.crt"
	e["BROKER_AIRLOCK_CA_KEY_FILE"] = "/run/airlock/ca.key"
	return e
}

// resultsEntry is the proxy block of the profile a result entry's grants are held to,
// DISPATCH_RESULTS_TOKEN: kind dpop, bound to Dispatch's host, carried in Authorization alone,
// with an allow list.
func resultsEntry() proxyrules.SecretRule {
	return proxyrules.SecretRule{
		Name: "DISPATCH_RESULTS_TOKEN", Hosts: []string{"dispatch.example"}, Headers: []string{"Authorization"},
		Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/projects/"}}, Kind: proxyrules.KindDPoP,
	}
}

// resultsDestination is the private destination resultsEntry's host goes to: Dispatch's address
// inside the cluster.
func resultsDestination() PrivateUpstream {
	return PrivateUpstream{Host: "dispatch.example", URL: &url.URL{Scheme: "http", Host: "dispatch.dispatch.svc.cluster.local:8766"}}
}

// TestAResultEntryNeedsTheDPoPEntryAtStart: BROKER_EXCHANGE_RESULT_WORKFLOWS with no profiles file
// refuses the start naming BROKER_PROFILES_FILE, and AirlockResultRules, for a broker with
// result entries, refuses profiles with no DISPATCH_RESULTS_TOKEN profile, one whose proxy block
// binds no host, one that binds a `*.` name or more than one host (the run's token and a proof of
// the pod's key go to every host the profile binds, so it binds Dispatch's one host and nothing
// else), one not of kind dpop, one with no allow list or an empty one, and one carried in any
// header but Authorization alone, each refusal naming BROKER_PROFILES_FILE and what is missing.
// The profile whole, and the one with the default header (Authorization), are taken with the
// private destination of its host.
func TestAResultEntryNeedsTheDPoPEntryAtStart(t *testing.T) {
	e := withResultsAirlock(validEnv())
	e["BROKER_EXCHANGE_GITHUB_ISSUER"] = "https://token.actions.githubusercontent.com"
	e["BROKER_EXCHANGE_GITHUB_AUDIENCE"] = "dispatch-exchange"
	e["BROKER_EXCHANGE_RESULT_WORKFLOWS"] = "example-org/example-repo=.github/workflows/ci-results.yaml=refs/heads/*=EXAMPLE=ada@example.com"
	if _, err := Load(env(e)); err != nil {
		t.Fatalf("result workflows with the airlock: %v", err)
	}
	delete(e, "BROKER_PROFILES_FILE")
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") || !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") {
		t.Errorf("result workflows with no profiles file: err = %v, want a refusal naming BROKER_EXCHANGE_RESULT_WORKFLOWS and BROKER_PROFILES_FILE", err)
	}
	if err := AirlockResultRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{resultsEntry()}}, resultsDestination(), true); err != nil {
		t.Fatalf("the whole entry: %v", err)
	}
	defaultHeader := resultsEntry()
	defaultHeader.Headers = nil
	if err := AirlockResultRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{defaultHeader}}, resultsDestination(), true); err != nil {
		t.Fatalf("the entry with the default header: %v", err)
	}
	for _, c := range []struct {
		name, says string
		spoil      func(*proxyrules.SecretRule)
	}{
		{"no such profile", "holds no profile", func(s *proxyrules.SecretRule) { s.Name = "OTHER_TOKEN"; s.Kind = "" }},
		{"no host", "one exact host", func(s *proxyrules.SecretRule) { s.Hosts = nil }},
		{"a wildcard host", "one exact host", func(s *proxyrules.SecretRule) { s.Hosts = []string{"*.example.com"} }},
		{"a second host", "one exact host", func(s *proxyrules.SecretRule) { s.Hosts = []string{"dispatch.example", "api.github.com"} }},
		{"a second, wildcard host", "one exact host", func(s *proxyrules.SecretRule) {
			s.Hosts = []string{"dispatch.example", "*.attacker.example"}
		}},
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
		err := AirlockResultRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{entry}}, resultsDestination(), true)
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_RESULTS_TOKEN") || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want a refusal naming DISPATCH_RESULTS_TOKEN and %q", c.name, err, c.says)
			continue
		}
		if !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") || strings.Contains(err.Error(), "BROKER_AIRLOCK_RULES_FILE") {
			t.Errorf("%s: err = %v, want a refusal naming BROKER_PROFILES_FILE, whose proxy blocks are the rules, and not the removed BROKER_AIRLOCK_RULES_FILE", c.name, err)
		}
	}
}

// TestTheResultsEntryIsCheckedOnABrokerWithNoResultEntries: AirlockResultRules holds the profile
// DISPATCH_RESULTS_TOKEN on a broker with no result entries of its own as on one with them, since
// any broker with an airlock port serves every result grant of the database it shares. Profiles
// whose results profile binds a `*.` name, two hosts, its one host with no private destination or
// with a destination of another host, profiles with a second proxy block of kind dpop, and profiles
// where another proxy block, `*.` or exact, binds the results host (the refusal names that host),
// are each refused, naming BROKER_PROFILES_FILE and the setting or the profile at fault, and never
// BROKER_EXCHANGE_RESULT_WORKFLOWS, which is unset. Control: the profile with the destination of
// its host passes, and so do profiles with no dpop block and no destination.
func TestTheResultsEntryIsCheckedOnABrokerWithNoResultEntries(t *testing.T) {
	secretOnly := proxyrules.Rules{Secrets: []proxyrules.SecretRule{{Name: "GH_TOKEN", Hosts: []string{"api.github.com"}}}}
	if err := AirlockResultRules(secretOnly, PrivateUpstream{}, false); err != nil {
		t.Fatalf("control: profiles with no dpop block and no destination: %v", err)
	}
	if err := AirlockResultRules(proxyrules.Rules{Secrets: []proxyrules.SecretRule{resultsEntry(), secretOnly.Secrets[0]}}, resultsDestination(), false); err != nil {
		t.Fatalf("control: the profile with the destination of its host: %v", err)
	}
	elsewhere := resultsDestination()
	elsewhere.Host = "elsewhere.example"
	for _, c := range []struct {
		name, says  string
		destination PrivateUpstream
		spoil       func(*proxyrules.Rules)
	}{
		{"a wildcard profile", "one exact host", resultsDestination(), func(r *proxyrules.Rules) { r.Secrets[0].Hosts = []string{"*.example"} }},
		{"a two-host profile", "one exact host", resultsDestination(), func(r *proxyrules.Rules) {
			r.Secrets[0].Hosts = []string{"dispatch.example", "evil.example"}
		}},
		{"no private destination", "BROKER_AIRLOCK_PRIVATE_UPSTREAM is unset", PrivateUpstream{}, func(*proxyrules.Rules) {}},
		{"a destination of another host", "names the host elsewhere.example", elsewhere, func(*proxyrules.Rules) {}},
		{"a second dpop profile", "OTHER_TOKEN of kind dpop", resultsDestination(), func(r *proxyrules.Rules) {
			other := resultsEntry()
			other.Name, other.Hosts = "OTHER_TOKEN", []string{"elsewhere.example"}
			r.Secrets = append(r.Secrets, other)
		}},
		{"a lone dpop profile over the destination", "LONE_TOKEN of kind dpop", resultsDestination(), func(r *proxyrules.Rules) {
			r.Secrets[0].Name, r.Secrets[0].Hosts = "LONE_TOKEN", []string{"*.example"}
		}},
		{"a wildcard secret profile over the destination", "the profile WILDCARD_TOKEN of BROKER_PROFILES_FILE binds the host dispatch.example", resultsDestination(), func(r *proxyrules.Rules) {
			r.Secrets = append(r.Secrets, proxyrules.SecretRule{Name: "WILDCARD_TOKEN", Hosts: []string{"*.example"}})
		}},
		{"an exact secret profile over the destination", "the profile SAME_TOKEN of BROKER_PROFILES_FILE binds the host dispatch.example", resultsDestination(), func(r *proxyrules.Rules) {
			r.Secrets = append(r.Secrets, proxyrules.SecretRule{Name: "SAME_TOKEN", Hosts: []string{"dispatch.example"}})
		}},
	} {
		rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{resultsEntry()}}
		c.spoil(&rules)
		err := AirlockResultRules(rules, c.destination, false)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want a refusal saying %q", c.name, err, c.says)
			continue
		}
		if strings.Contains(err.Error(), "BROKER_EXCHANGE_RESULT_WORKFLOWS") {
			t.Errorf("%s: the refusal names BROKER_EXCHANGE_RESULT_WORKFLOWS, which is unset: %v", c.name, err)
		}
		if !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") || strings.Contains(err.Error(), "BROKER_AIRLOCK_RULES_FILE") {
			t.Errorf("%s: err = %v, want a refusal naming BROKER_PROFILES_FILE, whose proxy blocks are the rules, and not the removed BROKER_AIRLOCK_RULES_FILE", c.name, err)
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
