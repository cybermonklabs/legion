package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities/capabilitiestest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeOmp answers the two Oh My Pi commands the checks run as this pod's omp answers them, by
// the variables a test sets in the Image's Env: Python unavailable (exit 1, as omp exits) under
// LEGION_TEST_NO_PYTHON, model fallback on under LEGION_TEST_FALLBACK_ON.
const fakeOmp = `#!/bin/sh
case "$*" in
"setup python --check --json")
  if [ -n "${LEGION_TEST_NO_PYTHON:-}" ]; then echo '{"available": false, "usingManagedEnv": false, "managedEnvPath": ""}'; exit 1; fi
  echo '{"available": true}' ;;
"config get retry.modelFallback --json")
  if [ -n "${LEGION_TEST_FALLBACK_ON:-}" ]; then v=true; else v=false; fi
  printf '{"key":"retry.modelFallback","value":%s,"type":"boolean"}\n' "$v" ;;
*) echo "fake omp: unexpected argv: $*" >&2; exit 64 ;;
esac
`

// stubs is a worker image in miniature: a bin directory first on PATH with a stub for every
// binary the image rows look for, a HOME whose legion profile's plugin lock enables the CodeGraph
// plugin, the claude-envoy root and Claude Code pin the claude-code row reads, and an omp that
// answers the checks' commands.
type stubs struct {
	t                           *testing.T
	bin, home, omp, claude, pin string
}

func newStubs(t *testing.T) stubs {
	t.Helper()
	s := stubs{t: t, bin: t.TempDir(), home: t.TempDir(), claude: capabilitiestest.ClaudeRoot(t, "1"), pin: capabilitiestest.ClaudeCodePin(t)}
	capabilitiestest.InstallBinaries(t, s.bin)
	s.omp = filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(s.omp, []byte(fakeOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	s.writeLock(capabilitiestest.CodeGraphLock)
	return s
}

// install writes an executable script into bin.
func (s stubs) install(name, script string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s stubs) remove(name string) {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.bin, name)); err != nil {
		s.t.Fatal(err)
	}
}

// lock is the legion profile's plugin lock.
func (s stubs) lock() string {
	return capabilitiestest.LockPath(s.home)
}

// onPATH is the detail a row that checks PATH alone renders for names, each under s.bin.
func (s stubs) onPATH(names ...string) string {
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(s.bin, name)
	}
	return "on PATH: " + strings.Join(paths, ", ")
}

func (s stubs) writeLock(content string) {
	capabilitiestest.WriteLock(s.t, s.home, content)
}

// image is the Image the checks read: the stubs' PATH alone, never this process's, and the
// variables extra adds.
func (s stubs) image(extra ...string) Image {
	return Image{
		Launch:        s.omp,
		Env:           append([]string{"PATH=" + s.bin, "HOME=" + s.home, "OMP_PROFILE=legion"}, extra...),
		WorkDir:       s.t.TempDir(),
		ClaudeRoot:    s.claude,
		ClaudeCodePin: s.pin,
	}
}

// byName is the states by capability.
func byName(states []State) map[Name]State {
	m := map[Name]State{}
	for _, state := range states {
		m[state.Name] = state
	}
	return m
}

// Under an image that carries everything, every row is rendered in Table's order: the image rows
// present with their evidence — codegraph among them, the CLI on PATH and the plugin enabled in the
// profile's lock, since a pod's launch now loads that plugin — the others as what they are, and no
// error.
func TestCheckImageRendersEveryRowWhenEveryImageRowIsPresent(t *testing.T) {
	s := newStubs(t)

	states, err := CheckImage(context.Background(), s.image())

	if err != nil {
		t.Fatalf("CheckImage = %v, want every image row present", err)
	}
	if len(states) != len(Table) {
		t.Fatalf("CheckImage rendered %d states, want one per row (%d)", len(states), len(Table))
	}
	for i, row := range Table {
		state := states[i]
		if state.Name != row.Name {
			t.Errorf("state %d is %s, want %s: the table's order", i, state.Name, row.Name)
		}
		want := map[Site]string{SiteImage: StatusPresent, SiteLive: StatusReported, SiteDeployment: StatusReported, SiteWithheld: StatusWithheld}[row.Site]
		if state.Status != want {
			t.Errorf("%s: status %q, want %q", state.Name, state.Status, want)
		}
		if row.Site == SiteWithheld && !strings.HasPrefix(state.Detail, row.Citation+": ") {
			t.Errorf("%s: detail %q, want the citation %s first", state.Name, state.Detail, row.Citation)
		}
		if row.Site == SiteLive && state.Detail != "a claimed session reports it while it runs, so the probe cannot check it; `legion state` renders it: "+row.Summary {
			t.Errorf("%s: detail %q, want it to say a claimed session reports the row while it runs, then the summary", state.Name, state.Detail)
		}
		if row.Site == SiteDeployment && state.Detail != "the daemon reports it from "+row.MeasuredFrom+": "+row.Summary {
			t.Errorf("%s: detail %q, want it to say what the daemon measures the row from, then the summary", state.Name, state.Detail)
		}
	}
	got := byName(states)
	for name, want := range map[Name]string{
		EvalPython: "omp setup python --check reports available=true",
		Browser:    filepath.Join(s.bin, "chromium") + " (chromium on PATH) --version: Chromium 141.0.0.0",
		LSP:        s.onPATH("gopls", "typescript-language-server", "pyright-langserver"),
		CodeGraph:  filepath.Join(s.bin, "codegraph") + " on PATH; @bopstack/pi-codegraph enabled in " + s.lock(),
		Toolchain:  s.onPATH("go", "gcc", "curl", "wget", "python3", "node", "bun", "uv", "kubectl", "jq", "ssh-keygen", "flock", "psql", "redis-cli", "sqlite3", "nc", "dig") + "; go version go1.27.3 linux/amd64",
		ClaudeCode: s.claude + " holds dist/envoy-channel.js and legion/.claude-plugin/plugin.json; " +
			s.pin + " pins @anthropic-ai/claude-code " + capabilitiestest.ClaudeCodeVersion + " with the sha512 integrity of @anthropic-ai/claude-code-linux-x64; " +
			s.onPATH("npm", "node"),
		RepositoryExtensions: "a claimed session reports it while it runs, so the probe cannot check it; `legion state` renders it: loads the Oh My Pi extensions the repository it works carries",
		ResourceLimits:       "the daemon reports it from the deployment's configuration: runs within the CPU and memory the deployment's pod resources give it",
		RepositoryCacheRoom:  "the daemon reports it from each repository cache's last health read: fills its workspace from a repository cache with room on its volume to fetch and repack the clone",
	} {
		if got[name].Detail != want {
			t.Errorf("%s: detail %q, want %q", name, got[name].Detail, want)
		}
	}
	if want := "probe-image: capability eval-python: present (omp setup python --check reports available=true)"; got[EvalPython].String() != want {
		t.Errorf("State.String() = %q, want %q", got[EvalPython].String(), want)
	}
	if want := "probe-image: capability codegraph: present (" + got[CodeGraph].Detail + ")"; got[CodeGraph].Status != StatusPresent || got[CodeGraph].String() != want {
		t.Errorf("codegraph state = %q, want %q: present, with no sentence about a pod's launch", got[CodeGraph].String(), want)
	}
}

// Every missing image row is named, in the table's order, so one rebuild fixes them all; the
// rendered state says missing with the same detail; and the rows that are present still render
// present.
func TestCheckImageNamesEveryMissingRow(t *testing.T) {
	s := newStubs(t)
	s.remove("gopls")
	s.remove("pyright-langserver")

	states, err := CheckImage(context.Background(), s.image("LEGION_TEST_NO_PYTHON=1"))

	want := "capability eval-python is missing: omp setup python --check reports available=false; " +
		"capability lsp is missing: gopls, pyright-langserver are not on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	got := byName(states)
	if got[EvalPython].Status != StatusMissing || got[EvalPython].Detail != "omp setup python --check reports available=false" {
		t.Errorf("eval-python state = %+v, want missing with omp's answer", got[EvalPython])
	}
	if got[LSP].Status != StatusMissing || got[LSP].Detail != "gopls, pyright-langserver are not on PATH" {
		t.Errorf("lsp state = %+v, want missing naming both servers", got[LSP])
	}
	if got[Browser].Status != StatusPresent || got[Toolchain].Status != StatusPresent {
		t.Errorf("browser and toolchain = %+v, %+v, want both present still", got[Browser], got[Toolchain])
	}
	if len(states) != len(Table) {
		t.Errorf("rendered %d states, want the whole table (%d) however many rows are missing", len(states), len(Table))
	}
}

