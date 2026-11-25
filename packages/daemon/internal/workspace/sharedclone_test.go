package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// Provisioning from a feed, as `workspace-init provision` does in a pod, marks origin as gh's
// default repository in the shared clone (configureCloneDefaultRepository) and turns git's
// ambiguous-ref warning off there (configureCloneReads): the clone and the issue's workspace each
// read one gh-resolved line, `remote.origin.gh-resolved base`, and core.warnAmbiguousRefs as
// false. The feed is left as Fetch wrote it: it reads no core.warnAmbiguousRefs, and the next
// Fetch keeps it, since its configuration still holds only feedConfigKeys, which name neither key.
func TestProvisionFromAFeedMarksOriginAndTurnsOffTheAmbiguousRefWarningLeavingTheFeedAlone(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the feed: %v", err)
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
	// git exits 1, printing nothing, for a key no configuration sets.
	if output, code := gitConfigRead(t, first.Feed, "--git-dir="+first.Feed, "config", "--get", "core.warnAmbiguousRefs"); output != "" || code != 1 {
		t.Errorf("the feed %s reads core.warnAmbiguousRefs as %q (exit %d), want nothing and exit 1", first.Feed, output, code)
	}
	// Fetch removes a feed whose configuration holds a key outside feedConfigKeys and clones it
	// again whole, so a kept feed is one that passed the check.
	again, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("Fetch after provisioning: %v", err)
	}
	if again.Cloned {
		t.Errorf("Fetch after provisioning reported %+v: it cloned the feed again whole, want the feed kept", again)
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
// GIT_CONFIG variable removed and GIT_CONFIG_NOSYSTEM set. After a provision from a feed, as
// `workspace-init provision` does in a pod, a git in the issue's workspace under such an
// environment (the fixtures': no GIT_CONFIG variable, GIT_CONFIG_NOSYSTEM=1,
// GIT_CONFIG_GLOBAL=/dev/null) reads each of them from the local scope, the clone's own file. A
// value a role changed there is set back at the next provision. The feed holds none of them,
// since Fetch would refuse a feed whose configuration did (feedConfigKeys).
func TestTheSharedCloneMapsPacksInSmallWindows(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	fed, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
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
		t.Fatalf("provision from the feed: %v", err)
	}
	readsAll(t, working)
	runSetup(t, working.Clone, "git", "--git-dir="+filepath.Join(working.Clone, ".git"), "config", "core.packedGitLimit", "32g")
	again, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	readsAll(t, again)
	for _, setting := range packWindows {
		// git exits 1, printing nothing, for a key no configuration sets.
		if output, code := gitConfigRead(t, fed.Feed, "--git-dir="+fed.Feed, "config", "--get", setting[0]); output != "" || code != 1 {
			t.Errorf("the feed %s reads %s as %q (exit %d), want nothing and exit 1", fed.Feed, setting[0], output, code)
		}
	}
}

// noMaintenance are the two settings that keep a git in the shared clone from starting git's
// automatic maintenance, written out here rather than read from the list provisioning writes, so
// that a key taken out of that list, or a value changed in it, fails each test that holds a git to
// them.
var noMaintenance = [][2]string{
	{"gc.auto", "0"},
	{"maintenance.auto", "false"},
}

// The shared clone's own configuration turns git's automatic maintenance off, for every git a role
// runs in the clone or in the issue's workspace, the git jj runs for a fetch among them: neither a
// role's GIT_CONFIG pairs (sandbox's gitConfigPairs) nor the worker image's /etc/gitconfig carries
// either setting, so the clone's file is the one place a role's git reads them from. After a
// provision from a feed, as `workspace-init provision` does in a pod, a git in the issue's
// workspace reads each from the local scope, the clone's own file. A clone made before the
// settings existed holds neither, as does the clone of a seed volume that new issue volumes are
// restored from, and a role may have changed either: the next provision writes both again, so such
// a clone takes them at the seed's next refresh or at its pod's next start, which each provision it.
func TestTheSharedCloneTurnsOffGitsAutomaticMaintenance(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	if _, err := Fetch(context.Background(), run, fetch); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	readsAll := func(t *testing.T, working Workspace) {
		t.Helper()
		for _, setting := range noMaintenance {
			if output, code := gitConfigRead(t, working.Dir, "config", "--show-scope", "--get", setting[0]); output != "local\t"+setting[1]+"\n" || code != 0 {
				t.Errorf("the issue's workspace %s reads %s as %q (exit %d), want %s from the clone's own configuration", working.Dir, setting[0], output, code, setting[1])
			}
		}
	}
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the feed: %v", err)
	}
	readsAll(t, working)
	gitDir := "--git-dir=" + filepath.Join(working.Clone, ".git")
	runSetup(t, working.Clone, "git", gitDir, "config", "--unset-all", "gc.auto")
	runSetup(t, working.Clone, "git", gitDir, "config", "maintenance.auto", "true")
	again, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	readsAll(t, again)
}

