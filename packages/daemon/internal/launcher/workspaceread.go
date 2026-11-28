package launcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The launcher's workspace read (R6 of #5285): the daemon releases a pod no claim
// owns, or closes a left pod with no live claim, only after a read of the issue workspace found
// nothing that origin does not hold. No role process runs in such a pod, so no role's shim can
// read it; the launcher runs there for the pod's whole life, and its working directory is the
// issue's workspace (internal/runtime/sandbox's rolesDirLauncher, WorkingDir). It makes the read a
// shim makes (workspace.ReadWork), so the two count the same work, and answers on the connection
// of the role the daemon asked through. Its hello says it answers (shimwire.LauncherHello's
// ReadsWorkspace), since a launcher before it closes its connection on the frame.

// The read's first command snapshots the working copy, which writes a commit, and every jj command
// reads the user's, the repository's and the workspace's configuration, which a role can write: a
// signing program for that write, a file monitor, a template or revset alias that calls a
// signature's verifier or changes what the read counts. The launcher holds every role's launcher
// token, which no role may read, so its read runs no program that any configuration names, and
// counts what the read's own revset says, whichever jj the image carries. So it isolates the
// configuration rather than switching known keys off one by one: jj runs with an environment of
// the read's own (launcherReadEnv), whose home is an empty directory made for the one read. jj
// reads the user's configuration from JJ_CONFIG, an empty file there, and keeps each repository's
// and each workspace's configuration under that home's config directory, keyed by the ids the
// repository and the workspace record, so it finds none of either. jj loads a configuration file
// inside the repository's own directories too (`config.toml` of the repository,
// `.jj/workspace-config.toml` of the workspace) where the id file beside it is missing, migrating
// it into the configuration it reads. So before jj runs, the read runs the guards the daemon's own
// jj commands run (workspace.GuardJJ): the workspace and the shared clone that the layout puts
// beside it, and each such legacy file removed. The image's jj keeps a repository's configuration
// by id (0.38 and later), so after the guards no file a role wrote loads. A workspace the guards
// refuse, or whose legacy file cannot be removed, is not read, and the daemon keeps its pod as it
// keeps one whose read cannot be made. The snapshot still runs, so a change in the working copy is
// counted (R5).

// launcherReadEnv is the whole environment of the read's jj: the launcher's PATH, and a home of the
// read's own, home, for every place jj reads a configuration or a state from. No other variable of
// the launcher's reaches jj.
func launcherReadEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"JJ_CONFIG=" + filepath.Join(home, readUserConfig),
	}
}

// readUserConfig is the empty file in the read's home that JJ_CONFIG names.
const readUserConfig = "jj-config.toml"

// launcherReadConfig is the one setting the read names on the command line, the daemon's own and
// not any configuration's: the pod's clones keep the commits a fetch leaves unreachable
// (git.abandon-unreachable-commits false, workspace's ensureFetchConfiguration), and the read's
// first command imports git refs, which with jj's default would abandon those commits.
var launcherReadConfig = []string{"--config=git.abandon-unreachable-commits=false"}

// readWorkspace answers one shimwire.WorkspaceWork on this role's connection, writer, as a role's
// shim answers the same read (shimwire.AnswerWorkspaceWork: a refusal's reason cut to one line,
// its end, which names a stale git lock, kept). It runs on a goroutine of its own (serve), so the
// role's start and stop commands do not wait behind jj; the writer has a mutex of its own, so its
// answer and the command loop's never interleave. A request with no id names nothing to answer,
// and is logged and dropped.
func (m *manager) readWorkspace(ctx context.Context, writer *shimwire.Writer, request shimwire.WorkspaceWork) {
	result, refused := shimwire.AnswerWorkspaceWork(request, func() (string, bool, []string, error) {
		return m.workspaceWork(ctx, request)
	})
	if refused != nil {
		m.logf("launcher: refusing a workspace-work frame: %v", refused)
		return
	}
	if err := writer.WriteFrame(result); err != nil {
		m.logf("launcher: answer the workspace read %s: %v", request.ID, err)
	}
}

// workspaceWork runs the read in the issue workspace, within the request's budget and the
// container's context: Config.Workspace where a test names one, and otherwise the launcher's own
// working directory, which the pod sets to the issue's workspace. The jj is PATH's, the image's own,
// and each command starts through the launcher's pid guard, so PID 1's reaper leaves its exit to
// the command's own wait (pidGuard.starter). It runs with no configuration of anyone's
// (launcherReadEnv), in a home made for the one read and removed after it, and only after the
// workspace guards hold the workspace to the layout and remove each legacy configuration file
// (workspace.GuardJJ); a workspace they refuse is not read.
func (m *manager) workspaceWork(ctx context.Context, request shimwire.WorkspaceWork) (head string, changed bool, work []string, err error) {
	dir := m.cfg.Workspace
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", false, nil, fmt.Errorf("launcher: the launcher's working directory, which is the issue's workspace, cannot be read: %w", err)
		}
	}
	jj, err := exec.LookPath("jj")
	if err != nil {
		return "", false, nil, fmt.Errorf("launcher: jj is not on PATH (%s); no jj to read the workspace with", os.Getenv("PATH"))
	}
	if err := workspace.GuardJJ(dir); err != nil {
		return "", false, nil, fmt.Errorf("launcher: the workspace guards refuse the read: %w", err)
	}
	home, err := os.MkdirTemp("", "legion-workspace-read-")
	if err != nil {
		return "", false, nil, fmt.Errorf("launcher: make the read's own home: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	if err := os.WriteFile(filepath.Join(home, readUserConfig), nil, 0o600); err != nil {
		return "", false, nil, fmt.Errorf("launcher: write the read's empty jj configuration: %w", err)
	}
	start := m.guard.starter()
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started, env := time.Now(), launcherReadEnv(home)
	run := func(args ...string) (string, error) {
		return workspace.RunCommand(bounded, start, budget, started, env, dir, nil, args)
	}
	return workspace.ReadWork(run, jj, dir, launcherReadConfig...)
}
