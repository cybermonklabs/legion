package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The workspace read: the shim of one role answers whether the workspace every role of its issue
// shares holds work that no branch or tag on origin holds (workspace.ReadWork, which counts
// workspace.WorkRevset). A pod update round that replaces the pod without waiting for every role
// reads it to name what goes with the pod, and the idle release reads it to take the pod only when
// it holds no work. The shim runs the jj of the workspace it already works in; where no role
// process runs, the pod's launcher makes the same read (internal/launcher's readWorkspace).

// workspaceWork answers `workspace-work`: the read of the work the workspace this shim runs in
// holds that origin does not, answered as the pod's launcher answers the same read
// (shimwire.AnswerWorkspaceWork: a refusal's reason cut to one line, the first
// shimwire.MaxWorkspaceWorkCommits work commits listed and all of them counted). A request that
// names no id to answer is logged and dropped.
func (s *shim) workspaceWork(request shimwire.WorkspaceWork) {
	result, refused := shimwire.AnswerWorkspaceWork(request, func() (string, bool, []string, error) {
		return s.readWorkspaceWork(request)
	})
	if refused != nil {
		s.log.Printf("[worker-shim] refusing a workspace-work frame: %v", refused)
		return
	}
	s.toDaemon(result)
}

// readWorkspaceWork runs the read (workspace.ReadWork) in the workspace the shim's environment
// names in LEGION_WORKSPACE, with the jj of PATH and within the request's budget, as the adoption
// does (openWorkspace). It first snapshots the working copy of each task of the issue
// (snapshotTaskWorkingCopies), so the clone-wide revset counts their work too. It answers the
// working copy's commit, whether that commit holds a change, and every work commit
// (workspace.WorkRevset), newest first. The one thing it writes is jj's snapshot of each working
// copy.
func (s *shim) readWorkspaceWork(request shimwire.WorkspaceWork) (head string, changed bool, work []string, err error) {
	opened, done, err := s.openWorkspace("read", s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return "", false, nil, err
	}
	defer done()
	if err := snapshotTaskWorkingCopies(opened.run, opened.jj, opened.dir, request.Issue); err != nil {
		return "", false, nil, err
	}
	run := func(args ...string) (string, error) { return opened.run(nil, args...) }
	return workspace.ReadWork(run, opened.jj, opened.dir)
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
