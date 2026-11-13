package daemon

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// runningCopyPeriod is how long the cadence leaves between two copies of one issue's workspace,
// measured from the end of the copy before it and never from its start: a copy that took three
// minutes would otherwise have two of the next period's minutes already spent, and a copy slower
// than the period would run into the next one for ever. Five minutes is the bound the change
// holds itself to — the work a pod loses to an eviction is the work of the minutes since the last
// copy — and it is the cost as well, since each copy is a `git bundle create` in the workspace a
// role is working in.
//
// It is a constant and not a `legion.yaml` key on purpose. A deployment that had set a key could
// not go back to an earlier daemon release without editing its file first, and editing the file
// is exactly the one action a deployment takes when this change goes wrong.
const runningCopyPeriod = 5 * time.Minute

// runningCopyBudget is how long one copy is given before the daemon stops waiting for it. It is
// below the period rather than equal to it: a budget of a whole period would guarantee that a
// workspace slow enough to spend it has its next copy fall due at the moment the last one ends,
// and two runs of the shim's bundler in one workspace share the ref prefix `refs/legion-bundle/`,
// so one run's delete lands inside the other's `git bundle create`. The period less one tick of
// the daemon's own minute (idleTickInterval) leaves the next period a whole tick of room.
//
// The shim's own side is given the same budget, not one of its own: the runtime hands it the
// smaller of runtime.WorkspaceBundleTimeout and what is left of this context
// (sandbox's Runtime.BundleWorkspace), so a copy the daemon stops waiting for is a copy the shim
// stops building. A bundler left running past the daemon's wait would still hold the workspace
// when the next turn's end asked for a copy, which is the second run in one workspace the ref
// prefix cannot carry. The copy is abandoned with the reason logged all the same.
const runningCopyBudget = runningCopyPeriod - idleTickInterval

// The lines one copy writes are pinned as constants, as supervise's turn lines are: an
// operator and a production check read each whole, and a message reworded at one call site would
// answer that search with a part of the answer.
const (
	// runningCopyStoredLine is a copy taken and stored, at Info: the issue, the bytes it holds
	// and how long the copy took, the bundler's build and the write together.
	runningCopyStoredLine = "running copy: stored a copy of an issue workspace's unpushed work"
	// runningCopyDroppedLine is a workspace that bundled to nothing, at Info. The store refuses
	// an empty copy, so the issue's running copy is dropped instead: a copy taken minutes ago
	// holds work that has since been pushed, and the next pod must never restore stale work,
	// which is the rule the release bundle's own save follows (outbox.bundleIssueWorkspace).
	runningCopyDroppedLine = "running copy: the issue workspace holds nothing unpushed, so its running copy is dropped"
	// runningCopyUnsavedLine is a copy that was not taken or not stored, at Warn, under the
	// reason it failed by name. Such a copy holds nothing and leaves the issue with the copy it
	// had; the issue is tried again at the next period. A push's copy and an owed copy of an
	// issue with no live role write it at Debug instead (copyWorkspaceAtPush, takeRunningCopy).
	runningCopyUnsavedLine = "running copy: no copy of the issue workspace was stored"
	// runningCopyOvertakenLine is a copy that was being built when the daemon recorded a push of
	// the issue's branch, at Info. It is not stored: it may have read the workspace before the
	// pushed commits were made, and a copy stored after a push that does not hold it is the very
	// copy the restore's push rule exists to keep from a pod. The copy the push asked for is owed
	// and is taken as this one ends.
	runningCopyOvertakenLine = "running copy: a push of the issue's branch was recorded while this copy was being taken, so it is not stored and the copy the push asked for is taken next"
	// runningCopyStoppedLine is a copy the daemon's own stop ended, at Debug: no copy was stored,
	// nothing failed, and the next daemon takes the issue's copies again.
	runningCopyStoppedLine = "running copy: the daemon is stopping, so this copy of the issue workspace ends unstored"
)

