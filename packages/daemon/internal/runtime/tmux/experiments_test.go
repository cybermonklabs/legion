package tmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The two experiments every test here declares, each with one arm that names an overlay and an
// instructions file and one control arm that names neither: alpha's overlays and beta's are two
// lists of their own, so the order of the two in a launch's values can only come from the order
// the project's file declares them in.
var (
	alphaOverlays = []string{"/etc/legion-alpha/alpha.yml", "/etc/legion-alpha/alpha-extra.yml"}
	betaOverlays  = []string{"/etc/legion-beta/beta.yml"}
)

// twoExperimentVariants are the arms of the two experiments as the runtime is handed them
// (Options.ExperimentVariants, experiment.DeclaredVariants' order): every arm of the first
// experiment, then every arm of the second.
func twoExperimentVariants() []runtime.Variant {
	return []runtime.Variant{
		{Experiment: "alpha", Name: "on", ConfigFiles: alphaOverlays},
		{Experiment: "alpha", Name: "off"},
		{Experiment: "beta", Name: "on", ConfigFiles: betaOverlays},
		{Experiment: "beta", Name: "off"},
	}
}

// twoExperimentRuntime is a fake server runtime of a deployment declaring both experiments, on
// stateDir, with the operator's own overlay and baselineRoleFiles as role_config_files, so two
// launches over one state directory differ by the spec alone. reversed is the same deployment
// whose file declares beta before alpha: the negative control, which the runtime is handed in
// that order and nothing else distinguishes.
func twoExperimentRuntime(t *testing.T, stateDir string, reversed bool) *fakeServerRuntime {
	t.Helper()
	return newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
		o.StateDir = stateDir
		o.PiConfigFiles = []string{baselineOperatorOverlay}
		o.RoleConfigFiles = baselineRoleFiles
		o.ExperimentVariants = twoExperimentVariants()
		if reversed {
			o.ExperimentVariants = append(o.ExperimentVariants[2:], o.ExperimentVariants[:2]...)
		}
	})
}

// drawnIntoBoth is f's launch of role for a tree drawn into the `on` arm of both experiments,
// with the daemon's copies of the two arms' instructions written under dir, since a pane's
// launch reads every prompt file. reversed is the same launch under the file that declares beta
// first: the arms reach the spec in the file's order, which is the one thing the two differ by.
func drawnIntoBoth(t *testing.T, f *fakeServerRuntime, dir string, role claim.Role, reversed bool) runtime.SpawnSpec {
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
	instructions := []string{write("alpha-on.md", armInstructions["alpha-on.md"]+"\n"), write("beta-on.md", armInstructions["beta-on.md"]+"\n")}
	if reversed {
		slices.Reverse(variants)
		slices.Reverse(instructions)
	}
	spec := f.spec(t, role)
	spec.Prompt.DeploymentInstructionsPath = write("deployment.md", "Deployment instructions.\n")
	spec.Variants, spec.Prompt.VariantInstructionsPaths = variants, instructions
	return spec
}

// armInstructions are the texts drawnIntoBoth writes as the two `on` arms' instructions, by file
// name, each written with a trailing newline the composed prompt drops.
var armInstructions = map[string]string{"alpha-on.md": "Alpha instructions.", "beta-on.md": "Beta instructions."}

// panePrompt is the system prompt the pane's Oh My Pi is started with: the contents of the file
// the one --append-system-prompt word of the pane's shell command names, read as Oh My Pi reads
// it. The word is the command's last, and is unquoted as the pane's shell unquotes it
// (shellprefix.Word: bare, or single-quoted with each `'` closed, escaped and reopened).
func panePrompt(t *testing.T, role claim.Role, argv []string) string {
	t.Helper()
	_, word, ok := strings.Cut(argv[len(argv)-1], "--append-system-prompt ")
	if !ok || word == "" {
		t.Fatalf("the %s's pane command names no --append-system-prompt word:\n%s", role, argv[len(argv)-1])
	}
	path := word
	if quoted, ok := strings.CutPrefix(word, "'"); ok {
		path = strings.ReplaceAll(strings.TrimSuffix(quoted, "'"), `'\''`, "'")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("the %s's --append-system-prompt word %s names no absolute path", role, word)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the %s's prompt file: %v", role, err)
	}
	return string(text)
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
// (runtime/sandbox's experiments_test).
func TestATreeInTwoExperimentsNamesEveryArmInTheFilesOrder(t *testing.T) {
	launches := 0
	for _, tc := range []struct {
		name     string
		reversed bool
		overlays []string
		prompt   []string
	}{
		{"the file's order", false, slices.Concat(alphaOverlays, betaOverlays), []string{"alpha-on.md", "beta-on.md"}},
		{"the other order", true, slices.Concat(betaOverlays, alphaOverlays), []string{"beta-on.md", "alpha-on.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, role := range claim.Roles {
				f := twoExperimentRuntime(t, t.TempDir(), tc.reversed)
				spec := drawnIntoBoth(t, f, dir, role, tc.reversed)
				argv, pairs := newWindow(t, f, spec)
				launches++
				want := "PI_CONFIG_FILES=" + strings.Join(slices.Concat([]string{baselineOperatorOverlay}, baselineRoleFiles[role], tc.overlays), ":")
				if !slices.Contains(pairs, want) {
					t.Errorf("the %s's pairs = %q, want %q", role, pairs, want)
				}
				prompt := panePrompt(t, role, argv)
				tail := "Deployment instructions."
				for _, name := range tc.prompt {
					tail += "\n\n" + armInstructions[name]
				}
				if !strings.HasSuffix(prompt, tail) {
					t.Errorf("the %s's system prompt =\n%s\nwant it to end\n%s", role, prompt, tail)
				}
			}
		})
	}
	if want := 2 * len(claim.Roles); launches != want {
		t.Fatalf("the table read %d launches, want %d: six roles under each order", launches, want)
	}
}

