package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// binary is an executable in dir that reads as a compiled program, as the real gh is: an ELF
// header, no `#!` line.
func binary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// currentJJ is what the worker image's jj prints for `jj --version`.
const currentJJ = "jj 0.45.1-sami.20260910-043938-bb5ffc8f23b1e2a9dac6fdf05b0540058a3a21d0"

// fakeJJ is an executable jj in dir whose `jj --version` prints version.
func fakeJJ(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "jj")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The jj the worker image pins merges two commits with more than one merge base as it merges any
// other two. m1 and m2 below are two merges of the same n commits, each of which changes f from
// the root's "c" to "b", so f is "b" on both sides and in every merge base, and a merge of m1 and
// m2 can only be a clean "b". That is the minimal case of jj-vcs/jj#6369, where jj merges such
// commits back to the root's "c" without a conflict to say so. The pinned jj must carry the port of
// jj-vcs/jj#10100, which fixes it, so this test is red at every pin that lacks that port.
func TestPinnedJJKeepsWhatEverySideOfAMergeHolds(t *testing.T) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("this test runs the jj the worker image pins, which CI's daemon-go job installs at ARG JJ_TOOL of packages/daemon/docker/worker.Dockerfile: %v", err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test reads its fixture's merge bases with a real git: %v", err)
	}
	home := t.TempDir()
	version := fixturegit.Isolated(t, home).Run(home, jj, "--version")
	t.Logf("%s is %s", jj, version)
	for _, tc := range []struct {
		name string
		n    int
	}{{"two merge bases", 2}, {"three merge bases", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			run := fixturegit.Isolated(t, t.TempDir())
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, "f"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A commit is named by its commit id, which in a colocated repository is git's id of the
			// same commit, so git merge-base reads the ids jj prints.
			commitID := func(revision string) string {
				t.Helper()
				return run.Out(repo, jj, "log", "-r", revision, "--no-graph", "-T", "commit_id")
			}
			init := fixturegit.JJInit(repo, true)
			init[0] = jj
			run.Run(repo, init...)
			write("c\n")
			run.Run(repo, jj, "commit", "-m", "root")
			root := commitID("@-")
			var bases []string
			for i := 1; i <= tc.n; i++ {
				run.Run(repo, jj, "new", root, "-m", fmt.Sprintf("x%d", i))
				write("b\n")
				run.Run(repo, jj, "new")
				bases = append(bases, commitID("@-"))
			}
			// Each merge keeps its description, so the next `jj new` leaves it in place rather than
			// abandoning it as an empty working-copy commit.
			merges := make([]string, 2)
			for i := range merges {
				run.Run(repo, slices.Concat([]string{jj, "new"}, bases, []string{"-m", fmt.Sprintf("m%d", i+1)})...)
				merges[i] = commitID("@")
			}
			listed := strings.Fields(run.Out(repo, git, "merge-base", "--all", merges[0], merges[1]))
			slices.Sort(listed)
			want := slices.Sorted(slices.Values(bases))
			if !slices.Equal(listed, want) {
				t.Fatalf("the fixture is wrong, not jj: git merge-base --all m1 m2 lists %v, want the %d commits x1 to x%d, %v", listed, tc.n, tc.n, want)
			}
			for i, merge := range merges {
				if held := run.Out(repo, jj, "file", "show", "-r", merge, "f"); held != "b\n" {
					t.Fatalf("the fixture is wrong, not jj: m%d holds %q in f, want \"b\\n\", as each of its parents does", i+1, held)
				}
			}
			run.Run(repo, jj, "new", merges[0], merges[1], "-m", "merge")
			state := run.Out(repo, jj, "log", "-r", "@", "--no-graph", "-T", `if(conflict, "conflict", "clean")`)
			held := run.Out(repo, jj, "file", "show", "-r", "@", "f")
			if state != "clean" || held != "b\n" {
				t.Errorf("%s merges m1 and m2, two merges of the same %d commits that each hold \"b\\n\" in f, into a %s commit holding %q in f; want a clean one holding \"b\\n\". "+
					"This is jj-vcs/jj#6369, and the pinned jj must carry the port of jj-vcs/jj#10100", version, tc.n, state, held)
			}
		})
	}
}

