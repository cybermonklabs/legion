// Package rescuetest builds an issue pod's disk for the push check's tests: a bare repository
// standing in for GitHub, and on the disk a jj clone of it with the issue's jj workspace, laid out
// as workspace.Location lays a pod's out. Every command is the real git and jj from PATH, under
// fixtureenv.Isolated: no workspace, configuration, remote or credential of whoever runs the tests.
package rescuetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Owner and Repo are the repository the disk's clone is of.
const (
	Owner = "acme"
	Repo  = "widgets"
)

// The stand-in shared public leak checks a disk's pushes are checked against, which are
// leakguardtest's. The real check is a script no clone of this repository holds, and the push
// check refuses a push it can run no check for, so every disk carries one: Passes to begin with,
// and Refuses for a test that wants the push refused (Disk.LeakCheck). They are named here so
// that a test of the push check writes a disk's stand-in in the disk's own words.
const (
	Passes  = leakguardtest.Passes
	Refuses = leakguardtest.Refuses
)

// Disk is one pod's disk and the remote its clone pushes to.
type Disk struct {
	t testing.TB
	// leakCheckDir holds the stand-in shared public leak check PUBLIC_LEAK_CHECK names in Env,
	// whose body LeakCheck writes, and the file RecordUpdates's stand-in records to.
	leakCheckDir string
	// Remote is the bare repository standing in for GitHub, which a disk NextPod makes shares.
	Remote string
	// StateDir is the disk's root, a pod's /legion.
	StateDir string
	// Clone is the shared clone, a colocated jj repository whose origin is Remote.
	Clone string
	// Workspace is the issue's jj workspace of Clone.
	Workspace string
	// Issue is the issue key, and Branch its branch, which Remote holds at the base commit.
	Issue, Branch string
	// Base is the commit Remote's main and the issue's branch both hold when the remote is seeded,
	// which for a disk NextPod makes is its first disk's start.
	Base string
	// Env is what every command of the disk runs in, and what the push check runs in.
	Env []string
	// Provisioned is when the pod was made: New and NextPod take it before anything else, and
	// record it as the workspace's creation in the workspace's .jj (workspace.RecordCreated), which
	// the launcher's resume check reads.
	Provisioned time.Time
}

// LeakCheck writes script as the body of the stand-in shared public leak check this disk's
// pushes are checked against, the one PUBLIC_LEAK_CHECK names in Env: Passes, Refuses, or a
// script of the test's own. A disk starts with Passes, and a call of this before a check
// replaces it, since the guard resolves the check again at every call.
func (d *Disk) LeakCheck(script string) {
	d.t.Helper()
	leakguardtest.Write(d.t, d.leakCheckDir, script)
}

