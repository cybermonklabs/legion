package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// An experiment arm's folder of texts reaches the agent as one extension root that
// Oh My Pi loads before the plugins a pane has installed: `--extension <root>` ahead of
// `--model @default`. Oh My Pi gives an explicit root the bare name of a skill both it and a
// plugin ship, and the plugin's differing copy an alias, so a root named after the plugins (or not
// at all) would make the plugin's copy the one the model reads. The order is the observable of the
// requirement, so it is held at each place the command line is built: innerCommand, and the
// launcher the daemon's live proofs call in place of a tmux pane (NewOmpLauncher's Launch, whose
// inner command and pairs a local runtime runs Oh My Pi under). The installed plugins appear in no
// flag, so the root is the one `--extension` of the command. The last subtest is the negative
// control: the same check, run on a command with the root after the model flag, on one with no
// root and on one with another root, fails, so a check that passed whatever the command said would
// show here.
func TestATextsLaunchPutsItsRootBeforeTheInstalledPlugins(t *testing.T) {
	const omp = "/opt/omp/bin/omp"
	const promptFile = "/state/secrets/legion-omp-legion-43-tester-system-prompt.md"

	t.Run("innerCommand", func(t *testing.T) {
		root := "/state/texts/legion-omp-legion-43-tester/1/root"
		for resume, want := range map[string]string{
			"":                              omp + " --extension " + root + " --model @default --mode rpc --append-system-prompt " + promptFile,
			"/state/trees/x/.omp/s 1.jsonl": omp + " --extension " + root + " --model @default --resume='/state/trees/x/.omp/s 1.jsonl' --mode rpc --append-system-prompt " + promptFile,
		} {
			got := innerCommand(nil, omp, resume, promptFile, root)
			if got != want {
				t.Errorf("innerCommand with a root (resume %q) = %q, want %q", resume, got, want)
			}
			if err := rootBeforeTheModel(got, root); err != nil {
				t.Errorf("innerCommand with a root (resume %q): %v", resume, err)
			}
		}
		// A launch with no root is the command it always was.
		if got, want := innerCommand(nil, omp, "", promptFile), omp+" --model @default --mode rpc --append-system-prompt "+promptFile; got != want {
			t.Errorf("innerCommand with no root = %q, want %q", got, want)
		}
		// Behind an operator's launch prefix the root still follows the invocation and leads the
		// flags.
		got := innerCommand([]string{"env", "K=v", "--"}, omp, "", promptFile, root)
		if want := "env 'K=v' -- " + omp + " --extension " + root + " --model @default --mode rpc --append-system-prompt " + promptFile; got != want {
			t.Errorf("innerCommand behind a prefix = %q, want %q", got, want)
		}
	})

	t.Run("the launcher a local runtime runs Oh My Pi by", func(t *testing.T) {
		spec, host, root := textsLaunch(t)
		inner, _, err := NewOmpLauncher(nil, omp).Launch(spec, host, promptFile)
		if err != nil {
			t.Fatalf("Launch of a spec carrying a folder of texts = %v, want the command", err)
		}
		if err := rootBeforeTheModel(inner, root); err != nil {
			t.Errorf("Launch's command %q: %v", inner, err)
		}
		if _, err := os.Stat(filepath.Join(root, "agents", "goal-red-team.md")); err != nil {
			t.Errorf("the command names the root %s, but the launch built none holding the folder's agent: %v", root, err)
		}
		// The launch of no folder names no root: the command is innerCommand's own.
		plain := spec
		plain.Texts, plain.Variants = nil, nil
		control, _, err := NewOmpLauncher(nil, omp).Launch(plain, host, promptFile)
		if err != nil {
			t.Fatal(err)
		}
		if want := innerCommand(nil, omp, "", promptFile); control != want {
			t.Errorf("Launch of a spec with no folder = %q, want %q", control, want)
		}
	})

	t.Run("negative control: the order check fails a command that breaks the order", func(t *testing.T) {
		const root = "/state/texts/legion-omp-legion-43-tester/1/root"
		for name, command := range map[string]string{
			"the root after the model flag":  omp + " --model @default --extension " + root + " --mode rpc --append-system-prompt " + promptFile,
			"the root after every flag":      omp + " --model @default --mode rpc --append-system-prompt " + promptFile + " --extension " + root,
			"no root":                        omp + " --model @default --mode rpc --append-system-prompt " + promptFile,
			"another root":                   omp + " --extension /opt/legion/pi-legion --model @default --mode rpc --append-system-prompt " + promptFile,
			"the root and a plugin's root":   omp + " --extension " + root + " --extension /opt/legion/pi-legion --model @default --mode rpc",
			"the root with no model flag":    omp + " --extension " + root + " --mode rpc --append-system-prompt " + promptFile,
			"an --extension with no value":   omp + " --model @default --extension",
			"a model flag before a plugin's": omp + " --model @default --extension " + root + " --extension /opt/legion/pi-envoy",
		} {
			if err := rootBeforeTheModel(command, root); err == nil {
				t.Errorf("the order check passed %s: %q", name, command)
			}
		}
		if err := rootBeforeTheModel(omp+" --extension "+root+" --model @default --mode rpc", root); err != nil {
			t.Errorf("the order check failed the command with the root first: %v", err)
		}
	})
}

