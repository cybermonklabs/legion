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
// the repository's, the workspace's and the user's configuration files (rescue.snapshot). Eight
// hold this build's own default, among them the two that say what a file's own bytes and mode
// are recorded as (working-copy.eol-conversion and working-copy.exec-bit-change, which
// TestTheSnapshotsPinsOutrankTheWorkspacesOwnBytesAndModeConfiguration reads as the recorded
// file rather than as text); the ninth is the git jj itself starts.
//
// The size limit is not among them, and theSnapshotOf reads that it is on the command line
// under no value at all: the snapshot runs under the workspace's own
// snapshot.max-new-file-size, since a pin above it would have jj track the large file in the
// role's own working-copy commit, where it would stay. What jj refuses for that limit is
// carried into the tree the check pushes by a git of its own (rescue.carried).
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
		`working-copy.eol-conversion="none"`,
		`working-copy.exec-bit-change="auto"`,
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
	for i, arg := range snapshot.args {
		if arg == "--config" && i+1 < len(snapshot.args) && strings.HasPrefix(snapshot.args[i+1], "snapshot.max-new-file-size") {
			t.Errorf("the check's snapshot of %s pins %s, want the workspace's own size limit left standing: %v", dir, snapshot.args[i+1], snapshot.args)
		}
	}
	for _, suppressed := range []string{"--ignore-working-copy", "--at-op=@"} {
		if slices.Contains(snapshot.args, suppressed) {
			t.Errorf("the check's snapshot of %s carries %s, which suppresses the snapshot: %v", dir, suppressed, snapshot.args)
		}
	}
	return snapshot
}

// pinsNameKeysThisJJKnows asserts that every key the snapshot pins is a key this jj has. The
// build takes an unknown key on a command line with no error at all, so a pin's name proves
// nothing by itself: a key misspelt, or one a later jj renames, would be written and would
// decide nothing, and the snapshot would run under whatever the workspace's own configuration
// said. `jj config list --include-defaults` is jj's own answer to what its keys are, and the
// read is made with --ignore-working-copy --at-op=@ so that it snapshots nothing itself.
func pinsNameKeysThisJJKnows(t *testing.T, d *rescuetest.Disk, snapshot ran) {
	t.Helper()
	known := d.Run(d.Workspace, "jj", "--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never",
		"-R", d.Workspace, "config", "list", "--include-defaults")
	for i, arg := range snapshot.args {
		if arg != "--config" || i+1 >= len(snapshot.args) {
			continue
		}
		key, _, _ := strings.Cut(snapshot.args[i+1], "=")
		if !strings.Contains(known, "\n"+key+" = ") && !strings.HasPrefix(known, key+" = ") {
			t.Errorf("the snapshot pins %q, which `jj config list --include-defaults` does not name as a key of this jj: %s", key, known)
		}
	}
}

// gitSubcommand is the subcommand of one git the check ran, with the arguments after it: every
// git it starts opens with its global options, a `-c <key>=<value>` pair among them, and the
// first argument that is none of those is the subcommand.
func gitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i], args[i+1:]
		}
	}
	return "", nil
}

