package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// These tests carry no build tag, so CI's daemon-go race step, which runs ./internal/launcher/
// whole, runs them; childstarts_test.go, which CI runs only through a fixed -run list, holds none.

// legionPluginTexts are the texts the two plugins of a texts case ship, by their paths in a
// folder of texts: pi-envoy the skill dispatch-first, pi-legion the agents oracle and deep-worker
// and the skill legion-worker with one reference.
var legionPluginTexts = map[string]string{
	"skills/dispatch-first/SKILL.md":                "---\nname: dispatch-first\ndescription: Legion's.\n---\n\nLegion's dispatch-first.\n",
	"agents/oracle.md":                              "---\nname: oracle\ndescription: Legion's.\n---\n\nLegion's oracle.\n",
	"agents/deep-worker.md":                         "---\nname: deep-worker\ndescription: Legion's.\n---\n\nLegion's deep worker.\n",
	"skills/legion-worker/SKILL.md":                 "---\nname: legion-worker\ndescription: Legion's.\n---\n\nLegion's worker skill.\n",
	"skills/legion-worker/references/plan-tests.md": "Legion's reference on plan tests.\n",
}

// The arm's own texts a case's folder holds: an agent and a reference of a pi-legion skill.
const (
	armOracle    = "---\nname: oracle\ndescription: The arm's.\n---\n\nThe arm's oracle.\n"
	armPlanTests = "The arm's reference on plan tests.\n"
	// legionPrompt and armPrompt are the system prompts of the control's start and of the folder's.
	legionPrompt = "Legion's role prompt.\n"
	armPrompt    = "The arm's role prompt.\n"
)

// textsChild is the stand-in agent a texts case starts, as `/bin/sh -c textsChild omp <args>`: it
// writes what it was started with into the directory $TOLD names — its arguments after its name,
// each ended by a NUL (argv); its own environment as /proc reads it (environ); the bytes of the
// file its --append-system-prompt names (prompt) — then the marker done, last, and waits to be
// stopped.
const textsChild = `set -e
prev=
for arg in "$@"; do
  if [ "$prev" = --append-system-prompt ]; then cat "$arg" > "$TOLD/prompt"; fi
  prev=$arg
done
printf '%s\0' "$@" > "$TOLD/argv"
cat "/proc/$$/environ" > "$TOLD/environ"
: > "$TOLD/done"
exec sleep 600`

// textsCase is one role launcher of the start rig (newRig) that is handed folders of texts: the two
// plugins its starts load, the issue's volume the launcher reaches every root from (useTextsVolume)
// and the role's state home on it, under the volume's scratch/state as in a pod, which its roots
// are built under, the operator's settings overlay its PI_CONFIG_FILES names first (which ignores
// the skill operator-skill, so a root's settings layer has a base it must keep), the config root
// its profile resolves to, which is under the home, and the launcher's view of the container's
// home, of the directory that holds what is mounted under it and of the mounts there role-init
// binds back (useHomeView), which are directories of the case's own and the mount points it lists
// in mounted, by their paths relative to homeMounts.
type textsCase struct {
	t                     *testing.T
	g                     *rig
	envoy, legion         string
	volume                string
	stateHome, overlay    string
	configRoot, workspace string
	home, homeMounts      string
	mounted               []string
}

// useHomeView points the launcher's view of the container at a home, a home mounts directory and
// a mount table of the test's own (roleHome, roleHomeMounts, homeMountsUnder), since a machine
// running the tests has neither the image's /home/legion nor its /var/run/legion/home, nor the
// mounts an issue pod's container holds there: bound answers, when the launcher reads the table,
// the mount points it lists under mounts, by their paths relative to it. It puts the image's back
// when the test ends. It is called before the rig starts, which reads them, and a test that calls
// it is not parallel: parallel tests read them.
func useHomeView(t *testing.T, home, mounts string, bound func() []string) {
	t.Helper()
	previousHome, previousMounts, previousUnder := roleHome, roleHomeMounts, homeMountsUnder
	roleHome, roleHomeMounts = home, mounts
	homeMountsUnder = func(dir string) (string, []string, error) {
		if dir != mounts {
			return "", nil, fmt.Errorf("the test's mount table is of %s, not %s", mounts, dir)
		}
		var points []string
		for _, rel := range bound() {
			points = append(points, filepath.Join(mounts, filepath.FromSlash(rel)))
		}
		return mounts, points, nil
	}
	t.Cleanup(func() { roleHome, roleHomeMounts, homeMountsUnder = previousHome, previousMounts, previousUnder })
}

// useTextsVolume points the issue's volume the launcher reaches every root of a folder of texts
// from (textsVolume) at a directory of the test's own, since a machine running the tests has no
// /legion, and puts the pod's back when the test ends. It is called before the rig starts, as
// useHomeView is, and a test that calls it is not parallel.
func useTextsVolume(t *testing.T, volume string) {
	t.Helper()
	previous := textsVolume
	textsVolume = volume
	t.Cleanup(func() { textsVolume = previous })
}

