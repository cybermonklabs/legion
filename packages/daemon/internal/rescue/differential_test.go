package rescue_test

// The differential between the two reads of a worktree's files (rescue.edits): the whole-tree
// `git add --all` the check made of every worktree before this route existed, and the route it
// takes through a worktree jj holds, jj's own snapshot with what that read would have kept and
// the snapshot left out carried in (rescue.snapshotTree, rescue.carried). The class this stands
// guard over is one: the two trees differ while the workspace reads rescued. Each review of the
// route found one more shape of it; this test holds every shape found so far as a row, and a new
// shape is a new row, which fails here rather than waiting for a reader to think of it.
//
// For each row, two trees are built over the same worktree, in this order. First the git read's,
// because the jj route's snapshot rewrites the worktree's colocated git index, which the git read
// starts from. The git read is written out here, command by command, rather than called in the
// package (rescue.indexTree), so the comparison does not lean on the code under test. Then the jj
// route's (rescue.TreeTheJJRouteBuilds).
//
// The two trees are held to each other in both directions (heldAsRuled). Every entry of the git
// read's tree must be in the jj route's with the same path, the same mode and the same blob, but
// for one exception: an embedded git repository, which the git read records as a gitlink and the
// route records as nothing. And the jj route's tree may hold no path the git read's lacks, but
// for the paths a row declares by name, each with its reason. The exception and each declared
// path are checked for exactly the paths they cover, so an allowance that grew fails as loudly
// as a lost entry does. A conflicted path is neither: it is carried in jj's own shape, which is
// checked path for path on its own (carriedAsItsSides).
//
// One row alone declares such a path, and no other row may: a worktree whose git HEAD is
// unborn, holding a file .gitignore ignores that a role forced into the index with `git add
// -f`. The git read copies the worktree's index only where HEAD names a commit, and otherwise
// starts from an empty index (`read-tree --empty`), so it loses that file; the route seeds what
// it carries from the copy of the index whatever HEAD is, so it keeps the file. A file the role
// put in its own index is work, and the route that pushes it loses nothing the git read kept;
// everywhere else the route must answer the git read's tree exactly.
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

