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
func resultsEntry() proxyrules.Binding {
	return proxyrules.Binding{
		Name: "DISPATCH_RESULTS_TOKEN", Way: proxyrules.WayDPoP,
		Inline: &proxyrules.Kind{
			Hosts: []string{"dispatch.example"}, Headers: []string{"Authorization"},
			Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/projects/"}},
		},
	}
}

// resultsDestination is the private destination resultsEntry's host goes to: Dispatch's address
// inside the cluster.
func resultsDestination() PrivateUpstream {
	return PrivateUpstream{Host: "dispatch.example", URL: &url.URL{Scheme: "http", Host: "dispatch.dispatch.svc.cluster.local:8766"}}
}

// resultsStart is the start check of the results capability as cmd/cipherd makes it: the results
// profile's own rules (ResultsEntryRules), then every dpop block's (ProxyMintedRules) against the
// private destinations given.
func resultsStart(rules proxyrules.Rules, upstreams []PrivateUpstream, resultWorkflows bool) error {
	if err := ResultsEntryRules(rules, resultWorkflows); err != nil {
		return err
	}
	return ProxyMintedRules(rules, upstreams)
}

// TestAResultEntryNeedsTheDPoPEntryAtStart: BROKER_EXCHANGE_RESULT_WORKFLOWS with no profiles file
// refuses the start naming BROKER_PROFILES_FILE, and the start check (resultsStart), for a broker
// with result entries, refuses profiles with no DISPATCH_RESULTS_TOKEN profile, one whose proxy
// block binds no host, one that binds a `*.` name or more than one host (the run's token and a
// proof of the pod's key go to every host the profile binds, so it binds Dispatch's one host and
// nothing else), one with no allow list or an empty one, and one carried in any header but
// Authorization alone, each refusal naming BROKER_PROFILES_FILE and what is missing. A profile not
// of kind dpop is no file the profiles parser reads (policy.ParseProfiles, which
// TestProfilesCheckRefusesWhatTheBootRefusesOfTheResultsProfile drives), so it reaches no rule here.
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
	if err := resultsStart(proxyrules.Rules{Names: []proxyrules.Binding{resultsEntry()}}, []PrivateUpstream{resultsDestination()}, true); err != nil {
		t.Fatalf("the whole entry: %v", err)
	}
	defaultHeader := resultsEntry()
	defaultHeader.Inline.Headers = nil
	if err := resultsStart(proxyrules.Rules{Names: []proxyrules.Binding{defaultHeader}}, []PrivateUpstream{resultsDestination()}, true); err != nil {
		t.Fatalf("the entry with the default header: %v", err)
	}
	for _, c := range []struct {
		name, says string
		spoil      func(*proxyrules.Binding)
	}{
		{"no such profile", "holds no profile", func(s *proxyrules.Binding) { s.Name = "OTHER_TOKEN"; s.Way = "" }},
		{"no host", "one exact host", func(s *proxyrules.Binding) { s.Inline.Hosts = nil }},
		{"a wildcard host", "one exact host", func(s *proxyrules.Binding) { s.Inline.Hosts = []string{"*.example.com"} }},
		{"a second host", "one exact host", func(s *proxyrules.Binding) { s.Inline.Hosts = []string{"dispatch.example", "api.github.com"} }},
		{"a second, wildcard host", "one exact host", func(s *proxyrules.Binding) {
			s.Inline.Hosts = []string{"dispatch.example", "*.attacker.example"}
		}},
		{"no allow list", "allow list", func(s *proxyrules.Binding) { s.Inline.Allow = nil }},
		{"an empty allow list", "allow list", func(s *proxyrules.Binding) { s.Inline.Allow = []proxyrules.Allow{} }},
		{"another header", "Authorization", func(s *proxyrules.Binding) { s.Inline.Headers = []string{"X-Token"} }},
		{"a second header", "Authorization", func(s *proxyrules.Binding) { s.Inline.Headers = []string{"Authorization", "X-Token"} }},
		{"no header", "Authorization", func(s *proxyrules.Binding) { s.Inline.Headers = []string{} }},
	} {
		entry := resultsEntry()
		c.spoil(&entry)
		err := resultsStart(proxyrules.Rules{Names: []proxyrules.Binding{entry}}, []PrivateUpstream{resultsDestination()}, true)
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_RESULTS_TOKEN") || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want a refusal naming DISPATCH_RESULTS_TOKEN and %q", c.name, err, c.says)
			continue
		}
		if !strings.Contains(err.Error(), "BROKER_PROFILES_FILE") || strings.Contains(err.Error(), "BROKER_AIRLOCK_RULES_FILE") {
			t.Errorf("%s: err = %v, want a refusal naming BROKER_PROFILES_FILE, whose proxy blocks are the rules, and not the removed BROKER_AIRLOCK_RULES_FILE", c.name, err)
		}
	}
}