// One binary off PATH names that binary alone: onPath's single-absent branch (two absent is the lsp
// case in TestCheckImageNamesEveryMissingRow). Which names toolchain() checks, and in what order, is
// pinned by the Toolchain detail in TestCheckImageRendersEveryRowWhenEveryImageRowIsPresent, so one
// name is enough here.
func TestCheckImageNamesTheOneBinaryOffPATH(t *testing.T) {
	s := newStubs(t)
	s.remove("python3")

	states, err := CheckImage(context.Background(), s.image())

	want := "capability toolchain is missing: python3 is not on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	if got := byName(states)[Toolchain]; got.Status != StatusMissing || got.Detail != "python3 is not on PATH" {
		t.Errorf("toolchain state = %+v, want missing naming python3", got)
	}
}

// The Toolchain row's Summary is the one copy of the toolchain list no surface renders (the probe
// prints the detail), so it is held to the list toolchain() checks: the names recovered from a
// passing detail, joined as the Summary spells them.
func TestToolchainSummaryNamesTheListToolchainChecks(t *testing.T) {
	s := newStubs(t)

	detail, ok := toolchain(context.Background(), s.image())

	if !ok {
		t.Fatalf("toolchain() = %q, not ok; the stubs must make it pass", detail)
	}
	const prefix = "on PATH: "
	listed, ok := strings.CutPrefix(detail, prefix)
	if !ok {
		t.Fatalf("toolchain() = %q, want it to begin %q", detail, prefix)
	}
	paths, _, _ := strings.Cut(listed, "; ")
	var names []string
	for _, path := range strings.Split(paths, ", ") {
		names = append(names, filepath.Base(path))
	}
	want := "builds and runs with the generic toolchain: " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	i := slices.IndexFunc(Table, func(c Capability) bool { return c.Name == Toolchain })
	if i < 0 {
		t.Fatal("Table has no Toolchain row")
	}
	if row := Table[i]; row.Summary != want {
		t.Errorf("Toolchain Summary %q, want %q (the list toolchain() checks)", row.Summary, want)
	}
}

// The browser is resolved as Oh My Pi resolves it: PUPPETEER_EXECUTABLE_PATH when the environment
// sets it, else chromium on PATH; and it is run, so a path that names nothing is missing even with
// a chromium on PATH, which Oh My Pi would never launch under that variable.
func TestCheckImageResolvesTheBrowserAsOhMyPiDoes(t *testing.T) {
	s := newStubs(t)
	s.install("my-chrome", capabilitiestest.BinaryScript)
	for name, testCase := range map[string]struct {
		env    []string
		status string
		detail string
	}{
		"PUPPETEER_EXECUTABLE_PATH naming nothing": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium"}, StatusMissing,
			"/nonexistent/chromium (PUPPETEER_EXECUTABLE_PATH) --version: fork/exec /nonexistent/chromium: no such file or directory",
		},
		"PUPPETEER_EXECUTABLE_PATH naming a browser": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=" + filepath.Join(s.bin, "my-chrome")}, StatusPresent,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
		"a bare name, looked up on PATH as a spawn does": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=my-chrome"}, StatusPresent,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			states, err := CheckImage(context.Background(), s.image(testCase.env...))
			got := byName(states)[Browser]
			if got.Status != testCase.status || got.Detail != testCase.detail {
				t.Errorf("browser state = %+v, want %s (%s)", got, testCase.status, testCase.detail)
			}
			if (err != nil) != (testCase.status == StatusMissing) {
				t.Errorf("CheckImage = %v, want an error exactly when the browser is missing", err)
			}
			if err != nil && !strings.Contains(err.Error(), "capability browser is missing") {
				t.Errorf("CheckImage = %v, want it to name the browser", err)
			}
		})
	}
	t.Run("no chromium anywhere", func(t *testing.T) {
		s := newStubs(t)
		s.remove("chromium")
		states, _ := CheckImage(context.Background(), s.image())
		if got := byName(states)[Browser]; got.Status != StatusMissing || got.Detail != "chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable" {
			t.Errorf("browser state = %+v, want missing naming both resolutions", got)
		}
	})
}

// SkipBrowser (`probe-image --skip-browser`) leaves the browser row unchecked and runs no browser,
// so an image with no Chromium passes while every other image row is still checked: one that
// lacks another capability still fails, naming that row and not the browser.
func TestCheckImageSkipBrowserLeavesTheBrowserRowUnchecked(t *testing.T) {
	const detail = "not checked (--skip-browser): the image is for a deployment that turns Oh My Pi's browser off"
	t.Run("no chromium anywhere", func(t *testing.T) {
		s := newStubs(t)
		s.remove("chromium")
		img := s.image()
		img.SkipBrowser = true

		states, err := CheckImage(context.Background(), img)

		if err != nil {
			t.Fatalf("CheckImage = %v, want no error: the browser row is the only one the image lacks", err)
		}
		got := byName(states)
		if got[Browser].Status != StatusUnchecked || got[Browser].Detail != detail {
			t.Errorf("browser state = %+v, want %s (%s)", got[Browser], StatusUnchecked, detail)
		}
		for _, row := range Table {
			if row.Site == SiteImage && row.Name != Browser && got[row.Name].Status != StatusPresent {
				t.Errorf("%s state = %+v, want %s: SkipBrowser leaves only the browser row unchecked", row.Name, got[row.Name], StatusPresent)
			}
		}
	})
	t.Run("a chromium on PATH is not run", func(t *testing.T) {
		s := newStubs(t)
		ran := filepath.Join(t.TempDir(), "ran")
		s.install("chromium", "#!/bin/sh\ntouch "+ran+"\nexit 1\n")
		img := s.image()
		img.SkipBrowser = true

		states, err := CheckImage(context.Background(), img)

		if err != nil || byName(states)[Browser].Status != StatusUnchecked {
			t.Errorf("CheckImage = %v, browser state %+v, want no error and the row unchecked", err, byName(states)[Browser])
		}
		if _, statErr := os.Stat(ran); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the chromium on PATH ran (stat %s: %v), want no browser run under SkipBrowser", ran, statErr)
		}
	})
	t.Run("another capability missing", func(t *testing.T) {
		s := newStubs(t)
		s.remove("chromium")
		s.remove("gopls")
		img := s.image()
		img.SkipBrowser = true

		_, err := CheckImage(context.Background(), img)

		if err == nil || !strings.Contains(err.Error(), "capability lsp is missing") || strings.Contains(err.Error(), "capability browser") {
			t.Errorf("CheckImage = %v, want an error naming lsp and not the browser", err)
		}
	})
}

// CodeGraph is the CLI on PATH and its plugin enabled in the profile's lock, both; each failure is
// named, and the row is missing and in CheckImage's error — the gate refuses an image lacking
// either half.
func TestCheckImageNeedsTheCodeGraphCLIAndItsPluginEnabled(t *testing.T) {
	for name, testCase := range map[string]struct {
		setup func(stubs)
		want  string
	}{
		"the plugin disabled": {func(s stubs) {
			s.writeLock(`{"plugins":{"@bopstack/pi-codegraph":{"version":"0.1.1","enabled":false}}}`)
		}, "@bopstack/pi-codegraph is disabled in the plugin lock "},
		"the plugin not installed":  {func(s stubs) { s.writeLock(`{"plugins":{"@sjawhar/pi-legion":{"enabled":true}}}`) }, "@bopstack/pi-codegraph is not in the plugin lock "},
		"no lock":                   {func(s stubs) { os.Remove(s.lock()) }, "cannot read the plugin lock "},
		"a lock that is not JSON":   {func(s stubs) { s.writeLock("not json") }, "is not the lock's JSON"},
		"the CLI off PATH":          {func(s stubs) { s.remove("codegraph") }, "codegraph is not on PATH"},
		"both the CLI and the lock": {func(s stubs) { s.remove("codegraph"); os.Remove(s.lock()) }, "codegraph is not on PATH; cannot read the plugin lock "},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStubs(t)
			testCase.setup(s)

			states, err := CheckImage(context.Background(), s.image())

			got := byName(states)[CodeGraph]
			if got.Status != StatusMissing || !strings.Contains(got.Detail, testCase.want) {
				t.Errorf("codegraph state = %+v, want missing saying %q", got, testCase.want)
			}
			if err == nil || !strings.HasPrefix(err.Error(), "capability codegraph is missing: ") {
				t.Errorf("CheckImage = %v, want it to name codegraph", err)
			}
		})
	}
}

