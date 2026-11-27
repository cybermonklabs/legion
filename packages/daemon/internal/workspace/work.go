package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The workspace read: whether the workspace every role of an issue shares holds work that no
// branch or tag on origin holds. A role's shim makes it in the workspace it works in, and the
// issue pod's launcher makes it where no role process runs (R6 of #5285). A pod
// update round that replaces the pod without waiting for every role reads it to name what goes
// with the pod, and the idle release, the release of a pod no claim owns and the close of a left
// pod read it to take the pod only when it holds no work. Both readers run ReadWork, so they count
// the same work.

// WorkRevset is the work the read counts: every commit under a workspace's working copy or a
// local bookmark, less three kinds of commit that are not work.
//
// A commit origin holds is not work: one under a remote bookmark or a tag is already pushed, and
// the replacement pod's fetch gets it back. Tags are subtracted beside remote bookmarks because a
// clone of this repository holds thousands of them, jj makes each tagged commit a visible head,
// and the release commits they point at are reachable from no remote bookmark: with remote
// bookmarks alone subtracted, every such commit under a working copy or a bookmark would count as
// work that origin already holds.
//
// A commit with no change and no description is not work: it carries nothing a replacement can
// lose, whether it is a working copy or the parent a bare `jj new` leaves. Every issue pod's clone
// keeps such a working copy, and jj 0.45.1 keeps the old empty working copy as the new one's
// parent after a bare `jj new` (measured). A described empty commit is work: a role wrote its
// description.
//
// A commit a fetch left visible, and a side head that no working copy and no bookmark reaches,
// are not counted, because Legion's clones keep unreachable commits
// (git.abandon-unreachable-commits = false, ensureFetchConfiguration): after any role's
// `jj git fetch`, the commits of every branch origin deleted or rewrote stay visible with no
// bookmark, hundreds of them other issues' work. Counting every visible head would name those as
// this issue's. The cost is a side head of this issue's own (a role ran `jj new` over a rewritten
// parent, bookmark.go), which goes uncounted: a round does not name it, and a release does not
// keep the pod for it.
//
// This read is the one the pod update round and every release make, so the work a round names as
// going with a pod is the work that keeps a release from taking it.
const WorkRevset = `(::(working_copies() | bookmarks()) ~ ::(remote_bookmarks() | tags())) ~ (empty() & description(exact:""))`

// ReadWork runs the read's jj commands with jj, through run, which runs one command in the
// directory it is given and answers its trimmed standard output (RunCommand under the caller's
// budget and environment, which for the pod's launcher holds no configuration of anyone's). dir is
// the issue's own workspace, and global are jj options the caller puts before each command's own.
//
// The read first snapshots the working copy of each task of the issue (taskCopiesOf), each in that
// working copy itself: a task's agent works there, and the list below sees each working copy as
// its last snapshot left it, so without this an edit the agent left with no jj command after it
// would read as no work, and a release would delete it with the disk. A working copy of which only
// the aside of a cut-short Remove is left is being removed, and is not read. Then the read runs
// two commands in dir: the first reads the working copy's commit and whether it is empty, and is
// the snapshot of the issue's own working copy; the second is --ignore-working-copy, so both
// describe one state of the workspace. It answers the working copy's commit, whether that commit
// holds a change, and every work commit (WorkRevset) of every working copy, newest first. A dir
// outside Location's layout is refused before any jj runs, since the read could not name the
// working copies of its tasks.
//
// The snapshots are what the read writes, as every jj command that reads a working copy does: jj
// records an operation and, where files of the working copy changed, a new working-copy commit and
// the git ref jj keeps for it (refs/jj/keep). The read changes no file of any working copy, moves
// no bookmark and pushes nothing, and it needs no git.
func ReadWork(run func(dir string, args ...string) (string, error), jj, dir string, global ...string) (head string, changed bool, work []string, err error) {
	command := func(args ...string) []string {
		return append(append([]string{jj}, global...), args...)
	}
	copies, err := taskCopiesOf(dir)
	if err != nil {
		return "", false, nil, err
	}
	for _, task := range copies {
		if _, err := os.Lstat(task.Dir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return "", false, nil, fmt.Errorf("the working copy of %s: %w", task.Bookmark, err)
		}
		if _, err := run(task.Dir, command("log", "-R", task.Dir, "-r", "@", "--no-graph", "-T", "commit_id")...); err != nil {
			return "", false, nil, err
		}
	}
	at, err := run(dir, command("log", "-R", dir, "-r", "@", "--no-graph", "-T", `commit_id ++ " " ++ empty`)...)
	if err != nil {
		return "", false, nil, err
	}
	head, empty, _ := strings.Cut(at, " ")
	if head == "" || (empty != "true" && empty != "false") {
		return "", false, nil, fmt.Errorf("jj answered %q for the working copy, not its commit and whether it is empty", at)
	}
	listed, err := run(dir, command("log", "-R", dir, "--ignore-working-copy", "-r", WorkRevset, "--no-graph", "-T", `commit_id ++ "\n"`)...)
	if err != nil {
		return "", false, nil, err
	}
	return head, empty == "false", OutputLines(listed), nil
}

// taskCopiesOf is the working copy of each task beside dir, the issue's own working copy
// (TaskCopies), read from dir's place in Location's layout (layoutWorkspace). The layout writes
// the issue's key in lower case, and an issue key is upper case (claim.IsIssueKey), so the issue's
// own branch names the directory's name in upper case. A dir outside the layout is refused.
func taskCopiesOf(dir string) ([]Workspace, error) {
	issue, err := layoutWorkspace(dir)
	if err != nil {
		return nil, fmt.Errorf("the working copies of the tasks beside %s: %w", dir, err)
	}
	issue.Bookmark = Bookmark(strings.ToUpper(filepath.Base(issue.Dir)))
	return TaskCopies(issue)
}