// The reasons runningCopyUnsavedLine carries. They are names rather than sentences because a
// deployment counts them: a cadence that stores nothing is read from which of these it writes.
const (
	// runningCopyClaimsUnread: the daemon could not read its own claims, so no role was chosen.
	runningCopyClaimsUnread = "claims_unread"
	// runningCopyNoRole: the issue has no live role with a process to ask. Only a shim can build
	// the bundle, in the workspace it already works in. A push's copy writes it at Debug, since a
	// push of a released or finished issue meets it every time (copyWorkspaceAtPush), and so does
	// an owed copy, whose role went after everything that owed it found one (takeRunningCopy).
	runningCopyNoRole = "no_live_role"
	// runningCopyBundlerFailed: the bundler answered an error.
	runningCopyBundlerFailed = "bundler_failed"
	// runningCopyBudgetSpent: the bundler was still building at runningCopyBudget and the daemon
	// stopped waiting for it.
	runningCopyBudgetSpent = "budget_spent"
	// runningCopyTooLarge: the workspace's unpushed work is over runtime.MaxWorkspaceBundleBytes,
	// the bound a saved copy carries.
	runningCopyTooLarge = "bundle_too_large"
	// runningCopySaveFailed: the store did not take the copy.
	runningCopySaveFailed = "save_failed"
	// runningCopyDropFailed: the store did not drop the copy of a workspace that bundled to
	// nothing, which leaves the issue holding work that has since been pushed.
	runningCopyDropFailed = "drop_failed"
)

// runningCopies is what the cadence holds in memory: per issue, whether a copy of that issue is
// in flight and when the last one ended, and the context of the daemon's run, which every copy
// runs under.
//
// The count is per issue and never per role. One workspace belongs to one issue and the six roles
// of that issue share it, so six roles ending their turns in the same minute must take one bundle
// of that workspace and not six: two runs of the shim's bundler in one workspace share the ref
// prefix `refs/legion-bundle/`, and one run's delete would land inside the other's `git bundle
// create`. A release's save is such a run as well, and it takes the same place (hold).
//
// It is memory and not a table. A daemon that restarts has taken no copy in this process, so
// every issue in a turn is due at its first pass, which is the right answer: the daemon cannot
// know what the daemon before it copied, and a copy whose content equals the stored row's moves
// that row's time alone (store.PutRunningWorkspaceCopy). An entry is a few words per issue the
// daemon has copied since it started.
//
// The run's context is held here rather than handed down because the paths that start a copy are
// callbacks — a claim's durable state (copyWorkspaceAtTurnEnd) and a push fact's commit
// (copyWorkspaceAtPush) — that are given no context of their own. A copy that ran under none
// would outlive the daemon's stop: it would run to its budget against a store already closed and
// log a failed save for a copy nobody could have stored.
//
// It holds nothing of how a copy is logged. A copy chooses the role it asks before it takes the
// issue's place (takeRunningCopy), so one with no role to ask never takes the place, and each
// entry holds the order of the issue's copies alone: the place, the copy owed behind it, the push
// that overtook it, and the end the next period runs from.
type runningCopies struct {
	mu     sync.Mutex
	issues map[string]runningCopy
	run    context.Context
}

// runUnder makes ctx, the daemon's run, the context every copy is started under from now on.
func (c *runningCopies) runUnder(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run = ctx
}

// running is the daemon's run's context, and false before the run has started or once it has
// ended, when no copy is started at all.
func (c *runningCopies) running() (context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil || c.run.Err() != nil {
		return nil, false
	}
	return c.run, true
}

// runningCopy is one issue's place in the cadence.
type runningCopy struct {
	// owed is whether a copy of the issue was asked for while the one in flight was being built
	// and so is still to be taken: that copy read the workspace before the work which asked for
	// this one was written, so it holds none of that work.
	//
	// It is one flag and not a queue on purpose. Six roles ending their turns while one copy is
	// being built owe one copy between them and not six, because the copy taken after the one in
	// flight reads the workspace as it stands then and so holds the work of all six turns; a
	// queue would buy nothing but one `git bundle create` per turn end in a workspace a role is
	// working in.
	owed bool
	// pushed is whether the daemon recorded a push of the issue's branch while this copy was in
	// flight. Such a copy may have read the workspace before the pushed commits were made, so it
	// is not stored (takeRunningCopy), and the push's own copy, owed behind it, is.
	pushed bool
	// ended is when the last copy of the issue finished, whatever it stored; zero for an issue
	// this daemon has copied none of. The period is measured from it.
	ended time.Time
	// free is the issue's one place in flight: nil while nothing of the issue runs, made when a
	// copy or a release's save takes the place (take) and closed when that run gives it back
	// (giveBack), which is what a save waiting for the place waits on (hold).
	free chan struct{}
}

