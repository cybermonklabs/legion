package daemon

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// runningCopyPeriod is the most the cadence leaves between the starts of two copies of one
// issue's workspace inside a turn, when the daemon's minute tick (idleTickInterval) arrives on
// time. It is counted from the start of the copy before it, and an issue is due once less than
// one tick of it is left (due), so the tick that starts the next copy comes at most one period
// after the last copy started, and never less than a period less one tick after it. A copy holds
// the workspace as it was when the copy started, so the work an eviction loses is the work since
// the last copy's start, which is what the period bounds; counted from the end of the copy
// instead, a copy that took two minutes would put the next start seven minutes or more after its
// own. Counting from the start runs no copy into the next: a copy's budget ends it by the time the
// next can fall due (runningCopyBudget), and the issue's one place in flight keeps a second copy
// from starting beside it whatever the timing (runningCopies). Five minutes is the bound the change
// holds itself to, and it is the cost as well, since each copy is a `git bundle create` in the
// workspace a role is working in.
//
// It is a constant and not a `legion.yaml` key on purpose. A deployment that had set a key could
// not go back to an earlier daemon release without editing its file first, and editing the file
// is exactly the one action a deployment takes when this change goes wrong.
const runningCopyPeriod = 5 * time.Minute

// runningCopyBudget is how long one copy is given before the daemon stops waiting for it. It is
// the period less one tick of the daemon's own minute (idleTickInterval), which is how long after
// a copy's start the next copy of its issue can first fall due (due). A copy that spends its whole
// budget has therefore ended, and given the issue's place back, by the first tick that finds the
// issue due, so the next copy starts on that tick and the period's bound holds for a slow
// workspace as for a quick one, with no two copies running at once. A budget of a whole period
// would have such a copy still holding the place at that tick, and its next copy would start a
// whole tick past the bound, since the place keeps the next copy from starting beside it: two runs
// of the shim's bundler in one workspace share the ref prefix `refs/legion-bundle/`, and one run's
// delete lands inside the other's `git bundle create`.
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
	// runningCopyStoredLine is a copy taken and stored, at Info: the issue, the bytes it holds,
	// `taken`, the store's time the copy's take began (store.WorkspaceCopyBegun), which is the
	// copy's own time, and how long the copy took, the bundler's build and the write together. The
	// cadence's period runs on its own clock from runningCopy.started, which the copy's take sets a
	// moment before the store's time is read, so `taken` stands close to the period's start. The
	// line's own timestamp is when the copy ended, so a reader measuring the cadence reads `taken`
	// and not the line's time.
	runningCopyStoredLine = "running copy: stored a copy of an issue workspace's unpushed work"
	// runningCopyDroppedLine is a workspace that bundled to nothing, at Info. The store refuses
	// an empty copy, so the issue's running copy is dropped instead: a copy taken minutes ago
	// holds work that has since been pushed, and the next pod must never restore stale work,
	// which is the rule the release bundle's own save follows (outbox.bundleIssueWorkspace).
	runningCopyDroppedLine = "running copy: the issue workspace holds nothing unpushed, so its running copy is dropped"
	// runningCopyUnsavedLine is a copy that was not taken or not stored, at Warn, under the
	// reason it failed by name. Such a copy holds nothing and leaves the issue with the copy it
	// had; the issue is tried again at the next period, between turns as well, once a role of it
	// is live (runningCopy.retry), unless the copy was refused for its size (runningCopyTooLarge).
	// A push's copy of an issue with no live role writes it at Debug instead (copyWorkspaceAtPush,
	// takeRunningCopy), and a copy off a pod an idle release has saved its bundle off writes it at
	// Info, since nothing failed (runningCopyReleaseBundled), as do a copy the store refuses because
	// the issue's running copy was taken after it (runningCopyNewerStands) and a copy of an issue
	// the daemon's project does not record (runningCopyNotRecorded).
	runningCopyUnsavedLine = "running copy: no copy of the issue workspace was stored"
	// runningCopyOvertakenLine is a copy that was being built when the daemon recorded a push of
	// the issue's branch, at Info. It is not stored: it may have read the workspace before the
	// pushed commits were made, and a copy stored after a push that does not hold it is the very
	// copy the restore's push rule exists to keep from a pod. The copy the push asked for is owed
	// and is taken as this one ends, or, where it came to nothing before it could be owed, the
	// issue owes a retry in its place (runningCopy.missed).
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
	// the bundle, in the workspace it already works in. A push's copy is the one copy that writes
	// it, at Debug, since a push of a released or finished issue meets it every time
	// (copyWorkspaceAtPush). A turn's end and the copy owed behind one in flight ask the role they
	// were handed and read no claims (takeRunningCopyOf, copyAsk), and the cadence asks only of an
	// issue with a live role in the claims it read and hands its copy those claims, so neither
	// meets it (copyWorkingWorkspaces).
	runningCopyNoRole = "no_live_role"
	// runningCopyClockUnread: the daemon could not read the store's clock as the copy's take
	// began, which is the copy's time (store.Store.BeginWorkspaceCopy), so it asked no shim.
	runningCopyClockUnread = "clock_unread"
	// runningCopyBundlerFailed: the bundler answered an error.
	runningCopyBundlerFailed = "bundler_failed"
	// runningCopyBudgetSpent: the bundler was still building at runningCopyBudget and the daemon
	// stopped waiting for it.
	runningCopyBudgetSpent = "budget_spent"
	// runningCopyTooLarge: the workspace's unpushed work is over runtime.MaxWorkspaceBundleBytes,
	// the bound a saved copy carries. The shim refuses such a bundle itself and names its size,
	// and the worker stream's connection answers that refusal, and a bundle that arrives whole
	// over the bound all the same, as runtime.WorkspaceBundleTooLargeError
	// (stream.Conn.BundleWorkspace), whose size the line carries as `bytes`. It is no fault to
	// retry, so it owes no retry (runningCopy.retry): the work stays over the bound until a role
	// pushes or drops some of it, and a retry between turns would have the shim build the same
	// refused bundle every period for as long as the pod stands. The cadence asks again at its
	// next period while a turn of the issue runs, and a turn's end and a push ask as they come.
	runningCopyTooLarge = "bundle_too_large"
	// runningCopySaveFailed: the store did not take the copy.
	runningCopySaveFailed = "save_failed"
	// runningCopyDropFailed: the store did not drop the copy of a workspace that bundled to
	// nothing, which leaves the issue holding work that has since been pushed.
	runningCopyDropFailed = "drop_failed"
	// runningCopyReleaseBundled: an idle release of the issue has saved its bundle off the pod
	// the copy would be taken off, and that bundle is the pod's last copy, so no copy is taken
	// off that pod (takeRunningCopyOf, store.Store.ReleaseBundleSavedOff). It is written at Info:
	// it is no fault, and it is not one of the reasons above that a deployment counts. The pod's
	// roles are still live between the release's save and their stops, and a push the daemon
	// records then asks them for a copy.
	runningCopyReleaseBundled = "release_bundle_saved"
	// runningCopyBundleUnread: the daemon could not read whether the issue's release bundle came
	// off the pod the copy would be taken off, so it asked no shim of that pod.
	runningCopyBundleUnread = "release_bundle_unread"
	// runningCopyNewerStands: the issue's running copy was taken after this copy's take began —
	// a cancelled release or a confirmed restore wrote it at that move, or a copy of another pod
	// came first — and a copy never replaces a newer one, nor drops one
	// (store.ErrNewerCopyStands, store.Store.PutRunningWorkspaceCopy,
	// store.Store.DeleteRunningWorkspaceCopy). A workspace that bundled to nothing writes it with
	// `bytes` 0: it says what the workspace held as its take began, and the row holds work read
	// after that. It is written at Info: it is no fault, and it is not one of the reasons above
	// that a deployment counts.
	runningCopyNewerStands = "newer_copy_stands"
	// runningCopyNotRecorded: the daemon's project does not record the issue, so the store wrote
	// no copy of it and dropped none (store.ErrIssueNotRecorded,
	// store.Store.PutRunningWorkspaceCopy, store.Store.DeleteRunningWorkspaceCopy). The cadence
	// copies a pod of every live claim of the project (runningCopyClaim), and an operator's spawn
	// of an issue another project records is such a claim: on a database the two projects'
	// daemons share, the spawn's copy would replace the running copy of the project that records
	// the issue, and its empty bundle would delete it. It is written at Info: it is no fault, it
	// is not one of the reasons above that a deployment counts, and it owes no retry, since a
	// retry would be refused the same way.
	runningCopyNotRecorded = "issue_not_recorded"
)

