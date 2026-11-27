package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// checkerRole is a role of the deployment, `checker`, with its prompt file beside the
// configuration (rolesFile writes it).
const checkerRole = "roles: {checker: {type: testing, prompt_file: checker.md, model: checker-model}}\n"

// worker_backends takes a role the deployment defines as a key, and Claude Code runs it; a file
// that defines no such role is refused with the text of any other name that is no issue role.
func TestWorkerBackendsTakesARoleOfTheDeployment(t *testing.T) {
	cfg, err := Load(rolesFile(t, checkerRole+"worker_backends: {checker: claude}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.BackendFor("checker"); got != runtime.BackendClaude {
		t.Errorf("BackendFor(checker) = %q, want claude", got)
	}

	_, err = Load(rolesFile(t, "worker_backends: {checker: claude}\n"), noEnv)
	want := `worker_backends names "checker", which is not an issue role (architect, planner, implementer, tester, reviewer, merger)`
	if err == nil || err.Error() != want {
		t.Fatalf("Load without roles = %v, want %q", err, want)
	}
}

// The check across keys runs once every key it judges is set, so a name no role holds is refused
// through the daemon's load and through the check's load alike, with a list that names the roles
// of the deployment beside Legion's; a name outside the role grammar is refused when the key is
// read, before any other key is, by its line alone, since it has no key's shape (unknownKeyAt).
func TestTheCheckAcrossKeysSeesEveryKeyItJudges(t *testing.T) {
	want := `worker_backends names "ghost", which is not an issue role (architect, planner, implementer, tester, reviewer, merger, checker)`
	path := rolesFile(t, checkerRole+"worker_backends: {ghost: omp}\n", "checker.md")
	for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
		if _, err := load(path, noEnv); err == nil || err.Error() != want {
			t.Errorf("%s = %v, want %q", name, err, want)
		}
	}

	_, err := Load(rolesFile(t, "worker_backends: {Checker: omp}\n"), noEnv)
	want = fmt.Sprintf("unknown key at line %d under worker_backends, whose name has no key's shape (%s): it must be an issue role (architect, planner, implementer, tester, reviewer, merger)",
		strings.Count(minimalFile, "\n")+1, keyShapeWords)
	if err == nil || err.Error() != want {
		t.Fatalf("Load of a name outside the grammar = %v, want %q", err, want)
	}
}

