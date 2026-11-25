package podsafety

//ci:reads packages/daemon/docker/worker.Dockerfile TestTheWorkerImageShipsItsGhExtensionsWhereTheShimLinksFrom

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// Variables are two of the variables that place a pod's sessions, both reserved to the runtime,
// and nothing else: another, OMP_CONFIG_ROOT, is not set (the package doc says why), nothing else
// is set on a pod's agent, so nothing of a repository's .env is held off beyond those two, and no
// settings overlay of Legion's reaches PI_CONFIG_FILES.
func TestVariablesAreTheTwoThatPlaceSessions(t *testing.T) {
	want := []Variable{{Name: "PI_CONFIG_DIR", Reserved: placesSessions}, {Name: "OMP_SESSION_STORAGE", Reserved: placesSessions}}
	if got := Variables(); !slices.Equal(got, want) {
		t.Errorf("Variables() = %v, want %v", got, want)
	}
}

func envMap(environ []string) map[string]string {
	env := map[string]string{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}

// named is how many times environ names variable.
func named(environ []string, variable string) int {
	return len(slices.DeleteFunc(slices.Clone(environ), func(pair string) bool { return !strings.HasPrefix(pair, variable+"=") }))
}

// Apply leaves PI_CONFIG_FILES as the pod carries it and writes nothing under the state
// directory: Legion names no settings overlay of its own, so the overlays a role's Oh My Pi reads
// are the operator's own and the role's, which the runtime composed before the shim ran, and a
// repository's settings sit under those alone.
func TestApplyLeavesTheOverlaysThePodCarriesAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		operator []string
		want     string
		set      bool
	}{
		"the operator's overlays":  {operator: []string{"PI_CONFIG_FILES=/etc/op.yml:/etc/op2.yml"}, want: "/etc/op.yml:/etc/op2.yml", set: true},
		"an empty PI_CONFIG_FILES": {operator: []string{"PI_CONFIG_FILES="}, want: "", set: true},
		"no overlay of the pod's":  {},
	} {
		t.Run(name, func(t *testing.T) {
			env := Apply(append([]string{"HOME=/home/legion"}, tc.operator...))
			got, set := envMap(env)["PI_CONFIG_FILES"]
			if got != tc.want || set != tc.set {
				t.Errorf("PI_CONFIG_FILES = %q (set %v), want %q (set %v)", got, set, tc.want, tc.set)
			}
			if n := named(env, "PI_CONFIG_FILES"); n > 1 {
				t.Errorf("PI_CONFIG_FILES is named %d times in %q", n, env)
			}
		})
	}
}

// Each baseline variable is set only where the pod's environment leaves it unset or empty — the
// case in which Oh My Pi would fill it from a repository's .env — so a value the pod carries wins,
// the Sandbox runtime's session database among them, which it names before Apply runs (Apply does
// not know who set it; the Sandbox runtime is what refuses an operator's pod setting either,
// sandbox.CheckPod). Nothing beyond the two is set, PI_CONFIG_FILES included.
func TestApplySetsEachBaselineVariableOnlyWhereThePodLeavesItUnset(t *testing.T) {
	baseline := map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "file"}
	for name, tc := range map[string]struct {
		pod, want map[string]string
	}{
		"unset":                {map[string]string{}, baseline},
		"the pod's own value":  {map[string]string{"OMP_SESSION_STORAGE": "sql"}, map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql"}},
		"empty, as .env fills": {map[string]string{"OMP_SESSION_STORAGE": ""}, baseline},
		"another config root":  {map[string]string{"PI_CONFIG_DIR": ".config/omp"}, map[string]string{"PI_CONFIG_DIR": ".config/omp", "OMP_SESSION_STORAGE": "file"}},
		"the runtime's session database": {
			map[string]string{"OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
			map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var environ []string
			for variable, value := range tc.pod {
				environ = append(environ, variable+"="+value)
			}
			env := Apply(environ)
			got := envMap(env)
			for variable, want := range tc.want {
				if got[variable] != want {
					t.Errorf("%s = %q, want %q", variable, got[variable], want)
				}
				if n := named(env, variable); n != 1 {
					t.Errorf("%s is named %d times in %q", variable, n, env)
				}
			}
			for variable := range got {
				if _, pods := tc.pod[variable]; !pods && tc.want[variable] == "" {
					t.Errorf("Apply set %s=%q, which is neither the pod's nor the baseline's", variable, got[variable])
				}
			}
		})
	}
}

