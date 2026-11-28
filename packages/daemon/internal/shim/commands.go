package shim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The commands the daemon asks this shim for: the working copy's adoption, answered in a
// goroutine of its own (answer), the context every such command runs on, which the shim ends at
// its own end and waits out before Run returns (endCommands, drainCommands), and the start of
// every command that runs in the role's workspace (openWorkspace). The workspace read is a
// command of this kind too and lives in workspace_work.go.

// answer runs one frame's answer — the adoption, the workspace read — in a goroutine of its own,
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
// The wait is bounded by that stop's own span, the stop's grace (procgroup.StopGraceNow) plus the
// WaitDelay exec then waits for anything still holding the command's pipes (procgroup.WaitDelayNow)
// — counted from
// the moment the commands were ended, not from here. On the daemon's `shutdown` those are not the
// same moment: terminate ends the commands and then the agent has its whole grace, which this wait
// would otherwise follow with a span the command has already spent. Counting the difference into
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
	bound := max(procgroup.StopGraceNow()+procgroup.WaitDelayNow()-spent, 0)
	select {
	case <-returned:
	case <-s.clock.After(bound):
		s.log.Printf("[worker-shim] a command the daemon asked for is still running %v after it was ended; leaving it", s.sinceCommandsEnded())
	}
}

// openedWorkspace is what one request of the daemon's runs its commands with in the workspace
// its role works in (openWorkspace): the workspace, the jj of PATH, the request's budget, ctx,
// which that budget ends, and runWithin, which runs one command there with the request's
// environment, with stdin fed to it, ended at bound after the budget began, and answers its
// trimmed standard output (workspace.RunCommand); its timeout names that bound. run runs a
// command within the whole budget; the adoption's `jj workspace update-stale` runs within less,
// which leaves the reads after it a reserve of the budget (updateReserve). The waits for the
// workspace's locks end with ctx (workspace_lock.go).
type openedWorkspace struct {
	dir       string
	jj        string
	budget    time.Duration
	ctx       context.Context
	runWithin func(bound time.Duration, stdin []byte, args ...string) (string, error)
}

// run runs one command within the request's whole budget (runWithin).
func (o openedWorkspace) run(stdin []byte, args ...string) (string, error) {
	return o.runWithin(o.budget, stdin, args...)
}

// openWorkspace is the one start of every request a shim answers in its role's workspace: the
// read (readWorkspaceWork) and the adoption (runAdoption). The workspace is the one the shim's
// environment names in LEGION_WORKSPACE, which the runtime sets on every role, and the jj is the
// one of PATH, which the pane's or pod's own shell runs, since no runtime pins it on a pane or a
// pod. env is the environment the request's commands run with: the shim's own (s.cfg.Env) for the
// read, and for the adoption that environment with the requested identity added (JJ_USER,
// JJ_EMAIL). The budget is a bound on the shim's command context (s.commands) rather than a
// context of its own, so the shim's own end ends the request's commands at once and a command that
// outlives the budget is still reported as a timeout. The caller calls done once its commands have
// run. verb is what the caller does to the workspace ("read", "adopt"), which both refusals name:
// "no workspace to <verb>" for a shim with no LEGION_WORKSPACE, and "no jj to <verb> the
// workspace with" for a PATH with no jj.
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
	start := s.children.Starter()
	runWithin := func(bound time.Duration, stdin []byte, args ...string) (string, error) {
		within, end := context.WithDeadline(ctx, started.Add(bound))
		defer end()
		return workspace.RunCommand(within, start, bound, started, env, dir, stdin, args)
	}
	return openedWorkspace{dir: dir, jj: jj, budget: budget, ctx: ctx, runWithin: runWithin}, cancel, nil
}

// adopt answers `adopt-working-copy`, which fromDaemon hands it, with the adoption's outcome
// (runAdoption). A request the shim cannot act on is answered with the reason when it names an
// id to answer. The answer goes out as one line, so a refusal carries at most
// shimwire.MaxAnswerErrorBytes of its reason (shimwire.CutAnswerError): jj's standard error has no
// bound of its own. The answer names each commit in which bringing a stale working copy up to
// date left the edits that no jj command had recorded (EditsCommits), whether the adoption then
// succeeded or not, and the shim's log says the same in one line.
func (s *shim) adopt(request shimwire.AdoptWorkingCopy) {
	result := shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing an adopt-working-copy frame: %v", err)
			return
		}
		result.OK, result.Error = false, shimwire.CutAnswerError(err.Error())
	} else {
		edits, err := s.runAdoption(request)
		if len(edits) > 0 {
			result.EditsCommits = edits
			s.log.Printf("[worker-shim] bringing the stale working copy up to date left the edits that no jj command had recorded in %s %s: they are there, and no longer on disk",
				plural(len(edits), "commit", "commits"), strings.Join(edits, ", "))
		}
		if err != nil {
			result.OK, result.Error = false, shimwire.CutAnswerError(err.Error())
		}
	}
	s.toDaemon(result)
}

