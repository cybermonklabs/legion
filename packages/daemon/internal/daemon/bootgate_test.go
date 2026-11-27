package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testkernel"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// manifestAt is where the tests' pi-legion manifest goes under a plugins root, and envoyManifestAt
// the pi-envoy manifest beside it, as `omp plugin install` lays both out.
func manifestAt(root string) string {
	return filepath.Join(root, "plugins", "node_modules", "@sjawhar", "pi-legion", "package.json")
}

func envoyManifestAt(root string) string {
	return filepath.Join(root, "plugins", "node_modules", "@sjawhar", "pi-envoy", "package.json")
}

// writeManifest writes a pi-legion manifest at path; legion is the raw JSON of its `legion`
// member, or empty for a manifest without one.
func writeManifest(t *testing.T, path, legion string) {
	t.Helper()
	body := `{"name":"@sjawhar/pi-legion","version":"1.57.0"`
	if legion != "" {
		body += `,"legion":` + legion
	}
	writeManifestFile(t, path, body+"}\n")
}

// writeEnvoyManifest writes a pi-envoy manifest at path: a name and a version, since the gate
// holds pi-envoy to no contract, only to a manifest of its name.
func writeEnvoyManifest(t *testing.T, path string) {
	t.Helper()
	writeManifestFile(t, path, `{"name":"@sjawhar/pi-envoy","version":"1.57.0"}`+"\n")
}

func writeManifestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s's directory: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// link installs the unpacked plugin at unpacked into a profile as `omp plugin install` does: the
// profile's package directory a link to it.
func link(t *testing.T, unpacked, installed string) {
	t.Helper()
	mkdir(t, filepath.Dir(installed))
	if err := os.Symlink(unpacked, installed); err != nil {
		t.Fatalf("link the plugin into the profile: %v", err)
	}
}

// The gate reads the manifest where Oh My Pi, started under the pane's environment, looks for its
// plugins: the profile OMP_PROFILE (else PI_PROFILE) names, under HOME's .omp, unless an XDG data
// root already holds that profile's state.
func TestPluginManifestPathIsWhereOhMyPiResolvesItsPluginsRoot(t *testing.T) {
	t.Parallel()
	passwdHome := ""
	if u, err := user.Current(); err == nil {
		passwdHome = u.HomeDir
	}
	for _, testCase := range []struct {
		name    string
		env     func(home, xdg string) map[string]string
		layout  func(t *testing.T, home, xdg string)
		want    func(home, xdg string) string
		profile string
	}{
		{
			name: "the default profile under HOME",
			env:  func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name:   "the default profile under an XDG data root that exists",
			env:    func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
		{
			name: "a named profile stays under HOME while its own XDG directory does not exist",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work"}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:    func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp", "profiles", "work")) },
			profile: "work",
		},
		{
			name: "a named profile whose XDG directory exists",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": " work "}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp", "profiles", "work")) },
			want:    func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp", "profiles", "work")) },
			profile: "work",
		},
		{
			name: "PI_PROFILE when OMP_PROFILE is unset",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "PI_PROFILE": "legacy"}
			},
			want:    func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp", "profiles", "legacy")) },
			profile: "legacy",
		},
		{
			name: "an empty OMP_PROFILE is the default profile, whatever PI_PROFILE says",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "", "PI_PROFILE": "legacy"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "OMP_PROFILE=default is the default profile",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "default"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "no HOME is the account's home directory",
			env:  func(string, string) map[string]string { return map[string]string{} },
			want: func(string, string) string { return manifestAt(filepath.Join(passwdHome, ".omp")) },
		},
		// Oh My Pi's config root is OMP_CONFIG_ROOT when it names an absolute directory, else
		// PI_CONFIG_DIR under HOME when that is set, and its XDG data root still wins when that
		// exists (getConfigRootOverride, getBaseConfigRoot, DirResolver's constructor).
		{
			name: "an absolute OMP_CONFIG_ROOT names the config root, over PI_CONFIG_DIR",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_CONFIG_ROOT": filepath.Join(home, "omp-root"), "PI_CONFIG_DIR": ".omp-alt"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, "omp-root")) },
		},
		{
			name: "PI_CONFIG_DIR names the config root under HOME",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp-alt")) },
		},
		{
			name: "an XDG data root that exists wins over PI_CONFIG_DIR",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
		// An honoured PI_CODING_AGENT_DIR moves only the agent directory. It reaches the plugins by
		// one route: an agent directory other than the config root's own turns the XDG data root off.
		{
			name: "an honoured PI_CODING_AGENT_DIR leaves the plugins under the config root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": filepath.Join(home, "elsewhere")}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "an honoured PI_CODING_AGENT_DIR turns the XDG data root off",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": filepath.Join(home, "elsewhere")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "PI_CODING_AGENT_DIR naming the config root's own agent directory keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": filepath.Join(home, ".omp", "agent") + "/"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
		{
			name: "a named profile ignores PI_CODING_AGENT_DIR",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work", "PI_CODING_AGENT_DIR": filepath.Join(home, "elsewhere")}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp", "profiles", "work")) },
			want:    func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp", "profiles", "work")) },
			profile: "work",
		},
		{
			name: "the agent directory PI_PROFILE hands down, under PI_CONFIG_DIR, keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work", "PI_CONFIG_DIR": ".omp-alt",
					"PI_CODING_AGENT_DIR": filepath.Join(home, ".omp-alt", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
		{
			name: "PI_PROFILE's agent directory under another config root is honoured",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work", "PI_CONFIG_DIR": ".omp-alt",
					"PI_CODING_AGENT_DIR": filepath.Join(home, ".omp", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp-alt")) },
		},
		// Oh My Pi resolves a relative agent directory against the directory it runs in, the
		// resolution's working directory, here <home>/work.
		{
			name: "a relative PI_CODING_AGENT_DIR elsewhere turns the XDG data root off",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": "agent"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "a relative PI_CODING_AGENT_DIR naming the config root's own agent directory keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": "../.omp/agent"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home, xdg := t.TempDir(), t.TempDir()
			if testCase.layout != nil {
				testCase.layout(t, home, xdg)
			}
			got, profile, err := pluginManifestPath(testCase.env(home, xdg), filepath.Join(home, "work"))
			if err != nil {
				t.Fatalf("pluginManifestPath: %v", err)
			}
			if want := testCase.want(home, xdg); got != want {
				t.Errorf("manifest = %s, want %s", got, want)
			}
			if profile != testCase.profile {
				t.Errorf("profile = %q, want %q", profile, testCase.profile)
			}
		})
	}
}

// Oh My Pi refuses a profile name it cannot use, and so does the gate, naming it.
func TestPluginManifestPathRefusesAProfileOhMyPiRefuses(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Work", "..", "work.", "-work", "con", "nul.txt"} {
		_, _, err := pluginManifestPath(map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": name}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: err = %v, want the invalid-profile refusal naming it", name, err)
		}
	}
}

// fakeOmp is an `omp` that answers the load probe by a plan, one step per attempt (the last step
// repeats), and records each attempt's argv, environment, and the probe file it was handed. Every
// yes-* step prints interface 1 for pi-legion, and 1 for pi-envoy except the steps that test a
// missing or mismatched pi-envoy, whatever the release's own number is, on purpose: the gate
// compares the number pi-legion speaks with the one pi-envoy publishes, never either with a
// release. invocation is path as the gate's OMP invocation holds it, one shell word
// (shellprefix.Word), as omplaunch.ResolveInvocation quotes LEGION_OMP_PATH: a temporary path can
// hold a space or a quote.
type fakeOmp struct {
	dir, path, prefix, invocation string
}

