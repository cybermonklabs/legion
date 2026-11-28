package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// productionCheckFile is minimalFile with value as DEMO's `production_check`, and minimalFile
// itself for the empty value, which is the project that names the key at all.
func productionCheckFile(value string) string {
	if value == "" {
		return minimalFile
	}
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {production_check: "+value+"}\n", 1)
}

// projects.<KEY>.production_check is read by the key's presence: a project that names none gets
// the tester, `off` is the empty role, which opens no check, and `tester` is the tester. Every
// other value is refused at load naming the key, and the word where it has a key's shape, so an
// operator who writes a role that works no check reads it back rather than losing the project's
// checks to a word the loader took for off; a value of no key's shape, the empty string included,
// is named by its line alone, since it can be a token pasted where the word goes.
func TestProjectProductionCheckIsTheTesterUnlessTheFileTurnsItOff(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        claim.Role
		refusal     string
	}{
		{name: "no key at all", want: claim.RoleTester},
		{name: "off, which YAML reads as the word", value: "off", want: ""},
		{name: "the tester", value: "tester", want: claim.RoleTester},
		{name: "the empty string", value: `""`,
			refusal: "projects.DEMO.production_check at line 7 has no key's shape (" + keyShapeWords + "): it must be off or tester"},
		{name: "a role that works no production check", value: "reviewer",
			refusal: `projects.DEMO.production_check must be off or tester (got "reviewer")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, productionCheckFile(tc.value)), noEnv)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("Load error = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Projects["DEMO"].ProductionCheck; got != tc.want {
				t.Fatalf("ProductionCheck = %q, want %q", got, tc.want)
			}
		})
	}
}
