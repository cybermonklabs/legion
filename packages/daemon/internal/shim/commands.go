package shim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The commands the daemon asks this shim for: the working copy's adoption, answered in a
// goroutine of its own (answer), the context every such command runs on, which the shim ends at
// its own end and waits out before Run returns (endCommands, drainCommands), and the start of
// every command that runs in the role's workspace (openWorkspace). The release's save is a
// command of this kind too and lives in bundle.go, which is long enough to be a file of its own.

// answer runs one frame's answer — the adoption, the release's save — in a goroutine of its own,
// so the connection's reader never waits on the git and jj it starts, and counts it so that Run
// waits for it before it returns (drainCommands). Every one of them is dispatched from
// fromDaemon, which the reader calls one frame at a time, so a command is counted before the
// frame after it is read. Two counts are kept: commandRuns, which the drain waits on, and
// commandsInFlight, which says whether there is anything to wait for at all. The second is
// lowered first, so a count of 0 means every command has returned and nothing but the
// goroutines' own last line is left.
func (s *shim) answer(command func()) {
	s.commandsInFlight.Add(1)
	s.commandRuns.Add(1)
	go func() {
		defer s.commandRuns.Done()
		defer s.commandsInFlight.Add(-1)
		command()
	}()
}

// endCommands ends the context every command the daemon asked for runs on, and keeps when it did
// (commandsEnded), which is where drainCommands' bound starts. It runs at each of the shim's
// three ends — the context Run was given, the agent's SIGTERM (terminate) and the shim's own loop
// — and at the head of the drain, so the drain always has that reading; the first call is the one
// that counts, and the rest cancel an already cancelled context.
func (s *shim) endCommands() {
	s.commandsEnded.CompareAndSwap(0, int64(time.Since(s.started)))
	s.cancelCommands()
}

// sinceCommandsEnded is how long the commands the daemon asked for have been ended for, read on
// this process's monotonic clock as the difference of two offsets from s.started
// (commandsEnded). It answers 0 before endCommands has run.
func (s *shim) sinceCommandsEnded() time.Duration {
	if ended := s.commandsEnded.Load(); ended != 0 {
		return time.Since(s.started) - time.Duration(ended)
	}
	return 0
}

// drainCommands waits for the commands the daemon asked for (answer) to return before Run does,
// once the context their budgets are built on has ended: terminate ends it at the agent's SIGTERM
// and the loop's own end ends it too, so every one of those commands is already being stopped when
// this wait begins. What it keeps alive meanwhile is this process, which is what sends that stop:
// procgroup gives each command's whole group a SIGTERM, a grace in which a git that handles it
// removes the lock files it holds, and a SIGKILL for whatever outlived the grace
// (internal/procgroup), and none of that runs once the shim has exited.
//
// A drain with nothing in flight waits for nothing and takes no wait on the shim's clock at all.
// commandsInFlight is 0 once every command answered has returned, which is the ordinary end of
// the daemon's `shutdown`: the commands are ended at the agent's SIGTERM and the agent then has
// its whole grace, so an agent that spends the stop span below inside that grace leaves this
// drain a bound of 0 as well. A drain that took its select whatever that count read would then be
// choosing between a timer of 0 that has already fired and a goroutine that has not yet closed its
// channel. Such a select takes the timer: the pane's last lines would say that a command the
// daemon asked for is still running, seconds after it returned, which is the line a person reads
// while diagnosing that very stop. A shim that answered no such frame has nothing in flight either.
//
// The wait is bounded by that stop's own span, procgroup.StopGrace plus the procgroup.WaitDelay
// exec then waits for anything still holding the command's pipes — counted from the moment the
// commands were ended, not from here. On the daemon's `shutdown` those are not the same moment:
// terminate ends the commands and then the agent has its whole grace, which this wait would
// otherwise follow with a span the command has already spent. Counting the difference into
// MinStopGrace instead would raise the smallest `worker_stop_timeout_seconds` the daemon accepts
// (internal/config refuses one under it, and its refusal names the number), which is a change to
// which deployments are accepted and not this wait's to make. A command still running past the
// bound has escaped its group — a descendant of its own that called setsid, which neither signal
// reaches — so no further waiting would end it: it is logged and left, and the shim's exit reaches
// the daemon rather than being held past the stop grace at which the launcher kills it.
//
// The two readings the bound is built from are on two clocks, because they have to be: the wait
// itself is taken from the shim's own clock (s.clock, which a test drives step by step), and
// Clock hands out waits and has no reading of the time, so what the commands have already spent
// is read from this process's monotonic clock instead — as an offset from s.started, which keeps
// a wall clock stepped mid-stop from making that span negative, which would stretch this wait
// past the bound it means to give, or enormous, which would leave no wait at all.
func (s *shim) drainCommands() {
	if s.commandsInFlight.Load() == 0 {
		return
	}
	s.endCommands()
	returned := make(chan struct{})
	go func() {
		s.commandRuns.Wait()
		close(returned)
	}()
	spent := s.sinceCommandsEnded()
	bound := max(procgroup.StopGrace+procgroup.WaitDelay-spent, 0)
	select {
	case <-returned:
	case <-s.clock.After(bound):
		s.log.Printf("[worker-shim] a command the daemon asked for is still running %v after it was ended; leaving it", s.sinceCommandsEnded())
	}
}

