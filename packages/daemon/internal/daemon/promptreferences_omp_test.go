package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// The load probe resolves every task agent and skill Legion's prompts name through the real Oh My
// Pi's own agent and skill discovery, on the lane the probed Oh My Pi loads the plugins by: a pod's
// two explicit roots beside its discovery, or a pane's installed plugins. Only the real binary shows
// that the probe's imports of that discovery work and see what the launch sees, the profile's skill
// settings included, since a session drops a skill they disable or ignore. The profile's one model
// is an operator's provider nothing listens on, so no credential the machine carries decides the
// run (no probe here makes a model call). The Envoy plugin is the test fixture's, publishing the
// interface as the real one does; the real one loads in TestTheRealPluginsOnTheRealOhMyPi. Every
// path the test hands Oh My Pi sits under testbin.OMPDir rather than the caller's TMPDIR: Bun,
// which Oh My Pi runs on, loads no module from a path holding a '?', so under such a TMPDIR the
// pinned Oh My Pi loads neither its natives nor a plugin. cybermonklabs/oh-my-pi#46 fixes the
// natives; a plugin under such a path still cannot load, and its error then names Bun's limit.
func TestThePromptReferenceProbeOnTheRealOhMyPi(t *testing.T) {
	omp := testbin.OMP(t)
	noAgent := "finds no task agent thermonuclear-deep-review (dispatched by dist/skills/legion-worker/SKILL.md)"
	noRubric := "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md)"
	for _, testCase := range []struct {
		name                 string
		agent, rubric, onPod bool
		// settings is appended to the profile's config.yml, the settings the launch reads.
		settings string
		refusal  string
	}{
		{name: "a pod, the plugin shipping the agent and its rubric", agent: true, rubric: true, onPod: true},
		{name: "a pod, the plugin without the agent", rubric: true, onPod: true, refusal: noAgent},
		{name: "a pod, the plugin without the rubric", agent: true, onPod: true, refusal: noRubric},
		{name: "a pane, the installed plugin shipping the agent and its rubric", agent: true, rubric: true},
		{name: "a pane, the installed plugin without the agent", rubric: true, refusal: noAgent},
		{name: "a pane, the installed plugin without the rubric", agent: true, refusal: noRubric},
		{name: "a pod, a profile that disables the rubric", agent: true, rubric: true, onPod: true,
			settings: "disabledExtensions:\n  - skill:thermonuclear-deep-review\n", refusal: noRubric},
		{name: "a pane, a profile that ignores the rubric", agent: true, rubric: true,
			settings: "skills:\n  ignoredSkills:\n    - thermonuclear-deep-review\n", refusal: noRubric},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := testbin.OMPDir(t, omp)
			home := filepath.Join(dir, "home")
			testbin.OMPHome(t, omp, home)
			root, envoyRoot := referencePlugin(t, dir, testCase.agent, testCase.rubric), testEnvoyPlugin(t, dir, nil)
			agentDir := filepath.Join(home, ".omp", "profiles", "legion", "agent")
			mkdir(t, agentDir)
			for name, content := range map[string]string{
				"models.yml": "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n" +
					"    apiKey: unused\n    models:\n      - id: probe-test\n        name: Probe test\n",
				"config.yml": "modelRoles:\n  default: operator/probe-test\n",
			} {
				if err := os.WriteFile(filepath.Join(agentDir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			environ := []string{"HOME=" + home, "OMP_PROFILE=legion", "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
			if testCase.settings != "" {
				config, err := os.OpenFile(filepath.Join(agentDir, "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := config.WriteString(testCase.settings); err != nil {
					t.Fatal(err)
				}
				if err := config.Close(); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{}
			for _, pair := range environ {
				name, value, _ := strings.Cut(pair, "=")
				env[name] = value
			}
			log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
			var err error
			if testCase.onPod {
				err = ProbeImage(context.Background(), ImageProbe{Omp: shellprefix.Word(omp), Contract: 3, Env: env, WorkDir: dir, PluginRoot: root, EnvoyPluginRoot: envoyRoot, RoleReferences: promptrefs.New(), Log: log})
			} else {
				installPlugins(t, omp, environ, envoyRoot, root)
				// A pane loads the plugins through discovery, as the daemon's boot gate on tmux
				// probes it.
				err = pluginGate{env: env, workDir: dir, invocation: shellprefix.Word(omp), timeout: defaultProbeTimeout, retry: bootprobe.Image,
					contract: 3, log: log}.verify(context.Background())
			}

			switch {
			case testCase.refusal == "" && err != nil:
				t.Fatalf("ProbeImage = %v, want every probe to pass", err)
			case testCase.refusal != "" && (err == nil || !strings.Contains(err.Error(), testCase.refusal)):
				t.Fatalf("ProbeImage = %v, want the refusal to say %q", err, testCase.refusal)
			case testCase.refusal != "" && strings.Contains(err.Error(), "scout"):
				t.Errorf("ProbeImage = %v, which names scout, a bundled agent Oh My Pi finds", err)
			case testCase.refusal != "" && strings.Contains(err.Error(), "legion-worker (loaded by"):
				t.Errorf("ProbeImage = %v, which names legion-worker, a skill the plugin ships", err)
			}
		})
	}
}

// installPlugins installs each of roots into the profile environ names, as an operator installs a
// release: `omp plugin install <dir>` links the directory into the profile's plugins.
func installPlugins(t *testing.T, omp string, environ []string, roots ...string) {
	t.Helper()
	for _, root := range roots {
		install := exec.Command(omp, "plugin", "install", root)
		install.Env = environ
		if out, err := install.CombinedOutput(); err != nil {
			t.Fatalf("omp plugin install %s: %v\n%s", root, err, out)
		}
	}
}

// The plugins this checkout releases, packed as the release packs them and installed into a
// pane's profile on the real Oh My Pi. With pi-envoy alone — every session that is not a Legion
// pane — the probe reads no Legion marker and the Envoy interface at version 5, and the gate
// refuses, naming the pi-legion release to install; with both, it reads pi-legion loaded,
// speaking the interface version pi-envoy publishes, and the gate passes on the skills and agents
// the two packages ship between them. Only the real bundles show that the symbol strings the probe
// reads are the ones the entries write.
func TestTheRealPluginsOnTheRealOhMyPi(t *testing.T) {
	omp := testbin.OMP(t)
	// The packed plugins sit outside the checkout as well as off a '?' path, so Oh My Pi resolves
	// none of their imports from the repository's node_modules: a bundle missing a dependency fails
	// here as it would where it is installed.
	checkout := filepath.Join("..", "..", "..", "..")
	plugins := testtmp.WithoutAccepting(t, "", "?", func(dir string) error { return outsideCheckout(checkout, dir) })
	envoy, legion := packedPlugin(t, plugins, "pi-envoy", "envoy"), packedPlugin(t, plugins, "pi-legion", "legion")
	for _, testCase := range []struct {
		name      string
		installed []string
		// answer is on the probe's own output; refusal, on the gate's, or "" for a pass.
		answer  []string
		refusal string
	}{
		{"pi-envoy alone", []string{envoy}, []string{"LEGION_PLUGIN_LOADED=no\n", "LEGION_ENVOY_INTERFACE=5\n"},
			"pi-legion manifest at @MANIFEST could not be read"},
		{"both", []string{envoy, legion}, []string{"LEGION_PLUGIN_LOADED=yes\n", "LEGION_PLUGIN_ENVOY_INTERFACE=5\n", "LEGION_ENVOY_INTERFACE=5\n"}, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := testbin.OMPDir(t, omp)
			home := filepath.Join(dir, "home")
			testbin.OMPHome(t, omp, home)
			agentDir := filepath.Join(home, ".omp", "profiles", "legion", "agent")
			writeTree(t, agentDir, map[string]string{
				"models.yml": "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n" +
					"    apiKey: unused\n    models:\n      - id: probe-test\n        name: Probe test\n",
				"config.yml": "modelRoles:\n  default: operator/probe-test\n",
			})
			environ := []string{"HOME=" + home, "OMP_PROFILE=legion", "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
			installPlugins(t, omp, environ, testCase.installed...)
			env := map[string]string{}
			for _, pair := range environ {
				name, value, _ := strings.Cut(pair, "=")
				env[name] = value
			}

			// The probe's own answer, read as the gate reads it: `omp models` with the probe as an
			// extension beside the installed plugins, under the pane's environment.
			probe := filepath.Join(dir, "probe.mjs")
			if err := os.WriteFile(probe, pluginLoadProbe, 0o600); err != nil {
				t.Fatal(err)
			}
			models := exec.Command(omp, "models", "--extension", probe, "--json")
			models.Env, models.Dir = environ, dir
			var stderr bytes.Buffer
			models.Stderr = &stderr
			if err := models.Run(); err != nil {
				t.Fatalf("omp models with the probe: %v\n%s", err, stderr.String())
			}
			for _, want := range testCase.answer {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the probe answered without %q:\n%s", strings.TrimSpace(want), stderr.String())
				}
			}

			// The gate, in a pane's lane, against the contract the packed pi-legion declares: this
			// daemon's. The image build's probe skips the agents' models, and so does this, since
			// the profile configures none of the roles the shipped agents name.
			gate := pluginGate{env: env, workDir: dir, invocation: shellprefix.Word(omp), timeout: defaultProbeTimeout, retry: bootprobe.Image,
				contract: api.DaemonAPIVersion, skipAgentModels: true, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
			err := gate.verify(context.Background())
			switch {
			case testCase.refusal == "" && err != nil:
				t.Fatalf("the gate = %v, want a pass on both packed plugins", err)
			case testCase.refusal != "":
				want := strings.ReplaceAll(testCase.refusal, "@MANIFEST", manifestAt(filepath.Join(home, ".omp", "profiles", "legion")))
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Install the @sjawhar/pi-legion release") {
					t.Fatalf("the gate = %v, want the refusal saying %q and naming the @sjawhar/pi-legion release", err, want)
				}
			}
		})
	}
}

// outsideCheckout answers why dir will not do for a module that must resolve nothing from
// checkout's node_modules: it is the checkout or below it. The two are compared by their real
// paths, so a symlink into the checkout, or a checkout reached through one, counts.
func outsideCheckout(checkout, dir string) error {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return fmt.Errorf("the checkout %s has no real path: %w", checkout, err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%s has no real path: %w", dir, err)
	}
	if rel, err := filepath.Rel(root, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("it is inside the checkout %s, whose node_modules would resolve what the plugins must carry", root)
	}
	return nil
}

// A directory inside the checkout is refused whether it is named by its own path or through a
// symlink from outside, and a directory outside it is taken.
func TestOutsideCheckoutComparesRealPaths(t *testing.T) {
	root := t.TempDir()
	checkout, outside := filepath.Join(root, "checkout"), filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(checkout, ".cache", "x"), outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(checkout, ".cache"), alias); err != nil {
		t.Fatal(err)
	}
	for dir, inside := range map[string]bool{
		filepath.Join(checkout, ".cache", "x"): true,
		filepath.Join(alias, "x"):              true,
		checkout:                               true,
		outside:                                false,
	} {
		if err := outsideCheckout(checkout, dir); (err != nil) != inside {
			t.Errorf("outsideCheckout(%s) = %v, want refused %t", dir, err, inside)
		}
	}
}

// packedPlugin packs the plugin under packages/<name> as its release is packed — the committed
// manifest's `omp.extensions` rewritten to the one built bundle, dist/<entry>.js, then `bun pm
// pack`, which runs the package's prepack (the build, the notices, the staged skills) — and
// unpacks the tarball into base, a directory of the test's (testbin.OMPDir), which it answers. The
// rewrite is in place, since bun packs the manifest it finds, so the committed manifest is
// restored byte for byte whatever the pack did, and no two packs of one package may run at once;
// the one test that packs does so one package after the other.
func packedPlugin(t *testing.T, base, name, entry string) string {
	t.Helper()
	pkg := filepath.Join("..", "..", "..", name)
	path := filepath.Join(pkg, "package.json")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(committed, &manifest); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	manifest["omp"].(map[string]any)["extensions"] = []string{"dist/" + entry + ".js"}
	rewritten, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "pack-"+name)
	mkdir(t, out)
	pack := exec.Command("bun", "pm", "pack", "--destination", out, "--quiet")
	pack.Dir = pkg
	output, packErr := pack.CombinedOutput()
	if err := os.WriteFile(path, committed, 0o644); err != nil {
		t.Fatalf("restore %s: %v", path, err)
	}
	if packErr != nil {
		t.Fatalf("bun pm pack in %s: %v\n%s", pkg, packErr, output)
	}
	tarballs, err := filepath.Glob(filepath.Join(out, "*.tgz"))
	if err != nil || len(tarballs) != 1 {
		t.Fatalf("bun pm pack wrote %v (%v), want one tarball", tarballs, err)
	}
	unpacked := filepath.Join(base, name)
	mkdir(t, unpacked)
	if output, err := exec.Command("tar", "xzf", tarballs[0], "-C", unpacked, "--strip-components=1").CombinedOutput(); err != nil {
		t.Fatalf("unpack %s: %v\n%s", tarballs[0], err, output)
	}
	return unpacked
}