func newFakeOmp(t *testing.T, plan ...string) fakeOmp {
	t.Helper()
	dir := t.TempDir()
	// What the fake leaves running ends at the test's cleanup, if no stop has ended it first (held).
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o644) })
	if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write the plan: %v", err)
	}
	script := `#!/bin/sh
dir=` + shellprefix.Literal(dir) + `
# held stands in for a process that hangs: it runs until a signal ends it, the test's cleanup
# writes release, or the test process is gone, and never ends on a timer of its own.
held() { while [ ! -e "$dir/release" ] && kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; do sleep 0.05; done; }
n=$(( $(cat "$dir/count" 2>/dev/null || echo 0) + 1 ))
# Renamed into place: the test reads this file while a later attempt writes it, and a plain
# redirection truncates before it writes, so the reader can see it empty.
echo "$n" >"$dir/count.$n" && mv "$dir/count.$n" "$dir/count"
# One argument a line: the probe's path is a temporary one, which can hold a space.
printf '%s\n' "$@" >"$dir/argv.$n"
env >"$dir/env.$n"
cp "$3" "$dir/probe.$n"
step=$(sed -n "${n}p" "$dir/plan")
[ -n "$step" ] || step=$(tail -n 1 "$dir/plan")
# Where a real Oh My Pi reports each plugin loaded from: the real path of the legion.js or envoy.js
# the profile links, as import.meta.url renders it (symlinks resolved, a cache-busting query
# appended). Every yes-* step loads pi-legion from the profile speaking interface 1; what it says
# of pi-envoy, and of the pre-split package, is the step's own.
root="$HOME/.omp"
[ -n "${OMP_PROFILE:-}" ] && root="$root/profiles/$OMP_PROFILE"
installed=$(cd "$root/plugins/node_modules/@sjawhar/pi-legion" 2>/dev/null && pwd -P)
envoy=$(cd "$root/plugins/node_modules/@sjawhar/pi-envoy" 2>/dev/null && pwd -P)
legion="LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://$installed/dist/legion.js?mtime=1\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n"
with_envoy="LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://$envoy/dist/envoy.js?mtime=1\n"
case "$step" in
yes) printf "$legion$with_envoy" >&2; exit 0 ;;
# A pass that takes its time, as Oh My Pi does on a loaded machine: longer than a budget of a
# second or two, and far inside the rig's.
yes-slowly) sleep 2; printf "$legion$with_envoy" >&2; exit 0 ;;
# Answers, leaves a child holding the output pipe (held &), and exits. started holds when the kernel
# made this process (testexec.MarkStart's starttime, which no line of the process moves), and exited
# the moment just before its exit (testexec.MarkNow): between the two is the stand-in's own run.
yes-then-linger) ` + testexec.MarkStart(`"$dir/started"`) + `; printf "$legion$with_envoy" >&2; held & echo $! >"$dir/lingering.pid"; ` + testexec.MarkNow(`"$dir/exited"`) + `; exit 0 ;;
yes-elsewhere) printf "LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://$HOME/elsewhere/dist/legion.js?mtime=1\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n$with_envoy" >&2; exit 0 ;;
yes-unowned) printf "LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://$HOME/unowned/legion.js\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n$with_envoy" >&2; exit 0 ;;
# No pi-envoy; a skill asked for is missing too, so a test can see which refusal wins.
yes-no-envoy) printf "${legion}LEGION_ENVOY_INTERFACE=none\n" >&2; [ -z "${LEGION_PROMPT_SKILLS:-}" ] || echo "LEGION_PROMPT_SKILLS_MISSING=$LEGION_PROMPT_SKILLS" >&2; exit 0 ;;
yes-envoy-mismatch) printf "${legion}LEGION_ENVOY_INTERFACE=2\nLEGION_ENVOY_LOADED_FROM=file://$envoy/dist/envoy.js?mtime=1\n" >&2; exit 0 ;;
yes-legacy) printf "$legion${with_envoy}LEGION_LEGACY_PLUGIN_LOADED_FROM=file://$HOME/legacy/dist/legion.js?mtime=1\n" >&2; exit 0 ;;
yes-envoy-elsewhere) printf "${legion}LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://$HOME/envoy-elsewhere/dist/envoy.js?mtime=1\n" >&2; exit 0 ;;
yes-then-die) echo LEGION_PLUGIN_LOADED=yes >&2; echo "database is locked" >&2; exit 1 ;;
yes-then-killed) echo LEGION_PLUGIN_LOADED=yes >&2; kill -KILL $$ ;;
no) echo LEGION_PLUGIN_LOADED=no >&2; exit 0 ;;
silent) exit 0 ;;
denied) echo "secrets: ANTHROPIC_API_KEY was denied" >&2; exit 3 ;;
denied-then-killed) echo "secrets: ANTHROPIC_API_KEY was denied" >&2; kill -KILL $$ ;;
# Writes that attempt n's stand-in hangs, the fact a row's budget passes at (hangsUnder).
hang) : >"$dir/hanging.$n"; held; exit 0 ;;
# Answers, then hangs (held), so the budget cuts it only after the answer.
no-then-hang) echo LEGION_PLUGIN_LOADED=no >&2; held; exit 0 ;;
esac
`
	path := filepath.Join(dir, "omp")
	if err := execfile.Write(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	// The launch prefix lives under a path with a space and an apostrophe, so the launch command
	// must quote it as one single-quoted word. A renderer that stops quoting the prefix then fails
	// the launch-refusal row (TestTheLoadProbeRefusesALaunchThatFailsBeforeOhMyPi) at every run,
	// not only under a runner whose TMPDIR happens to hold such a character.
	prefixDir := filepath.Join(dir, "q dir", "it's")
	if err := os.MkdirAll(prefixDir, 0o755); err != nil {
		t.Fatalf("make the launch-prefix directory: %v", err)
	}
	prefix := filepath.Join(prefixDir, "launch-prefix")
	if err := execfile.Write(prefix, []byte("#!/bin/sh\necho prefixed >"+shellprefix.Literal(filepath.Join(dir, "prefixed"))+"\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("write the launch prefix: %v", err)
	}
	return fakeOmp{dir: dir, path: path, prefix: prefix, invocation: shellprefix.Word(path)}
}

func (f fakeOmp) attempts(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "count"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the attempt count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse the attempt count: %v", err)
	}
	return n
}

func (f fakeOmp) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// markedAt is when the fake, or its launch prefix, last wrote the file name: the file's
// modification time, which the kernel stamps as the stand-in writes it, so a stand-in's marks are
// read on no clock of the stand-in's own and no process it would have to start.
func (f fakeOmp) markedAt(t *testing.T, name string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("read when %s was written: %v", name, err)
	}
	return info.ModTime()
}

// gateUnder is a gate over a pane environment whose HOME holds a pi-legion manifest declaring
// legion (raw JSON) and a pi-envoy manifest beside it, each installed as `omp plugin install`
// installs it — the profile's package directory a link to the unpacked plugin — run through f with
// f's launch prefix, and a log that records what it said. Its attempt budget is answerBudget, the
// image probe rig's, so an attempt that answers races no clock: one that answers and then hangs
// is cut only after its answer. A case whose attempts hang before they answer anything has each
// attempt's budget pass once its stand-in hangs (hangsUnder).
func gateUnder(t *testing.T, f fakeOmp, legion string) (pluginGate, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	profile := filepath.Join(home, ".omp", "profiles", "gate")
	unpacked := filepath.Join(home, "unpacked-plugin")
	writeManifest(t, filepath.Join(unpacked, "package.json"), legion)
	link(t, unpacked, filepath.Dir(manifestAt(profile)))
	unpackedEnvoy := filepath.Join(home, "unpacked-envoy")
	writeEnvoyManifest(t, filepath.Join(unpackedEnvoy, "package.json"))
	link(t, unpackedEnvoy, filepath.Dir(envoyManifestAt(profile)))
	var logged bytes.Buffer
	return pluginGate{
		env: map[string]string{
			"HOME": home, "OMP_PROFILE": "gate", "PATH": os.Getenv("PATH"),
			"XDG_DATA_HOME": filepath.Join(home, "state", "home", ".local", "share"),
		},
		workDir:    t.TempDir(),
		invocation: f.invocation,
		prefix:     []string{f.prefix},
		timeout:    answerBudget,
		retry:      bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond},
		contract:   3,
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
	}, &logged
}

// Each probe attempt runs under the gate's own budget, slow_command_timeout_seconds: the context
// budget gives an attempt ends timeout after the call. The rows that cut a probe that hangs before
// it answers pass its budget at a fact of the run (hangsUnder), so no load can pass it before the
// probe has started. This row holds the clock budget gives that end from, and the rows whose fake
// answers and then hangs hold that clock's end on a running probe
// (TestTheLoadProbeRefusesAPluginOhMyPiDidNotLoad's no-then-hang, and the image probe's
// missing-then-hang): only answerBudget's clock cuts those.
func TestAProbeAttemptRunsUnderTheGatesTimeout(t *testing.T) {
	t.Parallel()
	const timeout = 7 * time.Second
	before := time.Now()
	attempt, cancel := pluginGate{timeout: timeout}.budget(context.Background())
	defer cancel()
	after := time.Now()

	deadline, ok := attempt.Deadline()
	if !ok || deadline.Before(before.Add(timeout)) || deadline.After(after.Add(timeout)) {
		t.Fatalf("the attempt's deadline is %s (one set: %t), want %s after the call, between %s and %s",
			deadline.Format(time.RFC3339Nano), ok, timeout, before.Add(timeout).Format(time.RFC3339Nano), after.Add(timeout).Format(time.RFC3339Nano))
	}
}

