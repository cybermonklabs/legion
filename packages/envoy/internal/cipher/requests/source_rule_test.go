package requests

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// movedOtherProject is a second prefix in another project than movedConfigured's, which a move
// between two projects reaches.
const movedOtherProject = "projects/111111111111/secrets/cipher-"

// TestSourceRuleOnSecretManagerSources holds sourceRule.counts, built as the broker builds it on
// Google Cloud Secret Manager (gcpRule), on Secret Manager resource names alone
// (projects/<number>/secrets/<id>: the sources a GCP deployment serves and its Postgres holds).
//
// It counts as the policy's source when the two are equal, with the second prefix set or not; and,
// with the second prefix set, a stored source under the second prefix with the same remainder, a
// minted source of the same type whose store source is under the second prefix with the same
// remainder, either one across two projects, and one whose remainder opens with the second
// prefix's id. It refuses a source that differs in the remainder, a minted source of another type,
// a source under a prefix that is neither of the two, one in another project than the second prefix
// names, a stored frozen source against a minted policy source and the reverse, a source under the
// configured prefix where the policy serves one under the second, a source under the second prefix
// with the setting empty, a minted source that names no store source, a source that is the second
// prefix itself where the policy serves the configured prefix itself, which leaves no remainder
// after either, and a source under the configured prefix whose id holds the second prefix's id.
//
// The named reverts: replace the prefix in both directions, and the "configured against second"
// row counts; drop the guard against an empty remainder, and the two rows of the bare prefixes
// count; compare no type, and the rows of another type count.
func TestSourceRuleOnSecretManagerSources(t *testing.T) {
	moved := gcpRule(t, movedConfigured, movedSecond)
	unset := gcpRule(t, movedConfigured, "")
	acrossProjects := gcpRule(t, movedConfigured, movedOtherProject)
	app := func(store string) string { return policy.TypeSource(policy.TypeGitHubApp, store) }
	installations := func(store string) string { return policy.TypeSource(policy.TypeGitHubInstallations, store) }
	const idOfSecond = "agent-secrets-deel-api-key"
	for _, row := range []struct {
		name           string
		rule           sourceRule
		frozen, served string
		counts         bool
	}{
		{"an equal stored source", moved, movedConfigured + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"an equal stored source with the setting empty", unset, movedConfigured + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"an equal minted source", moved, app(movedConfigured + "app-key"), app(movedConfigured + "app-key"), true},
		{"a stored source under the second prefix with the same remainder", moved, movedSecond + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"a minted source of the same type under the second prefix", moved, app(movedSecond + "app-key"), app(movedConfigured + "app-key"), true},
		{"a stored source under a second prefix in another project", acrossProjects, movedOtherProject + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"a minted source under a second prefix in another project", acrossProjects, app(movedOtherProject + "app-key"), app(movedConfigured + "app-key"), true},
		{"a stored source whose remainder opens with the second prefix's id", moved, movedSecond + idOfSecond, movedConfigured + idOfSecond, true},

		{"a stored source under the second prefix with another remainder", moved, movedSecond + "other-key", movedConfigured + "deel-api-key", false},
		{"a minted source under the second prefix with another remainder", moved, app(movedSecond + "other-key"), app(movedConfigured + "app-key"), false},
		{"a minted source of another type under the second prefix", moved, installations(movedSecond + "app-key"), app(movedConfigured + "app-key"), false},
		{"a minted source of another type under the configured prefix", moved, installations(movedConfigured + "app-key"), app(movedConfigured + "app-key"), false},
		{"a stored source under a prefix that is neither", moved, movedNeither + "deel-api-key", movedConfigured + "deel-api-key", false},
		{"a minted source under a prefix that is neither", moved, app(movedNeither + "app-key"), app(movedConfigured + "app-key"), false},
		{"a stored source in another project than the second prefix names", moved, movedOtherProject + "deel-api-key", movedConfigured + "deel-api-key", false},
		{"a stored frozen source against a minted policy source", moved, movedSecond + "app-key", app(movedConfigured + "app-key"), false},
		{"a stored frozen source against a minted policy source, both under the configured prefix", moved, movedConfigured + "app-key", app(movedConfigured + "app-key"), false},
		{"a minted frozen source against a stored policy source", moved, app(movedSecond + "deel-api-key"), movedConfigured + "deel-api-key", false},
		{"a source under the configured prefix where the policy serves one under the second", moved, movedConfigured + "deel-api-key", movedSecond + "deel-api-key", false},
		{"a source under the second prefix where the policy serves one under neither", moved, movedSecond + "deel-api-key", movedNeither + "deel-api-key", false},
		{"a source under the second prefix with the setting empty", unset, movedSecond + "deel-api-key", movedConfigured + "deel-api-key", false},
		{"a minted source under the second prefix with the setting empty", unset, app(movedSecond + "app-key"), app(movedConfigured + "app-key"), false},
		{"a minted source that names no store source", moved, "mint:" + policy.TypeGitHubApp + ":", app(movedConfigured + "app-key"), false},
		{"a source that is the second prefix itself, where the policy serves the configured prefix itself", moved, movedSecond, movedConfigured, false},
		{"a minted source on the second prefix itself, where the policy mints from the configured prefix itself", moved, app(movedSecond), app(movedConfigured), false},
		{"a source under the configured prefix whose id holds the second prefix's id", moved, movedConfigured + idOfSecond, movedConfigured + "deel-api-key", false},
	} {
		if got := row.rule.counts(row.frozen, row.served); got != row.counts {
			t.Errorf("%s: counts(%q, %q) = %t, want %t", row.name, row.frozen, row.served, got, row.counts)
		}
	}
}

