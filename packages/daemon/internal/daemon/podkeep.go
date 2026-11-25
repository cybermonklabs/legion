package daemon

// podkeep.go is R7 of #5285: an update round replaces a pod only over a clean read
// of its workspace, and a replacement that still takes a disk says what it took. The read itself
// is readPodWorkspace (podupdate.go), made before every replacement a round makes; this file is
// what the read decides, the record of a pod a round kept, the read that may end that record, and
// the architect's notice of what a replacement took.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// roundKeeps is whether read, the read of the workspace before one of round's replacements, keeps
// the pod: it found work that no branch or tag on origin holds, or it could not be made, the round
// is over a drift the pod's roles can keep working under (its recorded reasons, which reasons is,
// do not open on runtime.BreakingDrift: runtime.Breaks), and the pod's disk is no kept volume
// that the issue's next pod mounts (roundKeptVolume), which no replacement deletes. A round over a
// drift that breaks the pod (today a drift of the pod's service account, which the Sandbox
// runtime's PodDrift marks where it decides it, spec v23) is replaced whatever the read found,
// since no role can keep working in that pod, and the issue's architect is told what the
// replacement took (roundReplaced). A round is read by the reasons it recorded at its open: a
// drift that breaks the pod and comes while a round is open is the
// next round's, which the keep of this round's read holds back no more than any other
// (podKeptForWork).
func (s *supervision) roundKeeps(issue string, group []supervise.Claim, round, reasons string, read workspaceRead) bool {
	return !read.clean() && !runtime.Breaks(reasons) && !s.roundKeptVolume(issue, group, round)
}

// keepPodForRound ends round with no replacement, because the read before its replacement found
// work that no branch or tag on origin holds, or could not be made, over a drift the pod's roles
// can keep working under (roundKeeps). It first records the keep as a release's decline is
// recorded (recordPodKept): the pod, the work and the moment, and whom it told. Only once that row
// is written does it end the round (endPodUpdates): each claim that answered gets the task the
// round held (supervise.PodUpdateEnded), and the pod and its roles keep working on the old image
// and settings, a turn that runs at the bound included. No new round opens over the pod while the
// record stands (podKeptForWork). A write that fails ends nothing: the round stays open, and the
// next sweep reads the workspace again. group is the issue's claims, the round's members among
// them.
func (s *supervision) keepPodForRound(ctx context.Context, issue, round, pod, reasons string, read workspaceRead, group []supervise.Claim) {
	keep, err := s.supervisor.store.PodKeepOf(ctx, s.plan.project, issue)
	if err == nil {
		var told bool
		if told, err = s.recordPodKept(ctx, issue, round, pod, reasons, read, group, keep.Phase, false); err == nil {
			s.log.Info("supervise: the read of this update round's pod found work that no branch or tag on origin holds, or could not be made; the round ends with no replacement, and the pod and its roles keep working on the old image and settings",
				append([]any{"issue", issue, "round", round, "pod", pod, "reasons", reasons, "told", told}, read.attrs()...)...)
			s.endPodUpdates(issue, group, round)
			return
		}
	}
	if ctx.Err() == nil {
		s.log.Error("supervise: record the update round's keep of its pod; nothing was replaced, the round stays open, and the next sweep reads the workspace again",
			append([]any{"issue", issue, "round", round, "pod", pod, "error", err}, read.attrs()...)...)
	}
}

