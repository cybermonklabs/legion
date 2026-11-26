package sandbox

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// An experiment arm's folder of texts reaches the agent in a pod as one extension
// root, built by the launcher on the issue's volume at `<role state home>/legion-texts/g<N>`, and
// Oh My Pi must load it before both plugins: the root's copy of a skill the plugin ships too keeps
// the plain name, and the plugin's differing copy is only an alias (Oh My Pi's skills
// documentation, "Collision and duplicate handling"), which the arm's settings layer then hides. A root named after either plugin
// would make the plugin's copy the one the model reads, so the order of agentArgv's flags is the
// observable of the requirement. The order check below reads the argument list a pod's agent is
// started with and passes only a list whose extension roots are the arm's root, pi-envoy's and
// pi-legion's, in that order, all before `--model`. The last subtest is the negative control of
// the order test: the same check on the arguments with the root after pi-legion fails, so a check
// that passed whatever the order was would show here.
func TestATextsLaunchPutsItsRootAheadOfBothPlugins(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.Variants = []runtime.Variant{{Experiment: "shorter-prompts", Name: "texts", Texts: "/etc/legion/texts/shorter"}}
	spec.Texts = &runtime.Texts{
		Folder:      "/etc/legion/texts/shorter",
		Fingerprint: strings.Repeat("ab", 32),
		Experiment:  "shorter-prompts",
		Variant:     "texts",
	}
	root := roleStateHome(spec.Role) + "/legion-texts/g" + strconv.FormatUint(spec.Generation, 10)

	t.Run("a launch carrying a folder", func(t *testing.T) {
		argv := launchOf(t, r, spec).agentArgv(r.agent)
		if err := rootAheadOfBothPlugins(argv, root); err != nil {
			t.Errorf("agentArgv = %q: %v", argv, err)
		}
		want := slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(spec.Generation)})
		if !slices.Equal(argv, want) {
			t.Errorf("agentArgv = %q, want %q", argv, want)
		}
	})

	t.Run("a launch that resumes names its generation's own root", func(t *testing.T) {
		later := spec
		later.Generation, later.BootToken, later.ResumeSessionFile = 2, "boot-g2", resumeSession
		argv := launchOf(t, r, later).agentArgv(r.agent)
		if err := rootAheadOfBothPlugins(argv, roleStateHome(later.Role)+"/legion-texts/g2"); err != nil {
			t.Errorf("agentArgv of generation 2 = %q: %v", argv, err)
		}
	})

	t.Run("a launch carrying no folder keeps the two plugins alone", func(t *testing.T) {
		plain := spec
		plain.Variants, plain.Texts = nil, nil
		argv := launchOf(t, r, plain).agentArgv(r.agent)
		want := slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(plain.Generation)})
		if !slices.Equal(argv, want) {
			t.Errorf("agentArgv with no folder = %q, want %q", argv, want)
		}
	})

	t.Run("negative control: the order test fails arguments with the root after pi-legion", func(t *testing.T) {
		model := []string{"--model", "@default", "--mode", "rpc"}
		for name, argv := range map[string][]string{
			"the root after pi-legion":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--extension", root}, model),
			"the root between the two":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", root, "--extension", legionPlugin}, model),
			"the root after the model":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default", "--extension", root, "--mode", "rpc"}),
			"no root":                     slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin}, model),
			"a root of another role":      slices.Concat(r.agent, []string{"--extension", "/legion/state/other/legion-texts/g1", "--extension", envoyPlugin, "--extension", legionPlugin}, model),
			"the root and a plugin twice": slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin, "--extension", legionPlugin}, model),
		} {
			if err := rootAheadOfBothPlugins(argv, root); err == nil {
				t.Errorf("the order test passed %s: %q", name, argv)
			}
		}
		good := slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin}, model)
		if err := rootAheadOfBothPlugins(good, root); err != nil {
			t.Errorf("the order test failed arguments with the root first: %v", err)
		}
	})
}

// rootAheadOfBothPlugins is nil when argv names exactly three extension roots, root, then the
// image's pi-envoy, then its pi-legion, and names all three before `--model`.
func rootAheadOfBothPlugins(argv []string, root string) error {
	var roots []string
	last, model := -1, -1
	for i, word := range argv {
		switch word {
		case "--extension":
			if i+1 == len(argv) {
				return fmt.Errorf("--extension names no root")
			}
			roots = append(roots, argv[i+1])
			last = i
		case "--model":
			model = i
		}
	}
	switch {
	case !slices.Equal(roots, []string{root, envoyPlugin, legionPlugin}):
		return fmt.Errorf("the extension roots are %q, want the arm's root %q, then %q and %q", roots, root, envoyPlugin, legionPlugin)
	case model < 0:
		return fmt.Errorf("no --model flag, so the roots cannot come before it")
	case last > model:
		return fmt.Errorf("an --extension at word %d comes after --model at word %d", last, model)
	}
	return nil
}

