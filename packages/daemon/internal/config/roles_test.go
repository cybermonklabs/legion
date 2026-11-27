package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// rolesFile writes body after minimalFile as a legion.yaml and each named prompt file beside it,
// and answers the configuration's path: a role's prompt_file resolves against that directory.
func rolesFile(t *testing.T, body string, prompts ...string) string {
	t.Helper()
	return configFileWith(t, minimalFile+body, prompts...)
}

// configFileWith writes the whole of body as a legion.yaml and each named prompt file beside it,
// and answers the configuration's path.
func configFileWith(t *testing.T, body string, prompts ...string) string {
	t.Helper()
	path := writeConfigFile(t, body)
	for _, name := range prompts {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte("You check the change.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// `roles` defines a role of the deployment's own with its type, its prompt file (resolved against
// the file's directory), its model and its GitHub App, the review App where the file names none;
// the deployment holds it beside Legion's own roles. `owner`, the key of a task's owner, names no
// role of Legion's, and a type of `role_types` is a type a role may have. No role takes a name of
// Legion's own, the claim role `sub-owner` among them, or the name of a task's sub-owner's agent,
// whose claim token a step of that role would take. A name outside the grammar or too long is
// named as written where it has a key's shape (keyShape), and by its line alone where it has none.
func TestTheDeploymentsRolesAreReadAndRefused(t *testing.T) {
	path := rolesFile(t, "roles: {checker: {type: testing, prompt_file: checker.md, model: checker-model}}\n", "checker.md")
	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checker := Role{Name: "checker", Type: plan.TypeTesting, PromptFile: filepath.Join(filepath.Dir(path), "checker.md"), Model: "checker-model", GitHubApp: "review"}
	if want := []Role{checker}; !reflect.DeepEqual(cfg.Roles, want) {
		t.Errorf("Roles = %+v, want %+v", cfg.Roles, want)
	}
	if got, ok := cfg.RoleOf("checker"); !ok || got != checker {
		t.Errorf("RoleOf(checker) = %+v, %v; want %+v, true", got, ok, checker)
	}
	if got, ok := cfg.RoleOf("ghost"); ok {
		t.Errorf("RoleOf(ghost) = %+v, true; want no role", got)
	}
	deployment := cfg.Deployment()
	if got, want := deployment.Roles["checker"], (plan.RoleDef{Name: "checker", Type: "testing"}); got != want {
		t.Errorf("Deployment().Roles[checker] = %+v, want %+v", got, want)
	}
	for name, builtin := range plan.BuiltinRoles() {
		if got := deployment.Roles[name]; got != builtin {
			t.Errorf("Deployment().Roles[%s] = %+v, want Legion's own %+v", name, got, builtin)
		}
	}
	if got, want := len(deployment.Roles), len(plan.BuiltinRoles())+1; got != want {
		t.Errorf("Deployment() holds %d roles, want %d", got, want)
	}

	cfg, err = Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load without roles: %v", err)
	}
	if cfg.Roles != nil || cfg.RoleTypes != nil {
		t.Errorf("a file with no roles reads Roles %v and RoleTypes %v, want both nil", cfg.Roles, cfg.RoleTypes)
	}
	if got, want := cfg.Deployment(), (plan.Deployment{Roles: plan.BuiltinRoles()}); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment() of a file with no roles = %+v, want Legion's own roles %+v", got, want)
	}

	for _, tc := range []struct {
		name, body string
		prompts    []string
		want       []Role
		types      []plan.Type
	}{
		{name: "owner names no role of Legion's",
			body: "roles: {owner: {type: testing, prompt_file: owner.md}}\n", prompts: []string{"owner.md"},
			want: []Role{{Name: "owner", Type: plan.TypeTesting, PromptFile: "owner.md", GitHubApp: "review"}}},
		{name: "a type of role_types named after the role",
			body: "roles: {audit: {type: security, prompt_file: audit.md, github_app: implement}}\nrole_types: [security]\n", prompts: []string{"audit.md"},
			want:  []Role{{Name: "audit", Type: "security", PromptFile: "audit.md", GitHubApp: "implement"}},
			types: []plan.Type{"security"}},
		{name: "a name of 32 characters",
			body: "roles: {a-role-name-of-thirty-two-letter: {type: review, prompt_file: long.md}}\n", prompts: []string{"long.md"},
			want: []Role{{Name: "a-role-name-of-thirty-two-letter", Type: plan.TypeReview, PromptFile: "long.md", GitHubApp: "review"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := rolesFile(t, tc.body, tc.prompts...)
			cfg, err := Load(path, noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for i := range tc.want {
				tc.want[i].PromptFile = filepath.Join(filepath.Dir(path), tc.want[i].PromptFile)
			}
			if !reflect.DeepEqual(cfg.Roles, tc.want) {
				t.Errorf("Roles = %+v, want %+v", cfg.Roles, tc.want)
			}
			if !reflect.DeepEqual(cfg.RoleTypes, tc.types) {
				t.Errorf("RoleTypes = %v, want %v", cfg.RoleTypes, tc.types)
			}
		})
	}

	nameLine := strings.Count(minimalFile, "\n") + 1
	refusals := []struct{ name, body, want string }{
		{"not a mapping", "roles: [checker]\n",
			"roles must be a mapping of a role's name to its block, e.g. {checker: {type: testing, prompt_file: checker.md}}"},
		{"a block that is not a mapping", "roles: {checker: testing}\n",
			"roles.checker must be a mapping"},
		{"a name outside the grammar", "roles: {Checker: {type: testing, prompt_file: checker.md}}\n",
			fmt.Sprintf("unknown key at line %d under roles, whose name has no key's shape (%s): a role's name is a word of [a-z][a-z0-9-]*", nameLine, keyShapeWords)},
		{"a name that starts with a digit", "roles: {2nd-check: {type: testing, prompt_file: checker.md}}\n",
			fmt.Sprintf("unknown key at line %d under roles, whose name has no key's shape (%s): a role's name is a word of [a-z][a-z0-9-]*", nameLine, keyShapeWords)},
		{"a name of a key's shape outside the grammar", "roles: {code_checker: {type: testing, prompt_file: checker.md}}\n",
			`roles names "code_checker", which is outside [a-z][a-z0-9-]*`},
		{"a name of 33 characters", "roles: {a-role-name-of-thirty-three-chars: {type: testing, prompt_file: checker.md}}\n",
			fmt.Sprintf("unknown key at line %d under roles, whose name has no key's shape (%s): a role's name has at most 32 characters: Dispatch's step record takes no longer one", nameLine, keyShapeWords)},
		{"a name of a key's shape of 33 characters", "roles: {" + strings.Repeat("x", 33) + ": {type: testing, prompt_file: checker.md}}\n",
			"roles." + strings.Repeat("x", 33) + " is 33 characters, and a role's name has at most 32: Dispatch's step record takes no longer one"},
		{"a role named twice", "roles: {checker: {type: testing, prompt_file: checker.md}, checker: {type: review, prompt_file: checker.md}}\n",
			"roles names checker twice"},
		{"a key the block does not take", "roles: {checker: {type: testing, prompt_file: checker.md, phase: step}}\n",
			"unknown key roles.checker.phase"},
		{"no type", "roles: {checker: {prompt_file: checker.md}}\n",
			"roles.checker.type is required: a role of the deployment has the type of its steps"},
		{"a type that is none of the five", "roles: {checker: {type: security, prompt_file: checker.md}}\n",
			"roles.checker.type is security, which is none of design, planning, implementation, testing, review and role_types"},
		{"no prompt_file", "roles: {checker: {type: testing, model: checker-model}}\n",
			"roles.checker.prompt_file is required: a role of the deployment starts with its own prompt"},
		{"an empty model", "roles: {checker: {type: testing, prompt_file: checker.md, model: \"\"}}\n",
			"roles.checker.model must not be empty"},
		{"a github_app of neither App", "roles: {checker: {type: testing, prompt_file: checker.md, github_app: merge}}\n",
			"roles.checker.github_app is merge, which is neither implement nor review"},
		{"role_types that is not a list", "role_types: security\n",
			"role_types must be an array of non-empty strings"},
		{"a type of role_types outside the grammar", "role_types: [Security]\n",
			`role_types names "Security", which is outside [a-z][a-z0-9-]*`},
		{"a type of role_types that is one of the five", "role_types: [testing]\n",
			"role_types names testing, which is one of Legion's five types"},
		{"a type of role_types of 33 characters", "role_types: [a-type-name-of-thirty-three-chars]\n",
			"role_types names a-type-name-of-thirty-three-chars, which is 33 characters, and a type has at most 32: Dispatch's step record takes no longer one"},
		{"a type named twice", "role_types: [security, security]\n",
			"role_types names security twice"},
	}
	for _, name := range []string{"architect", "planner", "implementer", "tester", "reviewer", "merger", "controller", "acceptance", "sub-owner"} {
		refusals = append(refusals, struct{ name, body, want string }{"the Legion name " + name,
			"roles: {" + name + ": {type: testing, prompt_file: checker.md}}\n",
			"roles." + name + " names a role of Legion's own: a deployment defines roles of its own names"})
	}
	subOwnerOfF := string(plan.SubOwnerAgent("f"))
	if !plan.IsSubOwnerAgent(claim.Agent(subOwnerOfF)) {
		t.Fatalf("IsSubOwnerAgent(%q) = false: the case below names no sub-owner's agent", subOwnerOfF)
	}
	refusals = append(refusals, struct{ name, body, want string }{"the name of the agent of a task's sub-owner",
		"roles: {" + subOwnerOfF + ": {type: testing, prompt_file: checker.md}}\n",
		"roles." + subOwnerOfF + " starts with sub-owner-, as the agent of a task's sub-owner is named: a step of the role would take that agent's claim"})
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(rolesFile(t, tc.body, "checker.md"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}

	// The prompt is read where the daemon loads its file to run, and not where a check of the
	// file loads it (LoadForValidation), which keeps the path alone.
	t.Run("a prompt_file the daemon cannot read", func(t *testing.T) {
		path := rolesFile(t, "roles: {checker: {type: testing, prompt_file: missing.md}}\n")
		missing := filepath.Join(filepath.Dir(path), "missing.md")
		_, err := Load(path, noEnv)
		want := "roles.checker.prompt_file " + missing + " cannot be read: "
		if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "no such file or directory") {
			t.Fatalf("Load = %v, want %q and the read's error", err, want)
		}
		cfg, err := LoadForValidation(path, noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation: %v", err)
		}
		if got := cfg.Roles[0].PromptFile; got != missing {
			t.Errorf("LoadForValidation's prompt_file = %q, want %q", got, missing)
		}
	})
}