// tickWorkspaceCopies keeps a copy of each issue workspace's unpushed work outside its pod while
// the pod runs, on the daemon's own period (idleTickInterval), so the work a pod loses when the
// cluster ends it is the work of the minutes since the last copy rather than the work of the
// whole turn. It is the other half of the copy a turn's end takes (copyWorkspaceAtTurnEnd): the
// turn's end gives the copy that closes each turn, and this gives the one inside a turn long
// enough to outlast the period, and takes again, between turns as well, the copy of an issue whose
// last copy came to nothing (copyWorkingWorkspaces).
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
	ticker := time.NewTicker(w.copyTick())
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

// copyWorkingWorkspaces takes a copy of every issue of the project that is in a turn and is due,
// and of every issue whose last copy came to nothing, that is due and that has a live role in the
// claims it read, whatever its roles are doing (runningCopy.retry). An issue between turns whose
// last copy was stored is left alone: the turn that ended took the copy that holds that work, and
// copying a workspace nobody is writing would store the same bundle again every period for as
// long as the pod stands. One whose last copy came to nothing — a turn's end whose bundler failed
// or whose write failed, above all — holds no copy of the turn that ended, so it is taken again
// each period until a copy of it is stored or settled, and an eviction of its idle pod then
// restores the end of the turn.
//
// The pass reads one list: the issues in a turn, then every issue owing a retry that is not among
// them. It takes a copy of each that is due and has a live role in the claims it read, chosen as
// the copy itself chooses the role it asks (runningCopyClaim); an issue in a turn always has one,
// since the role in the turn is live. A retry so waits for a live role of its issue. An issue with
// none — released, or with every role stopped — has no running pod whose work a copy could hold,
// so the pass skips it with no line and it keeps its retry, which runs at the first due tick after
// a resumed or relaunched pod's role is live again. The pass also forgets every issue no claim of
// the project names at all (forgetUnclaimed), so the cadence's memory does not grow with every
// issue that ever failed a copy.
//
// Which issues are in a turn is read from the claims the daemon's own store holds rather than
// from anything this process remembers, so a daemon that has just restarted reads it exactly as
// one that has been up for a day. Those same claims are what each copy chooses its role from, so
// the read is made once here and handed on (takeRunningCopy) rather than made again per issue.
// The cadence's clock is read once for the pass, and the tick due allows for is the period the
// ticker runs at (copyTick).
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
		w.log.Warn("running copy: could not read the project's claims, so no issue was copied at this tick", "error", err)
		return
	}
	claimed := make(map[string]bool)
	for _, c := range claims {
		if c.Project == w.projectID && c.Issue != "" {
			claimed[c.Issue] = true
		}
	}
	w.copies.forgetUnclaimed(claimed)
	now, tick := w.copies.clock(), w.copyTick()
	issues := issuesInTurn(claims, w.projectID)
	for _, issue := range w.copies.retries() {
		if !slices.Contains(issues, issue) {
			issues = append(issues, issue)
		}
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		if !w.copies.due(issue, now, tick) {
			continue
		}
		if _, live := runningCopyClaim(claims, w.projectID, issue); !live {
			continue
		}
		w.startRunningCopy(issue, claims)
	}
}