// inFlight is whether a copy of the issue or a release's save holds the issue's place.
func (held runningCopy) inFlight() bool { return held.free != nil }

// take takes issue's one place in flight if nothing holds it, and answers the issue's entry as it
// found it and whether it took the place. It is the one way the place is taken, by a copy (begin)
// and by a release's save (hold). The caller holds c.mu.
func (c *runningCopies) take(issue string) (runningCopy, bool) {
	if c.issues == nil {
		c.issues = make(map[string]runningCopy)
	}
	held := c.issues[issue]
	if held.inFlight() {
		return held, false
	}
	held.free = make(chan struct{})
	c.issues[issue] = held
	return held, true
}

// giveBack gives issue's place back at at, and answers the issue's entry as it found it, which
// says whether a copy was owed while the place was held. It is the one way the place is given
// back, by a copy (done) and by a release's save (hold): it wakes a save waiting for the place,
// starts the issue's next period at at with nothing owed and nothing overtaken, and drops the
// entry of every issue whose place is free and whose period has passed by at. The caller holds
// c.mu.
//
// Such an entry answers due exactly as a missing one does, so keeping it says nothing the map
// would not say without it, and a daemon that runs for weeks would hold one word per issue it
// has ever copied for ever. It is pruned where the map is written, as the runtime's two records
// of a pod's end are (sandbox's prunePodEnds), so nothing has to sweep it. The entry this call
// writes ends at at, so no period of its own has passed and it stays.
func (c *runningCopies) giveBack(issue string, at time.Time) runningCopy {
	held := c.issues[issue]
	if held.inFlight() {
		close(held.free)
	}
	c.issues[issue] = runningCopy{ended: at}
	maps.DeleteFunc(c.issues, func(_ string, held runningCopy) bool {
		return !held.inFlight() && at.Sub(held.ended) >= runningCopyPeriod
	})
	return held
}

// begin takes the issue's one place in flight for a copy, answering false when a copy of it or a
// release's save already holds it, and marking that refused copy owed so that the copy in flight
// takes it when it ends (done).
//
// A refused copy is owed and not dropped because the copy in flight read the workspace before
// the work that asked for this one was written. The copy which closes a turn is the case that
// decides it: neither the turn's end nor the cadence copies an issue between turns, and no push
// need come to copy it, so a turn's end that was dropped would leave the newest copy of a pod
// whose roles have all gone idle short of the work its last turn ended with, and an eviction of
// that idle pod would then lose the end of the turn — which is exactly what the daemon holds
// itself to (R1).
func (c *runningCopies) begin(issue string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, took := c.take(issue)
	if !took {
		held.owed = true
		c.issues[issue] = held
	}
	return took
}

// pushed marks the copy of issue in flight, if one is, as overtaken by a push of the issue's
// branch the daemon has just recorded (runningCopy.pushed).
func (c *runningCopies) pushed(issue string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.issues[issue]; ok && held.inFlight() {
		held.pushed = true
		c.issues[issue] = held
	}
}

// overtaken is whether the copy of issue in flight was overtaken by a push (pushed).
func (c *runningCopies) overtaken(issue string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues[issue].pushed
}