// rootBeforeTheModel is nil when the Oh My Pi command line names exactly one extension root, root,
// and names it before the model flag: the installed plugins load after the explicit roots, so
// nothing else is named, and the first root Oh My Pi reads is the arm's.
func rootBeforeTheModel(command, root string) error {
	words := strings.Fields(command)
	var roots []string
	rootAt, modelAt := -1, -1
	for i, word := range words {
		switch word {
		case "--extension":
			if i+1 == len(words) {
				return fmt.Errorf("--extension names no root")
			}
			roots = append(roots, words[i+1])
			if words[i+1] == root {
				rootAt = i
			}
		case "--model":
			modelAt = i
		}
	}
	switch {
	case !slices.Equal(roots, []string{root}):
		return fmt.Errorf("the extension roots are %q, want %q alone", roots, root)
	case modelAt < 0:
		return fmt.Errorf("no --model flag, so the root cannot come before it")
	case rootAt > modelAt:
		return fmt.Errorf("--extension %s is at word %d, after --model at word %d", root, rootAt, modelAt)
	}
	return nil
}

// textsLaunch is a spec for the tester's claim that carries a folder of texts, with the state
// directory its launch writes under, the two plugins the pane has installed (each holding the one
// text the folder replaces or none), and the extension root the launch builds beside the daemon's
// copy of the folder: `<state>/texts/<claim>/<n>/root`, the copy being `<n>/folder`. The folder's
// manifest records the digest of the plugin's agent, the Legion text its own agent replaces.
func textsLaunch(t *testing.T) (runtime.SpawnSpec, Host, string) {
	t.Helper()
	dir := t.TempDir()
	state, envoy, legion := filepath.Join(dir, "state"), filepath.Join(dir, "pi-envoy"), filepath.Join(dir, "pi-legion")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agent := "---\nname: goal-red-team\ndescription: Goal red team.\n---\nReads the change against the goal.\n"
	write(filepath.Join(envoy, "dist", "skills", "dispatch-first", "SKILL.md"), "---\nname: dispatch-first\ndescription: Use when a session has the dispatch command.\n---\nSearch first.\n")
	write(filepath.Join(legion, "agents", "goal-red-team.md"), agent)
	write(filepath.Join(legion, "dist", "skills", "legion-worker", "SKILL.md"), "---\nname: legion-worker\ndescription: Use when dispatched as a worker.\n---\nWork.\n")

	spec := testSpec()
	spec.Variants = []runtime.Variant{{Experiment: "shorter-prompts", Name: "texts", Texts: "/etc/legion/texts/shorter"}}
	folder := filepath.Join(state, "texts", string(spec.Claim), "1", "folder")
	digest := sha256.Sum256([]byte(agent))
	write(filepath.Join(folder, "agents", "goal-red-team.md"), "---\nname: goal-red-team\ndescription: Changed by the arm.\n---\nReads the change.\n")
	write(filepath.Join(folder, "manifest.json"), `{"files":{"agents/goal-red-team.md":"sha256:`+hex.EncodeToString(digest[:])+`"}}`)
	spec.Texts = &runtime.Texts{
		Folder:      "/etc/legion/texts/shorter",
		Fingerprint: strings.Repeat("ab", 32),
		Copy:        folder,
		Experiment:  "shorter-prompts",
		Variant:     "texts",
	}
	return spec, Host{StateDir: state, Legion: "/opt/legion/legion", EnvoyPlugin: envoy, LegionPlugin: legion}, filepath.Join(filepath.Dir(folder), "root")
}