// EnsureStateHome makes the directory Oh My Pi resolves its state root to under XDG_STATE_HOME —
// `omp/profiles/<profile>` for the profile OMP_PROFILE (else PI_PROFILE) names, `omp` for the
// default profile — since Oh My Pi reads the variable only where that directory already exists;
// with no state home it makes nothing, it refuses a path it cannot make, naming it, and it refuses
// an environment under which Oh My Pi would root its state elsewhere than the directory it made,
// naming both.
func TestEnsureStateHomeMakesOhMyPisStateRootUnderTheStateHome(t *testing.T) {
	for name, tc := range map[string]struct {
		environ func(stateHome string) []string
		want    string
	}{
		"the image's profile": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "OMP_PROFILE=legion"} },
			"omp/profiles/legion",
		},
		"PI_PROFILE where OMP_PROFILE is undefined": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "PI_PROFILE=work"} },
			"omp/profiles/work",
		},
		"no profile": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "HOME=/home/legion"} },
			"omp",
		},
		"an empty OMP_PROFILE, the default profile over PI_PROFILE": {
			func(home string) []string {
				return []string{"XDG_STATE_HOME=" + home, "OMP_PROFILE=", "PI_PROFILE=work"}
			},
			"omp",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateHome := filepath.Join(t.TempDir(), "state", "tester")
			if err := EnsureStateHome(tc.environ(stateHome)); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(stateHome, tc.want))
			if err != nil {
				t.Fatalf("%s under the state home: %v", tc.want, err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want a directory at mode 0700", tc.want, info.Mode())
			}
		})
	}
	t.Run("no state home", func(t *testing.T) {
		dir := t.TempDir()
		for _, environ := range [][]string{{"OMP_PROFILE=legion", "HOME=" + dir}, {"XDG_STATE_HOME=", "OMP_PROFILE=legion", "HOME=" + dir}} {
			if err := EnsureStateHome(environ); err != nil {
				t.Fatalf("%q: %v", environ, err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("with no state home, EnsureStateHome made %v", entries)
		}
	})
	t.Run("a file where the directory should go", func(t *testing.T) {
		stateHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateHome, "omp"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "OMP_PROFILE=legion"})
		if err == nil || !strings.Contains(err.Error(), filepath.Join(stateHome, "omp", "profiles", "legion")) {
			t.Fatalf("EnsureStateHome = %v, want an error naming %s", err, filepath.Join(stateHome, "omp", "profiles", "legion"))
		}
	})
	t.Run("a profile Oh My Pi refuses", func(t *testing.T) {
		stateHome := t.TempDir()
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "OMP_PROFILE=../escape"})
		if err == nil || !strings.Contains(err.Error(), `"../escape"`) {
			t.Fatalf("EnsureStateHome = %v, want a refusal naming the profile", err)
		}
		if entries, _ := os.ReadDir(stateHome); len(entries) != 0 {
			t.Errorf("a refused profile made %v", entries)
		}
	})
	t.Run("an agent directory elsewhere turns the state home off", func(t *testing.T) {
		home, stateHome := t.TempDir(), t.TempDir()
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "HOME=" + home, "PI_CODING_AGENT_DIR=" + filepath.Join(home, "elsewhere")})
		made, read := filepath.Join(stateHome, "omp"), filepath.Join(home, ".omp")
		if err == nil || !strings.Contains(err.Error(), made) || !strings.Contains(err.Error(), read) {
			t.Fatalf("EnsureStateHome = %v, want a refusal naming %s and %s", err, made, read)
		}
	})
}

