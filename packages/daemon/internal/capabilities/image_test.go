package capabilities

//ci:reads packages/daemon/docker/claude-code/** TestTheCommittedClaudeCodePinIsOneAPodCanInstall reads the worker image's claude-code pin through ../../docker

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

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities/capabilitiestest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
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
// run: eval-python, missing at once, stands, and the browser, whose Chromium sleeps past the
// deadline, and the rows after it are unanswered.
func TestCheckImageKeepsARowMissingBeforeTheDeadlineAndMarksTheRestUnanswered(t *testing.T) {
	s := newStubs(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sleep, filepath.Join(s.bin, "sleep")); err != nil {
		t.Fatal(err)
	}
	s.install("chromium", "#!/bin/sh\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	states, err := CheckImage(ctx, s.image("LEGION_TEST_NO_PYTHON=1"))

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
// browser's Chromium answers the timeout's SIGTERM with an exit of its own 0.9 s later, inside
// procgroup.StopGrace, and the caller's context ends 0.4 s into that stop. Which end fired is the
// cause the run recorded when it fired, so the row is a hanging command's, missing and in the
// error, never unanswered; the rows after it, which the ended context leaves unrun, are unanswered.
// The context ends a set time after Chromium started, read from the marker it writes first, so the
// window does not hang on how long the rows before the browser's took.
func TestCheckImageKeepsARowItsOwnTimeoutEndedMissingThoughItsStopOutlastsTheDeadline(t *testing.T) {
	was := commandTimeout
	commandTimeout = time.Second
	t.Cleanup(func() { commandTimeout = was })
	limit := commandTimeout
	s := newStubs(t)
	marker := filepath.Join(t.TempDir(), "started")
	s.install("chromium", "#!/bin/sh\n: >'"+marker+"'\ntrap '/bin/sleep 0.9; exit 1' TERM\n/bin/sleep 30 & wait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// seen says whether the marker appeared; once it has, the context ends 0.4 s after the timeout
	// fires and 0.5 s before Chromium's exit.
	seen := make(chan bool, 1)
	go func() {
		for bound := time.Now().Add(10 * time.Second); time.Now().Before(bound); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				time.Sleep(limit + 400*time.Millisecond)
				cancel()
				seen <- true
				return
			}
		}
		t.Error("the browser's Chromium never wrote its marker within 10s")
		seen <- false
	}()

	states, err := CheckImage(ctx, s.image())
	ended := ctx.Err() != nil

	if !<-seen {
		t.Fatal("the browser's Chromium never started, so the row covers no window")
	}
	if !ended {
		t.Fatal("CheckImage returned before the context ended; the row needs the context to end while the timeout's stop is under way")
	}
	got := byName(states)
	if browser := got[Browser]; browser.Status != StatusMissing || !strings.HasSuffix(browser.Detail, " --version: timed out after 1s") {
		t.Fatalf("browser = %+v, want missing, its Chromium timed out after 1s", browser)
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
}

// A command that exits on its own inside its budget did not time out, however long
// procgroup.WaitDelay then holds the run open for a child still holding its output pipe. The
// budget here is 100 ms short of WaitDelay, so every such run returns past its deadline, while the
// command still has 900 ms to exit under load: a run that read a timeout from the deadline having
// passed would report "timed out" for a check whose command answered inside the last WaitDelay of
// commandTimeout, in place of what it answered.
func TestRunReportsTheCommandsOwnExitWhenAChildHoldsItsOutputPastTheBudget(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "lingering.pid")
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}
	budget := procgroup.WaitDelay - 100*time.Millisecond

	started := time.Now()
	r := img.runWithin(context.Background(), budget, "sh", "-c", `echo 1.2.3; sleep 30 & echo $! >"$1"; exit 0`, "sh", pidFile)
	elapsed := time.Since(started)

	if elapsed < budget {
		t.Fatalf("the run returned after %s, inside its %s budget: nothing held the pipe past the deadline, so this run proves nothing", elapsed, budget)
	}
	if r.err != nil || r.exit != 0 || strings.TrimSpace(r.stdout) != "1.2.3" {
		t.Errorf("run = exit %d, error %v, stdout %q after %s; want exit 0, no error, and the answer: the command exited on its own inside its %s budget",
			r.exit, r.err, r.stdout, elapsed, budget)
	}
}

// A command its budget cuts off is a run that timed out, named with that budget, never an exit of
// its own, and no run the caller's context cut.
func TestRunReportsACommandItsBudgetCutOffAsTimedOut(t *testing.T) {
	img := Image{Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: t.TempDir()}

	r := img.runWithin(context.Background(), time.Second, "sh", "-c", "exec sleep 30")

	if r.err == nil || r.err.Error() != "timed out after 1s" {
		t.Errorf("run = exit %d, error %v; want the error \"timed out after 1s\": the budget cut the command off", r.exit, r.err)
	}
	if r.cut {
		t.Error("run is cut, want a run its own budget ended, which the caller's context did not cut")
	}
}

// A command its caller's context ended was not cut off by its budget: the run is no timeout but
// the signal the stop sent (procgroup.Err), since the command has no exit of its own, and it is cut
// (ran.cut), so the check's row is unanswered rather than a hanging command's
// (TestCheckImageKeepsARowMissingBeforeTheDeadlineAndMarksTheRestUnanswered). The cancel lands
// once the command runs, never on one not yet started.
func TestRunDoesNotReportACommandItsCallerStoppedAsTimedOut(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
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

	r := img.runWithin(ctx, time.Minute, "sh", "-c", `: >"$1"; exec sleep 30`, "sh", started)

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
// assertions by design (a backgrounded sleep standing in for a command's own lingering child).
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