// readsTheWorktreeThroughJJAndNamedPaths asserts the check read dir's files through jj's
// snapshot, with no whole-tree read of git's but the one git's own rule for new files is: no
// `add --all`, and no add — a `git add`, or a `git update-index` carrying `--add` — without `--`
// and at least one path after it, none of those paths `.`, so every add names the files it adds
// (rescue.carried); no `update-index --refresh`, `--really-refresh` or `--again`, which read
// every tracked file, while `update-index -z --index-info`, which reads its records from its
// standard input, is the seed's and allowed; no `status`; and no `ls-files` listing the files the
// index does not track but the one `ls-files -z --others --exclude-standard -- .` the carry reads
// them by. Every other jj it ran there carries --ignore-working-copy, so it reads jj's view and
// snapshots nothing.
func readsTheWorktreeThroughJJAndNamedPaths(t *testing.T, c *commands, dir string) {
	t.Helper()
	snapshot := theSnapshotOf(t, c, dir)
	others := []string{"-z", "--others", "--exclude-standard", "--", "."}
	// namesPaths fails the test unless an add's arguments carry `--` and at least one path after
	// it, none of them `.`; it answers the arguments before `--`.
	namesPaths := func(what string, args, rest []string) []string {
		t.Helper()
		flags, paths, named := rest, []string(nil), false
		if at := slices.Index(rest, "--"); at >= 0 {
			flags, paths, named = rest[:at], rest[at+1:], true
		}
		if !named || len(paths) == 0 {
			t.Errorf("the check ran a `%s` over %s that names no path after `--`: %v", what, dir, args)
		}
		if slices.Contains(paths, ".") {
			t.Errorf("the check ran a `%s` over %s naming `.`, the whole tree, where it names the files it adds: %v", what, dir, args)
		}
		return flags
	}
	for _, r := range c.in(dir) {
		if r.name == "jj" && !slices.Equal(r.args, snapshot.args) && !slices.Contains(r.args, "--ignore-working-copy") {
			t.Errorf("the check ran jj over %s without --ignore-working-copy, which is a second snapshot of the working copy: %v", dir, r.args)
		}
		if r.name != "git" {
			continue
		}
		sub, rest := gitSubcommand(r.args)
		switch sub {
		case "add":
			flags := namesPaths("git add", r.args, rest)
			for _, all := range []string{"--all", "-A", "--no-ignore-removal"} {
				if slices.Contains(flags, all) {
					t.Errorf("the check ran `git add %s` over %s, which is the whole-tree read jj's snapshot replaces: %v", all, dir, r.args)
				}
			}
		case "update-index":
			flags := rest
			if at := slices.Index(rest, "--"); at >= 0 {
				flags = rest[:at]
			}
			if slices.Contains(flags, "--add") {
				namesPaths("git update-index --add", r.args, rest)
			}
			for _, whole := range []string{"--refresh", "--really-refresh", "--again", "-g"} {
				if slices.Contains(flags, whole) {
					t.Errorf("the check ran `git update-index %s` over %s, which reads every tracked file: %v", whole, dir, r.args)
				}
			}
		case "status":
			t.Errorf("the check ran `git status` over %s, a whole-tree read of git's: %v", dir, r.args)
		case "ls-files":
			if (slices.Contains(rest, "--others") || slices.Contains(rest, "-o")) && !slices.Equal(rest, others) {
				t.Errorf("the check ran `git ls-files %s` over %s, want the one read of the files the index does not track to be `ls-files %s`", strings.Join(rest, " "), dir, strings.Join(others, " "))
			}
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
// and through no whole-tree read of git's (readsTheWorktreeThroughJJAndNamedPaths); the
// workspace's files and its git HEAD are what the role left; and the only refs it
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
	readsTheWorktreeThroughJJAndNamedPaths(t, started, d.Workspace)
	pinsNameKeysThisJJKnows(t, d, theSnapshotOf(t, started, d.Workspace))
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

// pushedByTheRole commits what a role's own jj has snapshotted of the workspace and pushes that
// commit to the remote, which answers it. A case that reads what a role's jj recorded has to
// run one, and the working copy it leaves behind holds that work and is unpushed: the check
// would then rescue it and the files beside it as two branches, where the case is about the one
// commit the check makes for the files. Committing it and pushing it leaves the working copy
// empty again, so that one commit is the whole of what the check has to push.
func pushedByTheRole(t *testing.T, d *rescuetest.Disk, message string) string {
	t.Helper()
	d.JJ("commit", "-m", message)
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.Run(d.Clone, "git", "push", "--quiet", "origin", commit+":refs/heads/main")
	return commit
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

// A new file above the size limit the workspace's own configuration sets is rescued. The
// snapshot runs under that limit — a pin above it would have jj track the file in the role's
// own working-copy commit, where it would stay — so jj refuses the file, and the check carries
// it into the tree it pushes through a scratch index of its own (rescue.carried). This
// workspace holds jj's default of 1 MiB in its repository configuration and a 2 MiB file, which
// a role's own jj leaves out for it.
//
// What the carry must not do is track that file for the role: the working copy jj holds the
// workspace at is read before the check and after it, and tracks the file no more after than
// before, which `jj st` says in its own words as well.
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
	wasAt := workingCopy(t, d)
	tracked := d.Git("ls-tree", "-r", "--name-only", wasAt)
	if strings.Contains(tracked, "big.bin") {
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
	if now, was := d.Git("ls-tree", "-r", "--name-only", workingCopy(t, d)), tracked; now != was {
		t.Errorf("the role's working copy (%s, was %s) tracks %q after the check, want the %q it tracked before: the carry is made in a scratch index and tracks nothing of the role's",
			workingCopy(t, d), wasAt, now, was)
	}
	if status := d.JJ("status"); !strings.Contains(status, "? big.bin") {
		t.Errorf("`jj st` says %q after the check, want big.bin still among its untracked paths", status)
	}
}

// The two pins that say what a file's own bytes and mode are recorded as outrank the
// repository's own configuration of them, which is what a pin read as text does not show. This
// workspace configures the other value of each — working-copy.eol-conversion = "input", which
// records a file of CRLF lines with LF, and working-copy.exec-bit-change = "ignore", which
// records the mode the commit under it carries rather than the mode the file has — and holds
// two pairs of files.
//
// A role's own jj snapshots the first pair and loses both: measured on this build, the commit
// it wrote holds `x\n` for the CRLF file and mode 100644 for the executable. That commit is
// pushed, so the one commit left for the check to make is the one for the files beside it. The
// second pair is written after that snapshot and is new to the check's own, which records
// `x\r\n` and mode 100755 under the same repository configuration — and nothing re-records the
// first pair, since jj answers `No snapshot needed.` for a file nothing has touched since.
func TestTheSnapshotsPinsOutrankTheWorkspacesOwnBytesAndModeConfiguration(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "working-copy.eol-conversion", `"input"`)
	d.JJ("config", "set", "--repo", "working-copy.exec-bit-change", `"ignore"`)
	executable := func(name string) {
		t.Helper()
		if err := os.Chmod(filepath.Join(d.Workspace, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.Write(d.Workspace, "role-crlf.txt", "x\r\n")
	d.Write(d.Workspace, "role.sh", "#!/bin/sh\necho hi\n")
	executable("role.sh")
	role := pushedByTheRole(t, d, "the pair a role's own jj recorded")
	if size := d.Git("cat-file", "-s", role+":role-crlf.txt"); size != "2" {
		t.Fatalf("a role's own jj recorded role-crlf.txt as %s bytes, want the 2 of `x\\n`: this case stands on the repository's configuration losing the file's own bytes", size)
	}
	if mode := d.Git("ls-tree", role, "--", "role.sh"); !strings.HasPrefix(mode, "100644 ") {
		t.Fatalf("a role's own jj recorded role.sh as %q, want mode 100644: this case stands on the repository's configuration losing the file's own mode", mode)
	}
	d.Write(d.Workspace, "crlf.txt", "x\r\n")
	d.Write(d.Workspace, "run.sh", "#!/bin/sh\necho hi\n")
	executable("run.sh")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the files as the worktree holds them", w)
	}
	commit := w.Branches[0].Commit
	if size := d.Run(d.Remote, "git", "cat-file", "-s", commit+":crlf.txt"); size != "3" {
		t.Errorf("the rescued commit's crlf.txt is %s bytes, want the 3 of `x\\r\\n` the worktree holds", size)
	}
	if got, want := d.Run(d.Remote, "git", "rev-parse", commit+":crlf.txt"), d.Git("hash-object", "--no-filters", "--", "crlf.txt"); got != want {
		t.Errorf("the rescued commit's crlf.txt is the blob %s, want %s, the worktree's own bytes", got, want)
	}
	if mode := d.Run(d.Remote, "git", "ls-tree", commit, "--", "run.sh"); !strings.HasPrefix(mode, "100755 ") {
		t.Errorf("the rescued commit holds %q, want run.sh at mode 100755, as the worktree holds it", mode)
	}
}

// rescuedWithEveryFileCarried is what a workspace holding files jj's snapshot leaves out is read
// by: it writes each of files new in the worktree, with its body, and a role's own jj then
// snapshots the workspace and leaves every one of them out, so what the rescued commit holds of
// them is the carry's doing and not jj's. Then the check rescues the workspace as one branch;
// that commit holds each file, by its whole name, with the worktree's own bytes; the check read
// the worktree through jj's snapshot and git reads that name the paths they add
// (readsTheWorktreeThroughJJAndNamedPaths); and the working copy jj holds the workspace at tracks
// no more after the check than before it: the carry is made in a scratch index of its own and
// tracks nothing of the role's. It answers the rescued commit.
func rescuedWithEveryFileCarried(t *testing.T, d *rescuetest.Disk, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		d.Write(d.Workspace, name, body)
	}
	d.JJ("status")
	wasAt := workingCopy(t, d)
	tracked := d.Git("ls-tree", "-r", "-z", "--name-only", wasAt)
	for name := range files {
		if slices.Contains(strings.Split(tracked, "\x00"), name) {
			t.Fatalf("the working copy a role's own jj wrote holds %q, want it left out of jj's snapshot", name)
		}
	}
	started := recorded(t)

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding every file jj's snapshot left out", w)
	}
	commit := w.Branches[0].Commit
	for name, body := range files {
		if got, err := os.ReadFile(filepath.Join(d.Workspace, name)); err != nil || string(got) != body {
			t.Fatalf("the workspace's %q holds %d bytes (%v) after the check, want the %d the role wrote", name, len(got), err, len(body))
		}
		if got, want := d.Run(d.Remote, "git", "rev-parse", commit+":"+name), d.Git("hash-object", "--no-filters", "--", name); got != want {
			t.Errorf("the rescued commit's %q is the blob %s, want %s, the %d bytes the workspace holds under that whole name", name, got, want, len(body))
		}
	}
	readsTheWorktreeThroughJJAndNamedPaths(t, started, d.Workspace)
	if now := d.Git("ls-tree", "-r", "-z", "--name-only", workingCopy(t, d)); now != tracked {
		t.Errorf("the role's working copy (%s, was %s) tracks %q after the check, want the %q it tracked before", workingCopy(t, d), wasAt, now, tracked)
	}
	return commit
}

// Fifteen files jj refused whose own names hold `: ` are carried, every one of them, and the
// workspace is rescued. Measured on this build, a file named `huge-01: notes.bin` of 4800 bytes
// under a 1 KiB limit is refused as the one line
// `  huge-01: notes.bin: 4.7KiB (4800 bytes); the maximum size allowed is 1.0KiB (1024 bytes)`,
// which a read of that line that cut the path at its first `: ` took for the path `huge-01`. The
// carry reads no line jj writes: it reads the names git's own `ls-files -z --others` answers,
// NUL-separated, each whole (rescue.carried).
func TestTheRefusedPathsWhoseNamesHoldAColonAreCarriedAndTheWorkspaceIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1KiB"`)
	refused := map[string]string{}
	for i := 1; i <= 15; i++ {
		name := fmt.Sprintf("huge-%02d: notes.bin", i)
		refused[name] = strings.Repeat(strconv.Itoa(i%10), 4800)
	}
	rescuedWithEveryFileCarried(t, d, refused)
}

// Fifteen files jj refused whose own names hold a newline are carried, every one of them, and
// the workspace is rescued. jj writes a refused path raw, so such a name is written over two
// lines, the first with none of the shape a refusal has: measured on this build, a file named
// `refused-01\n  spill.bin` of 4800 bytes under a 1 KiB limit is refused as the two lines
// `  refused-01` and
// `  spill.bin: 4.7KiB (4800 bytes); the maximum size allowed is 1.0KiB (1024 bytes)`. The
// check reads none of what jj prints: git's own NUL-separated read of the files the index does
// not track lists each such file by its whole name (rescue.carried).
func TestRefusedFilesWhoseNamesJJWritesOverTwoLinesAreCarriedAndTheWorkspaceIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1KiB"`)
	refused := map[string]string{}
	for i := 1; i <= 15; i++ {
		name := fmt.Sprintf("refused-%02d\n  spill.bin", i)
		refused[name] = strings.Repeat(strconv.Itoa(i%10), 4800)
	}
	rescuedWithEveryFileCarried(t, d, refused)
}

// A file jj refused whose name jj writes as two refused paths the worktree does not hold is
// carried by its own name, and the workspace is rescued. Measured on this build, a file named
// `big: 1B (1 bytes); the maximum size allowed is 1B\n  z` of 4800 bytes under a 1 KiB limit is
// refused as `  big: 1B (1 bytes); the maximum size allowed is 1B` and
// `  z: 4.7KiB (4800 bytes); the maximum size allowed is 1.0KiB (1024 bytes)`, which read as the
// paths `big` and `z`. The check reads neither line: git lists the file by its whole name
// (rescue.carried).
func TestARefusedFileWhoseNameJJWritesAsTwoRefusedPathsIsCarriedAndTheWorkspaceIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1KiB"`)
	const name = "big: 1B (1 bytes); the maximum size allowed is 1B\n  z"
	commit := rescuedWithEveryFileCarried(t, d, map[string]string{name: strings.Repeat("n", 4800)})
	for _, path := range []string{"big", "z"} {
		if held := d.Run(d.Remote, "git", "ls-tree", commit, "--", path); held != "" {
			t.Errorf("the rescued commit holds %q, want no %s: the worktree holds no such path", held, path)
		}
	}
}

