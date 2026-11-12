package rescue_test

// The route the check takes through a worktree jj holds: jj's own snapshot of that worktree, in
// place of the whole-tree git read (rescue.edits, rescue.snapshotTree). Two kinds of case here.
//
// The route itself: what the check pushes, what it ran to read the files, and what it wrote in
// the repository. Every assertion is on the check's own answer, on the commands it started
// (rescue.ObserveCommands) or on the state of the repositories afterwards; nothing here stands
// in for git or jj, which are the machine's own in these tests.
//
// And what the route still finds, file by file, where the git read it replaces found something:
// a file above the size limit a workspace configures, the files jj refuses, an untracked nested
// git repository, a workspace narrowed to part of its tree, and a conflicted working copy.
//
// The numbers these cases stand on were measured on this build, jj 0.45.1-sami.20260910 and git
// 2.47.3, and each is noted where it is asserted.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// ran is one git or jj the check started, as rescue.ObserveCommands reports it.
type ran struct {
	dir, name string
	args      []string
}

// commands is every git and jj one check started, in the order it started them.
type commands struct {
	mu  sync.Mutex
	all []ran
}

// recorded records every git and jj the check starts for the rest of t. It is the one place a
// test reads them: no test of this package puts a stand-in binary on PATH, since the git and jj
// these cases drive are the machine's own.
func recorded(t *testing.T) *commands {
	t.Helper()
	c := &commands{}
	rescue.ObserveCommands(t, func(dir, name string, args []string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.all = append(c.all, ran{dir: dir, name: name, args: slices.Clone(args)})
	})
	return c
}

// in is every command the check ran in dir.
func (c *commands) in(dir string) []ran {
	c.mu.Lock()
	defer c.mu.Unlock()
	var there []ran
	for _, r := range c.all {
		if r.dir == dir {
			there = append(there, r)
		}
	}
	return there
}

// snapshotPins is the configuration the snapshot puts on its own command line, which outranks
// the repository's, the workspace's and the user's configuration files (rescue.snapshot). Six
// hold this build's own default; the last two are the package's own, and the size limit is read
// here as the text it is pinned with, 64 GiB, which is the raised limit the whole route rests on
// — a file a workspace's own configuration would refuse is carried because of it.
func snapshotPins(t *testing.T) []string {
	t.Helper()
	// The git the check runs is the one this process would run: the package resolves it with
	// exec.LookPath and pins it as the git jj itself starts.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the check pins the git it runs as jj's own, and this process has none on its PATH: %v", err)
	}
	return []string{
		`fsmonitor.backend="none"`,
		`git.filter.enabled=false`,
		`signing.backend="none"`,
		`signing.behavior="keep"`,
		`snapshot.auto-track="all()"`,
		`snapshot.auto-update-stale=false`,
		`snapshot.max-new-file-size="64GiB"`,
		`git.executable-path="` + git + `"`,
	}
}

