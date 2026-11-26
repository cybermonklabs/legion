package shim

import (
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
// does (openWorkspace). It answers the working copy's commit, whether that commit holds a change,
// and every work commit (workspace.WorkRevset), newest first; the one thing it writes is jj's
// snapshot of the working copy.
func (s *shim) readWorkspaceWork(request shimwire.WorkspaceWork) (head string, changed bool, work []string, err error) {
	opened, done, err := s.openWorkspace("read", s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return "", false, nil, err
	}
	defer done()
	// CONTROL COMMIT, never for merge: 121 s of the request's budget spent before the read's
	// own commands run.
	time.Sleep(121 * time.Second)
	run := func(args ...string) (string, error) { return opened.run(nil, args...) }
	return workspace.ReadWork(run, opened.jj, opened.dir)
}
