package daemon

// workspace_check.go is the workspace check that every release and close of an issue pod runs
// before it deletes a disk.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// checkIssueWorkspace is the first act of an idle pod's release: it asks the shim of the role the
// release named — a process still running in the pod, because this row runs before the `release`
// rows that stop them — to read whether the workspace every role of the issue shares holds work
// that no branch or tag on origin holds (runtime.WorkspaceWorkReader, workspace.WorkRevset): a
// commit under a working copy or a local bookmark that no remote branch or tag reaches, or a change
// in the working copy. The shim runs the `jj` of the workspace it already works in, and it dies
// with its role's process. The daemon keeps no copy of the workspace: a pod whose workspace holds
// such work is not released at all.
//
// It is the first act of the release of a pod no claim owns too, where that release deletes the
// disk (payload.Launcher, workflow.UnownedPodRelease's Read): no role process runs in that pod, so
// the pod's launcher makes the same read (runtime.PodWorkspaceReader, R6 of
// #5285), and a decline tells the issue's architect (declineFor).
//
// A read that finds nothing lets the release go on to its role stops and its teardown, which
// deletes the pod's disk: nothing on it is lost, since all of it is on origin. A read that finds
// work declines the release, and so does a read that fails, since it cannot show that the
// workspace holds nothing (declineRelease). The daemon pushes nothing on a role's behalf.
//
// A check of a release that is over does nothing. One that finds the project paused cancels the
// release as before, and so does one that finds the pod given work since the release's mark — a
// wake, a start, a claim in a turn or a turn after the mark's newest — before any read
// (store.IssueReleaseCheck, ReleaseCausePaused and ReleaseCauseOvertaken).
func (r *outbox) checkIssueWorkspace(ctx context.Context, row record.OutboxRow, payload record.IssueWorkspaceCheck) error {
	if r.supervisor == nil {
		return errors.New("issue workspace check executor has no claim supervisor")
	}
	if _, ok := r.supervisor.deps.Runtime.(runtime.IssueReleaser); !ok {
		// A runtime with no shared issue resources has no pod to release, so nothing to read.
		return nil
	}
	if payload.LeftPodClose() {
		return r.checkLeftPodWorkspace(ctx, row, payload)
	}
	decision, err := r.trees.IssueReleaseCheck(ctx, r.project, row.Issue, row.ID)
	if err != nil {
		return err
	}
	switch decision.Act {
	case store.ReleaseOver:
		r.log.Info("outbox release workspace check finished without acting: the pod's release is over",
			"row", row.ID, "issue", row.Issue)
		return nil
	case store.ReleaseCancel:
		// The project is paused (ReleaseCausePaused): the pause deletes no pod, so the release
		// is cancelled before any role stops, whatever reached its roles first. Or the pod was
		// given work since the mark (ReleaseCauseOvertaken): a read beside that work could miss
		// what it makes, so the release is cancelled before the read, and the pass decides the
		// pod again.
		hold, why := releaseHold(decision.Cause)
		return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
	}
	// The read's moment is taken before it starts, so a turn that starts while it runs is a turn
	// after the decline it may record (pods.IdleDecline.Stands).
	readAt := r.now().UTC()
	work, failed := r.readIssueWorkspace(ctx, row.Issue, payload)
	if readOtherPod(payload, work) {
		// The launcher read a pod that took the place of the one the release was marked over: its
		// answer is evidence of that pod alone, which no pass decided, so it declines nothing and
		// lets nothing through, and the release is cancelled for the next pass to decide afresh.
		r.log.Info("outbox: the launcher read another pod than the one the release of a pod no claim owns was marked over; the read is discarded and the release cancelled",
			"issue", row.Issue, "markedPod", payload.PodUID, "readPod", work.PodUID)
		hold, why := releaseHold(store.ReleaseCauseOvertaken)
		return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
	}
	if failed != nil || work.HoldsWork() {
		return r.declineRelease(ctx, row.Issue, payload, readAt, work, failed)
	}
	// The workspace holds none of the work an earlier decline named, so that decline goes: kept,
	// it would hold the pod for work the daemon's own newer read found gone, whatever becomes of
	// this release (store.Store.ForgetIdleDecline).
	if err := r.trees.ForgetIdleDecline(ctx, r.project, row.Issue); err != nil {
		return err
	}
	// The release goes on: after a launcher's read (payload.Launcher) to its teardown alone, since
	// it stops no role, and after a role's read to its role stops.
	r.log.Info("outbox: the release read the issue's workspace and found nothing that no branch or tag on origin holds; the release goes on",
		"issue", row.Issue, "role", payload.Role, "launcher", payload.Launcher, "head", work.Head)
	return nil
}

