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
// read's tree must be in the jj route's with the same path, the same mode and the same blob, and
// the jj route's tree may hold no path the git read's lacks, but for the exceptions below. Each
// is checked for exactly the paths it covers, so an allowance that grew fails as loudly as a lost
// entry does, and so does one that no longer describes the route. A conflicted path is no
// exception: it is carried in jj's own shape, which is checked path for path on its own
// (carriedAsItsSides).
//
// The exceptions are these, each a row of its own:
//
//   - gained: a file new in @ that `git reset` took out of the index and an ignore rule then
//     covers. jj keeps tracking a file it snapshotted, whatever an ignore rule says; the git read,
//     over an index that no longer holds the file, adds no file an ignore rule covers.
//   - contentLost: a file jj's tree holds at the copy of the index's own record, which is a file
//     HEAD holds that no edit jj snapshotted has changed, rewritten at its size with its mtime set
//     back to the one jj recorded. jj's stat rule passes over it, as the role's own jj commit
//     would, so jj's tree keeps the blob jj read before; the git read carries the new one only
//     because the index jj writes has no stat data and git hashes every file. A symlink pointed
//     at a name as long as its old one, its mtime set back the same way, is the same case
//     (matrix_test.go). A file jj's tree holds at another entry than the copy's, an edit or a new
//     file jj snapshotted, is no exception: the carry updates it from the worktree
//     (rescue.seeds).
//   - reformed: a file jj's tree holds at the copy's own record that git converts when it
//     hashes it again, such as CRLF lines committed before an eol rule of .gitattributes was
//     written. The git read stores git's form of the file (LF lines) and the route the bytes HEAD
//     holds. A file the carry adds or updates is in git's form already, since `update-index
//     --add` and `git add -f` run the same conversion: a tracked file rewritten with CRLF lines
//     under an eol rule or core.autocrlf, one rewritten under a clean filter, a new file under
//     that filter and a tracked file whose executable bit was set under core.fileMode=false are
//     each in both trees at the same entry.
//
// Those rows declare each path by name, with its reason, under the word for what the two trees
// hold there, and each declaration is checked against both trees (heldAsRuled):
//
//   - contentLost is a path both trees hold at different entries, where the git read's entry is
//     the worktree's own entry as git stores it (worktreeAsGitStores): a file's `git hash-object
//     -- <path>`, at 100755 where any execute bit of the file is set and at 100644 where none
//     is, or a symlink's target as a blob at 120000; and the jj route's is other content: not a
//     blob git's own conversion at that path stores as the git read's. The route lost the new
//     content the worktree holds, whatever HEAD holds there. It is a word of its own, and not one
//     word for every change, because a change whose route content is not the worktree's is a
//     loss and must read as one.
//   - reformed is a path both trees hold at entries that differ in form alone, where the jj
//     route's entry is the worktree's own file: its bytes as `git hash-object --no-filters`
//     hashes them, at 100755 where any execute bit of the file is set and at 100644 where none
//     is. The git read's entry is git's own conversion of those bytes at the same mode (`git
//     hash-object --path=<path> --stdin`, with the repository's attributes and configuration),
//     or the same blob at the other mode, the one git's index keeps. The content is the same.
//     Anything else fails, and the failure names the side that does not hold its form, so a
//     route that kept HEAD's mode or git's bytes where the worktree's differ does not pass.
//   - lost is a path the git read's tree holds and the jj route's does not.
//   - gained is a path the jj route's tree holds and the git read's does not.
//
// Every path a row declares, under any of the four kinds, carries a reason that is not empty
// (unreasoned), and so does every path a cell of the matrix declares: a declared difference is
// the route keeping less or more than today's read would, and a reader must be told why each one
// is right.
//
// There are kinds at all because a declared difference is how a loss looks: a path the route
// drops passes only written as lost, and a path the route keeps at other content than the
// worktree's only written as contentLost, where a reader sees the word, never as an allowance
// that the two trees may differ. A path declared in two kinds fails, and so does a declared path
// both trees hold at the same entry, an allowance that no longer describes the route. Everywhere
// else the route must answer the git read's tree exactly.
//
// Record, drop or fail. For every path, `git add --all` records it, drops it or fails, and where
// it fails, today's read fails the workspace and the pod is not replaced. So where today's read
// fails, the route must fail too. A row where today's read fails and the route answers a tree
// fails as a loss, whatever the row declares: the route would read clean a worktree today's check
// refuses, and the pod would go with the work in it. A row that sets todayFails without fails
// fails before anything is built, calling the shape a loss. A row whose today's read fails while
// it sets no todayFails fails as well, naming the row and what each read answered.
//
// The opposite shape is a deliberate refusal under R4: the route fails and today's read answers a
// tree, as with a directory this process cannot read, over which `git add --all` warns, exits 0
// and loses what is inside, where jj's snapshot fails; with a directory it can list and not
// search; and with a file marked skip-worktree that was edited and made unreadable. A row whose
// route fails says so (fails), and the failure is asserted: a workspace that fails is never a
// silent loss, since its pod is not replaced for it. A row that expects a tree and gets a
// failure, or expects a failure and gets a tree, fails. Where today's read fails too, the row says
// so beside the route's failure (todayFails), and that failure is asserted in place of its tree,
// with the paths the row stands on, each of which the route's failure must name (failsNaming,
// namesPath): a route that fails there for another reason than today's read does is no agreement
// with it.
//
// The rows are the shapes reviews found. The generated matrix (matrix_test.go) holds the two reads
// to the same comparison over every cell it can build of an index state and a disk state at one
// path, under each git HEAD and index file a worktree can have.

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
	"time"

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
// add --all` over that scratch index, and the tree it then writes. It answers that tree, or the
// error that read fails the workspace with: the step that failed, its exit status and what git
// said. It decides nothing about a failure; its caller holds the failure to the rules (the
// comment at the head of this file). A git that cannot be started, or a scratch index that cannot
// be written, fails the test.
func readToday(t *testing.T, d *rescuetest.Disk, dir, head string) (string, error) {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	run := func(env []string, args ...string) (string, error) {
		out, code, stderr := gitExit(t, d, dir, env, "", args...)
		if code != 0 {
			return "", fmt.Errorf("the git read's `git %s` exited %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr))
		}
		return strings.TrimSpace(out), nil
	}
	copied := false
	if head != "" {
		own, err := run(nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
		if err != nil {
			return "", err
		}
		body, err := os.ReadFile(own)
		switch {
		case err == nil:
			if err := os.WriteFile(index, body, 0o600); err != nil {
				t.Fatal(err)
			}
			copied = true
		case !errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("the git read could not read the worktree's own git index %s: %w", own, err)
		}
	}
	base := []string{"read-tree", "--empty"}
	if head != "" {
		base = []string{"read-tree", head}
	}
	if !copied {
		if _, err := run(env, base...); err != nil {
			return "", err
		}
	}
	if _, err := run(env, "add", "--all", "--", "."); err != nil {
		return "", err
	}
	return run(env, "write-tree")
}

// reads is what the two reads of one worktree answered: each its tree, or the error it fails the
// workspace with.
type reads struct {
	today, route       string
	todayErr, routeErr error
}

// readBoth is the two reads of the worktree d.Workspace, whose git HEAD is head: the git read's
// first, because the jj route's snapshot rewrites the worktree's colocated git index, which the
// git read starts from, then the jj route's (rescue.TreeTheJJRouteBuilds).
func readBoth(t *testing.T, d *rescuetest.Disk, head string) reads {
	t.Helper()
	var r reads
	r.today, r.todayErr = readToday(t, d, d.Workspace, head)
	r.route, r.routeErr = rescue.TreeTheJJRouteBuilds(context.Background(), d.Env, d.Workspace, head)
	return r
}

// answered is a read's answer as a failure message says it: the tree, or the failure.
func answered(tree string, err error) string {
	if err != nil {
		return fmt.Sprintf("fails (%v)", err)
	}
	return "answers the tree " + tree
}

// failureLost is the loss where today's read fails the workspace and the jj route answers a tree,
// said as a failure message says it, and "" for every other pair of answers. Where `git add
// --all` fails, the route must fail the workspace too, whatever a row or a cell declares.
func failureLost(r reads) string {
	if r.todayErr == nil || r.routeErr != nil {
		return ""
	}
	return fmt.Sprintf("loss: today's read fails the workspace (%v), and the jj route answers the tree %s: where `git add --all` fails, the route must fail the workspace too, or it reads clean a worktree today's check refuses", r.todayErr, r.route)
}

// namesPath is whether err, the jj route's failure, names path as the route writes a path, with
// %q (rescue.quoted), or names a path under it: where both reads fail, the route's failure must
// name the path today's read fails on, or it may fail for another reason, which is no agreement.
func namesPath(err error, path string) bool {
	text, named := err.Error(), strconv.Quote(path)
	return strings.Contains(text, named) || strings.Contains(text, strings.TrimSuffix(named, `"`)+"/")
}

// unreasoned is every path row declares under one of its kinds, contentLost, reformed, lost and
// gained, with a reason that is empty, each said as `<kind> <path>`, sorted. A declared
// difference is how a loss looks, so every one carries the reason the two trees may differ
// there, and a reader is told why each one is right.
func unreasoned(row differentialRow) []string {
	var paths []string
	for _, kind := range []struct {
		word     string
		declared map[string]string
	}{{"contentLost", row.contentLost}, {"reformed", row.reformed}, {"lost", row.lost}, {"gained", row.gained}} {
		for path, reason := range kind.declared {
			if strings.TrimSpace(reason) == "" {
				paths = append(paths, fmt.Sprintf("%s %q", kind.word, path))
			}
		}
	}
	slices.Sort(paths)
	return paths
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
// same path, mode and blob, but for a path the row declares by name with its reason, under the
// word for what the two trees hold there: contentLost, reformed, lost or gained. An embedded git
// repository is no exception: the route records it as the gitlink the git read records, or fails
// where that read fails (rescue.carried). The walk over the git read's entries passes over a
// path declared in any kind, which is checked below; the walk over the jj route's paths the git
// read's lacks passes over only a path declared gained. Then each declaration is
// held to what the two trees hold. A path declared contentLost or reformed must be in both trees,
// at different entries: one both hold at the same entry fails as an allowance that no longer
// describes the route, and one either tree lacks fails, since that is a loss or a gain and is
// declared as one. Beyond that, a path declared contentLost must be held by the git read's tree
// at the worktree's own entry as git stores it (worktreeAsGitStores): a file's blob `git
// hash-object -- <path>` at its mode by its execute bits, or a symlink's target as a blob at
// 120000, which is the new content; and by the jj route's tree at other content, an entry that
// is no regular file or a blob git's own conversion at that path does not store as the git
// read's (asGitStores): the route lost the new content the worktree holds, whatever HEAD holds
// there. Either side that is not so fails, naming both trees' entries, and the row of CRLF lines
// committed before an eol rule fails so declared, at the route's side, whose bytes git stores as
// the git read's blob. A path declared reformed must be a regular file in both trees, and the jj
// route's entry must be the worktree's own file at that path, its
// bytes unconverted and its mode by its execute bits (worktreeHolds); the git read's entry must
// then differ from it in form alone: the same blob at the other mode, or the same mode and the
// blob git's own conversion of the route's bytes makes at that path (asGitStores). Anything else
// is a change of content, not of form, and fails naming the side that does not hold its form:
// the jj route's where its entry is not the worktree's file, and the git read's where its entry
// is not git's form of it. A path declared lost must be in the git read's tree and not in the jj
// route's. A path declared gained must be in the jj route's tree and not in the git read's. A
// path declared in more than one kind fails. There are kinds at all because a declared difference
// is how a loss looks: a path the route drops passes only written as lost, and a path the route
// keeps at other content than the worktree's only written as contentLost, where a reader sees
// the word, never as an allowance that the trees differ. contentLost is a word apart from
// reformed for that reason: a change whose route content is not the worktree's is a loss, and
// must read as one, where a reformed path loses nothing.
//
// And it fails the test for every path of the jj route's tree that the git read's lacks, but for
// the paths the row declares gained.
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
	heldEntriesAsRuled(t, d, entriesOf(t, d, today), entriesOf(t, d, route), conflictedPaths(t, d), row)
}

// heldEntriesAsRuled is heldAsRuled over the entries of the two trees, was the git read's and now
// the jj route's, with listed the paths jj lists as conflicted: the matrix holds each cell to the
// rule over the entries under the cell's own path alone (matrix_test.go).
func heldEntriesAsRuled(t *testing.T, d *rescuetest.Disk, was, now map[string]entry, listed []string, row differentialRow) {
	t.Helper()
	carriedAsItsSides(t, d, now, listed, row.conflicts)
	sorted := func(entries map[string]entry) []string {
		paths := make([]string, 0, len(entries))
		for path := range entries {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		return paths
	}
	kinds := []struct {
		word     string
		declared map[string]string
	}{{"contentLost", row.contentLost}, {"reformed", row.reformed}, {"lost", row.lost}, {"gained", row.gained}}
	declared := func(path string) bool {
		for _, kind := range kinds {
			if _, ok := kind.declared[path]; ok {
				return true
			}
		}
		return false
	}
	for _, path := range sorted(was) {
		if slices.Contains(listed, path) {
			continue
		}
		if e := was[path]; e.mode != gitlinkMode && conflictMarker.MatchString(gitRead(t, d, d.Workspace, nil, "cat-file", "blob", e.object)) {
			t.Errorf("the git read keeps %q holding jj's conflict markers, and jj lists no conflict at that path", path)
		}
		kept, held := now[path]
		switch {
		case declared(path):
			continue
		case held && kept == was[path]:
			continue
		case !held:
			t.Errorf("lost: the git read keeps %q at mode %s blob %s, and the jj route's tree holds no such path", path, was[path].mode, was[path].object)
		default:
			t.Errorf("differs: the git read keeps %q at mode %s blob %s, and the jj route's tree holds it at mode %s blob %s, which the row declares neither contentLost nor reformed", path, was[path].mode, was[path].object, kept.mode, kept.object)
		}
	}
	var gained []string
	for _, path := range sorted(now) {
		if _, held := was[path]; held {
			continue
		}
		if _, ok := row.gained[path]; ok {
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
	if len(gained) > 0 {
		t.Errorf("gained: the jj route's tree holds %q, which the git read's lacks and the row does not declare gained", gained)
	}
	holding := func(entries map[string]entry, path string) string {
		if e, ok := entries[path]; ok {
			return fmt.Sprintf("mode %s blob %s", e.mode, e.object)
		}
		return "no such path"
	}
	for i, kind := range kinds {
		paths := make([]string, 0, len(kind.declared))
		for path := range kind.declared {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		for _, path := range paths {
			reason := kind.declared[path]
			var also []string
			for _, other := range kinds[i+1:] {
				if why, ok := other.declared[path]; ok {
					also = append(also, fmt.Sprintf("%s (%s)", other.word, why))
				}
			}
			if len(also) > 0 {
				t.Errorf("the row declares %q %s (%s) and also %s, where a path is declared in one kind: the git read's tree holds %s and the jj route's %s", path, kind.word, reason, strings.Join(also, " and "), holding(was, path), holding(now, path))
			}
			before, inWas := was[path]
			after, inNow := now[path]
			if kind.word == "contentLost" || kind.word == "reformed" {
				switch {
				case !inWas && !inNow:
					t.Errorf("the row declares %q %s (%s), and neither tree holds that path: the allowance no longer describes the route", path, kind.word, reason)
					continue
				case !inNow:
					t.Errorf("the row declares %q %s (%s), and the jj route's tree holds no such path, where the git read's holds %s: a path one tree lacks is a loss or a gain, and is declared as one", path, kind.word, reason, holding(was, path))
					continue
				case !inWas:
					t.Errorf("the row declares %q %s (%s), and the git read's tree holds no such path, where the jj route's holds %s: a path one tree lacks is a loss or a gain, and is declared as one", path, kind.word, reason, holding(now, path))
					continue
				case before == after:
					t.Errorf("the row declares %q %s (%s), and both trees hold it at mode %s blob %s: the allowance no longer describes the route", path, kind.word, reason, before.mode, before.object)
					continue
				}
			}
			regular := func(mode string) bool { return mode == "100644" || mode == "100755" }
			switch kind.word {
			case "contentLost":
				stored, unreadable := worktreeAsGitStores(t, d, path)
				if unreadable != "" {
					t.Errorf("the row declares %q contentLost (%s), and %s, so the git read's side cannot be the new content the worktree holds: a path declared contentLost is one the git read keeps as the worktree's own file or symlink, as git stores it", path, reason, unreadable)
					continue
				}
				switch {
				case before != stored:
					t.Errorf("the row declares %q contentLost (%s), and the git read's side fails: its tree holds the path at mode %s blob %s, where the worktree's own entry as git stores it is mode %s blob %s (a file's execute bits and `git hash-object -- <path>`, or 120000 and the blob of a symlink's target): a path declared contentLost is one the git read keeps as the new content the worktree holds, and the jj route's tree holds mode %s blob %s", path, reason, before.mode, before.object, stored.mode, stored.object, after.mode, after.object)
				case regular(after.mode) && asGitStores(t, d, path, after.object) == before.object:
					t.Errorf("the row declares %q contentLost (%s), and the jj route's side fails: its tree holds the path at mode %s blob %s, which git's own conversion at that path stores as the git read's blob %s: that is the same content in another form, not other content, and a path declared contentLost is one the route keeps at other content than the worktree's", path, reason, after.mode, after.object, before.object)
				}
			case "reformed":
				own, unreadable := worktreeHolds(t, d, path)
				switch {
				case !regular(before.mode) || !regular(after.mode):
					t.Errorf("the row declares %q reformed (%s), and the git read's tree holds it at mode %s blob %s and the jj route's at mode %s blob %s: git converts the form of a regular file alone, so this is a change of content, not of form", path, reason, before.mode, before.object, after.mode, after.object)
				case unreadable != "":
					t.Errorf("the row declares %q reformed (%s), and %s, so the jj route's side cannot be its form: a reformed path is a regular file of the worktree that the route keeps as the worktree holds it", path, reason, unreadable)
				case after != own:
					t.Errorf("the row declares %q reformed (%s), and the jj route's side fails: its tree holds the path at mode %s blob %s, where the worktree's own file is mode %s blob %s (its execute bits, and `git hash-object --no-filters`); a reformed path is one the route keeps in the worktree's own bytes and mode, and the git read's tree holds mode %s blob %s", path, reason, after.mode, after.object, own.mode, own.object, before.mode, before.object)
				case before.object == after.object:
				case before.mode != after.mode:
					t.Errorf("the row declares %q reformed (%s), and the git read's side fails: its tree holds the path at mode %s blob %s, and the jj route's, the worktree's own file, at mode %s blob %s: both the mode and the blob differ, so this is a change of content, not of form", path, reason, before.mode, before.object, after.mode, after.object)
				default:
					if stored := asGitStores(t, d, path, after.object); stored != before.object {
						t.Errorf("the row declares %q reformed (%s), and the git read's side fails: its tree holds the path at blob %s, and the jj route's, the worktree's own file, at blob %s, which git's own conversion at that path stores as blob %s: this is a change of content, not of form", path, reason, before.object, after.object, stored)
					}
				}
			case "lost":
				if !inWas || inNow {
					t.Errorf("the row declares %q lost (%s), and the git read's tree holds %s and the jj route's %s: a lost path is one the git read's tree holds and the jj route's does not", path, reason, holding(was, path), holding(now, path))
				}
			case "gained":
				if inWas || !inNow {
					t.Errorf("the row declares %q gained (%s), and the git read's tree holds %s and the jj route's %s: a gained path is one the jj route's tree holds and the git read's does not", path, reason, holding(was, path), holding(now, path))
				}
			}
		}
	}
}

// asGitStores is the blob git stores at path for the bytes of the blob object: `git hash-object
// --path=<path> --stdin` in the workspace over those bytes, which converts them as `git add`
// converts the file at path, with the repository's attributes and configuration (an eol rule,
// core.autocrlf, a clean filter).
func asGitStores(t *testing.T, d *rescuetest.Disk, path, object string) string {
	t.Helper()
	body := gitRead(t, d, d.Workspace, nil, "cat-file", "blob", object)
	out, code, stderr := gitExit(t, d, d.Workspace, nil, body, "hash-object", "--path="+path, "--stdin")
	if code != 0 {
		t.Fatalf("git hash-object --path=%s exited %d: %s", path, code, stderr)
	}
	return strings.TrimSpace(out)
}

// fileAsGitStores is the blob git stores for the worktree's own file at path: `git hash-object --
// <path>` in the workspace, which converts the file's bytes as `git add` converts them, with the
// repository's attributes and configuration, and writes no object.
func fileAsGitStores(t *testing.T, d *rescuetest.Disk, path string) string {
	t.Helper()
	out, code, stderr := gitExit(t, d, d.Workspace, nil, "", "hash-object", "--", path)
	if code != 0 {
		t.Fatalf("git hash-object -- %s exited %d: %s", path, code, stderr)
	}
	return strings.TrimSpace(out)
}

// worktreeAsGitStores is the entry git stores for the worktree's own entry at path. A regular
// file is its blob as `git hash-object -- <path>` converts it (fileAsGitStores), at 100755 where
// any execute bit of the file is set and at 100644 where none is; a symlink is mode 120000 and
// the blob of its target, `git hash-object --stdin` over what readlink answers, which git stores
// unconverted. Where the worktree holds neither at path, it answers why, and the entry is empty.
func worktreeAsGitStores(t *testing.T, d *rescuetest.Disk, path string) (entry, string) {
	t.Helper()
	at := filepath.Join(d.Workspace, path)
	info, err := os.Lstat(at)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(at)
		if err != nil {
			return entry{}, fmt.Sprintf("the worktree's symlink at that path cannot be read (%v)", err)
		}
		out, code, stderr := gitExit(t, d, d.Workspace, nil, target, "hash-object", "--stdin")
		if code != 0 {
			t.Fatalf("git hash-object --stdin over the target of %s exited %d: %s", path, code, stderr)
		}
		return entry{mode: "120000", object: strings.TrimSpace(out)}, ""
	}
	own, unreadable := worktreeHolds(t, d, path)
	if unreadable != "" {
		return entry{}, unreadable
	}
	return entry{mode: own.mode, object: fileAsGitStores(t, d, path)}, ""
}

// worktreeHolds is the entry the worktree's own file at path is with no conversion of git's: the
// blob `git hash-object --no-filters` makes of its bytes, at 100755 where any execute bit of the
// file's mode is set and at 100644 where none is. Where the worktree holds no regular file at
// path, it answers why, and the entry is empty.
func worktreeHolds(t *testing.T, d *rescuetest.Disk, path string) (entry, string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(d.Workspace, path))
	switch {
	case err != nil:
		return entry{}, fmt.Sprintf("the worktree's file at that path cannot be read (%v)", err)
	case !info.Mode().IsRegular():
		return entry{}, fmt.Sprintf("the worktree holds no regular file at that path, but one of mode %s", info.Mode())
	}
	mode := "100644"
	if info.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}
	out, code, stderr := gitExit(t, d, d.Workspace, nil, "", "hash-object", "--no-filters", "--", path)
	if code != 0 {
		t.Fatalf("git hash-object --no-filters -- %s exited %d: %s", path, code, stderr)
	}
	return entry{mode: mode, object: strings.TrimSpace(out)}, ""
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
	// todayFails is a text the git read's failure holds, where the row expects that read to fail
	// the workspace as well; "" where it expects a tree. A row sets it only beside fails: where
	// `git add --all` fails and the jj route answers a tree, that is a loss, and no row declares
	// it (the comment at the head of this file).
	todayFails string
	// failsNaming is the paths a row that sets todayFails stands on, each of which the jj
	// route's failure must name as the route writes a path (namesPath): a route that fails for
	// another reason than today's read is no agreement with it. Every row that sets todayFails
	// sets it.
	failsNaming []string
	// contentLost, reformed, lost and gained are the paths the row declares the two trees differ
	// at, each with the reason they may: the exceptions the comment at the head of this file
	// lists, each checked for exactly its path against what the two trees hold (heldAsRuled).
	// contentLost is a path both trees hold at different entries, the git read's being the
	// worktree's own file or symlink as git stores it and the jj route's other content: the
	// route lost the new content the worktree holds, a loss, written under its own word so that
	// it reads as one, whatever HEAD holds there. reformed is a path both trees hold as a
	// regular file at entries that differ in form alone, by git's conversion of the file on its
	// way into the index: the same blob at the other mode, or a blob git's conversion of the
	// route's bytes turns into the git read's. lost is a path the git read's tree holds and the
	// jj route's does not, written under that word because a declared absence is how a loss
	// looks. gained is a path the jj route's tree holds and the git read's does not. Every path
	// of every kind carries a reason that is not empty (unreasoned).
	contentLost, reformed, lost, gained map[string]string
	// holds is paths both trees must hold, and absent is paths neither may: the comparison
	// alone passes two trees that agree in losing a file, or in keeping one they should not.
	holds, absent []string
	// conflicts is every path the row leaves conflicted, each carried as its sides.
	conflicts map[string]conflict
}

// trackedThenIgnored is the reason of the one gain: the env.local row's, and the matrix cells'
// of the same cause (matrixDeclared).
const trackedThenIgnored = "jj tracks the file because it snapshotted it, the index the git read starts from lacks it, and an ignore rule covers it, so jj keeps it whatever the rule says and the git read adds no file the rule covers; whether that index lacks it through a git reset, a missing index file or an unborn HEAD is a detail of that one cause"

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

// flaggedThenADirectory commits the file x beside a .gitignore of `*.log`, marks it with flag
// (--assume-unchanged or --skip-worktree, and no mark where flag is empty), and replaces it with
// a directory of that name holding each of files, and nothing where files is empty.
func flaggedThenADirectory(t *testing.T, d *rescuetest.Disk, flag string, files ...string) {
	t.Helper()
	d.Write(d.Workspace, ".gitignore", "*.log\n")
	d.Write(d.Workspace, "x", "a committed file that is marked and then becomes a directory\n")
	pushedByTheRole(t, d, "the file that is marked and becomes a directory")
	if flag != "" {
		d.Git("update-index", flag, "x")
	}
	deleted(t, d, "x")
	if err := os.Mkdir(filepath.Join(d.Workspace, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		d.Write(d.Workspace, "x/"+file, "a file of the directory that replaced a marked file\n")
	}
}

// removeAll removes the path at, and everything under it.
func removeAll(t *testing.T, at string) {
	t.Helper()
	if err := os.RemoveAll(at); err != nil {
		t.Fatal(err)
	}
}

// committedSubmodule makes dir an embedded git repository (embeddedRepository) and commits it with
// plain git as the gitlink it is, which moves the workspace's git HEAD behind jj's back, and
// answers the repository's directory.
func committedSubmodule(d *rescuetest.Disk, dir string) string {
	at := embeddedRepository(d, dir)
	d.Git("add", dir)
	d.Git("commit", "--quiet", "-m", "a submodule committed with plain git")
	return at
}

// withoutIndexFile removes the workspace's own git index file, under a git HEAD that must name a
// commit, so the git read starts from `read-tree <head>` (rescue.indexTree).
func withoutIndexFile(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	if headOf(t, d, d.Workspace) == "" {
		t.Fatal("the workspace's git HEAD is unborn, want it to name a commit, which this row stands on")
	}
	if err := os.Remove(strings.TrimSpace(gitRead(t, d, d.Workspace, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"))); err != nil {
		t.Fatal(err)
	}
}

// rewrittenAtTheSameSize rewrites the tracked file at file with body, which is the size of what
// the role's jj recorded there. With setBack the file's mtime is then set back to the one it had
// before, which is the one jj recorded, so the file has the size and the mtime jj's stat rule
// compares; without it the mtime is left where the write moved it, and the row fails where it did
// not move, since the row then stands on nothing.
func rewrittenAtTheSameSize(t *testing.T, d *rescuetest.Disk, file, body string, setBack bool) {
	t.Helper()
	path := filepath.Join(d.Workspace, file)
	was, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if was.Size() != int64(len(body)) {
		t.Fatalf("%s holds %d bytes, want the %d of the body it is rewritten with", file, was.Size(), len(body))
	}
	d.Write(d.Workspace, file, body)
	if setBack {
		if err := os.Chtimes(path, was.ModTime(), was.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	now, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if moved := !now.ModTime().Equal(was.ModTime()); moved == setBack {
		t.Fatalf("%s has the mtime %s after the rewrite, from %s before it: the row stands on its mtime being set back (%v)", file, now.ModTime(), was.ModTime(), setBack)
	}
}

// unreadableFile makes the file at file mode 000, and gives it back its mode when the test ends.
// A process that reads such a file all the same, as root does, makes no file git cannot read, so
// the test is skipped there.
func unreadableFile(t *testing.T, d *rescuetest.Disk, file string) {
	t.Helper()
	at := filepath.Join(d.Workspace, file)
	if err := os.Chmod(at, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(at, 0o644) })
	if f, err := os.Open(at); err == nil {
		f.Close()
		t.Skip("this process reads a file of mode 000, as root does, so no file it writes is one git cannot read")
	}
}

// requiredFilterFails commits f.fx under a .gitattributes that puts it through the clean filter
// bad, which the repository does not yet configure, and then configures bad as a required filter
// that fails on every file. The index the role's jj then wrote has no stat data, so the git read
// hashes f.fx again through that filter.
func requiredFilterFails(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	d.Write(d.Workspace, ".gitattributes", "*.fx filter=bad\n")
	d.Write(d.Workspace, "f.fx", "AAAA\n")
	pushedByTheRole(t, d, "a file under a clean filter the repository does not configure yet")
	requireIndexWithoutStatData(t, d)
	d.Git("config", "filter.bad.clean", "false")
	d.Git("config", "filter.bad.required", "true")
}

// requiredFilterWorks is requiredFilterFails with a required clean filter that works: up, which
// writes a file's letters in upper case, over f.fx committed as aaaa.
func requiredFilterWorks(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	d.Write(d.Workspace, ".gitattributes", "*.fx filter=up\n")
	d.Write(d.Workspace, "f.fx", "aaaa\n")
	pushedByTheRole(t, d, "a file under a clean filter the repository does not configure yet")
	requireIndexWithoutStatData(t, d)
	d.Git("config", "filter.up.clean", "tr a-z A-Z")
	d.Git("config", "filter.up.required", "true")
}

// safecrlfRefuses commits c.txt in CRLF lines, and then writes the eol rule `*.txt text eol=lf`
// and sets core.safecrlf=true, under which git's add refuses a file whose CRLF lines the rule
// would turn into LF. The index the role's jj wrote has no stat data, so the git read hashes
// c.txt again under that rule.
func safecrlfRefuses(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	d.Write(d.Workspace, "c.txt", "AB\r\n")
	pushedByTheRole(t, d, "a file of CRLF lines committed before an eol rule was written")
	d.Write(d.Workspace, ".gitattributes", "*.txt text eol=lf\n")
	d.Git("config", "core.safecrlf", "true")
	requireIndexWithoutStatData(t, d)
}

// encodingRefuses commits w.u16 as six bytes with no byte order mark, and then writes the rule
// `*.u16 working-tree-encoding=UTF-16`, under which git's add refuses UTF-16 that has no byte
// order mark. The index the role's jj wrote has no stat data, so the git read hashes w.u16 again
// under that rule.
func encodingRefuses(t *testing.T, d *rescuetest.Disk) {
	t.Helper()
	d.Write(d.Workspace, "w.u16", "AAAAA\n")
	pushedByTheRole(t, d, "a file committed before its working-tree-encoding was written")
	d.Write(d.Workspace, ".gitattributes", "*.u16 working-tree-encoding=UTF-16\n")
	requireIndexWithoutStatData(t, d)
}

// hiddenRewrite sets the tracked file's mtime a minute back, has the role's jj record it with
// `jj status`, and rewrites it with body, which is its size, with that mtime set back again
// (rewrittenAtTheSameSize): an edit jj's stat rule passes over, so jj's tree keeps what HEAD
// holds there.
func hiddenRewrite(t *testing.T, d *rescuetest.Disk, file, body string) {
	t.Helper()
	back := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(d.Workspace, file), back, back); err != nil {
		t.Fatal(err)
	}
	d.JJ("status")
	rewrittenAtTheSameSize(t, d, file, body, true)
	requireIndexWithoutStatData(t, d)
}

// statDataByGitThenUnreadable commits u.txt with its mtime a minute back, has git write its
// record with stat data (`update-index --refresh`, after the role's last jj command, since jj
// writes none), and makes it mode 000 once its ctime is in a later second than the record's
// (pastCtimeSecond). With trustCtime git compares that ctime, as it does by default, and
// otherwise the repository sets core.trustctime=false. The row stands on git's own stat
// comparison, which is checked here: u.txt reads as changed to `git diff-files` with trustCtime
// and as unchanged without it.
func statDataByGitThenUnreadable(t *testing.T, d *rescuetest.Disk, trustCtime bool) {
	t.Helper()
	at := filepath.Join(d.Workspace, "u.txt")
	d.Write(d.Workspace, "u.txt", "AAAA\n")
	setTimes(t, at, time.Now().Add(-time.Minute))
	pushedByTheRole(t, d, "a file whose record git then writes with stat data")
	if !trustCtime {
		d.Git("config", "core.trustctime", "false")
	}
	if _, code, stderr := gitExit(t, d, d.Workspace, nil, "", "update-index", "-q", "--refresh"); code != 0 {
		t.Fatalf("git update-index --refresh exited %d: %s", code, stderr)
	}
	pastCtimeSecond(t, at)
	unreadableFile(t, d, "u.txt")
	_, code, stderr := gitExit(t, d, d.Workspace, nil, "", "diff-files", "--quiet", "--", "u.txt")
	if changed := code == 1; changed != trustCtime || code > 1 {
		t.Fatalf("git diff-files --quiet -- u.txt exited %d (%s) with ctime compared %v: the row stands on git reading u.txt as changed exactly where it compares the ctime", code, stderr, trustCtime)
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
		holds: []string{"nested"},
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
		// The git read warns that it cannot open shut, exits 0 and answers a tree without
		// shut/inside.txt; the route refuses, since a step that cannot tell whether there is
		// work to save refuses the replacement rather than lose that work with the pod (R4).
		name: "a directory this process cannot read",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "shut/inside.txt", "a file under a directory no one may read\n")
			d.Write(d.Workspace, "open.txt", "a file beside it\n")
			d.JJ("status")
			shut := filepath.Join(d.Workspace, "shut")
			if err := os.Chmod(shut, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(shut, 0o755) })
			if f, err := os.Open(shut); err == nil {
				f.Close()
				t.Skip("this process opens a directory of mode 000, as root does, so no directory it makes is one jj cannot read")
			}
		},
		fails:  "Failed to read directory",
		holds:  []string{"open.txt"},
		absent: []string{"shut/inside.txt"},
	},
	{
		// Measured on this build on 2026-11-13 over this shape: `git add --all` over a copy of
		// the index warned `half/inside.txt: Permission denied` and `half/new.txt: Permission
		// denied`, exited 0 and kept both as the copy recorded them, half/new.txt at the empty
		// blob jj wrote for a file new in @, so its content was lost; jj's snapshot failed
		// (`Failed to stat file …/half/new.txt: Permission denied`). The route refuses, as for
		// the directory above (R4).
		name: "a directory this process can list but not search",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "half/inside.txt", "a committed file under a directory no one may search\n")
			pushedByTheRole(t, d, "a file under a directory that is then made unsearchable")
			d.Write(d.Workspace, "half/new.txt", "a new file under it\n")
			d.Write(d.Workspace, "open.txt", "a file beside it\n")
			d.JJ("status")
			half := filepath.Join(d.Workspace, "half")
			if err := os.Chmod(half, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(half, 0o755) })
			if _, err := os.Lstat(filepath.Join(half, "inside.txt")); err == nil {
				t.Skip("this process searches a directory of mode 0644, as root does, so no directory it makes is one jj cannot stat a file in")
			}
		},
		fails: "Failed to stat file",
		holds: []string{"open.txt", "half/inside.txt"},
	},
	{
		// Measured on this build on 2026-11-13 over this shape: `git add --all` over a copy of
		// the index exited 0 and kept s.txt at HEAD's blob without opening it, as it keeps
		// every file marked skip-worktree; jj's snapshot, the file's mtime and size having
		// moved, failed to open it (`Failed to open file …/s.txt: Permission denied`). The
		// route refuses, as for the directory above (R4).
		name: "a file marked skip-worktree, edited and then made unreadable",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "s.txt", "s\n")
			pushedByTheRole(t, d, "a file then marked skip-worktree")
			d.Git("update-index", "--skip-worktree", "--", "s.txt")
			d.Write(d.Workspace, "s.txt", "s, edited\n")
			unreadableFile(t, d, "s.txt")
		},
		fails: "Failed to open file",
		holds: []string{"s.txt"},
	},
	{
		// Measured on this build on 2026-11-13 over this shape: `git add --all` over a copy of
		// the index exited 128 (`error: open("u.txt"): Permission denied`, `fatal: updating
		// files failed`), where `jj status` answered `The working copy has no changes.`. The
		// route refuses it as the git read does (rescue.unreadable).
		name: "a tracked file this process cannot read, as it was committed",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "u.txt", "AAAA\n")
			pushedByTheRole(t, d, "a file that is then made unreadable")
			requireIndexWithoutStatData(t, d)
			unreadableFile(t, d, "u.txt")
		},
		todayFails:  `open("u.txt"): Permission denied`,
		fails:       "cannot be read, and the git read fails on a tracked file it cannot read",
		failsNaming: []string{"u.txt"},
	},
	{
		// The tester's shape: jj's stat rule passes over the rewrite, so jj's tree holds HEAD's
		// AAAA and a route that read u.txt clean would lose BBBB, where the git read fails on
		// it. Measured on this build on 2026-11-13: `git add --all` over a copy of the index
		// exited 128 with the same open error, and `jj status` answered `The working copy has
		// no changes.`.
		name: "a tracked file rewritten at its size with its mtime set back, then made unreadable",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "u.txt", "AAAA\n")
			pushedByTheRole(t, d, "a file that is then rewritten and made unreadable")
			back := time.Now().Add(-time.Minute)
			if err := os.Chtimes(filepath.Join(d.Workspace, "u.txt"), back, back); err != nil {
				t.Fatal(err)
			}
			d.JJ("status")
			rewrittenAtTheSameSize(t, d, "u.txt", "BBBB\n", true)
			requireIndexWithoutStatData(t, d)
			unreadableFile(t, d, "u.txt")
		},
		todayFails:  `open("u.txt"): Permission denied`,
		fails:       "cannot be read, and the git read fails on a tracked file it cannot read",
		failsNaming: []string{"u.txt"},
	},
	{
		// git wrote u.txt's record with its stat data, and core.trustctime=false has it compare
		// no ctime, so the chmod changes nothing git compares: measured with git 2.47.3 on
		// 2026-11-13, `git add --all` over a copy of the index exited 0 and kept u.txt at its
		// record without opening it, where the CI's red reproduced. The route asks git's own stat
		// comparison (rescue.wouldRead) and answers the same tree.
		name: "a tracked file git trusts under core.trustctime=false, made unreadable",
		build: func(t *testing.T, d *rescuetest.Disk) {
			statDataByGitThenUnreadable(t, d, false)
		},
		holds: []string{"u.txt"},
	},
	{
		// The same record with ctime compared, as git compares it by default, and the chmod's
		// ctime in a later second than the record's: git's stat comparison reports u.txt
		// changed, so both reads open it and fail.
		name: "a tracked file whose stat data git wrote, made unreadable in a later ctime second",
		build: func(t *testing.T, d *rescuetest.Disk) {
			statDataByGitThenUnreadable(t, d, true)
		},
		todayFails:  `open("u.txt"): Permission denied`,
		fails:       "cannot be read, and the git read fails on a tracked file it cannot read",
		failsNaming: []string{"u.txt"},
	},
	{
		// Measured on this build on 2026-11-13 over this shape: `git add --all` over a copy of
		// the index exited 128 (`error: external filter 'false' failed 1`, `fatal: f.fx: clean
		// filter 'bad' failed`), where `jj status` answered `The working copy has no changes.`;
		// the route reads f.fx again through the carry's `git add -f` (rescue.converting) and
		// fails with it.
		name: "a tracked file a required clean filter fails on, as it was committed",
		build: func(t *testing.T, d *rescuetest.Disk) {
			requiredFilterFails(t, d)
		},
		todayFails:  "clean filter 'bad' failed",
		fails:       "clean filter 'bad' failed",
		failsNaming: []string{"f.fx"},
	},
	{
		// Once the file is edited, jj snapshots the edit and the carry updates it with `git add
		// -f`, which runs the filter as the git read does and fails with it.
		name: "a tracked file a required clean filter fails on, edited",
		build: func(t *testing.T, d *rescuetest.Disk) {
			requiredFilterFails(t, d)
			d.Write(d.Workspace, "f.fx", "AAAA, edited\n")
		},
		todayFails:  "clean filter 'bad' failed",
		fails:       "clean filter 'bad' failed",
		failsNaming: []string{"f.fx"},
	},
	{
		// The tester's shape: an edit jj's stat rule passes over, so jj's tree holds HEAD's
		// AAAA, under a filter that fails. Before the route read such a file again, it answered
		// HEAD's tree here and lost BBBB, where the git read fails.
		name: "a tracked file a required clean filter fails on, rewritten at its size with its mtime set back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			requiredFilterFails(t, d)
			hiddenRewrite(t, d, "f.fx", "BBBB\n")
		},
		todayFails:  "clean filter 'bad' failed",
		fails:       "clean filter 'bad' failed",
		failsNaming: []string{"f.fx"},
	},
	{
		// A required filter that works fails nothing: both reads store the edit as the filter
		// cleans it, in upper case.
		name: "a tracked file under a required clean filter that works, edited",
		build: func(t *testing.T, d *rescuetest.Disk) {
			requiredFilterWorks(t, d)
			d.Write(d.Workspace, "f.fx", "aaaa, edited\n")
		},
		holds: []string{"f.fx"},
	},
	{
		// The edit jj's stat rule passes over is read again through the filter by the carry
		// (rescue.converting), so the route keeps BBBB as the git read does, where it would
		// otherwise keep HEAD's aaaa.
		name: "a tracked file under a required clean filter that works, rewritten at its size with its mtime set back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			requiredFilterWorks(t, d)
			hiddenRewrite(t, d, "f.fx", "bbbb\n")
		},
		holds: []string{"f.fx"},
	},
	{
		// Measured with git 2.47.3 on 2026-11-13: `git add --all` over a copy of the index exited
		// 128 (`fatal: CRLF would be replaced by LF in c.txt`).
		name: "a tracked file of CRLF lines under an eol rule core.safecrlf refuses, as it was committed",
		build: func(t *testing.T, d *rescuetest.Disk) {
			safecrlfRefuses(t, d)
		},
		todayFails:  "CRLF would be replaced by LF in c.txt",
		fails:       "CRLF would be replaced by LF in c.txt",
		failsNaming: []string{"c.txt"},
	},
	{
		// The rewrite holds LF lines, which the eol rule keeps as they are, so git's add takes it;
		// the route reads it again through that add and keeps it as the git read does.
		name: "a tracked file of CRLF lines under an eol rule core.safecrlf refuses, rewritten at its size in LF lines with its mtime set back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			safecrlfRefuses(t, d)
			hiddenRewrite(t, d, "c.txt", "ABC\n")
		},
		holds: []string{"c.txt"},
	},
	{
		// Measured with git 2.47.3 on 2026-11-13: `git add --all` over a copy of the index exited
		// 128 (`fatal: BOM is required in 'w.u16' if encoded as UTF-16`).
		name: "a tracked file whose bytes are not in its working-tree-encoding, as it was committed",
		build: func(t *testing.T, d *rescuetest.Disk) {
			encodingRefuses(t, d)
		},
		todayFails:  "BOM is required in 'w.u16'",
		fails:       "BOM is required in 'w.u16'",
		failsNaming: []string{"w.u16"},
	},
	{
		// The rewrite is UTF-16 with its byte order mark, which git's add converts; the route
		// reads it again through that add and keeps it as the git read does.
		name: "a tracked file whose bytes are not in its working-tree-encoding, rewritten at its size in that encoding with its mtime set back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			encodingRefuses(t, d)
			hiddenRewrite(t, d, "w.u16", "\xff\xfeA\x00B\x00")
		},
		holds: []string{"w.u16"},
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
		holds: []string{"b"},
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
		// Both reads leave forced.log out: with HEAD unborn the git read starts from an empty
		// index and not from the worktree's (rescue.indexTree), and so does the route's copy of
		// it (rescue.snapshotTree), and `*.log` covers the file.
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
		holds:  []string{".gitignore"},
		absent: []string{"forced.log"},
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
		// Both reads drop x: `git add -u -- x` takes the record of a tracked file whose path
		// is a directory holding no repository out, and the directory holds nothing to add.
		name: "a tracked file that becomes an empty directory",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "")
		},
		holds:  []string{".gitignore"},
		absent: []string{"x"},
	},
	{
		// Both reads drop x, and `*.log` covers the one file the directory holds.
		name: "a tracked file that becomes a directory holding only an ignored file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "", "i.log")
		},
		holds:  []string{".gitignore"},
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
		// The index copy's entry under d/sub is what makes git walk into it, in the route's read
		// of the untracked files as in the git read.
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
		holds:  []string{"e"},
		absent: []string{"e/inner.txt", "e/loose.txt"},
	},
	{
		// Measured on this build: `git add --all` exits 128 (`'e/' does not have a commit checked
		// out`), so the git read refuses the replacement; the route's `update-index --add
		// --replace -- e` exits 128 (`e: is a directory - add files inside instead`), so it
		// refuses it too rather than drop e/work.txt.
		name: "an embedded git repository with no commit, holding a file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Run(d.Workspace, "git", "init", "--quiet", "--initial-branch=main", "e")
			d.Write(d.Workspace, "e/work.txt", "work in a repository with no commit\n")
		},
		todayFails:  "does not have a commit checked out",
		fails:       "is a directory - add files inside instead",
		failsNaming: []string{"e"},
	},
	{
		// The same over a fresh `jj git init --colocate` inside e, whose git HEAD is unborn: e
		// holds a .git and a .jj, so jj's snapshot leaves it out and git lists it as `e/`.
		name: "a fresh colocated jj repository in a subdirectory, its git HEAD unborn",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := filepath.Join(d.Workspace, "e")
			if err := os.MkdirAll(at, 0o755); err != nil {
				t.Fatal(err)
			}
			d.Run(at, "jj", "git", "init", "--colocate")
			d.Write(at, "work.txt", "work in a fresh colocated jj repository\n")
			if head := headOf(t, d, at); head != "" {
				t.Fatalf("e's git HEAD is %s, want it unborn, which this row stands on", head)
			}
		},
		todayFails:  "does not have a commit checked out",
		fails:       "is a directory - add files inside instead",
		failsNaming: []string{"e"},
	},
	{
		// The reviewer's shape of round 8: the index copy holds d/old.txt, so the git read walks
		// into d although d holds a .git, and keeps what it finds there; d/old.txt itself is gone.
		name: "a tracked file deleted under a directory that then becomes a plain git repository with a commit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/old.txt", "a tracked file the role deletes before d becomes a repository\n")
			pushedByTheRole(t, d, "the file under d")
			deleted(t, d, "d/old.txt")
			becomesAGitRepository(d, "d")
			d.Write(d.Workspace, "d/new.txt", "a file written after d became a repository\n")
		},
		holds:  []string{"d/loose.txt", "d/new.txt"},
		absent: []string{"d/old.txt"},
	},
	{
		name: "a file new in @ deleted under a directory that then becomes a colocated jj repository",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/a.txt", "a file a role's jj snapshotted before the role deleted it\n")
			intentToAdd(t, d, "d/a.txt")
			deleted(t, d, "d/a.txt")
			becomesAColocatedJJRepository(d, "d")
		},
		holds:  []string{"d/b.txt"},
		absent: []string{"d/a.txt"},
	},
	{
		name: "a tracked file rewritten at the same size with its mtime set back to the one jj recorded",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "f.txt", "AAAA")
			pushedByTheRole(t, d, "the file rewritten at the same size")
			rewrittenAtTheSameSize(t, d, "f.txt", "BBBB", true)
		},
		contentLost: map[string]string{
			"f.txt": "an mtime set back on purpose to the value jj recorded hides a same-size rewrite from jj's stat rule, so the route keeps HEAD's AAAA and loses the BBBB the git read carried; the role's own jj commit misses it the same way, and the git read carried it only because the stat-less index makes git hash every file",
		},
		holds: []string{"f.txt"},
	},
	{
		// The comparison proves both trees hold f.txt as BBBB.
		name: "a tracked file rewritten at the same size with its mtime left to move",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "f.txt", "AAAA")
			pushedByTheRole(t, d, "the file rewritten at the same size")
			rewrittenAtTheSameSize(t, d, "f.txt", "BBBB", false)
		},
		holds: []string{"f.txt"},
	},
	{
		// The deep review's shape of round 13: e.txt is committed, edited and snapshotted by a
		// role's `jj status`, which writes the colocated index with HEAD's blob for it, and then
		// rewritten at the same size with its mtime set back to the one jj recorded. jj's tree
		// keeps the edit jj read, which is not the copy's record, so the carry updates e.txt from
		// the worktree (rescue.seeds).
		name: "an edited file jj snapshotted, rewritten at the same size with its mtime set back",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "e.txt", "AAAA")
			pushedByTheRole(t, d, "the file edited and then rewritten at the same size")
			d.Write(d.Workspace, "e.txt", "BBBB")
			d.JJ("status")
			rewrittenAtTheSameSize(t, d, "e.txt", "CCCC", true)
		},
		holds: []string{"e.txt"},
	},
	{
		name: "a file new in @ under a plain directory",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "p/new.txt", "a file a role's jj snapshotted under a plain directory\n")
			intentToAdd(t, d, "p/new.txt")
		},
		holds: []string{"p/new.txt"},
	},
	{
		name: "a file new in @ that a later .gitignore rule covers",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "late.log", "a file a role's jj snapshotted before *.log was ignored\n")
			intentToAdd(t, d, "late.log")
			d.Write(d.Workspace, ".gitignore", "*.log\n")
		},
		holds: []string{"late.log", ".gitignore"},
	},
	{
		// The replay's shape: jj keeps tracking env.local, which it snapshotted into @, where
		// the git read starts from an index that `git reset` took the file out of.
		name: "a file new in @ that git reset takes out of the index and an ignore rule then covers",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "env.local", "a file a role's jj snapshotted before git reset\n")
			intentToAdd(t, d, "env.local")
			d.Git("reset", "--quiet")
			d.Write(d.Workspace, ".gitignore", "env.local\n")
		},
		gained: map[string]string{
			"env.local": trackedThenIgnored,
		},
		holds: []string{".gitignore"},
	},
	{
		name: "a skip-worktree entry whose file is gone",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitignore", "*.log\n")
			pushedByTheRole(t, d, "the ignore rules")
			d.Write(d.Workspace, "forced.log", "a file a role forced into the index and marked skip-worktree\n")
			d.Git("add", "-f", "forced.log")
			d.Git("update-index", "--skip-worktree", "forced.log")
			deleted(t, d, "forced.log")
		},
		holds: []string{"forced.log"},
	},
	{
		// Measured on this build: both trees record sub at the submodule's new HEAD, the route
		// by `update-index --add --replace -- sub` over jj's tree, which keeps the gitlink HEAD
		// holds.
		name: "a committed submodule whose own HEAD moved",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := embeddedRepository(d, "sub")
			d.Git("add", "sub")
			d.Git("commit", "--quiet", "-m", "a submodule committed with plain git")
			d.Run(at, "git", "add", "loose.txt")
			d.Run(at, "git", "commit", "--quiet", "-m", "a commit made inside the submodule")
		},
		holds: []string{"sub"},
	},
	{
		// The same move staged with `git add sub`: the index copy holds the submodule's new
		// HEAD, which the git read records, and jj's tree the gitlink HEAD holds.
		name: "a committed submodule whose own HEAD moved, the move staged with git add",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			d.Run(at, "git", "add", "loose.txt")
			d.Run(at, "git", "commit", "--quiet", "-m", "a commit made inside the submodule")
			d.Git("add", "sub")
		},
		holds: []string{"sub"},
	},
	{
		// The bump a role makes in a pod, where jj checks the submodule out as an empty
		// directory: `update-index --cacheinfo` stages another commit at sub. git resolves no
		// HEAD in sub, so the git read keeps the staged gitlink, where jj's tree and HEAD hold
		// the committed one.
		name: "a committed submodule bumped with update-index --cacheinfo over its empty directory",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			d.Run(at, "git", "add", "loose.txt")
			d.Run(at, "git", "commit", "--quiet", "-m", "the commit the bump stages")
			bump := d.Run(at, "git", "rev-parse", "HEAD")
			removeAll(t, at)
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
			d.Git("update-index", "--cacheinfo", gitlinkMode+","+bump+",sub")
		},
		holds: []string{"sub"},
	},
	{
		// The same bump staged with `git add sub` over the submodule's moved HEAD, whose .git is
		// removed after: the git read keeps the staged gitlink, and none of the files left in sub.
		name: "a committed submodule bumped with git add, whose directory has lost its .git",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			d.Run(at, "git", "add", "loose.txt")
			d.Run(at, "git", "commit", "--quiet", "-m", "the commit the bump stages")
			d.Git("add", "sub")
			removeAll(t, filepath.Join(at, ".git"))
		},
		holds:  []string{"sub"},
		absent: []string{"sub/inner.txt", "sub/loose.txt"},
	},
	{
		// A committed submodule taken out of the index and replaced there by a file under its
		// path: the git read records sub/new.txt and no gitlink, where jj's tree holds the
		// gitlink HEAD holds.
		name: "a committed submodule emptied, taken out of the index, and a file added under its path",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			removeAll(t, at)
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
			d.Git("rm", "--quiet", "--cached", "sub")
			d.Write(d.Workspace, "sub/new.txt", "a file added where the submodule was\n")
			d.Git("add", "--", "sub/new.txt")
		},
		holds:  []string{"sub/new.txt"},
		absent: []string{"sub"},
	},
	{
		// Only the index copy holds the gitlink, which `git add sub` staged over an untracked
		// embedded repository, and sub has since lost its .git: the git read keeps the copy's
		// gitlink, since git resolves no HEAD in sub, and adds none of the files left there.
		name: "a gitlink only the index holds, staged with git add, whose directory has lost its .git",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := embeddedRepository(d, "sub")
			d.Git("add", "sub")
			removeAll(t, filepath.Join(at, ".git"))
		},
		holds:  []string{"sub"},
		absent: []string{"sub/inner.txt", "sub/loose.txt"},
	},
	{
		// The same with a jj command of the role's after the strip, whose snapshot takes
		// sub/inner.txt and writes it into the colocated index beside the gitlink. Measured on
		// this build: the git read's `git add --all` exits 0 keeping both, and its write-tree
		// exits 128 (`You have both sub and sub/inner.txt`), so it refuses the replacement. The
		// route's seed of the gitlink would replace sub/inner.txt and answer a tree, so the
		// route refuses the copy that holds both (rescue.keptUnderAKeptPath).
		name: "a gitlink only the index holds, staged with git add, whose directory has lost its .git before a jj command",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := embeddedRepository(d, "sub")
			d.Git("add", "sub")
			removeAll(t, filepath.Join(at, ".git"))
			d.JJ("status")
		},
		todayFails:  "You have both sub and sub/inner.txt",
		fails:       "holds both \"sub\" and \"sub/inner.txt\" under it",
		failsNaming: []string{"sub", "sub/inner.txt"},
	},
	{
		// The same over an uncommitted `git submodule add`, whose sub/.git is a file.
		name: "a gitlink only the index holds, from an uncommitted git submodule add, whose directory has lost its .git",
		build: func(t *testing.T, d *rescuetest.Disk) {
			upstream := filepath.Join(filepath.Dir(d.Remote), "upstream")
			d.Run(filepath.Dir(upstream), "git", "init", "--quiet", "--initial-branch=main", upstream)
			d.Write(upstream, "u.txt", "a file of the submodule's upstream\n")
			d.Run(upstream, "git", "add", "u.txt")
			d.Run(upstream, "git", "commit", "--quiet", "-m", "the submodule's upstream")
			d.Git("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", upstream, "sub")
			removeAll(t, filepath.Join(d.Workspace, "sub", ".git"))
		},
		holds:  []string{"sub", ".gitmodules"},
		absent: []string{"sub/u.txt"},
	},
	{
		// The reviewer's reproduction of round 10: HEAD names a commit and the worktree has no
		// index file, so the git read starts from `read-tree <head>`, whose entry under d makes
		// it walk into d although d holds a .git. jj's snapshot leaves d out whole.
		name: "a born git HEAD with no index file, over a tracked file under a directory that becomes a git repository",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/x.txt", "a tracked file under what becomes a repository\n")
			pushedByTheRole(t, d, "the file under d")
			becomesAGitRepository(d, "d")
			d.Write(d.Workspace, "d/x.txt", "edited under the directory's own repository\n")
			withoutIndexFile(t, d)
		},
		holds: []string{"d/x.txt", "d/loose.txt"},
	},
	{
		name: "a committed submodule whose directory becomes a regular file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			removeAll(t, at)
			d.Write(d.Workspace, "sub", "a regular file where the submodule was\n")
		},
		holds: []string{"sub"},
	},
	{
		name: "a committed submodule whose directory becomes a symlink",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "sub")
			removeAll(t, at)
			symlink(t, "base.txt", at)
		},
		holds: []string{"sub"},
	},
	{
		// The git read drops the gitlink of a submodule whose path is gone, as it drops a
		// deleted file; jj's tree keeps it.
		name: "a committed submodule whose directory is gone",
		build: func(t *testing.T, d *rescuetest.Disk) {
			removeAll(t, committedSubmodule(d, "sub"))
		},
		absent: []string{"sub"},
	},
	{
		// The git read reads no file of an assume-unchanged entry, so it keeps the entry's
		// recorded blob whatever the worktree holds; jj's snapshot reads the file.
		name: "an assume-unchanged entry whose file is gone",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "au.txt", "a committed file marked assume-unchanged\n")
			pushedByTheRole(t, d, "the file marked assume-unchanged")
			d.Git("update-index", "--assume-unchanged", "au.txt")
			deleted(t, d, "au.txt")
		},
		holds: []string{"au.txt"},
	},
	{
		// The comparison proves both trees hold au.txt at its committed blob, not the edit.
		name: "an assume-unchanged entry whose file is edited",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "au.txt", "a committed file marked assume-unchanged\n")
			pushedByTheRole(t, d, "the file marked assume-unchanged")
			d.Git("update-index", "--assume-unchanged", "au.txt")
			d.Write(d.Workspace, "au.txt", "edited after it was marked assume-unchanged\n")
		},
		holds: []string{"au.txt"},
	},
	{
		// The comparison proves both trees hold sw.txt at its committed blob, not the edit.
		name: "a skip-worktree entry whose file is edited",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "sw.txt", "a committed file marked skip-worktree\n")
			pushedByTheRole(t, d, "the file marked skip-worktree")
			d.Git("update-index", "--skip-worktree", "sw.txt")
			d.Write(d.Workspace, "sw.txt", "edited after it was marked skip-worktree\n")
		},
		holds: []string{"sw.txt"},
	},
	{
		name: "an assume-unchanged file that becomes a directory holding a file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "--assume-unchanged", "k.txt")
		},
		holds:  []string{"x/k.txt"},
		absent: []string{"x"},
	},
	{
		name: "a skip-worktree file that becomes a directory holding a file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "--skip-worktree", "k.txt")
		},
		holds:  []string{"x/k.txt"},
		absent: []string{"x"},
	},
	{
		// The git read keeps x at its record: the directory holds nothing for it to add.
		name: "an assume-unchanged file that becomes an empty directory",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "--assume-unchanged")
		},
		holds: []string{"x"},
	},
	{
		// The git read keeps x at its record: `*.log` covers the one file the directory holds.
		name: "a skip-worktree file that becomes a directory holding only an ignored file",
		build: func(t *testing.T, d *rescuetest.Disk) {
			flaggedThenADirectory(t, d, "--skip-worktree", "i.log")
		},
		holds:  []string{"x"},
		absent: []string{"x/i.log"},
	},
	{
		// Both reads record d as the symlink it is and d/p.txt as deleted: git reads no file
		// beyond a symbolic link.
		name: "a tracked file under a directory that becomes a symlink to a directory holding a file of that name",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "d/p.txt", "a tracked file under what becomes a symlink\n")
			pushedByTheRole(t, d, "the file under d")
			target := filepath.Join(filepath.Dir(d.Remote), "target")
			d.Write(target, "p.txt", "the file of that name in the directory the symlink names\n")
			removeAll(t, filepath.Join(d.Workspace, "d"))
			symlink(t, target, filepath.Join(d.Workspace, "d"))
		},
		holds:  []string{"d"},
		absent: []string{"d/p.txt"},
	},
	{
		// Measured on this build: both reads record p as the symlink it is and nothing at
		// p/sub, the gitlink the index copy and HEAD hold; jj's tree holds no p/sub either. So
		// what guards this row is the seed's test of the copy's p/sub (rescue.inWorktree):
		// os.Lstat of p/sub follows the link and finds a directory holding a repository, which
		// git, reading nothing beyond a symbolic link, never reads, and a seed of it fails `git
		// add -f` (`pathspec 'p/sub' is beyond a symbolic link`).
		name: "a committed submodule under a directory that becomes a symlink to a directory holding it",
		build: func(t *testing.T, d *rescuetest.Disk) {
			at := committedSubmodule(d, "p/sub")
			target := filepath.Join(filepath.Dir(d.Remote), "target")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(at, filepath.Join(target, "sub")); err != nil {
				t.Fatal(err)
			}
			removeAll(t, filepath.Join(d.Workspace, "p"))
			symlink(t, target, filepath.Join(d.Workspace, "p"))
		},
		holds:  []string{"p"},
		absent: []string{"p/sub"},
	},
	{
		// Today's `git add --all` refuses GIT~1 and `.git. ` (`invalid path`, exit 128).
		// jj's snapshot leaves both out for their size, and `update-index --add` ignores each
		// with exit 0 (`Ignoring path GIT~1`), so the route's read-back refuses them too: both
		// reads refuse the replacement.
		name: "new files under names git refuses, above the workspace's own limit",
		build: func(t *testing.T, d *rescuetest.Disk) {
			limitTo(d, oneKiB)
			d.Write(d.Workspace, "GIT~1", sized(4096, "g"))
			d.Write(d.Workspace, ".git. ", sized(4096, "t"))
		},
		todayFails:  "invalid path",
		fails:       "left out paths that git could not carry",
		failsNaming: []string{"GIT~1", ".git. "},
	},
	{
		// Measured on this build: the git read keeps y.txt at 100644 blob 975fbec8 ("y\n", which
		// is HEAD's), and so does the route, where jj's tree holds it at 100644 blob 5758d80d
		// ("y\r\n"): the copy holds HEAD's blob, which is not jj's entry, so the carry updates
		// y.txt with `git add -f`, which converts it as the git read does (rescue.seeds).
		name: "a tracked file rewritten with CRLF lines under a .gitattributes eol=lf rule",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitattributes", "*.txt text eol=lf\n")
			d.Write(d.Workspace, "y.txt", "y\n")
			pushedByTheRole(t, d, "the eol rule and the file it covers")
			d.Write(d.Workspace, "y.txt", "y\r\n")
		},
		holds: []string{"y.txt", ".gitattributes"},
	},
	{
		// The same file committed with CRLF lines before the eol rule is written: the copy holds
		// the CRLF blob HEAD holds, which is jj's entry too, so the carry does not read y.txt,
		// where the git read hashes it again and converts it.
		name: "a file committed with CRLF lines before a .gitattributes eol=lf rule",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "y.txt", "y\r\n")
			pushedByTheRole(t, d, "the file with CRLF lines")
			d.Write(d.Workspace, ".gitattributes", "*.txt text eol=lf\n")
		},
		reformed: map[string]string{
			"y.txt": "the eol rule has git store the CRLF lines as LF when it hashes the file again, and the route keeps the CRLF bytes HEAD and the copy hold, which jj's tree holds too: the same lines in another form",
		},
		holds: []string{"y.txt", ".gitattributes"},
	},
	{
		// Measured on this build: the git read keeps c.txt at 100644 blob f2ad6c76 ("c\n", which
		// is HEAD's), and so does the route, where jj's tree holds it at 100644 blob 764d69f6
		// ("c\r\n"), as for the eol rule.
		name: "a tracked file rewritten with CRLF lines under core.autocrlf=true",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "c.txt", "c\n")
			pushedByTheRole(t, d, "the file core.autocrlf converts")
			d.Git("config", "core.autocrlf", "true")
			d.Write(d.Workspace, "c.txt", "c\r\n")
		},
		holds: []string{"c.txt"},
	},
	{
		// Measured on this build: the git read keeps a.fx at 100644 blob 03fbba43 ("A
		// REWRITTEN\n", the filter's output), and so does the route, where jj's tree holds it at
		// 100644 blob d3055428 ("a rewritten\n"); HEAD holds 37beafe0 ("a committed\n"). b.fx is
		// new and untracked: jj's tree holds it at 100644 blob b32a5fba ("b new and untracked\n",
		// its own bytes), and the carry's `update-index --add`, which runs the clean filter,
		// records it at 100644 blob 7fe8dabd ("B NEW AND UNTRACKED\n"), the git read's entry, so
		// both trees hold b.fx at the same entry.
		name: "a tracked file rewritten and a new untracked file under a clean filter",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, ".gitattributes", "*.fx filter=up\n")
			d.Write(d.Workspace, "a.fx", "a committed\n")
			pushedByTheRole(t, d, "the filter rule and the file it covers")
			d.Git("config", "filter.up.clean", "tr a-z A-Z")
			d.Git("config", "filter.up.smudge", "cat")
			d.Write(d.Workspace, "a.fx", "a rewritten\n")
			d.Write(d.Workspace, "b.fx", "b new and untracked\n")
		},
		holds: []string{"a.fx", "b.fx", ".gitattributes"},
	},
	{
		// Measured on this build: the git read keeps x.sh at 100644 blob 67b376aa, the mode the
		// index records, and so does the route, where jj's tree holds it at 100755 blob
		// 67b376aa: the carry seeds the copy's record and `git add -f` keeps its mode.
		name: "a tracked file whose executable bit was set under core.fileMode=false",
		build: func(t *testing.T, d *rescuetest.Disk) {
			d.Write(d.Workspace, "x.sh", "echo x\n")
			pushedByTheRole(t, d, "the file whose executable bit is set")
			d.Git("config", "core.fileMode", "false")
			if err := os.Chmod(filepath.Join(d.Workspace, "x.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		holds: []string{"x.sh"},
	},
	{
		name:  "every shape that answers a tree at once, the tester's adversarial worktree",
		build: everyShapeAtOnce,
		holds: []string{
			"forced.log", "big: 1B (1 bytes); the maximum size allowed is 1B\n  z", "refused-01\n  spill.bin",
			"jd/a.txt", "jd/b.txt", "m/a.txt", "m/loose.txt", "x/k.txt",
			"ga/loose.txt", "ga/new.txt", "jb/b.txt", "rw.txt", "pd/new.txt", "late.log",
			"au.txt", "sw.txt", "skip.txt", "sf", "ss", "sl", "b", "eg", "nested",
			"sm", "sb", "sc", "sd", "sr/new.txt", "p", "na", "su", ".gitmodules", "ax/k.txt", "sx/k.txt", "ae", "si",
		},
		absent: []string{
			"x", "x/i.log", "gone.txt", "eg/inner.txt", "eg/loose.txt", "ga/old.txt", "jb/a.txt", "sg", "sl/p.txt",
			"p/sub", "na/inner.txt", "na/loose.txt", "su/u.txt", "ax", "sx", "si/i.log",
			"sd/inner.txt", "sd/loose.txt", "sr",
		},
	},
}

// everyShapeAtOnce is one worktree holding every shape of the rows above whose route answers a
// tree, whose git HEAD names a commit, whose worktree has an index file and that declare no path
// the two trees may differ at. The rows that declare one are left out, each standing alone so
// that its exception covers its own shape and no other: the file git reset took out of the
// index, which is gained, the mtime set back over HEAD's content, which is contentLost, and the
// CRLF file committed before the eol rule, which is reformed; so are the four whose git converts
// a file, whose attributes and configuration would convert the files of every other shape, the
// unborn HEAD's row, whose HEAD cannot be both, and the row with no index file, which could not
// hold the index entries the other shapes stand on. So is the mtime set back over an edit jj
// snapshotted: measured on this build on 2026-11-13, such a file here, with the carry of the
// copy's records jj's tree holds at another entry taken out of rescue.seeds, read equal in both
// trees, which the row standing alone does not, so here it would guard nothing. The jj commands
// come first, the snapshot that puts jd/a.txt, m/a.txt, jb/a.txt, pd/new.txt and late.log in the
// colocated index among them, then the plain git commits that move the git HEAD behind jj's back,
// the submodules sf, ss, sg, sm, sb, sc, sd, sr and p/sub among them, then the files and the
// directories that change, and last what a role writes into the index: the `git add -f`, the
// flags, the staged bumps of sb, sc and sd, sr taken out of the index with sr/new.txt added under
// it, and the gitlinks na and su only the index holds. Any jj command of the workspace after them
// would rewrite that index. The rewrite of rw.txt at its size leaves its mtime to move, so this
// row declares no path for it.
func everyShapeAtOnce(t *testing.T, d *rescuetest.Disk) {
	limitTo(d, oneMiB)
	d.Write(d.Workspace, "aa", "the tracked file the refused name is magic for\n")
	d.Write(d.Workspace, "d/x.log", "committed before *.log was ignored\n")
	d.Write(d.Workspace, "dir/kept.txt", "a file of the directory the patterns name\n")
	d.Write(d.Workspace, "x", "a tracked file that becomes a directory\n")
	d.Write(d.Workspace, "gone.txt", "a tracked file the role deletes\n")
	d.Write(d.Workspace, "ga/old.txt", "a tracked file the role deletes before ga becomes a repository\n")
	d.Write(d.Workspace, "rw.txt", "AAAA")
	d.Write(d.Workspace, "au.txt", "a committed file marked assume-unchanged\n")
	d.Write(d.Workspace, "sw.txt", "a committed file marked skip-worktree\n")
	d.Write(d.Workspace, "skip.txt", "a committed file marked skip-worktree and deleted\n")
	d.Write(d.Workspace, "sl/p.txt", "a tracked file under what becomes a symlink\n")
	for _, file := range []string{"ax", "sx", "ae", "si"} {
		d.Write(d.Workspace, file, "a committed file that is marked and then becomes a directory\n")
	}
	d.JJ("sparse", "set", "--add", "dir")
	pushedByTheRole(t, d, "the tracked files the shapes stand on")
	d.Write(d.Workspace, "jd/a.txt", "a file a role's jj snapshotted before jd became a repository\n")
	d.Write(d.Workspace, "m/a.txt", "a file a role's jj snapshotted before m became a repository\n")
	d.Write(d.Workspace, "jb/a.txt", "a file a role's jj snapshotted before the role deleted it\n")
	d.Write(d.Workspace, "pd/new.txt", "a file a role's jj snapshotted under a plain directory\n")
	d.Write(d.Workspace, "late.log", "a file a role's jj snapshotted before *.log was ignored\n")
	intentToAdd(t, d, "jd/a.txt", "m/a.txt", "jb/a.txt", "pd/new.txt", "late.log")
	commitWithGit(d, "fix.txt", "a fix made with plain git\n")
	for _, sub := range []string{"sf", "ss", "sg"} {
		removeAll(t, committedSubmodule(d, sub))
	}
	moved := committedSubmodule(d, "sm")
	bumped := committedSubmodule(d, "sb")
	cached := committedSubmodule(d, "sc")
	unlinked := committedSubmodule(d, "sd")
	unsubmoduled := committedSubmodule(d, "sr")
	beyond := committedSubmodule(d, "p/sub")
	for _, at := range []string{moved, bumped, cached, unlinked} {
		d.Run(at, "git", "add", "loose.txt")
		d.Run(at, "git", "commit", "--quiet", "-m", "a commit made inside the submodule")
	}
	bump := d.Run(cached, "git", "rev-parse", "HEAD")
	for _, at := range []string{cached, unsubmoduled} {
		removeAll(t, at)
		if err := os.Mkdir(at, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	targetP := filepath.Join(filepath.Dir(d.Remote), "target-p")
	if err := os.MkdirAll(targetP, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(beyond, filepath.Join(targetP, "sub")); err != nil {
		t.Fatal(err)
	}
	removeAll(t, filepath.Join(d.Workspace, "p"))
	symlink(t, targetP, filepath.Join(d.Workspace, "p"))
	d.Write(d.Workspace, "sf", "a regular file where the submodule was\n")
	symlink(t, "base.txt", filepath.Join(d.Workspace, "ss"))
	target := filepath.Join(filepath.Dir(d.Remote), "target")
	d.Write(target, "p.txt", "the file of that name in the directory the symlink names\n")
	removeAll(t, filepath.Join(d.Workspace, "sl"))
	symlink(t, target, filepath.Join(d.Workspace, "sl"))
	rewrittenAtTheSameSize(t, d, "rw.txt", "BBBB", false)

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
	deleted(t, d, "ga/old.txt")
	becomesAGitRepository(d, "ga")
	d.Write(d.Workspace, "ga/new.txt", "a file written after ga became a repository\n")
	deleted(t, d, "jb/a.txt")
	becomesAColocatedJJRepository(d, "jb")

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
	d.Git("update-index", "--assume-unchanged", "au.txt")
	deleted(t, d, "au.txt")
	d.Git("update-index", "--skip-worktree", "sw.txt", "skip.txt")
	d.Write(d.Workspace, "sw.txt", "edited after it was marked skip-worktree\n")
	deleted(t, d, "skip.txt")
	d.Git("add", "sb")
	d.Git("update-index", "--cacheinfo", gitlinkMode+","+bump+",sc")
	d.Git("add", "sd")
	removeAll(t, filepath.Join(unlinked, ".git"))
	d.Git("rm", "--quiet", "--cached", "sr")
	d.Write(d.Workspace, "sr/new.txt", "a file added where the submodule was\n")
	d.Git("add", "--", "sr/new.txt")
	stripped := embeddedRepository(d, "na")
	d.Git("add", "na")
	removeAll(t, filepath.Join(stripped, ".git"))
	upstream := filepath.Join(filepath.Dir(d.Remote), "upstream")
	d.Run(filepath.Dir(upstream), "git", "init", "--quiet", "--initial-branch=main", upstream)
	d.Write(upstream, "u.txt", "a file of the submodule's upstream\n")
	d.Run(upstream, "git", "add", "u.txt")
	d.Run(upstream, "git", "commit", "--quiet", "-m", "the submodule's upstream")
	d.Git("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", upstream, "su")
	removeAll(t, filepath.Join(d.Workspace, "su", ".git"))
	d.Git("update-index", "--assume-unchanged", "ax", "ae")
	d.Git("update-index", "--skip-worktree", "sx", "si")
	for file, inside := range map[string]string{"ax": "k.txt", "sx": "k.txt", "ae": "", "si": "i.log"} {
		deleted(t, d, file)
		if err := os.Mkdir(filepath.Join(d.Workspace, file), 0o755); err != nil {
			t.Fatal(err)
		}
		if inside != "" {
			d.Write(d.Workspace, file+"/"+inside, "a file of the directory that replaced a marked file\n")
		}
	}
}

// heldAndAbsent fails the test where a tree of the row lacks a path the row says both hold, or
// holds a path the row says neither may.
func heldAndAbsent(t *testing.T, d *rescuetest.Disk, row differentialRow, trees map[string]string) {
	t.Helper()
	entries := make(map[string]map[string]entry, len(trees))
	for name, tree := range trees {
		entries[name] = entriesOf(t, d, tree)
	}
	entriesHeldAndAbsent(t, row, entries)
}

// entriesHeldAndAbsent is heldAndAbsent over the entries of each tree, by the tree's name.
func entriesHeldAndAbsent(t *testing.T, row differentialRow, trees map[string]map[string]entry) {
	t.Helper()
	for name, entries := range trees {
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
// git read keeps, the jj route keeps at the same path, mode and blob, and the jj route keeps no
// path the git read lacks, but for the exceptions heldAsRuled names, each checked for exactly the
// paths a row names under its kind; every path a row declares, under any kind, carries its
// reason; where the route fails, the row says so; and where the git read fails, the route fails
// too, naming the paths the row stands on, or the row fails as a loss.
func TestTheJJRouteKeepsEveryEntryTheWholeWorktreeGitReadKeeps(t *testing.T) {
	for _, row := range differentialRows {
		for _, path := range unreasoned(row) {
			t.Errorf("the row %q declares %s with no reason, want the reason the two trees may differ there", row.name, path)
		}
		if row.todayFails != "" && len(row.failsNaming) == 0 {
			t.Errorf("the row %q sets todayFails and names no path the jj route's failure must name (failsNaming), so a route that fails for another reason would pass as agreeing", row.name)
		}
	}
	for _, row := range differentialRows {
		t.Run(row.name, func(t *testing.T) {
			if row.todayFails != "" && row.fails == "" {
				t.Fatalf("loss: the row %q declares today's read failing the workspace saying %q and the jj route answering a tree, where `git add --all` fails and the route must fail the workspace too, so want fails set beside todayFails", row.name, row.todayFails)
			}
			d := rescuetest.New(t, issue)
			row.build(t, d)
			r := readBoth(t, d, headOf(t, d, d.Workspace))

			if lost := failureLost(r); lost != "" {
				t.Fatalf("the row %q: %s", row.name, lost)
			}
			switch {
			case r.todayErr != nil && row.todayFails == "":
				t.Fatalf("the row %q: today's read fails the workspace (%v), and the row sets no todayFails; the jj route %s", row.name, r.todayErr, answered(r.route, r.routeErr))
			case r.todayErr != nil && !strings.Contains(r.todayErr.Error(), row.todayFails):
				t.Fatalf("the row %q: today's read fails the workspace (%v), want it to fail saying %q", row.name, r.todayErr, row.todayFails)
			case r.todayErr == nil && row.todayFails != "":
				t.Fatalf("the row %q: today's read answers the tree %s, want it to fail the workspace saying %q", row.name, r.today, row.todayFails)
			}
			if row.fails != "" {
				if r.routeErr == nil {
					t.Fatalf("the row %q: the jj route answers the tree %s, want it to fail saying %q", row.name, r.route, row.fails)
				}
				if !strings.Contains(r.routeErr.Error(), row.fails) {
					t.Fatalf("the row %q: the jj route fails saying %q, want %q", row.name, r.routeErr, row.fails)
				}
				for _, path := range row.failsNaming {
					if !namesPath(r.routeErr, path) {
						t.Errorf("the row %q: both reads fail the workspace, and the jj route's failure names neither %q nor a path under it, so it may fail for another reason than today's read does. Today's read: %v. The jj route: %v", row.name, path, r.todayErr, r.routeErr)
					}
				}
				if row.todayFails == "" {
					heldAndAbsent(t, d, row, map[string]string{"git read's": r.today})
				}
				return
			}
			if r.routeErr != nil {
				t.Fatalf("the row %q: the jj route fails (%v), want a tree; today's read %s", row.name, r.routeErr, answered(r.today, r.todayErr))
			}
			heldAsRuled(t, d, r.today, r.route, row)
			heldAndAbsent(t, d, row, map[string]string{"git read's": r.today, "jj route's": r.route})
		})
	}
}

// The walk for a tracked file this process cannot read (rescue.unreadable) runs under the read's
// own cut, git's stat comparison of what it found among it: a walk the workspace's bound ends is
// cut as the rest of the read is, so it eats nothing of the push's reserve, and the commits the
// check already found are pushed. The cut falls inside the walk every run: the observer every git
// of the check passes through holds the walk at its git question, `diff-files`, until the read's
// bound has run out, and the question then runs under the cut.
func TestAReadCutInsideTheWalkForUnreadableFilesStillRescuesTheCommitsFound(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "u.txt", "AAAA\n")
	pushedByTheRole(t, d, "a file that is then made unreadable")
	d.Write(d.Workspace, "feature.txt", "work the remote lacks\n")
	d.JJ("commit", "-m", "work the remote lacks")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	requireIndexWithoutStatData(t, d)
	unreadableFile(t, d, "u.txt")

	// The read's own bound is what the workspace's leaves past the push's reserve, 10 s here,
	// as in the cut cases of rescue_test.go; the walk is held a second past it.
	bound := rescue.CheckRun + rescue.GuardReserve + 10*time.Second
	started := time.Now()
	held := false
	rescue.ObserveCommands(t, func(dir, name string, args []string) {
		if name == "git" && slices.Contains(args, "diff-files") && !held {
			held = true
			time.Sleep(time.Until(started.Add(11 * time.Second)))
		}
	})
	checked := rescue.Run(context.Background(), rescue.Request{
		StateDir: d.StateDir, Branch: prefix, Env: d.Env,
		WorkspaceTimeout: bound, Deadline: time.Now().Add(bound + time.Minute),
	})
	took := time.Since(started)
	if !held {
		t.Fatal("the check ran no `git diff-files`, so the walk never reached its git question and this case cut nothing inside it")
	}
	w := only(t, d, checked)
	if w.Status != rescue.Failed || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want the one commit it found rescued and the workspace failed", w)
	}
	if b := w.Branches[0]; b.Name != prefix+"-1" || b.Commit != commit || d.RemoteBranch(b.Name) != commit {
		t.Errorf("the check pushed %s at %s, want %s-1 at the commit %s the workspace holds", b.Name, b.Commit, prefix, commit)
	}
	if !strings.Contains(w.Error, "could not be read in the time the workspace's bound left for them") || !strings.Contains(w.Error, d.Workspace) || !strings.Contains(w.Error, "diff-files") {
		t.Errorf("the entry failed with %q, want it to name the read of %s as the step that ran out, cut at the walk's `diff-files`", w.Error, d.Workspace)
	}
	if !strings.Contains(w.Error, "on the remote") {
		t.Errorf("the entry failed with %q, want it to say that what the check did find reached the remote", w.Error)
	}
	if ran := failedAt(t, w, rescuetest.FileStep); ran <= 0 || ran > took {
		t.Errorf("the entry says the read ran %s, want a time inside the %s the whole check took: %q", ran, took, w.Error)
	}
	if took > 30*time.Second {
		t.Errorf("the check took %s of its %s bound, want the read stopped with the push's reserve in hand", took, bound)
	}
}