// folderPath is the folder the rig's arm names, as the operator wrote it; the daemon's copy of it
// is what a launch reads.
const folderPath = "/etc/legion/texts/shorter"

// The texts the rig's plugins ship and the folder replaces, and the arm's own versions.
const (
	pluginAgent  = "---\nname: goal-red-team\ndescription: Goal red team.\n---\nReads the change against the goal.\n"
	pluginSkill  = "---\nname: legion-worker\ndescription: Use when dispatched as a worker.\n---\nWork.\n"
	armAgent     = "---\nname: goal-red-team\ndescription: Changed by the arm.\n---\nReads the change.\n"
	armSkill     = "---\nname: legion-worker\ndescription: Changed by the arm.\n---\nWork, the arm's way.\n"
	armRolePart  = "You are the arm's tester.\n"
	legionRole   = "You are Legion's tester.\n"
	armNotice    = "Arm instructions."
	controlsRole = "You are the stand-in."
)

// textsRig is a tmux runtime over a profile that has both of Legion's plugins installed, as an
// operator installs them (`omp plugin install`): under `<home>/.omp/plugins/node_modules/@sjawhar`,
// where the runtime finds them over the pane's environment, as the boot gate's pane lane finds
// pi-legion. pi-legion ships two agents and a skill with a reference, pi-envoy dispatch-first.
// The operator's overlay ignores a skill, so a launch has a base the texts layer must keep.
type textsRig struct {
	t                          *testing.T
	state, home                string
	envoy, legion              string
	operator, role, alpha, arm string
}

func rigWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTextsRig(t *testing.T) *textsRig {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "home", ".omp", "plugins", "node_modules", "@sjawhar")
	r := &textsRig{
		t: t, state: filepath.Join(dir, "state"), home: filepath.Join(dir, "home"),
		envoy: filepath.Join(plugins, "pi-envoy"), legion: filepath.Join(plugins, "pi-legion"),
		operator: filepath.Join(dir, "overlays", "operator.yml"), role: filepath.Join(dir, "overlays", "tester.yml"),
		alpha: filepath.Join(dir, "overlays", "alpha.yml"), arm: filepath.Join(dir, "overlays", "arm.yml"),
	}
	if err := os.MkdirAll(r.state, 0o700); err != nil {
		t.Fatal(err)
	}
	rigWrite(t, filepath.Join(r.envoy, "dist", "skills", "dispatch-first", "SKILL.md"), "---\nname: dispatch-first\ndescription: Use when a session has the dispatch command.\n---\nSearch first.\n")
	rigWrite(t, filepath.Join(r.legion, "agents", "goal-red-team.md"), pluginAgent)
	rigWrite(t, filepath.Join(r.legion, "agents", "plan-reviewer.md"), "---\nname: plan-reviewer\ndescription: Plan reviewer.\n---\nReviews the plan.\n")
	rigWrite(t, filepath.Join(r.legion, "dist", "skills", "legion-worker", "SKILL.md"), pluginSkill)
	rigWrite(t, filepath.Join(r.legion, "dist", "skills", "legion-worker", "references", "plan-tests.md"), "Plan tests.\n")
	rigWrite(t, r.operator, "skills:\n  ignoredSkills:\n    - asd-ste100\n")
	rigWrite(t, r.role, "# The tester's own overlay.\n")
	rigWrite(t, r.alpha, "retry:\n  enabled: false\n")
	rigWrite(t, r.arm, "retry:\n  enabled: true\n")
	return r
}