// copyWorkspaceAtTurnEnd takes the copy that ends a turn (supervise.Deps.CopyTurnEnd): the
// machine of c calls it at every end of c's turn, so what the role did in that turn is in the
// workspace and in no copy yet. It is called from the turn's end itself and not from the claim
// going durably idle: without the copy started at the turn's end, a turn that ended into a held
// stop — a project's pause, an operator's drain, a tree's linger — would be stopped before any
// idle report, and its end would be in no copy.
//
// c is the claim whose turn ended, as the machine holds it at that end: its role and the locator
// of the process the turn ran in. The copy asks that role's shim and reads no claims to find a
// role (takeRunningCopyOf), so a stop that lands before the copy's goroutine has run cannot leave
// it with no live role to ask, which a read of the claims made then would find. The ask says it
// is a turn's end (copyAsk.turnEnd), so a copy owed behind one in flight asks this role whatever
// else asked for it. A stop of the role, held for the turn's end or asked while the copy is taken,
// waits for the copy besides: finished is how the machine hears the copy has ended, whatever it
// came to — stored, refused, failed or abandoned at its budget — and it is called once, from the
// copy's own goroutine, never from this call; nil tells nobody, and the ask is a turn's end all
// the same. The daemon's supervisor makes it of the machine's own event (supervisor.copyTurnEnd).
// The copy owed behind one in flight that stands for this one calls it in its place (copyAsk). It
// answers the copy's budget, which the machine counts from the turn's end as the bound of the
// stop's wait, and false where it starts no copy: a controller's claim, a claim of no issue, a
// daemon that bundles no workspace or keeps no store, and a daemon whose run has ended
// (startRunningCopyOf). The copy's own budget runs from its start, which is the turn's end for a
// copy started at once and later for one owed behind another copy or kept until the daemon's run
// starts (goCopy); such a copy can outlast the wait, and then asks a role the stop has taken.
//
// It runs on the machine's own goroutine, inside the decision, so it hands the copy to a
// goroutine of its own and returns at once, exactly as podResume does. Nothing of the turn waits
// for it: not the turn's end, not the send of the next task, not the claim's own state write.
func (w *workflowRuntime) copyWorkspaceAtTurnEnd(c supervise.Claim, finished func()) (time.Duration, bool) {
	if w.copyBundler == nil || w.copyStore == nil || c.Issue == "" || c.Role == claim.RoleController {
		return 0, false
	}
	ask := copyAsk{of: c, turnEnd: true}
	if finished != nil {
		ask.finished = []func(){finished}
	}
	if !w.startRunningCopyOf(ask) {
		return 0, false
	}
	return w.runningCopyBudget(), true
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
// or late webhook neither discards the copy in flight nor bundles the workspace again. A push the
// daemon records while an idle release runs, between its save and its role stops, takes no copy:
// the release's bundle is that pod's last copy (takeRunningCopy).
//
// An issue with no live role to ask is logged at Debug for a push (takeRunningCopy). Every push of
// the branch that moves the record asks, whoever pushed and whatever the issue is doing, so a push
// of an issue that was released or has finished finds no role at all, and a Warn for each would
// bury the lines a deployment counts. The push is the one copy that reads the claims for its role
// after the fact: the turn's end hands its copy the claim whose turn ended, the copy owed behind
// one in flight asks the role it was owed with (copyAsk), and the cadence asks only of an issue
// with a live role in the claims it read. The role is read before the issue's place in flight is
// taken, so such a push's copy logs at once, beside a copy in flight as well, and is never owed
// behind it. It owes a retry all the same, which the cadence takes once a role of the issue is live
// (copyWorkingWorkspaces). Beside a copy in flight it marks that retry on the issue's place
// (runningCopies.untaken), and the copy in flight, which this push overtook and which is not
// stored, leaves it owed as it ends: nothing else follows that copy.
func (w *workflowRuntime) copyWorkspaceAtPush(issue string) {
	if w.copyBundler == nil || w.copyStore == nil || issue == "" {
		return
	}
	w.copies.pushed(issue)
	w.startRunningCopy(issue, nil)
}

// startRunningCopy hands one copy of issue's workspace to a goroutine of its own, to ask the role
// it chooses from the project's claims (takeRunningCopy): the cadence's, which hands the claims
// it read, and a push's, which hands nil and has them read there.
func (w *workflowRuntime) startRunningCopy(issue string, claims []supervise.Claim) {
	w.goCopy(issue, func(ctx context.Context) { w.takeRunningCopy(ctx, issue, claims) })
}

// startRunningCopyOf hands one copy of the workspace of ask's issue to a goroutine of its own, to
// ask the role of ask's claim (takeRunningCopyOf): a turn's end, which hands the claim whose turn
// ended, and the copy owed behind one in flight or behind a release's save (startOwedCopy). The
// ends of the turns ask stands for are told once the copy has ended. It answers false where it
// starts nothing, which is a daemon whose run has ended, and those ends are then told nothing; a
// copy asked for before the run has started is started as the run starts (goCopy).
func (w *workflowRuntime) startRunningCopyOf(ask copyAsk) bool {
	return w.goCopy(ask.of.Issue, func(ctx context.Context) { w.takeRunningCopyOf(ctx, ask) })
}

// goCopy runs take, one copy of issue's workspace, on a goroutine of its own, under the budget one
// copy is given and inside the daemon's run (runningCopies.startUnderRun), so the daemon's stop
// ends it, and answers whether it started it. Nothing waits on it: a copy is a saving against an
// eviction, never a step of the turn, the tick or the fact that asked for it, and a stop held for
// a turn's end waits on it through the machine's own event, never on this goroutine
// (copyWorkspaceAtTurnEnd). It is how the owed copy is taken too (startOwedCopy), so a copy owed
// to a turn's end holds up neither the copy that owed it nor anything else. A copy asked for
// before the run starts is kept and started as the run starts, with its budget counted from then:
// the boot restores each claim's machine before the run begins, and a turn's end in that window
// would otherwise take no copy and owe none, so an issue idle from then on would keep the copy
// from before that turn. Once the run has ended no copy is started at all.
//
// The goroutine takes a hold mark of its own for the store's pool (store.ForConcurrentWorker),
// as every loop of the daemon's run does: copies of different issues run beside each other and
// beside those loops, and each reads the claims and writes its row through the pool, so a mark
// shared with any of them would have the pool refuse one its first connection while another held
// one, and an unmarked context would leave the pool unable to refuse a copy a second connection
// of its own. The run it is made from holds no connection, so a fresh mark sees everything.
func (w *workflowRuntime) goCopy(issue string, take func(context.Context)) bool {
	started := w.copies.startUnderRun(func(run context.Context) {
		go func() {
			ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(run), w.runningCopyBudget())
			defer cancel()
			take(ctx)
		}()
	})
	if !started {
		w.log.Debug(runningCopyStoppedLine, "issue", issue)
	}
	return started
}

