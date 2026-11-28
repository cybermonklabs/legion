package requests

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// movedOtherProject is a second prefix in another project than movedConfigured's, which a move
// between two projects reaches.
const movedOtherProject = "projects/111111111111/secrets/cipher-"

// TestSourceRuleOnSecretManagerSourcesIsMains holds sourceRule.counts, on Secret Manager resource
// names alone (projects/<number>/secrets/<id>: the sources a GCP deployment serves and its
// Postgres holds), to the answers main gave: every row passed against the requests package of main
// aa3eaba7c3b2, whose counts cut the configured prefix from the start of the served source. The
// rows are TestSourceCountsThroughTheSecondPrefix's own, which this table took over, and five more.
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
// The named reverts, each run against main's counts: replace the prefix in both directions, and
// the "configured against second" row counts; drop the guard against an empty remainder, and the
// two rows of the bare prefixes count; compare no type, and the rows of another type count.
func TestSourceRuleOnSecretManagerSourcesIsMains(t *testing.T) {
	moved := sourceRule{prefix: movedConfigured, second: movedSecond}
	unset := sourceRule{prefix: movedConfigured}
	acrossProjects := sourceRule{prefix: movedConfigured, second: movedOtherProject}
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