// Boot resolves the binaries Legion itself runs or hands a pane — git, jj and the real gh a pane's
// `legion gh` runs — once, each by its LEGION_<TOOL>_PATH override or on the daemon's PATH, and
// names every missing one with its override.
func TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool(t *testing.T) {
	onPath, elsewhere := t.TempDir(), t.TempDir()
	executable(t, onPath, "git")
	pinned := executable(t, elsewhere, "git")
	env := map[string]string{"PATH": onPath, "LEGION_GIT_PATH": pinned}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "jj (set LEGION_JJ_PATH to an absolute executable path)") ||
		!strings.Contains(err.Error(), "gh (set LEGION_GH_PATH to an absolute executable path)") || strings.Contains(err.Error(), "git (") {
		t.Fatalf("resolveTools with no jj or gh = %v, want only jj and gh named with their overrides", err)
	}
	fakeJJ(t, onPath, currentJJ)
	gh := executable(t, elsewhere, "gh")
	env["LEGION_GH_PATH"] = gh
	tools, err := resolveTools(lookup)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if tools["git"] != pinned || tools["jj"] != filepath.Join(onPath, "jj") || tools["gh"] != gh || len(tools) != 3 {
		t.Fatalf("resolveTools = %v, want the override for git and gh and PATH for jj", tools)
	}
	env["LEGION_JJ_PATH"] = "jj"
	if _, err := resolveTools(lookup); err == nil || !strings.Contains(err.Error(), "LEGION_JJ_PATH") {
		t.Fatalf("resolveTools with a relative override = %v, want it refused", err)
	}
}

// A configuration that puts a role on claude needs Claude Code, bun for its MCP server, and the
// built claude-envoy plugin: boot names every gap in one refusal, and takes each tool's override
// before PATH.
func TestResolveClaudeNamesEveryGap(t *testing.T) {
	onPath, plugin := t.TempDir(), t.TempDir()
	env := map[string]string{"PATH": onPath}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	resolve := func() (launchReads, error) { return resolveClaude(config.Config{}, lookup, nil) }
	_, err := resolve()
	want := "worker_backends runs a role on claude, and boot is missing bun (set LEGION_BUN_PATH to an absolute executable path), " +
		"claude (set LEGION_CLAUDE_PATH to an absolute executable path), the claude-envoy plugin (set LEGION_CLAUDE_PLUGIN_DIR to its package directory)"
	if err == nil || err.Error() != want {
		t.Fatalf("resolveClaude with nothing = %v\nwant %s", err, want)
	}
	executable(t, onPath, "bun")
	pinned := executable(t, t.TempDir(), "claude")
	env["LEGION_CLAUDE_PATH"], env["LEGION_CLAUDE_PLUGIN_DIR"] = pinned, plugin
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "holds no dist/envoy-channel.js: build the plugin") {
		t.Fatalf("resolveClaude with an unbuilt plugin = %v", err)
	}
	for _, file := range []string{"dist/envoy-channel.js", "legion/.claude-plugin/plugin.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(plugin, file)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	read.log(slog.New(slog.NewTextHandler(&logged, nil)))
	for _, want := range []string{"claude=" + pinned, "bun=" + filepath.Join(onPath, "bun"), "plugin=" + plugin} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("resolveClaude logged %q, want %s", logged.String(), want)
		}
	}
	if read.launcher.Marker() != " claude-bridge -- " || read.invocation != "" {
		t.Errorf("resolveClaude = marker %q, invocation %q", read.launcher.Marker(), read.invocation)
	}
	env["LEGION_CLAUDE_PLUGIN_DIR"] = "relative"
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "LEGION_CLAUDE_PLUGIN_DIR is not an absolute path") {
		t.Errorf("a relative plugin dir = %v", err)
	}
}