// startOwedCopy takes the copy that was owed while a copy in flight or a release's save held its
// issue's place (runningCopies.done, runningCopies.hold), once that place is given back. The owed
// copy is a copy like every other: a goroutine of its own and a budget of its own, because nothing
// of a turn waits for a copy, under the daemon's run and not the context of what held the place,
// which ends as it gives the place back. It asks the role it was owed with (copyAsk) — the role
// of the last turn's end that owed it, whose stop waits for it — and reads no claims to find one.
// A stop of that role, held for its turn's end or asked while the copy is being taken, waits for
// this copy, so the role is still there to ask while the wait lasts: at most the copy's budget from
// the turn's end and never past the stop's own deadline, an operator's stop timeout above all,
// which the copy in flight or a release's save ahead of this one can outlast. A suspension that may
// not be held (supervise.RequestSuspend.NeverHold), the fresh session's, waits for no copy. A copy
// whose role a stop took first fails as a copy of any stopped role does (runningCopyBundlerFailed),
// keeps the copy the issue had and owes a retry. The turn's ends it stands for are told once it
// has ended, and at once where the daemon's run has ended and it can no longer start, since no
// copy is coming to tell them later.
func (w *workflowRuntime) startOwedCopy(owed copyAsk) {
	if !w.startRunningCopyOf(owed) {
		owed.tellCopied()
	}
}