// The claude-code row is the image's half of a Claude worker, which carries no Claude Code: the
// claude-envoy plugin it loads and the pin a pod installs Claude Code from and verifies it
// against, with the npm that install runs and the node its version check runs. Each failure is
// named, every one at once, and the row is missing and in CheckImage's error.
func TestCheckImageNeedsTheClaudePluginThePinAndWhatInstallsFromIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		setup func(s *stubs)
		want  string
	}{
		"no root given": {func(s *stubs) { s.claude = "" }, "no claude-envoy root was given to check"},
		"no pin given":  {func(s *stubs) { s.pin = "" }, "no Claude Code pin was given to check"},
		"npm off PATH":  {func(s *stubs) { s.remove("npm") }, "npm is not on PATH"},
		"no channel server": {func(s *stubs) {
			os.Remove(filepath.Join(s.claude, "dist", "envoy-channel.js"))
		}, "holds no envoy-channel.js"},
		"no lockfile": {func(s *stubs) {
			os.Remove(filepath.Join(s.pin, "package-lock.json"))
		}, "the Claude Code pin cannot be read: open "},
		"all three at once": {func(s *stubs) {
			os.Remove(filepath.Join(s.claude, "legion", ".claude-plugin", "plugin.json"))
			os.Remove(filepath.Join(s.pin, "package.json"))
			s.remove("node")
		}, "holds no plugin.json"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStubs(t)
			testCase.setup(&s)

			states, err := CheckImage(context.Background(), s.image())

			got := byName(states)[ClaudeCode]
			if got.Status != StatusMissing || !strings.Contains(got.Detail, testCase.want) {
				t.Errorf("claude-code state = %+v, want missing saying %q", got, testCase.want)
			}
			if name == "all three at once" && strings.Count(got.Detail, "; ") != 2 {
				t.Errorf("claude-code detail %q, want the plugin, the pin and node named at once", got.Detail)
			}
			if err == nil || !strings.Contains(err.Error(), "capability claude-code is missing: ") {
				t.Errorf("CheckImage = %v, want it to name claude-code", err)
			}
		})
	}
}

// The pin the worker image ships, packages/daemon/docker/claude-code as committed, is one a pod
// can install and verify: ReadClaudeCodePin passes it, answering the version its package.json
// pins, so a regenerated lockfile that dropped the linux-x64 package or its integrity fails here
// before it fails an image build.
func TestTheCommittedClaudeCodePinIsOneAPodCanInstall(t *testing.T) {
	dir := filepath.Join("..", "..", "docker", "claude-code")
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pinned struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pinned); err != nil {
		t.Fatal(err)
	}
	version, err := ReadClaudeCodePin(dir)
	if want := pinned.Dependencies[claudeCodePackage]; err != nil || version != want {
		t.Errorf("ReadClaudeCodePin(%s) = %q, %v, want %q", dir, version, err, want)
	}
}

// The lock follows the profile root Oh My Pi resolves (ompdirs.ProfileRoot): the named profile's
// under .omp/profiles, the default profile's under .omp itself; and a profile name Oh My Pi
// refuses is the refusal, naming it.
func TestPluginLockFollowsTheProfile(t *testing.T) {
	if got, err := pluginLock([]string{"HOME=/home/legion", "OMP_PROFILE=legion"}, "/work"); err != nil || got != "/home/legion/.omp/profiles/legion/plugins/omp-plugins.lock.json" {
		t.Errorf("pluginLock(legion) = %q, %v, want the legion profile's lock", got, err)
	}
	if got, err := pluginLock([]string{"HOME=/home/legion"}, "/work"); err != nil || got != "/home/legion/.omp/plugins/omp-plugins.lock.json" {
		t.Errorf("pluginLock(default) = %q, %v, want the default profile's lock", got, err)
	}
	if _, err := pluginLock([]string{"HOME=/home/legion", "OMP_PROFILE=Work"}, "/work"); err == nil || !strings.HasPrefix(err.Error(), `Invalid OMP profile "Work"`) {
		t.Errorf("pluginLock(Work) = %v, want Oh My Pi's refusal of the profile", err)
	}
}

// A profile name Oh My Pi refuses leaves no lock to read: the codegraph row is missing with the
// refusal as its detail, and the refusal names the row in CheckImage's error.
func TestCheckImageReportsAProfileOhMyPiRefusesOnTheCodeGraphRow(t *testing.T) {
	s := newStubs(t)

	states, err := CheckImage(context.Background(), s.image("OMP_PROFILE=Work"))

	got := byName(states)[CodeGraph]
	if got.Status != StatusMissing || !strings.HasPrefix(got.Detail, `Invalid OMP profile "Work" in the environment Oh My Pi starts under.`) {
		t.Errorf("codegraph state = %+v, want missing with Oh My Pi's refusal of the profile", got)
	}
	if err == nil || !strings.HasPrefix(err.Error(), `capability codegraph is missing: Invalid OMP profile "Work"`) {
		t.Errorf("CheckImage = %v, want it to name codegraph with the refusal", err)
	}
}

