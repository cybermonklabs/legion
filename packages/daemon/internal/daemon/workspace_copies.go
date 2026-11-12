package daemon

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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

// The three lines one copy writes are pinned as constants, as supervise's turn lines are: an
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
	// had; the issue is tried again at the next period.
	runningCopyUnsavedLine = "running copy: no copy of the issue workspace was stored"
)

// The reasons runningCopyUnsavedLine carries. They are names rather than sentences because a
// deployment counts them: a cadence that stores nothing is read from which of these it writes.
const (
	// runningCopyClaimsUnread: the daemon could not read its own claims, so no role was chosen.
	runningCopyClaimsUnread = "claims_unread"
	// runningCopyNoRole: the issue has no live role with a process to ask. Only a shim can build
	// the bundle, in the workspace it already works in.
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
// in flight and when the last one ended.
//
// The count is per issue and never per role. One workspace belongs to one issue and the six roles
// of that issue share it, so six roles ending their turns in the same minute must take one bundle
// of that workspace and not six: two runs of the shim's bundler in one workspace share the ref
// prefix `refs/legion-bundle/`, and one run's delete would land inside the other's `git bundle
// create`.
//
// It is memory and not a table. A daemon that restarts has taken no copy in this process, so
// every issue in a turn is due at its first pass, which is the right answer: the daemon cannot
// know what the daemon before it copied, and a copy whose content equals the stored row's moves
// that row's time alone (store.PutRunningWorkspaceCopy). An entry is two words per issue the
// daemon has copied since it started.
type runningCopies struct {
	mu     sync.Mutex
	issues map[string]runningCopy
}

// runningCopy is one issue's place in the cadence.
type runningCopy struct {
	// running is whether a copy of the issue is in flight now.
	running bool
	// ended is when the last copy of the issue finished, whatever it stored; zero for an issue
	// this daemon has copied none of. The period is measured from it.
	ended time.Time
}

// begin takes the issue's one place in flight, answering false when a copy of it already runs.
func (c *runningCopies) begin(issue string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.issues == nil {
		c.issues = make(map[string]runningCopy)
	}
	held := c.issues[issue]
	if held.running {
		return false
	}
	held.running = true
	c.issues[issue] = held
	return true
}

// done gives the issue's place back and starts its next period at at, and drops the entry of
// every issue whose copy is not in flight and whose period has passed by at. Only the caller
// whose begin answered true calls it.
//
// Such an entry answers due exactly as a missing one does, so keeping it says nothing the map
// would not say without it, and a daemon that runs for weeks would hold one word per issue it
// has ever copied for ever. It is pruned where the map is written, as the runtime's two records
// of a pod's end are (sandbox's prunePodEnds), so nothing has to sweep it. The entry this call
// writes ends at at, so no period of its own has passed and it stays.
func (c *runningCopies) done(issue string, at time.Time, period time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues[issue] = runningCopy{ended: at}
	maps.DeleteFunc(c.issues, func(_ string, held runningCopy) bool {
		return !held.running && at.Sub(held.ended) >= period
	})
}

// due is whether the cadence takes a copy of issue now: none of it is in flight, and the period
// has passed since the last one ended. An issue this daemon has copied none of is due at once.
func (c *runningCopies) due(issue string, now time.Time, period time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.issues[issue]
	return !held.running && now.Sub(held.ended) >= period
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
	if w.copyBundler == nil || w.idleStore == nil {
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
	claims, err := w.idleStore.Claims(ctx)
	if err != nil {
		w.log.Warn("running copy: could not read the project's claims, so no issue was found to be in a turn", "error", err)
		return
	}
	now := time.Now()
	period := w.runningCopyPeriod()
	for _, issue := range issuesInTurn(claims, w.projectID) {
		if ctx.Err() != nil {
			return
		}
		if !w.copies.due(issue, now, period) {
			continue
		}
		w.startRunningCopy(ctx, issue, claims)
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
	if state != supervise.StateIdle || w.copyBundler == nil || w.idleStore == nil {
		return
	}
	if c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	// The budget is the copy's own, measured from here, and the context is not the daemon's: a
	// callback is given none, as podResume's goroutine is. It hands on no claims: this path holds
	// the one claim whose turn ended and not the project's, so the copy reads them itself.
	w.startRunningCopy(context.Background(), c.Issue, nil)
}

// startRunningCopy hands one copy of issue's workspace to a goroutine of its own, under the
// budget one copy is given. Nothing waits on it: a copy is a saving against an eviction, never a
// step of the turn or of the tick that asked for it. claims is the caller's own read of the
// project's claims where it has one, and nil where it has not.
func (w *workflowRuntime) startRunningCopy(parent context.Context, issue string, claims []supervise.Claim) {
	go func() {
		ctx, cancel := context.WithTimeout(parent, w.runningCopyBudget())
		defer cancel()
		w.takeRunningCopy(ctx, issue, claims)
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
// It takes the issue's one place in flight first and holds it until the call has answered, so no
// second copy of the issue is ever being built: a copy abandoned at its budget while the shim went
// on building would leave exactly the two runs in one workspace that the ref prefix cannot carry.
// The next period runs from the moment this returns.
//
// Every way it can come to nothing is logged under its own reason and holds nothing: the issue
// keeps the copy it had, and the next period tries again.
func (w *workflowRuntime) takeRunningCopy(ctx context.Context, issue string, claims []supervise.Claim) {
	if !w.copies.begin(issue) {
		w.log.Debug("running copy: a copy of this issue's workspace is already being taken", "issue", issue)
		return
	}
	defer func() { w.copies.done(issue, time.Now(), w.runningCopyPeriod()) }()

	if claims == nil {
		read, err := w.idleStore.Claims(ctx)
		if err != nil {
			w.log.Warn(runningCopyUnsavedLine, "issue", issue, "reason", runningCopyClaimsUnread, "error", err)
			return
		}
		claims = read
	}
	role, ok := runningCopyRole(claims, w.projectID, issue)
	if !ok {
		w.log.Warn(runningCopyUnsavedLine, "issue", issue, "reason", runningCopyNoRole)
		return
	}
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
		w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", reason,
			"budget", w.runningCopyBudget().String(), "took", took(), "error", err)
		return
	}
	switch size := bundle.Size(); {
	case size > runtime.MaxWorkspaceBundleBytes:
		w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopyTooLarge,
			"bytes", size, "max", runtime.MaxWorkspaceBundleBytes, "took", took())
	case size == 0:
		if err := w.idleStore.DeleteRunningWorkspaceCopy(ctx, issue); err != nil {
			w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopyDropFailed,
				"took", took(), "error", err)
			return
		}
		w.log.Info(runningCopyDroppedLine, "issue", issue, "role", role, "bytes", 0, "took", took())
	default:
		if err := w.idleStore.PutRunningWorkspaceCopy(ctx, issue, bundle); err != nil {
			w.log.Warn(runningCopyUnsavedLine, "issue", issue, "role", role, "reason", runningCopySaveFailed,
				"bytes", size, "took", took(), "error", err)
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

// runningCopyPeriod is the cadence's period, the production constant unless a test shortened it.
func (w *workflowRuntime) runningCopyPeriod() time.Duration {
	if w.copyPeriod > 0 {
		return w.copyPeriod
	}
	return runningCopyPeriod
}

// runningCopyBudget is how long one copy is given, the production constant unless a test
// shortened it.
func (w *workflowRuntime) runningCopyBudget() time.Duration {
	if w.copyBudget > 0 {
		return w.copyBudget
	}
	return runningCopyBudget
}