// A file the carry cannot add fails the workspace, and nothing of it is pushed: the file is in no
// commit this check can push, and a workspace reported clean would have its pod replaced and the
// file lost with it. The file here is one jj refused for the workspace's own size limit and that
// no one may read (mode 000): jj refuses it from its size alone, git lists it among the files the
// index does not track, and the add that names it exits 128. Measured on this build,
// `update-index --add --replace -- refused.bin` over it wrote `error: open("refused.bin"):
// Permission denied` and `fatal: Unable to process path refused.bin`, and `git add --
// refused.bin` wrote the same open error, as the whole-tree `git add --all` the route replaces
// did over the same file.
func TestAFileTheCarryCannotAddFailsTheWorkspaceAndPushesNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1KiB"`)
	d.Write(d.Workspace, "refused.bin", strings.Repeat("u", 4800))
	unreadable := filepath.Join(d.Workspace, "refused.bin")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o644) })
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("this process reads a file of mode 000, as root does, so no file it writes is one git cannot read")
	}
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the workspace failed with nothing pushed", w)
	}
	failedAt(t, w, rescuetest.FileStep)
	for _, want := range []string{`"update-index", "--add", "--replace", "--", "refused.bin"`, `open("refused.bin"): Permission denied`} {
		if !strings.Contains(w.Error, want) {
			t.Errorf("the failure says %q, want it to name the add that could not carry the file, %s", w.Error, want)
		}
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a workspace whose file could not be carried pushes nothing", after, before)
	}
}