// hold takes the issue's one place in flight for a release's save, waiting while a copy holds
// it, and answers the call that gives it back; it answers ctx's error when ctx ends first. The
// save and a copy both run the shim's bundler in the issue's workspace, so the save must never
// run beside one, and only the timing of the two kept them apart before: a turn's end and the
// copy it was owed can take two copy budgets, which is more than a short `idle_minutes`.
//
// Giving the place back is a copy's own give-back (giveBack): it starts the issue's period and
// prunes as a copy's end does, and it drops any copy that was owed while the save held it. The
// save is the newer copy of the same workspace, taken once the roles had stopped, and it drops the
// issue's running copy as it is stored (store.PutWorkspaceBundle), so a copy taken after it would
// be a second, later copy of the same work in the other table, which the restore would then offer
// in place of the bundle and leave the bundle holding the issue's pod.
func (c *runningCopies) hold(ctx context.Context, issue string) (func(), error) {
	for {
		c.mu.Lock()
		held, took := c.take(issue)
		c.mu.Unlock()
		if took {
			return func() {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.giveBack(issue, time.Now())
			}, nil
		}
		select {
		case <-held.free:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// done gives the issue's place back and starts its next period at at (giveBack), and answers
// whether a copy of the issue is owed — one was asked for while this one was in flight, and the
// caller takes it now. Only the caller whose begin answered true calls it.
//
// The owed copy it reports is cleared by the reading: the copy the caller then starts takes the
// issue's place in flight again and is owed another only if a third is asked for while it runs,
// so a storm of turn ends is one copy after the one in flight rather than one copy per end.
func (c *runningCopies) done(issue string, at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.giveBack(issue, at).owed
}

// due is whether the cadence takes a copy of issue now: nothing of it is in flight, and the
// period has passed since the last copy ended. An issue this daemon has copied none of is due at
// once.
func (c *runningCopies) due(issue string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.issues[issue]
	return !held.inFlight() && now.Sub(held.ended) >= runningCopyPeriod
}

// tickWorkspaceCopies keeps a copy of each issue workspace's unpushed work outside its pod while
// the pod runs, on the daemon's own period (idleTickInterval), so the work a pod loses when the
// cluster ends it is the work of the minutes since the last copy rather than the work of the
// whole turn. It is the other half of the copy a turn's end takes (copyWorkspaceAtTurnEnd): the
// turn's end gives the copy that closes each turn, and this gives the one inside a turn long
// enough to outlast the period.
//
// It is a ticker of its own beside tickIdle and tickWakes, and it is not the idle release's: a
// deployment that releases no pod (`idle_minutes: 0`) still has pods the cluster can end under
// it, so the cadence is wired in attach rather than in attachIdleRelease. What turns it off is
// having nothing to ask or nowhere to put the answer — a runtime that implements no
// runtime.WorkspaceBundler, which is `runtime: tmux`, or a daemon with no store — and either
// leaves it running no ticker at all.
func (w *workflowRuntime) tickWorkspaceCopies(ctx context.Context) {
	if w.copyBundler == nil || w.copyStore == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = idleTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.copyWorkingWorkspaces(ctx)
		}
	}
}

// copyWorkingWorkspaces takes a copy of every issue of the project that is in a turn and whose
// period has passed. An issue between turns is left alone: the turn that ended took the copy that
// holds that work, and copying a workspace nobody is writing would store the same bundle again
// every period for as long as the pod stands.
//
// Which issues are in a turn is read from the claims the daemon's own store holds rather than
// from anything this process remembers, so a daemon that has just restarted reads it exactly as
// one that has been up for a day. Those same claims are what each copy chooses its role from, so
// the read is made once here and handed on (takeRunningCopy) rather than made again per issue.
//
// The idle release's gate on the Envoy listener (listenerLive) is not read here. That gate asks
// whether a released pod could be woken by a message, which is the release's question alone: a
// copy releases no pod and wakes none, it is taken while the pod runs, and a deployment whose
// listener has gone quiet is one whose pods keep working and whose work still has to be held
// against an eviction. What turns the cadence off is having nothing to ask or nowhere to put the
// answer, which tickWorkspaceCopies reads before it starts a ticker at all.
func (w *workflowRuntime) copyWorkingWorkspaces(ctx context.Context) {
	claims, err := w.copyStore.Claims(ctx)
	if err != nil {
		w.log.Warn("running copy: could not read the project's claims, so no issue was found to be in a turn", "error", err)
		return
	}
	now := time.Now()
	for _, issue := range issuesInTurn(claims, w.projectID) {
		if ctx.Err() != nil {
			return
		}
		if !w.copies.due(issue, now) {
			continue
		}
		w.startRunningCopy(issue, claims, slog.LevelWarn)
	}
}

// copyWorkspaceAtTurnEnd takes the copy that ends a turn: the claim of the issue has just gone
// durably idle, so what the role did in that turn is in the workspace and in no copy yet.
//
// It is the daemon's durable-state callback and runs on the machine's own goroutine, so it hands
// the copy to a goroutine of its own and returns at once, exactly as podResume does. Nothing of
// the turn waits for it: not the turn's end, not the send of the next task, not the claim's own
// state write, each of which the machine makes with this callback still running.
func (w *workflowRuntime) copyWorkspaceAtTurnEnd(c supervise.Claim, state supervise.ClaimState) {
	if state != supervise.StateIdle || w.copyBundler == nil || w.copyStore == nil {
		return
	}
	if c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	// It hands on no claims: this path holds the one claim whose turn ended and not the
	// project's, so the copy reads them itself.
	w.startRunningCopy(c.Issue, nil, slog.LevelWarn)
}

// copyWorkspaceAtPush takes a copy of issue's workspace because the daemon has just recorded a
// push of its branch: the workflow engine calls it once the push fact's transaction has committed
// (workflow.Config.PushRecorded). The restore of a new pod refuses every running copy older than
// the issue's newest recorded push (runtime.DecideWorkspaceRestore), because such a copy would put
// the workspace back on work behind the pushed commits; this is the copy that takes the refused
// one's place, taken after the push and so holding it and whatever the roles did beyond it. Update
// rounds make it the common case, since roles push just before their pod is replaced. No restore
// sets the issue's branch itself (runtime.RestorableBookmarks), so neither copy can move it.
//
// It goes through the cadence's own single flight (takeRunningCopy), so it never runs beside
// another copy or a release's save in the same workspace. A copy already in flight is marked
// overtaken first: it may have read the workspace before the pushed commits were made, so it is
// not stored, and this copy, owed behind it, is taken as it ends. It runs on a goroutine of its
// own like every other copy, so the fact's commit waits for nothing. The engine asks for it only
// for a push that moved the issue's push record (store.Store.RecordIssuePush), so a redelivered
// or late webhook neither discards the copy in flight nor bundles the workspace again.
//
// An issue with no live role to ask is logged at Debug for a push, where the turn's end and the
// cadence log it at Warn (takeRunningCopy). Every push of the branch that moves the record asks,
// whoever pushed and whatever the issue is doing, so a push of an issue that was released or has
// finished finds no role at all, and a Warn for each would bury the cadence's and the turn end's
// own, which are the ones a deployment counts: those paths ask only of an issue that had a role a
// moment ago. The role is read before the issue's place in flight is taken, so such a push's copy
// logs at once, beside a copy in flight as well, and is never owed behind it.
func (w *workflowRuntime) copyWorkspaceAtPush(issue string) {
	if w.copyBundler == nil || w.copyStore == nil || issue == "" {
		return
	}
	w.copies.pushed(issue)
	w.startRunningCopy(issue, nil, slog.LevelDebug)
}

// startRunningCopy hands one copy of issue's workspace to a goroutine of its own, under the
// budget one copy is given and inside the daemon's run (runningCopies.running), so the daemon's
// stop ends it. Nothing waits on it: a copy is a saving against an eviction, never a step of the
// turn, the tick or the fact that asked for it. claims is the caller's own read of the project's
// claims where it has one, and nil where it has not. It is how the owed copy is taken too
// (takeRunningCopy), so a copy owed to a turn's end holds up neither the copy that owed it nor
// anything else. Before the run starts and once it has ended no copy is started at all. noRole is
// the level the copy logs an issue with no live role to ask at (takeRunningCopy).
//
// The goroutine takes a hold mark of its own for the store's pool (store.ForConcurrentWorker),
// as every loop of the daemon's run does: copies of different issues run beside each other and
// beside those loops, and each reads the claims and writes its row through the pool, so a mark
// shared with any of them would have the pool refuse one its first connection while another held
// one, and an unmarked context would leave the pool unable to refuse a copy a second connection
// of its own. The run it is made from holds no connection, so a fresh mark sees everything.
func (w *workflowRuntime) startRunningCopy(issue string, claims []supervise.Claim, noRole slog.Level) {
	run, ok := w.copies.running()
	if !ok {
		w.log.Debug(runningCopyStoppedLine, "issue", issue)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(run), w.runningCopyBudget())
		defer cancel()
		w.takeRunningCopy(ctx, issue, claims, noRole)
	}()
}