// A check that asks Oh My Pi reads what Oh My Pi said, not its exit code: an omp that exits 1
// with `available: false` is a missing Python, and one that prints no JSON is named by what it
// printed.
func TestCheckImageJudgesPythonByOhMyPisAnswer(t *testing.T) {
	s := newStubs(t)
	if err := os.WriteFile(s.omp, []byte("#!/bin/sh\necho 'setup: no such command' >&2; exit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	states, err := CheckImage(context.Background(), s.image())

	got := byName(states)[EvalPython]
	want := "omp setup python --check --json exited 2 without a JSON available field: setup: no such command"
	if got.Status != StatusMissing || got.Detail != want {
		t.Errorf("eval-python state = %+v, want missing with %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "capability eval-python is missing") {
		t.Errorf("CheckImage = %v, want it to name eval-python", err)
	}
}

// Every image row has a check, and every check is an image row's.
func TestEveryImageRowHasACheck(t *testing.T) {
	for _, row := range Table {
		if _, ok := imageChecks[row.Name]; ok != (row.Site == SiteImage) {
			t.Errorf("row %s is checked at %s, and imageChecks has a check for it: %t", row.Name, row.Site, ok)
		}
	}
}

// A context that has ended before the check leaves every image row unanswered, and runs no row's
// command: the probe judges no row on a run it did not finish, and the next attempt checks them.
// Such rows are no missing capability, so the error is nil; every other row renders as it always
// does.
func TestCheckImageMarksEveryImageRowUnansweredUnderAnEndedContext(t *testing.T) {
	s := newStubs(t)
	ran := filepath.Join(t.TempDir(), "ran")
	record := "#!/bin/sh\n: >'" + ran + "'\n"
	for _, name := range []string{"chromium", "go"} {
		s.install(name, record)
	}
	img := s.image()
	img.Launch = "sh -c ': >\"$0\"' " + ran
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	states, err := CheckImage(ctx, img)

	if err != nil {
		t.Errorf("CheckImage under an ended context = %v, want no error: no row was found missing", err)
	}
	if len(states) != len(Table) {
		t.Fatalf("CheckImage rendered %d rows, want every row of the table (%d)", len(states), len(Table))
	}
	for i, row := range Table {
		want := map[Site]string{SiteImage: StatusUnanswered, SiteLive: StatusReported, SiteDeployment: StatusReported, SiteWithheld: StatusWithheld}[row.Site]
		if states[i].Name != row.Name || states[i].Status != want {
			t.Errorf("row %d = %+v, want %s %s", i, states[i], row.Name, want)
		}
		if row.Site == SiteImage && states[i].Detail != "not checked: context canceled" {
			t.Errorf("%s detail = %q, want %q", row.Name, states[i].Detail, "not checked: context canceled")
		}
	}
	if _, statErr := os.Stat(ran); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a check ran a command under the ended context (%s: %v)", ran, statErr)
	}
}

// A row whose check returned while the context was alive keeps what it found, a missing one in the
// error as ever, and a row whose command the context's deadline stopped, or kept from starting
// (ran.cut), is unanswered and left out of the error, as is every image row after it, which is not
// run: eval-python, missing at once, stands, and the browser, whose Chromium hangs until the
// deadline's stop, and the rows after it are unanswered. The deadline passes at a fact, not at a
// time (testwait.StandInDeadline): once Chromium has started, which is after eval-python's check
// returned, so no load can pass it before that check has answered. Chromium runs until a signal
// ends it, the row's cleanup writes held, or this test process is gone, never on a timer of its own.
//
// Main's 2 s caller deadline also bounded the checks before the browser: a check that took 2 s or
// more before it answered left eval-python unanswered. That bound stays, as main's 2 s, but on the
// product's own time and no stand-in's: the row's Oh My Pi (eval-python's command) writes the
// kernel's record of its own start as its first act (testexec.MarkStart), and the span from the
// call to that start is all CheckImage's, since eval-js runs no command. So an eval-python check
// that spends its time before it runs Oh My Pi fails here, and a loaded machine that holds Oh My Pi's
// own shell once it is made does not.
func TestCheckImageKeepsARowMissingBeforeTheDeadlineAndMarksTheRestUnanswered(t *testing.T) {
	s := newStubs(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	ctx, started, held, bound, ran := passedOnceStarted(t, context.DeadlineExceeded)
	s.install("chromium", "#!/bin/sh\n: >"+shellprefix.Literal(started)+"\nwhile [ ! -e "+shellprefix.Literal(held)+" ] && kill -0 "+strconv.Itoa(os.Getpid())+" 2>/dev/null; do "+shellprefix.Literal(sleep)+" 0.01; done\n")
	const checkBound = 2 * time.Second
	ompMark, marked := filepath.Join(t.TempDir(), "ompmark"), filepath.Join(t.TempDir(), "omp")
	if err := testexec.WriteFile(marked, []byte("#!/bin/sh\n"+testexec.MarkStart(shellprefix.Literal(ompMark))+"\nexec "+shellprefix.Literal(s.omp)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	img := s.image("LEGION_TEST_NO_PYTHON=1")
	img.Launch = marked

	called := testexec.SinceBoot(t)
	states, err := CheckImage(ctx, img)

	if !<-ran {
		t.Fatalf("the browser's Chromium never started within %s, so the deadline passed at no fact of the browser's run", bound)
	}
	if began := testexec.StartedAfter(t, ompMark, called); began > checkBound {
		t.Errorf("eval-python's Oh My Pi started %s after CheckImage was called, past main's %s: the checks before the browser took that long of the product's own time before they ran it", began, checkBound)
	}
	if want := "capability eval-python is missing: omp setup python --check reports available=false"; err == nil || err.Error() != want {
		t.Errorf("CheckImage = %v, want the error %q alone", err, want)
	}
	got := byName(states)
	if got[EvalJS].Status != StatusPresent || got[EvalPython].Status != StatusMissing {
		t.Errorf("eval-js = %+v, eval-python = %+v; want present and missing, each checked before the deadline", got[EvalJS], got[EvalPython])
	}
	for _, name := range []Name{Browser, LSP, CodeGraph, Skills, Toolchain, ClaudeCode} {
		if got[name].Status != StatusUnanswered || got[name].Detail != "not checked: context deadline exceeded" {
			t.Errorf("%s = %+v, want unanswered (not checked: context deadline exceeded)", name, got[name])
		}
	}
}

// A row whose command its own timeout (commandTimeout) ended while the context was alive stays
// missing, its detail saying it timed out, also where the timeout's stop outlasts the context: the
// browser's Chromium answers the timeout's SIGTERM by waiting, inside procgroup.StopGrace, until
// the caller's context has ended, and only then exits on its own. Which end fired is the cause the
// run recorded when it fired, so the row is a hanging command's, missing and in the error, never
// unanswered; the rows after it, which the ended context leaves unrun, are unanswered.
//
// commandTimeout is a second, as on main. The order of the two ends is a fact, not a time: the
// context ends once Chromium has written that the SIGTERM reached it (stopped), and Chromium exits
// once it has seen the release the row writes after the context ended, writing released first.
// How late the timeout's stop came is read from Chromium's own marks: it writes started as its
// first act once its trap is set, so a timeout counted from before that reaches it within its
// second, and one that reaches it more than lateBy past that second stopped its command late,
// which fails the run whatever its order. lateBy is main's: its context ended 400 ms past the
// timeout, counted from Chromium's start, and failed a stop that came after it. The one span this
// row does not own is its own cancel, which follows Chromium's stopped mark by a poll: a run in
// which a loaded machine kept this process from it for half of procgroup.StopGrace or more may
// have had the stop ended by the grace before the context ended, so it shows nothing of a stop
// that outlasts the context, and is not judged. The row runs the check again, within its bound,
// until a run of the right order comes, and fails naming the runs it made when none comes.
func TestCheckImageKeepsARowItsOwnTimeoutEndedMissingThoughItsStopOutlastsTheDeadline(t *testing.T) {
	was := commandTimeout
	commandTimeout = time.Second
	t.Cleanup(func() { commandTimeout = was })
	const lateBy, startBound = 400 * time.Millisecond, 10 * time.Second
	s := newStubs(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	// Each run's files are in the directory LEGION_TEST_RUN names. Chromium runs until a signal
	// ends it, the row's cleanup writes held, or this test process is gone, never on a timer of its own.
	s.install("chromium", "#!/bin/sh\nSLEEP="+shellprefix.Literal(sleep)+"\nOWNER="+strconv.Itoa(os.Getpid())+"\n"+
		"trap ': >\"$LEGION_TEST_RUN/stopped\"; while [ ! -e \"$LEGION_TEST_RUN/release\" ] && kill -0 \"$OWNER\" 2>/dev/null; do \"$SLEEP\" 0.01; done; : >\"$LEGION_TEST_RUN/released\"; exit 1' TERM\n"+
		testexec.MarkStart(`"$LEGION_TEST_RUN/startmark"`)+"\n: >\"$LEGION_TEST_RUN/started\"\n"+
		"while [ ! -e \"$LEGION_TEST_RUN/held\" ] && kill -0 \"$OWNER\" 2>/dev/null; do \"$SLEEP\" 0.01; done\n")
	watch := testwait.Bounded(t, answerBudget, time.Second)
	until := time.Now().Add(testwait.Bounded(t, 3*answerBudget, 2*answerBudget))
	for runs := 1; ; runs++ {
		run := filepath.Join(t.TempDir(), strconv.Itoa(runs))
		if err := os.Mkdir(run, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(filepath.Join(run, "held"), nil, 0o644) })
		ctx, cancel := context.WithCancel(context.Background())
		// cancelled is the moment this row ended the context, or the zero time where Chromium
		// recorded no SIGTERM within watch, after which the row ends the context all the same.
		cancelled := make(chan time.Time, 1)
		go func() {
			seen := testwait.Holds(watch, testwait.Poll, func() bool {
				_, err := os.Stat(filepath.Join(run, "stopped"))
				return err == nil
			})
			cancel()
			at := time.Now()
			_ = os.WriteFile(filepath.Join(run, "release"), nil, 0o644)
			if !seen {
				at = time.Time{}
			}
			cancelled <- at
		}()

		called := testexec.SinceBoot(t)
		states, err := CheckImage(ctx, s.image("LEGION_TEST_RUN="+run))
		ended := ctx.Err() != nil

		at := <-cancelled
		if at.IsZero() {
			t.Fatalf("run %d: the browser's Chromium recorded no SIGTERM within %s, so its own timeout never stopped it", runs, watch)
		}
		stopped, statErr := os.Stat(filepath.Join(run, "stopped"))
		if statErr != nil {
			t.Fatal(statErr)
		}
		// Chromium's first start is bounded as on main, at startBound from the call, but read from
		// the kernel's own record of when it made Chromium's process (testexec.MarkStart), not from
		// Chromium's first line, so the shell's own start-up on a loaded machine is no part of it. A
		// browser check that starts Chromium late, or checks before it that hold it, fail here.
		if mark := filepath.Join(run, "startmark"); func() bool { _, err := os.Stat(mark); return err == nil }() {
			if began := testexec.StartedAfter(t, mark, called); began > startBound {
				t.Fatalf("run %d: the browser's Chromium started %s after CheckImage was called, past main's %s: the browser check, or a check before it, started it late", runs, began, startBound)
			}
		}
		// A Chromium with no started mark had the SIGTERM before its first act, which is no late stop.
		if started, statErr := os.Stat(filepath.Join(run, "started")); statErr == nil {
			if took := stopped.ModTime().Sub(started.ModTime()); took > commandTimeout+lateBy {
				t.Fatalf("run %d: the timeout's SIGTERM reached the browser's Chromium %s after its start, more than its %s timeout and %s: the timeout stopped its command late", runs, took, commandTimeout, lateBy)
			}
		}
		if gap := at.Sub(stopped.ModTime()); gap >= procgroup.StopGrace/2 {
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: this row ended the context %s after the SIGTERM reached Chromium, half of the stop's %s grace or more", runs, gap, procgroup.StopGrace)
				continue
			}
			t.Fatalf("no run of the right order in %d runs: in each, this row ended the context half of the stop's %s grace or more after the SIGTERM reached Chromium, so no run shows a stop that outlasts the context", runs, procgroup.StopGrace)
		}
		if !ended {
			t.Fatalf("run %d: CheckImage returned before the context ended; the row needs the context to end while the timeout's stop is under way", runs)
		}
		got := byName(states)
		if browser := got[Browser]; browser.Status != StatusMissing || !strings.HasSuffix(browser.Detail, " --version: timed out after "+commandTimeout.String()) {
			t.Fatalf("browser = %+v, want missing, its Chromium timed out after %s", browser, commandTimeout)
		}
		if want := "capability browser is missing: " + got[Browser].Detail; err == nil || err.Error() != want {
			t.Errorf("CheckImage = %v, want the error %q alone", err, want)
		}
		notChecked := "not checked: " + ctx.Err().Error()
		for _, name := range []Name{LSP, CodeGraph, Skills, Toolchain, ClaudeCode} {
			if got[name].Status != StatusUnanswered || got[name].Detail != notChecked {
				t.Errorf("%s = %+v, want unanswered (%s)", name, got[name], notChecked)
			}
		}
		return
	}
}

// A command that exits on its own while its budget is far away did not time out, however long
// procgroup.WaitDelay then holds the run open for a child still holding its output pipe: a run
// that read a timeout from the child would report "timed out" for a check whose command answered,
// in place of what it answered. The child stays in the command's process group and runs until the
// row's cleanup ends it, never on a timer of its own. The budget is answerBudget, so it cannot pass
// while the command is still exiting, however loaded the machine: one that passed between the
// command's reap and Wait's read of its context would still meet procgroup's stop, which the child
// in the group takes, and the run would report a timeout (procgroup.Configure's doc). No fact
// outside os/exec orders that window for a child in the group, so this row asserts neither answer
// there; the next row crosses the budget with its child outside the group, where the window has
// one answer.
func TestRunReportsTheCommandsOwnExitWhenAChildHoldsItsOutputPastItsExit(t *testing.T) {
	dir := t.TempDir()
	pidFile, held := filepath.Join(dir, "lingering.pid"), filepath.Join(dir, "held")
	t.Cleanup(func() {
		_ = os.WriteFile(held, nil, 0o644)
		killPIDFile(t, pidFile)
	})
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

	started := time.Now()
	r := img.runWithin(context.Background(), answerBudget, "sh", "-c", `echo 1.2.3; (while [ ! -e "$2" ] && kill -0 "$3" 2>/dev/null; do sleep 0.01; done) & echo $! >"$1"; exit 0`, "sh", pidFile, held, strconv.Itoa(os.Getpid()))
	elapsed := time.Since(started)

	if r.err != nil || r.exit != 0 || strings.TrimSpace(r.stdout) != "1.2.3" {
		t.Errorf("run = exit %d, error %v, stdout %q after %s; want exit 0, no error, and the answer: the command exited on its own while its %s budget was far away",
			r.exit, r.err, r.stdout, elapsed, answerBudget)
	}
	if elapsed < procgroup.WaitDelay {
		t.Fatalf("the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe past the command's exit, so this run proves nothing", elapsed)
	}
}

// A command that exited on its own inside its budget did not time out, however long
// procgroup.WaitDelay then holds the run open for a child holding its output pipe, and however much
// of that time passes after the budget: a run that read a timeout from its budget's cause alone,
// or from the time the run took, and not from whether that budget's stop reached the command
// (killed), would report "timed out" for a check whose command answered. The budget is 100 ms
// short of procgroup.WaitDelay, so a run whose command exits inside it returns past it.
//
// The child that holds the pipe runs in a session of its own (setsid) and writes its pid from
// inside that session, and the command exits only once that pid is written, so the budget's stop,
// where os/exec still runs it between the command's reap and Wait's read of its context, finds no
// process group left to signal, which leaves killed false whichever way that window falls. A
// loaded machine can hold the command past a budget counted from before its shell started, and
// that run, which the budget's stop ended, shows nothing of this. Only a run whose command was
// seen reaped before its budget could pass is judged (watchReaped), so the row runs the command
// again until a run of the right order comes, within a minute.
func TestRunReportsTheCommandsOwnExitWhenItsBudgetPassesWhileAChildOutsideItsGroupHoldsItsOutput(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Fatalf("setsid is not on PATH (%v): this row starts the child that holds the pipe in a session of its own", err)
	}
	const budget = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, budget+procgroup.StopGrace+procgroup.WaitDelay+time.Second)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leaderFile, holderFile, held := filepath.Join(dir, "leader.pid"), filepath.Join(dir, "holder.pid"), filepath.Join(dir, "held")
		t.Cleanup(func() {
			_ = os.WriteFile(held, nil, 0o644)
			killPIDFile(t, holderFile)
		})
		reaped := watchReaped(leaderFile, within)
		img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

		started := time.Now()
		r := img.runWithin(context.Background(), budget, "sh", "-c",
			`echo $$ >"$1"; echo 1.2.3; setsid sh -c 'echo $$ >"$1.tmp" && mv "$1.tmp" "$1"; while [ ! -e "$2" ] && kill -0 "$3" 2>/dev/null; do sleep 0.01; done' sh "$2" "$3" "$4" & until [ -s "$2" ]; do sleep 0.01; done; exit 0`,
			"sh", leaderFile, holderFile, held, strconv.Itoa(os.Getpid()))
		elapsed := time.Since(started)
		// The run is over: its child is released now, not at the row's end, and it also ends once
		// this test process is gone, so neither a run that is not judged nor a timeout's panic
		// leaves it running.
		if err := os.WriteFile(held, nil, 0o644); err != nil {
			t.Errorf("release run %d's child: %v", runs, err)
		}
		at := <-reaped

		if at.IsZero() {
			t.Fatalf("run %d: the command was not seen reaped within %s of the run's start (run = exit %d, error %v after %s)", runs, within, r.exit, r.err, elapsed)
		}
		if !at.Before(started.Add(budget)) {
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the command was seen reaped %s after the run's start, not inside its %s budget (run = exit %d, error %v)", runs, at.Sub(started), budget, r.exit, r.err)
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the command was seen reaped only after its %s budget could pass", runs, within, budget)
		}
		if elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d returned after %s, inside procgroup.WaitDelay: nothing held the pipe past the command's exit, so this run proves nothing", runs, elapsed)
		}
		if r.err != nil || r.exit != 0 || strings.TrimSpace(r.stdout) != "1.2.3" {
			t.Errorf("run %d = exit %d, error %v, stdout %q after %s; want exit 0, no error, and the answer: the command exited on its own inside its %s budget, and the budget's stop reached nothing of it",
				runs, r.exit, r.err, r.stdout, elapsed, budget)
		}
		return
	}
}