// The arm every launch below runs in: its folder under an operator mount the loader requires
// (runtime.kubernetes.pod.volume_mounts), its own overlay under another, and, beside it, one arm of
// another experiment the unit was drawn into, which a launch that falls back keeps. textsInFolder
// and textsArmWords mark the prompt the folder's launch carries and the control's never does.
const (
	textsMount      = "/etc/legion/texts"
	textsFolderPath = textsMount + "/shorter"
	textsArmDir     = "/etc/legion/arms"
	textsExperiment = "shorter-prompts"
	textsArmName    = "texts"
	textsInFolder   = "in the folder's words"
	textsArmWords   = "Arm instructions."
)

var (
	textsFingerprint = strings.Repeat("ab", 32)
	textsArmOverlay  = textsArmDir + "/shorter.yml"
	textsOtherArm    = runtime.Variant{Experiment: "other", Name: "on"}
)

// textsArm is the arm of the experiment `shorter-prompts` that names the folder and one overlay.
func textsArm() runtime.Variant {
	return runtime.Variant{Experiment: textsExperiment, Name: textsArmName, ConfigFiles: []string{textsArmOverlay}, Texts: textsFolderPath}
}

// textsLaunch is spec as the daemon builds the launch of a unit drawn into textsArm and textsOtherArm
// when its own check of the folder passed (specs.armTexts): the folder's prompt, carrying a marker
// the control's lacks, the arm's instructions, both arms, and Texts with the control's prompt and
// arms as its fallback (runtime.SpawnSpec.FallBack): Legion's prompt, the other arm alone.
func textsLaunch(t *testing.T, spec runtime.SpawnSpec) runtime.SpawnSpec {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	control := spec.Prompt
	folder := control
	folder.RolePromptPaths = []string{write("role.md", "You are the "+string(spec.Role)+", "+textsInFolder+".\n")}
	folder.VariantInstructionsPaths = []string{write("arm.md", textsArmWords+"\n")}
	spec.Variants = []runtime.Variant{textsArm(), textsOtherArm}
	spec.Prompt = folder
	spec.Texts = &runtime.Texts{
		Folder: textsFolderPath, Fingerprint: textsFingerprint, Experiment: textsExperiment, Variant: textsArmName,
		Fallback: runtime.TextsFallback{Prompt: control, Variants: []runtime.Variant{textsOtherArm}},
	}
	return spec
}

// textsReports is what the runtime told the daemon, through Options.TextsFellBack, of each launch
// that ran its fallback.
type textsReports struct {
	mu      sync.Mutex
	reports []runtime.TextsReport
}

func (c *textsReports) record(report runtime.TextsReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, report)
}

func (c *textsReports) told() []runtime.TextsReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reports)
}

// textsRig is a rig whose deployment declares textsArm and textsOtherArm, mounts the folder and the
// arm's overlay from the operator's pod, names the baseline role overlays, and tells reports of
// each fallback its runtime finds. objects are the cluster's starting state.
func textsRig(t *testing.T, reports *textsReports, objects []k8sruntime.Object, options ...rigOption) *rig {
	t.Helper()
	declared := withOptions(func(o *Options) {
		o.Pod = operatorPod()
		mountExperiments(o, textsArmDir, textsMount)
		o.RoleConfigFiles = baselineRoleFiles
		o.ExperimentVariants = []runtime.Variant{textsArm(), textsOtherArm}
		o.TextsFellBack = reports.record
	})
	return newRig(t, objects, append([]rigOption{declared}, options...)...)
}

// startIDs are the ids of starts, in the order they were sent.
func startIDs(starts []shimwire.LauncherStart) []string {
	ids := make([]string, len(starts))
	for i, start := range starts {
		ids[i] = start.ID
	}
	return ids
}

// extensionRoots are the roots argv names with `--extension`, in order.
func extensionRoots(argv []string) []string {
	var roots []string
	for i, word := range argv {
		if word == "--extension" && i+1 < len(argv) {
			roots = append(roots, argv[i+1])
		}
	}
	return roots
}

// armsOf is the value of the spend attributes' legion.arms in env, percent-decoded: the arms the
// launch's model calls name, as `<experiment>:<variant>` joined by commas; "" for none.
func armsOf(t *testing.T, env map[string]string) string {
	t.Helper()
	for _, member := range strings.Split(env[runtime.SpendAttributesVariable], ",") {
		if value, ok := strings.CutPrefix(member, runtime.SpendArmsAttribute+"="); ok {
			arms, err := url.QueryUnescape(value)
			if err != nil {
				t.Fatalf("%s names arms %q: %v", runtime.SpendAttributesVariable, value, err)
			}
			return arms
		}
	}
	return ""
}

// installedRoots are the plugin roots an installing start's agent loads (pluginRoots).
func installedRoots(role claim.Role, generation uint64) []string {
	envoy, legion := pluginRoots(pluginGenDir(roleStateHome(role), generation))
	return []string{envoy, legion}
}