// readIssueWorkspace is the workspace check's one read, and the error of a read that could not be
// made: through the role's shim the check names (runtime.WorkspaceWorkReader), or, for a check of
// a pod in which no role process runs (payload.Launcher), through the pod's launcher
// (runtime.PodWorkspaceReader). A runtime that cannot make the read is such an error.
func (r *outbox) readIssueWorkspace(ctx context.Context, issue string, payload record.IssueWorkspaceCheck) (runtime.WorkspaceWork, error) {
	if payload.Launcher {
		reader, ok := r.supervisor.deps.Runtime.(runtime.PodWorkspaceReader)
		if !ok {
			return runtime.WorkspaceWork{}, errors.New("the runtime cannot have the pod's launcher read the issue workspace")
		}
		return reader.PodWorkspaceWork(ctx, issue)
	}
	reader, ok := r.supervisor.deps.Runtime.(runtime.WorkspaceWorkReader)
	if !ok {
		return runtime.WorkspaceWork{}, errors.New("the runtime cannot read the issue workspace")
	}
	return reader.WorkspaceWork(ctx, issue, payload.Role)
}

// readOtherPod is whether a launcher's read of the check (payload.Launcher) was made in another pod
// than the one the pass queued the check over (record.IssueWorkspaceCheck.PodUID): a pod took its
// place, and the read is evidence of that pod alone (R6 of #5285, Sol's point 2).
// A check or a read that names no pod is no such read.
func readOtherPod(payload record.IssueWorkspaceCheck, work runtime.WorkspaceWork) bool {
	return payload.Launcher && payload.PodUID != "" && work.PodUID != "" && work.PodUID != payload.PodUID
}

// readPod is the pod a launcher's clean read lets a teardown take: the pod it read, else the pod
// the check was queued over. A role's read binds none ("").
func readPod(payload record.IssueWorkspaceCheck, work runtime.WorkspaceWork) string {
	switch {
	case !payload.Launcher:
		return ""
	case work.PodUID != "":
		return work.PodUID
	}
	return payload.PodUID
}

// holdsPod is whether the runtime holds pod as issue's pod, read by a teardown that a launcher's
// clean read of pod let through (releaseIssue, suspendIssue) under the runtime's pod lock, which
// orders it after any launch that replaced the pod. A runtime that cannot say holds it.
func (r *outbox) holdsPod(issue, pod string) bool {
	pods, ok := r.supervisor.deps.Runtime.(issuePods)
	if !ok {
		return true
	}
	now, read := pods.IssuePod(issue)
	return read && now == pod
}

