package workspace

//ci:reads packages/daemon/docker/worker.Dockerfile requireImagePackWindows reads ../../docker/worker.Dockerfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// colorRunner runs every command through runner under a jj user configuration that colors all
// output, as an operator's `ui.color = "always"` does.
type colorRunner struct {
	Runner
	config string
}

func newColorRunner(t *testing.T, runner Runner) *colorRunner {
	t.Helper()
	config := filepath.Join(t.TempDir(), "color.toml")
	if err := os.WriteFile(config, []byte("[ui]\ncolor = \"always\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &colorRunner{Runner: runner, config: config}
}

func (r *colorRunner) Run(ctx context.Context, command Command) (Result, error) {
	command.Env = append(append([]string(nil), command.Env...), "JJ_CONFIG="+r.config)
	return r.Runner.Run(ctx, command)
}

// Every jj read provisioning and removal parse runs uncolored, whatever the jj configuration
// asks: a commit id wrapped in color codes is not a commit id. Under `ui.color = "always"` the
// issue's workspace is provisioned, removed, and provisioned again at its bookmark, and the second
// provisioning writes no jj setting the first one already wrote.
func TestProvisioningParsesUncoloredReadsUnderColorAlways(t *testing.T) {
	local := newLocalRunner(t)
	run := newColorRunner(t, local)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	commit := strings.TrimSpace(runSetup(t, first.Clone, "jj", "log", "-r", first.Bookmark, "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never"))
	if err := Remove(context.Background(), run, first, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	before := len(local.Calls())
	second, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if got := strings.TrimSpace(runSetup(t, second.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--color=never")); got != commit {
		t.Errorf("the second workspace starts at %s, want the bookmark's %s", got, commit)
	}
	for _, call := range local.Calls()[before:] {
		if commandWith(call.Argv, "jj", "config", "set") {
			t.Errorf("the second provision wrote a setting the first had written: %q", call.Argv)
		}
	}
}

// pendingRunner changes a file in the shared clone's own working copy before every command once
// the clone exists, so any command that snapshots that working copy records an operation.
type pendingRunner struct {
	*recordingRunner
	clone string
	n     int
}

func (r *pendingRunner) Run(ctx context.Context, command Command) (Result, error) {
	if _, err := os.Stat(filepath.Join(r.clone, ".jj")); err == nil {
		r.n++
		if err := os.WriteFile(filepath.Join(r.clone, "pending.txt"), []byte(strings.Repeat("x", r.n)), 0o644); err != nil {
			return Result{}, err
		}
	}
	return r.recordingRunner.Run(ctx, command)
}

// Nothing provisioning or removal runs on the shared clone snapshots its working copy but `jj
// workspace add`, which jj refuses --ignore-working-copy on: over a provision, remove and
// provision again, with a change pending in the clone before every command, the clone's log holds
// exactly one snapshot per add.
func TestProvisioningSnapshotsTheSharedCloneOnlyInWorkspaceAdd(t *testing.T) {
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	run := &pendingRunner{recordingRunner: newLocalRunner(t), clone: location.Clone}
	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := Remove(context.Background(), run, working, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("provision again: %v", err)
	}
	adds := 0
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "jj", "workspace", "add") {
			adds++
		}
	}
	if adds < 2 {
		t.Fatalf("the cycle ran %d workspace adds, want at least 2", adds)
	}
	if snapshots := cloneSnapshots(t, location.Clone); snapshots != adds {
		t.Errorf("the shared clone's log holds %d working-copy snapshots over %d workspace adds, want one each", snapshots, adds)
	}
}

// Provisioning marks origin as GitHub's default repository in the shared clone
// (configureCloneDefaultRepository) and turns git's ambiguous-ref warning off there
// (configureCloneReads): the clone and the issue's workspace each read one gh-resolved line,
// `remote.origin.gh-resolved base`, and core.warnAmbiguousRefs as false.
func TestProvisionMarksOriginAndTurnsOffTheAmbiguousRefWarning(t *testing.T) {
	run := newLocalRunner(t)
	request := provisionRequest(t)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, place := range []struct{ name, dir string }{
		{"the shared clone", working.Clone},
		{"the issue's workspace", working.Dir},
	} {
		if output, code := gitConfigRead(t, place.dir, "config", "--get", "core.warnAmbiguousRefs"); output != "false\n" || code != 0 {
			t.Errorf("%s %s reads core.warnAmbiguousRefs as %q (exit %d), want false", place.name, place.dir, output, code)
		}
		if output, code := gitConfigRead(t, place.dir, "config", "--get-regexp", `^remote\..*\.gh-resolved$`); output != "remote.origin.gh-resolved base\n" || code != 0 {
			t.Errorf("%s %s reads its gh-resolved lines as %q (exit %d), want origin's base alone", place.name, place.dir, output, code)
		}
	}
}

// packWindows are the four pack settings every git of an issue pod is to read, written out here
// rather than read from packWindowConfig, so that a key taken out of that list, or a value changed
// in it, fails each test that holds a git to them.
var packWindows = [][2]string{
	{"core.packedGitWindowSize", "32m"},
	{"core.packedGitLimit", "256m"},
	{"pack.windowMemory", "64m"},
	{"core.deltaBaseCacheLimit", "64m"},
}

// The shared clone's own configuration carries the four pack settings, for the one git of a pod
// that reads nothing else: the shared public leak check's, which every push runs with every
// GIT_CONFIG variable removed and GIT_CONFIG_NOSYSTEM set. After `workspace-init provision` fills a
// pod's clone directly, a git in the issue's workspace under such an environment (the fixtures': no
// GIT_CONFIG variable, GIT_CONFIG_NOSYSTEM=1, GIT_CONFIG_GLOBAL=/dev/null) reads each of them from
// the local scope, the clone's own file. A value a role changed there is set back at the next
// provision. The repository cache holds none of them: Fetch rebuilds a cache whose configuration
// does (cacheConfigKeys).
func TestTheSharedCloneMapsPacksInSmallWindows(t *testing.T) {
	run := newLocalRunner(t)
	request := provisionRequest(t)
	readsAll := func(t *testing.T, working Workspace) {
		t.Helper()
		for _, setting := range packWindows {
			if output, code := gitConfigRead(t, working.Dir, "config", "--show-scope", "--get", setting[0]); output != "local\t"+setting[1]+"\n" || code != 0 {
				t.Errorf("the issue's workspace %s reads %s as %q (exit %d), want %s from the clone's own configuration", working.Dir, setting[0], output, code, setting[1])
			}
		}
	}
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	readsAll(t, working)
	runSetup(t, working.Clone, "git", "--git-dir="+filepath.Join(working.Clone, ".git"), "config", "core.packedGitLimit", "32g")
	again, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	readsAll(t, again)
}

// The worker image's /etc/gitconfig holds packWindowConfig, for provisioning's git, whose runner
// removes the role environment's pairs (execRunner.Run). A Dockerfile cannot read the Go list, so
// worker.Dockerfile writes the four settings out twice: the `/usr/bin/git config --system <key>
// <value>` writes of its final stage, and the `for setting in <key>=<value> …` list its last step
// holds /etc/gitconfig to. Each copy is packWindowConfig, key and value, in the list's order, so a
// value changed in one place and not in the others fails here, in CI. The pack-memory Job
// (scripts/e2e/pack-memory-job.sh) is what measures the built image itself.
func TestTheWorkerImageWritesPackWindowConfig(t *testing.T) {
	requireImagePackWindows(t, filepath.Join("..", "..", "docker", "worker.Dockerfile"))
}

// imagePackWindowWrite is one `/usr/bin/git config --system <key> <value>` of a RUN instruction.
// A key that begins with a hyphen is an option, as the smoke step's `--get` is, and no write.
var imagePackWindowWrite = regexp.MustCompile(`/usr/bin/git config --system ([^-\s]\S*) (\S+)`)

// imagePackWindowSmoke is the smoke step's `for setting in <key>=<value> …; do` list.
var imagePackWindowSmoke = regexp.MustCompile(`for setting in ([^;]*); do`)

// requireImagePackWindows fails t unless the final stage of the Dockerfile at path writes
// packWindowConfig with `git config --system` and its smoke step's list holds the same, each copy
// in the list's order, naming the key and both values of each setting that differs. A copy the
// Dockerfile does not hold fails too.
func requireImagePackWindows(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The final stage's instructions, each joined across its continuation lines, as
	// TestImageEnvIsWhatTheWorkerImageSets (runtime/sandbox) reads them.
	var instructions []string
	var current strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if current.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if continued, ok := strings.CutSuffix(line, "\\"); ok {
			current.WriteString(continued + " ")
			continue
		}
		current.WriteString(line)
		instruction := current.String()
		current.Reset()
		if strings.HasPrefix(instruction, "FROM ") {
			instructions = nil
		}
		instructions = append(instructions, instruction)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	var writes, smoke [][2]string
	for _, instruction := range instructions {
		if !strings.HasPrefix(instruction, "RUN ") {
			continue
		}
		for _, match := range imagePackWindowWrite.FindAllStringSubmatch(instruction, -1) {
			writes = append(writes, [2]string{match[1], match[2]})
		}
		for _, match := range imagePackWindowSmoke.FindAllStringSubmatch(instruction, -1) {
			for _, setting := range strings.Fields(match[1]) {
				key, value, _ := strings.Cut(setting, "=")
				smoke = append(smoke, [2]string{key, value})
			}
		}
	}
	// settingAt is a copy's setting at i as key=value, or "none" past its end.
	settingAt := func(settings [][2]string, i int) string {
		if i >= len(settings) {
			return "none"
		}
		return settings[i][0] + "=" + settings[i][1]
	}
	for _, place := range []struct {
		name     string
		settings [][2]string
	}{
		{"`git config --system` writes", writes},
		{"smoke step's `for setting in` list", smoke},
	} {
		if len(place.settings) == 0 {
			t.Errorf("%s: no %s found in the final stage", path, place.name)
			continue
		}
		for i := range max(len(place.settings), len(packWindowConfig)) {
			if have, want := settingAt(place.settings, i), settingAt(packWindowConfig, i); have != want {
				t.Errorf("%s: setting %d of the %s is %s, and of packWindowConfig %s", path, i+1, place.name, have, want)
			}
		}
	}
}

// gitConfigRead runs `git <args>` in dir under the fixtures' environment and returns what it
// printed and its exit code. A git that could not run fails the test.
func gitConfigRead(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	output, err := fixturegit.Runner{TB: t, Env: setupEnvironment()}.Try(dir, append([]string{"git"}, args...)...)
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return output, exit.ExitCode()
	case err != nil:
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return output, 0
}

// Every provision marks origin as gh's default repository in the shared clone
// (configureCloneDefaultRepository). This test covers the tmux runtime's GitHub path. The issue
// pod's directly filled clone gets the same marker, as
// TestWorkspaceInitProvisionsTheIssueWorkspace in cmd/legion/workspace_init_test.go proves. The
// clone and the issue's workspace each read one gh-resolved line,
// `remote.origin.gh-resolved base`. A clone that already holds a marker, here the one `gh repo fork
// --remote` leaves on the remote it renames origin to, keeps it at its next provision and gets no
// origin line beside it.
func TestProvisionMarksOriginAsGhsDefaultRepository(t *testing.T) {
	// readsOnly fails the test unless the shared clone and the issue's workspace each print the
	// one line want from `git config --get-regexp '^remote\..*\.gh-resolved$'`, the read gh makes
	// of every remote's default marker (cli/cli git/client.go, populateResolvedRemotes). git exits
	// 1, printing nothing, where no key matches.
	readsOnly := func(t *testing.T, working Workspace, want string) {
		t.Helper()
		for _, place := range []struct{ name, dir string }{
			{"the shared clone", working.Clone},
			{"the issue's workspace", working.Dir},
		} {
			if output, code := gitConfigRead(t, place.dir, "config", "--get-regexp", `^remote\..*\.gh-resolved$`); output != want || code != 0 {
				t.Errorf("%s %s reads its gh-resolved lines as %q (exit %d), want %q", place.name, place.dir, output, code, want)
			}
		}
	}
	const marked = "remote.origin.gh-resolved base\n"
	t.Run("from GitHub", func(t *testing.T) {
		run := newLocalRunner(t)
		working, err := Provision(context.Background(), run, provisionRequest(t))
		if err != nil {
			t.Fatalf("provision from GitHub: %v", err)
		}
		readsOnly(t, working, marked)
	})
	t.Run("over a marker the clone holds", func(t *testing.T) {
		run := newLocalRunner(t)
		request := provisionRequest(t)
		working, err := Provision(context.Background(), run, request)
		if err != nil {
			t.Fatalf("first provision: %v", err)
		}
		runSetup(t, working.Clone, "git", "config", "--unset", "remote.origin.gh-resolved")
		runSetup(t, working.Clone, "git", "config", "remote.upstream.gh-resolved", "base")
		if _, err := Provision(context.Background(), run, request); err != nil {
			t.Fatalf("provision the existing workspace: %v", err)
		}
		readsOnly(t, working, "remote.upstream.gh-resolved base\n")
	})
}
