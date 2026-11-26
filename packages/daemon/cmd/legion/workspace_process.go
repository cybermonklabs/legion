package main

import (
	"io"
	"os"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// workspaceProcess is what `legion workspace-init` reads of the process it runs in: its environment
// (each variable it reads, the PATH it resolves git and jj on, the TMPDIR its one-shot credential
// goes under, and the environment every git and jj it starts is built from), the clock its fetch's
// retries read and wait on, the fetch's bound, the runner its commands run under, and whether it
// points the process's standard logger at its stderr for the run. A run as `legion` takes its
// process's own (processWorkspace). A test runs the command inside its test process with one of
// its own (runIn), so a test of the command changes nothing the whole test process shares.
type workspaceProcess struct {
	environ []string
	// clock is what the fetch's clone retries read and wait on (workspace.FetchRequest's Clock).
	clock workspace.Clock
	// fetchBound is the fetch's workspace.FetchRequest.Bound, the time each clocked stage of the
	// fetch gets. Zero is workspace.FetchTimeout.
	fetchBound time.Duration
	// newRunner is the runner the commands run under, given the environment its children's are
	// built from, the slow-command budget (commandBudget) and the tools resolved on PATH.
	newRunner func(environ []string, timeout time.Duration, tools map[string]string) workspace.Runner
	// standardLog is whether the run points the process's standard logger at its stderr
	// (podLogs). A run as `legion` does: any line a library writes there then passes the URL rule.
	// A test's run inside its test process does not, since the logger is every test's; the one
	// writer of it the command is known to start, the cache's HTTP server, is handed a logger on
	// stderr of its own (repocache.Options.ErrorLog).
	standardLog bool
}

// processWorkspace is this process's own: its environment as it started, the real clock, the
// fetch's own bound, the production runner and the standard logger on stderr.
func processWorkspace() workspaceProcess {
	return workspaceProcess{environ: os.Environ(), clock: clock.Real(), newRunner: workspace.NewRunnerIn, standardLog: true}
}

// logs points the standard logger at stderr when the process does (standardLog), and answers
// what puts it back.
func (p workspaceProcess) logs(stderr io.Writer) (restore func()) {
	if !p.standardLog {
		return func() {}
	}
	return podLogs(stderr)
}

// lookup is name's value in the environment and whether it holds one (envLookup).
func (p workspaceProcess) lookup(name string) (string, bool) {
	return envLookup(p.environ, name)
}

// getenv is name's value in the environment, "" when it is not set (envValue).
func (p workspaceProcess) getenv(name string) string {
	return envValue(p.environ, name)
}

// tempDir is the environment's TMPDIR, or /tmp where it names none (os.TempDir).
func (p workspaceProcess) tempDir() string {
	if dir := p.getenv("TMPDIR"); dir != "" {
		return dir
	}
	return "/tmp"
}

// runner is the runner the commands run under, its children's environments built from this one.
func (p workspaceProcess) runner(timeout time.Duration, tools map[string]string) workspace.Runner {
	return p.newRunner(p.environ, timeout, tools)
}