// checkLeftPodWorkspace is the first act of the close of a pod whose issue left the workflow while
// its tree runs on (workflow.LeftPodClose): the close deletes the pod's disk, so the disk is read
// first, as the idle release reads it, through a live role where one runs in the pod, and through
// the pod's launcher where none does (payload.Launcher, R6 of #5285). A read that
// finds work that origin does not hold, or that fails, declines the close (declineFor,
// store.DeclineLeftPodClose): the pod stays with its roles as they are, the role read through, or
// for a launcher's read the issue's architect, is told once, and the rule holds the pod while the
// decline stands. Its hold carries the read's sentence as its detail, where a cancel's carries
// none, and is otherwise recorded as a cancel's is (cancelRelease), as is its line. A read that
// found work while a claim of the issue was in a turn, or had started one since the pass that
// queued the check (store.Store.LeftPodTurned), records nothing: that turn can have pushed the work,
// so the check finishes without acting and the next pass reads again (the Legion reviewer's point
// 2 on R6). A read that finds nothing queues the close's stops and the issue's
// Sandbox suspension (leftPodStopRows) in one transaction with a view read again
// (workflow.LeftPodCloseAfterCheck), so a role that started a turn since the pass that queued the
// check (payload.TurnStart) — one that ended while the read ran too — or that is in one or was
// given work now, is not stopped; the next pass decides that pod again. A clean read also forgets
// the decline on record, whose work it found gone, in that same transaction.
//
// The check takes the lifecycle serializer (treelifecycle.Serialize) before it touches a row of the
// issue, as every writer of the issue's lifecycle does: a clean read that deleted the decline row
// before ReleaseIdleIssue took the serializer would hold that row while waiting for the serializer,
// and a decline written under the serializer by the lane that holds the row now would wait for that
// row, a deadlock. The decline itself, and the claim states a launcher's decline names, are read
// before the transaction.
//
// Either way the check finishes its own row in the transaction that records what it decided
// (FinishLeasedOutbox), so that transaction commits only while this run's lease holds the row: a
// run whose lease another lane took writes nothing, and the lane that holds the row decides the
// close. And a check whose commit outcome is unknown leaves no row to run again: the stops it
// committed run on the lane, and a retry would read the workspace after they took the role's shim
// away, and decline a close already done, under a hold that no pass ends, since no pass visits a
// pod with no Sandbox.
func (r *outbox) checkLeftPodWorkspace(ctx context.Context, row record.OutboxRow, payload record.IssueWorkspaceCheck) error {
	readAt := r.now().UTC()
	work, failed := r.readIssueWorkspace(ctx, row.Issue, payload)
	if readOtherPod(payload, work) {
		// The launcher read a pod that took the place of the one the close was queued over: its
		// answer is evidence of that pod alone. The close marked nothing, so the check finishes
		// without acting and the next pass decides the pod that is there.
		r.log.Info("outbox: the launcher read another pod than the one the close of a left pod was queued over; the read is discarded",
			"issue", row.Issue, "queuedPod", payload.PodUID, "readPod", work.PodUID)
		return nil
	}
	now := r.now().UTC()
	declines := failed != nil || work.HoldsWork()
	var decline store.IdleReleaseDecline
	var hold workflow.IdleHold
	var why string
	if declines {
		var err error
		if decline, hold, why, err = r.declineFor(ctx, row.Issue, payload, readAt, work, failed); err != nil {
			return err
		}
	}
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		if declines {
			// A turn that ran beside the read can have pushed the work the read found: a decline
			// from it would hold the pod as holding that work until another turn, and for good
			// once the claims stop (pods.IdleDecline.HoldsUnpushedWork). The read is discarded,
			// and the next pass reads again, as a clean read beside such a turn is
			// (workflow.LeftPodCloseAfterCheck).
			turned, err := r.trees.LeftPodTurned(ctx, r.project, row.Issue, payload.TurnStart)
			if err != nil {
				return err
			}
			if turned {
				store.OnCommit(ctx, func() {
					r.log.Info("outbox: the close of the pod of an issue that left the workflow read work while a turn of its claims ran beside the read; the read is discarded, and the next pass reads again",
						"issue", row.Issue, "reason", why, "role", payload.Role, "launcher", payload.Launcher)
				})
				return r.records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken)
			}
			told, err := r.trees.DeclineLeftPodClose(ctx, r.project, row.Issue, decline)
			if err != nil {
				return err
			}
			if err := store.RecordPodHoldTx(ctx, tx, store.PodHold{Project: r.project, Issue: row.Issue,
				Reason: hold.String(), Detail: why, At: r.now().UTC()}); err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				r.log.Info("outbox: the close of the pod of an issue that left the workflow is declined, and the pod stays with its workspace",
					"issue", row.Issue, "hold", hold.String(), "reason", why, "role", payload.Role, "launcher", payload.Launcher, "told", told)
			})
		} else {
			if err := r.trees.ForgetIdleDecline(ctx, r.project, row.Issue); err != nil {
				return err
			}
			closed, err := r.trees.ReleaseIdleIssue(ctx, r.project, row.Issue, leftPodStopsAfterCheck(now, payload.TurnStart, readPod(payload, work)))
			if err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				r.log.Info("outbox: the close of the pod of an issue that left the workflow read its workspace and found nothing that no branch or tag on origin holds",
					"issue", row.Issue, "role", payload.Role, "launcher", payload.Launcher, "head", work.Head, "closing", closed)
			})
		}
		// The check's row is finished twice: here, fenced on this run's lease, so a lease another
		// lane holds fails the whole transaction (record.ErrOutboxLeaseLost); and once the check
		// returns, by the lane's own finish (outbox.finish), whose FinishOutbox then deletes
		// nothing and answers nil.
		return r.records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken)
	})
}