// plural is one where n is 1 and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// The adoption's commands, adoptionArgs, describedArgs and runAdoption's `jj new`, each run with
// --ignore-working-copy, for every project: none of them snapshots the working copy, which on a
// checkout of some 342,000 files took most of the adoption's budget, and none of them writes the
// working copy's files or its state. So the adoption must keep the tree of the working-copy
// commit: jj finds a working copy fresh only while that tree does not change, and a command that
// changed it would leave the role's next jj command facing a stale working copy. The metaedit
// rewrites the author alone, and `jj new` starts an empty commit on @, so both keep the tree, and
// so does exportHeadArgs's rewrite. The freshness check around them reads with the same flag
// (readFreshness); the one command of the adoption that writes the working copy, `jj workspace
// update-stale`, runs only on a working copy that check read as stale (bringUpToDate), and a
// check that cannot read the working copy refuses the adoption instead. With the flag jj takes no
// working-copy lock either, so the adoption takes the workspace's locks itself
// (workspace_lock.go).

// adoptionArgs is the adoption's change of an undescribed working copy, in the one adoption the
// daemon asks a role's shim to run before a phase task (runAdoption): its author becomes the
// identity in the command's environment, and only while it is undescribed — a described working
// copy is a previous phase's work and keeps its author.
func adoptionArgs(jj, dir string) []string {
	return []string{jj, "--ignore-working-copy", "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", dir}
}

// describedArgs prints the working copy's commit id when it is described, and nothing otherwise.
func describedArgs(jj, dir string) []string {
	return []string{jj, "--ignore-working-copy", "log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", dir}
}

// exportHeadArgs rewrites @ with nothing changed (--force-rewrite: same tree, same author, same
// description), which writes one operation, and at every operation it writes in a workspace that
// shares its tree with git, jj points git's HEAD at @'s parent. runAdoption runs it where an
// operation of another jj command was merged in beside its change: the merge writes no git HEAD,
// so on the `jj new` branch git's HEAD can still name the described commit as it was before that
// command's snapshot rewrote it.
func exportHeadArgs(jj, dir string) []string {
	return []string{jj, "--ignore-working-copy", "metaedit", "--force-rewrite", "-r", "@", "-R", dir}
}

// adoptionRounds is how many times at most the adoption runs a change: once, and again for each
// time a jj command that wrote beside it overlapped it, up to this bound (runAdoption).
const adoptionRounds = 3

