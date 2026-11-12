package rescue_test

// The differential between the two reads of a worktree's files (rescue.edits): the whole-tree
// `git add --all` the check made of every worktree before this route existed, and the route it
// takes through a worktree jj holds, jj's own snapshot with every path that snapshot left out
// carried in (rescue.snapshotTree). The class this stands guard over is one: a path the git read
// keeps that the jj route loses while the workspace reads rescued. Each review of the route found
// one more shape of it; this test holds every shape found so far as a row, and a new shape is a
// new row, which fails here rather than waiting for a reader to think of it.
//
// For each row, two trees are built over the same worktree, in this order. First the git read's,
// because the jj route's snapshot rewrites the worktree's colocated git index, which the git read
// starts from. The git read is written out here, command by command, rather than called in the
// package (rescue.indexTree), so the comparison does not lean on the code under test. Then the jj
// route's (rescue.TreeTheJJRouteBuilds). Every entry of the git read's tree must be in the jj
// route's with the same path, the same mode and the same blob, but for two exceptions, each named
// by its reason and each checked for exactly the entries it covers (lostAsRuled). The jj route's
// tree may hold more than the git read's; that is no loss.
//
// A row whose route fails says so, and the failure is asserted: a workspace that fails is never a
// silent loss, since its pod is not replaced for it. A row that expects a tree and gets a failure,
// or expects a failure and gets a tree, fails.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
)