// recordPodKept writes the record of a round's keep of pod, the issue's pod of round, for read
// (store.Store.DeclinePodReplacement), and answers whether it queued the message that tells of the
// keep. The record is the issue's decline row (store's idle_release_declines), as #4869 records a
// release's decline: the pod, the work the read found or that it failed, the moment (keptMoment),
// and the role told. The idle rule reads the same row, so the idle release of the kept pod waits
// as for its own decline (pods.IdleDecline).
//
// The moment is read.at, taken before the claims the keep was decided on were read (group), moved
// before the start of every turn that ran at the read: a turn group shows running, and a turn the
// claims read again once the read is made show running with a start no later than read.at. Such a
// turn may have changed the workspace after the read, and the read it owes comes once it has ended
// (keptPodReadDue). A turn that started after read.at owes that read anyway (the Legion
// reviewer's point 1).
//
// The open phase's worker (workflow.OpenPhaseRole of issuePhase) is told, where its claim has a
// process in the pod and is not gone, and the read could read: by a role message that the pod
// waits for the push of the work (podKeptMessage). Otherwise the issue's architect is told, by the
// workspace-kept notice (record.WorkspaceKeptNotice, roundKeptMessage) on the decline a launcher's
// read records too (keptNoticeDecline), with each claim's state and what the read found or why it
// failed. Each is told once: the store queues a role's message only where the decline names other
// work than the row on record, and the architect's notice once for each pod (store's
// recordDecline). So a read after a turn that finds the same work moves the row's moment and tells
// nobody. again is a read that may end an earlier round's keep of the pod, made with no round
// (readKeptPod), whose messages name no round (podKeptMessage, roundKeptMessage).
func (s *supervision) recordPodKept(ctx context.Context, issue, round, pod, reasons string, read workspaceRead, group []supervise.Claim, issuePhase phase.Phase, again bool) (bool, error) {
	after, err := s.supervisor.Claims(ctx)
	if err != nil {
		return false, fmt.Errorf("read the claims of %s once the read of its pod is made: %w", issue, err)
	}
	at := keptMoment(read.at, append(slices.Clone(group), s.noticeGroup(claimsOf(after, issue))...))
	if worker, live := openPhaseWorker(group, round, issuePhase); live && read.err == nil {
		message, err := record.NewOutboxRow(issue, record.RoleMessage{Role: string(worker.Token), Body: podKeptMessage(issue, pod, reasons, read.work, again)}, at)
		if err != nil {
			return false, fmt.Errorf("encode the message of %s's kept pod: %w", issue, err)
		}
		decline := declineOf(worker.Role, pod, at, read.work, read.err)
		decline.Message = &store.IdleReleaseRow{Kind: string(message.Kind), Payload: message.Payload}
		return s.supervisor.store.DeclinePodReplacement(ctx, s.plan.project, issue, decline)
	}
	claims, err := s.supervisor.store.ClaimStatesIn(ctx, s.plan.project, issue)
	if err != nil {
		return false, err
	}
	decline, err := keptNoticeDecline(issue, pod, at, read.work, read.err, roundKeptMessage(issue, pod, reasons, claims, read.work, read.err, again))
	if err != nil {
		return false, err
	}
	return s.supervisor.store.DeclinePodReplacement(ctx, s.plan.project, issue, decline)
}

// keptMoment is the moment a round's keep records (recordPodKept): the read's own, readAt, or,
// where a claim of the issue was in a turn at the read, a microsecond before the start of the
// earliest such turn. A turn that runs past the bound runs on in the kept pod and can change the
// workspace after the read, so it is a turn after the record, and the read that may end the record
// is made once it has ended (keptPodReadDue).
func keptMoment(readAt time.Time, group []supervise.Claim) time.Time {
	at := readAt
	for _, c := range group {
		if c.State == supervise.StateWorking && !c.LastTurnStart.IsZero() && !c.LastTurnStart.After(at) {
			at = c.LastTurnStart.Add(-time.Microsecond)
		}
	}
	return at
}

// openPhaseWorker is the claim of the open phase's worker of an issue in issuePhase
// (workflow.OpenPhaseRole), where that claim records a process in round's pod and is not gone: the
// one role a round's keep tells by a message of its own (recordPodKept).
func openPhaseWorker(group []supervise.Claim, round string, issuePhase phase.Phase) (supervise.Claim, bool) {
	role := workflow.OpenPhaseRole(issuePhase)
	if role == "" {
		return supervise.Claim{}, false
	}
	for _, c := range group {
		if c.Role == role && inRoundPod(c, round) && !c.Gone() {
			return c, true
		}
	}
	return supervise.Claim{}, false
}