// The files jj refused are carried, every one of them, and the workspace is rescued, by adds that
// name each file (readsTheWorktreeThroughJJAndNamedPaths). A refusal stops no other file:
// measured on this build, a second new file was snapshotted all the same, so the rescued commit
// holds what jj took and what the carry added alike.
func TestTheFilesTheSnapshotRefusedAreCarriedAndTheWorkspaceIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1KiB"`)
	refused := map[string]string{}
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("refused-%02d.bin", i)
		refused[name] = strings.Repeat(strconv.Itoa(i), 4096)
		d.Write(d.Workspace, name, refused[name])
	}
	d.Write(d.Workspace, "notes.txt", "a file small enough for the workspace's own limit\n")
	started := recorded(t)

	w := only(t, d, d.Check(context.Background(), prefix))
	readsTheWorktreeThroughJJAndNamedPaths(t, started, d.Workspace)
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the files jj refused", w)
	}
	commit := w.Branches[0].Commit
	for name, body := range refused {
		if got := d.Run(d.Remote, "git", "rev-parse", commit+":"+name); got != d.Git("hash-object", "--no-filters", "--", name) {
			t.Errorf("the rescued commit's %s is the blob %s, want the %d bytes the workspace holds", name, got, len(body))
		}
	}
	if got := d.Run(d.Remote, "git", "show", commit+":notes.txt"); got != "a file small enough for the workspace's own limit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the file jj's own snapshot took beside the refused ones", got)
	}
}

// A file jj refused whose own name is git's pathspec magic is carried under that name, and the
// tracked file the magic stands for is left as it is. Every path this check hands git is a name
// a role wrote, read out of git's own NUL-separated answers, and never a pattern, so every git
// the package starts carries --literal-pathspecs (rescue.gitPins).
//
// Measured on this build, over a worktree holding a refused 2 MiB file named `:aa` beside a
// tracked `aa`: `git add -- ':aa'` without that option exited 0 with the index holding `aa`
// alone — a carry that adds nothing, reads back nothing as missing, and leaves the file in no
// commit of a workspace nothing failed — and the same add with the option put `:aa` in the
// index.
func TestARefusedPathWhoseNameIsGitsPathspecMagicIsCarriedIntoTheRescuedCommit(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", `"1MiB"`)
	d.Write(d.Workspace, "aa", "the tracked file the refused name is magic for\n")
	role := pushedByTheRole(t, d, "the tracked file beside the refused one")
	const big = 2 * 1024 * 1024
	d.Write(d.Workspace, ":aa", strings.Repeat("m", big))
	// A role's own jj refuses it for the workspace's own limit, so what the rescued commit
	// holds under that name is the carry's doing and not jj's.
	d.JJ("status")
	if tracked := d.Git("ls-tree", "-r", "--name-only", workingCopy(t, d)); strings.Contains(tracked, ":aa") {
		t.Fatalf("the working copy a role's own jj wrote holds :aa (%q), want it refused for the workspace's own limit", tracked)
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the file whose name is pathspec magic", w)
	}
	commit := w.Branches[0].Commit
	// `<commit>::aa` is that commit's path `:aa`: git cuts a revision's path at the first
	// colon and reads the rest as the name.
	if size := d.Run(d.Remote, "git", "cat-file", "-s", commit+"::aa"); size != strconv.Itoa(big) {
		t.Errorf("the rescued commit's :aa is %s bytes, want the %d the workspace holds", size, big)
	}
	if got, want := d.Run(d.Remote, "git", "rev-parse", commit+"::aa"), d.Git("hash-object", "--no-filters", "--", ":aa"); got != want {
		t.Errorf("the rescued commit's :aa is the blob %s, want %s, the worktree's own bytes", got, want)
	}
	if got, want := d.Run(d.Remote, "git", "rev-parse", commit+":aa"), d.Run(d.Remote, "git", "rev-parse", role+":aa"); got != want {
		t.Errorf("the rescued commit's aa is the blob %s, want the %s the role pushed: the carry adds the name it is handed and nothing that name could stand for", got, want)
	}
}