// Main's row, with the child that holds the pipe in the command's own process group, asked one
// more thing than the row above: that runWithin never stops that group after the run is over. A
// runWithin that, once its budget had passed, stopped what is left of the group after the command's
// own exit, and read that stop as its budget's, failed main's row on every run.
//
// Main's row also failed a correct runWithin under load: where a loaded machine holds the command's
// reap past the budget, os/exec's Wait meets the ended context first and calls procgroup's stop,
// which the child in the group takes, and the run reads a timeout. The two are told apart by the
// kernel, not by a clock. Once the run is over, os/exec has closed the read end of the command's
// output pipe (procgroup.WaitDelay ran out); a correct runWithin stops the group only inside Wait,
// while that read end is still open. So the child ignores SIGPIPE, and its SIGTERM trap writes one
// byte to its stdout: EPIPE says the reader was already gone, the group was stopped after the run,
// and the row fails; a write that succeeds says the reader was still there, the stop was os/exec's
// inside Wait, and that run is not judged. A run whose child took no SIGTERM is judged as main
// judged it. The one false failure left needs two things at once: a reap held past the budget, and
// the child's trap held past procgroup.WaitDelay after the stop, which lets Wait close the reader
// before the trap writes.
func TestRunNeverStopsTheCommandsGroupAfterItsOwnExitWhenAChildInTheGroupHoldsItsOutput(t *testing.T) {
	const budget = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, budget+procgroup.StopGrace+procgroup.WaitDelay+time.Second)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		held := filepath.Join(dir, "held")
		childFile, open, closed := filepath.Join(dir, "child.pid"), filepath.Join(dir, "term-open"), filepath.Join(dir, "term-closed")
		t.Cleanup(func() {
			_ = os.WriteFile(held, nil, 0o644)
			killPIDFile(t, childFile)
		})
		img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}
		// The child sets both traps before it writes its pid, and the command exits only once that
		// pid is written, so any SIGTERM the group takes finds the child's trap set.
		child := `trap '' PIPE; trap 'if printf . 2>/dev/null; then : >"$1/term-open"; else : >"$1/term-closed"; fi; exit 0' TERM; ` +
			`echo $$ >"$1/child.pid.tmp" && mv "$1/child.pid.tmp" "$1/child.pid"; ` +
			`while [ ! -e "$1/held" ] && kill -0 "$2" 2>/dev/null; do sleep 0.01; done`

		started := time.Now()
		r := img.runWithin(context.Background(), budget, "sh", "-c",
			`echo 1.2.3; sh -c "$1" sh "$2" "$3" & until [ -s "$2/child.pid" ]; do sleep 0.01; done; exit 0`,
			"sh", child, dir, strconv.Itoa(os.Getpid()))
		elapsed := time.Since(started)
		termed := childTermed(t, childFile, open, closed, within)
		if err := os.WriteFile(held, nil, 0o644); err != nil {
			t.Errorf("release run %d's child: %v", runs, err)
		}

		switch termed {
		case closed:
			t.Fatalf("run %d: the command's group was stopped after the run was over: the child in the group took a SIGTERM after os/exec had closed the command's output pipe, so runWithin stopped the group after the command's own exit (run = exit %d, error %v, stdout %q after %s)", runs, r.exit, r.err, r.stdout, elapsed)
		case open:
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the child in the group took a SIGTERM while the command's output pipe was still read, which is os/exec's stop inside Wait, where a loaded machine held the command's reap past its %s budget (run = exit %d, error %v)", runs, budget, r.exit, r.err)
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, os/exec's Wait met the ended %s budget before the command's reap and stopped its group", runs, within, budget)
		case childGone:
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the child in the group was ended by a signal before its trap was set, which only a stop inside Wait can do, where a loaded machine held the command past its %s budget before it exited (run = exit %d, error %v)", runs, budget, r.exit, r.err)
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the command's group was stopped before its child set its trap", runs, within)
		}
		if elapsed < budget {
			t.Fatalf("run %d returned after %s, inside its %s budget: nothing held the pipe past the deadline, so this run proves nothing", runs, elapsed, budget)
		}
		if r.err != nil || r.exit != 0 || strings.TrimSpace(r.stdout) != "1.2.3" {
			t.Errorf("run %d = exit %d, error %v, stdout %q after %s; want exit 0, no error, and the answer: the command exited on its own inside its %s budget",
				runs, r.exit, r.err, r.stdout, elapsed, budget)
		}
		return
	}
}