// takeRunningCopy asks one live role of issue for the git bundle of its workspace's unpushed work
// and stores it as the issue's running copy — the same BundleWorkspace call an idle release's save
// makes (outbox.bundleIssueWorkspace), the same bound on its size, and the same answer to a
// workspace that bundles to nothing.
//
// claims is the project's claims as the caller already read them, which is what the role is
// chosen from (runningCopyRole); the tick reads them once for the whole pass and hands that one
// slice to every copy it starts, and a caller with none — the turn's end, which holds one claim
// and not the project's — passes nil and has them read here.
//
// The role is chosen first, and a copy with no role to ask, or no claims to choose one from, ends
// there, logged, without taking the issue's place in flight: it bundles nothing, so there is no
// bundler of it to keep apart from another and nothing for it to owe. noRole is the level it logs
// an issue with no live role at: Warn for the turn's end and the cadence, which ask of an issue
// that had a role a moment ago, and Debug for a push (copyWorkspaceAtPush) and for an owed copy.
// An owed copy is owed only by copies that found a live role before they asked, behind a copy in
// flight that found one as well, so one that finds none has met a pod that went in the moment
// since, and the Warn the turn's end and the cadence write is for an issue they found with none.
//
// A copy with a role takes the issue's one place in flight and holds it until the call has
// answered, so no second copy of the issue is ever being built: a copy abandoned at its budget
// while the shim went on building would leave exactly the two runs in one workspace that the ref
// prefix cannot carry. A copy refused by that mark is owed rather than dropped, and this call takes
// it as it returns: the copy in flight read the workspace before the work that asked for the
// refused one was written, and neither the turn's end nor the cadence copies an issue between
// turns, so a turn's end that was dropped would leave the newest copy of a pod whose roles have
// all gone idle short of the work its last turn ended with, and an eviction of that idle pod would
// lose the end of the turn (R1). The next period runs from the moment this returns.
//
// A copy a push of the issue's branch overtook while it was being built is not stored
// (copyWorkspaceAtPush): it may hold none of the pushed commits, and the push's own copy is owed
// behind it.
//
// Every way it can come to nothing is logged under its own reason and holds nothing: the issue
// keeps the copy it had, and the next period tries again. The daemon's own stop is not such a
// way: the run's context ending ends the copy, which is the stop and nothing an operator need
// read, so it is logged at Debug alone.
func (w *workflowRuntime) takeRunningCopy(ctx context.Context, issue string, claims []supervise.Claim, noRole slog.Level) {
	// unsaved logs a copy that came to nothing under its reason, or the daemon's stop where that
	// is what ended it.
	unsaved := func(args ...any) {
		if _, running := w.copies.running(); !running {
			w.log.Debug(runningCopyStoppedLine, "issue", issue)
			return
		}
		w.log.Warn(runningCopyUnsavedLine, append([]any{"issue", issue}, args...)...)
	}

	if claims == nil {
		read, err := w.copyStore.Claims(ctx)
		if err != nil {
			unsaved("reason", runningCopyClaimsUnread, "error", err)
			return
		}
		claims = read
	}
	role, ok := runningCopyRole(claims, w.projectID, issue)
	if !ok {
		w.log.Log(ctx, noRole, runningCopyUnsavedLine, "issue", issue, "reason", runningCopyNoRole)
		return
	}
	if !w.copies.begin(issue) {
		w.log.Debug("running copy: a copy of this issue's workspace is already being taken, so this one is owed and will be taken when that one ends", "issue", issue)
		return
	}
	defer func() {
		if !w.copies.done(issue, time.Now()) {
			return
		}
		// The owed copy is a copy like every other: a goroutine of its own and a budget of its
		// own, because nothing of a turn waits for a copy, under the daemon's run and not this
		// copy's context, which is cancelled the moment this call returns. It is handed no
		// claims — the claims this copy was given were read before the work that owed the copy
		// was written, and the role it asks is read as the turn's end reads it.
		w.startRunningCopy(issue, nil, slog.LevelDebug)
	}()
	// How long the copy took, read at whichever line is written: the bundler's own build and,
	// where one was made, the write that stored or dropped the row. A deployment reads it to see
	// a workspace growing slow long before a copy is abandoned at its budget.
	started := time.Now()
	took := func() string { return time.Since(started).Round(time.Millisecond).String() }
	bundle, err := w.copyBundler.BundleWorkspace(ctx, issue, role)
	if err != nil {
		reason := runningCopyBundlerFailed
		if errors.Is(err, context.DeadlineExceeded) {
			reason = runningCopyBudgetSpent
		}
		unsaved("role", role, "reason", reason, "budget", w.runningCopyBudget().String(), "took", took(), "error", err)
		return
	}
	switch size := bundle.Size(); {
	case size > runtime.MaxWorkspaceBundleBytes:
		w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopyTooLarge,
			"bytes", size, "max", runtime.MaxWorkspaceBundleBytes, "took", took())
	case size == 0:
		if err := w.copyStore.DeleteRunningWorkspaceCopy(ctx, issue); err != nil {
			unsaved("role", role, "reason", runningCopyDropFailed, "took", took(), "error", err)
			return
		}
		w.log.Info(runningCopyDroppedLine, "issue", issue, "role", role, "bytes", 0, "took", took())
	case w.copies.overtaken(issue):
		w.log.Info(runningCopyOvertakenLine, "issue", issue, "role", role, "bytes", size, "taken", bundle.At, "took", took())
	default:
		if err := w.copyStore.PutRunningWorkspaceCopy(ctx, issue, bundle); err != nil {
			unsaved("role", role, "reason", runningCopySaveFailed, "bytes", size, "took", took(), "error", err)
			return
		}
		w.log.Info(runningCopyStoredLine, "issue", issue, "role", role, "bytes", size, "took", took())
	}
}