func (r *textsRig) alphaOn() runtime.Variant {
	return runtime.Variant{Experiment: "alpha", Name: "on", ConfigFiles: []string{r.alpha}}
}

func (r *textsRig) folderArm() runtime.Variant {
	return runtime.Variant{Experiment: "shorter", Name: "folder", ConfigFiles: []string{r.arm}, Texts: folderPath}
}

// runtime is a fake server runtime on the rig's state, whose pane runs under the rig's home, with
// the operator's overlay, the tester's own and the two experiments' arms declared. fellBack is its
// Options.TextsFellBack.
func (r *textsRig) runtime(fellBack runtime.TextsFellBack) *fakeServerRuntime {
	r.t.Helper()
	return newFakeServerRuntimeWith(r.t, staticCredential, nil, func(o *Options) {
		o.StateDir = r.state
		o.Environ = append(o.Environ, "HOME="+r.home)
		o.PiConfigFiles = []string{r.operator}
		o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {r.role}}
		o.ExperimentVariants = []runtime.Variant{r.alphaOn(), r.folderArm()}
		o.TextsFellBack = fellBack
	})
}

// specs are the tester's launch of the arm that names a folder, as the daemon builds it (its prompt
// reads the role part of the daemon's copy at copyDir, and ends with the arm's instructions), and
// the control's launch beside it, which is what the arm's fallback runs: Legion's own prompt, and
// the other experiment's arm alone.
func (r *textsRig) specs(t *testing.T, f *fakeServerRuntime) (folder, control runtime.SpawnSpec, copyDir string) {
	t.Helper()
	base := f.spec(t, claim.RoleTester)
	copyDir = filepath.Join(r.state, "texts", string(base.Claim), "2", "folder")
	instructions := filepath.Join(r.state, "arm-instructions.md")
	rigWrite(t, instructions, armNotice+"\n")
	control = base
	control.Variants = []runtime.Variant{r.alphaOn()}
	folder = base
	folder.Variants = []runtime.Variant{r.alphaOn(), r.folderArm()}
	folder.Prompt = runtime.PromptParts{
		RolePromptPaths:          []string{filepath.Join(copyDir, "roles", "tester.md")},
		VariantInstructionsPaths: []string{instructions},
	}
	folder.Texts = &runtime.Texts{
		Folder: folderPath, Fingerprint: strings.Repeat("ab", 32), Copy: copyDir, Experiment: "shorter", Variant: "folder",
		Skills:   []string{"legion-worker"},
		Fallback: runtime.TextsFallback{Prompt: base.Prompt, Variants: control.Variants},
	}
	return folder, control, copyDir
}

// matching are the files of a folder cut from these plugins, and the manifest that records the
// digests of the texts they replace: a role part, the plugin's agent and the plugin's skill.
func (r *textsRig) matching() (files, manifest map[string]string) {
	files = map[string]string{
		"roles/tester.md":               armRolePart,
		"agents/goal-red-team.md":       armAgent,
		"skills/legion-worker/SKILL.md": armSkill,
	}
	manifest = map[string]string{
		"roles/tester.md":               texts.Digest([]byte(legionRole)),
		"agents/goal-red-team.md":       texts.Digest([]byte(pluginAgent)),
		"skills/legion-worker/SKILL.md": texts.Digest([]byte(pluginSkill)),
	}
	return files, manifest
}