// role_config_files and worker_stall_backstop_hours take a role the deployment defines as a key,
// and each check of the key walks it as it walks Legion's roles: its overlays are refused while
// it runs on Claude Code, and its bound below its own stall ladder. A file that defines no such
// role is refused with the text of any other name that is no Legion role, and a name no role
// holds with a list that names the roles of the deployment beside Legion's.
func TestTheRoleKeyedSettingsTakeARoleOfTheDeployment(t *testing.T) {
	cfg, err := Load(rolesFile(t, checkerRole+"role_config_files: {checker: [/etc/x.yml]}\nworker_stall_backstop_hours: {checker: 2}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RoleConfigFiles["checker"], []string{"/etc/x.yml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RoleConfigFiles[checker] = %v, want %v", got, want)
	}
	if got := cfg.WorkerStallBackstops["checker"]; got != 2*time.Hour {
		t.Errorf("WorkerStallBackstops[checker] = %s, want 2h", got)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"overlays without roles", "role_config_files: {checker: [/etc/x.yml]}\n",
			`role_config_files names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a backstop without roles", "worker_stall_backstop_hours: {checker: 2}\n",
			`worker_stall_backstop_hours names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"overlays of a name no role holds", checkerRole + "role_config_files: {ghost: [/etc/x.yml]}\n",
			`role_config_files names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)`},
		{"a backstop of a name no role holds", checkerRole + "worker_stall_backstop_hours: {ghost: 2}\n",
			`worker_stall_backstop_hours names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)`},
		{"overlays of a name outside the grammar", checkerRole + "role_config_files: {Checker: [/etc/x.yml]}\n",
			fmt.Sprintf("unknown key at line %d under role_config_files, whose name has no key's shape (%s): it must be a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)",
				strings.Count(minimalFile+checkerRole, "\n")+1, keyShapeWords)},
		{"overlays of a role on Claude Code", checkerRole + "worker_backends: {checker: claude}\nrole_config_files: {checker: [/etc/x.yml]}\n",
			"role_config_files.checker names Oh My Pi overlays, and worker_backends.checker is claude, which reads none: drop the role from one of the two"},
		{"a backstop below the role's own ladder", checkerRole + "worker_stall_backstop_hours: {checker: 1}\n",
			"worker_stall_backstop_hours.checker (1h0m0s) is below the whole stall ladder the checker's own plugin runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(rolesFile(t, tc.body, "checker.md"), noEnv)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}
}

// A name of the role grammar with no key's shape (keyShape) that no role holds is refused by the
// check across keys by its line alone, as a key's reader refuses a name outside the grammar, with
// the list that names the roles of the deployment, through the daemon's load and through the
// check's load alike: a made-up lower-case hex token of 32 characters fits the grammar, as an API
// key pasted where a role goes can. A role of the deployment whose name has no key's shape,
// `code-checker`, still reaches each key's reader.
func TestARoleKeyOfNoKeyShapeNoRoleHoldsIsNamedByItsLine(t *testing.T) {
	const token = "deadbeefcafe0123456789abcdef0123"
	line := strings.Count(minimalFile+checkerRole, "\n") + 1
	for _, tc := range []struct{ key, value, what string }{
		{"worker_backends", "omp", "an issue role (architect, planner, implementer, tester, reviewer, merger, checker)"},
		{"role_config_files", "[/etc/x.yml]", "a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)"},
		{"worker_stall_backstop_hours", "2", "a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			path := rolesFile(t, checkerRole+tc.key+": {"+token+": "+tc.value+"}\n", "checker.md")
			want := fmt.Sprintf("unknown key at line %d under %s, whose name has no key's shape (%s): it must be %s", line, tc.key, keyShapeWords, tc.what)
			for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
				if _, err := load(path, noEnv); err == nil || err.Error() != want {
					t.Errorf("%s = %v, want %q", name, err, want)
				}
			}
		})
	}

	const dashRole = "roles: {code-checker: {type: testing, prompt_file: checker.md}}\n"
	cfg, err := Load(rolesFile(t, dashRole+"role_config_files: {code-checker: [/etc/x.yml]}\nworker_stall_backstop_hours: {code-checker: 2}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RoleConfigFiles["code-checker"], []string{"/etc/x.yml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RoleConfigFiles[code-checker] = %v, want %v", got, want)
	}
	if got := cfg.WorkerStallBackstops["code-checker"]; got != 2*time.Hour {
		t.Errorf("WorkerStallBackstops[code-checker] = %s, want 2h", got)
	}
	if cfg, err = Load(rolesFile(t, dashRole+"worker_backends: {code-checker: claude}\n", "checker.md"), noEnv); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.BackendFor("code-checker"); got != runtime.BackendClaude {
		t.Errorf("BackendFor(code-checker) = %q, want claude", got)
	}
}

// A project's `memory` overlays reach every role of a memory-group tree, so a role of the
// deployment on Claude Code, which reads no PI_CONFIG_FILES, refuses them as an issue role of
// Legion's on Claude Code does; on Oh My Pi it takes them.
func TestADeploymentRolesOverlaysAreJudgedAsAnIssueRolesAre(t *testing.T) {
	const block = "{mode: on, config_files: [/etc/x.yml]}"
	_, err := Load(configFileWith(t, memoryFile(block, checkerRole+"worker_backends: {checker: claude}\n"), "checker.md"), noEnv)
	want := "projects.DEMO.memory names config_files, and worker_backends.checker is claude, which reads no PI_CONFIG_FILES: put every issue role on omp, or take the overlays out of the block"
	if err == nil || err.Error() != want {
		t.Fatalf("Load with the role on Claude Code = %v, want %q", err, want)
	}
	if _, err := Load(configFileWith(t, memoryFile(block, checkerRole+"worker_backends: {checker: omp}\n"), "checker.md"), noEnv); err != nil {
		t.Fatalf("Load with the role on Oh My Pi: %v", err)
	}
}

// A hook's `roles` and a project's `default_steps` take a role the deployment defines; a file
// that defines no such role is refused with the text of any other name no role holds, which for
// `default_steps` is the plan's own refusal of a role nobody defines. The judgments of
// `default_steps` run once the whole file is read, in the order of the projects' keys, so a
// refusal of an earlier key comes first, and of two projects the one whose key sorts first is
// told.
func TestAHooksRolesAndDefaultStepsTakeARoleOfTheDeployment(t *testing.T) {
	const hook = "hooks: [{name: nudge, on: work_back, every: 1, roles: [checker], prompt: Look again.}]\n"
	steps := func(projects string) string {
		return strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n"+projects, 1)
	}

	cfg, err := Load(rolesFile(t, checkerRole+hook, "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load of a hook of the role: %v", err)
	}
	if got := cfg.Hooks[0].Roles; len(got) != 1 || got[0] != "checker" {
		t.Errorf("Hooks[0].Roles = %v, want [checker]", got)
	}
	cfg, err = Load(configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")+checkerRole, "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load of default_steps naming the role: %v", err)
	}
	if got := cfg.Projects["DEMO"].DefaultSteps; len(got) != 2 || got[1].Role != "checker" {
		t.Errorf("DefaultSteps = %v, want [implementer checker]", got)
	}

	for _, tc := range []struct{ name, path, want string }{
		{"a hook of the role without roles", rolesFile(t, hook),
			`hooks[0].roles names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger)`},
		{"a hook of a name no role holds", rolesFile(t, checkerRole+strings.Replace(hook, "[checker]", "[ghost]", 1), "checker.md"),
			`hooks[0].roles names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, checker)`},
		{"a hook of a name outside the grammar", rolesFile(t, checkerRole+strings.Replace(hook, "[checker]", "[Checker]", 1), "checker.md"),
			`hooks[0].roles names "Checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger)`},
		{"default_steps of the role without roles", configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")),
			"projects.DEMO.default_steps: step 2 of task default names the role checker, which neither the deployment nor the plan defines."},
		{"default_steps this release refuses for another cause", configFileWith(t, steps("  DEMO: {default_steps: [tester]}\n")),
			"projects.DEMO.default_steps: task default has no step implementer. This release of Legion does not carry it out yet. " +
				"step 1 of task default names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none. This release of Legion does not carry it out yet."},
		{"default_steps whose role of the deployment does implementation work after the implementer",
			configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")+strings.Replace(checkerRole, "type: testing", "type: implementation", 1), "checker.md"),
			"projects.DEMO.default_steps: task default names checker at step 2, a step of the type implementation after the step implementer at step 1, and Legion's retro is done by the implementer in this release. This release of Legion does not carry it out yet."},
		// A role runs at most one step of a task, a role of the deployment as Legion's own.
		{"default_steps that name a role of the deployment at two steps",
			configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker, reviewer, checker]}\n")+checkerRole, "checker.md"),
			"projects.DEMO.default_steps: task default names the role checker at steps 2 and 4. This release of Legion does not carry it out yet."},
		{"a refusal of an earlier key first", configFileWith(t, steps("  DEMO: {default_steps: [tester]}\n")+"admission_cap: 0\n"),
			"admission_cap must be a positive integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(tc.path, noEnv); err == nil || err.Error() != tc.want {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("the project whose key sorts first", func(t *testing.T) {
		path := configFileWith(t, steps("  DEMO: {}\n  BETA: {default_steps: [tester]}\n  ACME: {default_steps: [tester]}\n"))
		for range 20 {
			if _, err := Load(path, noEnv); err == nil || !strings.HasPrefix(err.Error(), "projects.ACME.default_steps: ") {
				t.Fatalf("Load = %v, want the refusal of projects.ACME.default_steps", err)
			}
		}
	})
}