// A file whose name is not valid UTF-8 is skipped by jj's snapshot, with a warning and an exit
// status of 0, and is carried into the tree the check pushes by its name's own bytes, which git
// lists among the files the index does not track. The workspace does not fail for it: the file
// is in the commit the check pushes, read back out of it by that same name.
//
// Measured on this build, jj writes the skip as `  .: "bad\xFD.txt"` — the directory, then the
// name as Rust debug-quotes it. The check reads nothing jj prints (rescue.snapshot).
func TestAPathWhoseNameIsNotValidUTF8IsCarriedIntoTheRescuedCommit(t *testing.T) {
	const skipped = "bad\xfd.txt"
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, skipped, "work under a name that is not valid UTF-8\n")
	// A role's own jj skips it, so the working copy it wrote names no such path: what the
	// rescued commit holds is the carry's doing and not jj's.
	d.JJ("status")
	if tracked := d.Git("ls-tree", "-r", "--name-only", "-z", workingCopy(t, d)); strings.Contains(tracked, skipped) {
		t.Fatalf("the working copy a role's own jj wrote names %q (%q), want it skipped for its name", skipped, tracked)
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the file jj skipped", w)
	}
	commit := w.Branches[0].Commit
	if got := d.Run(d.Remote, "git", "show", commit+":"+skipped); got != "work under a name that is not valid UTF-8" {
		t.Errorf("the rescued commit's %q holds %q, want the work the role left under that name", skipped, got)
	}
}

// A directory whose own name holds `: "` is carried whole, the file in it whose name is not valid
// UTF-8 among it. Measured on this build, a file `w\xFE` in a directory `q: "z` is skipped as the
// line `  q: "z: "w\xFE"`, which a read of jj's line would have to cut in the right place; the
// carry reads git's NUL-separated name of the file instead (rescue.carried).
func TestADirectoryWhoseNameHoldsAColonAndAQuoteIsCarriedWholeIntoTheRescuedCommit(t *testing.T) {
	const dir, skipped = `q: "z`, "w\xfe"
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, dir+"/"+skipped, "work under a name that is not valid UTF-8\n")
	d.Write(d.Workspace, dir+"/plain.txt", "a file of the same directory jj's own snapshot takes\n")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the directory whole", w)
	}
	commit := w.Branches[0].Commit
	for file, want := range map[string]string{
		dir + "/" + skipped: "work under a name that is not valid UTF-8",
		dir + "/plain.txt":  "a file of the same directory jj's own snapshot takes",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %q holds %q, want %q", file, got, want)
		}
	}
}

// A directory holding a nested .jj directory is left out of jj's snapshot whole, with no warning
// of any kind, and is carried into the tree the check pushes as it stands. The workspace does
// not fail for it, and what the directory holds — the nested .jj's own files as the plain files
// they are to git, and the file beside them — is in the commit the check pushes.
//
// What jj leaves out is read here first, so the case stands on the carry and not on jj:
// measured on this build, a worktree holding d/.jj/f and d/plain.txt snapshotted to a tree with
// neither of them in it, while top.txt was taken. That commit of a role's own is pushed, so the
// one commit left for the check to make is the one that carries the directory.
func TestADirectoryHoldingANestedJJIsCarriedIntoTheRescuedCommit(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "d/plain.txt", "a file of the directory jj leaves out\n")
	d.Write(d.Workspace, "top.txt", "a file jj's own snapshot takes\n")
	role := pushedByTheRole(t, d, "the file a role's own jj took")
	tracked := d.Git("ls-tree", "-r", "--name-only", role)
	if !strings.Contains(tracked, "top.txt") {
		t.Fatalf("the commit a role's own jj wrote holds %q, want top.txt among its files", tracked)
	}
	for _, left := range []string{"d/plain.txt", "d/.jj/f"} {
		if strings.Contains(tracked, left) {
			t.Fatalf("the commit a role's own jj wrote holds %s (%q), want the directory holding a nested .jj left out whole", left, tracked)
		}
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the directory jj left out", w)
	}
	commit := w.Branches[0].Commit
	for file, want := range map[string]string{
		"d/.jj/f":     "the nested jj's own file",
		"d/plain.txt": "a file of the directory jj leaves out",
		"top.txt":     "a file jj's own snapshot takes",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
}