// RecordUpdates makes this disk's stand-in shared public leak check one that records what each
// push would move and clears every push: it appends its standard input, the update lines
// `<local ref> <local sha> <remote ref> <remote sha>` a pre-push hook is handed, to a file of this
// disk's own, and exits 0. read answers the lines recorded since this call, one string per line,
// blank lines dropped. It replaces the stand-in as LeakCheck does, so a later LeakCheck ends the
// recording.
//
// One push can run the check more than once. The push check runs it before it pushes (the guard
// over its own updates), and where the git on PATH is a pod's wrapper (packages/daemon/docker/git),
// as it is when these tests run in a Legion pod, git's push runs the deployed pre-push hook, which
// runs this same stand-in over git's own update lines. So a caller asserts on the lines recorded,
// not on how many times the check ran or how many lines there are.
func (d *Disk) RecordUpdates() (read func() []string) {
	d.t.Helper()
	record := filepath.Join(d.leakCheckDir, "updates")
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		d.t.Fatal(err)
	}
	// The path is in the script itself, so the record does not hang on the environment the guard
	// and the hook each hand the check.
	d.LeakCheck("cat >>'" + record + "'\nexit 0\n")
	return func() []string {
		d.t.Helper()
		body, err := os.ReadFile(record)
		if err != nil {
			d.t.Fatal(err)
		}
		var lines []string
		for line := range strings.Lines(string(body)) {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// New is a disk whose clone is jj's own colocated clone (`jj git clone --colocate`, which checks
// out a working tree), whose remote holds one commit on main and on the issue's branch, and whose
// workspace stands on that commit with nothing in it the remote lacks. Each of before runs once the
// remote holds that commit and before the pod's clone is made, so what it pushes to the remote the
// clone holds from the start.
func New(t testing.TB, issue string, before ...func(d *Disk)) *Disk {
	t.Helper()
	return newDisk(t, issue, before, cloneColocated, addWorkspace)
}

// cloneColocated is New's clone of the remote: jj's own colocated clone, which checks out a
// working tree. root is the directory the disk is laid out under (provision).
func cloneColocated(d *Disk, root string) {
	d.t.Helper()
	d.Run(root, "jj", "git", "clone", "--colocate", d.Remote, d.Clone)
}

// addWorkspace is New's issue workspace: a plain `jj workspace add` of the clone.
func addWorkspace(d *Disk) {
	d.t.Helper()
	d.Run(d.Clone, "jj", "workspace", "add", "--name", strings.ToLower(d.Issue), d.Workspace)
}

// NewWithoutCloneTree is a disk whose clone holds no working tree, as an issue pod's clone does
// (workspace.feedClone): a `git clone --no-checkout` of the feed, made jj's with `jj git init
// --colocate`, its sparse patterns cleared and the snapshot that init took abandoned. What that
// leaves is a clone directory holding nothing but .git and .jj, while its git HEAD still names the
// default branch's commit and its index still names every file of it. The clone here is made from
// the remote itself, so the `git remote set-url origin` with which feedClone points the clone at
// GitHub rather than the feed has nothing to change and is not run; every other step is the pod's,
// in the pod's order, which is load-bearing (feedClone says why).
//
// The issue's workspace is added as provisioning adds it (workspace.createWorkspace and Provision):
// registered with no file (`jj workspace add --sparse-patterns empty`), which leaves the clone's
// own working copy untouched, and checked out after (`jj sparse reset`). Everything else — the
// remote, the before hooks, the records provisioning writes — is New's.
func NewWithoutCloneTree(t testing.TB, issue string, before ...func(d *Disk)) *Disk {
	t.Helper()
	d := newDisk(t, issue, before,
		func(d *Disk, root string) {
			d.Run(root, "git", "clone", "--quiet", "--no-checkout", d.Remote, d.Clone)
			d.Run(root, fixturegit.JJInit(d.Clone, true)...)
			d.Run(d.Clone, "jj", "sparse", "set", "--clear", "--color=never", "-R", d.Clone)
			d.Run(d.Clone, "jj", "abandon", "@", "--color=never", "-R", d.Clone)
		},
		func(d *Disk) {
			d.Run(d.Clone, "jj", "workspace", "add", d.Workspace, "--name", strings.ToLower(d.Issue),
				"--revision", d.Base, "--sparse-patterns", "empty", "-R", d.Clone)
			d.Run(d.Workspace, "jj", "sparse", "reset")
		})
	d.requireNoCloneTree()
	return d
}

// NextPod is the disk of the pod that replaces this one: a disk of the same issue over the same
// Remote, made as New makes one from the remote as it stands at this call. It has its own
// StateDir, clone and workspace, its own environment and stand-in leak check (Passes), and the
// records provisioning writes, with Provisioned the time of this call. The remote is neither made
// again nor seeded, and no before hook runs, so a test makes the next pod before this disk's
// rescue, for a clone that lacks what the rescue pushes, or after it, for a clone that holds it.
// Base is this disk's: the commit the remote was seeded with, which the remote's branches need no
// longer hold.
func (d *Disk) NextPod(t testing.TB) *Disk {
	t.Helper()
	next, root := layOut(t, d.Issue)
	next.Remote, next.Base = d.Remote, d.Base
	next.provision(root, cloneColocated, addWorkspace)
	return next
}

// newDisk is what the two constructors share: a disk laid out (layOut), the bare remote holding one
// commit on main and on the issue's branch, the before hooks run on it, and the pod provisioned
// from it (provision) with clone and add, which make the shared clone and add the issue's
// workspace of it, each the way its constructor builds one.
func newDisk(t testing.TB, issue string, before []func(d *Disk), clone func(d *Disk, root string), add func(d *Disk)) *Disk {
	t.Helper()
	d, root := layOut(t, issue)
	d.Remote = filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	d.Run(root, fixturegit.InitBare(d.Remote, "main")...)
	d.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	d.Write(seed, "base.txt", "base\n")
	d.Run(seed, "git", "add", "base.txt")
	d.Run(seed, "git", "commit", "--quiet", "-m", "base")
	d.Run(seed, "git", "push", "--quiet", d.Remote, "main", "main:refs/heads/"+d.Branch)
	d.Base = d.Run(seed, "git", "rev-parse", "HEAD")
	for _, seedOrigin := range before {
		seedOrigin(d)
	}
	d.provision(root, clone, add)
	return d
}

// layOut is one pod's disk with nothing on it yet, and the directory it is laid out under: a home
// of its own and the isolated environment over it, the paths the pod's clone and workspace go at
// (workspace.Location), the stand-in leak check (Passes) Env names, and Provisioned, taken here.
// Remote and Base are the caller's to set.
func layOut(t testing.TB, issue string) (*Disk, string) {
	t.Helper()
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the push check's tests drive a real %s: %v", tool, err)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Disk{
		t:            t,
		leakCheckDir: root,
		StateDir:     filepath.Join(root, "legion"),
		Issue:        issue,
		Branch:       "legion/" + issue,
		Env:          fixtureenv.Isolated(home),
		Clone:        filepath.Join(root, "legion", "repos", "github.com", Owner, Repo),
		Workspace:    filepath.Join(root, "legion", "workspaces", Owner, Repo, strings.ToLower(issue)),
	}
	d.Env = append(d.Env, "PUBLIC_LEAK_CHECK="+leakguardtest.Write(t, d.leakCheckDir, Passes))
	d.Provisioned = time.Now()
	return d, root
}

// provision makes the pod's shared clone of Remote with clone and the issue's workspace of it with
// add, and writes the record provisioning writes of when the workspace was created
// (workspace.RecordCreated), with the issue's bookmark tracked. root, which clone is given, is the
// directory the disk is laid out under, where a jj that opens no repository runs, as provisioning
// runs those in the clone's parent (workspace.cloneSteps).
func (d *Disk) provision(root string, clone func(d *Disk, root string), add func(d *Disk)) {
	d.t.Helper()
	if err := os.MkdirAll(filepath.Dir(d.Clone), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(d.Workspace), 0o755); err != nil {
		d.t.Fatal(err)
	}
	clone(d, root)
	add(d)
	if err := workspace.RecordCreated(d.Workspace, d.Provisioned); err != nil {
		d.t.Fatal(err)
	}
	d.JJ("bookmark", "track", d.Branch, "--remote", "origin")
}

// requireNoCloneTree fails the test when the clone holds anything but .git and .jj. The state
// NewWithoutCloneTree stands for is a clone with no working tree, so a jj whose sparse clear or
// abandon stops leaving one is reported here, where it happens and by what it left, rather than as
// a confusing assertion in the case that follows.
func (d *Disk) requireNoCloneTree() {
	d.t.Helper()
	entries, err := os.ReadDir(d.Clone)
	if err != nil {
		d.t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != ".git" && name != ".jj" {
			d.t.Fatalf("the clone %s holds %s, want nothing but .git and .jj: this disk's clone must hold no working tree", d.Clone, name)
		}
	}
}

// Run runs argv in dir under the disk's environment and returns its trimmed output, failing the
// test when it fails.
func (d *Disk) Run(dir string, argv ...string) string {
	d.t.Helper()
	return fixturegit.Runner{TB: d.t, Env: d.Env}.Run(dir, argv...)
}

// JJ runs jj in the workspace, as a role does: its working copy snapshotted first.
func (d *Disk) JJ(args ...string) string {
	d.t.Helper()
	return d.Run(d.Workspace, append([]string{"jj"}, args...)...)
}

// Git runs git in the workspace.
func (d *Disk) Git(args ...string) string {
	d.t.Helper()
	return d.Run(d.Workspace, append([]string{"git"}, args...)...)
}

// CommitLine makes n commits on the issue's branch with one git fast-import, and returns their
// ids, oldest first: a line as long as a test needs, made in one process rather than in one
// command for each commit. Commit i, counted from 1, has subject(i) as its one line of message and
// writes `work <i>` to work.txt.
func (d *Disk) CommitLine(n int, subject func(i int) string) []string {
	d.t.Helper()
	ref := "refs/heads/" + d.Branch
	base := d.Git("rev-parse", ref)
	now := time.Now().Unix()
	var stream strings.Builder
	for i := 1; i <= n; i++ {
		message := subject(i) + "\n"
		body := fmt.Sprintf("work %d\n", i)
		fmt.Fprintf(&stream, "commit %s\nmark :%d\ncommitter Legion Test <test@example.invalid> %d +0000\ndata %d\n%s", ref, i, now, len(message), message)
		if i == 1 {
			fmt.Fprintf(&stream, "from %s\n", base)
		}
		fmt.Fprintf(&stream, "M 100644 inline work.txt\ndata %d\n%s\n", len(body), body)
	}
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir, cmd.Env, cmd.Stdin = d.Workspace, d.Env, strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		d.t.Fatalf("git fast-import of %d commits: %v\n%s", n, err, out)
	}
	line := strings.Fields(d.Git("rev-list", "--reverse", base+".."+ref))
	if len(line) != n {
		d.t.Fatalf("the issue's branch holds %d commits over its base, want the %d made", len(line), n)
	}
	return line
}