// carries is whether args holds flag followed by value, which is how a pin is read as a value
// and not only as a key.
func carries(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// theSnapshotOf is the one jj snapshot the check took of dir, with every pin on its command line
// read as a value. A worktree jj holds is snapshotted once and no more: the snapshot leaves the
// working copy as the last jj command of a role wrote it, and no retry of the check's makes a
// lock or a stale working copy newer.
func theSnapshotOf(t *testing.T, c *commands, dir string) ran {
	t.Helper()
	var snapshots []ran
	for _, r := range c.in(dir) {
		if r.name == "jj" && slices.Contains(r.args, "snapshot") {
			snapshots = append(snapshots, r)
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("the check took %d jj snapshots of %s, want one: %v", len(snapshots), dir, snapshots)
	}
	snapshot := snapshots[0]
	if tail := snapshot.args[len(snapshot.args)-2:]; !slices.Equal(tail, []string{"util", "snapshot"}) {
		t.Errorf("the check's snapshot of %s ends in %v, want `util snapshot`: %v", dir, tail, snapshot.args)
	}
	for _, pin := range snapshotPins(t) {
		if !carries(snapshot.args, "--config", pin) {
			t.Errorf("the check's snapshot of %s carries no `--config %s`: %v", dir, pin, snapshot.args)
		}
	}
	for _, suppressed := range []string{"--ignore-working-copy", "--at-op=@"} {
		if slices.Contains(snapshot.args, suppressed) {
			t.Errorf("the check's snapshot of %s carries %s, which suppresses the snapshot: %v", dir, suppressed, snapshot.args)
		}
	}
	return snapshot
}

// readsTheWorktreeThroughJJAlone asserts the check read dir's files through jj's snapshot and
// through nothing of git's: no `add` and no `update-index`, which the whole-tree git read it
// replaces runs, and no `status`. Every other jj it ran there carries --ignore-working-copy, so
// it reads jj's view and snapshots nothing.
func readsTheWorktreeThroughJJAlone(t *testing.T, c *commands, dir string) {
	t.Helper()
	snapshot := theSnapshotOf(t, c, dir)
	for _, r := range c.in(dir) {
		if r.name == "git" {
			for _, read := range []string{"add", "update-index", "status"} {
				if slices.Contains(r.args, read) {
					t.Errorf("the check ran `git %s` over %s, which is the whole-tree read jj's snapshot replaces: %v", read, dir, r.args)
				}
			}
		}
		if r.name == "jj" && !slices.Equal(r.args, snapshot.args) && !slices.Contains(r.args, "--ignore-working-copy") {
			t.Errorf("the check ran jj over %s without --ignore-working-copy, which is a second snapshot of the working copy: %v", dir, r.args)
		}
	}
}

// refsOf is every ref the clone holds, by name. The clone is where jj writes what a snapshot
// keeps, and where a push updates what it tracks.
func refsOf(t *testing.T, d *rescuetest.Disk) map[string]string {
	t.Helper()
	held := map[string]string{}
	for line := range strings.Lines(d.Run(d.Clone, "git", "for-each-ref", "--format=%(refname) %(objectname)")) {
		if name, sha, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			held[name] = sha
		}
	}
	return held
}

// theRefsTheCheckWrote asserts the check moved no ref of the clone and deleted none — no
// refs/heads/* of the clone among them — and that every ref it added is either jj's own, under
// refs/jj/, which is what the snapshot writes, or the remote-tracking ref of a branch this check
// pushed, which git writes with the push itself. It answers the commits the refs/jj/ ones name,
// sorted.
//
// Measured on this build, `git push origin <sha>:refs/heads/<name>` in the clone also writes
// refs/remotes/origin/<name>: it names the branch the push just made and loses nothing.
func theRefsTheCheckWrote(t *testing.T, before, after map[string]string, pushed []shimwire.RescueBranch) []string {
	t.Helper()
	tracking := map[string]string{}
	for _, b := range pushed {
		tracking["refs/remotes/origin/"+b.Name] = b.Commit
	}
	var jj []string
	for name, sha := range after {
		was, held := before[name]
		switch {
		case held && was != sha:
			t.Errorf("the check moved %s from %s to %s: it writes no ref of the clone but jj's own", name, was, sha)
		case held:
		case strings.HasPrefix(name, "refs/jj/"):
			jj = append(jj, sha)
		case tracking[name] == sha:
		default:
			t.Errorf("the check added %s at %s, which is neither jj's own nor the remote-tracking ref of a branch it pushed", name, sha)
		}
	}
	for name, sha := range before {
		if _, held := after[name]; !held {
			t.Errorf("the check deleted %s, which held %s", name, sha)
		}
	}
	slices.Sort(jj)
	return jj
}

// workingCopy is the commit jj holds the workspace's working copy at, and workingCopyParent its
// parent, each read as the check reads jj: at the operation the store is at, snapshotting
// nothing of its own.
func workingCopy(t *testing.T, d *rescuetest.Disk) string {
	t.Helper()
	return jjRead(t, d, "@")
}

func workingCopyParent(t *testing.T, d *rescuetest.Disk) string {
	t.Helper()
	return jjRead(t, d, "@-")
}

func jjRead(t *testing.T, d *rescuetest.Disk, revision string) string {
	t.Helper()
	return d.Run(d.Workspace, "jj", "--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never",
		"-R", d.Workspace, "log", "--no-graph", "-r", revision, "-T", "commit_id")
}

// rescuedBySnapshot is what the snapshot route is read by, over a workspace whose files hold
// edits that no commit holds: the check pushes one commit, whose tree is the working copy jj
// snapshotted and whose parent is the worktree's own git HEAD; it read that worktree through jj
// alone; the workspace's files and its git HEAD are what the role left; and the only refs it
// wrote in the clone are jj's own and the pushed branch's. It answers the commits those jj refs
// name.
func rescuedBySnapshot(t *testing.T, d *rescuetest.Disk, edits map[string]string) []string {
	t.Helper()
	head := d.Git("rev-parse", "HEAD")
	before := refsOf(t, d)
	started := recorded(t)

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch", w)
	}
	commit := w.Branches[0].Commit
	if held := d.RemoteBranch(w.Branches[0].Name); held != commit {
		t.Errorf("the remote holds %s at %q, want the commit %s the check reported", w.Branches[0].Name, held, commit)
	}
	if parent := d.Run(d.Remote, "git", "rev-parse", commit+"^"); parent != head {
		t.Errorf("the rescued commit's parent is %s, want the worktree's own git HEAD %s", parent, head)
	}
	wantMessage := "Edits left in the workspace " + d.Workspace + " when its pod was replaced"
	if message := d.Run(d.Remote, "git", "log", "-1", "--format=%B", commit); strings.TrimSpace(message) != wantMessage {
		t.Errorf("the rescued commit says %q, want %q", message, wantMessage)
	}
	if got, want := d.Run(d.Remote, "git", "rev-parse", commit+"^{tree}"), d.Git("rev-parse", workingCopy(t, d)+"^{tree}"); got != want {
		t.Errorf("the rescued commit's tree is %s, want %s, the tree of the working copy jj snapshotted", got, want)
	}
	for file, body := range edits {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != strings.TrimSuffix(body, "\n") {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, body)
		}
	}
	readsTheWorktreeThroughJJAlone(t, started, d.Workspace)
	for file, body := range edits {
		if got, err := os.ReadFile(filepath.Join(d.Workspace, filepath.FromSlash(file))); err != nil || string(got) != body {
			t.Errorf("the workspace's %s is %q (%v) after the check, want %q, as the role left it", file, got, err, body)
		}
	}
	if got := d.Git("rev-parse", "HEAD"); got != head {
		t.Errorf("the workspace's git HEAD is %s after the check, want %s", got, head)
	}
	return theRefsTheCheckWrote(t, before, refsOf(t, d), w.Branches)
}