// todaysPins is the configuration the git read puts on every git it runs, written here as the
// package writes it (rescue.gitPins) rather than read from it.
var todaysPins = []string{"--no-replace-objects", "--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// gitExit runs git under todaysPins in dir, in the disk's environment with env added, with stdin
// as its input, and answers its standard output as it stands and its exit status. A git that
// cannot be started fails the test.
func gitExit(t *testing.T, d *rescuetest.Disk, dir string, env []string, stdin string, args ...string) (string, int, string) {
	t.Helper()
	cmd := exec.Command("git", append(slices.Clone(todaysPins), args...)...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(d.Env), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("git %q could not be run: %v", args, err)
	}
	return stdout.String(), cmd.ProcessState.ExitCode(), stderr.String()
}

// gitRead is gitExit for a git that must exit 0.
func gitRead(t *testing.T, d *rescuetest.Disk, dir string, env []string, args ...string) string {
	t.Helper()
	out, code, stderr := gitExit(t, d, dir, env, "", args...)
	if code != 0 {
		t.Fatalf("git %q exited %d: %s", args, code, stderr)
	}
	return out
}

// nulSplit is a NUL-separated read of git's, each name as it stands: a name may hold any byte but
// NUL, a newline among them.
func nulSplit(out string) []string {
	names := strings.Split(out, "\x00")
	if last := len(names) - 1; names[last] == "" {
		names = names[:last]
	}
	return names
}

// todaysRead is what the git read makes of a worktree: the tree it builds, and the files of the
// worktree's own index that the second exception stands for (lostAsRuled), read off that index
// before the snapshot rewrites it.
type todaysRead struct {
	tree string
	// forced is every file the worktree's index holds that head's tree does not and an ignore
	// rule covers: what a role puts in the index with `git add -f`.
	forced map[string]bool
}

// readToday builds the git read's tree of the worktree at dir, whose git HEAD is head, as that
// read builds it: a copy of the worktree's own git index in a scratch file, `git add --all` over
// that copy, and the tree the copy then writes.
func readToday(t *testing.T, d *rescuetest.Disk, dir, head string) todaysRead {
	t.Helper()
	own := strings.TrimSpace(gitRead(t, d, dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	body, err := os.ReadFile(own)
	if err != nil {
		t.Fatalf("the worktree's own git index %s could not be read: %v", own, err)
	}
	index := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(index, body, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"GIT_INDEX_FILE=" + index}

	// `ls-files --cached --ignored --exclude-standard` is every file the index holds that an
	// ignore rule covers; those head's tree does not hold are the ones a role forced in.
	tracked := map[string]bool{}
	for _, name := range nulSplit(gitRead(t, d, dir, nil, "ls-tree", "-r", "-z", "--name-only", "--full-tree", head)) {
		tracked[name] = true
	}
	forced := map[string]bool{}
	for _, name := range nulSplit(gitRead(t, d, dir, env, "ls-files", "-z", "--cached", "--ignored", "--exclude-standard")) {
		if !tracked[name] {
			forced[name] = true
		}
	}

	gitRead(t, d, dir, env, "add", "--all", "--", ".")
	return todaysRead{tree: strings.TrimSpace(gitRead(t, d, dir, env, "write-tree")), forced: forced}
}

// entry is one file of a tree: its mode, and the object it is.
type entry struct{ mode, object string }

// entriesOf is every file of tree by its path, read with `git ls-tree -r -z --full-tree`, which
// lists a gitlink as the one entry it is and does not descend into it.
func entriesOf(t *testing.T, d *rescuetest.Disk, tree string) map[string]entry {
	t.Helper()
	entries := map[string]entry{}
	for _, line := range nulSplit(gitRead(t, d, d.Workspace, nil, "ls-tree", "-r", "-z", "--full-tree", tree)) {
		meta, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			t.Fatalf("git ls-tree wrote %q, want `<mode> <type> <object>\\t<path>`", line)
		}
		entries[path] = entry{mode: fields[0], object: fields[2]}
	}
	return entries
}

// gitlinkMode is the mode git records an embedded git repository at.
const gitlinkMode = "160000"

// conflictMarker is the line jj opens a conflict it materializes in a file with, on this build
// `<<<<<<< conflict 1 of 1`.
var conflictMarker = regexp.MustCompile(`(?m)^<<<<<<< [Cc]onflict [0-9]+ of [0-9]+`)

// lostAsRuled compares the two trees of a row and fails the test for every entry of the git
// read's that the jj route's does not hold at the same path, mode and blob, but for the two
// exceptions, each named by its reason:
//
//   - An entry at or under a path the git read's tree holds as a gitlink, which is an embedded git
//     repository: `git add --all` records it as a gitlink and carries no file of it, and jj does
//     not snapshot it at all, so neither read carries what is in it (rescue.edits).
//   - A file an ignore rule covers that the worktree's index held and HEAD does not track, which
//     is a file a role forced into the index with `git add -f`: jj never commits such a file, and
//     the architect has ruled it the one file the snapshot loses (the rescue package's doc).
//
// Each exception covers only an entry the jj route's tree holds no file at all at, and each is
// checked for exactly the entries it covers: the row names them, and an exception covering one
// more or one fewer than the row names fails, as a lost entry does.
//
// Beside the two exceptions, and not one of them, is a conflicted path, carried as its sides
// (carriedAsItsSides): a path jj lists as conflicted in the working-copy commit, where the git
// read keeps the file jj materialized with its conflict markers and the jj route keeps every side
// and the base in jj's own shape. It is no exception because nothing of it is lost: the content is
// all present, in jj's own shape, so an exception would hide a loss where the shape is what must
// be proved. Only the paths jj lists are read that way; a path jj does not list follows the
// ordinary rule, and a file of the git read's tree that holds jj's conflict markers at a path jj
// does not list fails.
func lostAsRuled(t *testing.T, d *rescuetest.Disk, today todaysRead, route string, row differentialRow) {
	t.Helper()
	was, now := entriesOf(t, d, today.tree), entriesOf(t, d, route)
	listed := conflictedPaths(t, d)
	carriedAsItsSides(t, d, now, listed, row.conflicts)
	var repositories []string
	for path, e := range was {
		if e.mode == gitlinkMode {
			repositories = append(repositories, path)
		}
	}
	underAGitlink := func(path string) bool {
		for _, repository := range repositories {
			if path == repository || strings.HasPrefix(path, repository+"/") {
				return true
			}
		}
		return false
	}
	paths := make([]string, 0, len(was))
	for path := range was {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var coveredAsGitlinks, coveredAsForced []string
	for _, path := range paths {
		if slices.Contains(listed, path) {
			continue
		}
		if e := was[path]; e.mode != gitlinkMode && conflictMarker.MatchString(gitRead(t, d, d.Workspace, nil, "cat-file", "blob", e.object)) {
			t.Errorf("the git read keeps %q holding jj's conflict markers, and jj lists no conflict at that path", path)
		}
		kept, held := now[path]
		switch {
		case held && kept == was[path]:
			continue
		case !held && underAGitlink(path):
			coveredAsGitlinks = append(coveredAsGitlinks, path)
		case !held && today.forced[path]:
			coveredAsForced = append(coveredAsForced, path)
		case !held:
			t.Errorf("lost: the git read keeps %q at mode %s blob %s, and the jj route's tree holds no such path", path, was[path].mode, was[path].object)
		default:
			t.Errorf("changed: the git read keeps %q at mode %s blob %s, and the jj route's tree holds it at mode %s blob %s", path, was[path].mode, was[path].object, kept.mode, kept.object)
		}
	}
	if !slices.Equal(coveredAsGitlinks, row.gitlinks) {
		t.Errorf("the embedded-repository exception covers %q, want exactly the %q this row names", coveredAsGitlinks, row.gitlinks)
	}
	if !slices.Equal(coveredAsForced, row.forced) {
		t.Errorf("the forced-into-the-index exception covers %q, want exactly the %q this row names", coveredAsForced, row.forced)
	}
}

// conflictedPaths is every path jj lists as conflicted in the workspace's working-copy commit,
// sorted, read at the operation the store is at and snapshotting nothing, NUL-separated since a
// name may hold a newline. Measured on this build, `jj file list -r @ -T 'if(conflict, path ++
// "\0")'` over a working copy conflicted at conflict.txt and at `sp ace/c d.txt` answered those
// two names and no other, as `jj resolve --list` did.
func conflictedPaths(t *testing.T, d *rescuetest.Disk) []string {
	t.Helper()
	cmd := exec.Command("jj", "--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never", "-R", d.Workspace,
		"file", "list", "-r", "@", "-T", `if(conflict, path ++ "\0")`)
	cmd.Dir = d.Workspace
	cmd.Env = d.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jj could not list the working copy's conflicted paths: %v: %s", err, stderr.String())
	}
	if len(out) == 0 {
		return nil
	}
	paths := nulSplit(string(out))
	slices.Sort(paths)
	return paths
}

// conflict is what the fixture wrote at a path it left conflicted: the body each of the two sides
// holds there, and the body the base holds, "" where the base has no such path.
type conflict struct {
	sides [2]string
	base  string
}

// carriedAsItsSides fails the test unless jj lists exactly the conflicted paths the row names, and
// the jj route's tree, now, holds each of them in jj's own shape: every side at
// .jjconflict-side-<n>/<path> with the blob of the body the fixture wrote there, the base at
// .jjconflict-base-0/<path> with its blob where the base held the path and nothing there where it
// did not, and JJ-CONFLICT-README. That is the content the git read's file with conflict markers
// carried, held whole.
func carriedAsItsSides(t *testing.T, d *rescuetest.Disk, now map[string]entry, listed []string, want map[string]conflict) {
	t.Helper()
	named := make([]string, 0, len(want))
	for path := range want {
		named = append(named, path)
	}
	slices.Sort(named)
	if !slices.Equal(listed, named) {
		t.Errorf("jj lists the conflicted paths %q, want exactly the %q this row names", listed, named)
	}
	blob := func(body string) string {
		out, code, stderr := gitExit(t, d, d.Workspace, nil, body, "hash-object", "--stdin")
		if code != 0 {
			t.Fatalf("git hash-object exited %d: %s", code, stderr)
		}
		return strings.TrimSpace(out)
	}
	holds := func(path, body string) {
		if got, ok := now[path]; !ok || got.object != blob(body) {
			t.Errorf("the jj route's tree holds %q as %+v (held: %v), want the blob of %q", path, got, ok, body)
		}
	}
	for _, path := range listed {
		c, ok := want[path]
		if !ok {
			continue
		}
		for n, side := range c.sides {
			holds(fmt.Sprintf(".jjconflict-side-%d/%s", n, path), side)
		}
		if base := ".jjconflict-base-0/" + path; c.base != "" {
			holds(base, c.base)
		} else if got, ok := now[base]; ok {
			t.Errorf("the jj route's tree holds %q as %+v, want no such path: the base has none", base, got)
		}
		if _, ok := now["JJ-CONFLICT-README"]; !ok {
			t.Errorf("the jj route's tree holds no JJ-CONFLICT-README, want jj's own account of the conflict at %q", path)
		}
	}
}

// differentialRow is one shape of a worktree jj holds.
type differentialRow struct {
	name string
	// build puts the workspace of a disk rescuetest.New made in the row's shape.
	build func(t *testing.T, d *rescuetest.Disk)
	// fails is a text the jj route's failure holds, where the row expects the route to fail; ""
	// where it expects a tree.
	fails string
	// gitlinks and forced are the entries each exception covers in this row, sorted.
	gitlinks, forced []string
	// conflicts is every path the row leaves conflicted, each carried as its sides.
	conflicts map[string]conflict
}

// The limits the rows set in the workspace's own repository configuration, and a body of size
// bytes.
const (
	oneMiB = `"1MiB"`
	oneKiB = `"1KiB"`
)

func sized(size int, fill string) string { return strings.Repeat(fill, size) }

func limitTo(d *rescuetest.Disk, limit string) {
	d.JJ("config", "set", "--repo", "snapshot.max-new-file-size", limit)
}

func symlink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

// embeddedRepository makes an untracked git repository at dir under the workspace, with one
// commit and one file in no commit of its own.
func embeddedRepository(d *rescuetest.Disk, dir string) string {
	d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", dir)
	at := filepath.Join(d.Workspace, dir)
	d.Write(at, "inner.txt", "work inside the embedded repository\n")
	d.Run(at, "git", "add", "inner.txt")
	d.Run(at, "git", "commit", "--quiet", "-m", "work inside the embedded repository")
	d.Write(at, "loose.txt", "a file in no commit of the embedded repository\n")
	return at
}

// excludeInTheClone adds rule to the exclude file of the clone's own git directory, which every
// worktree of it reads.
func excludeInTheClone(t *testing.T, d *rescuetest.Disk, rule string) {
	t.Helper()
	path := strings.TrimSpace(gitRead(t, d, d.Workspace, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	path = filepath.Join(path, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(rule + "\n"); err != nil {
		t.Fatal(err)
	}
}

// commitWithGit commits file with plain git in the workspace, which moves its git HEAD behind
// jj's back.
func commitWithGit(d *rescuetest.Disk, file, body string) {
	d.Write(d.Workspace, file, body)
	d.Git("add", "--", file)
	d.Git("commit", "--quiet", "-m", "a commit made with plain git")
}

// trackedThenIgnoredUnderANestedJJ is the reviewer's shape of this round: d/x.log is committed
// before `*.log` is ignored, and is then edited under a d that gains a nested .jj. jj's snapshot
// leaves d out whole and records d/x.log as deleted; the git read updates it as the tracked file
// it is, whatever an ignore rule says.
func trackedThenIgnoredUnderANestedJJ(t *testing.T, d *rescuetest.Disk) {
	d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
	pushedByTheRole(t, d, "a log file committed before it was ignored")
	d.Write(d.Workspace, ".gitignore", "*.log\n")
	d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "d/x.log", "edited under the nested .jj\n")
}

// nestedJJRepository is the reviewer's shape with the nested .jj a real jj repository, made with
// `jj git init <colocation> d` over the d whose d/x.log head tracks, and d/x.log edited after.
func nestedJJRepository(t *testing.T, d *rescuetest.Disk, colocation string) {
	d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
	pushedByTheRole(t, d, "a log file committed before it was ignored")
	d.Write(d.Workspace, ".gitignore", "*.log\n")
	d.Run(d.Workspace, "jj", "git", "init", colocation, "d")
	d.Write(d.Workspace, "d/x.log", "edited under the nested jj repository\n")
}

var differentialRows = []differentialRow{
	{
		name: "an edit of a tracked file and a new untracked file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
			d.Write(d.Workspace, "base.txt", "base, edited\n")
			requireIndexWithoutStatData(t, d)
		},
	},
	{
		name: "the same with the git HEAD moved behind jj's back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			commitWithGit(d, "fix.txt", "a fix made with plain git\n")
			d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
			d.Write(d.Workspace, "base.txt", "base, edited\n")
		},
	},
	{
		name: "a 2 MiB file above the workspace's own 1 MiB limit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneMiB)
			d.Write(d.Workspace, "big.bin", sized(2<<20, "b"))
			d.JJ("status")
		},
	},
	{
		name: "refused files beside a file the snapshot takes",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			for i := 1; i <= 3; i++ {
				d.Write(d.Workspace, fmt.Sprintf("refused-%02d.bin", i), sized(4096, strconv.Itoa(i)))
			}
			d.Write(d.Workspace, "notes.txt", "a file small enough for the workspace's own limit\n")
		},
	},
	{
		name: "refused names holding a colon and a space",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			for i := 1; i <= 15; i++ {
				d.Write(d.Workspace, fmt.Sprintf("huge-%02d: notes.bin", i), sized(4800, strconv.Itoa(i%10)))
			}
			d.JJ("status")
		},
	},
	{
		name: "a refused name that is pathspec magic beside the tracked file it stands for",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneMiB)
			d.Write(d.Workspace, "aa", "the tracked file the refused name is magic for\n")
			pushedByTheRole(t, d, "the tracked file beside the refused one")
			d.Write(d.Workspace, ":aa", sized(2<<20, "m"))
			d.JJ("status")
		},
	},
	{
		// jj writes this name as two lines, each of the refusal's shape, naming the paths
		// `big` and `z`: neither is there, so the route fails (rescue.carried).
		name: "a refused name holding a newline that jj writes as two refused paths",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			d.Write(d.Workspace, "big: 1B (1 bytes); the maximum size allowed is 1B\n  z", sized(4800, "n"))
		},
		fails: "left out paths that git could not carry",
	},
	{
		name: "a refused name holding a newline whose first line has no shape",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			d.Write(d.Workspace, "refused-01\n  spill.bin", sized(4800, "x"))
		},
		fails: "said it left out paths in lines this check cannot read",
	},
	{
		name: "an untracked nested git repository",
		build: func(t *testing.T, d *rescuetest.Disk) {
			embeddedRepository(d, "nested")
			d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
		},
		gitlinks: []string{"nested"},
	},
	{
		name: "a sparse workspace narrowed to part of its tree",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "other.txt", "a file the narrowed patterns leave out\n")
			pushedByTheRole(t, d, "another file of the tree")
			d.JJ("sparse", "set", "--clear", "--add", "base.txt")
		},
		fails: "holds part of its tree",
	},
	{
		name: "a sparse set of . and one more pattern",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "dir/kept.txt", "a file of the directory the patterns name\n")
			d.JJ("sparse", "set", "--add", "dir")
			pushedByTheRole(t, d, "the file of the directory the patterns name")
			d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
			d.Write(d.Workspace, "dir/new.txt", "an edit in no commit under the directory the patterns name\n")
		},
	},
	{
		name: "an empty sparse set",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.JJ("sparse", "set", "--clear")
		},
		fails: "holds no sparse pattern",
	},
	{
		// conflict.txt is conflicted over a base that holds it, added.txt over one that does
		// not, and notes.txt, written once the merge is made, is no conflict at all.
		name: "a conflicted working copy",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "conflict.txt", "the base of the conflict\n")
			pushedByTheRole(t, d, "the base of the conflict")
			d.Write(d.Workspace, "conflict.txt", "side A\n")
			d.Write(d.Workspace, "added.txt", "added on side A\n")
			d.JJ("describe", "-m", "side A")
			sideA := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
			d.JJ("new", "@-", "-m", "side B")
			d.Write(d.Workspace, "conflict.txt", "side B\n")
			d.Write(d.Workspace, "added.txt", "added on side B\n")
			sideB := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
			d.JJ("new", sideA, sideB, "-m", "a merge the role left conflicted")
			d.Write(d.Workspace, "notes.txt", "an edit in no commit beside the conflict\n")
		},
		conflicts: map[string]conflict{
			"conflict.txt": {sides: [2]string{"side A\n", "side B\n"}, base: "the base of the conflict\n"},
			"added.txt":    {sides: [2]string{"added on side A\n", "added on side B\n"}},
		},
	},
	{
		name: "a name that is not valid UTF-8",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "bad\xfd.txt", "work under a name that is not valid UTF-8\n")
			d.Write(d.Workspace, "sub/worse\xfe", "work in a directory under a name that is not valid UTF-8\n")
		},
	},
	{
		name: "a directory holding a nested .jj that is a directory",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "d/plain.txt", "a file of the directory jj leaves out\n")
			d.Write(d.Workspace, "top.txt", "a file jj's own snapshot takes\n")
		},
	},
	{
		name: "a directory holding a nested .jj that is a regular file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "e/.jj", "a nested .jj that is a regular file\n")
			d.Write(d.Workspace, "e/keep.txt", "the file beside it\n")
		},
	},
	{
		name: "a directory holding a nested .jj that is a symlink",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "h/keep.txt", "the file beside it\n")
			symlink(t, "keep.txt", filepath.Join(d.Workspace, "h", ".jj"))
		},
	},
	{
		name: "an ignored file beside a nested .jj",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "d/keep.txt", "a file of the directory jj leaves out\n")
			d.Write(d.Workspace, "d/x.log", "a file .gitignore covers\n")
		},
	},
	{
		name: "an ignored directory holding a .jj",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "ignored/\n")
			d.Write(d.Workspace, "ignored/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "ignored/keep.txt", "a file of the ignored directory\n")
			d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
		},
	},
	{
		name: "a nested .jj inside an embedded git repository",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := embeddedRepository(d, "b")
			d.Write(at, "sub/.jj/f", "the nested jj's own file, inside the embedded repository\n")
			d.Write(at, "sub/keep.txt", "a file beside that nested .jj\n")
			d.Write(d.Workspace, "notes.txt", "an edit in no commit\n")
		},
		gitlinks: []string{"b"},
	},
	{
		name: "an uncommitted .gitignore with a file it covers",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.tmp\n")
			d.Write(d.Workspace, "scratch.tmp", "a file the uncommitted .gitignore covers\n")
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "d/scratch.tmp", "a file it covers under a nested .jj\n")
			d.Write(d.Workspace, "d/keep.txt", "a file it does not cover under a nested .jj\n")
		},
	},
	{
		name: "an ignore rule with a negated rule beside it",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.log\n!keep.log\n")
			d.Write(d.Workspace, "keep.log", "the negated rule's file\n")
			d.Write(d.Workspace, "other.log", "a file the rule covers\n")
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "d/keep.log", "the negated rule's file under a nested .jj\n")
			d.Write(d.Workspace, "d/other.log", "a file the rule covers under a nested .jj\n")
		},
	},
	{
		name: "a rule in the clone's .git/info/exclude",
		build: func(t *testing.T, d *rescuetest.Disk) {
			excludeInTheClone(t, d, "excluded.txt")
			d.Write(d.Workspace, "excluded.txt", "a file the clone's exclude file covers\n")
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			d.Write(d.Workspace, "d/excluded.txt", "a file it covers under a nested .jj\n")
			d.Write(d.Workspace, "d/keep.txt", "a file it does not cover under a nested .jj\n")
		},
	},
	{
		name: "a symlink whose target is outside the worktree",
		build: func(t *testing.T, d *rescuetest.Disk) {
			outside := filepath.Join(filepath.Dir(d.Remote), "outside.txt")
			d.Write(filepath.Dir(outside), filepath.Base(outside), "a file outside the worktree\n")
			symlink(t, outside, filepath.Join(d.Workspace, "outside"))
			symlink(t, outside, filepath.Join(d.Workspace, "d", "outside"))
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
		},
	},
	{
		name:  "a tracked file an ignore rule covers under a directory that gains a nested .jj",
		build: trackedThenIgnoredUnderANestedJJ,
	},
	{
		name: "a tracked file an ignore rule covers under a directory that becomes a jj repository that is not colocated",
		build: func(t *testing.T, d *rescuetest.Disk) {
			nestedJJRepository(t, d, "--no-colocate")
		},
	},
	{
		// d is an embedded git repository with no commit as well, under which the index still
		// tracks d/x.log: measured on this build, the git read updates d/x.log at 100644 and
		// records no gitlink for d.
		name: "a tracked file an ignore rule covers under a directory that becomes a colocated jj repository with no commit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			nestedJJRepository(t, d, "--colocate")
		},
	},
	{
		// The same with a commit in the embedded repository: measured on this build, the git
		// read still updates d/x.log at 100644 and records no gitlink for d.
		name: "a tracked file an ignore rule covers under a directory that becomes a colocated jj repository with a commit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			nestedJJRepository(t, d, "--colocate")
			d.Run(filepath.Join(d.Workspace, "d"), "jj", "commit", "-m", "work inside the nested repository")
		},
	},
	{
		name: "a file forced into the index with git add -f that .gitignore ignores",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			pushedByTheRole(t, d, "the ignore rules")
			d.Write(d.Workspace, "forced.log", "a file a role forced into the index\n")
			d.Git("add", "-f", "forced.log")
		},
		forced: []string{"forced.log"},
	},
	{
		name:     "every shape that answers a tree at once, the tester's adversarial worktree",
		build:    everyShapeAtOnce,
		gitlinks: []string{"b", "nested"},
		forced:   []string{"forced.log"},
	},
}

