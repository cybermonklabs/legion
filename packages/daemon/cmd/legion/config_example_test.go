package main

//ci:reads deploy/kubernetes/daemon/legion.yaml.example stageShippedExample reads it from the repository root (runtime.Caller + ../../../..)

import (
	"bytes"
	"context"
	"fmt"
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
// docs site's configuration reference) passes `legion config check` once the files it
// names exist, as its header says: a loader change that would refuse it fails here rather than on
// an operator's first check. The example names no Postgres; the environment does, as its comment
// offers.
func TestShippedLegionYAMLExamplePassesCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
	checkShippedExample(t, func(example string) string { return example })
}

// The example's commented role_config_files, uncommented with the operator-roles volume and mount
// it points to, passes the same check, and names the three roles' overlays.
func TestShippedLegionYAMLExamplesRoleConfigFilesPassCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
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
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
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
	if memory.Mode != configpkg.ExperimentOn || !slices.Equal(memory.ConfigFiles, []string{"/etc/legion-memory/memory.yml"}) {
		t.Errorf("the WIDGETS memory block = %+v, want mode on with the operator-memory overlay alone", memory)
	}
	if memory.InstructionsPath != "" {
		t.Errorf("the WIDGETS memory instructions = %q, want none: the example leaves that key commented", memory.InstructionsPath)
	}
}

// The example's commented experiments block, uncommented with the operator-experiments volume and
// mount its one arm's overlay points to, passes the same check, and names the experiment the
// project declares: its mode, its unit, and the two arms it compares. The arm that names no file
// is the control, which is why only one of the two carries an overlay.
func TestShippedLegionYAMLExamplesExperimentsPassCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"  # WIDGETS:",
			"  #   experiments:",
			"  #     plan-length:",
			"  #       mode: random",
			"  #       unit: issue",
			"  #       variants:",
			"  #         - name: long",
			"  #           weight: 1",
			"  #           config_files: [/etc/legion-experiments/plan-length.yml]",
			"  #         - name: control",
			"        # - name: operator-experiments",
			"        #   config_map: {name: legion-operator-experiments}",
			"        # - {volume: operator-experiments, mount_path: /etc/legion-experiments}",
		)
		// The example's two forms of the project entry are alternatives, as the memory test says:
		// the active empty one is commented out so the uncommented one is the project's whole entry.
		return strings.Replace(example, "\n  WIDGETS: {}", "\n  # WIDGETS: {}", 1)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	declared := cfg.Projects["WIDGETS"].Experiments
	if len(declared) != 1 {
		t.Fatalf("the WIDGETS experiments = %+v, want the plan-length experiment alone", declared)
	}
	plan := declared[0]
	if plan.Name != "plan-length" || plan.Mode != configpkg.ExperimentRandom || plan.Unit != configpkg.ExperimentIssue {
		t.Errorf("the WIDGETS experiment = %+v, want plan-length at mode random over the issue", plan)
	}
	want := []configpkg.Variant{
		{Name: "long", Weight: 1, ConfigFiles: []string{"/etc/legion-experiments/plan-length.yml"}},
		{Name: "control", Weight: 1},
	}
	if !slices.EqualFunc(plan.Variants, want, func(got, want configpkg.Variant) bool {
		return got.Name == want.Name && got.Weight == want.Weight &&
			slices.Equal(got.ConfigFiles, want.ConfigFiles) && got.InstructionsPath == want.InstructionsPath
	}) {
		t.Errorf("the plan-length variants = %+v, want %+v: the control arm names no file", plan.Variants, want)
	}
	// The `memory` block stays commented beside it, so the project declares the one experiment:
	// a project writing the block and experiments.memory together is refused.
	if memory := cfg.Projects["WIDGETS"].Memory; memory.Mode != "" {
		t.Errorf("the WIDGETS memory block = %+v, want none: this test uncomments the experiments block alone", memory)
	}
}