// TestTheResultsEntryIsCheckedOnABrokerWithNoResultEntries: the start check (resultsStart) holds
// the profile DISPATCH_RESULTS_TOKEN on a broker with no result entries of its own as on one with
// them, since any broker with an airlock port serves every result grant of the database it shares.
// Profiles whose results profile binds a `*.` name, two hosts, its one host with no private
// destination or with a destination of another host only, profiles with a second proxy block of
// kind dpop whose host no destination names, a lone `*.` dpop block, and profiles whose file's
// kind, `*.` or exact, binds the results host (the refusal names that host), are each
// refused, naming BROKER_PROFILES_FILE and the setting or the profile at fault, and never
// BROKER_EXCHANGE_RESULT_WORKFLOWS, which is unset. Control: the profile with the destination of
// its host passes, and so do profiles with no dpop block and no destination.
func TestTheResultsEntryIsCheckedOnABrokerWithNoResultEntries(t *testing.T) {
	secretOnly := proxyrules.Rules{Kinds: []proxyrules.Kind{{Name: "github", Hosts: []string{"api.github.com"}, Ways: []string{proxyrules.WaySwap}}}}
	if err := resultsStart(secretOnly, nil, false); err != nil {
		t.Fatalf("control: profiles with no dpop block and no destination: %v", err)
	}
	if err := resultsStart(proxyrules.Rules{Kinds: secretOnly.Kinds, Names: []proxyrules.Binding{resultsEntry()}}, []PrivateUpstream{resultsDestination()}, false); err != nil {
		t.Fatalf("control: the profile with the destination of its host: %v", err)
	}
	elsewhere := resultsDestination()
	elsewhere.Host = "elsewhere.example"
	for _, c := range []struct {
		name, says   string
		destinations []PrivateUpstream
		spoil        func(*proxyrules.Rules)
	}{
		{"a wildcard profile", "one exact host", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) { r.Names[0].Inline.Hosts = []string{"*.example"} }},
		{"a two-host profile", "one exact host", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) {
			r.Names[0].Inline.Hosts = []string{"dispatch.example", "evil.example"}
		}},
		{"no private destination", "BROKER_PROXY_PRIVATE_UPSTREAM is unset", nil, func(*proxyrules.Rules) {}},
		{"a destination of another host", "which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names", []PrivateUpstream{elsewhere}, func(*proxyrules.Rules) {}},
		{"a second dpop profile of a host no destination names", "OTHER_TOKEN of BROKER_PROFILES_FILE is of kind dpop and binds elsewhere.example", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) {
			other := resultsEntry()
			other.Name, other.Inline.Hosts = "OTHER_TOKEN", []string{"elsewhere.example"}
			r.Names = append(r.Names, other)
		}},
		{"a lone dpop profile over the destination", "LONE_TOKEN of BROKER_PROFILES_FILE is of kind dpop", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) {
			r.Names[0].Name, r.Names[0].Inline.Hosts = "LONE_TOKEN", []string{"*.example"}
		}},
		{"a wildcard kind over the destination", "the kind wildcard of BROKER_PROFILES_FILE binds dispatch.example", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) {
			r.Kinds = append(r.Kinds, proxyrules.Kind{Name: "wildcard", Hosts: []string{"*.example"}, Ways: []string{proxyrules.WaySwap}})
		}},
		{"an exact kind over the destination", "the kind same of BROKER_PROFILES_FILE binds dispatch.example", []PrivateUpstream{resultsDestination()}, func(r *proxyrules.Rules) {
			r.Kinds = append(r.Kinds, proxyrules.Kind{Name: "same", Hosts: []string{"dispatch.example"}, Ways: []string{proxyrules.WaySwap}})
		}},
	} {
		rules := proxyrules.Rules{Names: []proxyrules.Binding{resultsEntry()}}
		c.spoil(&rules)
		err := resultsStart(rules, c.destinations, false)
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