// A directory whose nested .jj is a regular file, and one whose nested .jj is a symlink, are
// left out of jj's snapshot whole, as a directory holding a nested .jj directory is, and both
// are carried into the tree the check pushes. jj says nothing of either, and the carry needs it
// to say nothing: git lists every file of both directories among those the index does not
// track, whatever kind of file the .jj entry is (rescue.carried).
//
// What jj leaves out is read here first, so the case stands on the carry and not on jj:
// measured on this build, with e/.jj a regular file and h/.jj a symlink, each beside a
// keep.txt, the snapshot's tree held neither keep.txt and did hold the file at the worktree's
// root. That commit of a role's own is pushed, so the one commit left for the check to make is
// the one that carries the two directories.
func TestADirectoryWhoseNestedJJIsAFileOrASymlinkIsCarriedIntoTheRescuedCommit(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "e/.jj", "a nested .jj that is a regular file\n")
	d.Write(d.Workspace, "e/keep.txt", "the file beside the .jj that is a regular file\n")
	d.Write(d.Workspace, "h/keep.txt", "the file beside the .jj that is a symlink\n")
	if err := os.Symlink("keep.txt", filepath.Join(d.Workspace, "h", ".jj")); err != nil {
		t.Fatal(err)
	}
	d.Write(d.Workspace, "top.txt", "a file jj's own snapshot takes\n")
	role := pushedByTheRole(t, d, "the file a role's own jj took")
	tracked := d.Git("ls-tree", "-r", "--name-only", role)
	if !strings.Contains(tracked, "top.txt") {
		t.Fatalf("the commit a role's own jj wrote holds %q, want top.txt among its files", tracked)
	}
	for _, left := range []string{"e/.jj", "e/keep.txt", "h/.jj", "h/keep.txt"} {
		if strings.Contains(tracked, left) {
			t.Fatalf("the commit a role's own jj wrote holds %s (%q), want a directory holding a nested .jj of any kind left out whole", left, tracked)
		}
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the directories jj left out", w)
	}
	commit := w.Branches[0].Commit
	for file, want := range map[string]string{
		"e/.jj":      "a nested .jj that is a regular file",
		"e/keep.txt": "the file beside the .jj that is a regular file",
		"h/keep.txt": "the file beside the .jj that is a symlink",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
	// The fourth path is the symlink itself, carried as the symlink it is: mode 120000, whose
	// blob is the name it points at.
	if mode := d.Run(d.Remote, "git", "ls-tree", commit, "--", "h/.jj"); !strings.HasPrefix(mode, "120000 ") {
		t.Errorf("the rescued commit holds %q, want h/.jj at mode 120000, as the worktree holds it", mode)
	}
	if got := d.Run(d.Remote, "git", "show", commit+":h/.jj"); got != "keep.txt" {
		t.Errorf("the rescued commit's h/.jj names %q, want the keep.txt the worktree's symlink points at", got)
	}
}

// The carry keeps git's own ignore rules. A directory holding a nested .jj is left out of jj's
// snapshot whole and the carry adds what is under it, and what it adds there is what `git add
// --all` would have added: the nested .jj's own files as the plain files they are to git, the
// files beside them, and nothing an ignore rule covers. A commit of this check holding an
// ignored file would push what a role's own push would not (rescue.edits).
//
// Measured on this build, over a worktree holding `*.log` in .gitignore beside d/.jj/f,
// d/keep.txt and d/x.log: `git ls-files --others --exclude-standard` answered d/.jj/f and
// d/keep.txt and no d/x.log, and an add with -f would have put d/x.log in the index as well,
// which is why the carry's add of those files carries none.
func TestAnIgnoredFileBesideANestedJJIsLeftOutOfTheRescuedCommit(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, ".gitignore", "*.log\n")
	d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "d/keep.txt", "a file of the directory jj leaves out\n")
	d.Write(d.Workspace, "d/x.log", "a file .gitignore covers\n")
	role := pushedByTheRole(t, d, "the ignore rules a role's own jj took")
	tracked := d.Git("ls-tree", "-r", "--name-only", role)
	if !strings.Contains(tracked, ".gitignore") || strings.Contains(tracked, "d/") {
		t.Fatalf("the commit a role's own jj wrote holds %q, want the .gitignore and nothing of the directory holding a nested .jj", tracked)
	}

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch holding the directory jj left out", w)
	}
	commit := w.Branches[0].Commit
	for file, want := range map[string]string{
		"d/.jj/f":    "the nested jj's own file",
		"d/keep.txt": "a file of the directory jj leaves out",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
	if held := d.Run(d.Remote, "git", "ls-tree", "-r", "--name-only", "--full-tree", commit); strings.Contains(held, "d/x.log") {
		t.Errorf("the rescued commit holds %q, want no d/x.log: the carry adds under d what `git add --all` would have added there and no more", held)
	}
}