// declineFor is the decline a workspace check records for what its read found (work, failed nil)
// or for a read that failed (failed), and the hold and the sentence it is recorded under: the
// pod's launcher's (keptDecline) where payload.Launcher, else the role's (roleDecline).
func (r *outbox) declineFor(ctx context.Context, issue string, payload record.IssueWorkspaceCheck, readAt time.Time, work runtime.WorkspaceWork, failed error) (store.IdleReleaseDecline, workflow.IdleHold, string, error) {
	if payload.Launcher {
		return r.keptDecline(ctx, issue, readAt, work, failed)
	}
	return r.roleDecline(issue, payload.Role, readAt, work, failed)
}

// declineOf is what every decline records of its read: its moment, readAt, the moment the read
// began, so a turn that started while the read ran — one that may have pushed the work the read
// found — is a turn after the decline, which ends it (pods.IdleDecline.Stands); the role told;
// the reason, pods.DeclineUnpushedWork or, for a read that failed, pods.DeclineWorkspaceUnread;
// the pod; and for a read that could read, the work it found.
func declineOf(role claim.Role, pod string, readAt time.Time, work runtime.WorkspaceWork, failed error) store.IdleReleaseDecline {
	decline := store.IdleReleaseDecline{At: readAt, Role: role, Reason: pods.DeclineUnpushedWork, PodUID: pod}
	if failed != nil {
		decline.Reason = pods.DeclineWorkspaceUnread
		return decline
	}
	decline.Head, decline.Commits, decline.Changed = work.Head, work.Count, work.Changed
	return decline
}

// declineHold is the hold a declined check keeps its pod under, and the sentence the hold record
// and the log line carry: what the read found (declineReason), or that it failed and why. A pod a
// live role runs in is held under the idle release's names, whose spelling is the decline's reason
// (workflow's idleHoldNames), and a pod no role runs in, whose launcher read it (unowned,
// keptDecline), under the unowned_ names, so a reader of the records can tell the two apart.
func declineHold(unowned bool, work runtime.WorkspaceWork, failed error) (workflow.IdleHold, string) {
	switch {
	case failed != nil && unowned:
		return workflow.IdleHoldUnownedWorkspaceUnread, unreadReason(failed)
	case failed != nil:
		return workflow.IdleHoldWorkspaceUnread, unreadReason(failed)
	case unowned:
		return workflow.IdleHoldUnownedUnpushedWork, declineReason(work)
	}
	return workflow.IdleHoldUnpushedWork, declineReason(work)
}

// unreadReason is what the hold record and the log line say of a read that failed (readError).
func unreadReason(failed error) string {
	return "the workspace could not be read: " + readError(failed)
}

// readError is a failed workspace read's error as the daemon keeps it, in a notice, a hold record
// and a log line: the far side's text, bounded keeping both its ends (storetext.FarSideErrorEnds),
// since the process that made the read cut its middle out and kept its end for its note on a git
// lock (shimwire.CutAnswerError, the Legion reviewer's point 6 on R6's review fixes), and quoted
// (strconv.Quote). The text is the pod's: jj's words about files a role writes, or a path a role
// wrote that the guard refusing the read names (workspace's sharedRepository). A notice carries it
// to the issue's architect and, in its place, to the controller, whom the wakes reference tells to
// publish the notice's reason, so it goes as one quoted string, which no line of it can leave
// (the Legion reviewer's point 14).
func readError(failed error) string {
	return strconv.Quote(storetext.FarSideErrorEnds(failed.Error()))
}