// wantTheFolderStart holds start to be the start of armed's folder: the folder, its fingerprint,
// the root and the two plugins' roots handed to the launcher; the root first in the agent's
// arguments and named in LEGION_TEXTS_ROOT; its settings layer last in PI_CONFIG_FILES, after the
// operator's overlay, the role's and the arm's; the arm named in legion.arms; and the prompt the
// folder's role parts make, with the arm's instructions.
func wantTheFolderStart(t *testing.T, armed runtime.SpawnSpec, start shimwire.LauncherStart, root string) {
	t.Helper()
	if err := start.Validate(); err != nil {
		t.Errorf("the launcher refuses start %s: %v", start.ID, err)
	}
	want := &shimwire.LauncherTexts{Folder: textsFolderPath, Fingerprint: textsFingerprint, Root: root, EnvoyRoot: envoyPlugin, LegionRoot: legionPlugin}
	if !reflect.DeepEqual(start.Texts, want) {
		t.Errorf("start %s hands the launcher %+v, want %+v", start.ID, start.Texts, want)
	}
	if got := extensionRoots(start.Argv); !slices.Equal(got, []string{root, envoyPlugin, legionPlugin}) {
		t.Errorf("start %s loads the roots %q, want the arm's root %q, then %q and %q", start.ID, got, root, envoyPlugin, legionPlugin)
	}
	env := startEnv(start)
	if got := env[texts.RootVariable]; got != root {
		t.Errorf("start %s tells the role %s=%q, want %q", start.ID, texts.RootVariable, got, root)
	}
	layers := slices.Concat([]string{operatorPod().Env["PI_CONFIG_FILES"]}, baselineRoleFiles[armed.Role], []string{textsArmOverlay, root + "/" + texts.SettingsLayerName})
	if got := strings.Split(env["PI_CONFIG_FILES"], ":"); !slices.Equal(got, layers) {
		t.Errorf("start %s names the settings layers %q, want %q: the root's layer last", start.ID, got, layers)
	}
	if got, want := armsOf(t, env), runtime.SpendArmsValue(armed.Variants); got != want {
		t.Errorf("start %s names the arms %q in legion.arms, want %q", start.ID, got, want)
	}
	prompt, err := runtime.ComposePrompt(armed.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if got := start.Files[runtime.SystemPromptFile]; got != prompt || !strings.Contains(got, textsInFolder) || !strings.Contains(got, textsArmWords) {
		t.Errorf("start %s carries the prompt %q, want the folder's %q", start.ID, got, prompt)
	}
}

// wantTheControlsStart holds start to be the start of armed's control, the launch of no arm of its
// experiment: no folder handed to the launcher, no root in the agent's arguments or anywhere in the
// environment, Legion's own prompt with no marker of the folder's or of the arm's instructions, the
// overlays of the operator and the role alone (neither the arm's nor a texts layer), and the arm
// out of legion.arms while the other experiment's stays. roots are the plugin roots its agent loads.
func wantTheControlsStart(t *testing.T, armed runtime.SpawnSpec, start shimwire.LauncherStart, roots []string) {
	t.Helper()
	if err := start.Validate(); err != nil {
		t.Errorf("the launcher refuses start %s: %v", start.ID, err)
	}
	if start.Texts != nil {
		t.Errorf("start %s hands the launcher the folder %+v, want none", start.ID, start.Texts)
	}
	control, _ := armed.FallBack(texts.Fault{Cause: texts.Gone})
	prompt, err := runtime.ComposePrompt(control.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if got := start.Files[runtime.SystemPromptFile]; got != prompt || strings.Contains(got, textsInFolder) || strings.Contains(got, textsArmWords) {
		t.Errorf("start %s carries the prompt %q, want Legion's %q with no mark of the folder or of the arm", start.ID, got, prompt)
	}
	if got := extensionRoots(start.Argv); !slices.Equal(got, roots) {
		t.Errorf("start %s loads the roots %q, want %q alone: no root of the folder", start.ID, got, roots)
	}
	for _, word := range slices.Concat(start.Argv, start.Env) {
		if strings.Contains(word, shimwire.TextsRootsDir) {
			t.Errorf("start %s names a root of a folder in %q", start.ID, word)
		}
	}
	env := startEnv(start)
	if root, set := env[texts.RootVariable]; set {
		t.Errorf("start %s tells the role %s=%q, want none", start.ID, texts.RootVariable, root)
	}
	layers := slices.Concat([]string{operatorPod().Env["PI_CONFIG_FILES"]}, baselineRoleFiles[armed.Role])
	if got := strings.Split(env["PI_CONFIG_FILES"], ":"); !slices.Equal(got, layers) {
		t.Errorf("start %s names the settings layers %q, want %q: none of the arm's overlays, no texts layer", start.ID, got, layers)
	}
	if got, want := armsOf(t, env), runtime.SpendArmsValue(control.Variants); got != want || strings.Contains(got, textsExperiment) {
		t.Errorf("start %s names the arms %q in legion.arms, want %q: no arm of the experiment %s", start.ID, got, want, textsExperiment)
	}
}

// wantOneReport holds reports to be one fallback of armed's launch, for want: the claim, issue, role
// and generation of the launch, the experiment and arm, the folder with the fingerprint it was read
// at, and the fault, which names the folder.
func wantOneReport(t *testing.T, reports *textsReports, armed runtime.SpawnSpec, want texts.Fault) runtime.TextsReport {
	t.Helper()
	told := reports.told()
	if len(told) != 1 {
		t.Fatalf("the runtime told the daemon of %d fallbacks %+v, want exactly one", len(told), told)
	}
	want.Dir = textsFolderPath
	wanted := runtime.TextsReport{
		Claim: armed.Claim, Issue: armed.Issue, Role: armed.Role, Generation: armed.Generation,
		Experiment: textsExperiment, Variant: textsArmName, Folder: textsFolderPath, Fingerprint: textsFingerprint, Fault: want,
	}
	if !reflect.DeepEqual(told[0], wanted) {
		t.Errorf("the runtime told the daemon %+v, want %+v", told[0], wanted)
	}
	return told[0]
}

// A launch whose own check of the folder failed at the daemon is already the control's, which the
// daemon builds with no Texts (specs.armTexts) and has told the issue of itself: the runtime sends
// one start, of no folder, for every cause the daemon finds (foundByTheDaemon), tells no one, and
// hands the launcher nothing to check.
func TestADaemonTextsFaultSendsOnlyTheControlsStart(t *testing.T) {
	for _, cause := range foundByTheDaemon {
		t.Run(string(cause), func(t *testing.T) { runRowDaemonFault(t, cause) })
	}
}

func runRowDaemonFault(t *testing.T, cause texts.Cause) {
	t.Helper()
	reports := &textsReports{}
	g := textsRig(t, reports, nil, withChecksTexts())
	armed := textsLaunch(t, workerSpec(t))
	spec, _ := armed.FallBack(texts.Fault{Cause: cause, Path: "roles/tester.md"})
	if spec.Texts != nil {
		t.Fatalf("the daemon's control carries the folder %+v", spec.Texts)
	}
	g.spawn(spec)
	starts := startsSent(g, spec.Claim, 0)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-1"}) {
		t.Fatalf("the launch sent the starts %q, want start-1 alone", ids)
	}
	wantTheControlsStart(t, armed, starts[0], []string{envoyPlugin, legionPlugin})
	if told := reports.told(); len(told) != 0 {
		t.Errorf("the runtime told the daemon of %+v, which the daemon found and reported itself", told)
	}
}

// A start carries its arm's folder to the launcher: the folder as the operator named it, the
// fingerprint the daemon read it at, the root to build under the role's state home, and the two
// plugins' roots; the generation's own root is named first in the agent's arguments and in
// LEGION_TEXTS_ROOT, with its settings layer last in PI_CONFIG_FILES and the arm in legion.arms.
// The launcher accepts it, so the one start is the whole launch and no one is told of a fallback.
func TestATextsLaunchCarriesItsFolderToTheLauncher(t *testing.T) {
	reports := &textsReports{}
	g := textsRig(t, reports, nil, withChecksTexts())
	spec := textsLaunch(t, workerSpec(t))
	g.spawn(spec)
	starts := startsSent(g, spec.Claim, 0)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-1"}) {
		t.Fatalf("the launch sent the starts %q, want start-1 alone", ids)
	}
	wantTheFolderStart(t, spec, starts[0], roleStateHome(spec.Role)+"/legion-texts/g1")
	if told := reports.told(); len(told) != 0 {
		t.Errorf("the runtime told the daemon of %+v, want no fallback", told)
	}
	// The root's directory is the start's own XDG_STATE_HOME's, which is what the launcher holds
	// the root to (shimwire.LauncherStart.Validate); it is checked there above.
	if got := startEnv(starts[0])["XDG_STATE_HOME"]; got != roleStateHome(spec.Role) {
		t.Errorf("the start tells the role XDG_STATE_HOME=%q, want %q", got, roleStateHome(spec.Role))
	}
}