// childGone is what childTermed answers for a child that is gone, or a zombie, with no SIGTERM mark:
// a signal ended it before its trap ran, or it never wrote its pid because the command was stopped
// first. Only a stop inside os/exec's Wait comes that early: a stop after the run finds the child
// with its trap set, since the command exits only once the child has written its pid.
const childGone = "gone"

// childTermed waits, after a run, for what the child at childFile took: the path of its SIGTERM
// trap's mark (open or closed) once one is written; "" once it is seen alive with no SIGTERM pending
// and no mark for settle; or childGone. A SIGTERM the kernel holds for the child (its SigPnd or
// ShdPnd) is waited out, since its trap runs once the child next runs.
func childTermed(t *testing.T, childFile, open, closed string, bound time.Duration) string {
	t.Helper()
	const settle = 500 * time.Millisecond
	raw, err := os.ReadFile(childFile)
	if err != nil {
		return childGone
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var quietSince time.Time
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); time.Sleep(testwait.Poll) {
		for _, mark := range []string{closed, open} {
			if _, err := os.Stat(mark); err == nil {
				return mark
			}
		}
		status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		if err != nil || strings.Contains(string(status), "\nState:\tZ") {
			// One last look: a trap that ran and exited just now left its mark before the exit.
			for _, mark := range []string{closed, open} {
				if _, err := os.Stat(mark); err == nil {
					return mark
				}
			}
			return childGone
		}
		if termPending(string(status)) {
			quietSince = time.Time{}
			continue
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
		} else if time.Since(quietSince) >= settle {
			return ""
		}
	}
	t.Fatalf("the child (pid %d) wrote no SIGTERM mark and was not seen quiet within %s", pid, bound)
	return ""
}

// termPending answers whether a /proc/<pid>/status text holds a SIGTERM the kernel has not yet
// delivered, in the thread's pending set (SigPnd) or the process's (ShdPnd).
func termPending(status string) bool {
	const sigterm = uint64(1) << (15 - 1)
	for _, line := range strings.Split(status, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || (name != "SigPnd" && name != "ShdPnd") {
			continue
		}
		if mask, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64); err == nil && mask&sigterm != 0 {
			return true
		}
	}
	return false
}