// roleDecline is the decline of a check read through a live role's shim. Its pod is the one the
// runtime holds for the issue now, so a later pass can tell a decline taken in a pod a relaunch
// since replaced (pods.IdleDecline.Stands). The role is told that the release waits for its push
// (declineMessage), and the store queues that message only where the decline names other work than
// the decline on record (store's recordDecline). A read that failed tells nobody, since there is
// nothing the role can push on its word; it is a log line and a hold record, and the decline lets
// the pass ask again after an hour (pods.DeclineUnreadRetry).
func (r *outbox) roleDecline(issue string, role claim.Role, readAt time.Time, work runtime.WorkspaceWork, failed error) (store.IdleReleaseDecline, workflow.IdleHold, string, error) {
	pod := ""
	if podRuntime, ok := r.supervisor.deps.Runtime.(issuePods); ok {
		if uid, read := podRuntime.IssuePod(issue); read {
			pod = uid
		}
	}
	decline := declineOf(role, pod, readAt, work, failed)
	hold, why := declineHold(false, work, failed)
	if failed != nil {
		return decline, hold, why, nil
	}
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", fmt.Errorf("name the role told of %s's declined release: %w", issue, err)
	}
	message, err := record.NewOutboxRow(issue, record.RoleMessage{Role: string(token), Body: declineMessage(issue, work)}, readAt)
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", fmt.Errorf("encode the message of %s's declined release: %w", issue, err)
	}
	decline.Message = &store.IdleReleaseRow{Kind: string(message.Kind), Payload: message.Payload}
	return decline, hold, why, nil
}

// keptDecline is the decline of a check the pod's launcher read (R6 of #5285): no
// role process runs in the pod, so no live role is there to tell. The decline is Kept and names the
// pod the launcher read (runtime.WorkspaceWork.PodUID), since a pod that took its place since holds
// another disk. Its role is the issue's architect, whose workspace-kept notice (keptMessage) is its
// message: it names each claim's state and what the read found, a failed read's error included, and
// the store queues it once for each pod (store's recordDecline), whether the read found work or
// failed. Its hold is recorded under the unowned_ names (declineHold).
func (r *outbox) keptDecline(ctx context.Context, issue string, readAt time.Time, work runtime.WorkspaceWork, failed error) (store.IdleReleaseDecline, workflow.IdleHold, string, error) {
	hold, why := declineHold(true, work, failed)
	claims, err := r.trees.ClaimStatesIn(ctx, r.project, issue)
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", err
	}
	decline, err := keptNoticeDecline(issue, work.PodUID, readAt, work, failed, keptMessage(issue, work.PodUID, claims, work, failed))
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", err
	}
	return decline, hold, why, nil
}

// keptNoticeDecline is the Kept decline of the read at readAt of issue's pod, pod (declineOf), that
// tells the issue's architect by its workspace-kept notice (record.WorkspaceKeptNotice), whose
// reason is notice: the decline of a pod whose launcher read it (keptDecline), and the record of an
// update round's keep that tells no open phase's worker (recordPodKept). The store queues the
// notice once for each pod (store's recordDecline).
func keptNoticeDecline(issue, pod string, readAt time.Time, work runtime.WorkspaceWork, failed error, notice string) (store.IdleReleaseDecline, error) {
	message, err := record.NewOutboxRow(issue, record.Notice{Kind: record.WorkspaceKeptNotice, Role: claim.RoleArchitect, Reason: notice}, readAt)
	if err != nil {
		return store.IdleReleaseDecline{}, fmt.Errorf("encode the notice of %s's kept pod: %w", issue, err)
	}
	decline := declineOf(claim.RoleArchitect, pod, readAt, work, failed)
	decline.Kept, decline.Message = true, &store.IdleReleaseRow{Kind: string(message.Kind), Payload: message.Payload}
	return decline, nil
}