// A role's `git fetch` in the issue's workspace starts no git maintenance, however far past its
// limits the clone is. On 2026-11-25 one deployment's issue pods filled their volumes this way:
// each pod's clone held more than gc.autoPackLimit's 50 packs, so a role's fetch of one commit
// started `git maintenance run --auto`, then `git gc --auto`, then a `git repack -d -l --cruft` of
// the whole 41 GB clone, which ran out of room and left its temporary pack behind (INFRA-5727).
// Here gc.autoPackLimit 1 and a fetch that keeps its pack (fetch.unpackLimit 1) make a gc due on
// the fixture's small clone, and gc.autoDetach and maintenance.autoDetach false hold any such run
// in the foreground, so each of its gits is in the fetch's trace before the fetch returns. The
// fetch runs under the fixtures' environment, which reads no system or global file, so the
// clone's own configuration is all that stands between it and the gc.
func TestARolesFetchInTheSharedCloneStartsNoGitMaintenance(t *testing.T) {
	run := newLocalRunner(t)
	fetch := fetchRequest(t)
	if _, err := Fetch(context.Background(), run, fetch); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the feed: %v", err)
	}
	gitDir := filepath.Join(working.Clone, ".git")
	for _, pair := range [][2]string{{"gc.autoPackLimit", "1"}, {"gc.autoDetach", "false"}, {"maintenance.autoDetach", "false"}} {
		runSetup(t, working.Clone, "git", "--git-dir="+gitDir, "config", pair[0], pair[1])
	}
	// The fixture's clone holds its few objects loose, so they are packed first: then the clone
	// holds a pack, and the fetch keeps its own (an index-pack in its trace, not an
	// unpack-objects), two packs at its end, past gc.autoPackLimit 1, so a gc is due there.
	// Without both, the gc's absence below would say nothing of the clone's configuration.
	runSetup(t, working.Clone, "git", "--git-dir="+gitDir, "repack", "-d", "--quiet")
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) == 0 {
		t.Fatalf("the clone holds no pack before the fetch, so the fetch's pack alone stays within gc.autoPackLimit 1")
	}
	advanceRemote(t, run.remote)
	trace := filepath.Join(t.TempDir(), "trace2.json")
	runSetupWith(t, working.Dir, []string{
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url." + run.remote + ".insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/acme/widgets",
		"GIT_TRACE2_EVENT=" + trace,
	}, "git", "-c", "fetch.unpackLimit=1", "fetch", "--quiet", "origin")

	body, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read the fetch's git trace: %v", err)
	}
	fetched, kept := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode the git trace event %q: %v", line, err)
		}
		if event.Event == "start" && slices.Contains(event.Argv, "fetch") {
			fetched = true
		}
		if event.Event == "start" && slices.Contains(event.Argv, "index-pack") {
			kept = true
		}
		if slices.Contains(event.Argv, "maintenance") || slices.Contains(event.Argv, "gc") || slices.Contains(event.Argv, "repack") {
			t.Errorf("the role's fetch traced %s of %q: git's automatic maintenance ran in the shared clone", event.Event, strings.Join(event.Argv, " "))
		}
	}
	if !fetched || !kept {
		t.Fatalf("the git trace holds a git fetch: %t, and an index-pack keeping its pack: %t; without both no gc was due, and it says nothing of the fetch's maintenance:\n%s", fetched, kept, body)
	}
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
// (configureCloneDefaultRepository), on both runtimes' paths: from GitHub, as the tmux daemon
// does, and from a pod's feed, as `workspace-init provision` does, whose first provision
// TestProvisionFromAFeedMarksOriginAndTurnsOffTheAmbiguousRefWarningLeavingTheFeedAlone reads.
// The clone and the issue's workspace each read one gh-resolved line, `remote.origin.gh-resolved
// base`. A clone that already holds a marker, here the one `gh repo fork --remote` leaves on the
// remote it renames origin to, keeps it at its next provision and gets no origin line beside it.
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
		request := feedRequest(t, run, provisionRequest(t))
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