// A tree drawn into a control arm — one that names neither an overlay nor an instructions file —
// launches the pane a tree in no experiment launches, byte for byte but for one pair, and is
// started with the same system prompt: a launch names the files its arms name, and a control arm
// names none. The one pair is its spend attributes, which name the control arms in legion.arms, so
// a call of a control tree reads as control rather than as a tree in no experiment.
func TestATreeInAControlVariantLaunchesTheBytesOfATreeInNoExperiment(t *testing.T) {
	stateDir := t.TempDir()
	// One spec for both launches, so the two differ by the arms the spec carries alone.
	control := twoExperimentRuntime(t, stateDir, false).spec(t, claim.RoleTester)
	control.Variants = []runtime.Variant{{Experiment: "alpha", Name: "off"}, {Experiment: "beta", Name: "off"}}
	none := control
	none.Variants = nil

	// Both launches write the one prompt file of the claim, so each is read before the next.
	withControl, controlPairs := newWindow(t, twoExperimentRuntime(t, stateDir, false), control)
	controlPrompt := panePrompt(t, claim.RoleTester, withControl)
	without, nonePairs := newWindow(t, twoExperimentRuntime(t, stateDir, false), none)
	const arms = "," + runtime.SpendArmsAttribute + "=alpha%3Aoff%2Cbeta%3Aoff"
	named := 0
	unnamed := slices.Clone(withControl)
	for i, word := range unnamed {
		if value, ok := strings.CutPrefix(word, runtime.SpendAttributesVariable+"="); ok && strings.HasSuffix(value, arms) {
			unnamed[i] = strings.TrimSuffix(word, arms)
			named++
		}
	}
	if named != 1 {
		t.Errorf("a control tree's pane names %s %d times in %s, want once:\n%q", arms, named, runtime.SpendAttributesVariable, withControl)
	}
	if !slices.Equal(unnamed, without) {
		t.Errorf("a control tree's pane, its arms aside =\n%q\nwant the one a tree in no experiment gets:\n%q", unnamed, without)
	}
	if nonePrompt := panePrompt(t, claim.RoleTester, without); controlPrompt != nonePrompt {
		t.Errorf("a control tree's system prompt =\n%s\nwant the one a tree in no experiment gets:\n%s", controlPrompt, nonePrompt)
	}
	want := "PI_CONFIG_FILES=" + strings.Join(slices.Concat([]string{baselineOperatorOverlay}, baselineRoleFiles[claim.RoleTester]), ":")
	for _, pairs := range [][]string{controlPairs, nonePairs} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the pane's pairs = %q, want %q", pairs, want)
		}
	}
}

// The controller's pane carries no arm under a deployment that declares two experiments, as
// under one that declares none of its own and writes no `memory` block, whose memory arms name no
// file and stand here as nil: an experiment's unit is a tree or an issue of one, and the
// controller runs in neither. Two controller launches are read, and the test fails below two, so
// neither deployment can quietly drop out of the comparison.
func TestTheControllerCarriesNoVariant(t *testing.T) {
	read := 0
	for _, tc := range []struct {
		name     string
		variants []runtime.Variant
	}{
		{"a deployment declaring two experiments", twoExperimentVariants()},
		{"a deployment whose arms name no file", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
				o.PiConfigFiles = []string{baselineOperatorOverlay}
				o.RoleConfigFiles = baselineRoleFiles
				o.ExperimentVariants = tc.variants
			})
			_, pairs := newWindow(t, f, controllerSpec(t, "omp"))
			read++
			want := "PI_CONFIG_FILES=" + strings.Join(slices.Concat([]string{baselineOperatorOverlay}, baselineRoleFiles[claim.RoleController]), ":")
			if !slices.Contains(pairs, want) {
				t.Errorf("the controller's pairs = %q, want %q: it is in no experiment", pairs, want)
			}
		})
	}
	if read != 2 {
		t.Fatalf("the test read %d controller launches, want 2", read)
	}
}

// A controller's launch that carries an arm is refused before anything touches tmux, in the one
// wording every site uses (runtime.validateControllerShape), and one told a variant's
// instructions is refused beside it.
func TestAControllerCarryingAVariantIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*runtime.SpawnSpec)
		want string
	}{
		{"an arm", func(s *runtime.SpawnSpec) { s.Variants = twoExperimentVariants()[:1] },
			"spawn legion-omp-controller: the controller runs in no tree, so it is in no experiment"},
		{"a variant's instructions", func(s *runtime.SpawnSpec) {
			s.Prompt.VariantInstructionsPaths = []string{"/state/experiments/alpha/on.md"}
		}, "spawn legion-omp-controller: the controller is told no variant instructions (got /state/experiments/alpha/on.md)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := twoExperimentRuntime(t, t.TempDir(), false)
			spec := controllerSpec(t, "omp")
			tc.set(&spec)
			if _, err := f.Spawn(context.Background(), spec); err == nil || err.Error() != tc.want {
				t.Fatalf("Spawn = %v, want %q", err, tc.want)
			}
		})
	}
}
