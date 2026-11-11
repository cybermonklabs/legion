package sandbox

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The two experiments every test here declares, each with one arm that names an overlay and an
// instructions file and one control arm that names neither: alpha's files sit under alphaDir and
// beta's under betaDir, two mounts of the operator's pod, so the order of the two in a launch's
// values can only come from the order the project's file declares them in.
const (
	alphaDir = "/etc/legion-alpha"
	betaDir  = "/etc/legion-beta"
)

var (
	alphaOverlays = []string{alphaDir + "/alpha.yml", alphaDir + "/alpha-extra.yml"}
	betaOverlays  = []string{betaDir + "/beta.yml"}
)

// twoExperimentVariants are the arms of the two experiments as the runtime is handed them
// (Options.ExperimentVariants, declaredVariants' order): every arm of the first experiment, then
// every arm of the second.
func twoExperimentVariants() []runtime.Variant {
	return []runtime.Variant{
		{Experiment: "alpha", Name: "on", ConfigFiles: alphaOverlays},
		{Experiment: "alpha", Name: "off"},
		{Experiment: "beta", Name: "on", ConfigFiles: betaOverlays},
		{Experiment: "beta", Name: "off"},
	}
}

// mountExperiments adds to the operator's pod one ConfigMap volume per named directory, each
// with a volume name of its own, as the operator mounts an arm's overlays.
func mountExperiments(o *Options, dirs ...string) {
	for _, dir := range dirs {
		volume, mount := memoryMount(dir)
		volume.Name += strings.ReplaceAll(dir, "/", "-")
		mount.Name = volume.Name
		o.Pod.Volumes = append(o.Pod.Volumes, volume)
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, mount)
	}
}

// twoExperimentRuntime is the test runtime of a deployment declaring both experiments, with
// baselineRoleFiles as role_config_files and both ConfigMaps mounted on every pod. reversed is
// the same deployment whose file declares beta before alpha: the negative control, which the
// runtime is handed in that order and nothing else distinguishes.
func twoExperimentRuntime(t *testing.T, reversed bool) *Runtime {
	t.Helper()
	return runtimeWithOptions(t, func(o *Options) {
		mountExperiments(o, alphaDir, betaDir)
		o.RoleConfigFiles = baselineRoleFiles
		o.ExperimentVariants = twoExperimentVariants()
		if reversed {
			o.ExperimentVariants = append(o.ExperimentVariants[2:], o.ExperimentVariants[:2]...)
		}
	})
}

// drawnIntoBoth is role's launch for a tree drawn into the `on` arm of both experiments, with
// the daemon's copies of the two arms' instructions written under dir. reversed is the same
// launch under the file that declares beta first: the arms reach the spec in the file's order,
// which is the one thing the two differ by.
func drawnIntoBoth(t *testing.T, dir string, role claim.Role, reversed bool) runtime.SpawnSpec {
	t.Helper()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	variants := []runtime.Variant{
		{Experiment: "alpha", Name: "on", ConfigFiles: alphaOverlays},
		{Experiment: "beta", Name: "on", ConfigFiles: betaOverlays},
	}
	instructions := []string{write("alpha-on.md", "Alpha instructions.\n"), write("beta-on.md", "Beta instructions.\n")}
	if reversed {
		slices.Reverse(variants)
		slices.Reverse(instructions)
	}
	token, err := claim.NewToken(testProject, testTree, role)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t, token, role, testTree)
	spec.Variants, spec.Prompt.VariantInstructionsPaths = variants, instructions
	return spec
}

// A tree drawn into an arm of each of two experiments is told, for every role of the workflow,
// the operator's own overlays, then the role's own, then each arm's, in the order the project's
// file declares the experiments; and its system prompt ends with the deployment's instructions
// and then each arm's text in that same order. The second half of the table is the negative
// control: the same deployment with its two experiments declared the other way round produces
// the other order in both values, so the order comes from the file and from nothing else.
//
// Twelve launches, two readings of each: the six roles of claim.Roles under each of the two
// orders. The count is asserted, so a table that stopped covering a role would fail rather than
// prove less than it says. The other runtime's twelve are its own package's
// (runtime/tmux's experiments_test).
func TestATreeInTwoExperimentsNamesEveryArmInTheFilesOrder(t *testing.T) {
	dir := t.TempDir()
	operator := operatorPod().Env["PI_CONFIG_FILES"]
	launches := 0
	for _, tc := range []struct {
		name     string
		reversed bool
		overlays []string
		prompt   []string
	}{
		{"the file's order", false, slices.Concat(alphaOverlays, betaOverlays), []string{"Alpha instructions.", "Beta instructions."}},
		{"the other order", true, slices.Concat(betaOverlays, alphaOverlays), []string{"Beta instructions.", "Alpha instructions."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := twoExperimentRuntime(t, tc.reversed)
			for _, role := range claim.Roles {
				spec := drawnIntoBoth(t, dir, role, tc.reversed)
				worker := workerOf(t, r, spec)
				launches++
				want := strings.Join(slices.Concat([]string{operator}, baselineRoleFiles[role], tc.overlays), ":")
				if got := envOf(worker)["PI_CONFIG_FILES"]; got != want {
					t.Errorf("the %s's PI_CONFIG_FILES = %q, want %q", role, got, want)
				}
				prompt := appendSystemPromptOf(t, role, worker.Command)
				if tail := "\n\nDeployment instructions.\n\n" + strings.Join(tc.prompt, "\n\n"); !strings.HasSuffix(prompt, tail) {
					t.Errorf("the %s's system prompt =\n%q\nwant it to end\n%q", role, prompt, tail)
				}
			}
		})
	}
	if want := 2 * len(claim.Roles); launches != want {
		t.Fatalf("the table read %d launches, want %d: six roles under each order", launches, want)
	}
}