// A file head tracks that an ignore rule covers is rescued as the role edited it, where its
// directory gains a nested .jj: the reviewer's shape of this round. d/x.log is committed before
// `*.log` is ignored, and then edited under a d that gains a nested .jj. jj's snapshot leaves d
// out whole and records d/x.log as deleted, and `git ls-files --others --exclude-standard`
// names no d/x.log, since an ignore rule covers it; the `git add --all` this read replaces
// updates it all the same, as the tracked file it is. So the carry seeds it from the copy of the
// worktree's index the check made before the snapshot, and adds it by name with -f
// (rescue.seeds, rescue.carried).
//
// The nested .jj is either a directory holding a file, or a real jj repository, made with `jj
// git init d`, whose .jj/.gitignore is `/*`. Measured on this build, that init colocates, so d
// is an embedded git repository as well, with d/.git beside d/.jj, and `jj git init
// --no-colocate d` writes no .jj/.gitignore at all. Over that d, which the index still tracks
// d/x.log under, the `git add --all` this read replaces exits 0 with d/x.log updated at 100644
// and no gitlink for d; and `/*` ignores every file of d/.jj to git, so that read holds none of
// them, and the rescued commit holds none.
func TestATrackedFileAnIgnoreRuleCoversUnderADirectoryThatGainsANestedJJIsRescuedAsEdited(t *testing.T) {
	for _, tc := range []struct {
		name string
		// nest gives d its nested .jj and answers what the rescued commit holds under d/.jj.
		nest func(t *testing.T, d *rescuetest.Disk) map[string]string
	}{
		{"a nested .jj directory holding a file", func(t *testing.T, d *rescuetest.Disk) map[string]string {
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			return map[string]string{"d/.jj/f": "the nested jj's own file"}
		}},
		{"a nested jj repository whose .jj/.gitignore is /*", func(t *testing.T, d *rescuetest.Disk) map[string]string {
			d.Run(d.Workspace, "jj", "git", "init", "d")
			if _, err := os.Lstat(filepath.Join(d.Workspace, "d", ".git")); err != nil {
				t.Fatalf("the nested repository holds no d/.git (%v), want the colocated repository `jj git init` makes", err)
			}
			if rule, err := os.ReadFile(filepath.Join(d.Workspace, "d", ".jj", ".gitignore")); err != nil || string(rule) != "/*\n" {
				t.Fatalf("the nested repository's .jj/.gitignore holds %q (%v), want the `/*` this case stands on", rule, err)
			}
			return map[string]string{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
			pushedByTheRole(t, d, "a log file committed before it was ignored")
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			nested := tc.nest(t, d)
			d.Write(d.Workspace, "d/x.log", "edited under the nested .jj\n")

			started := recorded(t)
			w := only(t, d, d.Check(context.Background(), prefix))
			readsTheWorktreeThroughJJAndNamedPaths(t, started, d.Workspace)
			if w.Status != rescue.Rescued || len(w.Branches) != 1 {
				t.Fatalf("the check reported %+v, want one rescued branch holding the edited d/x.log", w)
			}
			commit := w.Branches[0].Commit
			if got := d.Run(d.Remote, "git", "show", commit+":d/x.log"); got != "edited under the nested .jj" {
				t.Errorf("the rescued commit's d/x.log holds %q, want the edit the role left under the nested .jj", got)
			}
			if entry := d.Run(d.Remote, "git", "ls-tree", commit, "--", "d/x.log"); !strings.HasPrefix(entry, "100644 blob ") {
				t.Errorf("the rescued commit holds d/x.log as %q, want a file at 100644, as the git read records it", entry)
			}
			for file, want := range nested {
				if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
					t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
				}
			}
			for line := range strings.Lines(d.Run(d.Remote, "git", "ls-tree", "-r", "--name-only", "--full-tree", commit)) {
				if file := strings.TrimSpace(line); file == "d" {
					t.Errorf("the rescued commit holds d as an entry of its own, want no gitlink: the git read records none there")
				} else if strings.HasPrefix(file, "d/.jj/") {
					if _, ok := nested[file]; !ok {
						t.Errorf("the rescued commit holds %s, want under d/.jj only %v, what `git add --all` would have added there", file, nested)
					}
				}
			}
		})
	}
}

// An ignored directory holding a .jj of its own reaches no commit of this check, and the
// workspace does not fail for it. jj leaves the directory out of its snapshot for that nested
// .jj, and git's own read of the files the index does not track lists nothing of it, since an
// ignore rule covers every file of it: the carry adds nothing of it and reads back nothing of it
// as missing (rescue.carried). Adding it would push what a role's own push would not, and
// failing for it would hold a pod's replacement back for content `git add --all` leaves out.
func TestAnIgnoredDirectoryHoldingANestedJJIsLeftOutOfTheRescuedCommitAndFailsNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, ".gitignore", "ignored/\n")
	d.Write(d.Workspace, "ignored/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "ignored/keep.txt", "a file of the ignored directory\n")
	d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch and no failure for the ignored directory", w)
	}
	commit := w.Branches[0].Commit
	if got := d.Run(d.Remote, "git", "show", commit+":notes.txt"); got != "an edit in no commit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the file the role left beside the ignored directory", got)
	}
	for line := range strings.Lines(d.Run(d.Remote, "git", "ls-tree", "-r", "--full-tree", commit)) {
		if strings.Contains(line, "ignored/") {
			t.Errorf("the rescued commit holds %q, want nothing of the ignored directory", strings.TrimSpace(line))
		}
	}
}

// An untracked nested git repository with a commit is recorded by both reads as the gitlink it
// is, and fails neither. `git add --all` records it at its HEAD, whose commit no object of this
// repository holds, and carries no file of it; the route records the same gitlink and no file
// from inside it (rescue.carried), so what is in it goes with the pod, as it would have gone had
// no check run. The file the role left beside it is rescued.
func TestAnUntrackedNestedGitRepositoryIsRescuedAsItsGitlinkAndFailsNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	nested := filepath.Join(d.Workspace, "nested")
	d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", "nested")
	d.Write(nested, "inner.txt", "work inside the nested repository\n")
	d.Run(nested, "git", "add", "inner.txt")
	d.Run(nested, "git", "commit", "--quiet", "-m", "work inside the nested repository")
	d.Write(nested, "loose.txt", "a file in no commit of the nested repository\n")
	d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
	gitlink := "160000 commit " + d.Run(nested, "git", "rev-parse", "HEAD") + "\tnested"

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch and no failure for the nested repository", w)
	}
	commit := w.Branches[0].Commit
	if got := d.Run(d.Remote, "git", "show", commit+":notes.txt"); got != "an edit in no commit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the file the role left beside the nested repository", got)
	}
	var held []string
	for line := range strings.Lines(d.Run(d.Remote, "git", "ls-tree", "-r", "--full-tree", commit)) {
		if line = strings.TrimSpace(line); strings.Contains(line, "nested") {
			held = append(held, line)
		}
	}
	if !slices.Equal(held, []string{gitlink}) {
		t.Errorf("the rescued commit holds %q of the nested repository, want only its gitlink %q, as the git read records it", held, gitlink)
	}
}