// The example's commented experiment that sets how each issue is done, uncommented alone, passes
// the same check, and names its two arms with the group each sets: `owner`, whose issues take the
// plan their owner gives, and `default`, whose issues run the project's default steps. It needs no
// volume and no mount: neither arm names a file.
func TestShippedLegionYAMLExamplesPlanningExperimentPassesCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"  # WIDGETS:",
			"  #   experiments:",
			"  #     planning: {mode: random, unit: issue, variants: [{name: owner, plan: owner}, {name: default, plan: default}]}",
		)
		return strings.Replace(example, "\n  WIDGETS: {}", "\n  # WIDGETS: {}", 1)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	declared := cfg.Projects["WIDGETS"].Experiments
	if len(declared) != 1 || declared[0].Name != "planning" || declared[0].Mode != configpkg.ExperimentRandom || declared[0].Unit != configpkg.ExperimentIssue {
		t.Fatalf("the WIDGETS experiments = %+v, want the planning experiment alone, at mode random over the issue", declared)
	}
	want := []configpkg.Variant{
		{Name: "owner", Weight: 1, Plan: configpkg.PlanGroupOwner},
		{Name: "default", Weight: 1, Plan: configpkg.PlanGroupDefault},
	}
	if !slices.EqualFunc(declared[0].Variants, want, func(got, want configpkg.Variant) bool {
		return got.Name == want.Name && got.Weight == want.Weight && got.Plan == want.Plan && len(got.ConfigFiles) == 0
	}) {
		t.Errorf("the planning variants = %+v, want %+v", declared[0].Variants, want)
	}
}

// The example's commented commit_issue_trailer_repos, uncommented, passes the same check and
// names the one repository whose commits a pane of the project stamps with its issue key. It
// needs no volume and no mount: the key names repositories, not files.
func TestShippedLegionYAMLExamplesCommitIssueTrailerReposPassesCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
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

// The example's commented leak_check block, uncommented with a digest in place of its placeholder,
// passes the same check and settles on the repository and the manifest digest it names. It needs
// no volume and no mount: the block names a repository and a digest, not files.
func TestShippedLegionYAMLExamplesLeakCheckPassesCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
	checker := strings.Repeat("5e", 32)
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"# leak_check:",
			"#   repository: example-org/deployment",
			"#   manifest_sha256: <64 hex digits>",
		)
		return strings.Replace(example, "manifest_sha256: <64 hex digits>", "manifest_sha256: "+checker, 1)
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LeakCheck; got.Repository.String() != "example-org/deployment" || got.ManifestSHA256 != checker {
		t.Errorf("the leak_check block uncommented settles on %+v, want example-org/deployment and %s", got, checker)
	}
}

// The example's first commented hook is the hook a file that names no `hooks:` key runs, its
// words included, so an operator reads the shipped default in the file it edits. Uncommented, it
// loads as exactly that hook: a default changed in one place and not the other would have the
// example teach a hook no daemon runs. The example's second hook names a `prompt_file`, a path
// only an operator has, whose words the check reads, so it is covered no more than the memory
// block's `instructions` is.
func TestShippedLegionYAMLExamplesFirstHookIsTheShippedDefault(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
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
	// Not parallel: it sets environment variables of the whole process (stageShippedExample).
	config := stageShippedExample(t, func(example string) string {
		example = uncommentExample(t, example, "# worker_silence_minutes: 30")
		return strings.Replace(example, "worker_silence_minutes: 30", "worker_silence_minutes: 15", 1)
	})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)

	want := "legion config check: worker_silence_minutes (15m0s) is below the 16m0s floor under it: a healthy phase worker that answers its reminder with a WAITING line is quiet for the 15m that reply buys before the reminder comes again (DEFAULT_STALL_LIMITS.waitingMs), so a shorter bound would send a second task for work the worker already holds, to a worker whose own reminder is about to start a turn\n"
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
	// Not parallel: it sets environment variables of the whole process (stageShippedExample).
	config := stageShippedExample(t, func(example string) string {
		example = uncommentExample(t, example, "# worker_background_hours: 4")
		return strings.Replace(example, "worker_background_hours: 4", "worker_background_hours: 0.5", 1)
	})

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)

	want := "legion config check: worker_background_hours (30m0s) is at or below worker_silence_minutes (30m0s): the bound on background work a worker reports must be above the bound on its silence, since a shorter one would name a job that has barely begun and would race the repair that sends the same worker its phase's task again\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// The example's two commented wait bounds, uncommented as the file ships them, pass the check