// The gh boot finds on PATH is every pane's real gh (LEGION_GH_PATH), which `legion gh` runs with
// the owner's gh directory. The real gh is a compiled program, so a script heading PATH is a
// wrapper — a machine's own gh shim, which routes to a token of its own, or Legion's own `legion
// gh` wrapper, which would run `legion gh` again — and boot refuses it, naming it and
// LEGION_GH_PATH, rather than taking the compiled gh behind it. LEGION_GH_PATH is the operator's
// own word, so a script it names is taken.
func TestResolveToolsRefusesAScriptAsTheRealGh(t *testing.T) {
	shims, bin := t.TempDir(), t.TempDir()
	binary(t, bin, "git")
	fakeJJ(t, bin, currentJJ)
	real := binary(t, bin, "gh")
	shim := filepath.Join(shims, "gh")
	if err := os.WriteFile(shim, []byte("#!/bin/bash\nexec knives gh -- \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PATH": shims + ":" + bin}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "the first gh on PATH, "+shim+", is a script") || !strings.Contains(err.Error(), "LEGION_GH_PATH") {
		t.Fatalf("resolveTools with a script first on PATH = %v, want it refused naming %s and LEGION_GH_PATH", err, shim)
	}
	env["PATH"] = bin + ":" + shims
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != real {
		t.Fatalf("resolveTools with the compiled gh first on PATH = %v, %v; want %s", tools, err, real)
	}
	env["PATH"], env["LEGION_GH_PATH"] = shims+":"+bin, shim
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != shim {
		t.Fatalf("resolveTools with LEGION_GH_PATH naming a script = %v, %v; want %s", tools, err, shim)
	}
}

// Provisioning relies on jj 0.38's configuration layout (minimumJJ), so boot refuses a jj older
// than 0.38, or one whose version it cannot read, naming what it found and LEGION_JJ_PATH, whether
// the jj came from the override or from PATH; 0.38 itself and later releases, a build's suffix
// included, pass.
func TestResolveToolsRefusesAJJOlderThanTheLayoutProvisioningReliesOn(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		override      bool
		refused       []string
	}{
		{"jj 0.37.0 on PATH", "jj 0.37.0", false, []string{"is 0.37.0, older than 0.38", "LEGION_JJ_PATH"}},
		{"jj 0.37.0 at the override", "jj 0.37.0-abc123", true, []string{"is 0.37.0-abc123, older than 0.38", "LEGION_JJ_PATH"}},
		{"a version it cannot read", "jujutsu, probably", false, []string{`printed "jujutsu, probably"`, "LEGION_JJ_PATH"}},
		{"jj 0.38.0", "jj 0.38.0", false, nil},
		{"the worker image's jj", currentJJ, false, nil},
		{"jj 1.0.0 at the override", "jj 1.0.0", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onPath, elsewhere := t.TempDir(), t.TempDir()
			executable(t, onPath, "git")
			binary(t, onPath, "gh")
			env := map[string]string{"PATH": onPath}
			if tc.override {
				env["LEGION_JJ_PATH"] = fakeJJ(t, elsewhere, tc.version)
			} else {
				fakeJJ(t, onPath, tc.version)
			}
			tools, err := resolveTools(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
			if tc.refused == nil {
				if err != nil {
					t.Fatalf("resolveTools with %s = %v, want it accepted", tc.version, err)
				}
				if tools["jj"] == "" {
					t.Fatalf("resolveTools with %s resolved no jj: %v", tc.version, tools)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveTools with %s = %v, want it refused", tc.version, tools)
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("resolveTools with %s = %v, want it to contain %q", tc.version, err, want)
				}
			}
		})
	}
}

// The jj --version boot runs starts without either raw NATS seed the daemon holds, as every
// command the daemon starts does (runtime.WithoutNATSSeeds).
func TestTheJJVersionCheckGetsNoNATSSeed(t *testing.T) {
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	jj := filepath.Join(dir, "jj")
	if err := os.WriteFile(jj, []byte("#!/bin/sh\nenv >"+dump+"\necho '"+currentJJ+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkJJVersion(jj); err != nil {
		t.Fatalf("checkJJVersion(%s) = %v, want it accepted", jj, err)
	}
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("jj --version never ran: %v", err)
	}
	vars := envlisting.Vars(string(seen))
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("jj --version's environment carries a NATS seed in %s:\n%s", name, seen)
	}
	if vars["PATH"] == "" {
		t.Errorf("jj --version's environment is not the daemon's (no PATH):\n%s", seen)
	}
}