// runAdoption runs the adoption under the requested identity and budget, in the workspace the
// shim's environment names in LEGION_WORKSPACE and with PATH's jj (openWorkspace): the one the
// pane's own shell runs, since no runtime pins a jj on a pane or pod. Every role of an issue
// shares the workspace, so a working copy the previous role left described (its pushed commit) is
// kept as it is, and the incoming role starts on a fresh one of its own (`jj new`): otherwise its
// work would land in the previous role's commit, authored by the previous role's App. One read
// decides which of the two changes the working copy needs, and only that one runs: `jj metaedit
// --update-author` on an undescribed @ (adoptionArgs), `jj new` on a described one.
//
// No command of the adoption snapshots the working copy (--ignore-working-copy, above adoptionArgs),
// and that moves four things. An edit on disk that no jj command recorded is taken in by the role's
// next ordinary jj command, so on the `jj new` branch it lands in the incoming role's new commit,
// not in the described one. The adoption imports neither git's HEAD nor its refs, which a snapshot
// does, so a role's `git commit` reaches jj at its next ordinary command. A stale git index.lock
// fails the adoption only where its second command writes an operation, after which jj resets git's
// HEAD state and index (the metaedit's `Failed to reset Git HEAD state`); the read writes neither.
// And the commands take no jj working-copy lock, so a jj command that snapshots this same workspace
// is not held back by them: the adoption takes the workspace lock for its whole run, which keeps
// the daemon's workspace read and any other adoption out (lockWorkspace), and it waits for jj's
// working-copy lock before its first jj command and before the check after its change, so a
// snapshot that holds that lock commits first (awaitWorkingCopy).
//
// A stale working copy, which jj refuses every ordinary command in, is brought up to date first:
// the freshness check reads it with no snapshot (readFreshness), and only a working copy it reads
// as stale runs `jj workspace update-stale` (bringUpToDate), which leaves the edits that no jj
// command had recorded in a commit that is no longer checked out; the answer names each such
// commit, whatever the outcome. A check that cannot read the working copy, one of its jj commands
// having failed or printed what does not parse, writes one warning line and refuses the adoption,
// naming that check, with no update, and the daemon sends the task again at its next sweep: after
// an adoption's change and until the role's next jj command, @ was rewritten after the operation
// the working copy recorded, which jj finds fresh where the tree is kept, and an update would
// take the edits off the disk. The adoption's own change keeps @'s tree and labels, so where
// nothing else wrote, the check after it reads fresh. Something else can write all the same: a
// jj command another workspace runs, which can leave the working copy stale, and a jj command
// that snapshots this workspace, loaded the repository before the change and commits after it,
// which commits an operation beside the change's. The next load merges the two, and @'s change is
// then divergent: @ is the change's commit, while the working copy records the snapshot's tree,
// which reads stale, or @ is the snapshot's commit, which reads fresh and carries the author it
// had, or on the `jj new` branch @ is the described commit again. So the adoption's change is
// done only when the check after it reads the working copy fresh and @ undescribed and authored by
// the requested identity (adopted), which both changes produce. Where nothing wrote between the
// start of the round and the check after its change (the head operation is the one the change
// loaded: the round's first check's, or the update's), the change wrote nothing either: @ already
// was undescribed and authored by that identity, and the second read is left out. Otherwise the
// adoption runs again — the update of a stale working copy, the read, the one change — at most
// adoptionRounds times in all, within the one budget, and past that it refuses, naming what the
// last check found. A commit the first change wrote that an overlap left beside @ is left where it
// is: it holds no edit of its own, since the metaedit's commit has the tree @ had before the
// change and the commit of `jj new` is empty, and nothing the adoption reads tells it apart (its
// change answers no commit id on its standard output), so an abandon of it would be a guess.
//
// The merge of an overlap's operation writes no git HEAD either, so on the `jj new` branch git's
// HEAD can name the described commit as it was before the snapshot rewrote it, not @'s parent;
// the jj this was measured with (0.46.0-sami.20261127-111037) names git's HEAD in no revset or
// template. So once the change is done, the adoption reads whether an operation other than the
// change's came in on top of the head the round's change loaded (overlapped): the head is a
// merge, or its one parent is not that head. Where one did, the round's change is done but the
// adoption is not: the next round's change is exportHeadArgs's rewrite, which writes nothing in
// @ and has jj point git's HEAD at @'s parent, and the check runs once more, within the same
// bound of rounds. A round no other command wrote beside writes nothing more.
//
// The checks see an overlap only where the snapshot commits before the check after the change
// reads: a jj command of the role's own that loaded the repository before the change and takes
// jj's working-copy lock only after that check commits its operation after the adoption has
// answered, and the role's next jj command then meets a stale working copy or a divergent @
// (docs/site's troubleshooting.md, "A role's own jj command can still overlap the adoption").
//
// Every command runs in the workspace itself, never in the shim's own directory: jj reads the
// repository of the directory it runs in, whatever -R names, and migrates that repository's legacy
// configuration (workspace.RunCommand). Every one runs through the same run under the one budget,
// the update to that budget less a reserve the reads after it take (updateReserve), and the waits
// for the locks inside it too. A failure is answered in workspace.RunCommand's words, which name
// the command and its exit or its timeout, with jj's standard error: the daemon decides what a
// failed adoption means. A shim without LEGION_WORKSPACE was not started by a runtime, and
// refuses; a PATH without jj refuses naming jj and the PATH searched (openWorkspace).
func (s *shim) runAdoption(request shimwire.AdoptWorkingCopy) (edits []string, err error) {
	env := append(slices.Clone(s.cfg.Env), "JJ_USER="+request.JJUser, "JJ_EMAIL="+request.JJEmail)
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	opened, done, err := s.openWorkspace("adopt", env, budget)
	if err != nil {
		return nil, err
	}
	defer done()
	unlock, err := s.lockWorkspace(opened, "adoption")
	if err != nil {
		return nil, err
	}
	defer unlock()
	// check waits for jj's working-copy lock (when is where the wait's line says it waits) and
	// then reads whether the working copy is fresh. A check that cannot read that (readFreshness)
	// writes one warning line and refuses the adoption, both naming which check it is.
	check := func(when, which string) (freshness, error) {
		if err := s.awaitWorkingCopy(opened, when); err != nil {
			return freshness{}, err
		}
		state, err := readFreshness(opened.read)
		if err != nil {
			why := "the freshness check " + which + " could not read the working copy, so the adoption runs no jj workspace update-stale and refuses"
			s.log.Printf("[worker-shim] warning: %s: %q", why, shimwire.CutAnswerError(err.Error()))
			return freshness{}, fmt.Errorf("worker-shim: %s: %w", why, err)
		}
		return state, nil
	}
	state, err := check("before its first jj command", "before the adoption's change")
	if err != nil {
		return nil, err
	}
	dir, jj, run := opened.dir, opened.jj, opened.run
	var found string
	// exportHead is whether the next round's change is exportHeadArgs's rewrite: the last round's
	// change was done, and an operation of another jj command came in beside it.
	exportHead := false
	for range adoptionRounds {
		// base is the head operation the round's change loads: the one the check read, or on a
		// stale working copy the one the update left.
		base := state.head
		if !state.fresh {
			left, err := s.bringUpToDate(opened, state, budget-updateReserve(budget))
			edits = append(edits, left...)
			if err != nil {
				return edits, err
			}
			if base, err = opened.head(); err != nil {
				return edits, err
			}
		}
		change := exportHeadArgs(jj, dir)
		if !exportHead {
			described, err := run(nil, describedArgs(jj, dir)...)
			if err != nil {
				return edits, err
			}
			change = adoptionArgs(jj, dir)
			if described != "" {
				// --ignore-working-copy as above adoptionArgs: the new commit is empty, so it keeps @'s tree.
				change = []string{jj, "--ignore-working-copy", "new", "-R", dir}
			}
		}
		if _, err := run(nil, change...); err != nil {
			return edits, err
		}
		if state, err = check("before it checks the working copy after its change", "after the adoption's change"); err != nil {
			return edits, err
		}
		exportHead = false
		switch {
		case !state.fresh:
			found = "the working copy stale"
			continue
		case state.head == base:
			return edits, nil
		}
		if found, err = opened.adopted(request); err != nil {
			return edits, err
		}
		if found != "" {
			continue
		}
		merged, err := opened.overlapped(base)
		if err != nil || !merged {
			return edits, err
		}
		found, exportHead = "an operation of another jj command merged in beside the change, which leaves git's HEAD where it was", true
	}
	return edits, fmt.Errorf("worker-shim: the adoption ran its change %d times, and the check after the last found %s, not a fresh working copy whose @ is undescribed and authored by %s <%s>",
		adoptionRounds, found, request.JJUser, request.JJEmail)
}