// declineRelease ends a release whose workspace check found work that no branch or tag on origin
// holds on a working copy or a local bookmark (failed nil), or could not read the workspace
// (failed, the read's error). A decline at the check's first attempt cancels the release before any
// role stopped, so the pod stays exactly as it was; where that attempt failed and the same pass's
// role stop has run, a later attempt's decline queues each stopped role's resume, as a cancel does.
// In the cancel's own transaction the store records the decline
// (store.DeclineIssueRelease, declineFor), which the idle rule reads to keep the pod under that
// reason with no new mark and no new read while it stands (pods.IdleDecline.Stands). So a pod that
// keeps its work is asked once, and its role is told once: a read after a turn that finds the same
// work keeps the decline and tells nobody. A decline made while the project is paused, where the
// release had stopped a role, records each such role as stopped by the pause rather than resuming
// it, holds the pod and arms the open phase's worker's backstop as a cancel does, by the same rule
// (pausedCancel, outbox.getsBackstop). Its hold carries the read's sentence as its detail, where a
// cancel's carries none, and is otherwise recorded as a cancel's is (cancelRelease), as is its line.
func (r *outbox) declineRelease(ctx context.Context, issue string, payload record.IssueWorkspaceCheck, readAt time.Time, work runtime.WorkspaceWork, failed error) error {
	decline, hold, why, err := r.declineFor(ctx, issue, payload, readAt, work, failed)
	if err != nil {
		return err
	}
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		declined, err := r.trees.DeclineIssueRelease(ctx, r.project, issue, decline, r.releaseResume(issue, payload.Tree), record.OutOfWorkflow,
			r.pausedCancel())
		if err != nil {
			return err
		}
		if !declined.Declined {
			store.OnCommit(ctx, func() {
				r.log.Info("outbox release workspace check finished without acting: the pod's release was over when it declined",
					"issue", issue, "hold", hold.String(), "reason", why)
			})
			return nil
		}
		if err := store.RecordPodHoldTx(ctx, tx, store.PodHold{Project: r.project, Issue: issue,
			Reason: hold.String(), Detail: why, At: r.now().UTC()}); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			// A launcher's decline (payload.Launcher) is of the release of a pod no claim owns,
			// told to the issue's architect once for the pod; a role's tells that role, and no
			// release is asked again until a role of the issue starts a turn.
			r.log.Info("outbox: the release of an issue's pod is declined, and the pod stays with its workspace",
				"issue", issue, "hold", hold.String(), "reason", why, "role", payload.Role, "launcher", payload.Launcher, "told", declined.Told)
		})
		return nil
	})
}

// declineReason is what the hold record and the log line say a read that found work found: the
// count of commits on a working copy or a local bookmark that no branch or tag on origin holds, and
// whether the working copy holds a change, on the working-copy commit the read was made at.
func declineReason(work runtime.WorkspaceWork) string {
	return fmt.Sprintf("%d commit(s) not on origin, working-copy change %t, head %s", work.Count, work.Changed, work.Head)
}

// declineMessage is what the role is told when the release's check finds work in the workspace
// that origin does not hold: what the read found, that the release waits for the role's push, and
// that the daemon pushes nothing. The role reads it in its session; people read it in the role's
// history, so it follows ASD-STE100. The commit ids in it are 40 hexadecimal digits each, since the
// daemon reads no other answer from the shim (shimwire.WorkspaceWorkResult.Validate).
func declineMessage(issue string, work runtime.WorkspaceWork) string {
	found := append([]string{"The daemon did not release this issue's idle pod. Its workspace holds work that no branch or tag on origin holds."},
		workFound(work)...)
	found = append(found,
		"The idle release does not take a pod whose workspace holds such work, so the pod stays and the release waits for your push. "+
			"The daemon does not push for you.",
		fmt.Sprintf("Push the work that you keep to legion/%s, and abandon the work that you do not keep. "+
			"You are told this once: the daemon reads the workspace again after a turn of a role of this issue, and tells you again only if it finds other work.", issue))
	return strings.Join(found, " ")
}

// workFound is what a read that found work found, in sentences, of a workspace that still holds it
// (workSentences): the change in the working copy, and the commits on a working copy or a bookmark
// that origin does not hold.
func workFound(work runtime.WorkspaceWork) []string {
	return workSentences(work, "holds", "on a working copy or a bookmark that origin does not hold")
}