// A tree drawn into a control arm — one that names neither an overlay nor an instructions file —
// launches the bytes a tree in no experiment launches, pod and process alike: a launch names the
// files its arms name, and a control arm names none.
func TestATreeInAControlVariantLaunchesTheBytesOfATreeInNoExperiment(t *testing.T) {
	r := twoExperimentRuntime(t, false)
	control := workerSpec(t)
	control.Variants = []runtime.Variant{{Experiment: "alpha", Name: "off"}, {Experiment: "beta", Name: "off"}}
	none := workerSpec(t)
	if got, want := podOf(t, r, control), podOf(t, r, none); !reflect.DeepEqual(got, want) {
		t.Errorf("a control tree's pod =\n%+v\nwant the one a tree in no experiment gets:\n%+v", got, want)
	}
	if got, want := workerOf(t, r, control), workerOf(t, r, none); !reflect.DeepEqual(got, want) {
		t.Errorf("a control tree's process =\n%+v\nwant the one a tree in no experiment gets:\n%+v", got, want)
	}
}

// The controller's launch carries no arm under a deployment that declares two experiments, as
// under one that declares none: an experiment's unit is a tree or an issue of one, and the
// controller runs in neither. Two controller launches are read, and the test fails below two, so
// neither deployment can quietly drop out of the comparison.
func TestTheControllerCarriesNoVariant(t *testing.T) {
	read := 0
	for _, tc := range []struct {
		name string
		r    *Runtime
	}{
		{"a deployment declaring two experiments", twoExperimentRuntime(t, false)},
		{"a deployment declaring none", baselineRuntime(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := controllerSpec(t)
			spec.Prompt.RolePromptPaths = []string{spec.Prompt.RolePromptPaths[0]}
			worker := workerOf(t, tc.r, spec)
			read++
			want := strings.Join(slices.Concat([]string{operatorPod().Env["PI_CONFIG_FILES"]}, baselineRoleFiles[claim.RoleController]), ":")
			if got := envOf(worker)["PI_CONFIG_FILES"]; got != want {
				t.Errorf("the controller's PI_CONFIG_FILES = %q, want %q: it is in no experiment", got, want)
			}
			for _, overlay := range slices.Concat(alphaOverlays, betaOverlays) {
				if strings.Contains(envOf(worker)["PI_CONFIG_FILES"], overlay) {
					t.Errorf("the controller's PI_CONFIG_FILES names %s, an arm's overlay", overlay)
				}
			}
		})
	}
	if read != 2 {
		t.Fatalf("the test read %d controller launches, want 2", read)
	}
}

// A launch over a pod built before an arm's mount was added is told none of that arm's overlays —
// the degrade a kept pod has always had — and says so once, naming the issue, the role, the
// experiment and the arm, since nothing else would record that this generation ran outside the
// arm it was drawn into. The other arm, whose mount the pod does hold, still reaches the role.
func TestAKeptPodMissingOneArmsMountDropsThatArmAndLogsIt(t *testing.T) {
	before := runtimeWithOptions(t, func(o *Options) {
		mountExperiments(o, alphaDir)
		o.RoleConfigFiles = baselineRoleFiles
		o.ExperimentVariants = twoExperimentVariants()[:2]
	})
	after := twoExperimentRuntime(t, false)
	spec := drawnIntoBoth(t, t.TempDir(), claim.RoleTester, false)
	s := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{Spec: podOf(t, before, spec)}}}
	l := launchOf(t, after, spec)
	kept, readable := after.keptBy(s, l)
	if !readable {
		t.Fatal("keptBy could not read the pod's template")
	}
	if !slices.Equal(kept.variantFiles, alphaOverlays) {
		t.Fatalf("the kept pod holds %q, want alpha's alone: %q", kept.variantFiles, alphaOverlays)
	}
	l.kept = &kept
	logs := &bytes.Buffer{}
	after.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	want := strings.Join(slices.Concat([]string{operatorPod().Env["PI_CONFIG_FILES"]}, baselineRoleFiles[claim.RoleTester], alphaOverlays), ":")
	if got := envOf(corev1.Container{Env: after.roleOperatorEnv(l)})["PI_CONFIG_FILES"]; got != want {
		t.Errorf("the kept launch's PI_CONFIG_FILES = %q, want %q", got, want)
	}
	line := logs.String()
	for _, named := range []string{"issue=" + spec.Issue, "role=tester", "experiment=beta", "variant=on", betaOverlays[0]} {
		if !strings.Contains(line, named) {
			t.Errorf("the launch logged\n%s\nwant one line naming %s", line, named)
		}
	}
	if strings.Contains(line, "experiment=alpha") {
		t.Errorf("the launch logged a drop of alpha, whose mount the pod holds:\n%s", line)
	}
}