// Write writes a file under dir.
func (d *Disk) Write(dir, name, body string) {
	d.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

// RemoteBranch is the commit the remote's branch holds, and "" when the remote has no such branch.
func (d *Disk) RemoteBranch(branch string) string {
	d.t.Helper()
	cmd := exec.Command("git", "--git-dir="+d.Remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Env = d.Env
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// RemoteReaches says whether a ref of the remote reaches object, a commit, tree or blob: whether
// anyone who reads the remote can read it. An object the remote still stores once no ref reaches
// it, such as a commit of a branch deleted there, is not reached.
func (d *Disk) RemoteReaches(object string) bool {
	d.t.Helper()
	for line := range strings.Lines(d.Run(d.Remote, "git", "rev-list", "--objects", "--all")) {
		if id, _, _ := strings.Cut(strings.TrimSpace(line), " "); id == object {
			return true
		}
	}
	return false
}

// RemoteBranches is every branch the remote has.
func (d *Disk) RemoteBranches() []string {
	d.t.Helper()
	return strings.Fields(d.Run(d.Remote, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads"))
}

// RefuseWrites makes the remote refuse every push, as a remote that refuses the role's credential
// would: its pre-receive hook says why and fails. Reads of the remote still work. The hook is the
// remote's own, which git runs on the remote's side whatever configuration the pushing side pins.
func (d *Disk) RefuseWrites() {
	d.t.Helper()
	hook := filepath.Join(d.Remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'the remote refuses this push' >&2\nexit 1\n"), 0o755); err != nil {
		d.t.Fatal(err)
	}
}

// Check runs the push check over the disk as a role's shim runs it (shim.rescue), under the
// disk's environment. prefix and target are the request's Branch and Target (rescue.Request): the
// per-pod prefix an older shim names its branches from, and the issue's one rescue branch, which
// is "" for a request that names none, as an older daemon's does. The check then pushes to prefix.
//
// The bounds are the check's own, the ones the daemon sets on every request
// (rescue.WorkspaceTimeout and rescue.Timeout, derived from rescue.GuardReserve): the guard's
// call keeps that reserve of the workspace's bound for the push it clears, wide enough for the
// second run of the check a pod's pre-push hook makes, so a fixture bounded under it would have
// every push here refused for want of time rather than checked. Nothing waits these out — they
// are what a check may spend, and these tests spend milliseconds.
func (d *Disk) Check(ctx context.Context, prefix, target string) []shimwire.RescueWorkspace {
	d.t.Helper()
	return rescue.Run(ctx, rescue.Request{
		StateDir: d.StateDir, Branch: prefix, Target: target, Env: d.Env,
		WorkspaceTimeout: rescue.WorkspaceTimeout, Deadline: time.Now().Add(rescue.Timeout),
	})
}