// workSentences is the work a read found, in the sentences of a notice: the change in the working
// copy, and the commits that origin does not hold, by id up to shimwire.MaxWorkspaceWorkCommits of
// them and by count all of them. holds is the verb of each sentence, and unheld says, after the
// commits, that origin does not hold them: a workspace that still holds the work says what it
// holds (workFound), and one a replacement took says what it held (readFound).
func workSentences(work runtime.WorkspaceWork, holds, unheld string) []string {
	var found []string
	if work.Changed {
		found = append(found, fmt.Sprintf("The working copy %s a change (commit %s).", holds, work.Head))
	}
	commits := strings.Join(work.Commits, ", ")
	switch {
	case work.Count > len(work.Commits):
		found = append(found, fmt.Sprintf("The workspace %s %d commits %s. The first %d of these commits are: %s.",
			holds, work.Count, unheld, len(work.Commits), commits))
	case work.Count == 1:
		found = append(found, fmt.Sprintf("The workspace %s one commit %s: %s.", holds, unheld, commits))
	case work.Count > 1:
		found = append(found, fmt.Sprintf("The workspace %s %d commits %s: %s.", holds, work.Count, unheld, commits))
	}
	return found
}

// keptMessage is the issue's architect's workspace-kept notice (record.WorkspaceKeptNotice): the
// daemon keeps issue's pod, in which no role process runs, because the pod's launcher read its
// workspace and found work that no branch or tag on origin holds (failed nil), or could not read it
// (failed). It names the pod, the state of each claim of the issue, what the read found or why it
// failed, that the daemon pushes and saves nothing, what ends the hold, and how the work is pushed.
// The architect reads it in its session and people read it in its history, so it follows
// ASD-STE100. The read's error is the far side's text as the daemon keeps it (readError).
func keptMessage(issue, pod string, claims store.ClaimStates, work runtime.WorkspaceWork, failed error) string {
	where := "The daemon did not delete the pod of " + issue
	if pod != "" {
		where += " (pod " + pod + ")"
	}
	return keptNotice(issue, claims, work, failed,
		where+". No role process runs in this pod, so the pod's launcher read its workspace before the delete.",
		"The launcher could not read the workspace",
		"The daemon does not push this work and does not save it. The pod stays until a later read finds nothing that origin does not hold, the runtime makes another pod for the issue, or the issue closes, or its tree closes or lingers. "+
			"The daemon reads the workspace again after a role of the issue starts or runs a turn.",
		fmt.Sprintf("To keep the work, start a role of %s in this pod, for example with `legion claims resume` or `legion claims retry`, and push the work to legion/%s. "+
			"You are told this once for this pod.", issue, issue))
}

// keptNotice is the issue's architect's workspace-kept notice (record.WorkspaceKeptNotice) in the
// one shape both keeps of a pod give it (keptMessage, roundKeptMessage): opening, which names the
// pod and why its workspace was read; what the read found, or, for a read that failed (failed),
// that unread, who could not read it, cannot show the workspace holds nothing, and the read's
// error (readError); the state of each claim of the issue; rest, which says what keeps the pod,
// what ends the keep and how the work is pushed; and, for a read that failed, the read the daemon
// makes again after an hour.
func keptNotice(issue string, claims store.ClaimStates, work runtime.WorkspaceWork, failed error, opening, unread string, rest ...string) string {
	found := []string{opening}
	if failed != nil {
		found = append(found, unread+", so the daemon cannot show that the workspace holds nothing that no branch or tag on origin holds. "+
			"The error of the read, in quotes, is: "+readError(failed)+".")
	} else {
		found = append(found, "The workspace holds work that no branch or tag on origin holds.")
		found = append(found, workFound(work)...)
	}
	states := "The claims of " + issue + " are: " + claims.String() + "."
	if len(claims) == 0 {
		states = "The daemon holds no claim of " + issue + "."
	}
	found = append(append(found, states), rest...)
	if failed != nil {
		found = append(found, "Because the read failed, the daemon also reads the workspace again after an hour.")
	}
	return strings.Join(found, " ")
}