// EnsureScratch makes every directory the runtime tells a role to write what it writes without
// being told where — TMPDIR, XDG_CACHE_HOME, XDG_DATA_HOME, GOCACHE, GOMODCACHE and
// BUN_INSTALL_CACHE_DIR — with every parent, since `go` and `bun` make their own cache
// directories but nothing makes the temporary directory a role is told, and mktemp fails where it
// does not exist. A name the environment leaves unset or empty, or sets to a relative path, makes
// nothing; XDG_STATE_HOME is EnsureStateHome's alone, untouched here; and a directory that cannot
// be made is a refusal naming the variable and the path, so a role whose volume cannot be written
// never starts half-configured.
func TestEnsureScratchMakesEveryDirectoryTheRoleIsToldToWriteUnder(t *testing.T) {
	t.Run("every told directory, with its parents", func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "legion", "scratch", "tester")
		told := map[string]string{
			"TMPDIR":                filepath.Join(scratch, "tmp"),
			"XDG_CACHE_HOME":        filepath.Join(scratch, "cache"),
			"XDG_DATA_HOME":         filepath.Join(scratch, "data"),
			"GOCACHE":               filepath.Join(scratch, "go", "build"),
			"GOMODCACHE":            filepath.Join(scratch, "go", "mod"),
			"BUN_INSTALL_CACHE_DIR": filepath.Join(scratch, "bun", "install", "cache"),
		}
		stateHome := filepath.Join(scratch, "state")
		environ := []string{"XDG_STATE_HOME=" + stateHome, "XDG_CONFIG_HOME=/run/legion/config", "OMP_PROFILE=legion"}
		for name, dir := range told {
			environ = append(environ, name+"="+dir)
		}
		if err := EnsureScratch(environ); err != nil {
			t.Fatal(err)
		}
		for name, dir := range told {
			info, err := os.Stat(dir)
			if err != nil {
				t.Errorf("the directory %s names: %v", name, err)
				continue
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("%s names %s, which is %v, want a directory at mode 0700", name, dir, info.Mode())
			}
		}
		if _, err := os.Stat(stateHome); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the state home %s: %v, want it left to EnsureStateHome", stateHome, err)
		}
	})
	for name, environ := range map[string][]string{
		"unset": {"XDG_CONFIG_HOME=/run/legion/config", "OMP_PROFILE=legion"},
		"empty": {"TMPDIR=", "XDG_CACHE_HOME=", "XDG_DATA_HOME=", "GOCACHE=", "GOMODCACHE=", "BUN_INSTALL_CACHE_DIR="},
		"relative": {
			"TMPDIR=tmp", "XDG_CACHE_HOME=.cache", "XDG_DATA_HOME=share/data",
			"GOCACHE=go/build", "GOMODCACHE=go/mod", "BUN_INSTALL_CACHE_DIR=bun/install/cache",
		},
	} {
		t.Run("a value that is "+name, func(t *testing.T) {
			dir := t.TempDir()
			// The working directory a relative value would be made under, were it the shim's to
			// resolve: nothing of it is the shim's, so nothing lands here.
			t.Chdir(dir)
			if err := EnsureScratch(environ); err != nil {
				t.Fatalf("%q: %v", environ, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("with every value %s, EnsureScratch made %v", name, entries)
			}
		})
	}
	t.Run("a file where the directory should go", func(t *testing.T) {
		scratch := t.TempDir()
		if err := os.WriteFile(filepath.Join(scratch, "go"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		blocked := filepath.Join(scratch, "go", "build")
		err := EnsureScratch([]string{"TMPDIR=" + filepath.Join(scratch, "tmp"), "GOCACHE=" + blocked})
		if err == nil || !strings.Contains(err.Error(), "GOCACHE") || !strings.Contains(err.Error(), blocked) {
			t.Fatalf("EnsureScratch = %v, want a refusal naming GOCACHE and %s", err, blocked)
		}
	})
}

// On the pinned Oh My Pi, a repository's own settings reach the agent under Apply's environment as
// they reach any agent session: its .omp/config.yml's remote compaction endpoint is read as the
// repository set it, and an operator overlay named in the pod's PI_CONFIG_FILES reads the
// operator's own endpoint over it. The baseline holds nothing of a repository's settings off, so
// the endpoint the repository sets must read the same with Apply and without it.
func TestARepositorysSettingsReachTheAgentUnderTheOperators(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	testbin.OMPHome(t, omp, home)
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository := "compaction:\n  remoteEndpoint: https://repository.example/compact\n"
	if err := os.WriteFile(filepath.Join(repo, ".omp", "config.yml"), []byte(repository), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	if err := os.WriteFile(operator, []byte("compaction:\n  remoteEndpoint: https://operator.example/compact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		setting string
		pod     []string
		noApply bool
		want    any
	}{
		"the repository's endpoint, without the baseline":  {setting: "compaction.remoteEndpoint", noApply: true, want: "https://repository.example/compact"},
		"the repository's endpoint reaches the agent":      {setting: "compaction.remoteEndpoint", want: "https://repository.example/compact"},
		"the operator's overlay outranks the repository's": {setting: "compaction.remoteEndpoint", pod: []string{"PI_CONFIG_FILES=" + operator}, want: "https://operator.example/compact"},
	} {
		t.Run(name, func(t *testing.T) {
			env := append([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, tc.pod...)
			if !tc.noApply {
				env = Apply(env)
			}
			cmd := exec.Command(omp, "config", "get", tc.setting, "--json")
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("omp config get: %v", err)
			}
			var setting struct{ Value any }
			if err := json.Unmarshal(out, &setting); err != nil {
				t.Fatalf("omp config get printed %q: %v", out, err)
			}
			if setting.Value != tc.want {
				t.Errorf("%s reads %#v, want %#v", tc.setting, setting.Value, tc.want)
			}
		})
	}
}

// The pinned Oh My Pi roots its state under the state home only where the shim made the profile's
// directory under it: with EnsureStateHome applied, `omp models list` (which writes a log; `omp
// config get` writes none) logs under `$XDG_STATE_HOME/omp/profiles/legion/logs` and the config
// root's profile gets no `logs`; without it, the log lands under the config root's profile and the
// state home stays empty. The config root is `$HOME/.omp`, or `$OMP_CONFIG_ROOT` where that names
// an absolute directory, here one outside HOME, which then gets the log while HOME's profile gets
// none. Each way ompdirs.StateRoot answers the root the log landed under, so the port, the shim and
// the binary agree on the rule.
func TestThePinnedOhMyPiRootsItsStateUnderTheStateHomeOnlyWhereTheShimMadeTheProfileDirectory(t *testing.T) {
	omp := testbin.OMP(t)
	for name, tc := range map[string]struct{ ensured, configRoot bool }{
		"the shim made the profile directory":        {ensured: true},
		"without the shim":                           {},
		"without the shim, under an OMP_CONFIG_ROOT": {configRoot: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			testbin.OMPHome(t, omp, home)
			stateHome := filepath.Join(dir, "state", "tester")
			if err := os.MkdirAll(stateHome, 0o700); err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(dir, "work")
			if err := os.Mkdir(work, 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "OMP_PROFILE=legion", "XDG_STATE_HOME=" + stateHome}
			underStateHome := filepath.Join(stateHome, "omp", "profiles", "legion")
			underHome := filepath.Join(home, ".omp", "profiles", "legion")
			want, others := underHome, []string{underStateHome}
			switch {
			case tc.ensured:
				if err := EnsureStateHome(env); err != nil {
					t.Fatal(err)
				}
				want, others = underStateHome, []string{underHome}
			case tc.configRoot:
				configRoot := filepath.Join(dir, "config-root")
				// Oh My Pi looks for its native module under the config root's `natives` and,
				// finding none, extracts all of it there, some 123 MiB. PI_NATIVES_DIR, which it
				// reads before the config root, points it at the copy OMPHome linked under HOME.
				env = append(env, "OMP_CONFIG_ROOT="+configRoot, "PI_NATIVES_DIR="+filepath.Join(home, ".omp", "natives"))
				want, others = filepath.Join(configRoot, "profiles", "legion"), []string{underStateHome, underHome}
			}
			cmd := exec.Command(omp, "models", "list")
			cmd.Dir, cmd.Env = work, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("omp models list: %v\n%s", err, out)
			}
			if logs, err := filepath.Glob(filepath.Join(want, "logs", "omp.*.log")); err != nil || len(logs) == 0 {
				t.Errorf("logs under %s: %v, %v, want Oh My Pi's log there", want, logs, err)
			}
			for _, other := range others {
				if _, err := os.Stat(filepath.Join(other, "logs")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("stat %s/logs: %v, want none", other, err)
				}
			}
			if !tc.ensured {
				if entries, err := os.ReadDir(stateHome); err != nil || len(entries) != 0 {
					t.Errorf("the state home holds %v, %v, want nothing without the shim", entries, err)
				}
			}
			if root, _, err := ompdirs.StateRoot(lookup(env), work); err != nil || root != want {
				t.Errorf("ompdirs.StateRoot = %s, %v, want %s, where the log landed", root, err, want)
			}
		})
	}
}

// shippedExtensions is an image's gh extension directory (GhExtensionsDir) under dir holding one
// extension, gh-hello, whose executable prints "hello <its arguments>", beside a file and a
// directory no gh extension is named like, which gh reads as no extension and nothing links.
func shippedExtensions(t *testing.T, dir string) string {
	t.Helper()
	shipped := filepath.Join(dir, "opt", "legion", "gh-extensions")
	for _, made := range []string{filepath.Join(shipped, "gh-hello"), filepath.Join(shipped, "not-an-extension")} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shipped, "gh-hello", "gh-hello"), []byte("#!/bin/sh\necho hello \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shipped, "gh-stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return shipped
}

// The image's gh extensions reach gh only through gh's own extension directory under the data
// home, which starts empty in every pod: EnsureGhExtensions links each one there, leaves an entry
// already at its path alone (a sibling role's link, or an extension an agent installed itself),
// links nothing for an image that ships none or a data home it is not told, and refuses, naming
// the extension, a link it cannot make.
func TestEnsureGhExtensionsLinksEachShippedExtensionWhereGhReadsIt(t *testing.T) {
	t.Run("each extension, once, whoever runs it first", func(t *testing.T) {
		dir := t.TempDir()
		shipped := shippedExtensions(t, dir)
		data := filepath.Join(dir, "legion", "scratch", "data")
		environ := []string{"XDG_DATA_HOME=" + data}
		for range 2 {
			if err := EnsureGhExtensions(environ, shipped); err != nil {
				t.Fatal(err)
			}
		}
		extensions := filepath.Join(data, "gh", "extensions")
		if target, err := os.Readlink(filepath.Join(extensions, "gh-hello")); err != nil || target != filepath.Join(shipped, "gh-hello") {
			t.Errorf("gh-hello links to %q (%v), want %s", target, err, filepath.Join(shipped, "gh-hello"))
		}
		entries, err := os.ReadDir(extensions)
		if err != nil || len(entries) != 1 {
			t.Errorf("gh's extension directory holds %v (%v), want gh-hello alone", entries, err)
		}
	})
	t.Run("an entry already at the path is left as it is", func(t *testing.T) {
		dir := t.TempDir()
		shipped := shippedExtensions(t, dir)
		data := filepath.Join(dir, "data")
		installed := filepath.Join(data, "gh", "extensions", "gh-hello")
		if err := os.MkdirAll(installed, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := EnsureGhExtensions([]string{"XDG_DATA_HOME=" + data}, shipped); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(installed); err != nil || !info.IsDir() {
			t.Errorf("the extension an agent installed is %v (%v), want its directory left in place", info, err)
		}
	})
	t.Run("nothing to link", func(t *testing.T) {
		dir := t.TempDir()
		shipped := shippedExtensions(t, dir)
		for name, tc := range map[string]struct {
			environ []string
			shipped string
		}{
			"no shipped directory": {[]string{"XDG_DATA_HOME=" + filepath.Join(dir, "a")}, filepath.Join(dir, "none")},
			"no data home":         {nil, shipped},
			"an empty data home":   {[]string{"XDG_DATA_HOME="}, shipped},
			"a relative data home": {[]string{"XDG_DATA_HOME=relative/data"}, shipped},
		} {
			if err := EnsureGhExtensions(tc.environ, tc.shipped); err != nil {
				t.Errorf("%s: %v, want nothing linked and no refusal", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an image that ships no extension made %s (%v), want nothing", filepath.Join(dir, "a"), err)
		}
		if _, err := os.Stat("relative"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a relative data home made ./relative (%v), want nothing", err)
		}
	})
	t.Run("a link that cannot be made", func(t *testing.T) {
		dir := t.TempDir()
		shipped := shippedExtensions(t, dir)
		data := filepath.Join(dir, "data")
		if err := os.MkdirAll(filepath.Join(data, "gh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "gh", "extensions"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := EnsureGhExtensions([]string{"XDG_DATA_HOME=" + data}, shipped)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(data, "gh", "extensions")) {
			t.Errorf("EnsureGhExtensions over a file where gh's extension directory goes = %v, want a refusal naming it", err)
		}
	})
}

// The link is what the real gh reads as an extension: with the data home EnsureGhExtensions
// linked into, `gh hello` runs the shipped executable with its arguments; with an empty data home
// gh knows no such command. It runs LEGION_GH_PATH, the real gh behind a pane's or pod's wrapper,
// or the gh on PATH, and skips where there is neither.
func TestTheRealGhRunsALinkedExtension(t *testing.T) {
	gh := os.Getenv("LEGION_GH_PATH")
	if gh == "" {
		found, err := exec.LookPath("gh")
		if err != nil {
			t.Skip("no real gh on this machine: the test proves the link against one")
		}
		gh = found
	}
	dir := t.TempDir()
	shipped := shippedExtensions(t, dir)
	data := filepath.Join(dir, "data")
	if err := EnsureGhExtensions([]string{"XDG_DATA_HOME=" + data}, shipped); err != nil {
		t.Fatal(err)
	}
	run := func(data string) (string, error) {
		cmd := exec.Command(gh, "hello", "stacked")
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "XDG_DATA_HOME=" + data, "XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
			"XDG_STATE_HOME=" + filepath.Join(dir, "state"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
			"GH_CONFIG_DIR=" + filepath.Join(dir, "gh-config"), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "GH_TELEMETRY=0"}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(data); err != nil || out != "hello stacked" {
		t.Errorf("gh hello with the linked data home printed %q (%v), want \"hello stacked\"", out, err)
	}
	if out, err := run(filepath.Join(dir, "empty")); err == nil || strings.Contains(out, "hello stacked") {
		t.Errorf("gh hello with an empty data home printed %q (%v), want gh to know no such command", out, err)
	}
}

// The worker image installs its gh extensions at GhExtensionsDir, the one directory the shim
// links from, so a move of either is a move of both.
func TestTheWorkerImageShipsItsGhExtensionsWhereTheShimLinksFrom(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	want := "COPY --from=tools /opt/tools/gh-extensions " + GhExtensionsDir + "\n"
	if got := strings.Count(string(dockerfile), want); got != 1 {
		t.Errorf("worker.Dockerfile holds %q %d times, want once", strings.TrimSpace(want), got)
	}
}