// head reads the repository's head operation with no snapshot, as the freshness check does.
func (o openedWorkspace) head() (string, error) {
	head, err := o.read("op", "log", "-n1", "--no-graph", "-T", "id")
	if err != nil {
		return "", err
	}
	if !isHex(head) {
		return "", fmt.Errorf("jj op log -n1 -T id printed %q, which is no operation id", firstLine(head))
	}
	return head, nil
}

// overlapped reads the head operation and its parents with no snapshot and answers whether an
// operation of another jj command came in on top of base, the head the round's change loaded:
// the head is neither base nor an operation whose one parent is base, which the change's own is.
// A merge of two operations that ran beside each other is a head of two parents.
func (o openedWorkspace) overlapped(base string) (bool, error) {
	out, err := o.read("op", "log", "-n1", "--no-graph", "-T", `id ++ " " ++ parents.map(|p| p.id()).join(" ")`)
	if err != nil {
		return false, err
	}
	ids := strings.Fields(out)
	if len(ids) < 2 || slices.ContainsFunc(ids, func(id string) bool { return !isHex(id) }) {
		return false, fmt.Errorf("jj op log -n1 printed %q, which is no operation id followed by its parents'", firstLine(out))
	}
	head, parents := ids[0], ids[1:]
	return head != base && !(len(parents) == 1 && parents[0] == base), nil
}

// adoptedTemplate prints whether @ is described and its author's name and email, one `key=value`
// line each, so a name or an email that is empty still leaves its line.
const adoptedTemplate = `"described=" ++ if(description, "yes", "no") ++ "\nname=" ++ author.name() ++ "\nemail=" ++ author.email() ++ "\n"`

// adopted reads @ with no snapshot and answers what it found where @ is not what the adoption
// makes, an undescribed commit authored by the requested identity, and "" where it is.
func (o openedWorkspace) adopted(request shimwire.AdoptWorkingCopy) (found string, err error) {
	out, err := o.read("log", "-r", "@", "--no-graph", "-T", adoptedTemplate)
	if err != nil {
		return "", err
	}
	fields := map[string]string{}
	for line := range strings.Lines(out) {
		key, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		fields[key] = value
	}
	switch described, name, email := fields["described"], fields["name"], fields["email"]; {
	case described == "no" && name == request.JJUser && email == request.JJEmail:
		return "", nil
	case described == "yes":
		return "@ described", nil
	case described == "no":
		return fmt.Sprintf("@ authored by %s <%s>", name, email), nil
	default:
		return fmt.Sprintf("what jj printed for @, which names no description and author: %q", firstLine(out)), nil
	}
}