// readToday builds the git read's tree of the worktree at dir, whose git HEAD is head ("" where
// HEAD is unborn), as that read builds it (rescue.indexTree): a copy of the worktree's own git
// index in a scratch file where HEAD names a commit and the worktree has an index, and otherwise
// a scratch index read from head's tree, or from the empty tree where HEAD is unborn; then `git
// add --all` over that scratch index, and the tree it then writes.
func readToday(t *testing.T, d *rescuetest.Disk, dir, head string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	copied := false
	if head != "" {
		own := strings.TrimSpace(gitRead(t, d, dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"))
		body, err := os.ReadFile(own)
		switch {
		case err == nil:
			if err := os.WriteFile(index, body, 0o600); err != nil {
				t.Fatal(err)
			}
			copied = true
		case !errors.Is(err, os.ErrNotExist):
			t.Fatalf("the worktree's own git index %s could not be read: %v", own, err)
		}
	}
	switch {
	case copied:
	case head != "":
		gitRead(t, d, dir, env, "read-tree", head)
	default:
		gitRead(t, d, dir, env, "read-tree", "--empty")
	}
	gitRead(t, d, dir, env, "add", "--all", "--", ".")
	return strings.TrimSpace(gitRead(t, d, dir, env, "write-tree"))
}

// headOf is the commit the worktree at dir has its git HEAD at, as the check reads it
// (rescue.check), and "" where HEAD is unborn.
func headOf(t *testing.T, d *rescuetest.Disk, dir string) string {
	t.Helper()
	out, code, _ := gitExit(t, d, dir, nil, "", "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
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

// conflictTerm is a path of jj's own shape of a conflicted commit's git tree: the base or a side
// of the conflict, at .jjconflict-base-<n>/<path> or .jjconflict-side-<n>/<path>.
var conflictTerm = regexp.MustCompile(`^\.jjconflict-(?:base|side)-[0-9]+/(.+)$`)

// heldAsRuled compares the two trees of a row in both directions.
//
// It fails the test for every entry of the git read's that the jj route's does not hold at the
// same path, mode and blob, but for the one exception: an entry at or under a path the git
// read's tree holds as a gitlink, which is an embedded git repository. `git add --all` records it
// as a gitlink and carries no file of it, and the route records nothing of it (rescue.carried
// leaves out every entry git lists with a trailing slash), so neither read carries what is in it.
// The exception covers only a path the jj route's tree holds nothing at, and is checked for
// exactly the paths it covers: the row names them, and an exception covering one more or one
// fewer than the row names fails, as a lost entry does.
//
// And it fails the test for every path of the jj route's tree that the git read's lacks, but for
// the paths the row declares as gained, each by name with its reason, which are checked the same
// way: a gained path the row does not name fails, and so does a named one the route does not
// gain.
//
// Beside them, and not one of them, is a conflicted path, carried as its sides
// (carriedAsItsSides): a path jj lists as conflicted in the working-copy commit, where the git
// read keeps the file jj materialized with its conflict markers and the jj route keeps every side
// and the base in jj's own shape. It is no exception because nothing of it is lost: the content is
// all present, in jj's own shape, so an exception would hide a loss where the shape is what must
// be proved. Only the paths jj lists are read that way; a path jj does not list follows the
// ordinary rule, and a file of the git read's tree that holds jj's conflict markers at a path jj
// does not list fails. The shape's own paths, which the git read's tree never holds, are held
// path for path the other way: JJ-CONFLICT-README, the conflicted paths' own terms, whose bodies
// carriedAsItsSides reads, and every other path of a term, which must be the entry the git read
// keeps at that path, since a path jj lists no conflict at is one every term holds as the
// worktree does.
func heldAsRuled(t *testing.T, d *rescuetest.Disk, today, route string, row differentialRow) {
	t.Helper()
	was, now := entriesOf(t, d, today), entriesOf(t, d, route)
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
	sorted := func(entries map[string]entry) []string {
		paths := make([]string, 0, len(entries))
		for path := range entries {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		return paths
	}
	var coveredAsGitlinks []string
	for _, path := range sorted(was) {
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
		case !held:
			t.Errorf("lost: the git read keeps %q at mode %s blob %s, and the jj route's tree holds no such path", path, was[path].mode, was[path].object)
		default:
			t.Errorf("changed: the git read keeps %q at mode %s blob %s, and the jj route's tree holds it at mode %s blob %s", path, was[path].mode, was[path].object, kept.mode, kept.object)
		}
	}
	if !slices.Equal(coveredAsGitlinks, row.gitlinks) {
		t.Errorf("the embedded-repository exception covers %q, want exactly the %q this row names", coveredAsGitlinks, row.gitlinks)
	}
	var gained []string
	for _, path := range sorted(now) {
		if _, held := was[path]; held {
			continue
		}
		if len(listed) > 0 {
			if path == "JJ-CONFLICT-README" {
				continue
			}
			if term := conflictTerm.FindStringSubmatch(path); term != nil {
				if slices.Contains(listed, term[1]) {
					continue
				}
				if kept, held := was[term[1]]; held && kept == now[path] {
					continue
				}
				t.Errorf("the jj route's tree holds the conflict term %q at mode %s blob %s, and the git read keeps no such entry at %q, where jj lists no conflict", path, now[path].mode, now[path].object, term[1])
				continue
			}
		}
		gained = append(gained, path)
	}
	declared := make([]string, 0, len(row.gained))
	for path := range row.gained {
		declared = append(declared, path)
	}
	slices.Sort(declared)
	if !slices.Equal(gained, declared) {
		t.Errorf("the jj route's tree holds %q, which the git read's lacks, want exactly the %q this row declares", gained, declared)
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
	// gitlinks is the entries the embedded-repository exception covers in this row, sorted.
	gitlinks []string
	// gained is every path the jj route's tree may hold that the git read's lacks, each with the
	// reason it may; one row alone declares any (the comment at the head of this file).
	gained map[string]string
	// holds is paths both trees must hold, and absent is paths neither may: the comparison
	// alone passes two trees that agree in losing a file, or in keeping one they should not.
	holds, absent []string
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

// intentToAdd is the files, each new in the working copy, as a role's own jj leaves them: one jj
// command snapshots them into the working-copy commit, and the colocated git index it then
// writes holds each as jj writes a file no commit under it holds, an intent-to-add entry at mode
// 100644 and the empty blob. The rows that change a directory after this stand on that entry:
// it is the copy of the index the carry seeds from (rescue.seeds).
func intentToAdd(t *testing.T, d *rescuetest.Disk, files ...string) {
	t.Helper()
	d.JJ("status")
	for _, file := range files {
		want := "100644 e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0\t" + file
		if got := d.Git("ls-files", "-s", "--", file); got != want {
			t.Fatalf("the colocated index holds %s as %q, want the intent-to-add entry %q this row stands on", file, got, want)
		}
	}
}

// becomesAColocatedJJRepository makes dir, which the colocated index tracks a file under, a
// colocated jj repository of its own (`jj git init --colocate` inside it), so it holds a .git
// and a .jj, and writes the new file dir/b.txt there after.
func becomesAColocatedJJRepository(d *rescuetest.Disk, dir string) {
	d.Run(filepath.Join(d.Workspace, dir), "jj", "git", "init", "--colocate")
	d.Write(d.Workspace, dir+"/b.txt", "a file written after the directory became a jj repository\n")
}

// becomesAGitRepository makes dir, which the colocated index tracks a file under, a plain git
// repository of its own, with no .jj, and a commit there holding dir/loose.txt.
func becomesAGitRepository(d *rescuetest.Disk, dir string) {
	d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", dir)
	at := filepath.Join(d.Workspace, dir)
	d.Write(at, "loose.txt", "a file the directory's own repository commits\n")
	d.Run(at, "git", "add", "loose.txt")
	d.Run(at, "git", "commit", "--quiet", "-m", "a commit of the directory's own repository")
}

// becomesADirectory replaces the tracked file at file with a directory of that name holding
// file/k.txt and file/i.log, the second of which `*.log` ignores.
func becomesADirectory(t *testing.T, d *rescuetest.Disk, file string) {
	t.Helper()
	if err := os.Remove(filepath.Join(d.Workspace, file)); err != nil {
		t.Fatal(err)
	}
	d.Write(d.Workspace, file+"/k.txt", "a file of the directory that replaced a tracked file\n")
	d.Write(d.Workspace, file+"/i.log", "a file .gitignore covers in that directory\n")
}

// deleted removes the tracked file at file from the worktree.
func deleted(t *testing.T, d *rescuetest.Disk, file string) {
	t.Helper()
	if err := os.Remove(filepath.Join(d.Workspace, file)); err != nil {
		t.Fatal(err)
	}
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
		// jj writes this name's refusal as two lines, each of the refusal's own shape; the
		// route reads no line jj writes, and git lists the file by its whole name.
		name: "a refused name holding a newline that jj writes as two refused paths",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			d.Write(d.Workspace, "big: 1B (1 bytes); the maximum size allowed is 1B\n  z", sized(4800, "n"))
		},
		holds: []string{"big: 1B (1 bytes); the maximum size allowed is 1B\n  z"},
	},
	{
		// jj writes this name's refusal as two lines, the first with no shape a refusal has.
		name: "a refused name holding a newline whose first line has no shape",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			d.Write(d.Workspace, "refused-01\n  spill.bin", sized(4800, "x"))
		},
		holds: []string{"refused-01\n  spill.bin"},
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
		holds: []string{"forced.log"},
	},
	{
		// The git read leaves forced.log out: with HEAD unborn it starts from an empty index
		// and not from the worktree's (rescue.indexTree), and `*.log` covers the file.
		name: "a file forced into the index with git add -f that .gitignore ignores, under an unborn git HEAD",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.JJ("new", "root()")
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			d.Write(d.Workspace, "forced.log", "a file a role forced into the index\n")
			d.JJ("status")
			d.Git("add", "-f", "forced.log")
			if head := headOf(t, d, d.Workspace); head != "" {
				t.Fatalf("the workspace's git HEAD is %s, want it unborn, which this row stands on", head)
			}
		},
		gained: map[string]string{
			"forced.log": "the index the role forced it into holds it, and the git read, which starts from an empty index where HEAD is unborn, does not read that index",
		},
		holds: []string{".gitignore"},
	},
	{
		name: "a file new in @ under a directory that then becomes a colocated jj repository",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/a.txt", "a file a role's jj snapshotted before d became a repository\n")
			intentToAdd(t, d, "d/a.txt")
			becomesAColocatedJJRepository(d, "d")
		},
		holds: []string{"d/a.txt", "d/b.txt"},
	},
	{
		// No .jj is in m: what tells the route m holds work is the index's entry under it.
		name: "a file new in @ under a directory that then becomes a plain git repository with a commit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "m/a.txt", "a file a role's jj snapshotted before m became a repository\n")
			intentToAdd(t, d, "m/a.txt")
			becomesAGitRepository(d, "m")
		},
		holds: []string{"m/a.txt", "m/loose.txt"},
	},
	{
		name: "a tracked file that becomes a directory holding a file and an ignored file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			d.Write(d.Workspace, "x", "a tracked file that becomes a directory\n")
			pushedByTheRole(t, d, "the file that becomes a directory")
			becomesADirectory(t, d, "x")
		},
		holds:  []string{"x/k.txt"},
		absent: []string{"x", "x/i.log"},
	},
	{
		name: "a tracked file deleted from the worktree",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "gone.txt", "a tracked file the role deletes\n")
			pushedByTheRole(t, d, "the file the role deletes")
			deleted(t, d, "gone.txt")
		},
		absent: []string{"gone.txt"},
	},
	{
		// The reviewer's shape of round 4 (thread PRRT_kwDORFy7ds62y2rH): HEAD tracks d/sub/x.txt,
		// jj drops d whole for its nested .jj, and d/sub becomes a git repository with a commit.
		// The seed's entry under d/sub is what makes git walk into it, as the git read does.
		name: "a tracked file under a directory jj drops for a nested .jj, whose subdirectory becomes a git repository with a commit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/sub/x.txt", "a tracked file under what becomes a repository\n")
			pushedByTheRole(t, d, "the file under d/sub")
			d.Write(d.Workspace, "d/.jj/f", "the nested jj's own file\n")
			becomesAGitRepository(d, "d/sub")
		},
		holds: []string{"d/sub/x.txt", "d/sub/loose.txt", "d/.jj/f"},
	},
	{
		name: "an embedded git repository with a commit and no index entry under it",
		build: func(t *testing.T, d *rescuetest.Disk) {
			embeddedRepository(d, "e")
		},
		gitlinks: []string{"e"},
		absent:   []string{"e/inner.txt", "e/loose.txt"},
	},
	{
		name:     "every shape that answers a tree at once, the tester's adversarial worktree",
		build:    everyShapeAtOnce,
		gitlinks: []string{"b", "eg", "nested"},
		holds: []string{
			"forced.log", "big: 1B (1 bytes); the maximum size allowed is 1B\n  z", "refused-01\n  spill.bin",
			"jd/a.txt", "jd/b.txt", "m/a.txt", "m/loose.txt", "x/k.txt",
		},
		absent: []string{"x", "x/i.log", "gone.txt", "eg/inner.txt", "eg/loose.txt"},
	},
}

