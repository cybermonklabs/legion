package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// Provisioning from a feed, as `workspace-init provision` does in a pod, turns git's ambiguous-ref
// warning off in the shared clone (configureCloneReads): the clone and the issue's workspace both
// read core.warnAmbiguousRefs as false. The feed is left as Fetch wrote it: it reads no such key,
// and the next Fetch keeps it, since its configuration still holds only feedConfigKeys.
func TestProvisionTurnsOffTheClonesAmbiguousRefWarningAndLeavesTheFeedAlone(t *testing.T) {
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
	// read runs `git <args> config --get core.warnAmbiguousRefs` in dir under the fixtures'
	// environment and returns what it printed and its exit code. git exits 1, printing nothing,
	// for a key no configuration sets.
	read := func(dir string, args ...string) (string, int) {
		t.Helper()
		command := exec.Command("git", append(args, "config", "--get", "core.warnAmbiguousRefs")...)
		command.Dir = dir
		command.Env = setupEnvironment()
		output, err := command.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("git config --get core.warnAmbiguousRefs in %s: %v", dir, err)
		}
		return string(output), command.ProcessState.ExitCode()
	}
	for _, place := range []struct{ name, dir string }{
		{"the shared clone", working.Clone},
		{"the issue's workspace", working.Dir},
	} {
		if output, code := read(place.dir); output != "false\n" || code != 0 {
			t.Errorf("%s %s reads core.warnAmbiguousRefs as %q (exit %d), want false", place.name, place.dir, output, code)
		}
	}
	if output, code := read(first.Feed, "--git-dir="+first.Feed); output != "" || code != 1 {
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