// requireIndexWithoutStatData fails the test where the workspace's colocated git index carries
// stat data. The fixture is the pod's own state: an index jj wrote and no git command has
// refreshed since, every entry of it carrying ctime, mtime, dev, ino, uid, gid and size 0, which
// is why the git read this route replaces hashes every tracked file of a pod's workspace
// (rescue.worktreeRead).
func requireIndexWithoutStatData(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	entries := d.Git("ls-files", "--debug")
	if !strings.Contains(entries, "base.txt") {
		t.Fatalf("the workspace's index names no file: %q", entries)
	}
	for line := range strings.Lines(entries) {
		if field := strings.TrimSpace(line); strings.HasPrefix(field, "mtime:") && field != "mtime: 0:0" {
			t.Fatalf("the workspace's index carries stat data (%q), want jj's own, which carries none: %q", field, entries)
		}
	}
}

// Files written in the workspace and in no commit become one commit on top of the workspace's
// head, whose tree is the one jj's snapshot wrote: the workspace's own git HEAD and its files are
// what they were, no git read of that worktree was made, and the only refs the check wrote in the
// clone are jj's own. The index and jj's operation log are not among what is unchanged: the
// snapshot writes a jj operation, a working-copy commit and the colocated git index (rescue.edits).
func TestADirtyTreeIsRescuedAsOneCommitWithoutChangingTheWorkspace(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "base.txt", "base, edited\n")
	requireIndexWithoutStatData(t, d)

	jj := rescuedBySnapshot(t, d, map[string]string{
		"notes/draft.txt": "an edit in no commit\n",
		"base.txt":        "base, edited\n",
	})
	// Measured on this build: a snapshot of a quiescent colocated worktree writes one
	// refs/jj/keep ref, naming the working-copy commit it wrote.
	if want := []string{workingCopy(t, d)}; !slices.Equal(jj, want) {
		t.Errorf("the check wrote the jj refs %v, want the one %v naming the working copy its snapshot wrote", jj, want)
	}
}