// everyShapeAtOnce is one worktree holding every shape of the rows above whose route answers a
// tree and whose git HEAD names a commit; the unborn HEAD's row is the one left out, since its
// HEAD cannot be both. The jj commands come first, the snapshot that puts jd/a.txt and m/a.txt in
// the colocated index among them, then the plain git commit that moves the git HEAD behind jj's
// back, then the files and the directories that change, and the `git add -f` last: any jj
// command of the workspace after it would rewrite the index that holds the forced file.
func everyShapeAtOnce(t *testing.T, d *rescuetest.Disk) {
	limitTo(d, oneMiB)
	d.Write(d.Workspace, "aa", "the tracked file the refused name is magic for\n")
	d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
	d.Write(d.Workspace, "dir/kept.txt", "a file of the directory the patterns name\n")
	d.Write(d.Workspace, "x", "a tracked file that becomes a directory\n")
	d.Write(d.Workspace, "gone.txt", "a tracked file the role deletes\n")
	d.JJ("sparse", "set", "--add", "dir")
	pushedByTheRole(t, d, "the tracked files the shapes stand on")
	d.Write(d.Workspace, "jd/a.txt", "a file a role's jj snapshotted before jd became a repository\n")
	d.Write(d.Workspace, "m/a.txt", "a file a role's jj snapshotted before m became a repository\n")
	intentToAdd(t, d, "jd/a.txt", "m/a.txt")
	commitWithGit(d, "fix.txt", "a fix made with plain git\n")

	d.Write(d.Workspace, "base.txt", "base, edited\n")
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "dir/new.txt", "an edit under the directory the patterns name\n")
	d.Write(d.Workspace, "big.bin", sized(2<<20, "b"))
	for i := 1; i <= 3; i++ {
		d.Write(d.Workspace, fmt.Sprintf("huge-%02d: notes.bin", i), sized(1<<20+4800, strconv.Itoa(i)))
	}
	d.Write(d.Workspace, ":aa", sized(2<<20, "m"))
	d.Write(d.Workspace, "big: 1B (1 bytes); the maximum size allowed is 1B\n  z", sized(1<<20+4800, "n"))
	d.Write(d.Workspace, "refused-01\n  spill.bin", sized(1<<20+4800, "x"))
	d.Write(d.Workspace, "bad\xfd.txt", "work under a name that is not valid UTF-8\n")
	embeddedRepository(d, "nested")
	embeddedRepository(d, "eg")
	d.Write(d.Workspace, "n/.jj/f", "the nested jj's own file\n")
	d.Write(d.Workspace, "n/plain.txt", "a file of the directory jj leaves out\n")
	d.Write(d.Workspace, "e/.jj", "a nested .jj that is a regular file\n")
	d.Write(d.Workspace, "e/keep.txt", "the file beside it\n")
	d.Write(d.Workspace, "h/keep.txt", "the file beside it\n")
	symlink(t, "keep.txt", filepath.Join(d.Workspace, "h", ".jj"))
	becomesAColocatedJJRepository(d, "jd")
	becomesAGitRepository(d, "m")
	becomesADirectory(t, d, "x")
	deleted(t, d, "gone.txt")

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