// everyShapeAtOnce is one worktree holding every shape of the rows above whose route answers a
// tree. The jj commands come first, then the plain git commit that moves the git HEAD behind
// jj's back, then the files, and the `git add -f` last: any jj command after it would rewrite the
// index that holds the forced file.
func everyShapeAtOnce(t *testing.T, d *rescuetest.Disk) {
	limitTo(d, oneMiB)
	d.Write(d.Workspace, "aa", "the tracked file the refused name is magic for\n")
	d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
	d.Write(d.Workspace, "dir/kept.txt", "a file of the directory the patterns name\n")
	d.JJ("sparse", "set", "--add", "dir")
	pushedByTheRole(t, d, "the tracked files the shapes stand on")
	commitWithGit(d, "fix.txt", "a fix made with plain git\n")

	d.Write(d.Workspace, "base.txt", "base, edited\n")
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "dir/new.txt", "an edit under the directory the patterns name\n")
	d.Write(d.Workspace, "big.bin", sized(2<<20, "b"))
	for i := 1; i <= 3; i++ {
		d.Write(d.Workspace, fmt.Sprintf("huge-%02d: notes.bin", i), sized(1<<20+4800, strconv.Itoa(i)))
	}
	d.Write(d.Workspace, ":aa", sized(2<<20, "m"))
	d.Write(d.Workspace, "bad\xfd.txt", "work under a name that is not valid UTF-8\n")
	embeddedRepository(d, "nested")
	d.Write(d.Workspace, "n/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "n/plain.txt", "a file of the directory jj leaves out\n")
	d.Write(d.Workspace, "e/.jj", "a nested .jj that is a regular file\n")
	d.Write(d.Workspace, "e/keep.txt", "the file beside it\n")
	d.Write(d.Workspace, "h/keep.txt", "the file beside it\n")
	symlink(t, "keep.txt", filepath.Join(d.Workspace, "h", ".jj"))

	d.Write(d.Workspace, ".gitignore", "*.log\n!keep.log\nignored/\n*.tmp\n")
	d.Write(d.Workspace, "scratch.tmp", "a file the uncommitted .gitignore covers\n")
	d.Write(d.Workspace, "keep.log", "the negated rule's file\n")
	d.Write(d.Workspace, "other.log", "a file the rule covers\n")
	d.Write(d.Workspace, "i/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "i/keep.txt", "a file of the directory jj leaves out\n")
	d.Write(d.Workspace, "i/x.log", "a file .gitignore covers beside a nested .jj\n")
	d.Write(d.Workspace, "i/keep.log", "the negated rule's file beside a nested .jj\n")
	d.Write(d.Workspace, "i/scratch.tmp", "a file .gitignore covers beside a nested .jj\n")
	d.Write(d.Workspace, "ignored/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "ignored/keep.txt", "a file of the ignored directory\n")
	at := embeddedRepository(d, "b")
	d.Write(at, "sub/.jj/f", "the nested jj's own file, inside the embedded repository\n")
	d.Write(at, "sub/keep.txt", "a file beside that nested .jj\n")
	excludeInTheClone(t, d, "excluded.txt")
	d.Write(d.Workspace, "excluded.txt", "a file the clone's exclude file covers\n")
	d.Write(d.Workspace, "i/excluded.txt", "a file it covers beside a nested .jj\n")
	outside := filepath.Join(filepath.Dir(d.Remote), "outside.txt")
	d.Write(filepath.Dir(outside), filepath.Base(outside), "a file outside the worktree\n")
	symlink(t, outside, filepath.Join(d.Workspace, "outside"))
	symlink(t, outside, filepath.Join(d.Workspace, "i", "outside"))
	d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "d/x.log", "edited under the nested .jj\n")

	d.Write(d.Workspace, "forced.log", "a file a role forced into the index\n")
	d.Git("add", "-f", "forced.log")
}

// Every entry the whole-worktree git read keeps, the jj route keeps at the same path, mode and
// blob, row by row, but for the two exceptions lostAsRuled names; and where the route fails, the
// row says so.
func TestTheJJRouteKeepsEveryEntryTheWholeWorktreeGitReadKeeps(t *testing.T) {
	for _, row := range differentialRows {
		t.Run(row.name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			row.build(t, d)
			head := d.Git("rev-parse", "HEAD")

			today := readToday(t, d, d.Workspace, head)
			route, err := rescue.TreeTheJJRouteBuilds(context.Background(), d.Env, d.Workspace, head)

			if row.fails != "" {
				if err == nil {
					t.Fatalf("the jj route answered the tree %s, want it to fail saying %q", route, row.fails)
				}
				if !strings.Contains(err.Error(), row.fails) {
					t.Fatalf("the jj route failed saying %q, want %q", err, row.fails)
				}
				return
			}
			if err != nil {
				t.Fatalf("the jj route failed, want a tree: %v", err)
			}
			lostAsRuled(t, d, today, route, row)
		})
	}
}