func newTextsCase(t *testing.T) *textsCase {
	t.Helper()
	base := t.TempDir()
	volume := filepath.Join(base, "volume")
	c := &textsCase{
		t:     t,
		envoy: filepath.Join(base, "pi-envoy"), legion: filepath.Join(base, "pi-legion"),
		volume: volume, stateHome: filepath.Join(volume, "scratch", "state", "tester"), overlay: filepath.Join(base, "operator", "overlay.yml"),
		configRoot: filepath.Join(base, "home", ".omp"), workspace: filepath.Join(base, "workspace"),
		home: filepath.Join(base, "home"), homeMounts: filepath.Join(base, "home-mounts"),
	}
	if err := os.Mkdir(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	useHomeView(t, c.home, c.homeMounts, func() []string { return c.mounted })
	useTextsVolume(t, c.volume)
	c.g = newRig(t)
	for p, body := range legionPluginTexts {
		writeTextsFile(t, c.pluginFile(p), body)
	}
	writeTextsFile(t, c.overlay, "skills:\n  ignoredSkills:\n    - operator-skill\n")
	return c
}

// pluginFile is where the plugins ship the text at p, a path of a folder of texts.
func (c *textsCase) pluginFile(p string) string {
	if rest, ok := strings.CutPrefix(p, "agents/"); ok {
		return filepath.Join(c.legion, "agents", rest)
	}
	rest := strings.TrimPrefix(p, "skills/")
	if strings.HasPrefix(rest, "dispatch-first/") {
		return filepath.Join(c.envoy, "dist", "skills", rest)
	}
	return filepath.Join(c.legion, "dist", "skills", rest)
}

func writeTextsFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// folder writes a folder of texts holding the arm's oracle and its reference on plan tests, with a
// manifest that records for each the digest of the plugin's text it replaces, or of another
// release's text for each path of stale, and returns the folder and its fingerprint as the daemon
// reads it.
func (c *textsCase) folder(stale ...string) (string, string) {
	c.t.Helper()
	dir := c.t.TempDir()
	files := map[string]string{"agents/oracle.md": armOracle, "skills/legion-worker/references/plan-tests.md": armPlanTests}
	digests := map[string]string{}
	for p, body := range files {
		writeTextsFile(c.t, filepath.Join(dir, filepath.FromSlash(p)), body)
		digests[p] = texts.Digest([]byte(legionPluginTexts[p]))
		if slices.Contains(stale, p) {
			digests[p] = texts.Digest([]byte("A plugin text of another release.\n"))
		}
	}
	if err := texts.WriteManifest(dir, digests); err != nil {
		c.t.Fatal(err)
	}
	read, err := texts.Read(dir)
	if err != nil {
		c.t.Fatal(err)
	}
	return dir, read.Fingerprint
}

// root is the extension root a start of generation builds.
func (c *textsCase) root(generation uint64) string {
	return shimwire.TextsRoot(c.stateHome, generation)
}

// promptFile is where the launcher writes generation's system prompt, which the agent's argv names.
func (c *textsCase) promptFile(generation uint64) string {
	return filepath.Join(c.g.generationDir(generation), "system-prompt.md")
}

// agentArgs are the agent's arguments after its executable for a start of generation: the
// extension root first where root names one, as the sandbox runtime puts it, then the agent's own.
func (c *textsCase) agentArgs(generation uint64, root string) []string {
	var args []string
	if root != "" {
		args = append(args, "--extension", root)
	}
	return append(args, "--mode", "rpc", "--append-system-prompt", c.promptFile(generation))
}

// env is the environment a start of generation tells its child, root and layer being the texts
// start's root and settings layer, "" for a start of no folder: the role's state home, the profile
// the child's Oh My Pi resolves (its config root, the default profile), the workspace, and the
// operator's overlay first in PI_CONFIG_FILES, with the layer last after it and the root as
// LEGION_TEXTS_ROOT where the start runs a folder.
func (c *textsCase) env(told, root, layer string) []string {
	overlays := c.overlay
	if layer != "" {
		overlays += ":" + layer
	}
	env := []string{
		"TOLD=" + told, "XDG_STATE_HOME=" + c.stateHome, "OMP_CONFIG_ROOT=" + c.configRoot, "OMP_PROFILE=",
		"LEGION_WORKSPACE=" + c.workspace, "PI_CONFIG_FILES=" + overlays,
	}
	if root != "" {
		env = append(env, texts.RootVariable+"="+root)
	}
	return env
}

// start is the start of generation that hands the launcher folder, read by the daemon at
// fingerprint, as the sandbox runtime writes it: the root first in the agent's argv, the root as
// LEGION_TEXTS_ROOT and its settings layer last in PI_CONFIG_FILES, the arm's prompt, and the
// image's two plugins. Its child says what it saw in told.
func (c *textsCase) start(generation uint64, folder, fingerprint, told string) shimwire.LauncherStart {
	root := c.root(generation)
	return shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv:  append([]string{"/bin/sh", "-c", textsChild, "omp"}, c.agentArgs(generation, root)...),
		Env:   c.env(told, root, filepath.Join(root, texts.SettingsLayerName)),
		Files: map[string]string{"system-prompt.md": armPrompt},
		Texts: &shimwire.LauncherTexts{Folder: folder, Fingerprint: fingerprint, Root: root, EnvoyRoot: c.envoy, LegionRoot: c.legion},
	}
}

// fallback is the start of the arm's control the daemon sends for generation once its launcher
// refused the folder, under the id the sandbox runtime gives it: Legion's prompt, and no root in
// the argv, the environment or the frame.
func (c *textsCase) fallback(generation uint64, told string) shimwire.LauncherStart {
	return shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10) + "-texts-fallback", Generation: generation,
		Argv:  append([]string{"/bin/sh", "-c", textsChild, "omp"}, c.agentArgs(generation, "")...),
		Env:   c.env(told, "", ""),
		Files: map[string]string{"system-prompt.md": legionPrompt},
	}
}