// A pod whose launcher did not say in its hello that it checks folders (an image of an earlier
// release) would ignore the folder and start an unchecked root, so it is sent the control's start
// alone, and the daemon is told the cause launcher-cannot-check, once. The launch is no failure.
func TestALauncherThatCannotCheckTextsGetsTheControlsStart(t *testing.T) { runRowCannotCheck(t) }

func runRowCannotCheck(t *testing.T) runtime.TextsReport {
	t.Helper()
	reports := &textsReports{}
	g := textsRig(t, reports, nil)
	spec := textsLaunch(t, workerSpec(t))
	g.spawn(spec)
	starts := startsSent(g, spec.Claim, 0)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-1"}) {
		t.Fatalf("the launch sent the starts %q, want start-1 alone", ids)
	}
	wantTheControlsStart(t, spec, starts[0], []string{envoyPlugin, legionPlugin})
	return wantOneReport(t, reports, spec, texts.Fault{Cause: texts.LauncherCannotCheck})
}

// A launch that installs Legion's plugin pair after its start into the pod it keeps cannot have its
// folder's agents and skills checked against the plugins it loads before that start, so it is sent
// the control's start alone: Legion's prompt, no arm of the experiment in legion.arms, no root in
// the arguments or the environment, and the install as it is today, wrapping the agent's command
// around the installed pair. The daemon is told installs-plugins, once.
func TestALaunchThatInstallsThePluginsReadsLegionsTextsOnly(t *testing.T) { runRowInstalls(t) }

