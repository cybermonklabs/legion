package shim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The project release's read: whether the workspace every role of the issue shares holds work no
// remote branch or tag holds, which the release asks before it deletes the pod with its disk. Two
// facts cross the wire, a count and a flag; the read builds no bundle, sends no commit and writes
// no file. What it does write is jj's own record of the working copy's snapshot.

// unpushedRevset is the commits of a workspace's unpushed work: every visible commit no remote
// branch or tag holds, on any head, less each workspace's working copy with no change and less the
// root commit. It starts from the set the bundle builder keeps (bundleKeptRevset) and is written
// out here rather than built from it, because dispatch://INFRA-3271's R4 removes the bundle code,
// and this read stays.
//
// Every empty working copy is subtracted, not only this workspace's own. A pod's shared clone has
// a workspace of its own, `default`, whose working copy is an empty commit on the default branch
// that no remote holds (workspace's feedClone abandons the clone's first snapshot), so with
// `~ (@ & empty())` in its place every pod's workspace would read one unpushed commit. A `default`
// working copy that does hold a change is still counted.
const unpushedRevset = `(::visible_heads() | ::@) ~ ::(remote_bookmarks() | tags()) ~ (working_copies() & empty()) ~ root()`

// workingCopyChangedRevset is the workspace's own working copy when it holds a change, and nothing
// when it holds none.
const workingCopyChangedRevset = `@ & ~empty()`

// Unpushed is a workspace's unpushed work as ReadUnpushed counts it, and nothing of the work
// itself: Commits is how many of its commits no remote branch or tag holds (unpushedRevset), and
// WorkingCopyChanged is whether its own working copy holds a change.
type Unpushed struct {
	Commits            int
	WorkingCopyChanged bool
}

// ReadUnpushed reads the unpushed work of the jj workspace at dir with the jj of PATH, the one the
// pane's or pod's own shell runs, under env and within budget, which bounds ctx. Its first jj runs
// without --ignore-working-copy, so jj snapshots the working copy before it reads whether the
// working copy holds a change: a change not yet snapshotted is unpushed work too. The count after
// it is --ignore-working-copy, so both facts describe one state of the workspace. Both commands
// run in dir itself (workspace.RunCommand). It is exported for a test's stand-in agent, which
// answers the daemon's read-unpushed on a workspace of its own as this shim does.
func ReadUnpushed(ctx context.Context, dir string, env []string, budget time.Duration) (Unpushed, error) {
	jj, err := exec.LookPath("jj")
	if err != nil {
		return Unpushed{}, fmt.Errorf("worker-shim: jj is not on PATH (%s); no jj to read the workspace with", os.Getenv("PATH"))
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started := time.Now()
	run := func(args ...string) (string, error) {
		return workspace.RunCommand(ctx, procgroup.NoReaper, budget, started, env, dir, nil, args)
	}
	changed, err := run(jj, "log", "-R", dir, "-r", workingCopyChangedRevset, "--no-graph", "-T", "commit_id")
	if err != nil {
		return Unpushed{}, err
	}
	commits, err := run(jj, "log", "-R", dir, "--ignore-working-copy", "-r", unpushedRevset, "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return Unpushed{}, err
	}
	return Unpushed{Commits: len(workspace.OutputLines(commits)), WorkingCopyChanged: changed != ""}, nil
}

// readUnpushed answers `read-unpushed` on the workspace the shim's environment names in
// LEGION_WORKSPACE, which the runtime sets on every role. A request the shim cannot act on is
// answered with the reason when it names an id to answer, and a shim with no workspace refuses by
// name rather than answering that nothing is unpushed. The budget bounds the shim's command
// context (s.commands), so the shim's own end ends the read's jj at once.
func (s *shim) readUnpushed(request shimwire.ReadUnpushed) {
	result := shimwire.ReadUnpushedResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing a read-unpushed frame: %v", err)
			return
		}
		result.OK, result.Error = false, err.Error()
	} else if dir := envValue(s.cfg.Env, "LEGION_WORKSPACE"); dir == "" {
		result.OK, result.Error = false, "worker-shim: LEGION_WORKSPACE is not set; no workspace to read for unpushed work"
	} else if read, err := ReadUnpushed(s.commands, dir, s.cfg.Env, time.Duration(request.TimeoutMs)*time.Millisecond); err != nil {
		result.OK, result.Error = false, err.Error()
	} else {
		result.Commits, result.WorkingCopyChanged = read.Commits, read.WorkingCopyChanged
	}
	s.toDaemon(result)
}