// watchReaped watches, from before the command starts, for the command that writes its own pid to
// leaderFile as its first act, and answers once when it first saw that pid answer ESRCH, which it
// does once Go has reaped the command (a zombie still answers), or the zero time where it never did
// within bound. Each moment is read after the fact it stands for.
func watchReaped(leaderFile string, bound time.Duration) <-chan time.Time {
	seen := make(chan time.Time, 1)
	go func() {
		var at time.Time
		if testwait.Holds(bound, testwait.Poll, func() bool {
			raw, err := os.ReadFile(leaderFile)
			if err != nil || !strings.HasSuffix(string(raw), "\n") {
				return false
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			return err == nil && pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		}) {
			at = time.Now()
		}
		seen <- at
	}()
	return seen
}

// answerBudget is a budget no command of these rows that answers comes near, however loaded the
// machine, as the gate rigs' budget of the same name is (internal/daemon): the slowest answering
// attempt they measured was about 8 s, at a load average of 1,200 on 288 CPUs.
const answerBudget = 20 * time.Second

// passedOnceStarted is a caller's context whose deadline passes with cause once the command a row
// runs has written the file started, as a budget passes at a fact (testwait.StandInDeadline), and
// never before. The command waits for the file held, which the row's cleanup writes. ran answers
// whether the command started within bound.
func passedOnceStarted(t *testing.T, cause error) (ctx context.Context, started, held string, bound time.Duration, ran <-chan bool) {
	t.Helper()
	dir := t.TempDir()
	started, held = filepath.Join(dir, "started"), filepath.Join(dir, "held")
	t.Cleanup(func() { _ = os.WriteFile(held, nil, 0o644) })
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	bound = testwait.Bounded(t, answerBudget, time.Second)
	startedWithin := make(chan bool, 1)
	go func() {
		startedWithin <- testwait.Holds(bound, testwait.Poll, func() bool {
			_, err := os.Stat(started)
			return err == nil
		})
		pass(cause)
	}()
	return ctx, started, held, bound, startedWithin
}

// A command its budget cuts off is a run that timed out, named with that budget, never an exit of
// its own, and no run the caller's context cut. The run tells its budget's end from any other by
// the cause its attempt recorded (errCommandTimeout). The first case gives the command its own
// clock, a budget of a second that runWithin itself must apply: a runWithin that applied none would
// leave the command running until the row's far bound. That clock starts before the command does,
// and its one run is judged, as main judged it: a command that started at all, however late a
// loaded machine started it, is still running when the second passes, so the budget's stop reaches
// it and the run timed out; only a runWithin that spends the second before Start leaves nothing to
// cut, and that is runWithin's fault. The second case passes the budget with that cause at a fact,
// once the command has started (testwait.StandInDeadline, the caller's context the attempt derives
// from), so every run of it is judged. The command runs until the budget's stop or the row's
// cleanup ends it, never on a timer of its own.
func TestRunReportsACommandItsBudgetCutOffAsTimedOut(t *testing.T) {
	t.Run("its own clock", func(t *testing.T) {
		const budget = time.Second
		held := filepath.Join(t.TempDir(), "held")
		t.Cleanup(func() { _ = os.WriteFile(held, nil, 0o644) })
		img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

		done := make(chan ran, 1)
		go func() {
			done <- img.runWithin(context.Background(), budget, "sh", "-c", `while [ ! -e "$1" ] && kill -0 "$2" 2>/dev/null; do sleep 0.01; done`, "sh", held, strconv.Itoa(os.Getpid()))
		}()
		var r ran
		far := testwait.Bounded(t, answerBudget, time.Second)
		select {
		case r = <-done:
		case <-time.After(far):
			t.Fatalf("the run had not returned %s after it was called: its own %s budget did not end the command", far, budget)
		}

		if want := "timed out after " + budget.String(); r.err == nil || r.err.Error() != want {
			t.Errorf("run = exit %d, error %v; want the error %q: the command's own budget cut it off", r.exit, r.err, want)
		}
		if r.cut {
			t.Error("run is cut, want a run its own budget ended, which the caller's context did not cut")
		}
	})

	t.Run("a budget that passes once the command runs", func(t *testing.T) {
		ctx, started, held, bound, ran := passedOnceStarted(t, errCommandTimeout)
		img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

		r := img.runWithin(ctx, answerBudget, "sh", "-c", `: >"$1"; while [ ! -e "$2" ] && kill -0 "$3" 2>/dev/null; do sleep 0.01; done`, "sh", started, held, strconv.Itoa(os.Getpid()))

		if !<-ran {
			t.Fatalf("the command never started within %s, so its budget passed at no fact of the run (run = exit %d, error %v)", bound, r.exit, r.err)
		}
		if want := "timed out after " + answerBudget.String(); r.err == nil || r.err.Error() != want {
			t.Errorf("run = exit %d, error %v; want the error %q: the budget cut the command off", r.exit, r.err, want)
		}
		if r.cut {
			t.Error("run is cut, want a run its own budget ended, which the caller's context did not cut")
		}
	})
}

// A command its caller's context ended was not cut off by its budget: the run is no timeout but
// the signal the stop sent (procgroup.Err), since the command has no exit of its own, and it is cut
// (ran.cut), so the check's row is unanswered rather than a hanging command's
// (TestCheckImageKeepsARowMissingBeforeTheDeadlineAndMarksTheRestUnanswered). The cancel lands
// once the command runs, never on one not yet started. The command runs until that cancel's stop or
// the row's cleanup ends it, never on a timer of its own, and the stop must end it within
// stopBound of the call: main's command, `exec sleep 30`, exited on its own 30 s after it started,
// which failed a stop that came later.
func TestRunDoesNotReportACommandItsCallerStoppedAsTimedOut(t *testing.T) {
	const stopBound = 30 * time.Second
	started := filepath.Join(t.TempDir(), "started")
	held := filepath.Join(t.TempDir(), "held")
	t.Cleanup(func() { _ = os.WriteFile(held, nil, 0o644) })
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	done := make(chan ran, 1)
	go func() {
		done <- img.runWithin(ctx, time.Minute, "sh", "-c", `: >"$1"; while [ ! -e "$2" ] && kill -0 "$3" 2>/dev/null; do sleep 0.01; done`, "sh", started, held, strconv.Itoa(os.Getpid()))
	}()
	var r ran
	bound := testwait.Bounded(t, stopBound, time.Second)
	select {
	case r = <-done:
	case <-time.After(bound):
		t.Fatalf("the run had not returned %s after it was called: the caller's stop did not end its command", bound)
	}

	if r.err == nil || r.err.Error() != "signal: terminated" {
		t.Errorf("run = exit %d, error %v; want the error \"signal: terminated\": the caller's context ended the command inside its minute, which is no timeout and leaves no exit", r.exit, r.err)
	}
	if !r.cut {
		t.Error("run is not cut, want a run the caller's context cut")
	}
}

// A command whose caller's context had ended before it could start is cut too (ran.cut), though
// no stop reached it, since the context's end kept it from starting, and it is no timeout: its run
// is the context's error, and the command never ran.
func TestRunReportsACommandItsCallerKeptFromStartingAsCut(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := img.runWithin(ctx, time.Minute, "sh", "-c", `: >"$1"`, "sh", ran)

	if !r.cut {
		t.Error("run is not cut, want a run the caller's ended context kept from starting")
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("run = exit %d, error %v; want the context's own error, context.Canceled, and no timeout", r.exit, r.err)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran under the ended context (%s: %v)", ran, err)
	}
}

// A command a signal ended after it printed (Oh My Pi killed after it named the overlay it could
// not read) fails naming the signal, with what it printed beneath, as its failed exit would carry
// it: the run's error is the whole of what the check reports for it (ran.failure).
func TestRunKeepsWhatACommandASignalEndedPrinted(t *testing.T) {
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

	r := img.runWithin(context.Background(), time.Minute, "sh", "-c", "echo 'cannot read overlay.yml' >&2; kill -KILL $$")

	if want := "signal: killed\ncannot read overlay.yml"; r.err == nil || r.err.Error() != want || r.failure() != want {
		t.Errorf("run = error %v, failure %q; want %q", r.err, r.failure(), want)
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a loop standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// ReadModelFallback is the JSON's value, on or off, read through the launch as a shell fragment,
// so a prefix in it runs; a command that fails, or prints no boolean, is an error naming the
// command and what it printed.
func TestReadModelFallback(t *testing.T) {
	s := newStubs(t)
	for name, testCase := range map[string]struct {
		img  Image
		want string
	}{
		"off by default":                 {s.image(), bootprobe.ModelFallbackOff},
		"on":                             {s.image("LEGION_TEST_FALLBACK_ON=1"), bootprobe.ModelFallbackOn},
		"the launch is a shell fragment": {Image{Launch: "/usr/bin/env LEGION_TEST_FALLBACK_ON=1 " + s.omp, Env: s.image().Env, WorkDir: t.TempDir()}, bootprobe.ModelFallbackOn},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadModelFallback(context.Background(), testCase.img)
			if err != nil || got != testCase.want {
				t.Errorf("ReadModelFallback = %q, %v, want %q", got, err, testCase.want)
			}
		})
	}
	t.Run("a failing command", func(t *testing.T) {
		img := s.image()
		img.Launch = s.omp + " --bogus"
		_, err := ReadModelFallback(context.Background(), img)
		want := s.omp + " --bogus config get retry.modelFallback --json exited 64: fake omp: unexpected argv: --bogus config get retry.modelFallback --json"
		if err == nil || err.Error() != want {
			t.Errorf("ReadModelFallback = %v, want %q", err, want)
		}
	})
	t.Run("no boolean value", func(t *testing.T) {
		omp := filepath.Join(t.TempDir(), "omp")
		if err := os.WriteFile(omp, []byte("#!/bin/sh\necho '{\"key\":\"retry.modelFallback\",\"value\":null}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		img := s.image()
		img.Launch = omp
		_, err := ReadModelFallback(context.Background(), img)
		if err == nil || !strings.Contains(err.Error(), "printed no JSON object with a boolean value: {\"key\":\"retry.modelFallback\",\"value\":null}") {
			t.Errorf("ReadModelFallback = %v, want an error quoting the output", err)
		}
	})
	t.Run("a launch that cannot run", func(t *testing.T) {
		img := s.image()
		img.Launch = "/nonexistent/omp"
		_, err := ReadModelFallback(context.Background(), img)
		if err == nil || !strings.Contains(err.Error(), "/nonexistent/omp config get retry.modelFallback --json exited 127") {
			t.Errorf("ReadModelFallback = %v, want the shell's 127 for a launch it cannot exec", err)
		}
	})
}

// scriptedOmp is an omp that answers `--version` and `--help` by printing version and help, and
// exits code on either; any other argv is refused as the fake omp refuses one. It prints with the
// shell's own printf, since the stubs' PATH holds no cat.
func scriptedOmp(t *testing.T, version, help string, code int) string {
	t.Helper()
	quoted := func(text string) string { return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'" }
	omp := filepath.Join(t.TempDir(), "omp")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"--version) printf '%s\\n' " + quoted(version) + "; exit " + strconv.Itoa(code) + " ;;\n" +
		"--help) printf '%s\\n' " + quoted(help) + "; exit " + strconv.Itoa(code) + " ;;\n" +
		"*) echo \"fake omp: unexpected argv: $*\" >&2; exit 64 ;;\nesac\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return omp
}

// withoutSessionID is the flags table of a fork release before the flag
// (v18.18.6-sami.20261117-013345, as its `omp --help` prints it): flags that begin with the words
// and mention sessions, and none that is --session-id.
const withoutSessionID = `omp v18.18.6

FLAGS
  -c, --continue                        Continue previous session
  -r, --resume=<value>                  Resume a session (by ID prefix, path, or picker if omitted)
      --restore=<value>                 Resume a session as --resume does, then go on with what its last process left unfinished
      --session-dir=<value>             Directory for session storage and lookup
      --no-session                      Don't save session (ephemeral)

Plugin Options:
  --plugin-dir <path>        Load plugin from directory (repeatable)`

// ReadSessionIDFlag is taken when a line of Oh My Pi's help names --session-id in its flag column
// — with its value, after a short flag, or alone at the line's end — and absent when the help
// lists no such flag, though a description mentions the words or a longer flag begins with them.
func TestReadSessionIDFlagReadsTheFlagColumnOfTheHelp(t *testing.T) {
	s := newStubs(t)
	for name, testCase := range map[string]struct {
		help, want string
	}{
		"the first release with the flag": {withoutSessionID + "\n      --session-id=<value>              Start a new session under this id (a lowercase UUID); refused when a stored session already holds it", bootprobe.SessionIDFlagTaken},
		"after a short flag":              {withoutSessionID + "\n  -s, --session-id=<value>  Start a new session under this id", bootprobe.SessionIDFlagTaken},
		"alone at the line's end":         {"FLAGS\n      --session-id", bootprobe.SessionIDFlagTaken},
		"a release before the flag":       {withoutSessionID, bootprobe.SessionIDFlagAbsent},
		"the words in a description":      {withoutSessionID + "\n      --fork=<value>   Start a session under a new id, as --session-id does", bootprobe.SessionIDFlagAbsent},
		"a longer flag":                   {withoutSessionID + "\n      --session-id-file=<value>   Read the session id from a file", bootprobe.SessionIDFlagAbsent},
	} {
		t.Run(name, func(t *testing.T) {
			img := s.image()
			img.Launch = scriptedOmp(t, "omp/18.18.6", testCase.help, 0)
			got, err := ReadSessionIDFlag(context.Background(), img)
			if err != nil || got != testCase.want {
				t.Errorf("ReadSessionIDFlag = %q, %v, want %q", got, err, testCase.want)
			}
		})
	}
}

// A help Oh My Pi did not give — a run that fails, prints nothing, or cannot start — is an error
// naming the command and what it printed, never a state: the probe writes it as unread.
func TestReadSessionIDFlagIsAnErrorWhenTheHelpIsNotRead(t *testing.T) {
	s := newStubs(t)
	for name, testCase := range map[string]struct {
		launch func(t *testing.T) string
		want   func(launch string) string
	}{
		"a failing command": {
			func(t *testing.T) string { return scriptedOmp(t, "omp/18.18.6", "Error: the profile is locked", 1) },
			func(launch string) string { return launch + " --help exited 1: Error: the profile is locked" },
		},
		"no help": {
			func(t *testing.T) string { return scriptedOmp(t, "omp/18.18.6", "", 0) },
			func(launch string) string { return launch + " --help printed no help: " },
		},
		"a launch that cannot run": {
			func(*testing.T) string { return "/nonexistent/omp" },
			func(launch string) string { return launch + " --help exited 127" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			img := s.image()
			img.Launch = testCase.launch(t)
			got, err := ReadSessionIDFlag(context.Background(), img)
			if want := testCase.want(img.Launch); err == nil || got != "" || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("ReadSessionIDFlag = %q, %v, want no state and an error beginning %q", got, err, want)
			}
		})
	}
}

// ReadOmpVersion is the first line Oh My Pi printed for --version, trimmed, read through the
// launch as a shell fragment; a run that fails or prints nothing is an error naming the command.
func TestReadOmpVersion(t *testing.T) {
	s := newStubs(t)
	t.Run("the first line", func(t *testing.T) {
		img := s.image()
		img.Launch = "/usr/bin/env LEGION_TEST_UNUSED=1 " + scriptedOmp(t, "  omp/18.18.6  \nbuilt from sami.20261117-074351", withoutSessionID, 0)
		if got, err := ReadOmpVersion(context.Background(), img); err != nil || got != "omp/18.18.6" {
			t.Errorf("ReadOmpVersion = %q, %v, want %q", got, err, "omp/18.18.6")
		}
	})
	t.Run("a failing command", func(t *testing.T) {
		img := s.image()
		img.Launch = scriptedOmp(t, "omp: cannot read its install", withoutSessionID, 1)
		_, err := ReadOmpVersion(context.Background(), img)
		if want := img.Launch + " --version exited 1: omp: cannot read its install"; err == nil || err.Error() != want {
			t.Errorf("ReadOmpVersion = %v, want %q", err, want)
		}
	})
	t.Run("no version", func(t *testing.T) {
		img := s.image()
		img.Launch = scriptedOmp(t, " ", withoutSessionID, 0)
		_, err := ReadOmpVersion(context.Background(), img)
		if want := img.Launch + " --version printed no version: "; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ReadOmpVersion = %v, want an error beginning %q", err, want)
		}
	})
}

// envValue is name's value in env, the last assignment winning as it does for the process env
// starts.
func TestEnvValueTakesTheLastAssignment(t *testing.T) {
	if got := envValue([]string{"PATH=a", "PATH=b"}, "PATH"); got != "b" {
		t.Errorf("envValue = %q, want the last assignment", got)
	}
}

// The table's states are one per row and print in order, so the probe pod's log reads as the list.
func TestLinesPrintInTableOrder(t *testing.T) {
	s := newStubs(t)
	states, err := CheckImage(context.Background(), s.image())
	if err != nil {
		t.Fatal(err)
	}
	var names []Name
	for _, state := range states {
		names = append(names, state.Name)
		if !strings.HasPrefix(state.String(), "probe-image: capability "+string(state.Name)+": "+state.Status+" (") || !strings.HasSuffix(state.String(), ")") {
			t.Errorf("State.String() = %q, want the probe's row shape", state.String())
		}
	}
	var want []Name
	for _, row := range Table {
		want = append(want, row.Name)
	}
	if !slices.Equal(names, want) {
		t.Errorf("states name %v, want Table's order %v", names, want)
	}
}

// State.String() is the probe's line, pinned whole, so a reworded format is caught here.
func TestStateStringIsTheProbeLine(t *testing.T) {
	state := State{Name: Browser, Status: StatusPresent, Detail: "x"}
	if want := "probe-image: capability browser: present (x)"; state.String() != want {
		t.Errorf("State.String() = %q, want %q", state.String(), want)
	}
}
