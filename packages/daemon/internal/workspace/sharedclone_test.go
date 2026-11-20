package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
