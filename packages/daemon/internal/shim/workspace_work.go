package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The workspace read: the shim of one role answers whether the workspace every role of its issue
// shares holds work that no branch or tag on origin holds, so a pod update round that replaces the
// pod without waiting for every role can name what goes with it. Only a shim can read it: it runs
// the jj of the workspace it already works in.

// workspaceWorkRevset is the work the read counts: every commit under a workspace's working copy
// or a local bookmark, less three kinds of commit that are not work.
//
// A commit origin holds is not work: one under a remote bookmark or a tag is already pushed, and
// the replacement pod's fetch gets it back. Tags are subtracted beside remote bookmarks for the
// reason bundleKeptRevset subtracts them.
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
// rewritten parent, workspace's bookmark.go), which goes unnamed.
//
// The idle release's save (bundleKeptRevset) is another set and stays as it is: it keeps every
// visible head, where this read names what it counts on the issue.
const workspaceWorkRevset = `(::(working_copies() | bookmarks()) ~ ::(remote_bookmarks() | tags())) ~ (empty() & description(exact:""))`

// workspaceWork answers `workspace-work`: the read of the work the workspace this shim runs in
// holds that origin does not. A request the shim cannot act on is answered with the reason when it
// names an id to answer. The answer lists the first shimwire.MaxWorkspaceWorkCommits work commits
// and counts them all, so it always fits one line.
func (s *shim) workspaceWork(request shimwire.WorkspaceWork) {
	result := shimwire.WorkspaceWorkResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a workspace-work frame: %v", err)
			return
		}
		result.OK, result.Error = false, err.Error()
	} else if head, changed, work, err := s.readWorkspaceWork(request); err != nil {
		result.OK, result.Error = false, err.Error()
	} else {
		result.Head, result.Changed, result.Count = head, changed, len(work)
		result.Commits = work[:min(len(work), shimwire.MaxWorkspaceWorkCommits)]
	}
	s.toDaemon(result)
}

// readWorkspaceWork runs the read's two jj commands with the jj of PATH in the workspace the
// shim's environment names in LEGION_WORKSPACE, as runBundle does. The first reads the working
// copy's commit and whether it is empty, and it is the one jj snapshot the read makes; the second
// is --ignore-working-copy, so both describe one state of the workspace. It answers the working
// copy's commit, whether that commit holds a change, and every work commit (workspaceWorkRevset),
// newest first. It writes no ref and no file, and needs no git. The budget is a bound on the
// shim's command context (s.commands), as runBundle's is.
func (s *shim) readWorkspaceWork(request shimwire.WorkspaceWork) (head string, changed bool, work []string, err error) {
	dir := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if dir == "" {
		return "", false, nil, errors.New("worker-shim: LEGION_WORKSPACE is not set; no workspace to read")
	}
	jj, err := exec.LookPath("jj")
	if err != nil {
		return "", false, nil, fmt.Errorf("worker-shim: jj is not on PATH (%s); no jj to read the workspace with", os.Getenv("PATH"))
	}
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(s.commands, budget)
	defer cancel()
	started := time.Now()
	run := func(args ...string) (string, error) {
		return workspace.RunCommand(ctx, procgroup.NoReaper, budget, started, s.cfg.Env, dir, nil, args)
	}

	at, err := run(jj, "log", "-R", dir, "-r", "@", "--no-graph", "-T", `commit_id ++ " " ++ empty`)
	if err != nil {
		return "", false, nil, err
	}
	head, empty, _ := strings.Cut(at, " ")
	if head == "" || (empty != "true" && empty != "false") {
		return "", false, nil, fmt.Errorf("worker-shim: jj answered %q for the working copy, not its commit and whether it is empty", at)
	}
	listed, err := run(jj, "log", "-R", dir, "--ignore-working-copy", "-r", workspaceWorkRevset, "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return "", false, nil, err
	}
	return head, empty == "false", workspace.OutputLines(listed), nil
}