func runRowInstalls(t *testing.T) runtime.TextsReport {
	t.Helper()
	reports := &textsReports{}
	g := textsRig(t, reports, nil, withChecksTexts())
	first := g.spawn(workerSpec(t))
	spec := textsLaunch(t, workerSpec(t))
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
	spec.Plugins = &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	before := len(g.sent(spec.Claim))
	loc, err := g.r.Resume(g.ctx, &first, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if loc.Sandbox.PodUID != first.Sandbox.PodUID {
		t.Fatalf("the launch replaced the pod %s with %s, want it kept: a kept pod is the installing launch", first.Sandbox.PodUID, loc.Sandbox.PodUID)
	}
	starts := startsSent(g, spec.Claim, before)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-2"}) {
		t.Fatalf("the launch sent the starts %q, want start-2 alone", ids)
	}
	start := starts[0]
	if i := slices.Index(start.Argv, "--"); i < 0 || i+1 >= len(start.Argv) || start.Argv[i+1] != "/bin/sh" {
		t.Errorf("the start's command %q carries no install wrapper", start.Argv)
	}
	wantTheControlsStart(t, spec, start, installedRoots(spec.Role, spec.Generation))
	return wantOneReport(t, reports, spec, texts.Fault{Cause: texts.InstallsPlugins})
}

// A launch that installs the pair into a pod it replaces keeps its folder: the replacement comes up
// on the configured image, whose plugins are already current, so the launch installs nothing
// (Plugins is cleared once the replacement is decided), and its start is the folder's, the root
// first, LEGION_TEXTS_ROOT, the layer last and the arm in legion.arms, and no one is told of a
// fallback. A suspended Sandbox is the replacement's reason here.
func TestAReplacingLaunchKeepsTheFolder(t *testing.T) {
	reports := &textsReports{}
	name := SandboxName(workerToken)
	suspended := sandboxObject(t, name, "uid-sandbox-kept", modeSuspended, claimLabels(claim.RoleTester))
	g := textsRig(t, reports, []k8sruntime.Object{suspended}, withChecksTexts())
	g.launcher(workerToken)
	spec := textsLaunch(t, workerSpec(t))
	spec.ResumeSessionFile = resumeSession
	spec.Plugins = &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	if _, err := g.r.Resume(g.ctx, nil, spec); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	starts := startsSent(g, workerToken, 0)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-1"}) {
		t.Fatalf("the launch sent the starts %q, want start-1 alone", ids)
	}
	if i := slices.Index(starts[0].Argv, "--"); i < 0 || i+1 >= len(starts[0].Argv) || starts[0].Argv[i+1] == "/bin/sh" {
		t.Errorf("the replacing launch's command %q installs the plugins, want a plain agent command", starts[0].Argv)
	}
	wantTheFolderStart(t, spec, starts[0], roleStateHome(spec.Role)+"/legion-texts/g1")
	if told := reports.told(); len(told) != 0 {
		t.Errorf("the runtime told the daemon of %+v, want no fallback", told)
	}
}

// refusingFolders is a launcher that refuses a folder it is handed as refusal says, having written
// nothing, and starts every start that hands it none.
func refusingFolders(refusal shimwire.TextsRefusal) func(shimwire.LauncherStart) *shimwire.LauncherStartResult {
	return func(start shimwire.LauncherStart) *shimwire.LauncherStartResult {
		if start.Texts == nil {
			return nil
		}
		return &shimwire.LauncherStartResult{
			ID: start.ID, Texts: &refusal,
			Error: "texts folder " + start.Texts.Folder + ": " + refusal.Path + ": " + refusal.Detail + " [" + refusal.Cause + "]",
		}
	}
}

// refusingEveryStart is a launcher that refuses the folder as refusal says and refuses every other
// start as well, over something of the pod's own.
func refusingEveryStart(refusal shimwire.TextsRefusal) func(shimwire.LauncherStart) *shimwire.LauncherStartResult {
	folders := refusingFolders(refusal)
	return func(start shimwire.LauncherStart) *shimwire.LauncherStartResult {
		if result := folders(start); result != nil {
			return result
		}
		return &shimwire.LauncherStartResult{ID: start.ID, Error: "write the generation's directory: the pod's volume is read-only"}
	}
}