// podKeptForWork is whether an earlier round's keep of pod, the pod the issue's claims record, still
// stands, so that no round opens over the pod at this sweep (kept; reconcileIssuePodUpdate), and
// whether the read that may end the keep is due over the sweep's snapshot, group (due;
// keptPodReadDue). The keep is the issue's decline row naming pod (keptDeclineOf). reasons is the
// drift this sweep read of the pod, as a round would record it (runtime.RoundReasons). A drift that
// breaks the pod (runtime.Breaks: today a drift of the pod's service account) is held back by no
// keep (spec v23): the round over it opens, and replaces the pod after the read, as any round over
// such a drift does. A store that cannot be read keeps the pod for this sweep: a reading nobody
// took may not open a round that deletes a disk.
//
// The read itself is no part of the walk (the Legion reviewer's point 1): the sweep makes it after
// the walk (podUpdateSweep.keptReads, readKeptPod), which reads the claims again, as their
// machines hold them then, before it reads the workspace. A read made on the walk's snapshot would
// miss a turn that started after the snapshot, and would hold every later issue of the walk for as
// long as it runs.
func (s *supervision) podKeptForWork(ctx context.Context, issue string, group []supervise.Claim, pod, reasons string) (kept, due bool) {
	keep, found, err := s.keptDeclineOf(ctx, issue, pod)
	declined := keep.Decline
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("supervise: read whether an update round kept this issue's pod for its disk; no round opens over the pod at this sweep",
				"issue", issue, "pod", pod, "error", err)
		}
		return true, false
	case !found:
		return false, false
	case runtime.Breaks(reasons):
		s.log.Info("supervise: this issue's pod is kept for work on its disk, and it has drifted in a way that breaks it; an update round opens over it and replaces it after the read of its workspace",
			"issue", issue, "pod", pod, "reason", declined.Reason, "reasons", reasons)
		return false, false
	case !keptPodReadDue(declined, group, s.plan.clock.Now()):
		s.log.Info("supervise: this issue's pod is kept for work on its disk that no branch or tag on origin holds, or for a read that could not be made; no update round opens over it until a read finds nothing that origin does not hold",
			"issue", issue, "pod", pod, "reason", declined.Reason)
		return true, false
	}
	return true, true
}

// keptDeclineOf is the issue's keep record (store.Store.PodKeepOf), with its decline row, and
// found, whether that row is an earlier round's keep of pod: a row naming another pod, or none,
// holds nothing of pod, since the runtime holds another pod for the issue.
func (s *supervision) keptDeclineOf(ctx context.Context, issue, pod string) (store.PodKeep, bool, error) {
	keep, err := s.supervisor.store.PodKeepOf(ctx, s.plan.project, issue)
	if err != nil {
		return store.PodKeep{}, false, err
	}
	declined := keep.Decline
	return keep, !declined.At.IsZero() && declined.PodUID != "" && declined.PodUID == pod, nil
}

// keptPodRead is a read that may end an earlier round's keep of issue's pod, which the walk found
// due (podKeptForWork) and the sweep makes after its walk (podUpdateSweep.keptReads,
// readKeptPod): reasons is the drift the sweep read of the pod, as a round would record it.
type keptPodRead struct {
	issue, pod, reasons string
}