// takeRunningCopy takes the copy of issue's workspace that the cadence or a push asked for,
// choosing the role it asks from the project's claims (runningCopyClaim) and asking it through
// takeRunningCopyOf, which says what the copy then is.
//
// claims is the project's claims as the caller already read them; the tick reads them once for
// the whole pass and hands that one slice to every copy it starts, and a push, which holds no
// claims, passes nil and has them read here. The role is chosen first, and a copy with no role to
// ask, or no claims to choose one from, ends there, logged, without taking the issue's place in
// flight: it bundles nothing, so there is no bundler of it to keep apart from another and nothing
// to be owed behind it. It starts the issue's period and owes a retry all the same, which it
// marks on the run that holds the place where one does (runningCopies.untaken). An issue with no
// live role is logged at Debug (copyWorkspaceAtPush): only a push meets it, since the cadence asks
// only of an issue it found a live role of in the claims it hands, and this chooses from those
// same claims.
func (w *workflowRuntime) takeRunningCopy(ctx context.Context, issue string, claims []supervise.Claim) {
	if claims == nil {
		read, err := w.copyStore.Claims(ctx)
		if err != nil {
			w.copies.untaken(issue, true)
			w.unsavedCopy(issue, "reason", runningCopyClaimsUnread, "error", err)
			return
		}
		claims = read
	}
	chosen, ok := runningCopyClaim(claims, w.projectID, issue)
	if !ok {
		w.copies.untaken(issue, true)
		w.log.Debug(runningCopyUnsavedLine, "issue", issue, "reason", runningCopyNoRole)
		return
	}
	w.takeRunningCopyOf(ctx, copyAsk{of: chosen})
}