// textsSeen is what a texts case's child was started with, as it wrote it (textsChild).
type textsSeen struct {
	argv    []string
	environ map[string]string
	prompt  string
}

// toldChild waits for the child that says what it saw in told, and reads what it says.
func toldChild(t *testing.T, told string) textsSeen {
	t.Helper()
	if !testwait.Holds(beforeDeadline(t), testwait.Poll, func() bool {
		_, err := os.Stat(filepath.Join(told, "done"))
		return err == nil
	}) {
		t.Fatal("the child never said what it was started with")
	}
	read := func(name string) string {
		body, err := os.ReadFile(filepath.Join(told, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	saw := textsSeen{environ: map[string]string{}, prompt: read("prompt")}
	for _, arg := range strings.Split(read("argv"), "\x00") {
		saw.argv = append(saw.argv, arg)
	}
	saw.argv = saw.argv[:len(saw.argv)-1] // after the last argument's NUL
	for _, entry := range strings.Split(read("environ"), "\x00") {
		if name, value, ok := strings.Cut(entry, "="); ok {
			saw.environ[name] = value
		}
	}
	return saw
}

// nothingWritten fails t where a refused start of generation wrote anything of it: the role's
// private directory holds no entry, so no g<generation> directory and no credential, and no root
// is at the generation's root.
func (c *textsCase) nothingWritten(generation uint64) {
	c.t.Helper()
	if entries, err := os.ReadDir(c.g.private); err != nil || len(entries) != 0 {
		c.t.Errorf("the refused start left the private directory %s holding %v (%v), want nothing", c.g.private, entries, err)
	}
	if _, err := os.Lstat(c.root(generation)); !errors.Is(err, os.ErrNotExist) {
		c.t.Errorf("the refused start left %s: %v, want no root", c.root(generation), err)
	}
}

// ignoredSkills is the skills.ignoredSkills the settings layer at file sets.
func ignoredSkills(t *testing.T, file string) []string {
	t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var layer struct {
		Skills struct {
			IgnoredSkills []string `yaml:"ignoredSkills"`
		} `yaml:"skills"`
	}
	if err := yaml.Unmarshal(body, &layer); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return layer.Skills.IgnoredSkills
}

// Every hello a launcher of this release writes says it checks a folder of texts a start hands it
// (shimwire.LauncherHello.ChecksTexts): the daemon hands a folder only to a launcher that said so,
// and to any other sends the start of the arm's control. The controller pod's single role says it,
// and so does each role of an issue pod's one container, on its own connection.
func TestTheLauncherHelloSaysItChecksTexts(t *testing.T) {
	if hello := newRig(t).hello; !hello.ChecksTexts {
		t.Errorf("the single role's hello = %#v, want it to say it checks texts", hello)
	}
	f := newFleet(t, 2*time.Second, "tester", "reviewer")
	for _, role := range []string{"tester", "reviewer"} {
		if hello := f.role(role).hello; !hello.ChecksTexts {
			t.Errorf("role %s's hello = %#v, want it to say it checks texts", role, hello)
		}
	}
}

// A folder whose agent or skill file records a digest that is not that of the plugin file the
// start's agent loads is refused before the launcher writes anything: the answer is a refusal
// that names the file and the cause `digest`, in the frame's own member and in its sentence beside
// the folder, the role's private directory holds no g<generation> directory, no root is built and
// no child starts. One row is an agent, the other a reference of a skill. The negative control is
// the same folder recording the plugin files' digests, which builds the root and starts the child.
func TestAStaleAgentOrSkillRefusesTheStartBeforeItWritesAnything(t *testing.T) {
	for _, stale := range []string{"agents/oracle.md", "skills/legion-worker/references/plan-tests.md"} {
		t.Run(stale, func(t *testing.T) {
			c := newTextsCase(t)
			folder, fingerprint := c.folder(stale)
			told := t.TempDir()
			got := c.g.start(c.start(7, folder, fingerprint, told))
			if got.OK || got.Texts == nil || got.Texts.Path != stale || got.Texts.Cause != string(texts.DigestDiffers) {
				t.Fatalf("the start of a folder whose %s is stale answered %#v (texts %+v), want a refusal naming the file and the cause digest", stale, got, got.Texts)
			}
			for _, named := range []string{folder, stale, "[digest]"} {
				if !strings.Contains(got.Error, named) {
					t.Errorf("the refusal %q does not name %s", got.Error, named)
				}
			}
			if err := got.Validate(); err != nil {
				t.Errorf("the refusal does not validate: %v", err)
			}
			c.nothingWritten(7)
			if entries, _ := os.ReadDir(told); len(entries) != 0 {
				t.Errorf("a child of the refused start ran and wrote %v", entries)
			}
			if c.g.latest.Child != nil {
				t.Errorf("the launcher reports child %+v after the refusal, want none", c.g.latest.Child)
			}
		})
	}
	t.Run("matching digests", func(t *testing.T) {
		c := newTextsCase(t)
		folder, fingerprint := c.folder()
		told := t.TempDir()
		if got := c.g.start(c.start(7, folder, fingerprint, told)); !got.OK || got.Texts != nil || got.RunningGeneration != 7 {
			t.Fatalf("the start of a folder that records the plugins' digests answered %#v, want generation 7 running", got)
		}
		toldChild(t, told)
		if body, err := os.ReadFile(filepath.Join(c.root(7), "agents", "oracle.md")); err != nil || string(body) != armOracle {
			t.Errorf("the root's oracle reads %q (%v), want the folder's", body, err)
		}
		c.g.stop(7)
	})
}

// A folder that changed between the daemon's read and the launcher's, here a file the arm edited
// with its manifest after the daemon read the folder, is refused before the launcher writes
// anything, with the cause `changed` for the folder as a whole: the launcher would otherwise run a
// folder the daemon never checked, whose role parts are not the prompt the daemon composed.
func TestAFolderThatChangedSinceTheDaemonReadItRefusesTheStart(t *testing.T) {
	c := newTextsCase(t)
	folder, fingerprint := c.folder()
	writeTextsFile(t, filepath.Join(folder, "agents", "oracle.md"), armOracle+"An edit after the daemon's read.\n")
	if read, err := texts.Read(folder); err != nil || read.Fingerprint == fingerprint {
		t.Fatalf("the edited folder reads as %v at %s, want a sound folder at another fingerprint", err, read.Fingerprint)
	}
	told := t.TempDir()
	got := c.g.start(c.start(7, folder, fingerprint, told))
	if got.OK || got.Texts == nil || got.Texts.Path != "" || got.Texts.Cause != string(texts.Changed) {
		t.Fatalf("the start of a folder that changed answered %#v (texts %+v), want a refusal of the folder with the cause changed", got, got.Texts)
	}
	for _, named := range []string{folder, fingerprint, "[changed]"} {
		if !strings.Contains(got.Error, named) {
			t.Errorf("the refusal %q does not name %s", got.Error, named)
		}
	}
	c.nothingWritten(7)
	if entries, _ := os.ReadDir(told); len(entries) != 0 {
		t.Errorf("a child of the refused start ran and wrote %v", entries)
	}
}

// The real manager.start runs both starts of check 5180afe2's launcher half. A start that hands
// the launcher a sound folder starts the child with exactly the argv and environment the daemon
// wrote — the root first among the agent's arguments, LEGION_TEXTS_ROOT naming it, and the root's
// settings layer last in PI_CONFIG_FILES after the operator's overlay — and the arm's prompt; and
// while the child runs, the root on disk holds the folder's agent and reference over the plugins'
// texts, pi-legion's other agent, pi-envoy's skill, and the settings layer, whose ignored skills
// keep the operator's and add the collision names of the skill the folder replaces. A later
// generation's start of a stale folder is refused with nothing written, and the start of the arm's
// control the daemon then sends for the same generation, under start-<generation>-texts-fallback,
// starts the child with Legion's prompt and no root in its argv or environment, and builds none.
func TestATextsStartRunsTheChildWithTheRootAndAFallbackStartWithout(t *testing.T) {
	c := newTextsCase(t)
	folder, fingerprint := c.folder()
	told := t.TempDir()
	start := c.start(7, folder, fingerprint, told)
	if got := c.g.start(start); !got.OK || got.RunningGeneration != 7 {
		t.Fatalf("the texts start answered %#v, want generation 7 running", got)
	}
	root := c.root(7)
	layer := filepath.Join(root, texts.SettingsLayerName)
	saw := toldChild(t, told)
	if want := c.agentArgs(7, root); !slices.Equal(saw.argv, want) {
		t.Errorf("the texts start's child was started with %q, want %q: the root first", saw.argv, want)
	}
	if got := saw.environ[texts.RootVariable]; got != root {
		t.Errorf("the texts start's child has %s=%q, want %q", texts.RootVariable, got, root)
	}
	if got, want := saw.environ["PI_CONFIG_FILES"], c.overlay+":"+layer; got != want {
		t.Errorf("the texts start's child has PI_CONFIG_FILES=%q, want %q: the layer last", got, want)
	}
	if saw.prompt != armPrompt {
		t.Errorf("the texts start's child read the prompt %q, want the arm's %q", saw.prompt, armPrompt)
	}
	for p, want := range map[string]string{
		"agents/oracle.md":                              armOracle,
		"agents/deep-worker.md":                         legionPluginTexts["agents/deep-worker.md"],
		"skills/dispatch-first/SKILL.md":                legionPluginTexts["skills/dispatch-first/SKILL.md"],
		"skills/legion-worker/SKILL.md":                 legionPluginTexts["skills/legion-worker/SKILL.md"],
		"skills/legion-worker/references/plan-tests.md": armPlanTests,
	} {
		if body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err != nil || string(body) != want {
			t.Errorf("the root's %s reads %q (%v), want %q", p, body, err, want)
		}
	}
	if got, want := ignoredSkills(t, layer), []string{"operator-skill", "*/legion-worker", "*/legion-worker~*"}; !slices.Equal(got, want) {
		t.Errorf("the settings layer ignores %q, want %q: the operator's kept, then the replaced skill's collision names", got, want)
	}
	c.g.stop(7)

	staleFolder, staleFingerprint := c.folder("agents/oracle.md")
	refusedTold := t.TempDir()
	if got := c.g.start(c.start(8, staleFolder, staleFingerprint, refusedTold)); got.OK || got.Texts == nil || got.Texts.Cause != string(texts.DigestDiffers) {
		t.Fatalf("the texts start of a stale folder answered %#v, want a texts refusal", got)
	}
	if _, err := os.Lstat(c.g.generationDir(8)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused texts start wrote %s: %v", c.g.generationDir(8), err)
	}
	fallbackTold := t.TempDir()
	fallback := c.fallback(8, fallbackTold)
	if got := c.g.start(fallback); !got.OK || got.RunningGeneration != 8 || got.Texts != nil {
		t.Fatalf("the fallback start %s answered %#v, want generation 8 running", fallback.ID, got)
	}
	saw = toldChild(t, fallbackTold)
	if want := c.agentArgs(8, ""); !slices.Equal(saw.argv, want) {
		t.Errorf("the fallback start's child was started with %q, want %q: no root", saw.argv, want)
	}
	if got, ok := saw.environ[texts.RootVariable]; ok {
		t.Errorf("the fallback start's child has %s=%q, want no such variable", texts.RootVariable, got)
	}
	if got := saw.environ["PI_CONFIG_FILES"]; got != c.overlay {
		t.Errorf("the fallback start's child has PI_CONFIG_FILES=%q, want the operator's overlay alone %q", got, c.overlay)
	}
	if saw.prompt != legionPrompt {
		t.Errorf("the fallback start's child read the prompt %q, want Legion's %q", saw.prompt, legionPrompt)
	}
	if _, err := os.Lstat(c.root(8)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the fallback start built a root at %s: %v", c.root(8), err)
	}
	c.g.stop(8)
}

// A texts start removes the roots earlier generations of its role built, each directory named
// g<generation> under the state home's legion-texts, and nothing else: another entry of
// legion-texts, an entry of a root's name that is no directory (a link a role left, whose target
// is not followed), and the rest of the state home stay as they were.
func TestATextsStartRemovesEarlierRootsAndNothingElse(t *testing.T) {
	c := newTextsCase(t)
	roots := filepath.Join(c.stateHome, shimwire.TextsRootsDir)
	target := t.TempDir()
	writeTextsFile(t, filepath.Join(target, "kept.md"), "Not the launcher's.\n")
	writeTextsFile(t, filepath.Join(roots, "g3", "agents", "oracle.md"), armOracle)
	writeTextsFile(t, filepath.Join(roots, "g4", "texts-settings.yml"), "skills: {}\n")
	writeTextsFile(t, filepath.Join(roots, "notes.md"), "A role's note.\n")
	if err := os.Symlink(target, filepath.Join(roots, "g5")); err != nil {
		t.Fatal(err)
	}
	kept := []string{
		filepath.Join(roots, "notes.md"),
		filepath.Join(c.stateHome, "omp", "profiles", "legion", "logs", "omp.log"),
		filepath.Join(c.stateHome, "legion-plugins", "g3", "package.json"),
		filepath.Join(target, "kept.md"),
	}
	for _, file := range kept[1:3] {
		writeTextsFile(t, file, "The role's.\n")
	}
	folder, fingerprint := c.folder()
	told := t.TempDir()
	if got := c.g.start(c.start(6, folder, fingerprint, told)); !got.OK {
		t.Fatalf("the texts start of generation 6 answered %#v, want it running", got)
	}
	toldChild(t, told)
	for _, gone := range []string{"g3", "g4"} {
		if _, err := os.Lstat(filepath.Join(roots, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the root %s of an earlier generation is still there: %v", gone, err)
		}
	}
	for _, file := range kept {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("%s is gone: %v", file, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(roots, "g5")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link g5 is %v (%v), want it as the role left it", info, err)
	}
	if _, err := os.Stat(filepath.Join(c.root(6), texts.SettingsLayerName)); err != nil {
		t.Errorf("generation 6's root holds no settings layer: %v", err)
	}
	c.g.stop(6)
}

// A fault of the pod itself — here a state home that cannot be written to build the root — is an
// ordinary refusal of the start, with no Texts refusal, so the daemon charges it as a launch
// failure rather than falling back to the control's launch over a broken pod, and leaves nothing
// written under the generation directory or as a partial root.
func TestAPodFaultBuildingTheTextsRootIsAnOrdinaryStartRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	c := newTextsCase(t)
	roots := filepath.Join(c.stateHome, shimwire.TextsRootsDir)
	if err := os.MkdirAll(roots, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roots, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roots, 0o755) })
	folder, fingerprint := c.folder()
	told := t.TempDir()
	got := c.g.start(c.start(7, folder, fingerprint, told))
	if got.OK || got.Texts != nil || !strings.Contains(got.Error, c.root(7)) {
		t.Fatalf("the start whose root could not be written answered %#v (texts %+v), want an ordinary refusal naming the root", got, got.Texts)
	}
	c.nothingWritten(7)
	if entries, _ := os.ReadDir(told); len(entries) != 0 {
		t.Errorf("a child of the refused start ran and wrote %v", entries)
	}
}

// A link a role puts in place of a directory on the way from the issue's volume to the roots of
// folders of texts — the scratch, its state directory, the role's state home or its legion-texts,
// the directory renamed away first — pointing at a directory outside the volume that holds what
// the launcher would clear and build in there, an earlier root among it, is never followed,
// whether the start finds it there or a role puts it there after the clear: a start that finds it
// is refused before anything is built, as an ordinary refusal naming it as a symlink
// (clearTextsRoots), and no child starts; a link put there once the clear passed is refused by the
// build itself, which reaches the roots from the volume again, naming it as a symlink
// (buildTextsRoot). Either way the directory it points at holds what it held: nothing in it is
// removed, made or written.
func TestALinkAtTheTextsRootsIsNeverBuiltThrough(t *testing.T) {
	// plant makes the directory at rel under c's volume where it is absent, with the state home's
	// legion-texts under it, renames it away, and puts in its place a link to a directory outside
	// the volume holding another role's file and, at the link's legion-texts, an earlier root g3.
	// It answers the link, the directory it points at and what that holds.
	plant := func(t *testing.T, c *textsCase, rel string) (string, string, map[string]string) {
		t.Helper()
		roots := filepath.Join(c.stateHome, shimwire.TextsRootsDir)
		link := filepath.Join(c.volume, filepath.FromSlash(rel))
		if err := os.MkdirAll(roots, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(link, link+"-moved"); err != nil {
			t.Fatal(err)
		}
		below, err := filepath.Rel(link, roots)
		if err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		writeTextsFile(t, filepath.Join(target, "kept.md"), "Another role's.\n")
		writeTextsFile(t, filepath.Join(target, below, "g3", "agents", "oracle.md"), armOracle)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return link, target, tree(t, target)
	}
	untouched := func(t *testing.T, target string, before map[string]string) {
		t.Helper()
		if after := tree(t, target); !maps.Equal(after, before) {
			t.Errorf("the start went through the link to %s: it holds %v, was %v", target, after, before)
		}
	}
	// The directories on the way from the volume to the roots, by their paths under it.
	type place struct{ name, rel string }
	scratch := place{"the scratch", "scratch"}
	state := place{"its state directory", "scratch/state"}
	stateHome := place{"the role's state home", "scratch/state/tester"}
	roots := place{"legion-texts", "scratch/state/tester/" + shimwire.TextsRootsDir}

	for _, at := range []place{scratch, state, stateHome, roots} {
		t.Run("planted at "+at.name+" before the start", func(t *testing.T) {
			c := newTextsCase(t)
			link, target, before := plant(t, c, at.rel)
			folder, fingerprint := c.folder()
			told := t.TempDir()
			got := c.g.start(c.start(7, folder, fingerprint, told))
			if got.OK || got.Texts != nil || !strings.Contains(got.Error, link+" is a symlink") {
				t.Fatalf("the start whose %s is a link answered %#v (texts %+v), want an ordinary refusal naming %s as a symlink", at.rel, got, got.Texts, link)
			}
			untouched(t, target, before)
			c.nothingWritten(7)
			if entries, _ := os.ReadDir(told); len(entries) != 0 {
				t.Errorf("a child of the refused start ran and wrote %v", entries)
			}
		})
	}

	for _, at := range []place{state, roots} {
		t.Run("planted at "+at.name+" after the clear", func(t *testing.T) {
			c := newTextsCase(t)
			dir, fingerprint := c.folder()
			given := *c.start(7, dir, fingerprint, t.TempDir()).Texts
			if err := clearTextsRoots(given.Root); err != nil {
				t.Fatalf("clearTextsRoots over plain directories = %v, want it cleared", err)
			}
			link, target, before := plant(t, c, at.rel)
			folder, err := texts.Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := buildTextsRoot(folder, given, nil); err == nil || !strings.Contains(err.Error(), link+" is a symlink") {
				t.Errorf("the build over a link put at %s after the clear = %v, want a refusal naming %s as a symlink", at.rel, err, link)
			}
			untouched(t, target, before)
		})
	}
}

// A start whose root is not under the issue's volume, here because its state home is elsewhere,
// is refused as an ordinary refusal naming the root and the volume, and nothing is made there: the
// launcher reaches every root from the volume, and no role's state home is anywhere else.
func TestATextsRootOutsideTheVolumeIsAnOrdinaryStartRefusal(t *testing.T) {
	c := newTextsCase(t)
	c.stateHome = filepath.Join(t.TempDir(), "scratch", "state", "tester")
	folder, fingerprint := c.folder()
	told := t.TempDir()
	got := c.g.start(c.start(7, folder, fingerprint, told))
	if got.OK || got.Texts != nil || !strings.Contains(got.Error, c.root(7)) || !strings.Contains(got.Error, c.volume) {
		t.Fatalf("the start of a root outside the volume answered %#v (texts %+v), want an ordinary refusal naming %s and %s", got, got.Texts, c.root(7), c.volume)
	}
	c.nothingWritten(7)
	if _, err := os.Lstat(c.stateHome); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused start made %s: %v", c.stateHome, err)
	}
	if entries, _ := os.ReadDir(told); len(entries) != 0 {
		t.Errorf("a child of the refused start ran and wrote %v", entries)
	}
}

// A path at or under the home is where the launcher's view of the container holds what the role
// sees there, by the mounts role-init binds back: a path a mount covers, one at the path or above
// it, is under the home mounts directory, so a file a directory mount lacks is absent there however
// the image holds it, and a file mount inside a directory mount is read from the mounts too; a path
// no mount covers is the image's own, even where the home mounts directory holds an entry at its
// place, as it does beside a file mount. A path outside the home, one that only shares the home's
// prefix or a mount's, and one that climbs out of the home are the same in both views. A home
// mounts directory that is not there, as in the controller pod's single-role container, holds no
// mount whatever a table lists.
func TestTheHomeViewReadsAPathFromTheMountThatShowsItToTheRole(t *testing.T) {
	dir := t.TempDir()
	home, mounts := filepath.Join(dir, "home"), filepath.Join(dir, "mounts")
	// The image's own files under the home.
	writeTextsFile(t, filepath.Join(home, "operator", "lacked.yml"), "image\n")
	writeTextsFile(t, filepath.Join(home, "operator", "overlay.yml"), "image\n")
	writeTextsFile(t, filepath.Join(home, ".omp", "overlay.yml"), "image\n")
	writeTextsFile(t, filepath.Join(home, ".omp", "image.yml"), "image\n")
	writeTextsFile(t, filepath.Join(home, "operator-other", "overlay.yml"), "image\n")
	// What the container holds under the home mounts directory: a directory mount at operator with
	// a file mount inside it, a file mount at .omp/overlay.yml, and entries under no mount.
	bound := []string{".omp/overlay.yml", "operator", "operator/profile/config.yml"}
	writeTextsFile(t, filepath.Join(mounts, "operator", "overlay.yml"), "mounted\n")
	writeTextsFile(t, filepath.Join(mounts, "operator", "profile", "config.yml"), "mounted file\n")
	writeTextsFile(t, filepath.Join(mounts, ".omp", "overlay.yml"), "mounted\n")
	writeTextsFile(t, filepath.Join(mounts, ".omp", "image.yml"), "under no mount\n")
	writeTextsFile(t, filepath.Join(mounts, "operator-other", "overlay.yml"), "under no mount\n")
	outside := filepath.Join(dir, "elsewhere", "overlay.yml")
	writeTextsFile(t, outside, "outside\n")
	useHomeView(t, home, mounts, func() []string { return bound })
	view, err := launcherHomeView()
	if err != nil {
		t.Fatalf("launcherHomeView = %v", err)
	}
	for _, tc := range []struct {
		name, path, want string
		// read is what the view reads at path, "" where it finds no file.
		read string
	}{
		{"a file a directory mount holds", filepath.Join(home, "operator", "overlay.yml"), filepath.Join(mounts, "operator", "overlay.yml"), "mounted\n"},
		{"a file a directory mount lacks, which the image holds", filepath.Join(home, "operator", "lacked.yml"), filepath.Join(mounts, "operator", "lacked.yml"), ""},
		{"a file mount inside a directory mount", filepath.Join(home, "operator", "profile", "config.yml"), filepath.Join(mounts, "operator", "profile", "config.yml"), "mounted file\n"},
		{"a file mount over the image's file", filepath.Join(home, ".omp", "overlay.yml"), filepath.Join(mounts, ".omp", "overlay.yml"), "mounted\n"},
		{"a file beside a file mount, which no mount covers", filepath.Join(home, ".omp", "image.yml"), filepath.Join(home, ".omp", "image.yml"), "image\n"},
		{"a file nothing holds", filepath.Join(home, ".omp", "absent.yml"), filepath.Join(home, ".omp", "absent.yml"), ""},
		{"a path that shares a mount's prefix", filepath.Join(home, "operator-other", "overlay.yml"), filepath.Join(home, "operator-other", "overlay.yml"), "image\n"},
		{"a path outside the home", outside, outside, "outside\n"},
		{"a path that shares the home's prefix", home + "-other/overlay.yml", home + "-other/overlay.yml", ""},
		{"a path that climbs out of the home", filepath.Join(home, "..", "elsewhere", "overlay.yml"), filepath.Join(home, "..", "elsewhere", "overlay.yml"), "outside\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := view.path(tc.path); got != tc.want {
				t.Errorf("the home view's path of %s = %s, want %s", tc.path, got, tc.want)
			}
			body, err := view.read(tc.path)
			switch {
			case tc.read == "" && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("the home view reads %s as %q, %v; want no such file", tc.path, body, err)
			case tc.read != "" && (err != nil || string(body) != tc.read):
				t.Errorf("the home view reads %s as %q, %v; want %q", tc.path, body, err, tc.read)
			}
		})
	}
	t.Run("a home mounts directory that is not there", func(t *testing.T) {
		useHomeView(t, home, filepath.Join(dir, "absent"), func() []string { return bound })
		view, err := launcherHomeView()
		if err != nil {
			t.Fatalf("launcherHomeView = %v", err)
		}
		if path := filepath.Join(home, ".omp", "overlay.yml"); view.path(path) != path {
			t.Errorf("the home view's path of %s = %s, want it as it is", path, view.path(path))
		}
	})
}

