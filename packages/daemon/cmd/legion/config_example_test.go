package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	configpkg "github.com/sjawhar/legion/daemon/internal/config"
)

// The legion.yaml example the repository ships (deploy/kubernetes/daemon/legion.yaml.example, the
// docs site's configuration reference) passes `legion start --check-config` once the files it
// names exist, as its header says: a loader change that would refuse it fails here rather than on
// an operator's first check. The example names no Postgres; the environment does, as its comment
// offers.
func TestShippedLegionYAMLExamplePassesCheckConfig(t *testing.T) {
	checkShippedExample(t, func(example string) string { return example })
}

// The example's commented role_config_files, uncommented with the operator-roles volume and mount
// it points to, passes the same check, and names the three roles' overlays.
func TestShippedLegionYAMLExamplesRoleConfigFilesPassCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		return uncommentExample(t, example,
			"# role_config_files:",
			"#   implementer: [/etc/legion-operator-roles/fallback.yml]",
			"#   tester: [/etc/legion-operator-roles/fallback.yml]",
			"#   merger: [/etc/legion-operator-roles/fallback.yml]",
			"        # - name: operator-roles",
			"        #   config_map: {name: legion-operator-roles}",
			"        # - {volume: operator-roles, mount_path: /etc/legion-operator-roles}",
		)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RoleConfigFiles) != 3 {
		t.Errorf("RoleConfigFiles = %v, want the implementer's, the tester's and the merger's", cfg.RoleConfigFiles)
	}
}

// The example's commented memory block, uncommented with the operator-memory volume and mount it
// points to, passes the same check, and names the project's mode and overlay. The block's
// `instructions` stays commented: boot reads that file from the daemon's own disk, and the example
// names a path only an operator has.
func TestShippedLegionYAMLExamplesMemoryPassesCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"  # WIDGETS:",
			"  #   memory:",
			"  #     mode: on",
			"  #     config_files: [/etc/legion-memory/memory.yml]",
			"        # - name: operator-memory",
			"        #   config_map: {name: legion-operator-memory}",
			"        # - {volume: operator-memory, mount_path: /etc/legion-memory}",
		)
		// The example's two forms of the project entry are alternatives: the empty one it ships
		// active, and the commented one that carries the optional keys. An operator turning memory
		// on moves to the second, so this comments the first out, after the uncommenting above, to
		// which a commented `WIDGETS: {}` would otherwise be the first `# WIDGETS:` line.
		return strings.Replace(example, "\n  WIDGETS: {}", "\n  # WIDGETS: {}", 1)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	memory := cfg.Projects["WIDGETS"].Memory
	if memory.Mode != configpkg.MemoryOn || !slices.Equal(memory.ConfigFiles, []string{"/etc/legion-memory/memory.yml"}) {
		t.Errorf("the WIDGETS memory block = %+v, want mode on with the operator-memory overlay alone", memory)
	}
	if memory.InstructionsPath != "" {
		t.Errorf("the WIDGETS memory instructions = %q, want none: the example leaves that key commented", memory.InstructionsPath)
	}
}

// The example's commented commit_issue_trailer_repos, uncommented, passes the same check and
// names the one repository whose commits a pane of the project stamps with its issue key. It
// needs no volume and no mount: the key names repositories, not files.
func TestShippedLegionYAMLExamplesCommitIssueTrailerReposPassesCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"  # WIDGETS:",
			"  #   commit_issue_trailer_repos: [acme/widgets]",
		)
		// The example's two forms of the project entry are alternatives, as the memory test says:
		// the active empty one is commented out so the uncommented one is the project's whole entry.
		return strings.Replace(example, "\n  WIDGETS: {}", "\n  # WIDGETS: {}", 1)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	stamped := cfg.Projects["WIDGETS"].CommitIssueTrailerRepos
	if len(stamped) != 1 || stamped[0].String() != "acme/widgets" {
		t.Errorf("the WIDGETS commit_issue_trailer_repos = %v, want acme/widgets alone", stamped)
	}
}