// openedWorkspace is what one request of the daemon's runs its commands with in the workspace
// its role works in (openWorkspace): the workspace, the jj of PATH, and run, which runs one
// command there with the request's environment and within its budget, with stdin fed to it, and
// answers its trimmed standard output (workspace.RunCommand). runIn runs one command the same way
// in another directory: the adoption's in the fix's working copy beside the workspace.
type openedWorkspace struct {
	dir   string
	jj    string
	run   func(stdin []byte, args ...string) (string, error)
	runIn func(dir string, stdin []byte, args ...string) (string, error)
}

// openWorkspace is the one start of every request a shim answers in its role's workspace: the
// save (runBundle), the read (readWorkspaceWork) and the adoption (runAdoption). The workspace is
// the one the shim's environment names in LEGION_WORKSPACE, which the runtime sets on every role,
// and the jj is the one of PATH, which the pane's or pod's own shell runs, since no runtime pins it
// on a pane or a pod. env is the environment the request's commands run with: the shim's own
// (s.cfg.Env) for the save and the read, and for the adoption that environment with the requested
// identity added (JJ_USER, JJ_EMAIL). The budget is a bound on the shim's command context
// (s.commands) rather than a context of its own, so the shim's own end ends the request's commands
// at once and a command that outlives the budget is still reported as a timeout. The caller calls
// done once its commands have run. verb is what the caller does to the workspace ("bundle",
// "read", "adopt"), which both refusals name: "no workspace to <verb>" for a shim with no
// LEGION_WORKSPACE, and "no jj to <verb> the workspace with" for a PATH with no jj.
func (s *shim) openWorkspace(verb string, env []string, budget time.Duration) (opened openedWorkspace, done context.CancelFunc, err error) {
	dir := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if dir == "" {
		return openedWorkspace{}, nil, fmt.Errorf("worker-shim: LEGION_WORKSPACE is not set; no workspace to %s", verb)
	}
	jj, err := exec.LookPath("jj")
	if err != nil {
		return openedWorkspace{}, nil, fmt.Errorf("worker-shim: jj is not on PATH (%s); no jj to %s the workspace with", os.Getenv("PATH"), verb)
	}
	ctx, cancel := context.WithTimeout(s.commands, budget)
	started := time.Now()
	runIn := func(at string, stdin []byte, args ...string) (string, error) {
		return workspace.RunCommand(ctx, procgroup.NoReaper, budget, started, env, at, stdin, args)
	}
	run := func(stdin []byte, args ...string) (string, error) {
		return runIn(dir, stdin, args...)
	}
	return openedWorkspace{dir: dir, jj: jj, run: run, runIn: runIn}, cancel, nil
}

// adopt answers `adopt-working-copy` (the adoption branch of createShimBridge's onLine,
// worker-shim.ts). A request the shim cannot act on is answered with the reason when it names an
// id to answer. The answer goes out as one line, so a refusal carries at most maxAnswerErrorBytes
// of its reason (cutAnswerError): jj's standard error has no bound of its own. An adoption that
// ran answers the echo of each member it acted on: the fix's directory for fixFrom, and the
// commit the task's working copy moved to for moveTo.
func (s *shim) adopt(request shimwire.AdoptWorkingCopy) {
	result := shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing an adopt-working-copy frame: %v", err)
			return
		}
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else if fixDir, movedTo, err := s.runAdoption(request); err != nil {
		result.OK, result.Error = false, cutAnswerError(err.Error())
	} else {
		result.FixDir, result.MovedTo = fixDir, movedTo
	}
	s.toDaemon(result)
}