// The same workspace with its git HEAD moved behind jj's back: a role committed with plain git
// there, so the workspace's HEAD names a commit jj's own view does not hold. That is the
// divergence rescue.check names, and jj's snapshot resets the working copy's parent to that HEAD
// before it reads the files, so the one commit the check pushes stands on the role's git commit
// and carries the edits left beside it.
//
// Measured on this build, with the divergence made both ways — `git commit` in the workspace, as
// TestAnUnpushedGitCommitOnTheWorkspaceHeadIsRescued makes one, and `git checkout --detach` of a
// commit jj does not know — the snapshot answers `Reset the working copy parent to the new Git
// HEAD.` and writes three refs/jj/keep refs either way. The role's own commit is the mechanism
// here: it is what a role does, and the check's text names it.
func TestADivergedGitHeadIsRescuedAsOneCommitWithoutChangingTheWorkspace(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "fix.txt", "a fix made with plain git\n")
	d.Git("add", "fix.txt")
	d.Git("commit", "--quiet", "-m", "a fix made with plain git")
	head := d.Git("rev-parse", "HEAD")
	if parent := workingCopyParent(t, d); parent == head {
		t.Fatalf("jj's view already holds the workspace's git HEAD %s as the working copy's parent, want the two diverged", head)
	}
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "base.txt", "base, edited\n")

	jj := rescuedBySnapshot(t, d, map[string]string{
		"notes/draft.txt": "an edit in no commit\n",
		"base.txt":        "base, edited\n",
		"fix.txt":         "a fix made with plain git\n",
	})
	wc := workingCopy(t, d)
	if parent := workingCopyParent(t, d); parent != head {
		t.Errorf("jj holds the working copy's parent at %s after the check, want the git HEAD %s it reset it to", parent, head)
	}
	// Measured on this build: three refs/jj/keep refs, naming the working-copy commit the
	// snapshot wrote, the git HEAD it imported, and the empty working-copy commit it made on
	// that HEAD and rewrote at once. All three are jj's own and name work nothing else of the
	// clone names.
	if len(jj) != 3 || !slices.Contains(jj, wc) || !slices.Contains(jj, head) {
		t.Errorf("the check wrote the jj refs %v, want three, among them the working copy %s and the git HEAD %s it was reset to", jj, wc, head)
	}
}

// A new file above the size limit the workspace's own configuration sets is rescued. The check
// pins its own limit on the snapshot's command line, above anything a pod's volume holds, and a
// command line outranks every configuration file: this workspace holds jj's default of 1 MiB in
// its repository configuration, a role's own jj leaves the 2 MiB file out for it, and the check
// carries it all the same.
func TestANewFileAboveTheWorkspacesOwnSizeLimitIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1MiB"`)
	if held := d.JJ("config", "list", "--repo"); !strings.Contains(held, `snapshot.max-new-file-size = "1MiB"`) {
		t.Fatalf("the repository's own configuration is %q, want the 1 MiB limit this case stands on", held)
	}
	const big = 2 * 1024 * 1024
	d.Write(d.Workspace, "big.bin", strings.Repeat("b", big))
	// A role's own jj snapshots what it can and leaves the file out for the workspace's limit,
	// so the working copy it writes holds every file of the tree but this one.
	d.JJ("status")
	if tracked := d.Git("ls-tree", "-r", "--name-only", workingCopy(t, d)); strings.Contains(tracked, "big.bin") {
		t.Fatalf("the working copy a role's own jj wrote holds big.bin (%q), want it refused for the workspace's own limit", tracked)
	}
	head := d.Git("rev-parse", "HEAD")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the file jj's own limit refused", w)
	}
	commit := w.Branches[0].Commit
	if parent := d.Run(d.Remote, "git", "rev-parse", commit+"^"); parent != head {
		t.Errorf("the rescued commit's parent is %s, want the workspace's head %s", parent, head)
	}
	if size := d.Run(d.Remote, "git", "cat-file", "-s", commit+":big.bin"); size != strconv.Itoa(big) {
		t.Errorf("the rescued commit's big.bin is %s bytes, want the %d the workspace holds", size, big)
	}
}

// A workspace whose snapshot refused files fails, and nothing of it is pushed: those files are in
// no commit the check can push, and a workspace reported clean would have the pod replaced and
// them lost. The failure names ten of them and counts the rest, which is what keeps the message
// inside what Dispatch takes (rescue.maxRefusedNamed).
//
// The limit is lowered here through the package's own variable, since the snapshot pins it on its
// command line and a command line outranks every configuration file a test could plant.
func TestAWorkspaceWhoseSnapshotRefusedFilesFailsNamingTenOfThem(t *testing.T) {
	rescue.SetSnapshotMaxNewFileSize(t, "1KiB")
	d := rescuetest.New(t, issue)
	var refused []string
	for i := 1; i <= 15; i++ {
		name := fmt.Sprintf("big-%02d.bin", i)
		refused = append(refused, name)
		d.Write(d.Workspace, name, strings.Repeat("x", 4096))
	}
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the workspace failed with nothing pushed", w)
	}
	if !strings.Contains(w.Error, "jj refused to snapshot files of "+d.Workspace) {
		t.Errorf("the failure says %q, want it to name the workspace whose files jj refused", w.Error)
	}
	named := 0
	for _, name := range refused {
		if strings.Contains(w.Error, name) {
			named++
		}
	}
	if named != 10 {
		t.Errorf("the failure names %d of the %d refused files, want ten of them: %q", named, len(refused), w.Error)
	}
	if !strings.Contains(w.Error, "and 5 more") {
		t.Errorf("the failure says %q, want it to count the five files it does not name", w.Error)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a workspace whose files were refused pushes nothing", after, before)
	}
}

