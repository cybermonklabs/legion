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
// named as written where it has a key's shape (keyShape), and by its line alone where it has none,
// in every refusal of its entry (shownRoleName): the name of a sub-owner's agent holds a "-".
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
		fmt.Sprintf("roles.<the name at line %d> starts with sub-owner-, as the agent of a task's sub-owner is named: a step of the role would take that agent's claim", nameLine)})
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

// A name of the role grammar with no key's shape (keyShape), written as the name of an entry of a
// mapping keyed by roles, is named by its line alone in every refusal of that entry, its value's
// and a name given twice among them, and a hook's role and a step of default_steps that no role
// holds by its place: a made-up lower-case hex token of 32 characters fits the grammar, as an API
// key pasted where a role goes can, and each reader takes it as a name `roles` may define. The
// name's own refusal, where no role holds it, is TestARoleKeyOfNoKeyShapeNoRoleHoldsIsNamedByItsLine's.
// Each case holds the refusal's whole text and that it holds no part of the token. The fault is a
// refusal that writes the name as the file wrote it, as each of these did while only the name's
// own refusal named its line; the control is shownRoleName answering the name as written, under
// which every case of the table but the hook's and default_steps' quotes the token, and those two
// fail with the check of a hook's roles or of default_steps writing the name.
func TestNoRefusalOfARoleKeyedEntryEchoesANameOfNoKeyShape(t *testing.T) {
	const head = "deadbeefcafe0123"
	const token = head + "456789abcdef0123"
	if !roleWord.MatchString(token) || len(token) > maxRoleWord || keyShape(token) {
		t.Fatalf("%q is no name of the role grammar without a key's shape: the cases test nothing", token)
	}
	subOwner := string(plan.SubOwnerAgent(head))
	if !plan.IsSubOwnerAgent(claim.Agent(subOwner)) || !roleWord.MatchString(subOwner) || len(subOwner) > maxRoleWord || !strings.Contains(subOwner, head) {
		t.Fatalf("%q is no name of a sub-owner's agent of the role grammar that holds %s: its case tests nothing", subOwner, head)
	}
	at := fmt.Sprintf("<the name at line %d>", strings.Count(minimalFile, "\n")+1)
	atMost := maxTimerSeconds / 3600
	steps := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n  DEMO: {default_steps: [implementer, TOKEN]}\n", 1)
	if steps == minimalFile {
		t.Fatal("minimalFile holds no `projects:\\n  DEMO: {}`: the default_steps case would name no step")
	}
	const legionAndChecker = "architect, planner, implementer, tester, reviewer, merger, checker"
	for _, tc := range []struct {
		name string
		file string // the whole file, TOKEN and SUBOWNER standing for the two names
		want string
	}{
		{"a role's block that is not a mapping", minimalFile + "roles: {TOKEN: testing}\n",
			"roles." + at + " must be a mapping"},
		{"a role named twice", minimalFile + "roles: {TOKEN: {type: testing, prompt_file: checker.md}, TOKEN: {type: review, prompt_file: checker.md}}\n",
			"roles names " + at + " twice"},
		{"a key a role's block does not take", minimalFile + "roles: {TOKEN: {type: testing, prompt_file: checker.md, phase: step}}\n",
			"unknown key roles." + at + ".phase"},
		{"a role with no type", minimalFile + "roles: {TOKEN: {prompt_file: checker.md}}\n",
			"roles." + at + ".type is required: a role of the deployment has the type of its steps"},
		{"a role's type that is none of the five", minimalFile + "roles: {TOKEN: {type: security, prompt_file: checker.md}}\n",
			"roles." + at + ".type is security, which is none of design, planning, implementation, testing, review and role_types"},
		{"a role with no prompt_file", minimalFile + "roles: {TOKEN: {type: testing, model: checker-model}}\n",
			"roles." + at + ".prompt_file is required: a role of the deployment starts with its own prompt"},
		{"a role's empty model", minimalFile + "roles: {TOKEN: {type: testing, prompt_file: checker.md, model: \"\"}}\n",
			"roles." + at + ".model must not be empty"},
		{"a role's github_app of neither App", minimalFile + "roles: {TOKEN: {type: testing, prompt_file: checker.md, github_app: merge}}\n",
			"roles." + at + ".github_app is merge, which is neither implement nor review"},
		{"a role named as a sub-owner's agent", minimalFile + "roles: {SUBOWNER: {type: testing, prompt_file: checker.md}}\n",
			"roles." + at + " starts with sub-owner-, as the agent of a task's sub-owner is named: a step of the role would take that agent's claim"},
		{"a backend named twice", minimalFile + "worker_backends: {TOKEN: omp, TOKEN: claude}\n",
			"worker_backends names " + at + " twice"},
		{"a backend that is no word", minimalFile + "worker_backends: {TOKEN: [omp]}\n",
			"worker_backends." + at + " must be one of claude, omp"},
		{"a backend there is no entry for", minimalFile + "worker_backends: {TOKEN: codex}\n",
			"worker_backends." + at + ` must be one of claude, omp (got "codex")`},
		{"overlays named twice", minimalFile + "role_config_files: {TOKEN: [/etc/x.yml], TOKEN: [/etc/y.yml]}\n",
			"role_config_files names " + at + " twice"},
		{"overlays that are no list", minimalFile + "role_config_files: {TOKEN: /etc/x.yml}\n",
			"role_config_files." + at + " must be an array of non-empty strings"},
		{"no overlay", minimalFile + "role_config_files: {TOKEN: []}\n",
			"role_config_files." + at + " names no overlay: give the role at least one path, or leave it out"},
		{"a relative overlay", minimalFile + "role_config_files: {TOKEN: [etc/x.yml]}\n",
			"role_config_files." + at + ` entry "etc/x.yml" must be a clean absolute path: the role's Oh My Pi reads it from wherever it starts`},
		{"an overlay holding the separator", minimalFile + "role_config_files: {TOKEN: [\"/etc/a:b.yml\"]}\n",
			"role_config_files." + at + ` entry "/etc/a:b.yml" holds ` + "`:`, which separates the overlays PI_CONFIG_FILES names"},
		{"an overlay named twice", minimalFile + "role_config_files: {TOKEN: [/etc/x.yml, /etc/x.yml]}\n",
			"role_config_files." + at + " names /etc/x.yml twice"},
		{"a backstop named twice", minimalFile + "worker_stall_backstop_hours: {TOKEN: 2, TOKEN: 3}\n",
			"worker_stall_backstop_hours names " + at + " twice"},
		{"a backstop that is no number", minimalFile + "worker_stall_backstop_hours: {TOKEN: soon}\n",
			"worker_stall_backstop_hours." + at + " must be a number of hours, or 0 to switch the backstop off for that role"},
		{"a negative backstop", minimalFile + "worker_stall_backstop_hours: {TOKEN: -1}\n",
			"worker_stall_backstop_hours." + at + " must be 0 (the backstop off for that role) or a positive number of hours"},
		{"a backstop past the timer's bound", minimalFile + fmt.Sprintf("worker_stall_backstop_hours: {TOKEN: %d}\n", atMost+1),
			fmt.Sprintf("worker_stall_backstop_hours.%s must be at most %d", at, atMost)},
		{"a backstop that rounds to no time", minimalFile + "worker_stall_backstop_hours: {TOKEN: 1e-14}\n",
			"worker_stall_backstop_hours." + at + " must be 0 (the backstop off for that role) or a positive number of hours"},
		{"a hook's role no role holds", minimalFile + checkerRole + "hooks: [{name: nudge, on: work_back, every: 1, roles: [implementer, TOKEN], prompt: Look again.}]\n",
			"hooks[0].roles[1] has no key's shape (" + keyShapeWords + "): it must be a Legion role (" + legionAndChecker + ")"},
		{"a step of default_steps no role holds", steps,
			"projects.DEMO.default_steps: step 2 names a role with no key's shape (" + keyShapeWords + "), which neither the deployment nor the plan defines."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := strings.NewReplacer("TOKEN", token, "SUBOWNER", subOwner).Replace(tc.file)
			_, err := Load(configFileWith(t, file, "checker.md"), noEnv)
			if err == nil {
				t.Fatalf("Load = nil, want %q", tc.want)
			}
			if strings.Contains(err.Error(), head) {
				t.Errorf("the refusal %q writes the name", err)
			}
			if err.Error() != tc.want {
				t.Errorf("Load = %q, want %q", err, tc.want)
			}
		})
	}

	// A role whose prompt the daemon cannot read is named by its prompt file alone: the roles the
	// loader settles keep no line.
	t.Run("a role's prompt_file the daemon cannot read", func(t *testing.T) {
		path := rolesFile(t, "roles: {"+token+": {type: testing, prompt_file: missing.md}}\n")
		missing := filepath.Join(filepath.Dir(path), "missing.md")
		_, err := Load(path, noEnv)
		want := "roles: the prompt_file " + missing + " of a role whose name has no key's shape (" + keyShapeWords + ") cannot be read: "
		if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "no such file or directory") {
			t.Fatalf("Load = %v, want %q and the read's error", err, want)
		}
		if strings.Contains(err.Error(), head) {
			t.Errorf("the refusal %q writes the name", err)
		}
	})
}