// adoptionArgs is the one adoption command both executors run (adoptWorkingCopyCommand,
// packages/workspace/src/workspace.ts): the working copy's author becomes the identity in
// the command's environment, and only while it is undescribed — a described working copy is a
// previous phase's work and keeps its author.
func adoptionArgs(jj, dir string) []string {
	return []string{jj, "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", dir}
}

// describedArgs prints the working copy's commit id when it is described, and nothing otherwise.
func describedArgs(jj, dir string) []string {
	return []string{jj, "log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", dir}
}

// fixWorkArgs prints each commit of the fix's working copy at fix that holds work over fixFrom: a
// commit between fixFrom and the working copy, the working copy included, that changes something.
// It prints nothing where the working copy does not stand over fixFrom, or holds nothing over it.
// It is no --ignore-working-copy read, so it records the copy's last edits first.
func fixWorkArgs(jj, fix, fixFrom string) []string {
	return []string{jj, "log", "-r", fixFrom + "::@ ~ " + fixFrom + " ~ empty()", "--no-graph", "-T", `commit_id ++ "\n"`, "-R", fix}
}

// runAdoption runs the adoption under the requested identity and budget, in the workspace the
// shim's environment names in LEGION_WORKSPACE and with PATH's jj (openWorkspace): the one the
// pane's own shell runs, since no runtime pins a jj on a pane or pod. Every role of an issue
// shares the workspace, so a working copy the previous role left described (its pushed commit) is
// kept as it is, and the incoming role starts on a fresh one of its own (`jj new`): otherwise its
// work would land in the previous role's commit, authored by the previous role's App. One read
// decides which of the two changes the working copy needs, and only that one runs. Every command
// runs in the working copy it acts on, never in the shim's own directory: jj reads the repository
// of the directory it runs in, whatever -R names, and migrates that repository's legacy
// configuration (workspace.RunCommand). A failure is reported the way the shipped runner does
// (commandFailure, packages/workspace/src/workspace.ts): the daemon decides what a failed adoption
// means. A shim without LEGION_WORKSPACE was not started by a runtime, and refuses
// (runWorkingCopyAdoption, worker-shim.ts); a PATH without jj refuses naming jj and the PATH
// searched.
//
// A request may name two commits. With MoveTo, the task's working copy moves first: one read of
// the commit it stands on, and a `jj new` onto MoveTo only where that is another commit; the answer
// names MoveTo as moved to. With FixFrom, the adoption is of the fix's working copy, beside the
// workspace (workspace.FixDir), in place of the workspace's: a jj workspace of the same clone added
// where none is there, on the head of the fix's own branch where the request names it (FixBranch)
// and origin holds it, and on FixFrom otherwise (fixStart). Where one is, it is brought up to date, and one read (fixWorkArgs)
// asks whether it holds work of its own over FixFrom — a commit, or an edit the read itself
// records. Where it does, it is this fix's copy, and it stays where it stands: the adoption before a
// fix's task runs again each time that task is sent again, and the pod that replaced a released one
// made the copy again at its own last working copy (the launcher's restoreWorkspace). Where it does
// not, it starts on a fresh commit over FixFrom, as a later fix of the task does. Then the adoption
// command there, so the fix's commits carry the role's identity. The answer names that directory.
// Every workspace of the clone shares its store, so a commit a step makes in the workspace is there
// in the fix's working copy by its id, and a rewrite made from the fix's working copy of a commit
// under the workspace's working copy leaves that working copy stale. So where the fix's working
// copy exists, every adoption that acts on the workspace brings it up to date first (`jj workspace
// update-stale`).
func (s *shim) runAdoption(request shimwire.AdoptWorkingCopy) (fixDir, movedTo string, err error) {
	env := append(slices.Clone(s.cfg.Env), "JJ_USER="+request.JJUser, "JJ_EMAIL="+request.JJEmail)
	opened, done, err := s.openWorkspace("adopt", env, time.Duration(request.TimeoutMs)*time.Millisecond)
	if err != nil {
		return "", "", err
	}
	defer done()
	dir, jj, run, runIn := opened.dir, opened.jj, opened.run, opened.runIn
	fix := workspace.FixDir(dir)
	fixMade, err := holdsJJ(fix)
	if err != nil {
		return "", "", err
	}
	if fixMade && (request.MoveTo != "" || request.FixFrom == "") {
		if _, err := run(nil, jj, "workspace", "update-stale", "-R", dir); err != nil {
			return "", "", err
		}
	}
	if request.MoveTo != "" {
		parent, err := run(nil, jj, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "-R", dir)
		if err != nil {
			return "", "", err
		}
		if parent != request.MoveTo {
			if _, err := run(nil, jj, "new", request.MoveTo, "-R", dir); err != nil {
				return "", "", err
			}
		}
		movedTo = request.MoveTo
	}
	if request.FixFrom != "" {
		if fixMade {
			if _, err := runIn(fix, nil, jj, "workspace", "update-stale", "-R", fix); err != nil {
				return "", "", err
			}
			work, err := runIn(fix, nil, fixWorkArgs(jj, fix, request.FixFrom)...)
			if err != nil {
				return "", "", err
			}
			if work == "" {
				if _, err := runIn(fix, nil, jj, "new", request.FixFrom, "-R", fix); err != nil {
					return "", "", err
				}
			}
		} else {
			from, err := fixStart(request, dir, jj, run)
			if err != nil {
				return "", "", err
			}
			if _, err := run(nil, jj, "workspace", "add", "--name", filepath.Base(fix), "-r", from, fix, "-R", dir); err != nil {
				return "", "", err
			}
		}
		if _, err := runIn(fix, nil, adoptionArgs(jj, fix)...); err != nil {
			return "", "", err
		}
		return fix, movedTo, nil
	}
	described, err := run(nil, describedArgs(jj, dir)...)
	if err != nil {
		return "", "", err
	}
	change := adoptionArgs(jj, dir)
	if described != "" {
		change = []string{jj, "new", "-R", dir}
	}
	if _, err := run(nil, change...); err != nil {
		return "", "", err
	}
	return "", movedTo, nil
}