// readKeptPod is the read that may end an earlier round's keep of the issue's pod, which the walk
// found due (podKeptForWork), made after the walk. The moment the read records is taken first,
// then the claims are read again as their machines hold them now, and the read is made only where
// it is still due over them (keptPodReadDue): a turn that started since the walk's snapshot runs
// now, and its read comes once it has ended. The read goes through a live role in the pod where
// one runs, else through the pod's launcher (readPodWorkspace). A read that finds nothing that
// origin does not hold forgets the row (store.Store.ForgetIdleDecline), and the next sweep may
// open a round over the pod. A read that finds work, or that cannot be made, writes the row again
// (recordPodKept): the same work moves its moment and tells nobody, so each turn of the issue
// costs one read. A read or a write that fails leaves the row as it stands, and the next sweep
// decides again.
func (s *supervision) readKeptPod(ctx context.Context, step keptPodRead) {
	issue, pod := step.issue, step.pod
	// inPod is the pod's round name, which names the pod to the readers of a round's pod
	// (podProcess, readPodWorkspace, recordPodKept), though no claim records a round over it now.
	inPod := supervise.PodRound(pod)
	at := s.plan.clock.Now()
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read this issue's claims again before the read of its kept pod; the pod stays kept, and the next sweep decides again",
				"issue", issue, "pod", pod, "error", err)
		}
		return
	}
	group := s.noticeGroup(claimsOf(claims, issue))
	keep, found, err := s.keptDeclineOf(ctx, issue, pod)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: read the keep of this issue's pod again before its read; the pod stays kept, and the next sweep decides again",
				"issue", issue, "pod", pod, "error", err)
		}
		return
	}
	if !found {
		return
	}
	if !keptPodReadDue(keep.Decline, group, at) {
		s.log.Info("supervise: the read of this issue's kept pod is not due over its claims as they stand now; a turn of the issue runs, and the read comes once it has ended",
			"issue", issue, "pod", pod, "working", workingClaims(group))
		return
	}
	process, running := podProcess(group, inPod)
	by := viaIssue
	if running {
		by = viaProcess
	}
	read := s.readPodWorkspace(ctx, issue, inPod, process, by, at)
	if read.clean() {
		if err := s.supervisor.store.ForgetIdleDecline(ctx, s.plan.project, issue); err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: forget the keep of a pod whose read found nothing that origin does not hold; the pod stays kept, and the next sweep decides again",
					"issue", issue, "pod", pod, "error", err)
			}
			return
		}
		s.log.Info("supervise: the read of a kept pod found nothing that no branch or tag on origin holds; the pod is kept no longer, and an update round may open over it",
			append([]any{"issue", issue, "pod", pod}, read.attrs()...)...)
		return
	}
	told, err := s.recordPodKept(ctx, issue, inPod, pod, step.reasons, read, group, keep.Phase, true)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: record the read of a kept pod; no round opens over the pod, and the next sweep reads the workspace again",
				append([]any{"issue", issue, "pod", pod, "error", err}, read.attrs()...)...)
		}
		return
	}
	s.log.Info("supervise: the read of a kept pod found work that no branch or tag on origin holds, or could not be made; the pod stays kept, and no update round opens over it",
		append([]any{"issue", issue, "pod", pod, "told", told}, read.attrs()...)...)
}

// keptPodReadDue is whether the read that may end a round's keep of a pod (podKeptForWork) is due
// at now, over declined, the keep's row, and group, the issue's claims: no claim of the issue is
// in a turn, and a claim of the issue ends the row as it ends a release's decline: it started a
// turn since the row's last read, and that turn has ended (pods.IdleDecline.TurnedSince), or its
// process was started again (StartedSince); or, for a read that could not be made,
// pods.DeclineUnreadRetry has passed (pods.IdleDecline.RetryDue). A role changes the workspace
// only in a turn, so the read is made once for each such turn, once that turn has ended.
func keptPodReadDue(declined pods.IdleDecline, group []supervise.Claim, now time.Time) bool {
	if len(workingClaims(group)) > 0 {
		return false
	}
	return declined.RetryDue(now) || slices.ContainsFunc(group, func(c supervise.Claim) bool {
		return declined.StartedSince(c.LastStartRow) || declined.TurnedSince(c.LastTurnStart)
	})
}

// turnsSince is the role of every claim of group that started a turn after at, the moment a read
// of the workspace began: such a turn can have made work the read did not see, so the last word
// on a replacement refuses it (replacementDue, issueReplacementDue).
func turnsSince(group []supervise.Claim, at time.Time) []string {
	var turned []string
	for _, c := range group {
		if c.LastTurnStart.After(at) {
			turned = append(turned, string(c.Role))
		}
	}
	return turned
}

// podKeptMessage is what the open phase's worker is told when a round kept the issue's pod, pod,
// because the read of its workspace found work that no branch or tag on origin holds (work): what
// the read found, that the round ended with no replacement and the role keeps working in the pod,
// what the round was opened for, and that the pod waits for the push of the work. again is a read
// that may end an earlier keep, made with no round (readKeptPod): it names no round, and the drift
// the daemon reads of the pod in place of a round's reasons (the Legion reviewer's point 10). The
// role reads it in its session and people read it in its history, so it follows ASD-STE100.
func podKeptMessage(issue, pod, reasons string, work runtime.WorkspaceWork, again bool) string {
	opening := fmt.Sprintf("The daemon did not replace this issue's pod (pod %s) for an update round. "+
		"The read of the workspace before the replacement found work that no branch or tag on origin holds.", pod)
	ended := keptEnded("The pod and its disk stay, and you keep working in this pod on its old image and settings.", reasons, again)
	if again {
		opening = fmt.Sprintf("The daemon read this issue's workspace again (pod %s). The pod is kept for its disk, and no update round opens over it. "+
			"The read found work that no branch or tag on origin holds.", pod)
	}
	found := append([]string{opening}, workFound(work)...)
	found = append(found, ended,
		fmt.Sprintf("The daemon does not push for you. Push the work that you keep to legion/%s, and abandon the work that you do not keep. "+
			"The daemon reads the workspace again after each turn of a role of this issue. When a read finds nothing that origin does not hold, a later round replaces the pod. "+
			"You are told this once.", issue))
	return strings.Join(found, " ")
}

