package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The commands the daemon asks this shim for: the working copy's adoption, answered in a
// goroutine of its own (answer), and the context every such command runs on, which the shim ends
// at its own end and waits out before Run returns (endCommands, drainCommands). The release's save
// is a command of this kind too and lives in bundle.go, which is long enough to be a file of its
// own.

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
// (internal/config refuses one under it, and AGENTS.md names the number), which is a change to
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

// adopt answers `adopt-working-copy` (the adoption branch of createShimBridge's onLine,
// worker-shim.ts). A request the shim cannot act on is answered with the reason when it names an
// id to answer.
func (s *shim) adopt(request shimwire.AdoptWorkingCopy) {
	result := shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing an adopt-working-copy frame: %v", err)
			return
		}
		result.OK, result.Error = false, err.Error()
	} else if err := s.runAdoption(request); err != nil {
		result.OK, result.Error = false, err.Error()
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

// runAdoption runs the adoption with PATH's jj — the one the pane's own shell runs, since no
// runtime pins a jj on a pane or pod — on the workspace the shim's environment names in
// LEGION_WORKSPACE, which the runtime sets on every pane, under the requested identity and
// budget. Every role of an issue shares the workspace, so a working copy the previous role left
// described (its pushed commit) is kept as it is, and the incoming role starts on a fresh one of
// its own (`jj new`): otherwise its work would land in the previous role's commit, authored by the
// previous role's App. One read decides which of the two changes the working copy needs, and only
// that one runs. Both commands run in the workspace itself, never in the shim's own directory: jj
// reads the repository of the directory it runs in, whatever -R names, and migrates that
// repository's legacy configuration (workspace.RunCommand). A failure is reported the way the
// shipped runner does (commandFailure, packages/workspace/src/workspace.ts): the daemon decides
// what a failed adoption means. A shim without LEGION_WORKSPACE was not started by a runtime, and
// refuses (runWorkingCopyAdoption, worker-shim.ts); a PATH without jj refuses naming jj and the
// PATH searched. The budget is a bound on the shim's command context (s.commands) rather than a
// context of its own, so the shim's own end ends this jj at once and a jj that outlives the budget
// is still reported as a timeout.
func (s *shim) runAdoption(request shimwire.AdoptWorkingCopy) error {
	dir := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if dir == "" {
		return errors.New("worker-shim: LEGION_WORKSPACE is not set; no workspace to adopt")
	}
	jj, err := exec.LookPath("jj")
	if err != nil {
		return fmt.Errorf("worker-shim: jj is not on PATH (%s); no jj to adopt the working copy with", os.Getenv("PATH"))
	}
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(s.commands, budget)
	defer cancel()
	env := append(slices.Clone(s.cfg.Env), "JJ_USER="+request.JJUser, "JJ_EMAIL="+request.JJEmail)
	started := time.Now()
	described, err := workspace.RunCommand(ctx, nil, budget, started, env, dir, nil, describedArgs(jj, dir))
	if err != nil {
		return err
	}
	change := adoptionArgs(jj, dir)
	if described != "" {
		change = []string{jj, "new", "-R", dir}
	}
	_, err = workspace.RunCommand(ctx, nil, budget, started, env, dir, nil, change)
	return err
}