const (
	contractPrevious = `{"daemonApiVersion":2}`
	contractCurrent  = `{"daemonApiVersion":3}`
)

// A manifest that declares another contract — or none, or one that is not a number — is refused
// naming the manifest, the package version, and both numbers, before any Oh My Pi is run.
func TestTheGateRefusesAPluginOfAnotherContractBeforeRunningOhMyPi(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, legion, declared string
	}{
		{"another number", contractPrevious, "speaks daemon API contract 2"},
		{"no contract", `{}`, "speaks daemon API contract none"},
		{"no legion member", "", "speaks daemon API contract none"},
		{"a string", `{"daemonApiVersion":"1"}`, `speaks daemon API contract "1"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFakeOmp(t, "yes")
			gate, _ := gateUnder(t, f, testCase.legion)

			err := gate.verify(context.Background())

			if err == nil {
				t.Fatal("the gate passed a plugin of another contract")
			}
			manifest := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))
			for _, want := range []string{manifest, "(package 1.57.0)", testCase.declared, "this daemon requires 3", "OMP profile gate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if n := f.attempts(t); n != 0 {
				t.Errorf("Oh My Pi ran %d times for a plugin the manifest already refused", n)
			}
		})
	}
}

// A plugin released before contract 12 declares this daemon's number under the field the boot gate
// read then and the TypeScript daemon's 9 under the one it reads now: it is refused, naming the 9,
// however the old field reads.
func TestTheGateRefusesAPluginFromBeforeTheRename(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, `{"daemonApiVersion":9,"goDaemonApiVersion":3}`)

	err := gate.verify(context.Background())

	if err == nil {
		t.Fatal("the gate passed a plugin that declares this contract only under the field before the rename")
	}
	for _, want := range []string{"speaks daemon API contract 9", "this daemon requires 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := f.attempts(t); n != 0 {
		t.Errorf("Oh My Pi ran %d times after the contract gate refused", n)
	}
}

func TestTheGateRefusesAManifestItCannotRead(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, contractCurrent)
	manifest := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))
	if err := os.WriteFile(manifest, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt the manifest: %v", err)
	}
	missing := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "absent"))

	for name, env := range map[string]string{"absent": missing, "gate": manifest} {
		gate.env["OMP_PROFILE"] = name
		err := gate.verify(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pi-legion manifest at "+env+" could not be read") {
			t.Errorf("profile %s: err = %v, want the unreadable-manifest refusal naming %s", name, err, env)
		}
	}
	if n := f.attempts(t); n != 0 {
		t.Errorf("Oh My Pi ran %d times with no manifest to hold it to", n)
	}
}

// The load probe is the embedded probe extension handed to `omp models`, through the launch
// prefix, under the pane's environment and nothing of the daemon's own. A pass is logged in the
// words the e2e stages grep for, naming the Envoy plugin's interface and where it loaded from.
func TestTheLoadProbeRunsWhatAPaneRunsAndPassesOnTheLoadedMarker(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	f := newFakeOmp(t, "yes")
	gate, logged := gateUnder(t, f, contractCurrent)
	t.Setenv("LEGION_DAEMON_ONLY_SECRET", "the daemon's own")

	if err := gate.verify(context.Background()); err != nil {
		t.Fatalf("the gate refused a loaded plugin of its contract: %v", err)
	}

	envoy, _ := filepath.EvalSymlinks(filepath.Dir(envoyManifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))))
	for _, want := range []string{
		`msg="boot gate: pi-legion speaks this daemon's contract and loads with pi-envoy"`,
		"daemonApiVersion=3", "envoyInterface=1", `envoyFrom="file://` + envoy + `/dist/envoy.js?mtime=1"`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the gate's log lacks %q:\n%s", want, logged.String())
		}
	}

	if n := f.attempts(t); n != 1 {
		t.Fatalf("Oh My Pi ran %d times, want once", n)
	}
	argv := strings.Split(strings.TrimSuffix(f.read(t, "argv.1"), "\n"), "\n")
	if len(argv) != 4 || argv[0] != "models" || argv[1] != "--extension" || argv[3] != "--json" {
		t.Errorf("omp argv = %q, want models --extension <probe> --json", argv)
	}
	if got := f.read(t, "probe.1"); got != string(pluginLoadProbe) {
		t.Errorf("omp was handed %q, want the embedded load probe", got)
	}
	if _, err := os.Stat(argv[2]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the probe file %s outlived the probe: %v", argv[2], err)
	}
	if f.read(t, "prefixed") == "" {
		t.Error("the probe did not run through the launch prefix")
	}
	// Each variable is read whole, by name (envlisting): a listing also holds PWD and TMPDIR,
	// whose values can contain any text, and env can print any variable first.
	env := envlisting.Vars(f.read(t, "env.1"))
	for name, value := range gate.env {
		if got, set := env[name]; name != "PATH" && (!set || got != value) {
			t.Errorf("the probe's environment lacks the pane's %s=%s", name, value)
		}
	}
	if _, set := env["LEGION_DAEMON_ONLY_SECRET"]; set {
		t.Error("the probe ran with a variable of the daemon's own environment")
	}
	if info, err := os.Stat(gate.env["XDG_DATA_HOME"]); err != nil || !info.IsDir() {
		t.Errorf("the pane's XDG_DATA_HOME was not created before the probe: %v", err)
	}
}

// Exit 0 with LEGION_PLUGIN_LOADED=yes already written is an answer, whatever the launch started
// that is still running when Oh My Pi itself exits: a process it left behind (held &, which only a
// signal or the test's cleanup ends, standing in for whatever a real tmux pane's launch can leave
// running) holds the probe's output pipe open past procgroup.WaitDelay, but the probe already
// answered before that, and must neither be retried for it nor wait for it. Attempts is capped at
// 1 so a reintroduced regression (treating the lingering child as a transient non-answer) fails
// fast with this test's own message, instead of retrying until go test's own timeout ends the run,
// and the child must still be running when the gate returns. The child is held, so it never ends
// on a timer of its own: a gate that waited for it to close the pipe would not return while it is
// held, and fails at the test binary's deadline.
//
// Main's form gave the attempt a 10 s budget from before the probe started, so a gate that spent
// that long on an attempt whose Oh My Pi answered at once was cut off and refused. The bound stays,
// as main's 10 s (gateBound), on the gate's own time and no stand-in's. The fake Oh My Pi records
// when the kernel made its process (started: the probe's own process, which the probe's exec
// keeps) and the moment just before it exits (exited), and the span between them is the stand-in's
// own run, which a loaded machine slows. What is left of the call is the gate's: from the call to
// that start, and from Oh My Pi's exit to the pass. Of the second, procgroup.WaitDelay is not the
// gate's to spend: the probe's leader has exited, and os/exec waits that long for the child to
// close the pipe before it closes the pipe itself (procgroup.Configure sets it on every probe the
// gate runs, Cmd.Wait starts it once the leader is reaped, and the run returns only once it is
// over). Main's budget ended at that reap, so it never counted WaitDelay either, and the bound
// leaves it out. A gate whose probe stays on after Oh My Pi answered, or that holds the pass after
// the run, fails here; a loaded machine that slows the fake does not. With one attempt, a budget
// kill fails the test too, which gateUnder's budget (answerBudget, 20 s) keeps out of reach of the
// fake's own run on a loaded box. The child stays in the probe's process group, so this row holds
// the in-group side of a budget far away. An exit inside the budget whose pipe is held past it is
// TestAProbeThatExitsWithinItsBudgetIsNotCutOffByAChildHoldingItsPipePastIt's, and with the child
// left in the group TestAProbeThatExitsWithinItsBudgetIsNotCutOffByAChildInItsGroupHoldingItsPipePastIt's.
func TestTheLoadProbePassesOnTheFirstAttemptEvenWhenALingeringChildHoldsTheOutputPipe(t *testing.T) {
	t.Parallel()
	const gateBound = 10 * time.Second
	f := newFakeOmp(t, "yes-then-linger")
	gate, _ := gateUnder(t, f, contractCurrent)
	gate.retry.Attempts = 1
	t.Cleanup(func() { killPIDFile(t, filepath.Join(f.dir, "lingering.pid")) })

	called := testexec.SinceBoot(t)
	if err := gate.verify(context.Background()); err != nil {
		t.Fatalf("the gate refused a plugin that answered yes before a lingering child outlived it: %v", err)
	}
	returned := testexec.SinceBoot(t)

	if n := f.attempts(t); n != 1 {
		t.Fatalf("Oh My Pi ran %d times, want once: a lingering descendant is not a reason to retry an answer already given", n)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(f.read(t, "lingering.pid")))
	if err != nil {
		t.Fatalf("read the lingering child's PID: %v", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("the lingering child (PID %d) was gone when the gate returned (%v): the gate waited for it to close the probe's output pipe", pid, err)
	}
	began := testexec.StartedAfter(t, filepath.Join(f.dir, "started"), called)
	exited, err := testexec.ReadNowMark(filepath.Join(f.dir, "exited"))
	if err != nil {
		t.Fatalf("read when the fake Oh My Pi exited: %v", err)
	}
	held := procgroup.WaitDelayNow()
	if own := began + (returned - exited) - held; own > gateBound {
		t.Errorf("the gate spent %s of its own on an attempt whose Oh My Pi answered at once, past main's %s: %s from the call to Oh My Pi's start, and %s from its exit to the pass, less the %s os/exec holds the lingering child's pipe (procgroup.WaitDelay): the probe stayed on after Oh My Pi answered, or the gate held the pass after the run",
			own, gateBound, began, returned-exited, held)
	}
}

// A probe that exits on its own inside its budget was not cut off by it, however long
// procgroup.WaitDelay then holds the run open for a child the probe left holding its output pipe,
// and however much of that time passes after the budget: a run that read "timed out" from the
// deadline having passed would call this answer a budget kill, and a load probe whose Oh My Pi
// answered yes inside the last WaitDelay of slow_command_timeout_seconds would be retried, and
// refused once its retries ran out.
//
// The budget passes at a fact of the run, not at a time (budgetPassedOnceTheProbeIsGone): once the
// probe's process group is gone, which is after the probe has exited on its own and Go has reaped
// it. WaitDelay then holds the run open for the child, and the watch of the group sees the group
// gone inside that second unless a loaded machine holds the watch up for as long. A run that
// returned inside WaitDelay had nothing holding its pipe, and fails at once. Of the others, only a
// run in which the budget passed before the run gave its answer back is judged. A run of the other
// order made its judgment before the budget passed, so it shows nothing of a gate that reads a
// passed budget: the row runs the probe again, within its bound (answerBudget, testwait.Bounded),
// until a run of the right order comes. It fails naming the runs it made when none comes, and
// naming the group when the group is not gone by the end of the bound, since the budget then never
// passed at all. Main's form counted a 900 ms budget from before the shell started, which a loaded
// machine could spend before the shell had exited.
//
// gate.timeout is the budget as a clock reads it, 100 ms short of WaitDelay. The attempt's deadline
// reads as the budget's at a time (when the attempt began plus timeout) until the budget passes,
// and as the moment the budget passed from then on (attemptDeadline, testwait.StandInDeadline).
// No clock in run reads either where expire is set, but a gate that judged the end by the clock,
// the run's time against timeout or the time against the attempt's deadline, read before the
// command runs or after, would call this exit a timeout: the run returns past WaitDelay, after
// both.
//
// The child holding the pipe leaves the probe's process group (setsid) before the probe exits, so
// the group is gone once the probe is, and a stop sent to the group after that finds nothing to
// signal (ESRCH), which procgroup.Configure reads as no kill. The child runs until the row
// releases it once the run is over, or a kill ends it, never on a timer of its own, so its own end
// lets no run's pipe go early.
//
// With a child still in the group, one side is held by facts, the other by a far budget
// (answerBudget, 20 s). A budget that passes while the probe runs stops the whole group
// (TestABudgetThatPassesWhileTheProbeRunsStopsItsWholeGroup), and a probe that exits on its own
// while its budget is far away is no kill
// (TestTheLoadProbePassesOnTheFirstAttemptEvenWhenALingeringChildHoldsTheOutputPipe). A deadline
// that passes between the leader's reap and Wait's read of its context still meets exec's Cancel,
// whose stop the child in the group takes, so the run reports a kill, as procgroup.Configure's doc
// says. No fact outside os/exec orders that window, so a run in it is not judged by the row that
// holds an exit inside main's 900 ms budget with the child still in the group
// (TestAProbeThatExitsWithinItsBudgetIsNotCutOffByAChildInItsGroupHoldingItsPipePastIt), which
// tells the budget's stop inside Wait from a stop after the run by the pipe the stop found.
func TestAProbeThatExitsWithinItsBudgetIsNotCutOffByAChildHoldingItsPipePastIt(t *testing.T) {
	t.Parallel()
	gate := pluginGate{env: map[string]string{"PATH": os.Getenv("PATH")}, workDir: t.TempDir(), timeout: procgroup.WaitDelay - 100*time.Millisecond}
	within := testwait.Bounded(t, answerBudget, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		pidFile, leader, release := filepath.Join(dir, "lingering.pid"), filepath.Join(dir, "leader"), filepath.Join(dir, "release")
		var watch *groupWatch
		gate.expire, watch = budgetPassedOnceTheProbeIsGone(leader, gate.timeout, until)

		r, err := gate.run(context.Background(), `echo $$ >"$2"; echo answered >&2; setsid sh -c 'echo $$ >"$0"; while [ ! -e "$1" ] && kill -0 "$2" 2>/dev/null; do sleep 0.01; done' "$1" "$3" "$4" & while [ ! -s "$1" ]; do sleep 0.01; done; exit 0`, pidFile, leader, release, strconv.Itoa(os.Getpid()))

		<-watch.done
		// The run is over: its child is released, and killed in case the release does not reach it.
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Errorf("release run %d's child: %v", runs, err)
		}
		killPIDFile(t, pidFile)
		if !watch.gone {
			t.Fatalf("run %d: the probe's process group was not gone by the end of the row's %s bound, so its budget never passed (run = %v, exit %d, timed out %t, stderr %q after %s)",
				runs, within, err, r.exit, r.timedOut, r.stderr, r.elapsed)
		}
		if r.elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d = %v returned after %s, inside WaitDelay (%s): nothing held the probe's output pipe past its exit, so this run proves nothing", runs, err, r.elapsed, procgroup.WaitDelay)
		}
		if !watch.passedFirst {
			if time.Now().Before(until) {
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the attempt's budget passed only after the run gave its answer back, so no run shows how the gate reads a passed budget",
				runs, within)
		}

		if err != nil {
			t.Fatalf("run %d = %v, want the probe's own exit", runs, err)
		}
		if r.timedOut || r.exit != 0 || !strings.Contains(r.stderr, "answered") {
			t.Errorf("run %d = exit %d, timed out %t, stderr %q after %s; want exit 0, not timed out, and the answer: the probe exited on its own before its budget passed",
				runs, r.exit, r.timedOut, r.stderr, r.elapsed)
		}
		return
	}
}

// The same exit inside its budget, with the child that holds the probe's output pipe left in the
// probe's process group, as main's form had it, and under main's budget: 900 ms on the gate's own
// clock, 100 ms short of WaitDelay, so the run, which WaitDelay holds open past the leader's reap,
// always returns past it. A gate that, once its budget has passed, stops what is left of the group
// after the run and reads a stop that found a member as a kill would call this exit a budget kill.
// The row above cannot see that gate: its child has left the group, which is empty by then.
//
// Main's form judged every run, and a loaded machine can reap the probe's leader past 900 ms. The
// budget then passes inside Wait, procgroup.Configure's Cancel stops the group, the child takes its
// SIGTERM, and a correct gate reports the kill it made. The row tells that stop from one after the
// run by where it found the probe's output pipe, which the probe's child, a testkernel.Child,
// records as that type's doc says:
//   - EPIPE: no reader was left, so the stop came after the run, and the row fails.
//   - the byte went through: os/exec still read the pipe, so the stop was the budget's own, inside
//     Wait, which a loaded machine brought on. That run is not judged, and the row runs the probe
//     again, within its bound (answerBudget, testwait.Bounded), until a run is judged.
//   - no record, and the child gone: the grace's SIGKILL, which only the budget's own stop sends,
//     ended it before its trap wrote. Not judged either.
//
// A run in which no SIGTERM reached the child by the run's return is judged as main judged it: the
// probe's own exit, no kill, the answer. That none reached it is a fact of the run too, the child's
// own beat past the run's return, not a record missing by some time. A run whose budget passed
// while the leader still ran, before the child had set its traps, is not judged.
func TestAProbeThatExitsWithinItsBudgetIsNotCutOffByAChildInItsGroupHoldingItsPipePastIt(t *testing.T) {
	t.Parallel()
	gate := pluginGate{env: map[string]string{"PATH": os.Getenv("PATH")}, workDir: t.TempDir(), timeout: procgroup.WaitDelay - 100*time.Millisecond}
	probe := testkernel.ChildCommand("echo answered >&2")
	within := testwait.Bounded(t, answerBudget, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		child := testkernel.NewChild(t)

		r, err := gate.run(context.Background(), probe, child.Args()...)
		returned := testexec.SinceBoot(t)

		seen := child.After(t, returned, until)
		// The run is read: its child is released, and killed in case the release does not reach it.
		child.Release(t)
		if seen.AfterRun {
			t.Fatalf("run %d: the gate stopped the probe's process group after its run: the child the probe left in its group took a SIGTERM (its trap ran %s from the run's return), and its write to the probe's output pipe answered EPIPE: os/exec had already closed the pipe, which it does once WaitDelay runs out, and the budget's own stop comes only before that (run = %v, exit %d, timed out %t, stderr %q after %s)",
				runs, seen.At-returned, err, r.exit, r.timedOut, r.stderr, r.elapsed)
		}
		if !seen.None {
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: %s (run = %v, exit %d, timed out %t, after %s)", runs, seen.Unjudged, err, r.exit, r.timedOut, r.elapsed)
				continue
			}
			t.Fatalf("no run of %d over %s was judged: in the last, %s (run = %v, exit %d, timed out %t, stderr %q after %s)",
				runs, within, seen.Unjudged, err, r.exit, r.timedOut, r.stderr, r.elapsed)
		}

		if err != nil {
			t.Fatalf("run %d = %v, want the probe's own exit", runs, err)
		}
		if r.elapsed < gate.timeout {
			t.Fatalf("run %d returned after %s, inside its %s budget: nothing held the pipe past the deadline, so this run proves nothing", runs, r.elapsed, gate.timeout)
		}
		if r.timedOut || r.exit != 0 || !strings.Contains(r.stderr, "answered") {
			t.Errorf("run %d = exit %d, timed out %t, stderr %q after %s; want exit 0, not timed out, and the answer: the probe exited on its own inside its %s budget, and no stop reached its child by the run's return",
				runs, r.exit, r.timedOut, r.stderr, r.elapsed, gate.timeout)
		}
		return
	}
}

// cutRunMargin is what the rows bounded by answerBudget leave of the test binary's deadline
// (testwait.Bounded): the time a run the bound cuts takes to return, its stop's grace and
// WaitDelay, and a second for the row to fail in.
const cutRunMargin = procgroup.StopGrace + procgroup.WaitDelay + time.Second

// budgetPassedOnceTheProbeIsGone is an expire (pluginGate.expire) whose attempt's budget passes as
// the gate's own budget does (testwait.StandInDeadline: its Err and its cause
// context.DeadlineExceeded) once the probe's process group is gone. The probe's shell writes its
// own pid, its group's id (procgroup.Configure), to leader as its first act, and kill(-pid, 0)
// answers ESRCH once no member of that group is left: after the probe has exited on its own and Go
// has reaped it, which Wait does before it starts the WaitDelay a child holding the pipe keeps the
// run open for. Until its budget passes, the attempt's deadline reads as the real budget's would,
// timeout after the attempt began (attemptDeadline).
//
// What it saw of the attempt is in the groupWatch it returns, for one attempt. The cancel the run
// calls once it has its answer records whether the budget had passed by then (passedFirst), before
// it ends the attempt itself. The watch goes on past the run's return, so a group that goes late
// is told from one that never goes: done closes once the group is gone (gone), or once until, the
// end of the row's bound, has passed without it. The watch then ends the attempt as a cancel, not
// as its budget, so a run still going then reads no budget passed.
func budgetPassedOnceTheProbeIsGone(leader string, timeout time.Duration, until time.Time) (func(context.Context) (context.Context, context.CancelFunc), *groupWatch) {
	watch := &groupWatch{done: make(chan struct{})}
	gone := func() bool {
		pid := pidIn(leader)
		return pid > 0 && errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
	}
	return func(ctx context.Context) (context.Context, context.CancelFunc) {
		run, pass, cancel := testwait.StandInDeadline(ctx)
		go func() {
			defer close(watch.done)
			if watch.gone = testwait.Holds(time.Until(until), testwait.Poll, gone); watch.gone {
				pass(context.DeadlineExceeded)
				return
			}
			cancel()
		}()
		return attemptDeadline{Context: run, budget: time.Now().Add(timeout)}, func() {
			watch.passedFirst = errors.Is(run.Err(), context.DeadlineExceeded)
			cancel()
		}
	}, watch
}

// groupWatch is what budgetPassedOnceTheProbeIsGone saw of one attempt. passedFirst is written by
// the attempt's cancel, on the goroutine that runs the probe; gone is written before done closes.
type groupWatch struct {
	done        chan struct{}
	gone        bool
	passedFirst bool
}

// attemptDeadline is an attempt whose deadline reads as budget, the moment the real budget of
// context.WithTimeout would pass, until the stand-in it wraps sets a deadline of its own: the moment
// the stand-in's budget passed.
type attemptDeadline struct {
	context.Context
	budget time.Time
}

func (a attemptDeadline) Deadline() (time.Time, bool) {
	if at, ok := a.Context.Deadline(); ok {
		return at, true
	}
	return a.budget, true
}

// killPIDFile kills the process named by the PID pidFile holds (pidIn), left running past a test's
// own assertions by design (a process standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	if pid := pidIn(pidFile); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// A budget that passes while the probe's leader still runs ends the attempt as a kill, and the
// stop reaches the probe's whole process group (procgroup.Configure): a child the leader started,
// left in the group and holding the output pipe, is gone after the run too, where a stop sent to
// the leader alone would leave it running. The budget passes at facts, not at a time: once the
// leader has started the child, while the leader waits for release. The row writes release only
// once it has seen the leader end, or at the end of its bound (answerBudget, testwait.Bounded)
// without that, so a run that no stop reached still returns, and the row fails on its answer. The
// child runs until a signal ends it or the row's cleanup writes held, never on a timer of its own,
// so within the row nothing but a stop ends it, however long the row waits for it.
func TestABudgetThatPassesWhileTheProbeRunsStopsItsWholeGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leader, child := filepath.Join(dir, "leader"), filepath.Join(dir, "child")
	release, held := filepath.Join(dir, "release"), filepath.Join(dir, "held")
	t.Cleanup(func() {
		_ = os.WriteFile(held, nil, 0o644)
		killPIDFile(t, child)
	})
	within := testwait.Bounded(t, answerBudget, cutRunMargin)
	until := time.Now().Add(within)
	gate := pluginGate{env: map[string]string{"PATH": os.Getenv("PATH")}, workDir: t.TempDir(), timeout: answerBudget}
	type watched struct{ passed, leaderEnded bool }
	saw := make(chan watched, 1)
	gate.expire = func(ctx context.Context) (context.Context, context.CancelFunc) {
		attempt, pass, cancel := testwait.StandInDeadline(ctx)
		go func() {
			var w watched
			if w.passed = testwait.Holds(time.Until(until), testwait.Poll, func() bool { return pidIn(leader) > 0 && pidIn(child) > 0 }); w.passed {
				pass(context.DeadlineExceeded)
				// The leader is the run's own child, which the run reaps once it has ended, so kill
				// answers ESRCH for it from then on: no /proc read, which this goroutine could not
				// fail the test on.
				pid := pidIn(leader)
				w.leaderEnded = testwait.Holds(time.Until(until), testwait.Poll, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
			}
			// Written whether or not the leader ended: a leader no stop reached exits on it.
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Errorf("write the release: %v", err)
			}
			saw <- w
		}()
		return attempt, cancel
	}

	r, err := gate.run(context.Background(), `echo $$ >"$1"; (while [ ! -e "$4" ] && kill -0 "$5" 2>/dev/null; do sleep 0.01; done) & echo $! >"$2"; while [ ! -e "$3" ] && kill -0 "$5" 2>/dev/null; do sleep 0.01; done; exit 0`, leader, child, release, held, strconv.Itoa(os.Getpid()))
	w := <-saw

	if !w.passed {
		t.Fatalf("the probe did not start its child within the row's %s bound, so its budget never passed (run = %v, exit %d, stderr %q after %s)", within, err, r.exit, r.stderr, r.elapsed)
	}
	if err != nil {
		t.Fatalf("run = %v, want the budget's kill", err)
	}
	if !r.timedOut {
		t.Fatalf("run = exit %d, timed out false, stderr %q after %s, the leader seen to end before its release: %t; want a kill: the budget passed while the probe ran",
			r.exit, r.stderr, r.elapsed, w.leaderEnded)
	}
	pid := pidIn(child)
	if !testwait.Holds(time.Until(until), testwait.Poll, func() bool { return testkernel.ProcessEnded(t, pid) }) {
		t.Errorf("the child the probe left in its group (PID %d) still ran at the end of the row's %s bound, after the run's kill: the budget's stop did not reach the group", pid, within)
	}
}

// pidIn is the PID a probe's shell wrote to path, once the whole line is there, and 0 before.
func pidIn(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "\n") {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// The plugin a pane loads must be the one whose manifest the contract probe read. A launch prefix
// that sets its own OMP_PROFILE — or a dotenv file Oh My Pi reads — can put a pane's Oh My Pi on
// another plugin root than the pane environment names, and the contract probe would have vouched
// for a plugin no pane runs.
func TestTheLoadProbeRefusesAPluginLoadedFromAnotherRoot(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		step string
		want []string
	}{
		{"yes-elsewhere", []string{
			"pi-legion loads in a pane from ", filepath.Join("elsewhere", "package.json"),
			"but the manifest this gate held to daemon API contract 3 is ",
			"OMP profile gate", "launch prefix",
		}},
		{"yes-unowned", []string{"no @sjawhar/pi-legion package.json above", filepath.Join("unowned", "legion.js")}},
	} {
		t.Run(testCase.step, func(t *testing.T) {
			f := newFakeOmp(t, testCase.step)
			gate, _ := gateUnder(t, f, contractCurrent)
			writeManifest(t, filepath.Join(gate.env["HOME"], "elsewhere", "package.json"), contractCurrent)

			err := gate.verify(context.Background())

			if err == nil {
				t.Fatal("the gate passed a plugin loaded from a root it did not read")
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if n := f.attempts(t); n != 1 {
				t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
			}
		})
	}
}

// A probe that ran and did not see the plugin load is an answer, not a failure to retry: the
// plugin is installed and disabled, or not registered. no-then-hang answers and then hangs, and
// only the gate's budget ends it. Each row runs under testwait.Context, so a budget that never
// fires fails the row by name before the test binary's deadline, and does not hang the binary.
func TestTheLoadProbeRefusesAPluginOhMyPiDidNotLoad(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"no", "silent", "no-then-hang"} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOmp(t, step)
			gate, _ := gateUnder(t, f, contractCurrent)

			err := gate.verify(testwait.Context(t))

			want := "pi-legion 1.57.0 is installed but not loaded by omp (disabled or unregistered): run OMP_PROFILE=gate omp plugin list"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if n := f.attempts(t); n != 1 {
				t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
			}
		})
	}
}

// A pi-legion loaded with no pi-envoy has nothing to claim its role through: the gate refuses,
// naming the pi-envoy release to install into the pane's profile.
func TestTheLoadProbeRefusesAPluginWithoutTheEnvoyPlugin(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes-no-envoy")
	gate, _ := gateUnder(t, f, contractCurrent)

	err := gate.verify(context.Background())

	want := "pi-legion 1.57.0 is loaded in a pane of OMP profile gate, but no pi-envoy is: install the @sjawhar/pi-envoy release built from this daemon's commit into OMP profile gate"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if n := f.attempts(t); n != 1 {
		t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
	}
}

// A pi-envoy publishing another interface version than the pi-legion speaks is a pair released
// from different commits: the gate refuses, naming both versions, where pi-envoy loaded from, and
// the remedy of installing both releases.
func TestTheLoadProbeRefusesAPluginAtAnotherEnvoyInterface(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes-envoy-mismatch")
	gate, _ := gateUnder(t, f, contractCurrent)

	err := gate.verify(context.Background())

	envoy, _ := filepath.EvalSymlinks(filepath.Dir(envoyManifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))))
	want := "pi-envoy at file://" + envoy + "/dist/envoy.js?mtime=1 publishes plugin interface 2; pi-legion speaks 1: install both releases built from this daemon's commit into OMP profile gate"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if n := f.attempts(t); n != 1 {
		t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
	}
}

// A pane that still loads the pre-split @sjawhar/pi-legion-envoy beside pi-legion would run two
// Legion entries against the same pane: the gate refuses, naming the uninstall under the pane's
// profile, before any word on pi-envoy.
func TestTheLoadProbeRefusesAPluginBesideTheOldPackage(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes-legacy")
	gate, _ := gateUnder(t, f, contractCurrent)

	err := gate.verify(context.Background())

	want := "a pane of OMP profile gate still loads @sjawhar/pi-legion-envoy from file://" + gate.env["HOME"] + "/legacy/dist/legion.js?mtime=1: run `OMP_PROFILE=gate omp plugin uninstall @sjawhar/pi-legion-envoy`"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if n := f.attempts(t); n != 1 {
		t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
	}
}

// The pi-envoy a pane loads is held to a manifest of its name, wherever that is: a pane's lane
// reads no pi-envoy manifest of its own to hold it to, so one loaded from another root passes,
// and one no @sjawhar/pi-envoy manifest owns is refused.
func TestTheLoadProbeHoldsTheEnvoyPluginToAManifestOfItsName(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		owned bool
		want  string
	}{
		{"an owning manifest elsewhere", true, ""},
		{"no owning manifest", false, "no @sjawhar/pi-envoy package.json above"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFakeOmp(t, "yes-envoy-elsewhere")
			gate, _ := gateUnder(t, f, contractCurrent)
			if testCase.owned {
				writeEnvoyManifest(t, filepath.Join(gate.env["HOME"], "envoy-elsewhere", "package.json"))
			}

			err := gate.verify(context.Background())

			if testCase.want == "" && err != nil {
				t.Fatalf("the gate refused a pi-envoy an @sjawhar/pi-envoy manifest owns: %v", err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want) || !strings.Contains(err.Error(), filepath.Join("envoy-elsewhere", "dist", "envoy.js"))) {
				t.Fatalf("err = %v, want it to say %q and name the file", err, testCase.want)
			}
		})
	}
}

// The refusals come in an order that sends the operator to the first remedy: with no pi-envoy
// loaded, the skills it ships are missing too, and the gate names the missing plugin, never the
// missing skill.
func TestTheGateRefusesAMissingEnvoyBeforeTheMissingSkills(t *testing.T) {
	t.Parallel()
	f := newFakeOmp(t, "yes-no-envoy")
	gate, _ := gateUnder(t, f, contractCurrent)
	gate.roleReferences = promptrefs.New()
	gate.roleReferences.Text("roles/implementer.md", []byte("Read skill://dispatch before you file anything.\n"))

	err := gate.verify(context.Background())

	if err == nil || !strings.Contains(err.Error(), "but no pi-envoy is: install the @sjawhar/pi-envoy release") {
		t.Fatalf("err = %v, want the missing-pi-envoy refusal", err)
	}
	if strings.Contains(err.Error(), "dispatch") {
		t.Errorf("err = %v, which names the skill dispatch, a remedy behind the missing plugin's", err)
	}
	if skills := f.read(t, "env.1"); envlisting.Vars(skills)["LEGION_PROMPT_SKILLS"] != "dispatch" {
		t.Errorf("the load probe was not asked for the skill dispatch:\n%s", skills)
	}
}

// A launch that fails before Oh My Pi says anything — the launch prefix refusing, a binary that is
// not Oh My Pi — is its own refusal, with the command and what it printed. One a signal ended names
// the signal where an exit names its status: it has none, and read as one it was `(exit -1)`.
func TestTheLoadProbeRefusesALaunchThatFailsBeforeOhMyPi(t *testing.T) {
	t.Parallel()
	for step, ending := range map[string]string{"denied": "(exit 3)", "denied-then-killed": "(signal: killed)"} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOmp(t, step)
			gate, _ := gateUnder(t, f, contractCurrent)

			err := gate.verify(context.Background())

			if err == nil {
				t.Fatal("the gate passed a launch that failed")
			}
			// The refusal names the launch command as the prefix program's base name and the omp
			// invocation, the one shell word the gate runs (shellprefix.Word of its path, so a
			// directory with a space or a quote stays one word), rendered as the refusal renders it
			// (%q). The prefix's directory is not part of what the refusal shows.
			for _, want := range []string{"OMP launch probe failed " + ending + " for launch command", strconv.Quote(filepath.Base(f.prefix) + " " + f.invocation), "secrets: ANTHROPIC_API_KEY was denied"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if got := strings.TrimSpace(f.read(t, "prefixed")); got != "prefixed" {
				t.Errorf("the launch prefix wrote %q, want %q", got, "prefixed")
			}
			if n := f.attempts(t); n != 1 {
				t.Errorf("Oh My Pi ran %d times for a definitive failure, want once", n)
			}
		})
	}
}

// A launch prefix with arguments is named by its program and how many arguments follow, and a
// prefix that fails echoing its words has them taken out of the stderr the refusal quotes: its
// program's path, an argument, and the value alone of a NAME=value argument, each wherever it
// stands. The argument and the value are made-up credentials, as `env NAME=<token>` or a proxy
// URL with a password would hand one.
func TestTheLoadProbeQuotesNoWordOfTheLaunchPrefix(t *testing.T) {
	t.Parallel()
	const argument, value = "madeUpPrefixArgument0123", "madeUpPrefixValue0123"
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, contractCurrent)
	echoing := filepath.Join(t.TempDir(), "echoing-prefix")
	script := "#!/bin/sh\necho \"$0: proxy refused $1, and again:$1; token ${2#*=} rejected\" >&2\nexit 3\n"
	if err := execfile.Write(echoing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gate.prefix = []string{echoing, argument, "OMP_TOKEN=" + value}

	err := gate.verify(context.Background())

	if err == nil {
		t.Fatal("the gate passed a launch whose prefix failed")
	}
	for _, want := range []string{"for launch command " + strconv.Quote("echoing-prefix (2 arguments, not shown) "+f.invocation),
		"<prefix word 0>: proxy refused <prefix word 1>, and again:<prefix word 1>; token <prefix word 2> rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	for _, secret := range []string{argument, value} {
		for i := 0; i+10 <= len(secret); i++ {
			if strings.Contains(err.Error(), secret[i:i+10]) {
				t.Fatalf("the refusal %v holds %q of the prefix's words", err, secret[i:i+10])
			}
		}
	}
}

// Oh My Pi dying after it loaded the plugin, with an exit of its own or by a signal (the kernel's
// out-of-memory kill), says nothing about the plugin: the gate waits it out and asks again, as it
// does a probe the budget cut off before it answered (TestTheLoadProbeGivesUpOnAProbeCutOffEveryTime).
// A pass slower than a second or two passes on the first attempt that answers, as it does on a
// loaded machine. Each attempt runs under gateUnder's budget (answerBudget, 20 s) on the gate's
// own clock, which no attempt of the fake comes near: an attempt that takes longer is cut off, and
// the row fails, on the gate's refusal or on an attempt past the three it asks for.
func TestTheLoadProbeRetriesWhatSaysNothingAboutThePlugin(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"yes-then-die", "yes-then-killed"} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOmp(t, step, step, "yes-slowly", "yes")
			gate, logged := gateUnder(t, f, contractCurrent)

			if err := gate.verify(context.Background()); err != nil {
				t.Fatalf("the gate refused after transient failures: %v", err)
			}
			if n := f.attempts(t); n != 3 {
				t.Errorf("Oh My Pi ran %d times, want two transient failures and a pass", n)
			}
			if got := strings.Count(logged.String(), "failed transiently"); got != 2 {
				t.Errorf("the gate logged %d transient failures, want 2:\n%s", got, logged.String())
			}
		})
	}
}

// A probe the budget cuts off every time is asked again until the retry budget ends, and the
// refusal then names that budget and the cut. Each attempt's budget passes once its stand-in hangs
// (hangsUnder), so the fake ran each attempt the gate counted: its count, the refusal's and the
// transient lines' agree.
func TestTheLoadProbeGivesUpOnAProbeCutOffEveryTime(t *testing.T) {
	t.Parallel()
	plan := []string{"hang"}
	f := newFakeOmp(t, plan...)
	gate, logged := gateUnder(t, f, contractCurrent)
	hangsUnder(t, &gate, plan, 3, func(n int) string { return filepath.Join(f.dir, "hanging."+strconv.Itoa(n)) })

	err := gate.verify(context.Background())

	for _, want := range []string{"never completed within its retry budget (3 attempts)", "timed out after " + answerBudget.String()} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	}
	if n := f.attempts(t); n != 3 {
		t.Errorf("Oh My Pi ran %d times, want the 3 attempts of the retry budget", n)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 2 {
		t.Errorf("the gate logged %d transient failures, want 2 before it gave up:\n%s", got, logged.String())
	}
}

// A daemon stopped while its gate waits kills the probe it is running and starts no other. The
// probe hangs until a signal ends it (held), so a gate that left it running would return only at
// the end of its attempt's minute budget. The gate returns within gateStopsWithin of its start, on
// every run, less only the time its stand-in took to start once the gate's launch reached it: from
// the launch prefix's mark (prefixed, the prefix's first act, once the gate's shell has exec'd the
// launch) to the fake Oh My Pi's (count, written as its own first act). Nothing of the gate's runs
// between the two marks, which the kernel stamps as the stand-ins write them (fakeOmp.markedAt):
// only the stand-ins' own start, which a loaded machine can hold up. A run past gateStopsWithin
// that this time accounts for is not judged, and the row runs again on a gate of its own, until a
// judged run comes, within a minute. A gate slow before it execs the probe, or slow to stop it,
// spends that time outside the two marks, and fails the run it is slow in.
func TestTheLoadProbeStopsWithTheDaemon(t *testing.T) {
	t.Parallel()
	// The last run may spend its attempt's minute budget and cutRunMargin for the row to fail in.
	within := testwait.Bounded(t, time.Minute, time.Minute+cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		f := newFakeOmp(t, "hang")
		gate, _ := gateUnder(t, f, contractCurrent)
		gate.timeout = time.Minute
		ctx, cancel := context.WithCancel(context.Background())
		// The cancellation this test is about is one that lands on a probe in flight, so it waits
		// for the probe to be running rather than for a span: a fixed delay on a loaded machine
		// fires before the probe starts, and the gate then cancels having run nothing. It closes
		// watched once it has cancelled.
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			defer cancel()
			// Read here rather than through attempts: a helper that fails the test belongs to the
			// test's own goroutine, and this one only needs to know the probe started.
			testwait.Holds(30*time.Second, testwait.Poll, func() bool {
				raw, err := os.ReadFile(filepath.Join(f.dir, "count"))
				return err == nil && strings.TrimSpace(string(raw)) == "1"
			})
		}()

		started := time.Now()
		err := gate.verify(ctx)
		elapsed := time.Since(started)
		<-watched

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d: err = %v, want the daemon's cancellation", runs, err)
		}
		if n := f.attempts(t); n != 1 {
			t.Errorf("run %d: Oh My Pi ran %d times, want the one the stop interrupted", runs, n)
		}
		if t.Failed() || elapsed <= gateStopsWithin {
			return
		}
		late := f.markedAt(t, "count").Sub(f.markedAt(t, "prefixed"))
		if elapsed-late > gateStopsWithin {
			t.Errorf("run %d: the gate took %s to stop, past %s, and its stand-in started %s after the gate's launch reached it, which leaves %s of the gate's own: the gate was slow to start its probe, or the daemon's stop did not end the probe in time",
				runs, elapsed, gateStopsWithin, late, elapsed-late)
			return
		}
		if time.Now().Before(until) {
			t.Logf("run %d is not judged: the gate took %s to stop, past %s, and its stand-in started %s after the gate's launch reached it, which accounts for the overrun", runs, elapsed, gateStopsWithin, late)
			continue
		}
		t.Fatalf("no judged run in %d runs over %s: the last took %s to stop, past %s, and its stand-in started %s after the gate's launch reached it, which accounts for the overrun", runs, within, elapsed, gateStopsWithin, late)
	}
}

// gateStopsWithin is how soon after its start a gate stopped while its probe hangs returns: the
// probe's start, the stop's SIGTERM, which the hanging stand-in does not handle, and the run's
// return, well inside five seconds. A stop that reached the probe late, or not at all, ends past it.
const gateStopsWithin = 5 * time.Second

// The gate runs under the environment a pane gets from the daemon's own (HOME and OMP_PROFILE here)
// before the daemon boots: a refusal records no boot, and a daemon stopped while the gate waits
// stops cleanly, having served nothing. The plugin is held to this daemon's own number, so the
// contract below it — the one the previous release pins — is refused like any other. The stop
// lands once the gate's first probe hangs, a fact of the run, and the run must return before that
// probe's attempt budget (slow_command_timeout_seconds) would have ended it: the probe hangs until
// a signal ends it (held), so a gate that left it running would return only then.
func TestRunGatesThePluginUnderThePaneEnvironmentBeforeItBoots(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	// The daemon holds the plugin to its own DaemonAPIVersion.
	thisDaemons := `{"daemonApiVersion":` + strconv.Itoa(api.DaemonAPIVersion()) + `}`
	previous := strconv.Itoa(api.DaemonAPIVersion() - 1)
	for _, testCase := range []struct {
		name, legion, step, want string
	}{
		{"another contract", `{"daemonApiVersion":1}`, "yes", "speaks daemon API contract 1"},
		{"the contract below this one", `{"daemonApiVersion":` + previous + `}`, "yes", "speaks daemon API contract " + previous},
		{"not loaded", thisDaemons, "no", "is installed but not loaded by omp"},
		{"stopped while the probe runs", thisDaemons, "hang", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testConfig(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("OMP_PROFILE", "gate")
			writeManifest(t, manifestAt(filepath.Join(home, ".omp", "profiles", "gate")), testCase.legion)
			f := newFakeOmp(t, testCase.step)
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.runtime = nil
			o.getenv = func(name string) string {
				if name == "LEGION_OMP_PATH" {
					return f.path
				}
				return ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopped := make(chan bool, 1)
			bound := testwait.Bounded(t, answerBudget, cutRunMargin)
			if testCase.want == "" {
				go func() {
					defer cancel()
					stopped <- testwait.Holds(bound, testwait.Poll, func() bool {
						_, err := os.Stat(filepath.Join(f.dir, "hanging.1"))
						return err == nil
					})
				}()
			}

			started := time.Now()
			err := run(ctx, cfg, quietLogger(), o)
			elapsed := time.Since(started)

			if testCase.want == "" {
				if !<-stopped {
					t.Fatalf("the gate's probe did not hang within %s, so the stop landed on no probe (run = %v)", bound, err)
				}
				if err != nil {
					t.Fatalf("run stopped during its gate = %v, want a clean stop", err)
				}
				if elapsed >= cfg.SlowCommandTimeout {
					t.Fatalf("run took %s to stop, its probe's whole %s attempt budget: the stop did not end the probe", elapsed, cfg.SlowCommandTimeout)
				}
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("run = %v, want the gate's refusal naming %q", err, testCase.want)
			}
			if count, _ := boots(t, cfg); count != 0 {
				t.Fatalf("boots after a start the gate did not pass = %d, want 0", count)
			}
		})
	}
}

// The tmux gate asks the load probe for the skills and task agents the daemon's role prompts name,
// beside the plugin's own: legion-controller is loaded by roles/controller-root.md alone.
func TestTheTmuxGateAsksForWhatTheRolePromptsName(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	cfg := testConfig(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OMP_PROFILE", "gate")
	writeManifest(t, manifestAt(filepath.Join(home, ".omp", "profiles", "gate")),
		`{"daemonApiVersion":`+strconv.Itoa(api.DaemonAPIVersion())+`}`)
	f := newFakeOmp(t, "no")
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.runtime = nil
	o.getenv = func(name string) string {
		if name == "LEGION_OMP_PATH" {
			return f.path
		}
		return ""
	}

	if err := run(context.Background(), cfg, quietLogger(), o); err == nil {
		t.Fatal("run passed the gate, want the fake's not-loaded refusal")
	}
	var skills string
	for _, line := range strings.Split(f.read(t, "env.1"), "\n") {
		if value, ok := strings.CutPrefix(line, "LEGION_PROMPT_SKILLS="); ok {
			skills = value
		}
	}
	if !slices.Contains(strings.Split(skills, ","), "legion-controller") {
		t.Errorf("the load probe was asked for the skills %q, want legion-controller among them", skills)
	}
}

// A pane's Oh My Pi gets each provider key from its shim, not from the daemon's environment, so the
// gate probes with the keys the same way: a task agent whose model's only key is a provider key
// resolves under the gate as it will in a pane. The daemon's own value of the variable, which no
// pane carries, never reaches the probe.
func TestTheGateProbesWithTheProviderKeysAPaneGets(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	cfg := testConfig(t)
	cfg.ProviderKeys = []config.ProviderKey{{Env: "GEMINI_API_KEY", Secret: "GEMINI_API_KEY_TESTS"}}
	stub := t.TempDir()
	secrets := `#!/bin/sh
case "$2:$3" in
  GEMINI_API_KEY_TESTS:--no-request) echo '{"key":"GEMINI_API_KEY_TESTS","tier":"agent"}' ;;
  GEMINI_API_KEY_TESTS:--value) printf 'the-provider-key\n' ;;
  *) exit 99 ;;