// roundKeptMessage is the issue's architect's workspace-kept notice (record.WorkspaceKeptNotice,
// in keptNotice's shape) for a pod a round kept (recordPodKept): the read of its workspace before
// the round's replacement found work that no branch or tag on origin holds (failed nil), or could
// not be made (failed). It names the pod, what the read found or why it failed, the state of each
// claim of the issue, what the round was opened for, that the daemon pushes and saves nothing,
// what ends the keep, and how the work is pushed. The architect reads it in its session and people
// read it in its history, so it follows ASD-STE100. The read's error is the far side's text as
// the daemon keeps it (readError).
func roundKeptMessage(issue, pod, reasons string, claims store.ClaimStates, work runtime.WorkspaceWork, failed error, again bool) string {
	opening := fmt.Sprintf("The daemon did not replace the pod of %s (pod %s) for an update round. The round read the workspace before the replacement.", issue, pod)
	ended := keptEnded("The pod and its disk stay, and the roles keep working in the pod on its old image and settings.", reasons, again)
	if again {
		opening = fmt.Sprintf("The daemon read the workspace of the pod of %s (pod %s) again. The pod is kept for its disk, and no update round opens over it.", issue, pod)
	}
	return keptNotice(issue, claims, work, failed, opening, "The read failed",
		ended,
		"The daemon does not push this work and does not save it. No update round opens over the pod until a later read finds nothing that origin does not hold, or the runtime makes another pod for the issue. "+
			"The daemon reads the workspace again after a role of the issue starts or runs a turn.",
		fmt.Sprintf("To keep the work, have a role of %s push it to legion/%s. If no role of %s runs, start one in this pod, for example with `legion claims resume` or `legion claims retry`. "+
			"You are told this once for this pod.", issue, issue, issue))
}

// keptEnded is what a keep's message says of the round after stays, the sentence that says the pod
// stays and who keeps working in it (podKeptMessage, roundKeptMessage): that the round ended with no
// replacement and what it was opened for, or, for a read that may end an earlier keep, made with no
// round (again), the drift the daemon reads of the pod in place of a round's reasons.
func keptEnded(stays, reasons string, again bool) string {
	if again {
		return stays + " The drift the daemon reads of the pod is: " + reasons + "."
	}
	return "The round ended with no replacement. " + stays + " " + roundOpenedFor(reasons)
}