// The example's first commented hook is the hook a file that names no `hooks:` key runs, its
// words included, so an operator reads the shipped default in the file it edits. Uncommented, it
// loads as exactly that hook: a default changed in one place and not the other would have the
// example teach a hook no daemon runs. The example's second hook names a `prompt_file`, a path
// only an operator has, whose words the check reads, so it is covered no more than the memory
// block's `instructions` is.
func TestShippedLegionYAMLExamplesFirstHookIsTheShippedDefault(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		return uncommentExample(t, example,
			"# hooks:",
			"#   - name: work-back",
			"#     on: work_back",
			"#     moves: any",
			"#     every: 5",
			"#     roles: [implementer, tester, reviewer]",
			"#     prompt: >-",
			"#       We read no progress on this issue: its work has gone back the number of times recorded",
			"#       below. Report to the architect what is in your way, in your own words. The architect",
			"#       takes a step back, reads what each of you says, and decides the path. Carry on with the",
			"#       work while you wait, and say so if you read it as on track.",
		)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := configpkg.DefaultHooks()
	if !reflect.DeepEqual(cfg.Hooks, want) {
		t.Errorf("the hooks of the example's first hook uncommented = %+v, want the shipped default %+v", cfg.Hooks, want)
	}
}

// The example's commented worker_silence_minutes, uncommented with a value one minute under the
// floor, is refused by the same check, which names the key, the value and the floor. The shipped
// line is where an operator reads what the key takes, so a floor it stated wrongly would be an
// operator's file refused on their first check; here it is this test that fails instead. The line
// stays commented in the file itself: an active key would change the stdout
// TestShippedLegionYAMLExamplePassesCheckConfig holds exactly.
func TestShippedLegionYAMLExampleRefusesAWorkerSilenceUnderTheFloor(t *testing.T) {
	config := stageShippedExample(t, func(example string) string {
		example = uncommentExample(t, example, "# worker_silence_minutes: 30")
		return strings.Replace(example, "worker_silence_minutes: 30", "worker_silence_minutes: 15", 1)
	})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

	want := "legion start: worker_silence_minutes (15m0s) is below the 16m0s floor under it: a healthy phase worker that answers its reminder with a WAITING line is quiet for the 15m that reply buys before the reminder comes again (DEFAULT_STALL_LIMITS.waitingMs), so a shorter bound would send a second task for work the worker already holds, to a worker whose own reminder is about to start a turn\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// The example's commented worker_background_hours, uncommented with a value at the silence bound
// the file ships commented beside it, is refused by the check that holds the two to their
// relation, which names both keys and both values. The shipped line is where an operator reads
// what the key takes, so a relation it stated wrongly would be an operator's file refused on
// their first check; here it is this test that fails instead. The line stays commented in the
// file itself, as the silence key's does, for the same reason: an active key would change the
// stdout TestShippedLegionYAMLExamplePassesCheckConfig holds exactly.
func TestShippedLegionYAMLExampleRefusesAWorkerBackgroundAtTheSilenceBound(t *testing.T) {
	config := stageShippedExample(t, func(example string) string {
		example = uncommentExample(t, example, "# worker_background_hours: 4")
		return strings.Replace(example, "worker_background_hours: 4", "worker_background_hours: 0.5", 1)
	})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

	want := "legion start: worker_background_hours (30m0s) is at or below worker_silence_minutes (30m0s): the bound on background work a worker reports must be above the bound on its silence, since a shorter one would name a job that has barely begun and would race the repair that sends the same worker its phase's task again\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// memoryGuardExampleLines are the example's commented memory_guard block, line by line, as an
// operator turning it on uncomments it.
var memoryGuardExampleLines = []string{
	"    # memory_guard:",
	"    #   issue_pods:",
	"    #     enabled: true",
	"    #     act_percent: 85",
	"    #     floor_percent: 10",
	"    #     wait_seconds: 5",
	"    #     interval_milliseconds: 100",
}

// The example's commented memory_guard block, uncommented, passes the same check and settles on
// the guard on at the defaults the block states, which are the wire's own
// (config.DefaultMemoryGuard): a default changed in the wire and not in the example would have the
// example teach a number no daemon runs.
func TestMemoryGuardTheShippedExamplesBlockPassesCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		return uncommentExample(t, example, memoryGuardExampleLines...)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Runtime.Kubernetes.MemoryGuard, configpkg.DefaultMemoryGuard(); got != want {
		t.Errorf("the example's memory_guard block uncommented settles on %+v, want the defaults %+v", got, want)
	}
}

// The example's memory_guard block, uncommented with act_percent one above the range it states, is
// refused by the same check, which names the whole key and both ends of its range. The shipped
// block is where an operator reads what each key takes, so a range it stated wrongly would be an
// operator's file refused on their first check; here it is this test that fails instead. The block
// stays commented in the file itself: its values are the defaults, which apply without it.
func TestMemoryGuardTheShippedExampleRefusesAnActShareAboveItsRange(t *testing.T) {
	config := stageShippedExample(t, func(example string) string {
		example = uncommentExample(t, example, memoryGuardExampleLines...)
		return strings.Replace(example, "act_percent: 85", "act_percent: 96", 1)
	})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	t.Logf("legion start --check-config exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())

	want := "legion start: runtime.kubernetes.memory_guard.issue_pods.act_percent must be between 50 and 95\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// uncommentExample is the example with each named line uncommented, each line the start of one of
// the example's own commented lines: the file comments a block out line by line, so turning it on
// is one `# ` removed per line, and a line the example does not hold is the test's own mistake.
func uncommentExample(t *testing.T, example string, lines ...string) string {
	t.Helper()
	for _, line := range lines {
		at := strings.Index(example, "\n"+line)
		if at < 0 {
			t.Fatalf("the example has no line starting %q", line)
		}
		uncommented := strings.Replace(line, "# ", "", 1)
		example = example[:at+1] + uncommented + example[at+1+len(line):]
	}
	return example
}

// stageShippedExample writes the shipped example as edit leaves it beside the files it names,
// and answers the configuration's path, ready for `legion start --check-config` to be run over
// it: the check that passes (checkShippedExample) and the check a bad value makes fail
// (TestShippedLegionYAMLExampleRefusesAWorkerSilenceUnderTheFloor) stage the same files.
func stageShippedExample(t *testing.T, edit func(string) string) string {
	t.Helper()
	legionState(t)
	t.Setenv("LEGION_POSTGRES_DSN", "postgres://legion:legion@127.0.0.1:1/legion")
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate command test source")
	}
	example, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../deploy/kubernetes/daemon/legion.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "legion.yaml")
	for name, contents := range map[string]string{
		config:           edit(string(example)),
		"operator-token": "operator\n",
		"dispatch-token": "dispatch\n",
		"envoy-token":    "envoy\n",
		// The context the example names, and nothing the check would dial.
		"kubeconfig": `apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
`,
	} {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config
}

// checkShippedExample runs `legion start --check-config` on the shipped example as edit leaves
// it, beside the files it names, and answers the file's path. It logs the command's exit code,
// stdout and stderr, so a `-v` run shows what the check printed for each edit of the example.
func checkShippedExample(t *testing.T, edit func(string) string) string {
	t.Helper()
	config := stageShippedExample(t, edit)

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	t.Logf("legion start --check-config exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())

	want := "Config OK: project=WIDGETS design-gate-steps=spec\n" +
		"projects.WIDGETS.production_check: tester — a merge that closes one of its issues opens a production check the tester works\n" +
		defaultCacheLine +
		"plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"
	if code != 0 || out.String() != want {
		t.Fatalf("legion start --check-config on the shipped example = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}
	return config
}