// A path inside an embedded git repository is carried by neither read, and the workspace fails
// for it no more than for the repository itself. A nested .jj inside such a repository is a
// directory jj's snapshot leaves out, and `git ls-files --others --exclude-standard` lists the
// embedded repository as itself, `b/`, with a trailing slash, and no file under it: the carry
// records that entry as the gitlink at b's HEAD, adds no file of it and reads back only b
// (rescue.carried). That is what the `git add --all` this read stands in for records: a gitlink
// whose commit no object of this repository holds, and no file of it at all.
func TestANestedJJInsideAnEmbeddedGitRepositoryIsRescuedAsItsGitlinkAndFailsNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	embedded := filepath.Join(d.Workspace, "b")
	d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", "b")
	d.Write(embedded, "inner.txt", "work inside the embedded repository\n")
	d.Run(embedded, "git", "add", "inner.txt")
	d.Run(embedded, "git", "commit", "--quiet", "-m", "work inside the embedded repository")
	d.Write(embedded, "sub/.jj/f", "the nested jj's own file, inside the embedded repository\n")
	d.Write(embedded, "sub/keep.txt", "a file beside that nested .jj\n")
	d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
	gitlink := "160000 commit " + d.Run(embedded, "git", "rev-parse", "HEAD") + "\tb"

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch and no failure for the nested .jj inside the embedded repository", w)
	}
	commit := w.Branches[0].Commit
	if got := d.Run(d.Remote, "git", "show", commit+":notes.txt"); got != "an edit in no commit" {
		t.Errorf("the rescued commit's notes.txt holds %q, want the file the role left beside the embedded repository", got)
	}
	var held []string
	for line := range strings.Lines(d.Run(d.Remote, "git", "ls-tree", "-r", "--full-tree", commit)) {
		if name := strings.TrimSpace(line); strings.HasSuffix(name, "\tb") || strings.Contains(name, "\tb/") {
			held = append(held, name)
		}
	}
	if !slices.Equal(held, []string{gitlink}) {
		t.Errorf("the rescued commit holds %q of the embedded repository, want only its gitlink %q, as the git read records it", held, gitlink)
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
	// Each pattern is written with %q: a sparse pattern is a path and may hold the separator the
	// list is written with, so a reader tells the members of that list apart only when each is
	// quoted (rescue.quoted).
	if !strings.Contains(w.Error, `its sparse patterns are "base.txt"`) {
		t.Errorf("the failure says %q, want it to name the patterns the workspace holds, each quoted", w.Error)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a narrowed workspace has nothing snapshotted", after, before)
	}
}

// A workspace whose sparse patterns are more than one and still cover its whole tree is no
// narrowing, and it is rescued. `.` is the whole tree, and `jj sparse set --add <dir>` over a
// workspace checked out whole leaves the two patterns `.` and that directory, which together
// are still every file of the tree (measured on this build). A check that failed such a
// workspace would hold a pod's replacement back for a tree it holds all of.
func TestASparseWorkspaceWhosePatternsCoverItsWholeTreeIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "dir/kept.txt", "a file of the directory the patterns name\n")
	d.JJ("sparse", "set", "--add", "dir")
	patterns := strings.Fields(d.JJ("sparse", "list"))
	if !slices.Equal(patterns, []string{".", "dir"}) {
		t.Fatalf("the workspace's sparse patterns are %v, want the `.` and `dir` this case stands on", patterns)
	}
	// The file that directory holds was snapshotted by the jj that set the patterns, and is
	// pushed, so the one commit left for the check to make is the one for the edit beside it.
	pushedByTheRole(t, d, "the file of the directory the patterns name")
	d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want the workspace rescued: its patterns hold every file of its tree", w)
	}
	commit := w.Branches[0].Commit
	for file, want := range map[string]string{
		"dir/kept.txt": "a file of the directory the patterns name",
		"notes.txt":    "an edit in no commit",
	} {
		if got := d.Run(d.Remote, "git", "show", commit+":"+file); got != want {
			t.Errorf("the rescued commit's %s holds %q, want %q", file, got, want)
		}
	}
}

// A workspace holding no sparse pattern at all fails, with nothing snapshotted and nothing
// pushed, and in words of its own rather than the narrowed workspace's: it holds no file of its
// tree, which is what an unfinished checkout leaves, and the failure names what a person runs
// to put the files back. Measured on this build, `jj sparse set --clear` leaves exactly that
// set, and `jj sparse list` then prints nothing.
func TestASparseWorkspaceHoldingNoPatternAtAllFails(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("sparse", "set", "--clear")
	if patterns := d.JJ("sparse", "list"); patterns != "" {
		t.Fatalf("the workspace's sparse patterns are %q, want the empty set this case stands on", patterns)
	}
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix))
	if w.Status != rescue.Failed || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want the workspace holding no pattern failed with nothing pushed", w)
	}
	failedAt(t, w, rescuetest.FileStep)
	if !strings.Contains(w.Error, "holds no sparse pattern") || !strings.Contains(w.Error, d.Workspace) {
		t.Errorf("the failure says %q, want it to say that the workspace holds no sparse pattern", w.Error)
	}
	if !strings.Contains(w.Error, "jj sparse reset") {
		t.Errorf("the failure says %q, want it to name the command a person puts the workspace's files back with", w.Error)
	}
	if strings.Contains(w.Error, "holds part of its tree") {
		t.Errorf("the failure says %q, want the empty set's own words and not the narrowed workspace's", w.Error)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged: a workspace holding no pattern has nothing snapshotted", after, before)
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