// writeFolder writes files and their manifest as the daemon's copy at dir.
func (r *textsRig) writeFolder(t *testing.T, dir string, files, manifest map[string]string) {
	t.Helper()
	for p, content := range files {
		rigWrite(t, filepath.Join(dir, filepath.FromSlash(p)), content)
	}
	raw, err := json.Marshal(map[string]map[string]string{"files": manifest})
	if err != nil {
		t.Fatal(err)
	}
	rigWrite(t, filepath.Join(dir, texts.ManifestName), string(raw))
}

// pairNamed is the value of the one pair of the pane named name, and how many pairs name it.
func pairNamed(pairs []string, name string) (value string, count int) {
	for _, pair := range pairs {
		if got, v, ok := strings.Cut(pair, "="); ok && got == name {
			value, count = v, count+1
		}
	}
	return value, count
}

// A launch of a folder whose texts pass every check runs Oh My Pi with the extension root built
// beside the daemon's copy of the folder: the pane's command names it once, before `--model`; the
// pane is told it as LEGION_TEXTS_ROOT, once; and PI_CONFIG_FILES ends with the root's settings
// layer, after the operator's overlay, the role's and each arm's, with the one variable naming them
// all. The layer keeps the operator's ignored skill and adds the collision names of the skill the
// folder replaces. The root holds every pi-legion agent and every pi-envoy skill with the folder's
// files over them and no role part, a root an earlier launch of the generation left is replaced,
// the prompt is the folder's role part and the arm's instructions, and the arm is named in the
// spend attributes. The daemon is told of no fallback.
func TestATextsLaunchNamesItsRootAndLayerInThePane(t *testing.T) {
	rig := newTextsRig(t)
	var reports []runtime.TextsReport
	f := rig.runtime(func(report runtime.TextsReport) { reports = append(reports, report) })
	spec, _, copyDir := rig.specs(t, f)
	files, manifest := rig.matching()
	rig.writeFolder(t, copyDir, files, manifest)
	root := filepath.Join(filepath.Dir(copyDir), "root")
	rigWrite(t, filepath.Join(root, "agents", "left-by-an-earlier-launch.md"), "stale\n")

	argv, pairs := newWindow(t, f, spec)

	if value, count := pairNamed(pairs, "LEGION_TEXTS_ROOT"); count != 1 || value != root {
		t.Errorf("the pane is told LEGION_TEXTS_ROOT %d times, as %q, want once, as %q", count, value, root)
	}
	layer := filepath.Join(root, "texts-settings.yml")
	if value, count := pairNamed(pairs, "PI_CONFIG_FILES"); count != 1 || value != strings.Join([]string{rig.operator, rig.role, rig.alpha, rig.arm, layer}, ":") {
		t.Errorf("the pane is told PI_CONFIG_FILES %d times, as %q, want once, the operator's overlay, the role's, each arm's and the texts layer last", count, value)
	}
	if value, _ := pairNamed(pairs, runtime.SpendAttributesVariable); !strings.Contains(value, runtime.SpendArmsAttribute+"=alpha%3Aon%2Cshorter%3Afolder") {
		t.Errorf("%s = %q, want the arms alpha:on and shorter:folder named", runtime.SpendAttributesVariable, value)
	}
	if err := rootBeforeTheModel(argv[len(argv)-1], root); err != nil {
		t.Errorf("the pane's command %q: %v", argv[len(argv)-1], err)
	}

	raw, err := os.ReadFile(layer)
	if err != nil {
		t.Fatalf("the launch wrote no settings layer: %v", err)
	}
	var settings struct {
		Skills struct {
			IgnoredSkills []string `yaml:"ignoredSkills"`
		} `yaml:"skills"`
	}
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if want := []string{"asd-ste100", "*/legion-worker", "*/legion-worker~*"}; !slices.Equal(settings.Skills.IgnoredSkills, want) {
		t.Errorf("the layer ignores %q, want the operator's skill and the collision names of the replaced skill %q", settings.Skills.IgnoredSkills, want)
	}

	for p, want := range map[string]string{
		"agents/goal-red-team.md":                       armAgent,
		"agents/plan-reviewer.md":                       "---\nname: plan-reviewer\ndescription: Plan reviewer.\n---\nReviews the plan.\n",
		"skills/legion-worker/SKILL.md":                 armSkill,
		"skills/legion-worker/references/plan-tests.md": "Plan tests.\n",
		"skills/dispatch-first/SKILL.md":                "---\nname: dispatch-first\ndescription: Use when a session has the dispatch command.\n---\nSearch first.\n",
	} {
		if got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err != nil || string(got) != want {
			t.Errorf("the root's %s = %q, %v; want %q", p, got, err, want)
		}
	}
	for _, gone := range []string{"agents/left-by-an-earlier-launch.md", "roles", "go"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the root holds %s (%v), want none: an earlier launch's file is replaced and no role part sits in a directory Oh My Pi scans", gone, err)
		}
	}

	if got, want := panePrompt(t, claim.RoleTester, argv), strings.TrimSuffix(armRolePart, "\n")+"\n\n"+armNotice; got != want {
		t.Errorf("the pane's prompt = %q, want the folder's role part and the arm's instructions %q", got, want)
	}
	if len(reports) != 0 {
		t.Errorf("the daemon was told of %d fallbacks for a folder that passed every check: %+v", len(reports), reports)
	}
}