// A launcher that refuses the folder over a file its check found at fault wrote nothing of the
// generation, so the runtime sends the control's start in the same launch, under the id
// `start-<generation>-texts-fallback` (the launcher remembers the refusal under the first id): Legion's
// prompt, no arm of the experiment in legion.arms, no root in the arguments or the environment, none of
// the arm's overlays, and the daemon is told once, with the file and the cause the launcher gave. The
// launch is no failed launch: no start is stopped, and through the supervisor the claim is charged
// nothing.
func TestAStartTheLauncherRefusesForTextsIsSentAgainAsTheControls(t *testing.T) {
	refusal := shimwire.TextsRefusal{Path: "agents/goal-red-team.md", Cause: string(texts.DigestDiffers), Detail: "recorded sha256:00, the plugin's file is sha256:11"}
	t.Run("the two starts", func(t *testing.T) { runRowRefused(t, refusal) })
	t.Run("through the supervisor the claim is charged nothing", func(t *testing.T) {
		reports := &textsReports{}
		g := textsRig(t, reports, nil, withChecksTexts(), withStartAnswer(refusingFolders(refusal)))
		spec := textsLaunch(t, workerSpec(t))
		machine, _, _ := supervisedSandboxMachine(t, g, spec)
		g.launcher(spec.Claim)
		if err := machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}); err != nil {
			t.Fatalf("the launch: %v", err)
		}
		if got := machine.Claim(); got.Budgets.LaunchFailures != 0 || got.Generation != 1 || got.Locator == nil {
			t.Errorf("claim after the launch = %s at generation %d with %d launch failures and locator %+v, want generation 1, no failure and a process",
				got.State, got.Generation, got.Budgets.LaunchFailures, got.Locator)
		}
		if ids := startIDs(startsSent(g, spec.Claim, 0)); !slices.Equal(ids, []string{"start-1", "start-1-texts-fallback"}) {
			t.Errorf("the launch sent the starts %q, want start-1 and start-1-texts-fallback", ids)
		}
	})
}

func runRowRefused(t *testing.T, refusal shimwire.TextsRefusal) runtime.TextsReport {
	t.Helper()
	reports := &textsReports{}
	g := textsRig(t, reports, nil, withChecksTexts(), withStartAnswer(refusingFolders(refusal)))
	spec := textsLaunch(t, workerSpec(t))
	loc := g.spawn(spec)
	if loc.Sandbox.Generation != 1 {
		t.Errorf("the launch is at %+v, want generation 1", loc.Sandbox)
	}
	starts := startsSent(g, spec.Claim, 0)
	if ids := startIDs(starts); !slices.Equal(ids, []string{"start-1", "start-1-texts-fallback"}) {
		t.Fatalf("the launch sent the starts %q, want the folder's start-1 and then start-1-texts-fallback", ids)
	}
	wantTheFolderStart(t, spec, starts[0], roleStateHome(spec.Role)+"/legion-texts/g1")
	wantTheControlsStart(t, spec, starts[1], []string{envoyPlugin, legionPlugin})
	if stops := stopsSent(g, spec.Claim); stops != 0 {
		t.Errorf("the launch stopped %d generations, want none: the refused start began nothing", stops)
	}
	return wantOneReport(t, reports, spec, texts.Fault{Path: refusal.Path, Cause: texts.Cause(refusal.Cause), Detail: refusal.Detail})
}

// The control's start is a start like any other: a launcher that refuses it fails the launch, a
// failure the supervisor charges as it charges any refused start, and the daemon was told once of
// the folder's refusal in the try. Through the supervisor each of the three tries is charged one
// launch failure and sends both starts, so the claim fails with the budget spent; the second start
// is no way round the budget.
func TestAFallbackStartThatIsRefusedIsCharged(t *testing.T) {
	refusal := shimwire.TextsRefusal{Path: "skills/legion-worker/SKILL.md", Cause: string(texts.NameDiffers), Detail: "its frontmatter names legion-worke"}
	t.Run("the launch's error", func(t *testing.T) {
		reports := &textsReports{}
		g := textsRig(t, reports, nil, withChecksTexts(), withStartAnswer(refusingEveryStart(refusal)))
		spec := textsLaunch(t, workerSpec(t))
		g.launcher(spec.Claim)
		_, err := g.r.Spawn(g.ctx, spec)
		if err == nil || !strings.Contains(err.Error(), "start through its authenticated launcher") {
			t.Fatalf("Spawn = %v, want the control's refused start as the launch's failure", err)
		}
		var refused startRefusal
		if !errors.As(err, &refused) || refused.texts != nil || refused.generation != 1 || !strings.Contains(refused.reason, "read-only") {
			t.Errorf("Spawn = %v, want the refusal of the control's start (generation 1, no texts refusal, the pod's own reason)", err)
		}
		var tooLarge *runtime.PromptTooLarge
		if errors.As(err, &tooLarge) || errors.Is(err, runtime.ErrAwaitingPodRound) || errors.Is(err, runtime.ErrSessionLookupUnavailable) ||
			errors.Is(err, runtime.ErrSessionFileMissing) || errors.Is(err, runtime.ErrSessionStore) {
			t.Errorf("Spawn = %v, an error the supervisor charges nothing for", err)
		}
		if ids := startIDs(startsSent(g, spec.Claim, 0)); !slices.Equal(ids, []string{"start-1", "start-1-texts-fallback"}) {
			t.Errorf("the launch sent the starts %q, want start-1 and start-1-texts-fallback", ids)
		}
		wantOneReport(t, reports, spec, texts.Fault{Path: refusal.Path, Cause: texts.Cause(refusal.Cause), Detail: refusal.Detail})
		if recorded, found := g.r.recorded(spec.Claim); found {
			t.Errorf("the runtime records %s after the failed launch", recorded.Incarnation)
		}
	})
	t.Run("through the supervisor each try is charged", func(t *testing.T) {
		reports := &textsReports{}
		g := textsRig(t, reports, nil, withChecksTexts(), withStartAnswer(refusingEveryStart(refusal)))
		spec := textsLaunch(t, workerSpec(t))
		machine, _, _ := supervisedSandboxMachine(t, g, spec)
		g.launcher(spec.Claim)
		if err := machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}); err == nil {
			t.Fatal("the launch succeeded, want the refused control's start to fail it")
		}
		if got := machine.Claim(); got.State != supervise.StateFailed || got.Budgets.LaunchFailures != 3 || got.Generation != 3 || got.Locator != nil {
			t.Errorf("claim after the launch = %s at generation %d with %d launch failures and locator %+v, want failed at generation 3 with 3 and no process",
				got.State, got.Generation, got.Budgets.LaunchFailures, got.Locator)
		}
		want := []string{"start-1", "start-1-texts-fallback", "start-2", "start-2-texts-fallback", "start-3", "start-3-texts-fallback"}
		if ids := startIDs(startsSent(g, spec.Claim, 0)); !slices.Equal(ids, want) {
			t.Errorf("the three tries sent the starts %q, want %q", ids, want)
		}
	})
}