// takeRunningCopyOf asks the role of ask's claim for the git bundle of its issue's workspace's
// unpushed work and stores it as the issue's running copy (storeRunningCopy) — the same
// BundleWorkspace call an idle release's save makes (outbox.bundleIssueWorkspace), the same bound
// on its size, and the same answer to a workspace that bundles to nothing. ask's claim is the
// claim of the role the copy asks — the claim whose turn ended, the claim an owed copy was owed
// with, or the claim the cadence or a push chose (takeRunningCopy) — and its locator names the pod
// the copy would be taken off. The copy reads no claims to find a role of its own: a turn's end
// hands it the claim before any stop held for that end can take the role, so the role it asks is
// the one whose stop waits for it.
//
// ask.finished is the end of each turn this copy stands for (copyWorkspaceAtTurnEnd), and each is
// told once the copy has ended, whatever it came to, so the stop held for that end waits no
// longer. A copy owed behind one in flight carries them on, and the owed copy tells them as it
// ends (copyAsk); the copy in flight tells its own once it has given the issue's place back and
// started the owed one.
//
// A copy takes the issue's one place in flight and holds it until the call has answered, so no
// second copy of the issue is ever being built: a copy abandoned at its budget while the shim went
// on building would leave exactly the two runs in one workspace that the ref prefix cannot carry.
// A copy refused by that mark is owed rather than dropped, and this call takes it as it returns:
// the copy in flight read the workspace before the work that asked for the refused one was
// written, and the cadence takes no copy between turns of an issue whose last copy was stored, so
// a turn's end that was dropped would leave the newest copy of a pod whose roles have all gone
// idle short of the work its last turn ended with, and an eviction of that idle pod would lose the
// end of the turn (R1). The next period runs from the moment the copy took the place, and what the
// copy came to is the retry the issue owes as it gives the place back (storeRunningCopy,
// runningCopies.done).
//
// A copy off a pod an idle release has saved its bundle off is refused, before the issue's place
// is taken and the shim is asked, and again by the store's write: once an idle release has saved
// its bundle, that bundle is the pod's last copy. The release's rows run the save first, then a
// stop per role, then the teardown (idleReleaseRows), so the pod's roles are still live after the
// save, and a copy asked of them then — the one a push the daemon records in that window asks
// for above all — would be stored as the later of the issue's two copies, offered to the next pod
// in the bundle's place, and the bundle's row would hold the issue's pod from every idle release
// after it (workflow.IdleHoldBundleUnrestored). The read names the pod asked's locator records,
// which is the pod the runtime's bundler names the copy by (sandbox's Runtime.BundleWorkspace), so
// no bundle is built off that pod; the write refuses by the pod the copy itself names
// (store.ErrReleaseBundleSaved), which also refuses a copy whose read ran before the save and
// whose place was taken after the save gave it back, and a copy asked of a role whose locator
// names no pod. The two are one rule (store.Store.ReleaseBundleSavedOff), and each logs it at Info
// under a reason of its own (runningCopyReleaseBundled). Every copy is taken here — the cadence's
// (copyWorkingWorkspaces), a turn's end (copyWorkspaceAtTurnEnd), a push's (copyWorkspaceAtPush)
// and the copy owed behind one in flight (done) or behind a release's save (runningCopies.hold) —
// so each meets it.
func (w *workflowRuntime) takeRunningCopyOf(ctx context.Context, ask copyAsk) {
	issue, role := ask.of.Issue, ask.of.Role
	// A locator that names no pod leaves the refusal to the store's write, which reads the pod
	// the copy itself names.
	if pod := podOf(ask.of.Locator); pod != "" {
		saved, err := w.copyStore.ReleaseBundleSavedOff(ctx, issue, pod)
		if err != nil {
			w.copies.untaken(issue, true)
			w.unsavedCopy(issue, "role", role, "reason", runningCopyBundleUnread, "pod", pod, "error", err)
			ask.tellCopied()
			return
		}
		if saved {
			w.copies.untaken(issue, false)
			w.log.Info(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopyReleaseBundled, "pod", pod)
			ask.tellCopied()
			return
		}
	}
	if !w.copies.begin(ask) {
		w.log.Debug("running copy: a copy of this issue's workspace is already being taken, so this one is owed and will be taken when that one ends", "issue", issue)
		return
	}
	if owed := w.copies.done(issue, w.storeRunningCopy(ctx, issue, role)); owed != nil {
		w.startOwedCopy(*owed)
	}
	ask.tellCopied()
}