// workspaceTakenMessage is the issue's architect's workspace-taken notice for a replacement round
// made of pod although the read of its workspace before it found work that no branch or tag on
// origin holds (work, failed nil), or failed (failed): why the round replaced the pod, the roles of
// named with their states, what the read found, and whether the work is gone or on a kept volume
// (kept, read once the replacement is made). A round over a drift its roles can keep working
// under replaced the pod only because its disk was a kept volume when the round read it
// (roundKeeps); where the replacement then found the volume failed and marked it for a reset, kept
// is false, and the notice says the next launch deletes the volume (the Legion reviewer's point
// 9). The architect reads it in its session and people read it in its history, so it follows
// ASD-STE100. The read's error is the far side's text as the daemon keeps it (readError).
func workspaceTakenMessage(issue, round, pod, reasons string, named []supervise.Claim, work runtime.WorkspaceWork, failed error, kept bool) string {
	breaks := runtime.Breaks(reasons)
	why := "The round was over a drift that breaks the pod, so no role could keep working in the pod."
	gone, lost := "The pod and its disk are gone.", "Work that is not on a branch at origin is lost."
	switch {
	case kept:
		if !breaks {
			why = "The pod's disk is a kept volume that the issue's next pod mounts, so the replacement did not delete the disk."
		}
		gone = "The pod is gone. Its disk is a kept volume, and the volume is still there."
		lost = "Work that is not on a branch at origin is on that volume. The next pod of this issue mounts the volume."
	case !breaks:
		why = "The pod's disk was a kept volume when the round read it, so the round replaced the pod. The replacement found that the volume failed, and marked it for a reset."
		gone = "The pod is gone. The next launch of this issue deletes its volume and makes a new one."
	}
	parts := []string{fmt.Sprintf("The daemon replaced the pod of %s (pod %s) for the update round %s. %s %s %s", issue, pod, round, roundOpenedFor(reasons), why, gone)}
	if len(named) > 0 {
		parts = append(parts, "These roles had not answered the round, and no process of these roles ran in the pod: "+roleStates(named)+".")
	}
	parts = append(parts, readFound(work, failed), lost,
		"Each role must read its issue branch at origin to see the work that it pushed. You are told this once for this replacement.")
	return strings.Join(parts, " ")
}

// launchTakenMessage is the issue's architect's workspace-taken notice for a launch's own
// replacement of an issue pod (runtime.TakenPod): what its act took and why the launch made it —
// the pod and its disk, or, for an act that takes the volume (runtime.TakesVolume), the issue's
// Sandbox, named by its uid, and the kept volume it owned, with the live pod of that Sandbox that
// went with them, for an act that took that pod too (runtime.TakenPod.WithPod), or else the last
// pod known to have used the volume where there is one — what the read of its workspace through
// the pod's launcher found before the replacement, or why no read could be made, and, once, that
// work no branch at origin holds is lost (the Legion reviewer's Minor on #5285 at 7da847c0). The
// architect reads it in its session and people read it in its history, so it follows ASD-STE100.
func launchTakenMessage(taken runtime.TakenPod) string {
	role := string(taken.Claim)
	if named, ok := taken.Claim.Role(); ok {
		role = string(named)
	}
	why := taken.Why
	if why == "" {
		why = "the launch could not start the role in that pod"
	}
	took := fmt.Sprintf("A launch of the %s replaced the pod of %s (pod %s) on its own account, because %s. The pod and its disk are gone.",
		role, taken.Issue, taken.PodUID, why)
	if taken.Act.Takes == runtime.TakesVolume {
		took = fmt.Sprintf("A launch of the %s deleted the Sandbox of %s (uid %s) and the kept volume it owned, which was the disk of the issue's pods, on its own account, because %s.",
			role, taken.Issue, taken.Act.Target, why)
		switch {
		case taken.WithPod:
			took += fmt.Sprintf(" The Sandbox ran pod %s, and the pod went with it.", taken.PodUID)
		case taken.PodUID != "":
			took += fmt.Sprintf(" The last pod that used the volume was pod %s.", taken.PodUID)
		}
	}
	return strings.Join([]string{
		took,
		readFound(taken.Work, taken.Err),
		"Work that is not on a branch at origin is lost. Each role must read its issue branch at origin to see the work that it pushed. You are told this once for this replacement.",
	}, " ")
}

// readFound is what a workspace-taken notice says the read before the replacement found
// (workspaceTakenMessage, launchTakenMessage): the error it failed with, or that no read could be
// made and why, or what the workspace held, in workSentences's sentences.
func readFound(work runtime.WorkspaceWork, failed error) string {
	if failed != nil {
		// The notice is stored and read by the issue's architect, and in its place by the
		// controller, so the read's error, which the shim's or the launcher's jj wrote, is kept in it
		// as the daemon keeps a read's error: bounded, and quoted as data (readError).
		return fmt.Sprintf("No read of the workspace could be made before the replacement. The error of the read, in quotes, is: %s. The daemon does not know if the workspace held work that no branch or tag at origin holds.",
			readError(failed))
	}
	found := append([]string{"Before the replacement, the daemon read the issue workspace. The read found work that no branch or tag at origin holds."},
		workSentences(work, "held", "that no branch or tag at origin holds")...)
	return strings.Join(found, " ")
}