// A folder whose texts fail the runtime's check against the plugins the pane has installed is not
// launched: the pane is the control's, byte for byte, as a launch of no arm of that experiment —
// the control's prompt, the same argv and -e pairs — with no `--extension`, no LEGION_TEXTS_ROOT,
// no texts layer, and no arm of the folder in the spend attributes, and no root is built. The
// daemon is told once, naming the experiment, the arm, the folder, the file and the cause. The rows
// are each fault the check names: an agent and a skill at another digest, one whose frontmatter
// name differs from its path, a path no plugin ships, a plugin that is not installed or cannot be
// read, and a copy that is gone.
func TestAStalePluginTextLaunchesTheControlsPane(t *testing.T) {
	for _, row := range []struct {
		name   string
		edit   func(t *testing.T, rig *textsRig, files, manifest map[string]string)
		noCopy bool
		file   string
		cause  texts.Cause
	}{
		{name: "an agent at another digest", file: "agents/goal-red-team.md", cause: texts.DigestDiffers,
			edit: func(_ *testing.T, _ *textsRig, _, manifest map[string]string) {
				manifest["agents/goal-red-team.md"] = texts.Digest([]byte("an earlier version of the agent\n"))
			}},
		{name: "a skill at another digest", file: "skills/legion-worker/SKILL.md", cause: texts.DigestDiffers,
			edit: func(_ *testing.T, _ *textsRig, _, manifest map[string]string) {
				manifest["skills/legion-worker/SKILL.md"] = texts.Digest([]byte("an earlier version of the skill\n"))
			}},
		{name: "an agent whose frontmatter name differs from its path", file: "agents/goal-red-team.md", cause: texts.NameDiffers,
			edit: func(_ *testing.T, _ *textsRig, files, _ map[string]string) {
				files["agents/goal-red-team.md"] = strings.Replace(armAgent, "name: goal-red-team", "name: goal-red-teem", 1)
			}},
		{name: "a skill whose frontmatter name differs from its path", file: "skills/legion-worker/SKILL.md", cause: texts.NameDiffers,
			edit: func(_ *testing.T, _ *textsRig, files, _ map[string]string) {
				files["skills/legion-worker/SKILL.md"] = strings.Replace(armSkill, "name: legion-worker", "name: legion-workr", 1)
			}},
		{name: "an agent no plugin ships", file: "agents/nothing.md", cause: texts.NoLegionText,
			edit: func(_ *testing.T, _ *textsRig, files, manifest map[string]string) {
				files["agents/nothing.md"] = "---\nname: nothing\ndescription: None.\n---\nNothing.\n"
				manifest["agents/nothing.md"] = texts.Digest([]byte("no such agent\n"))
			}},
		{name: "a skill no plugin ships", file: "skills/legion-workr/SKILL.md", cause: texts.NoLegionText,
			edit: func(_ *testing.T, _ *textsRig, files, manifest map[string]string) {
				files["skills/legion-workr/SKILL.md"] = "---\nname: legion-workr\ndescription: None.\n---\nNothing.\n"
				manifest["skills/legion-workr/SKILL.md"] = texts.Digest([]byte("no such skill\n"))
			}},
		{name: "a reference no plugin skill ships", file: "skills/legion-worker/references/nothing.md", cause: texts.NoLegionText,
			edit: func(_ *testing.T, _ *textsRig, files, manifest map[string]string) {
				files["skills/legion-worker/references/nothing.md"] = "Nothing.\n"
				manifest["skills/legion-worker/references/nothing.md"] = texts.Digest([]byte("no such reference\n"))
			}},
		{name: "pi-legion is not installed", cause: texts.Unreadable,
			edit: func(t *testing.T, rig *textsRig, _, _ map[string]string) {
				if err := os.RemoveAll(rig.legion); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "pi-envoy is not installed", cause: texts.Unreadable,
			edit: func(t *testing.T, rig *textsRig, _, _ map[string]string) {
				if err := os.RemoveAll(rig.envoy); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "pi-legion's agents cannot be read", cause: texts.Unreadable,
			edit: func(t *testing.T, rig *textsRig, _, _ map[string]string) {
				if os.Geteuid() == 0 {
					t.Skip("running as root, which reads a directory of mode 000: this row needs a user it can be refused")
				}
				agents := filepath.Join(rig.legion, "agents")
				if err := os.Chmod(agents, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(agents, 0o755) })
			}},
		{name: "the daemon's copy of the folder is gone", noCopy: true, cause: texts.Gone},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newTextsRig(t)
			var reports []runtime.TextsReport
			f := rig.runtime(func(report runtime.TextsReport) { reports = append(reports, report) })
			folder, control, copyDir := rig.specs(t, f)
			files, manifest := rig.matching()
			if row.edit != nil {
				row.edit(t, rig, files, manifest)
			}
			if !row.noCopy {
				rig.writeFolder(t, copyDir, files, manifest)
			}

			// Both launches write the claim's one prompt file, so each is read before the next.
			got, gotPairs := newWindow(t, f, folder)
			gotPrompt := panePrompt(t, claim.RoleTester, got)
			want, wantPairs := newWindow(t, rig.runtime(nil), control)
			wantPrompt := panePrompt(t, claim.RoleTester, want)

			if !slices.Equal(got, want) {
				t.Errorf("the fallback pane =\n%q\nwant the control's:\n%q", got, want)
			}
			if !slices.Equal(gotPairs, wantPairs) {
				t.Errorf("the fallback pane's pairs =\n%q\nwant the control's:\n%q", gotPairs, wantPairs)
			}
			if gotPrompt != wantPrompt || gotPrompt != controlsRole {
				t.Errorf("the fallback's prompt = %q, the control's = %q, want Legion's own %q", gotPrompt, wantPrompt, controlsRole)
			}
			if strings.Contains(gotPrompt, armNotice) || strings.Contains(gotPrompt, "arm's tester") {
				t.Errorf("the fallback's prompt names the arm: %q", gotPrompt)
			}
			for _, word := range got {
				if strings.Contains(word, "--extension") || strings.Contains(word, texts.SettingsLayerName) || strings.Contains(word, "shorter") {
					t.Errorf("the fallback pane names the arm's folder in %q", word)
				}
			}
			if _, count := pairNamed(gotPairs, texts.RootVariable); count != 0 {
				t.Errorf("the fallback pane is told %s", texts.RootVariable)
			}
			if value, _ := pairNamed(gotPairs, "PI_CONFIG_FILES"); value != strings.Join([]string{rig.operator, rig.role, rig.alpha}, ":") {
				t.Errorf("the fallback's PI_CONFIG_FILES = %q, want the operator's overlay, the role's and the other experiment's arm alone", value)
			}
			if value, _ := pairNamed(gotPairs, runtime.SpendAttributesVariable); !strings.Contains(value, runtime.SpendArmsAttribute+"=alpha%3Aon") || strings.Contains(value, "shorter") {
				t.Errorf("%s = %q, want the arm alpha:on alone, and no arm of the folder", runtime.SpendAttributesVariable, value)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(copyDir), "root")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a fallback built an extension root (%v)", err)
			}

			if len(reports) != 1 {
				t.Fatalf("the daemon was told of %d fallbacks, want one: %+v", len(reports), reports)
			}
			report := reports[0]
			fault := report.Fault
			if report.Claim != folder.Claim || report.Issue != folder.Issue || report.Role != folder.Role || report.Generation != folder.Generation ||
				report.Experiment != "shorter" || report.Variant != "folder" || report.Folder != folderPath || report.Fingerprint != folder.Texts.Fingerprint {
				t.Errorf("the report names %+v, want the launch %s of experiment shorter, arm folder, folder %s", report, folder.Claim, folderPath)
			}
			if fault.Path != row.file || fault.Cause != row.cause || fault.Dir != folderPath {
				t.Errorf("the report's fault is %+v, want the folder %s, file %q and cause %s", fault, folderPath, row.file, row.cause)
			}
		})
	}
}