// storeRunningCopy asks role's shim for the bundle of issue's workspace and stores it as the
// issue's running copy, while the copy takeRunningCopyOf started holds the issue's place, and
// answers whether the issue owes a retry (runningCopy.retry), which the place's give-back marks
// (runningCopies.done). Each way it ends writes its own line before it answers.
//
// A copy a push of the issue's branch overtook while it was being built is not stored
// (copyWorkspaceAtPush): it may hold none of the pushed commits. It owes no retry of its own,
// since the push's own copy settles what the push asked for: it is owed behind this one, or it was
// refused for no fault, or it came to nothing for a fault before it could be owed and left its
// retry owed on the issue's place (runningCopy.missed).
//
// The copy's time is the store's clock as its take begins, read once the copy holds the issue's
// place and before the shim is asked (store.Store.BeginWorkspaceCopy), and the store writes that
// time and not its own clock at the write. A copy read before a push and written after the push's
// record is then older than the push, and the restore refuses it (runtime.DecideWorkspaceRestore):
// the overtaken mark above is set only once the push's transaction has committed, so a copy that
// checks the mark just before it and writes just after the push's record is stored all the same.
// A clock the daemon cannot read is a copy not taken, logged under its reason as every other.
// That time is handed to the drop of a workspace that bundled to nothing as well
// (store.Store.DeleteRunningWorkspaceCopy), which drops no copy taken after it: the read says
// what the workspace held as the take began, and a row written since — a cancelled release's
// move, a confirmed restore's keep — holds work read after that. Such a drop is logged at Info
// under runningCopyNewerStands, and the copy it left stands for the next pod.
//
// Every way it can come to nothing for a fault is logged under its own reason and holds nothing:
// the issue keeps the copy it had and owes a retry, which the cadence takes at the next period,
// between turns as well, once a role of the issue is live (runningCopy.retry). A copy that settles
// the issue's copy — stores it, drops it as empty, or is refused for no fault by a newer copy, by
// the release bundle, by a push that overtook it or by the project rule below — owes none. Nor
// does a bundle over the bound, which the connection answers as
// runtime.WorkspaceBundleTooLargeError whether the shim refused it with its size or it arrived
// whole (stream.Conn.BundleWorkspace): it is no fault to retry (runningCopyTooLarge). The daemon's
// own stop is not a fault: the run's context ending ends the copy, which is the stop and nothing
// an operator need read, so it is logged at Debug alone.
//
// Both writes name the daemon's project, projectID: the claim token's spelling of it
// (claim.ProjectToken), which the claims the cadence chooses its role from carry
// (runningCopyClaim) and which the Kubernetes runtime's take of a pod's restore is bound to as
// well, since the boot hands the one token to both (sandbox.Options.Project, savedWorkspaceCopy).
// The store writes no copy of an issue that project does not record and drops none
// (store.ErrIssueNotRecorded), an operator's spawn of an issue another project records among them,
// so the running copy of the project that records the issue stands. Such a refusal is logged at
// Info under runningCopyNotRecorded, with the issue, the role and the pod, and owes no retry: it
// is no fault, and a retry would be refused the same way.
func (w *workflowRuntime) storeRunningCopy(ctx context.Context, issue string, role claim.Role) (retry bool) {
	// How long the copy took, read at whichever line is written: the bundler's own build and,
	// where one was made, the write that stored or dropped the row. A deployment reads it to see
	// a workspace growing slow long before a copy is abandoned at its budget.
	started := time.Now()
	took := func() string { return time.Since(started).Round(time.Millisecond).String() }
	begun, err := w.copyStore.BeginWorkspaceCopy(ctx)
	if err != nil {
		w.unsavedCopy(issue, "role", role, "reason", runningCopyClockUnread, "error", err)
		return true
	}
	bundle, err := w.copyBundler.BundleWorkspace(ctx, issue, role)
	var tooLarge *runtime.WorkspaceBundleTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopyTooLarge,
			"bytes", tooLarge.Bytes, "max", tooLarge.Max, "took", took())
		return false
	case err != nil:
		reason := runningCopyBundlerFailed
		if errors.Is(err, context.DeadlineExceeded) {
			reason = runningCopyBudgetSpent
		}
		w.unsavedCopy(issue, "role", role, "reason", reason, "budget", w.runningCopyBudget().String(), "took", took(), "error", err)
		return true
	}
	// refused logs a write of the issue's row that the store refused for no fault, at Info under
	// reason, and owes no retry. The drop of a workspace that bundled to nothing and the save of one
	// that did both meet the issue the project does not record and the newer copy, and the save the
	// release bundle besides; each is written here with one list of keys for both writes: bytes 0
	// for the drop, and for a newer copy the time this copy's take began, which the row it left was
	// taken after.
	refused := func(reason string, bytes int) bool {
		args := []any{"issue", issue, "role", role, "reason", reason, "pod", bundle.PodUID, "bytes", bytes}
		if reason == runningCopyNewerStands {
			args = append(args, "taken", begun.At())
		}
		w.log.Info(runningCopyUnsavedLine, append(args, "took", took())...)
		return false
	}
	size := bundle.Size()
	if size == 0 {
		err := w.copyStore.DeleteRunningWorkspaceCopy(ctx, w.projectID, issue, begun)
		switch {
		case errors.Is(err, store.ErrIssueNotRecorded):
			return refused(runningCopyNotRecorded, 0)
		case errors.Is(err, store.ErrNewerCopyStands):
			return refused(runningCopyNewerStands, 0)
		case err != nil:
			w.unsavedCopy(issue, "role", role, "reason", runningCopyDropFailed, "took", took(), "error", err)
			return true
		}
		w.log.Info(runningCopyDroppedLine, "issue", issue, "role", role, "bytes", 0, "took", took())
		return false
	}
	if w.copies.overtaken(issue) {
		w.log.Info(runningCopyOvertakenLine, "issue", issue, "role", role, "bytes", size, "taken", begun.At(), "took", took())
		return false
	}
	err = w.copyStore.PutRunningWorkspaceCopy(ctx, w.projectID, issue, begun, bundle)
	switch {
	case errors.Is(err, store.ErrIssueNotRecorded):
		return refused(runningCopyNotRecorded, size)
	case errors.Is(err, store.ErrReleaseBundleSaved):
		return refused(runningCopyReleaseBundled, size)
	case errors.Is(err, store.ErrNewerCopyStands):
		return refused(runningCopyNewerStands, size)
	case err != nil:
		w.unsavedCopy(issue, "role", role, "reason", runningCopySaveFailed, "bytes", size, "took", took(), "error", err)
		return true
	}
	w.log.Info(runningCopyStoredLine, "issue", issue, "role", role, "bytes", size, "taken", begun.At(), "took", took())
	return false
}

