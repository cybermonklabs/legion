package shim

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// startAtBranch answers `start-at-branch`: the start of a next change's working copy at the issue's
// branch, which the daemon moved to the head of the base the issue's last pull request merged into.
// The shim of the role about to take the change's first task runs it in the workspace it works in
// (workspace.StartAtBranch), so one copy of the steps runs on every runtime. A request the shim
// cannot act on is answered with the reason when it names an id to answer, and a refusal carries
// at most shimwire.MaxAnswerErrorBytes of its reason (shimwire.CutAnswerError), so the answer
// always fits one line.
func (s *shim) startAtBranch(request shimwire.StartAtBranch) {
	result := shimwire.StartAtBranchResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a start-at-branch frame: %v", err)
			return
		}
		result.OK, result.Error = false, shimwire.CutAnswerError(err.Error())
	} else if started, err := s.runStartAtBranch(request); err != nil {
		result.OK, result.Error = false, shimwire.CutAnswerError(err.Error())
	} else {
		result.Moved, result.From, result.At, result.BehindBase = started.Moved, started.From, started.At, started.BehindBase
	}
	s.toDaemon(result)
}

// runStartAtBranch runs the start in the workspace the shim's environment names in
// LEGION_WORKSPACE, with the jj of PATH and within the request's budget, as the adoption and the
// read do (openWorkspace). Its commands run with the shim's own environment, which is its role's,
// so its fetch uses the role's git credential, as a fetch the role runs itself does.
func (s *shim) runStartAtBranch(request shimwire.StartAtBranch) (workspace.StartedAt, error) {
	opened, done, err := s.openWorkspace("start", s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return workspace.StartedAt{}, err
	}
	defer done()
	jj := func(args ...string) (string, error) {
		return opened.run(nil, append([]string{opened.jj}, args...)...)
	}
	return workspace.StartAtBranch(jj, opened.dir, request.Bookmark, request.Base)
}
