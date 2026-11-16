package config

import (
	"maps"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// model_context_bytes gives each role it names its own bound, the controller among them, under
// either controller launch; a role it leaves out is not in the map, which is no bound, and a file
// that names none leaves the map nil.
func TestModelContextBytesGivesEachRoleItNamesItsBound(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"model_context_bytes:\n"+
		"  implementer: 800000\n"+
		"  controller: 400000\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[claim.Role]int{claim.RoleImplementer: 800000, claim.RoleController: 400000}
	if !maps.Equal(cfg.ModelContextBytes, want) {
		t.Errorf("ModelContextBytes = %v, want %v", cfg.ModelContextBytes, want)
	}
	if bound, named := cfg.ModelContextBytes[claim.RoleTester]; named {
		t.Errorf("the tester's bound = %d, want none: the file does not name the role", bound)
	}

	cfg, err = Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load without the key: %v", err)
	}
	if cfg.ModelContextBytes != nil {
		t.Errorf("ModelContextBytes without the key = %v, want nil", cfg.ModelContextBytes)
	}
}

// Every shape that is no bound on a Legion role's model is refused at load, naming the key or the
// entry: a value that is no mapping, a name that is not a Legion role, a role named twice, a value
// that is not a whole number — a decimal, a word, a list, nothing at all — and a whole number below
// one, which would refuse every launch of the role rather than bound it.
func TestModelContextBytesRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not a mapping", "model_context_bytes: [800000]\n",
			"model_context_bytes must be a mapping of a role to the bytes of system prompt its model's context holds, e.g. {implementer: 800000}"},
		{"a scalar", "model_context_bytes: 800000\n",
			"model_context_bytes must be a mapping of a role to the bytes of system prompt its model's context holds, e.g. {implementer: 800000}"},
		{"an unknown role", "model_context_bytes: {coder: 800000}\n",
			`model_context_bytes names "coder", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a role named twice", "model_context_bytes: {tester: 800000, tester: 400000}\n",
			"model_context_bytes names tester twice"},
		{"a decimal", "model_context_bytes: {tester: 800000.5}\n",
			"model_context_bytes.tester must be a whole number of bytes"},
		{"a word", "model_context_bytes: {tester: lots}\n",
			"model_context_bytes.tester must be a whole number of bytes"},
		{"a quoted number", "model_context_bytes: {tester: \"800000\"}\n",
			"model_context_bytes.tester must be a whole number of bytes"},
		{"a list", "model_context_bytes: {tester: [800000]}\n",
			"model_context_bytes.tester must be a whole number of bytes"},
		{"no value", "model_context_bytes: {tester: }\n",
			"model_context_bytes.tester must be a whole number of bytes"},
		{"zero", "model_context_bytes: {tester: 0}\n",
			"model_context_bytes.tester must be a whole number of bytes above zero (got 0): leave the role out to give it no bound"},
		{"a negative bound", "model_context_bytes: {tester: -1}\n",
			"model_context_bytes.tester must be a whole number of bytes above zero (got -1): leave the role out to give it no bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