// The base of the settings layer is read where the launcher sees the container, which is not where
// a role sees it: an operator's mount at or under the image's home is held under the home mounts
// directory, and bound back at its own path only in each role's namespace, over the image's home.
// So a settings overlay named at a path under the home that a mount holds is read there, where it
// would otherwise fail every start of the role on a file the role finds, or read the image's file
// the mount hides; a profile config.yml a mount holds is read over the image's own at that path,
// and a file mount inside a directory mount is read too; where a directory mount lacks a file the
// image holds, the role finds none, and nor does the base; and where no mount covers a path the
// image's file is read, whatever the home mounts directory holds there. The settings layer keeps
// the ignored skills of the file that wins, and a missing overlay refuses the start naming the path
// the role is told.
func TestATextsStartReadsTheSettingsBaseWhereTheLauncherSeesTheOperatorsMounts(t *testing.T) {
	list := func(patterns ...string) string {
		return "skills:\n  ignoredSkills: [" + strings.Join(patterns, ", ") + "]\n"
	}
	const overlay, profile = ".omp/overlay.yml", ".omp/profiles/legion/agent/config.yml"
	hidden := []string{"*/legion-worker", "*/legion-worker~*"}
	for _, tc := range []struct {
		name string
		// image and mounted are the files of the case by their path under the home: the image's own,
		// and those held under the home mounts directory. mounts are the mount points the case's
		// mount table lists there, by the same paths. overlays are the entries of PI_CONFIG_FILES
		// before the settings layer, paths under the home.
		image, mounted map[string]string
		mounts         []string
		overlays       []string
		want           []string
		refused        bool
	}{
		{name: "an overlay only a file mount holds",
			mounted: map[string]string{overlay: list("mounted-overlay")}, mounts: []string{overlay},
			overlays: []string{overlay}, want: []string{"mounted-overlay"}},
		{name: "an overlay a file mount holds over the image's",
			image:   map[string]string{overlay: list("image-overlay")},
			mounted: map[string]string{overlay: list("mounted-overlay")}, mounts: []string{overlay},
			overlays: []string{overlay}, want: []string{"mounted-overlay"}},
		{name: "an overlay a directory mount lacks, which the image holds",
			image: map[string]string{overlay: list("image-overlay")}, mounts: []string{".omp"},
			overlays: []string{overlay}, refused: true},
		{name: "a profile config.yml a directory mount holds over the image's",
			image:   map[string]string{profile: list("image-profile")},
			mounted: map[string]string{profile: list("mounted-profile")}, mounts: []string{".omp/profiles/legion"},
			want: []string{"mounted-profile"}},
		{name: "a profile config.yml a directory mount lacks, which the image holds",
			image: map[string]string{profile: list("image-profile")}, mounts: []string{".omp"}, want: nil},
		{name: "a profile config.yml a file mount inside a directory mount holds",
			image:   map[string]string{profile: list("image-profile")},
			mounted: map[string]string{profile: list("mounted-profile")}, mounts: []string{".omp", profile},
			want: []string{"mounted-profile"}},
		{name: "a profile config.yml no mount covers",
			image:   map[string]string{profile: list("image-profile")},
			mounted: map[string]string{profile: list("under-no-mount")}, want: []string{"image-profile"}},
		{name: "an overlay no view holds", overlays: []string{overlay}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTextsCase(t)
			for p, body := range tc.image {
				writeTextsFile(t, filepath.Join(c.home, filepath.FromSlash(p)), body)
			}
			for p, body := range tc.mounted {
				writeTextsFile(t, filepath.Join(c.homeMounts, filepath.FromSlash(p)), body)
			}
			// A mount point is there in the container: a directory where the case mounts no file.
			for _, p := range tc.mounts {
				point := filepath.Join(c.homeMounts, filepath.FromSlash(p))
				if _, err := os.Lstat(point); errors.Is(err, fs.ErrNotExist) {
					if err := os.MkdirAll(point, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.mounted = tc.mounts
			folder, fingerprint := c.folder()
			told := t.TempDir()
			start := c.start(7, folder, fingerprint, told)
			layer := filepath.Join(c.root(7), texts.SettingsLayerName)
			var entries []string
			for _, p := range tc.overlays {
				entries = append(entries, filepath.Join(c.home, filepath.FromSlash(p)))
			}
			start.Env = mergeEnv(start.Env, []string{
				"PI_CONFIG_FILES=" + strings.Join(append(entries, layer), ":"), "OMP_PROFILE=legion",
			})
			got := c.g.start(start)
			if tc.refused {
				if got.OK || got.Texts != nil || !strings.Contains(got.Error, entries[0]) {
					t.Fatalf("the start naming %s, which the role finds no file at, answered %#v (texts %+v), want an ordinary refusal naming it", entries[0], got, got.Texts)
				}
				c.nothingWritten(7)
				return
			}
			if !got.OK || got.RunningGeneration != 7 {
				t.Fatalf("the texts start answered %#v, want generation 7 running", got)
			}
			toldChild(t, told)
			if ignored, want := ignoredSkills(t, layer), append(slices.Clone(tc.want), hidden...); !slices.Equal(ignored, want) {
				t.Errorf("the settings layer ignores %q, want %q: the base of the file the role reads, then the replaced skill's collision names", ignored, want)
			}
			c.g.stop(7)
		})
	}
}

// Every role of an issue pod writes the issue's workspace, the launcher's working directory and so
// the project whose .omp/config.yml the base of a texts start's settings layer reads (textsBase)
// where no overlay sets the key, as none does here, so a role can make that file anything: a fifo,
// whose plain open waits for a writer for as long as none comes, and would hang the role's start
// and its connection's serve loop, or a link to /dev/zero, which a plain read holds whole in memory
// until the launcher, the pod's PID 1, and every role of the pod with it, is gone. Either is an
// ordinary refusal of the start naming the file, answered at once, and nothing of the generation
// is written, no root built and no child started. The rig waits for the answer with no span of its
// own (testwait.Receive), so a start that waits fails by name before the test binary's deadline,
// and one that is merely slow passes.
func TestATextsStartRefusesAFifoOrALinkToDevZeroAtTheWorkspaceSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"a fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a link to /dev/zero", func(t *testing.T, path string) {
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTextsCase(t)
			settings := filepath.Join(c.workspace, ".omp", "config.yml")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, settings)
			t.Chdir(c.workspace)
			folder, fingerprint := c.folder()
			told := t.TempDir()
			start := c.start(7, folder, fingerprint, told)
			start.Env = mergeEnv(start.Env, []string{"PI_CONFIG_FILES=" + filepath.Join(c.root(7), texts.SettingsLayerName)})
			got := c.g.start(start)
			if got.OK || got.Texts != nil || !strings.Contains(got.Error, settings) {
				t.Fatalf("the start over %s at %s answered %#v (texts %+v), want an ordinary refusal naming it", tc.name, settings, got, got.Texts)
			}
			c.nothingWritten(7)
			if entries, _ := os.ReadDir(told); len(entries) != 0 {
				t.Errorf("a child of the refused start ran and wrote %v", entries)
			}
		})
	}
}