// fixStart is the revision a fix's working copy is made at where the directory beside the
// workspace holds none: the head of the fix's own branch on origin where the request names that
// branch and origin holds it, so a fix that pushed its work at an update round finds it on the pod
// that replaced the round's, and FixFrom otherwise. One read of origin in the clone's git directory
// (`git ls-remote`, which answers an absent branch with no line and exit 0) decides, and only where
// it names a commit does a fetch of that one branch run, in the workspace, so the clone holds the
// branch's head as a remote bookmark. A track of that bookmark follows: a fetch leaves it
// untracked, and jj refuses to push a bookmark over a remote one it does not track and holds the
// commits of one immutable. Tracked, the branch is the fix's in this clone as it was in the pod
// that pushed it, so the fix's next push of it succeeds and the fix can rebase its own commits.
func fixStart(request shimwire.AdoptWorkingCopy, dir, jj string, run func(stdin []byte, args ...string) (string, error)) (string, error) {
	if request.FixBranch == "" {
		return request.FixFrom, nil
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("worker-shim: git is not on PATH (%s); no git to read the fix's branch %s with", os.Getenv("PATH"), request.FixBranch)
	}
	gitDir, err := workspace.GitDir(dir)
	if err != nil {
		return "", fmt.Errorf("worker-shim: %w", err)
	}
	pushed, err := run(nil, git, "--git-dir="+gitDir, "ls-remote", "origin", "refs/heads/"+request.FixBranch)
	if err != nil {
		return "", err
	}
	if pushed == "" {
		return request.FixFrom, nil
	}
	if _, err := run(nil, jj, "git", "fetch", "--remote", "origin", "--branch", request.FixBranch, "-R", dir); err != nil {
		return "", err
	}
	if _, err := run(nil, jj, "bookmark", "track", "exact:"+request.FixBranch, "--remote", "exact:origin", "-R", dir); err != nil {
		return "", err
	}
	return request.FixBranch + "@origin", nil
}

// holdsJJ is whether dir holds a jj workspace: a `.jj` directory. A dir that is not there holds
// none, and a read that fails for another reason is the adoption's failure, named.
func holdsJJ(dir string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, ".jj"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("worker-shim: read the fix's working copy %s: %w", dir, err)
	}
	return info.IsDir(), nil
}