// An untracked nested git repository is carried by neither read and fails neither. jj does not
// snapshot it at all, so the rescued commit holds no gitlink — whose commit no object of this
// repository would hold — and no file from inside it; what is in it goes with the pod, as it
// would have gone had no check run. The file the role left beside it is rescued.
func TestAnUntrackedNestedGitRepositoryIsRescuedAsNoGitlinkAndFailsNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	nested := filepath.Join(d.Workspace, "nested")
	d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", "nested")
	d.Write(nested, "inner.txt", "work inside the nested repository\n")
	d.Run(nested, "git", "add", "inner.txt")
	d.Run(nested, "git", "commit", "--quiet", "-m", "work inside the nested repository")
	d.Write(nested, "loose.txt", "a file in no commit of the nested repository\n")
	d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch and no failure for the nested repository", w)
	}
	commit := w.Branches[0].Commit
	if got := d.Run(d.Remote, "git", "show", commit+":notes.txt"); got != "an edit in no commit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the file the role left beside the nested repository", got)
	}
	for line := range strings.Lines(d.Run(d.Remote, "git", "ls-tree", "-r", "--full-tree", commit)) {
		if strings.Contains(line, "nested") {
			t.Errorf("the rescued commit holds %q, want nothing of the untracked nested repository", strings.TrimSpace(line))
		}
	}
}

// A workspace whose jj sparse patterns hold part of its tree fails, with nothing snapshotted and
// nothing pushed. jj's snapshot would read only what those patterns hold, where the git read it
// replaces kept every file of the worktree, so the workspace that would silently lose a file
// fails instead and the pod is not replaced for it.
func TestASparseWorkspaceHoldingPartOfItsTreeFails(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "other.txt", "a file the narrowed patterns leave out\n")
	d.JJ("commit", "-m", "another file of the tree")
	pushed := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.Run(d.Clone, "git", "push", "--quiet", "origin", pushed+":refs/heads/main")
	d.JJ("sparse", "set", "--clear", "--add", "base.txt")
	if _, err := os.Stat(filepath.Join(d.Workspace, "other.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workspace still holds other.txt (%v), want the narrowed patterns to have taken it out of the tree", err)
	}
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the narrowed workspace failed with nothing pushed", w)
	}
	if !strings.Contains(w.Error, "holds part of its tree") || !strings.Contains(w.Error, d.Workspace) {
		t.Errorf("the failure says %q, want it to say that the workspace holds part of its tree", w.Error)
	}
	if !strings.Contains(w.Error, "its sparse patterns are base.txt") {
		t.Errorf("the failure says %q, want it to name the patterns the workspace holds", w.Error)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a narrowed workspace has nothing snapshotted", after, before)
	}
}

// A conflicted working copy is no failure: jj writes a conflict into the worktree itself and
// carries every side of it in the commit's git tree, as .jjconflict-side-0/, .jjconflict-side-1/,
// JJ-CONFLICT-README and the one-sided file, so the snapshot loses no content. Failing there
// would hold a pod's replacement back for work the sides already hold.
//
// Measured on this build: the tree also holds .jjconflict-base-0/, the base of the conflict. The
// work the check pushes is the conflicted working-copy commit itself: the commit the snapshot
// would have made for the files holds that same tree, so the check pushes one branch and not
// two.
func TestAConflictedWorkingCopyIsRescuedInJJsOwnShape(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "conflict.txt", "side A\n")
	d.JJ("describe", "-m", "side A")
	sideA := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", "@-", "-m", "side B")
	d.Write(d.Workspace, "conflict.txt", "side B\n")
	sideB := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", sideA, sideB, "-m", "a merge the role left conflicted")
	if status := d.JJ("status"); !strings.Contains(status, "conflict") {
		t.Fatalf("jj reports %q, want a conflicted working copy", status)
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want the conflicted working copy rescued as one branch", w)
	}
	commit := w.Branches[0].Commit
	if wc := workingCopy(t, d); commit != wc {
		t.Errorf("the check pushed %s, want the conflicted working-copy commit %s", commit, wc)
	}
	for file, want := range map[string]string{
		".jjconflict-side-0/conflict.txt": "side A",
		".jjconflict-side-1/conflict.txt": "side B",
		".jjconflict-base-0/base.txt":     "base",
		"conflict.txt":                    "side A",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
	if readme := d.Run(d.Remote, "git", "show", commit+":JJ-CONFLICT-README"); !strings.Contains(readme, ".jjconflict-") {
		t.Errorf("the rescued commit's JJ-CONFLICT-README says %q, want jj's own account of the shape it wrote", readme)
	}
}