// A folder under an operator's volume mount reaches the container every issue role runs in: the
// roles container mounts the operator's volume at the mount's path, and the folder the start hands
// the launcher lies under a mount of that container, as the launcher itself sees the container: a
// mount at or under the image's home is held under HomeMountsDir, and bound back at its own path
// only in each role's namespace, which shows the role the folder where the arm names it. A folder
// under no mount is held by none, so the check below would fail it (the negative control).
func TestTheFolderUnderAnOperatorMountReachesTheRolesContainer(t *testing.T) {
	volume := corev1.Volume{Name: "operator-texts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-texts"},
	}}}
	for name, tc := range map[string]struct{ mount, folder, seen string }{
		"outside the image's home": {textsMount, textsFolderPath, textsFolderPath},
		"under the image's home":   {podHome + "/texts", podHome + "/texts/shorter", HomeMountsDir + "/texts/shorter"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runtimeWithOptions(t, func(o *Options) {
				o.Pod.Volumes = append(o.Pod.Volumes, volume)
				o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: volume.Name, MountPath: tc.mount, ReadOnly: true})
			})
			for _, role := range claim.Roles {
				spec := textsLaunch(t, baselineSpec(t, role))
				spec.Variants[0].Texts, spec.Texts.Folder = tc.folder, tc.folder
				l := launchOf(t, r, spec)
				pod := r.podTemplate(l).Spec
				container := containerNamed(t, pod, l.kind.container())
				start := launcherCommand(l, r)
				if start.Texts == nil || start.Texts.Folder != tc.seen {
					t.Fatalf("the %s's start hands the launcher %+v, want the folder at %s as the launcher sees it", role, start.Texts, tc.seen)
				}
				mount := holderMount(container.VolumeMounts, start.Texts.Folder)
				want := corev1.VolumeMount{Name: volume.Name, MountPath: containerHomePath(tc.mount), ReadOnly: true}
				if mount == nil || !reflect.DeepEqual(*mount, want) {
					t.Errorf("the %s's container holds the folder %s under the mount %+v, want %+v", role, start.Texts.Folder, mount, want)
				}
				if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == volume.Name }) {
					t.Errorf("the pod holds no volume %s, which the container mounts", volume.Name)
				}
				seen, ok := roleMounts(pod, role)
				if held := holderMount(seen, tc.folder); !ok || held == nil || held.Name != volume.Name || held.MountPath != tc.mount {
					t.Errorf("the %s sees the folder %s under the mount %+v, want the volume %s at %s", role, tc.folder, held, volume.Name, tc.mount)
				}
			}
		})
	}
	t.Run("negative control: a folder under no mount is held by none", func(t *testing.T) {
		r := runtimeWithOptions(t, func(*Options) {})
		spec := textsLaunch(t, baselineSpec(t, claim.RoleTester))
		l := launchOf(t, r, spec)
		container := containerNamed(t, r.podTemplate(l).Spec, l.kind.container())
		if mount := holderMount(container.VolumeMounts, launcherCommand(l, r).Texts.Folder); mount != nil {
			t.Errorf("a folder no operator mount holds is held by %+v", mount)
		}
	})
}