// TestSourceRuleOnMemorySources holds sourceRule.counts on the memory store's sources,
// memory:<name> (memory.Source), with the rule built as the broker builds it: from the memory
// store's own configuration (secretstore.Config.SourcePrefixes), whose prefixes stand behind the
// store's lead. For each pair of prefixes, an ordinary one and two that spell part of the lead
// itself (mem and memory:), a move from the second prefix counts, stored and minted, and an equal
// source counts with the setting empty; a source with no lead or another one, one under the second
// prefix with the setting empty, one under the configured prefix where the policy serves the
// second's, and the bare prefixes do not.
//
// The named reverts: have the memory store answer its prefixes without the lead, and no move
// counts; find the prefix where it first appears in the source, with the prefixes as given, and the
// moves between the prefixes that spell part of the lead do not count.
func TestSourceRuleOnMemorySources(t *testing.T) {
	app := func(store string) string { return policy.TypeSource(policy.TypeGitHubApp, store) }
	for _, pair := range []struct{ configured, second string }{
		{"example/agent-secrets/", "example/old-secrets/"},
		{"mem", "old"},
		{"memory:", "old:"},
	} {
		t.Run(pair.configured, func(t *testing.T) {
			moved := memoryRule(t, pair.configured, pair.second)
			unset := memoryRule(t, pair.configured, "")
			c, s := pair.configured, pair.second
			for _, row := range []struct {
				name           string
				rule           sourceRule
				frozen, served string
				counts         bool
			}{
				{"a stored source under the second prefix with the same remainder", moved, memory.Source(s + "deel-api-key"), memory.Source(c + "deel-api-key"), true},
				{"a minted source of the same type under the second prefix", moved, app(memory.Source(s + "app-key")), app(memory.Source(c + "app-key")), true},
				{"an equal stored source with the setting empty", unset, memory.Source(c + "deel-api-key"), memory.Source(c + "deel-api-key"), true},

				{"a source under the second prefix with no lead", moved, s + "deel-api-key", memory.Source(c + "deel-api-key"), false},
				{"a source under the second prefix behind another lead", moved, "other:" + s + "deel-api-key", memory.Source(c + "deel-api-key"), false},
				{"a source under the second prefix with the setting empty", unset, memory.Source(s + "deel-api-key"), memory.Source(c + "deel-api-key"), false},
				{"a source under the configured prefix where the policy serves one under the second", moved, memory.Source(c + "deel-api-key"), memory.Source(s + "deel-api-key"), false},
				{"a source that is the second prefix itself, where the policy serves the configured prefix itself", moved, memory.Source(s), memory.Source(c), false},
			} {
				if got := row.rule.counts(row.frozen, row.served); got != row.counts {
					t.Errorf("%s: counts(%q, %q) = %t, want %t", row.name, row.frozen, row.served, got, row.counts)
				}
			}
		})
	}
}

// memoryRule is the rule a broker on the memory store holds frozen sources to, for the prefix
// BROKER_SECRETS_PREFIX and the second prefix second ("" for none): the store's configuration
// parsed as the broker parses it, and its source prefixes, as cmd/cipherd hands them the machine.
func memoryRule(t *testing.T, prefix, second string) sourceRule {
	t.Helper()
	cfg, err := memory.Backend{}.ParseConfig(prefix, func(name string) string {
		switch name {
		case secretstore.SecondPrefixVariable:
			return second
		case "BROKER_MEMORY_SECRETS_FILE":
			return "memory-secrets.json"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("the memory store's ParseConfig(%q, second %q): %v", prefix, second, err)
	}
	configured, moved := cfg.SourcePrefixes()
	return sourceRule{prefix: configured, second: moved}
}

// gcpRule is the rule a broker on Google Cloud Secret Manager holds frozen sources to, for the
// prefix BROKER_SECRETS_PREFIX and the second prefix second ("" for none): the GCP store's
// configuration parsed as the broker parses it, and its source prefixes, as cmd/cipherd hands them
// the machine.
func gcpRule(t *testing.T, prefix, second string) sourceRule {
	t.Helper()
	cfg, err := gcp.Backend{}.ParseConfig(prefix, func(name string) string {
		switch name {
		case secretstore.SecondPrefixVariable:
			return second
		case "BROKER_SECRETS_KMS_KEY_NAME":
			return "projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("the GCP store's ParseConfig(%q, second %q): %v", prefix, second, err)
	}
	configured, moved := cfg.SourcePrefixes()
	return sourceRule{prefix: configured, second: moved}
}