// A launch of no folder is the launch it always was, whatever plugins the pane has installed: the
// command is innerCommand's own with no extension root, the pane is told neither
// LEGION_TEXTS_ROOT nor a texts layer, no root or copy is made under the state directory, and the
// bytes of every role's launch under a deployment with no experiment are the goldens
// (TestLaunchBaselineOfADeploymentWithNoExperiment).
func TestTheLaunchOfNoTextsIsUnchanged(t *testing.T) {
	rig := newTextsRig(t)
	f := rig.runtime(func(report runtime.TextsReport) { t.Errorf("a launch of no folder reported a fallback: %+v", report) })
	_, control, _ := rig.specs(t, f)

	argv, pairs := newWindow(t, f, control)

	command := argv[len(argv)-1]
	prompt := panePrompt(t, claim.RoleTester, argv)
	if want := innerCommand(nil, "omp", "", filepath.Join(rig.state, "secrets", "legion-omp-legion-43-tester-system-prompt.md")); !strings.HasSuffix(command, " -- "+want) {
		t.Errorf("the pane's command %q does not end with innerCommand's own %q", command, want)
	}
	if strings.Contains(command, "--extension") {
		t.Errorf("the pane's command names an extension root: %q", command)
	}
	if _, count := pairNamed(pairs, texts.RootVariable); count != 0 {
		t.Errorf("the pane is told %s", texts.RootVariable)
	}
	if value, _ := pairNamed(pairs, "PI_CONFIG_FILES"); value != strings.Join([]string{rig.operator, rig.role, rig.alpha}, ":") {
		t.Errorf("PI_CONFIG_FILES = %q, want the operator's overlay, the role's and the arm's alone", value)
	}
	if prompt != controlsRole {
		t.Errorf("the prompt = %q, want %q", prompt, controlsRole)
	}
	if _, err := os.Stat(filepath.Join(rig.state, "texts")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a launch of no folder made %s (%v)", filepath.Join(rig.state, "texts"), err)
	}
}