// heldAndAbsent fails the test where a tree of the row lacks a path the row says both hold, or
// holds a path the row says neither may.
func heldAndAbsent(t *testing.T, d *rescuetest.Disk, row differentialRow, trees map[string]string) {
	t.Helper()
	for name, tree := range trees {
		entries := entriesOf(t, d, tree)
		for _, path := range row.holds {
			if _, ok := entries[path]; !ok {
				t.Errorf("the %s tree holds no %q, want it held", name, path)
			}
		}
		for _, path := range row.absent {
			if e, ok := entries[path]; ok {
				t.Errorf("the %s tree holds %q at mode %s blob %s, want no such path", name, path, e.mode, e.object)
			}
		}
	}
}

// The whole-worktree git read and the jj route answer the same tree, row by row: every entry the
// git read keeps, the jj route keeps at the same path, mode and blob, but for the one exception
// heldAsRuled names, and the jj route keeps no path the git read lacks, but for the paths the one
// row that may declares; and where the route fails, the row says so.
func TestTheJJRouteKeepsEveryEntryTheWholeWorktreeGitReadKeeps(t *testing.T) {
	var declaring []string
	for _, row := range differentialRows {
		if len(row.gained) > 0 {
			declaring = append(declaring, row.name)
		}
	}
	if len(declaring) > 1 {
		t.Fatalf("the rows %q each declare a path the jj route may hold that the git read lacks, want one row at most: the unborn HEAD's", declaring)
	}
	for _, row := range differentialRows {
		t.Run(row.name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			row.build(t, d)
			head := headOf(t, d, d.Workspace)

			today := readToday(t, d, d.Workspace, head)
			route, err := rescue.TreeTheJJRouteBuilds(context.Background(), d.Env, d.Workspace)

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
			heldAsRuled(t, d, today, route, row)
			heldAndAbsent(t, d, row, map[string]string{"git read's": today, "jj route's": route})
		})
	}
}