// unsavedCopy logs a copy of issue that came to nothing under its reason, at Warn
// (runningCopyUnsavedLine), or the daemon's stop at Debug where that is what ended it
// (runningCopyStoppedLine).
func (w *workflowRuntime) unsavedCopy(issue string, args ...any) {
	if !w.copies.running() {
		w.log.Debug(runningCopyStoppedLine, "issue", issue)
		return
	}
	w.log.Warn(runningCopyUnsavedLine, append([]any{"issue", issue}, args...)...)
}

// issuesInTurn is every issue of the project with a turn in flight, each named once, in the order
// the claims were read. A claim in supervise.StateWorking is a role whose agent is in a turn, and
// the cadence copies that issue's workspace every period. An issue between turns is copied by the
// cadence only to retry a copy of it that came to nothing (copyWorkingWorkspaces).
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

// runningCopyClaim is the claim of the role a copy the cadence or a push takes is asked of: the
// first live role of the issue with a registered agent and a running process
// (supervise.HoldsCapability), which is what a shim is. It is the claim and not the role alone
// because its locator names the pod the copy would be taken off (takeRunningCopyOf). A turn's end
// chooses nothing: its copy asks the role whose turn ended (copyWorkspaceAtTurnEnd), and the copy
// owed behind one in flight asks the role it was owed with (copyAsk).
//
// The order is the order the store reads the claims in, by claim token, which is the order the
// idle release's own view reads them in and so the order of the roles its decision carries
// (workflow.IdleRelease); the release's save asks the first of those (decision.Roles[0]). One
// workspace is shared by every role of the issue, so which of them answers decides nothing but
// whose process does the work — and the two paths choosing the same way means a deployment reads
// one role's log for both.
func runningCopyClaim(claims []supervise.Claim, project, issue string) (supervise.Claim, bool) {
	for _, c := range claims {
		if c.Project != project || c.Issue != issue || c.Role == claim.RoleController {
			continue
		}
		if supervise.HoldsCapability(c.State) {
			return c, true
		}
	}
	return supervise.Claim{}, false
}

// runningCopyBudget is how long one copy is given, the production constant unless a test
// shortened it.
func (w *workflowRuntime) runningCopyBudget() time.Duration {
	if w.copyBudget > 0 {
		return w.copyBudget
	}
	return runningCopyBudget
}

// copyTick is the period the cadence's ticker runs at: the daemon's idle tick, idleTickInterval
// unless a test shortened it. It is the tick due allows for, so the bound runningCopyPeriod states
// holds at whatever period the ticker runs.
func (w *workflowRuntime) copyTick() time.Duration {
	if w.idleTick > 0 {
		return w.idleTick
	}
	return idleTickInterval
}