// and are read as the defaults they state: the example as it ships, which names neither key, reads
// the same two bounds, so a default changed in the loader and not in the example fails here. The
// lines stay commented in the file itself, as the silence key's do.
func TestShippedLegionYAMLExampleCheckConfigReadsTheTwoWaitBoundsItStates(t *testing.T) {
	stated, err := configpkg.LoadForValidation(checkShippedExample(t, func(example string) string {
		return uncommentExample(t, example,
			"# worker_take_timeout_seconds: 300",
			"# worker_turn_start_timeout_seconds: 1500",
		)
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := configpkg.LoadForValidation(stageShippedExample(t, func(example string) string { return example }), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stated.WorkerTakeTimeout != defaults.WorkerTakeTimeout || stated.WorkerTurnStartTimeout != defaults.WorkerTurnStartTimeout {
		t.Fatalf("the example's wait bounds read as %s and %s, want the defaults a file naming neither key reads, %s and %s",
			stated.WorkerTakeTimeout, stated.WorkerTurnStartTimeout, defaults.WorkerTakeTimeout, defaults.WorkerTurnStartTimeout)
	}
}

// memoryGuardExampleLines are the example's commented memory_guard block, line by line, as an
// operator turning it on uncomments it: `issue_pods` first and `controller_pod` after it, in the
// file's order, since uncommentExample takes the first commented line that starts with each, and
// the two blocks carry the same members at the same defaults but the wait after a kill, 5 seconds
// for issue pods and 1 for the controller's pod.
var memoryGuardExampleLines = []string{
	"    # memory_guard:",
	"    #   issue_pods:",
	"    #     enabled: true",
	"    #     act_percent: 85",
	"    #     floor_percent: 10",
	"    #     wait_seconds: 5",
	"    #     interval_milliseconds: 100",
	"    #   controller_pod:",
	"    #     enabled: true",
	"    #     act_percent: 85",
	"    #     floor_percent: 10",
	"    #     wait_seconds: 1",
	"    #     interval_milliseconds: 100",
}

// The example's commented memory_guard block, uncommented, passes the same check and settles on
// the guard on for both pod kinds at the defaults the block states, which are the wire's own
// (config.DefaultMemoryGuard): a default changed in the wire and not in the example would have the
// example teach a number no daemon runs. The controller pod's wait_seconds is 1.
func TestMemoryGuardTheShippedExamplesBlockPassesCheckConfig(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (checkShippedExample).
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
	if got := cfg.Runtime.Kubernetes.MemoryGuard.ControllerPod.WaitSeconds; got != 1 {
		t.Errorf("the example's controller_pod waits %d seconds after a kill, want 1", got)
	}
}

// The example's memory_guard block, uncommented with either pod kind's act_percent at 91, one
// above the 90 both kinds take (INFRA-3531 R4 of spec version 43), is refused by the same check:
// exit 1 and one line naming that kind's whole key and both ends of its range, which is what an
// operator who edits the shipped block past that range reads. It checks the loader's refusal, not
// the range the block's own comment states. The block stays commented in the file itself: its
// values are the defaults, which apply without it.
func TestMemoryGuardTheShippedExampleRefusesAnActShareAboveItsRange(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (stageShippedExample).
	for _, kind := range []struct {
		name string
		max  int
	}{{"issue_pods", 90}, {"controller_pod", 90}} {
		t.Run(kind.name, func(t *testing.T) {
			config := stageShippedExample(t, func(example string) string {
				example = uncommentExample(t, example, memoryGuardExampleLines...)
				// The kind's own act_percent: the first after its key, since both kinds carry 85.
				at := strings.Index(example, "\n      "+kind.name+":")
				if at < 0 {
					t.Fatalf("the uncommented example has no %s block", kind.name)
				}
				return example[:at] + strings.Replace(example[at:], "act_percent: 85", fmt.Sprintf("act_percent: %d", kind.max+1), 1)
			})

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)
			t.Logf("legion config check exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())

			want := fmt.Sprintf("legion config check: runtime.kubernetes.memory_guard.%s.act_percent must be between 50 and %d\n", kind.name, kind.max)
			if code != 1 || out.Len() != 0 || errb.String() != want {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
			}
		})
	}
}

// The example's commented session_database block, its writer_audience included, uncommented under
// `session_store: cloudsql-iam` with session_dsn_secret taken out, passes the same check as
// shipped, with the same stdout. Its impersonate_service_account set to the account's login form
// rather than its email is refused, naming the member and the value, and a user that is not the
// account's login is refused, naming the login the account has. The block is where an operator
// reads what each member takes, so a member it showed wrongly would be an operator's file refused
// on their first check; here it is this test that fails instead. The block stays commented in the
// file itself: the shipped store is postgres, and the stdout
// TestShippedLegionYAMLExamplePassesCheckConfig holds is that store's.
func TestShippedLegionYAMLExamplesSessionDatabasePassesCheckConfigOnlyWithTheAccountsEmailAndLogin(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (stageShippedExample).
	for _, tc := range []struct {
		name           string
		edit           [][2]string
		code           int
		stdout, stderr string
	}{
		{name: "as shipped", code: 0, stdout: shippedExampleCheckOK},
		{name: "impersonate_service_account as the account's login",
			edit: [][2]string{{"impersonate_service_account: legion-sessions@example-project.iam.gserviceaccount.com", "impersonate_service_account: legion-sessions@example-project.iam"}},
			code: 1,
			stderr: `legion config check: runtime.kubernetes.session_database.impersonate_service_account "legion-sessions@example-project.iam" must be a Google service account's email,` +
				" such as <name>@<project>.iam.gserviceaccount.com\n"},
		{name: "user as another login",
			edit: [][2]string{{"user: legion-sessions@example-project.iam ", "user: other@example-project.iam "}},
			code: 1,
			stderr: `legion config check: runtime.kubernetes.session_database.user "other@example-project.iam" must be "legion-sessions@example-project.iam":` +
				" Cloud SQL's IAM login for the service account impersonate_service_account names is its email without .gserviceaccount.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := stageShippedExample(t, func(example string) string {
				example = uncommentExample(t, example,
					"    # session_database:",
					"    #   instance: example-project:us-central1:example-instance",
					"    #   name: legion_sessions",
					"    #   user: legion-sessions@example-project.iam",
					"    #   impersonate_service_account: legion-sessions@example-project.iam.gserviceaccount.com",
					"    #   writer_audience: //iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/legion-session-writer/providers/example-cluster",
				)
				for _, edit := range append([][2]string{
					{"\n    session_store: postgres ", "\n    session_store: cloudsql-iam "},
					{"\n    session_dsn_secret: ", "\n    # session_dsn_secret: "},
				}, tc.edit...) {
					if !strings.Contains(example, edit[0]) {
						t.Fatalf("the example has no %q", edit[0])
					}
					example = strings.Replace(example, edit[0], edit[1], 1)
				}
				return example
			})

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)

			if code != tc.code || out.String() != tc.stdout || errb.String() != tc.stderr {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want %d, %q and %q", code, out.String(), errb.String(), tc.code, tc.stdout, tc.stderr)
			}
		})
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
// and answers the configuration's path, ready for `legion config check` to be run over
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

// shippedExampleCheckOK is the whole stdout of `legion config check` on the shipped
// example, the same under either session store the example shows.
var shippedExampleCheckOK = "Config OK: project=WIDGETS design-gate-steps=spec\n" +
	productionCheckTesterLine("WIDGETS") + defaultStepsTodayLine("WIDGETS") +
	defaultCacheLine +
	"plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"

// checkShippedExample runs `legion config check` on the shipped example as edit leaves
// it, beside the files it names, and answers the file's path. It logs the command's exit code,
// stdout and stderr, so a `-v` run shows what the check printed for each edit of the example.
func checkShippedExample(t *testing.T, edit func(string) string) string {
	t.Helper()
	config := stageShippedExample(t, edit)

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "config", "check", "--config", config}, &out, &errb)
	t.Logf("legion config check exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())

	if code != 0 || out.String() != shippedExampleCheckOK {
		t.Fatalf("legion config check on the shipped example = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), shippedExampleCheckOK)
	}
	return config
}
