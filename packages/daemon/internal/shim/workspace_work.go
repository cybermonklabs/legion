package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The workspace read: the shim of one role answers whether the workspace every role of its issue
// shares holds work that no branch or tag on origin holds. A pod update round that replaces the
// pod without waiting for every role reads it to name what goes with the pod, and the idle release
// reads it to take the pod only when it holds no work. Only a shim can read it: it runs the jj of
// the workspace it already works in.

// workspaceWorkRevset is the work the read counts: every commit under a workspace's working copy
// or a local bookmark, less three kinds of commit that are not work.
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
// (git.abandon-unreachable-commits = false, workspace's ensureFetchConfiguration): after any
// role's `jj git fetch`, the commits of every branch origin deleted or rewrote stay visible with
// no bookmark, hundreds of them other issues' work. Counting every visible head would name those
// as this issue's. The cost is a side head of this issue's own (a role ran `jj new` over a
// rewritten parent, workspace's bookmark.go), which goes uncounted: a round does not name it, and
// a release does not keep the pod for it.
//
// This read is the one both the pod update round and the idle release make, so the work a round
// names as going with a pod is the work that keeps a release from taking it.
const workspaceWorkRevset = `(::(working_copies() | bookmarks()) ~ ::(remote_bookmarks() | tags())) ~ (empty() & description(exact:""))`

// workspaceWork answers `workspace-work`: the read of the work the workspace this shim runs in
// holds that origin does not. A request the shim cannot act on is answered with the reason when it
// names an id to answer. The answer lists the first shimwire.MaxWorkspaceWorkCommits work commits
// and counts them all, and a refusal carries at most maxAnswerErrorBytes of its reason
// (cutAnswerError), so the answer always fits one line.
func (s *shim) workspaceWork(request shimwire.WorkspaceWork) {
	result := shimwire.WorkspaceWorkResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a workspace-work frame: %v", err)
			return
		}
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else if head, changed, work, err := s.readWorkspaceWork(request); err != nil {
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else {
		result.Head, result.Changed, result.Count = head, changed, len(work)
		result.Commits = work[:min(len(work), shimwire.MaxWorkspaceWorkCommits)]
	}
	s.toDaemon(result)
}

// maxAnswerErrorBytes is the most of a refusal's reason that a one-line answer to the daemon
// carries: the answer to `workspace-work` (workspaceWork) and to `adopt-working-copy` (adopt).
// A failed command's reason holds all of jj's standard error (workspace.RunCommand), which has no
// bound of its own. Such an answer goes out as one line, and the writer refuses a line over
// shimwire.MaxFrameBytes: the outbox then closes the connection and sends that line first again
// at each reconnect, so the role's stream stays down (ASVS v5.0.0-15.2.2). A cut reason keeps its
// start, which names the command, its exit and jj's first words, and its end, where
// workspace.RunCommand puts the note it adds to a failure (workspace.StaleGitLockNote, which tells
// the role what to do about a git lock). The daemon names the reason on its log lines, and for the
// read in the message on the issue too, which a short reason keeps readable.
const maxAnswerErrorBytes = 4 << 10

// cutAnswerError is reason as a one-line answer carries it: all of it when it is at most
// maxAnswerErrorBytes, and otherwise its first and its last half of that bound, each cut where a
// UTF-8 character starts, with a line between them that says how many bytes the shim cut there and
// how long the reason was.
func cutAnswerError(reason string) string {
	if len(reason) <= maxAnswerErrorBytes {
		return reason
	}
	head := maxAnswerErrorBytes / 2
	for head > 0 && !utf8.RuneStart(reason[head]) {
		head--
	}
	tail := len(reason) - maxAnswerErrorBytes/2
	for tail < len(reason) && !utf8.RuneStart(reason[tail]) {
		tail++
	}
	return fmt.Sprintf("%s\n[The shim cut %d bytes of this error here. The error had %d bytes.]\n%s",
		reason[:head], tail-head, len(reason), reason[tail:])
}

// readWorkspaceWork runs the read's two jj commands in the workspace the shim's environment
// names in LEGION_WORKSPACE, with the jj of PATH and within the request's budget, as the adoption
// does (openWorkspace). The first reads the working copy's commit and whether it is empty, and it
// is the one jj snapshot the read makes; the second is --ignore-working-copy, so both describe one
// state of the workspace. It answers the working copy's commit, whether that commit holds a
// change, and every work commit (workspaceWorkRevset), newest first. The snapshot is what the read
// writes, as every jj command that reads the working copy does: jj records an operation and, where
// files of the working copy changed, a new working-copy commit and the git ref jj keeps for it
// (refs/jj/keep). The read changes no file of the working copy, moves no bookmark and pushes
// nothing, and it needs no git.
func (s *shim) readWorkspaceWork(request shimwire.WorkspaceWork) (head string, changed bool, work []string, err error) {
	opened, done, err := s.openWorkspace("read", s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return "", false, nil, err
	}
	defer done()
	dir, jj, run := opened.dir, opened.jj, opened.run
	if err := snapshotTaskWorkingCopies(run, jj, dir, request.Issue); err != nil {
		return "", false, nil, err
	}

	at, err := run(nil, jj, "log", "-R", dir, "-r", "@", "--no-graph", "-T", `commit_id ++ " " ++ empty`)
	if err != nil {
		return "", false, nil, err
	}
	head, empty, _ := strings.Cut(at, " ")
	if head == "" || (empty != "true" && empty != "false") {
		return "", false, nil, fmt.Errorf("worker-shim: jj answered %q for the working copy, not its commit and whether it is empty", at)
	}
	listed, err := run(nil, jj, "log", "-R", dir, "--ignore-working-copy", "-r", workspaceWorkRevset, "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return "", false, nil, err
	}
	return head, empty == "false", workspace.OutputLines(listed), nil
}

// snapshotTaskWorkingCopies snapshots the existing task workspaces beside dir before the
// clone-wide revset reads them. It follows the same names and paths as workspace.ProvisionTask,
// including the suffix of a branch allocated to a later run. Missing task directories are not
// live working copies; other errors fail the read instead of answering that the disk is clean.
func snapshotTaskWorkingCopies(run func(stdin []byte, args ...string) (string, error), jj, dir, issue string) error {
	listed, err := run(nil, jj, "workspace", "list", "-R", dir, "--ignore-working-copy", "-T", `name ++ "\n"`)
	if err != nil {
		return err
	}
	prefix := strings.ToLower(issue) + "."
	for _, name := range workspace.OutputLines(listed) {
		part, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		branch := workspace.Bookmark(issue) + "." + part
		taskDir, err := workspace.TaskDir(dir, branch)
		if err != nil || filepath.Base(taskDir) != name {
			continue
		}
		if _, err := os.Lstat(taskDir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("worker-shim: the working copy of %s: %w", branch, err)
		}
		if _, err := run(nil, jj, "log", "-R", taskDir, "-r", "@", "--no-graph", "-T", "commit_id"); err != nil {
			return err
		}
	}
	return nil
}