// A workspace jj has moved past fails, with nothing pushed, and the failure says what a person
// runs to clear it — `jj workspace update-stale` — since nothing else tells them. The snapshot
// is tried once and no more: it leaves the working copy as the last jj command of a role wrote
// it, and no retry of the check's makes a stale working copy any newer.
//
// The workspace is left behind jj's operation by a restore made from the clone's own workspace
// (rescuetest.Disk.StaleWorkingCopy, which holds what such a restore must undo for the
// workspace to be stale at all).
func TestAWorkspaceWhoseWorkingCopyIsStaleFailsAndPushesNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	c := recorded(t)
	d.StaleWorkingCopy()
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the stale workspace failed with nothing pushed", w)
	}
	if !strings.Contains(w.Error, "working copy is stale") || !strings.Contains(w.Error, d.Workspace) {
		t.Errorf("the failure says %q, want it to say that %s holds a stale working copy", w.Error, d.Workspace)
	}
	if !strings.Contains(w.Error, "jj workspace update-stale") {
		t.Errorf("the failure says %q, want it to name the command a person clears this with", w.Error)
	}
	failedAt(t, w, rescuetest.FileStep)
	theSnapshotOf(t, c, d.Workspace)
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a stale workspace has nothing snapshotted", after, before)
	}
}

// A snapshot that cannot take jj's own working-copy lock is a read the bound cuts, not a refusal
// anyone reads: measured on this build with the lock held by another process, `jj util snapshot`
// printed nothing for six seconds and was killed. So the entry is the timeout-shaped one — the
// files of this workspace were not read in the time the workspace's bound left for them — and
// the snapshot is tried once, since no retry of the check's takes a lock another process holds.
//
// The lock is this test's own, taken before the check and held for the whole of it with
// LOCK_EX|LOCK_NB, as a role's jj in the same workspace would hold it. Nothing else of the check
// is slowed: every other jj it runs reads at the operation the store is at and takes no
// working-copy lock.
func TestASnapshotThatCannotTakeJJsWorkingCopyLockIsCutByTheBoundAndTriedOnce(t *testing.T) {
	d := rescuetest.New(t, issue)
	c := recorded(t)
	d.Write(d.Workspace, "edit.txt", "a file in no commit, read by a snapshot that cannot take the lock\n")
	lock, err := os.OpenFile(filepath.Join(d.Workspace, ".jj", "working_copy", "working_copy.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("jj's working-copy lock could not be taken, which is what stops this snapshot: %v", err)
	}
	before := d.RemoteBranches()

	bound := rescue.CheckRun + rescue.GuardReserve + 10*time.Second
	started := time.Now()
	checked := rescue.Run(context.Background(), rescue.Request{
		StateDir: d.StateDir, Branch: prefix, Env: d.Env,
		WorkspaceTimeout: bound, Deadline: time.Now().Add(bound + time.Minute),
	})
	took := time.Since(started)
	w := only(t, d, checked)
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the workspace failed with nothing to push", w)
	}
	if !strings.Contains(w.Error, "could not be read in the time the workspace's bound left for them") || !strings.Contains(w.Error, d.Workspace) {
		t.Errorf("the entry failed with %q, want it to name the read of %s as the step that ran out", w.Error, d.Workspace)
	}
	if ran := failedAt(t, w, rescuetest.FileStep); ran <= 0 || ran > took {
		t.Errorf("the entry says the read ran %s, want a time inside the %s the whole check took: %q", ran, took, w.Error)
	}
	theSnapshotOf(t, c, d.Workspace)
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
	// The lock is held for the whole check, so a check that did not stop its read would wait on
	// it with nothing to end the wait: the read's own bound here is 10 s, and what is left of
	// the time is the push's reserve.
	if took > 30*time.Second {
		t.Errorf("the check took %s of its %s bound, want the read stopped with the push's reserve in hand", took, bound)
	}
}