esac
`
	if err := execfile.Write(filepath.Join(stub, "secrets"), []byte(secrets), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OMP_PROFILE", "gate")
	t.Setenv("PATH", stub+":"+os.Getenv("PATH"))
	t.Setenv("GEMINI_API_KEY", "the-daemons-own")
	writeManifest(t, manifestAt(filepath.Join(home, ".omp", "profiles", "gate")),
		`{"daemonApiVersion":`+strconv.Itoa(api.DaemonAPIVersion())+`}`)
	f := newFakeOmp(t, "no")
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.runtime = nil
	o.getenv = func(name string) string {
		if name == "LEGION_OMP_PATH" {
			return f.path
		}
		return ""
	}

	if err := run(context.Background(), cfg, quietLogger(), o); err == nil || !strings.Contains(err.Error(), "is installed but not loaded by omp") {
		t.Fatalf("run = %v, want the gate's refusal", err)
	}
	var got []string
	for _, line := range strings.Split(f.read(t, "env.1"), "\n") {
		if strings.HasPrefix(line, "GEMINI_API_KEY=") {
			got = append(got, line)
		}
	}
	if want := []string{"GEMINI_API_KEY=the-provider-key"}; !slices.Equal(got, want) {
		t.Errorf("the probe ran with %q, want %q: the provider key, as a pane's shim exports it", got, want)
	}
}

// The boot gate probes under the environment a pane gets, so it carries the operator's own spend
// attributes as the operator wrote them in `runtime.tmux.pane_env` and nothing more: the probe
// serves no issue and runs in no role, so the composition every launch goes through
// (runtime.SpendResourceAttributes with the zero spec, gateEnvironment) writes neither
// legion.issue nor legion.role, and the probe's minutes are billed under the deployment's own
// attributes rather than under some launch's issue.
func TestTheGateIsToldTheOperatorsSpendAttributesAlone(t *testing.T) {
	t.Parallel()
	// The live deployment's shape, with a placeholder person, as an operator writes it: the
	// project, the lane, the credential the gateway bills and the person who owns the spend.
	const operator = "acme.project=legion,acme.lane=legion-infra,acme.credential=gateway,acme.person=sami@example.com"
	block := &config.Tmux{PaneEnv: map[string]string{runtime.SpendAttributesVariable: operator}}
	env, err := gateEnvironment([]string{"HOME=" + t.TempDir()}, t.TempDir(), "", block)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := env[runtime.SpendAttributesVariable]
	if !ok || value != operator {
		t.Errorf("the gate is told %s = %q (set %v), want the operator's own %q", runtime.SpendAttributesVariable, value, ok, operator)
	}
	if strings.Contains(value, runtime.SpendIssueAttribute) || strings.Contains(value, runtime.SpendRoleAttribute) {
		t.Errorf("the gate is told %s = %q, want no %s and no %s: the probe serves no issue and runs in no role",
			runtime.SpendAttributesVariable, value, runtime.SpendIssueAttribute, runtime.SpendRoleAttribute)
	}
	// An operator who sets no attributes leaves the gate told none, rather than an empty variable
	// invented for it.
	bare, err := gateEnvironment([]string{"HOME=" + t.TempDir()}, t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := bare[runtime.SpendAttributesVariable]; ok {
		t.Errorf("a gate under no operator attributes is told %s = %q, want no such variable", runtime.SpendAttributesVariable, value)
	}
}