// issuesInTurn is every issue of the project with a turn in flight, each named once, in the order
// the claims were read. A claim in supervise.StateWorking is a role whose agent is in a turn, and
// the cadence copies that issue's workspace and no other: the check the issue is held to is that
// no copy is taken between turns.
func issuesInTurn(claims []supervise.Claim, project string) []string {
	var issues []string
	seen := make(map[string]bool)
	for _, c := range claims {
		if c.Project != project || c.Issue == "" || c.Role == claim.RoleController {
			continue
		}
		if c.State != supervise.StateWorking || seen[c.Issue] {
			continue
		}
		seen[c.Issue] = true
		issues = append(issues, c.Issue)
	}
	return issues
}

// runningCopyRole is the role the copy is asked of: the first live role of the issue with a
// registered agent and a running process (supervise.HoldsCapability), which is what a shim is.
//
// The order is the order the store reads the claims in, by claim token, which is the order the
// idle release's own view reads them in and so the order of the roles its decision carries
// (workflow.IdleRelease); the release's save asks the first of those (decision.Roles[0]). One
// workspace is shared by every role of the issue, so which of them answers decides nothing but
// whose process does the work — and the two paths choosing the same way means a deployment reads
// one role's log for both.
func runningCopyRole(claims []supervise.Claim, project, issue string) (claim.Role, bool) {
	for _, c := range claims {
		if c.Project != project || c.Issue != issue || c.Role == claim.RoleController {
			continue
		}
		if supervise.HoldsCapability(c.State) {
			return c.Role, true
		}
	}
	return "", false
}

// runningCopyBudget is how long one copy is given, the production constant unless a test
// shortened it.
func (w *workflowRuntime) runningCopyBudget() time.Duration {
	if w.copyBudget > 0 {
		return w.copyBudget
	}
	return runningCopyBudget
}