// Every cause a launch of an arm's folder can fall back for is found on one side of it, and each
// has its row in the Kubernetes launch's table here: the daemon's own check (foundByTheDaemon), whose
// causes leave the runtime nothing to do but send the control's one start
// (TestADaemonTextsFaultSendsOnlyTheControlsStart); the pod's launcher (foundByTheLauncher), whose
// refusal over each is answered by the control's second start, which a row below runs for each;
// and the runtime's own state (foundByTheRuntime): a launcher that cannot check, and a launch that
// installs the plugins. The causes the tmux runtime finds are the launcher's list (it runs the same
// checks in the daemon's process), and its rows are runtime/tmux's own. A cause on no side has no
// row, so it fails here: the negative control names one.
func TestEveryLauncherCauseHasARow(t *testing.T) {
	t.Run("every cause is found on exactly one side", func(t *testing.T) {
		for _, cause := range texts.Causes {
			if _, err := sideOf(cause); err != nil {
				t.Error(err)
			}
		}
	})
	t.Run("negative control: a cause no side finds fails the inventory", func(t *testing.T) {
		if _, err := sideOf("an-invented-cause"); err == nil {
			t.Error("the inventory accepted a cause no side finds")
		}
	})
	for _, cause := range foundByTheLauncher {
		t.Run("the launcher refuses over "+string(cause), func(t *testing.T) {
			path := "agents/goal-red-team.md"
			if slices.Contains(foundByTheLauncherForTheFolder, cause) {
				path = ""
			}
			report := runRowRefused(t, shimwire.TextsRefusal{Path: path, Cause: string(cause), Detail: "what the check saw"})
			if report.Fault.Cause != cause || report.Fault.Path != path {
				t.Errorf("the daemon was told %+v, want the cause %s at %q", report.Fault, cause, path)
			}
		})
	}
	t.Run("the launcher cannot check", func(t *testing.T) {
		if report := runRowCannotCheck(t); report.Fault.Cause != texts.LauncherCannotCheck {
			t.Errorf("the daemon was told %+v, want %s", report.Fault, texts.LauncherCannotCheck)
		}
	})
	t.Run("the launch installs the plugins", func(t *testing.T) {
		if report := runRowInstalls(t); report.Fault.Cause != texts.InstallsPlugins {
			t.Errorf("the daemon was told %+v, want %s", report.Fault, texts.InstallsPlugins)
		}
	})
}

var (
	foundByTheDaemon   = []texts.Cause{texts.Unread, texts.UnknownAgent, texts.UnknownSkill, texts.NamesUnread, texts.TwoFolders}
	foundByTheLauncher = []texts.Cause{
		texts.Gone, texts.Unreadable, texts.NoManifest, texts.ManifestUnreadable, texts.Unlisted, texts.Missing, texts.BadPath,
		texts.LinkEscapes, texts.DigestDiffers, texts.NoLegionText, texts.NameDiffers, texts.Changed,
	}
	foundByTheRuntime = []texts.Cause{texts.LauncherCannotCheck, texts.InstallsPlugins}
	// foundByTheLauncherForTheFolder are the launcher's causes that name the folder as a whole and
	// no file of it.
	foundByTheLauncherForTheFolder = []texts.Cause{texts.Gone, texts.NoManifest, texts.ManifestUnreadable, texts.Changed}
)

// sideOf is the side that finds cause, "" for none, and an error for a cause two sides find.
func sideOf(cause texts.Cause) (string, error) {
	var found []string
	for side, causes := range map[string][]texts.Cause{
		"the daemon": foundByTheDaemon, "the launcher": foundByTheLauncher, "the runtime": foundByTheRuntime,
	} {
		if slices.Contains(causes, cause) {
			found = append(found, side)
		}
	}
	slices.Sort(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("cause %s is found on no side", cause)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("cause %s is found on %d sides: %v", cause, len(found), found)
}

// LEGION_TEXTS_ROOT names the root the pod's launcher built from a folder it checked, so only the
// runtime tells it: a spec naming it is refused before anything of the pod is touched, and so is an
// operator's pod env, which would point the plugin at texts no check read. It is in the owned set
// every launch spec is held to, and the launch that runs a folder is told it
// (TestATextsLaunchCarriesItsFolderToTheLauncher).
func TestNoSpecAndNoOperatorNamesTheTextsRoot(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !r.specOwned()[texts.RootVariable] {
		t.Errorf("the names a spec may not set are %v, want %s among them", slices.Sorted(maps.Keys(r.specOwned())), texts.RootVariable)
	}
	for name, armed := range map[string]bool{"a launch of no folder": false, "a launch of a folder": true} {
		spec := workerSpec(t)
		if armed {
			spec = textsLaunch(t, spec)
		}
		spec.Env[texts.RootVariable] = "/etc/elsewhere/legion-texts/g1"
		if _, err := r.prepare(spec); err == nil || !strings.Contains(err.Error(), "Env sets "+texts.RootVariable+", which the runtime sets itself") {
			t.Errorf("prepare of %s whose Env names %s = %v, want the refusal", name, texts.RootVariable, err)
		}
	}
	opts := testOptions()
	opts.Pod = Pod{Env: map[string]string{texts.RootVariable: "/etc/elsewhere/legion-texts/g1"}}
	if _, err := configure(opts); err == nil || !strings.Contains(err.Error(), "runtime.kubernetes.pod.env sets "+texts.RootVariable) {
		t.Errorf("configure with a pod env naming %s = %v, want the refusal", texts.RootVariable, err)
	}
}
