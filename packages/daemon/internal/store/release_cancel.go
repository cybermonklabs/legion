package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// This file is the cancel of an idle release that did not finish (CancelIssueRelease), the cancel a
// workspace check makes when it declines the release (DeclineIssueRelease), and the helpers only the
// cancel uses. The release's mark, its rows, its decline's record and its teardown's decisions are
// in idle.go and issue_release.go.

// DroppedWake is what a cancel deleted of one role's wake (dropWakesUnderRelease): Kind, Detail
// and Task, the wake kind, the detail and the task of the start it deleted
// (record.SuperviseRequest.WakeKind, WakeDetail and Task), each "" where the cancel found no start
// of the wake. Kind is the kind that the start's task was rendered with (workflow.WakeTask), and
// the builder needs that kind to render the task again. The role's newest resume can be the
// resume of another wake of the role, of another kind. The zero DroppedWake is a role whose wake
// the cancel did not drop. A start that an earlier daemon queued carries no detail, so its task
// cannot be rendered again (retellStarts says why), and the cancel hands its builder the zero
// DroppedWake for that role.
type DroppedWake struct {
	Kind, Detail, Task string
}

// ReleaseWokenRow builds the outbox row that brings one claim a cancelled release suspended back,
// at the generation the cancel read of the issue, told what the cancel dropped of the role's wake
// (DroppedWake). It is the caller's because the payload is record's to encode, as every row of a
// release is (IdleReleaseRow), and because what of the dropped wake the resume's own task carries
// is the workflow's rule (workflow.CancelledWakeTask).
type ReleaseWokenRow func(role claim.Role, generation uint64, wake DroppedWake) (IdleReleaseRow, error)

// CancelIssueRelease ends a release that did not finish, in one transaction: the resume of each
// claim this release suspended and still holds is queued, and the mark goes with it. The two are
// one act because either alone is a wrong state of the pod: a mark deleted before the resumes are
// durable leaves those roles `released` in a pod that still stands, with no backstop — a release
// arms one only once its pod is gone — and nothing left to read that says they are owed a
// process, since the next row of the release reads the release as over and the next idle tick
// sees a pod whose claims are suspended. Queueing the resumes rather than relaunching here is what
// makes them durable: the row is the daemon's own record of what the pod is owed, and its lane
// retries it until the role has it back.
//
// woken builds the row that resumes each claim (ReleaseWokenRow). It is told the kind, the detail
// and the task of the wake the cancel dropped for that role, and the zero DroppedWake for a role
// whose wake it did not drop.
//
// It answers the roles it queued a resume for (ReleaseCancelled.Resumed): the claims this release
// suspended that are still suspended as `released`, less a role whose stop the mark does not
// record that a queued start already brings back (below). A claim another path took over — a
// close, a linger, an operator — is left to that path, which is what the source on the suspension
// says; one an earlier release left `released` is no business of this release, which is why the
// mark's own set and this release's own stops are read and never every released claim of the
// issue.
//
// A wake of a role this release recorded as `released` can come before the release ends, and the
// release goes on then (releaseOvertaken). A cancel can still follow that wake, for another cause:
// a role stop that met a live start or a turn of another role, or an issue that left the workflow.
// Then the pod never goes away, and the wake brings nothing back. So, for each role of the mark's
// set that is still suspended as `released` and has a wake recorded above the mark's wake_row,
// this transaction deletes those resumes and the starts the wake queued (wakeStartRow, above the
// release's last row), and then queues the role's resume as above. Those starts have not run: a
// start waits while the release's teardown row is in the outbox (record's outboxInOrder), and
// that row is still there whichever row runs the cancel. So the role starts once, in the pod it
// was in, with no task that says its pod was released, and no resume is left for a release that
// did not happen. A root architect's entry start and a production check's look of that role name
// no phase either, but they are work the pod was given, not a wake's start, and they stay. The
// cause of the wake still reaches the role: a message that Envoy stored or kept arrives in the
// role's session, and the text of a CI settlement or of the backstop, which reach the role only
// through the wake's task (workflow's tellChecks tells the woken role no message of its own), goes
// to the role in the cancel's resume.
// The builder is told the kind, the detail and the task of the wake dropped for each role
// (DroppedWake), and its text says that the release did not finish rather than that the pod came
// back. A cancel made while the project is paused drops no wake of a released role; the paragraph
// on the pause below says why.
//
// Two kinds of role that this release stopped keep the start of their wake. Each such start says
// that the release took the pod, and the release took none, so the cancel tells it in the words of
// a release that did not finish (ReleaseCancelPause.TakenOverWakeTask, retellWakesOf), paused or
// not. The first is a role of the mark's set whose claim a close, a linger or an operator took
// over after its wake: its start is that path's (dropTakenOverWakes), and no cancel resumes it.
// The second is a role whose stop suspended its claim as `released` and did not record it on the
// mark (unrecordedStops): the stop records the role in a transaction of its own after the
// suspension (ReleaseSuspendedClaim), and that record can fail, or the daemon can stop before it.
// A wake of such a role reads as a wake of a role outside the set, which cancels the release
// (releaseOvertaken), and its start is the role's one start, in the pod that stayed: the cancel
// queues no resume of a role whose start is queued for the issue's generation (startQueued, as a
// wake reads it before it takes anything). A role of this kind with no start queued, as where no
// wake of it was taken, is resumed as a role of the set is, or recorded stopped by the pause while
// the project is paused (below): nothing else brings it back, since its stop's row, run again,
// reads the release over, and a pod that stayed arms no backstop. While the claim is still
// `released`, the cancel keeps that wake's resume and makes it pair with release 0, since no
// release took the pod (pairUnrecordedWakesWithNoRelease). Where a close, a linger or an operator
// took that claim over too, its start is that path's, as for a role of the set, and the cancel
// deletes the wake's resume by the same rule (dropTakenOverWakes). A role this release did not
// stop is not told again: one an earlier release left `released` and a wake brought back while
// this release stood. That earlier release did take its pod, so the start's words that its pod was
// released are true.
//
// Beside them it answers whether the release it ended was one that suspends no role
// (ReleaseCancelled.SuspendsNoRole, IdleReleaseMark.SuspendsNoRole, issue_releases.kind): the
// mark's kind, read in the same transaction that reads its id and its suspended set, since the
// cancel reads the mark itself and its caller holds none. The caller needs it to say what it
// cancelled — a release that suspended no role resumes none, so an empty answer there is the
// whole story rather than a release whose claims another path took over.
//
// A cancel made while the project is paused queues no resume (decision 12 of
// dispatch://INFRA-3759): the pause holds every role of the project, and a resume would be a
// start the pause holds anyway. It records each role it would have resumed as stopped by that
// pause instead (stopReleasedForPause), with the pod held and the backstop the stop transaction
// arms, and answers the pause and those roles (ReleaseCancelled.Pause, Stopped). paused is what
// that needs of the workflow and the clock. The pause is project's, read by its token
// (LockOpenPauseByTokenTx; readOpenPauseByToken says why). No production row reaches a cancel
// for another project's issue: a cancel runs from a row of the release, which is keyed by its
// issue, and the daemon's outbox runs only the rows of its own project's keys (record's
// DueRows). It is read after the issue's lock (LockIssueForPause), which this transaction takes
// after the serializer, the one order every writer of a pod hold keeps (decision 13). A wake that
// a role of the mark's set took since the mark stays open in this case: it is the launch the pause
// owes the role, which stopReleasedForPause records as owed (openPodResume), and its start runs
// at the pause's resume. Dropped here, as a cancel drops it while the project runs, it would leave
// the role no launch at all, since this cancel queues no resume. The task of that start was
// written for a session a release stopped, and says that the release took the pod; the release
// took none, and the cancel records the role as stopped by the pause. So this transaction rewrites
// that task as the task of a wake of a role the pause stopped (ReleaseCancelPause.WakeTask,
// retellWakesOf). A wake kept so that was taken while the project ran is no wake of the
// pause yet: no pause holds its resume, and its resume names the newest release recorded before
// the wake, which is not the release it waited for. The cancel makes it a held wake of the pause
// (holdWakesUnderPause): its resume names the pause and release 0, as the resume of a wake taken
// while the project is paused does (decision 15).
//
// An issue the record no longer holds, one that has left the workflow, and one whose tree
// lingers are given no resume at all, and the mark goes all the same: those claims are a close's
// to end. The gate is the one every path that acts on a released pod reads
// (pods.InWorkflowNotLingering), read here inside the cancel's own transaction, because the
// cancel of a release whose issue left the workflow runs beside that issue's close — the close's
// rows are queued above the release's and the teardown answers Unauthorized for the same leave —
// and a resume queued above them supersedes the close: the close's own suspension then finishes
// without acting, its Sandbox is never deleted, and the roles of a done issue run on in it with
// their launcher tokens and `gh` credentials. outOfWorkflow is the workflow's rule for a status
// that takes an issue out of it (record.OutOfWorkflow), which the store does not restate.
//
// The transaction takes the shared serializer before that read, as every other writer of the
// issue's lifecycle does (ReleaseIdleIssue, IssueRelease, IssueSuspension), and the lock rather
// than the read is what holds the gate. A park or a close is one fact transaction that moves the
// issue's status and queues the close's rows together, so without the lock a cancel can read the
// status from before that transaction and still take its resume's id from the sequence after it:
// a resume standing above a close that committed, which is the one order the gate exists to
// refuse. Under the lock the two commit in a line, and neither order leaves that state — a cancel
// that runs first queues its resume below the close's rows, which supersede it, and one that runs
// after reads the issue as left and queues nothing.
//
// The cancel reads the mark `for update`, after the serializer and before it deletes the wakes
// taken under the mark (dropWakesUnderRelease). A take (PodRecords.TakePodWake) locks the same
// mark `for share` before it records its resume. The workflow's takes also hold the serializer,
// but the outbox's delivery take does not, so the mark is the one lock every take shares with the
// cancel. When a take locks the mark first, this read waits until the take commits, and the
// delete then reads the take's resume and drops it. When this read comes first, the take waits
// until this transaction commits, and then finds the mark gone. Without the lock, a take still
// open when the delete runs leaves its resume behind, under a release that did not happen. The
// cancel takes the issue's pause lock and the pause's row after the mark, and before it writes or
// deletes a start or a resume, as a take orders them. It takes them whenever it writes for a role:
// when the mark records a role, and when a stop of the release that the mark does not record is
// still in the outbox (unrecordedStops). So a project's resume, which writes the starts the pause
// holds and the open resumes under the pause's row, never waits for a start or a resume this
// cancel wrote while this cancel waits for that row. PodRecords.TakePodWake gives the whole lock
// order.
func (s *Store) CancelIssueRelease(ctx context.Context, project, issue string, woken ReleaseWokenRow,
	outOfWorkflow func(status string) bool, paused ReleaseCancelPause) (ReleaseCancelled, error) {
	return s.cancelIssueRelease(ctx, project, issue, woken, outOfWorkflow, paused, nil)
}

// DeclineIssueRelease cancels a release whose check declined it, as CancelIssueRelease does, and in
// the same transaction records the decline (recordDecline). The decline is written only where the
// cancel found the release's mark: a check of a release that is over records nothing and tells
// nobody, so a check its lane runs again after the decline committed writes no second row and no
// second message. ReleaseCancelled.Declined says whether it was written, and Told whether its
// message was queued.
func (s *Store) DeclineIssueRelease(ctx context.Context, project, issue string, decline IdleReleaseDecline, woken ReleaseWokenRow,
	outOfWorkflow func(status string) bool, paused ReleaseCancelPause) (ReleaseCancelled, error) {
	if err := decline.check(issue); err != nil {
		return ReleaseCancelled{}, err
	}
	var told bool
	cancelled, err := s.cancelIssueRelease(ctx, project, issue, woken, outOfWorkflow, paused, func(tx pgx.Tx) (err error) {
		told, err = recordDecline(ctx, tx, project, issue, decline)
		return err
	})
	cancelled.Told = told && cancelled.Declined
	return cancelled, err
}

// cancelIssueRelease is CancelIssueRelease, and DeclineIssueRelease where declined is not nil:
// declined runs in the cancel's transaction once the mark is read, before the mark is deleted, and
// only where a mark stood that the cancel may end.
func (s *Store) cancelIssueRelease(ctx context.Context, project, issue string, woken ReleaseWokenRow,
	outOfWorkflow func(status string) bool, paused ReleaseCancelPause, declined func(pgx.Tx) error) (ReleaseCancelled, error) {
	if project == "" || issue == "" || woken == nil || outOfWorkflow == nil {
		return ReleaseCancelled{}, errors.New("cancelling a release needs its project, its issue, the row that resumes a claim and the workflow's own rule for a status")
	}
	if paused.At.IsZero() {
		return ReleaseCancelled{}, errors.New("cancelling a release needs the moment a pause's stops would carry (ReleaseCancelPause.At), from the caller's clock")
	}
	var cancelled ReleaseCancelled
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		cancelled = ReleaseCancelled{}
		var first, wake, last, recorded int64
		var suspended []string
		var kind string
		var committed bool
		err := tx.QueryRow(ctx, `select suspended, kind, first_row, wake_row, last_row, committed,
			coalesce((select p.id from pod_releases p where p.issue_release_id = issue_releases.id), 0) from issue_releases
			where project = $1 and issue = $2 for update`, project, issue).Scan(&suspended, &kind, &first, &wake, &last, &committed, &recorded)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// A release whose teardown recorded it has taken its pod (ReleaseDecision.PodTaken), and
		// one whose teardown the runtime was authorized to carry out may have taken it
		// (ReleaseDecision.Committed). Nothing undoes that: its teardown finishes it, and no cancel
		// may resume its roles in a pod that is gone.
		if recorded != 0 {
			return fmt.Errorf("the release of %s took its pod and was recorded as release %d, so it cannot be cancelled: its teardown finishes it", issue, recorded)
		}
		if committed {
			return fmt.Errorf("the release of %s was authorized to take its pod, so it cannot be cancelled: its teardown finishes it", issue)
		}
		cancelled.SuspendsNoRole = kind == releaseKindUnowned
		if declined != nil {
			if err := declined(tx); err != nil {
				return err
			}
			cancelled.Declined = true
		}
		roles, err := stillReleasedRoles(ctx, tx, project, issue, suspended)
		if err != nil {
			return err
		}
		stillReleased := releasedRoleNames(roles)
		// The roles this release stopped whose wake's start, where a wake of them was taken, is the
		// role's one start: the roles whose stop the mark does not record, and the roles of the set
		// that another path took over. paired are the roles whose stop the mark does not record and
		// whose claim is still `released`; the cancel resumes one of them only where no start of it
		// is queued (below). takenOver are the roles of either set whose claim is not suspended as
		// `released`: another path took it over, or the role's stop has not run yet. The reads
		// write nothing, so they come before the locks below.
		unrecorded, err := unrecordedStops(ctx, tx, issue, first, last, suspended)
		if err != nil {
			return err
		}
		unrecordedReleased, err := stillReleasedRoles(ctx, tx, project, issue, unrecorded)
		if err != nil {
			return err
		}
		paired := releasedRoleNames(unrecordedReleased)
		var takenOver []string
		for _, role := range slices.Concat(suspended, unrecorded) {
			if !slices.Contains(stillReleased, role) && !slices.Contains(paired, role) {
				takenOver = append(takenOver, role)
			}
		}
		var standing issueStanding
		if len(roles) > 0 || len(unrecordedReleased) > 0 {
			if standing, err = resumeStanding(ctx, tx, issue, outOfWorkflow); err != nil {
				return err
			}
		}
		var held Pause
		var open bool
		if len(suspended) > 0 || len(unrecorded) > 0 {
			// The issue's pause lock and the pause's row come after the mark and before any start
			// or resume is written or deleted, as a take orders them (PodRecords.TakePodWake).
			if err := LockIssueForPause(ctx, tx, issue); err != nil {
				return err
			}
			if held, open, err = LockOpenPauseByTokenTx(ctx, tx, project); err != nil {
				return err
			}
		}
		// A paired role with a start queued for the issue's generation, its wake's, keeps that
		// start as its one start. One with none, as where no wake of it was taken, joins roles: the
		// cancel resumes it, or records it stopped by the pause while the project is paused, as it
		// does a role of the set.
		for _, r := range unrecordedReleased {
			queued, err := startQueued(ctx, tx, issue, r.role, standing.generation)
			if err != nil {
				return err
			}
			if !queued {
				roles = append(roles, r)
			}
		}
		// Each of those roles is told in the words of a release that did not finish. The retell
		// comes before dropTakenOverWakes deletes the resume that the retell reads as the wake
		// taken under the mark. The resume of a paired role stays, and it pairs with release 0
		// from here on, whether the project runs or is paused (pairUnrecordedWakesWithNoRelease).
		if err := retellWakesOf(ctx, tx, project, issue, slices.Concat(takenOver, paired), wake, last, paused.TakenOverWakeTask); err != nil {
			return err
		}
		if err := pairUnrecordedWakesWithNoRelease(ctx, tx, project, issue, paired, wake); err != nil {
			return err
		}
		if err := dropTakenOverWakes(ctx, tx, project, issue, takenOver, wake); err != nil {
			return err
		}
		if len(roles) > 0 && open && standing.live {
			if err := stopReleasedForPause(ctx, tx, project, issue, held, roles, standing, paused); err != nil {
				return err
			}
			if err := holdWakesUnderPause(ctx, tx, project, issue, stillReleased, wake, held.ID); err != nil {
				return err
			}
			if err := retellWakesOf(ctx, tx, project, issue, stillReleased, wake, last, paused.WakeTask); err != nil {
				return err
			}
			cancelled.Pause = held.ID
			for _, r := range roles {
				cancelled.Stopped = append(cancelled.Stopped, r.role)
			}
			return deleteIssueRelease(ctx, tx, project, issue)
		}
		dropped, err := dropWakesUnderRelease(ctx, tx, project, issue, stillReleased, wake, last)
		if err != nil {
			return err
		}
		if !standing.live {
			roles = nil
		}
		for _, r := range roles {
			row, err := woken(r.role, standing.generation, dropped[r.role])
			if err != nil {
				return err
			}
			if _, err := insertOutboxRow(ctx, tx, project, issue, row); err != nil {
				return err
			}
			cancelled.Resumed = append(cancelled.Resumed, r.role)
		}
		return deleteIssueRelease(ctx, tx, project, issue)
	})
	if err != nil {
		return ReleaseCancelled{}, fmt.Errorf("cancel the release of %s: %w", issue, err)
	}
	return cancelled, nil
}

// ReleaseCancelPause is what a cancel made while the project is paused needs beside the store
// (decision 12 of dispatch://INFRA-3759): the moment the stops it records, the pod's hold and a
// backstop's due time are stamped with (At, the caller's clock), the backstop the stop of the
// issue's open phase's worker arms (the idle release's backstop_minutes; zero arms none, as
// where the deployment releases no pod), and which role works an issue's open phase
// (workflow.OpenPhaseRole), which the store does not restate (nil arms no backstop). At is
// required of every cancel, since whether the project is paused is read inside the cancel's own
// transaction; a cancel made while the project runs reads nothing else of it.
//
// WakeTask is the workflow's rule for the task of a wake the cancel keeps (workflow.PausedWakeTask):
// it answers, from the wake kind and the detail the wake's start carries
// (record.SuperviseRequest.WakeKind and WakeDetail) and the task it carries now, the task that
// start carries from now on. The store does not restate the wake's words. nil rewrites no task.
//
// TakenOverWakeTask is the workflow's rule for the task of a wake's start that every cancel keeps,
// paused or not, for a role this release stopped and no cancel resumes: a role whose claim a
// close, a linger or an operator took over after the wake, and a role whose stop the mark does not
// record (workflow.TakenOverWakeTask, CancelIssueRelease). It answers, from the wake kind, the
// detail and the task the start carries, the task that start carries from now on. nil rewrites
// no task.
type ReleaseCancelPause struct {
	At                time.Time
	Backstop          time.Duration
	OpenPhaseRole     func(phase.Phase) claim.Role
	WakeTask          func(kind, detail, task string) string
	TakenOverWakeTask func(kind, detail, task string) string
}

// ReleaseCancelled is what one cancel of a release did (CancelIssueRelease). Resumed are the roles
// it queued a resume for, and SuspendsNoRole whether the release was one that suspends no role.
// Stopped are the roles it recorded stopped by the project's open pause in place of resuming
// them, and Pause that pause; Pause is 0 where the cancel recorded no role stopped: while the
// project runs, and while it is paused where the cancel found no role to resume. A cancel that
// met a pause resumes none. Declined is whether a decline found the release's mark and recorded
// the decline (DeclineIssueRelease), and Told whether it queued the decline's message, which a
// decline that found the work on record does not; a cancel answers false for both.
type ReleaseCancelled struct {
	Resumed        []claim.Role
	SuspendsNoRole bool
	Pause          int64
	Stopped        []claim.Role
	Declined       bool
	Told           bool
}

// stopReleasedForPause records, for a cancel made while held is open, each role the release had
// stopped as stopped by that pause, through RecordPauseStopTx, the writer the stop transaction
// records a role the pause stops by (internal/daemon's projectPause.stopTx, decision 6 of
// dispatch://INFRA-3759): its stop, owing a launch where a wake taken since the release already
// owes it one (openPodResume); on the first record of its stop, the pod's hold, whatever the
// pod's other roles do, as the stop transaction holds it while its pause stands: an update round
// past its bound or an idle rule would otherwise take a pod that holds the stopped roles'
// unpushed work; and, for the worker of the issue's open phase serving the issue's generation, its
// backstop, which the record arms only where the stop owes no launch. A wake taken since is left
// open: it is the launch the role is owed, and its start row runs at the resume. The caller holds
// the issue's lock and held's row for share.
func stopReleasedForPause(ctx context.Context, tx pgx.Tx, project, issue string, held Pause, roles []releasedRole,
	standing issueStanding, paused ReleaseCancelPause) error {
	at := paused.At.UTC()
	for _, r := range roles {
		_, _, owed, err := openPodResume(ctx, tx, project, issue, r.role)
		if err != nil {
			return err
		}
		rec := PauseStopRecord{Pause: held, Stop: PauseStopClaim{Claim: r.token, Tree: r.tree, Issue: issue,
			Role: r.role, Generation: r.generation}, At: at, LaunchOwed: owed, HoldPod: true}
		if paused.Backstop > 0 && paused.OpenPhaseRole != nil &&
			r.role == paused.OpenPhaseRole(standing.phase) && r.serving == standing.generation {
			rec.Backstop = &PodWakeBackstop{Project: project, Issue: issue, Role: r.role, DueAt: at.Add(paused.Backstop)}
		}
		if _, err := RecordPauseStopTx(ctx, tx, rec); err != nil {
			return err
		}
	}
	return nil
}

// holdWakesUnderPause makes each wake that a paused cancel keeps a held wake of pause
// (CancelIssueRelease): each resume of one of roles above wake, the mark's wake_row, that no pause
// holds. Such a wake was taken while the project ran. Its resume names the newest release
// recorded before the wake (LatestPodRelease), and that release is not the release the wake
// waited for, so the records would count a pod's return for a release that did not take that
// pod. The cancel keeps the wake as the launch that the pause owes the role, so its resume now
// names the pause and release 0, as every held wake's resume does
// (pod_resumes_held_wake_pairs_with_no_release, decision 15 of dispatch://INFRA-3759). Its kind,
// its times and its seen release stay. These are the resumes that dropWakesUnderRelease deletes
// while the project runs. roles are the names of the claims of the mark's set that are still
// suspended as `released` (stillReleasedRoles). The cancel reads those names once and gives the
// same names to each of its acts on them.
func holdWakesUnderPause(ctx context.Context, tx pgx.Tx, project, issue string, roles []string, wake, pause int64) error {
	if len(roles) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `update pod_resumes set pause_id = $5, release_id = 0
		where project = $1 and issue = $2 and id > $3 and role = any($4) and pause_id is null`,
		project, issue, wake, roles, pause); err != nil {
		return fmt.Errorf("make the wakes taken while the release of %s stood held wakes of pause %d: %w", issue, pause, err)
	}
	return nil
}

// unrecordedStops are the roles of the stops of the release whose rows run from first to last
// (issue_releases.first_row and last_row) that are still in the outbox, less the roles the mark
// records (suspended): the roles this release stopped, or is still to stop, that its mark does
// not name. A stop's row leaves the outbox only once its stop has recorded its role on the mark
// (ReleaseSuspendedClaim), since that record is the row's last act and its error fails the row.
// So a role whose stop suspended its claim as `released` and whose record failed, or did not run
// before the daemon stopped, is here, and so is a role whose stop has not run yet, whose claim is
// live and has no wake to tell again. A role an earlier release left `released` is not: this
// release queued no stop for a claim that was not live.
func unrecordedStops(ctx context.Context, tx pgx.Tx, issue string, first, last int64, suspended []string) ([]string, error) {
	rows, err := tx.Query(ctx, `select distinct payload->>'role' from outbox where issue = $1 and id between $2 and $3
		and kind = 'supervise' and payload->>'op' = 'release' and not (payload->>'role' = any(coalesce($4::text[], '{}')))
		order by 1`, issue, first, last, suspended)
	if err != nil {
		return nil, fmt.Errorf("read the stops of the release of %s that its mark does not record: %w", issue, err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("read the stops of the release of %s that its mark does not record: %w", issue, err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stops of the release of %s that its mark does not record: %w", issue, err)
	}
	return roles, nil
}

// retellWakesOf rewrites the task of each start of one of roles above last, the release's last
// row, that has a wake's own shape (wakeStartRow) and whose role has a resume above wake, the
// mark's wake_row. It renders the task again from the wake kind and the detail the start carries
// (record.SuperviseRequest.WakeKind and WakeDetail), which are what its task was rendered with,
// and from the task the start carries now. The role's resume shows only that a wake of the role
// was taken under the mark: its newest resume can be another wake's, of another kind. A start of
// a role with no such resume is no wake's, and keeps its task. A root architect's entry start and
// a production check's look are no wake's start, and keep their task too. It is the statement of
// both retells of a cancel (CancelIssueRelease): a paused cancel's, of the wakes it keeps for the
// roles of the set still released (ReleaseCancelPause.WakeTask), and every cancel's, of the roles
// this release stopped that no cancel resumes (ReleaseCancelPause.TakenOverWakeTask).
// retellStarts writes the new tasks, and a nil retell writes nothing.
func retellWakesOf(ctx context.Context, tx pgx.Tx, project, issue string, roles []string, wake, last int64,
	retell func(kind, detail, task string) string) error {
	if retell == nil || len(roles) == 0 {
		return nil
	}
	return retellStarts(ctx, tx, issue, retell, `select outbox.id, outbox.payload->>'role', coalesce(outbox.payload->>'task', ''),
		outbox.payload->>'wakeKind', outbox.payload->>'wakeDetail' from outbox
		join lateral (select 1 from pod_resumes r where r.project = $1 and r.issue = $2 and r.id > $3
			and r.role = outbox.payload->>'role' limit 1) woken on true
		where outbox.issue = $2 and outbox.id > $5 and `+wakeStartRow+` and outbox.payload->>'role' = any($4)
		order by outbox.id`, project, issue, wake, roles, last)
}

// issueStanding is what a cancel reads of its issue before it queues a resume (resumeStanding):
// the generation the resume names, the phase whose worker a paused cancel arms a backstop for,
// and whether the issue and its tree root are both still the workflow's to run
// (pods.InWorkflowNotLingering).
type issueStanding struct {
	generation uint64
	phase      phase.Phase
	live       bool
}

// resumeStanding is issue's standing as a cancel reads it. An issue or a root the record no
// longer holds is not live.
func resumeStanding(ctx context.Context, tx pgx.Tx, issue string, outOfWorkflow func(status string) bool) (issueStanding, error) {
	var generation int64
	var issuePhase, status, rootPhase, rootStatus string
	var lingering, rootLingering bool
	err := tx.QueryRow(ctx, `select i.generation, i.phase, i.status, i.linger_until is not null,
		root.phase, root.status, root.linger_until is not null
		from issues i join issues root on root.key = i.tree where i.key = $1`, issue).Scan(
		&generation, &issuePhase, &status, &lingering, &rootPhase, &rootStatus, &rootLingering)
	if errors.Is(err, pgx.ErrNoRows) {
		return issueStanding{}, nil
	}
	if err != nil {
		return issueStanding{}, fmt.Errorf("read %s for the resumes of its cancelled release: %w", issue, err)
	}
	live := pods.InWorkflowNotLingering(
		pods.Standing{Phase: phase.Phase(issuePhase), Status: status, Lingers: lingering},
		pods.Standing{Phase: phase.Phase(rootPhase), Status: rootStatus, Lingers: rootLingering},
		outOfWorkflow)
	return issueStanding{generation: uint64(generation), phase: phase.Phase(issuePhase), live: live}, nil
}

// releasedRole is one claim a release suspended that is still suspended as `released`: the role
// a cancel resumes, or records stopped by the pause, with what that record names of the claim.
type releasedRole struct {
	token      claim.Token
	tree       string
	role       claim.Role
	generation uint64
	serving    uint64
}

// stillReleasedRoles are the claims among suspended that are still suspended as `released`, in
// role order.
func stillReleasedRoles(ctx context.Context, tx pgx.Tx, project, issue string, suspended []string) ([]releasedRole, error) {
	if len(suspended) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select token, tree, role, generation, serving_generation from claims where project = $1 and issue = $2
		and role = any($3) and state = 'suspended' and suspended_by = 'released' order by role`,
		project, issue, suspended)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []releasedRole
	for rows.Next() {
		var r releasedRole
		var token, role string
		var generation, serving int64
		if err := rows.Scan(&token, &r.tree, &role, &generation, &serving); err != nil {
			return nil, err
		}
		r.token, r.role, r.generation, r.serving = claim.Token(token), claim.Role(role), uint64(generation), uint64(serving)
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// releasedRoleNames is the role of each of roles, as the claim and pod tables hold it.
func releasedRoleNames(roles []releasedRole) []string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, string(r.role))
	}
	return names
}

// dropTakenOverWakes deletes the resumes of the wakes taken under the mark (above wake, the mark's
// wake_row) of each of takenOver: the roles this release stopped whose claim is not suspended as
// `released` (CancelIssueRelease). One rule holds for the roles of the mark's suspended set and
// for the roles whose stop the mark does not record (unrecordedStops). A close, a linger or an
// operator took such a claim over after the wake. A role whose stop has not run yet is among them
// too, but its claim is live, so no wake of it was taken and the delete finds nothing of it. The
// release did not finish, so no pod of it went away, and such a resume is no pod's return. Left
// behind, it would pair with the newest release recorded before the wake, and the records would
// count a pod return for a release that did not take that pod. With no first turn it would also be
// warned and recorded stuck, and it would refuse the role's next wake until a later release closed
// it. The wake's own start stays: it is the start of the path that took the claim over. It
// finishes without acting under a close or a linger (decision 10 of dispatch://INFRA-3759), and it
// waits for an operator's resume under an operator's suspension. The cancel has already told that
// start in the words of a release that did not finish (retellWakesOf), which read this resume as
// the wake taken under the mark before this delete.
//
// A pause's owed launch is kept whole (kind pods.WakePause). The stop transaction of a project's
// pause takes it for a role whose launch the pause owes (internal/daemon's projectPause.stopTx),
// and the pause's resume runs its start. That resume is the record that the pause still owes the
// launch (openPodResume), which no cancel of a release settles. A cancel calls this with the
// issue's pause lock and the pause's row held (CancelIssueRelease).
func dropTakenOverWakes(ctx context.Context, tx pgx.Tx, project, issue string, takenOver []string, wake int64) error {
	if len(takenOver) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `delete from pod_resumes where project = $1 and issue = $2 and id > $3 and role = any($4)
		and kind <> $5`, project, issue, wake, takenOver, pods.WakePause); err != nil {
		return fmt.Errorf("delete the wakes taken while the release of %s stood of roles another path took over: %w", issue, err)
	}
	return nil
}

// pairUnrecordedWakesWithNoRelease makes each resume of one of paired above wake, the mark's
// wake_row, that no pause holds pair with release 0. paired are the roles of this release's stops
// that its mark does not record (unrecordedStops) and whose claim is still suspended as
// `released` (CancelIssueRelease). A wake of such a role was taken while the release stood, so
// its resume names the newest release recorded before the wake (LatestPodRelease). This release
// took no pod, so the wake is no release's return. Left as it is, the records would count a pod
// return for a release that did not take that pod. Unlike dropTakenOverWakes, this keeps the
// resume. The cancel queues no resume of such a claim while the wake's start is queued
// (CancelIssueRelease), so that start is the role's one start, and it resumes the claim in the pod
// that stayed. The stuck warning and the role's first turn read the resume. Its kind, its times
// and its seen release stay. A resume of release 0 that no pause holds is the resume of no
// release, as the first wake of an issue that no release took is, and the stranded report leaves
// it out (UnfinishedPodResumesTx). A held wake already pairs with release 0, and this leaves it as
// it is. A cancel calls this whether the project runs or is paused (CancelIssueRelease).
//
// A role whose stop the mark does not record and whose claim a close, a linger or an operator
// took over is not one of paired. Its wake's start finishes without acting, or waits for an
// operator's resume, so the role takes no first turn from it. A kept resume would then be warned
// and recorded stuck, and it would refuse the role's next wake. The cancel deletes that resume
// instead (dropTakenOverWakes).
func pairUnrecordedWakesWithNoRelease(ctx context.Context, tx pgx.Tx, project, issue string, paired []string, wake int64) error {
	if len(paired) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `update pod_resumes set release_id = 0
		where project = $1 and issue = $2 and id > $3 and role = any($4) and pause_id is null`,
		project, issue, wake, paired); err != nil {
		return fmt.Errorf("pair the wakes taken while the release of %s stood of roles its mark does not record with no release: %w", issue, err)
	}
	return nil
}

// dropWakesUnderRelease deletes what a wake of each of roles left while the release stood: the
// role's resumes above wake, the mark's wake_row, and the starts of that role above last, the
// release's last row, that have a wake's own shape (wakeStartRow). A root architect's entry start
// and a production check's look are no wake's start: they stay, and their tasks are not the
// wake's. A role with no such resume keeps every start it has, since no wake of it was taken
// under the mark. A cancel calls it before it queues its own resumes (CancelIssueRelease). roles
// are the names of the claims of the mark's set that are still suspended as `released`, as for
// holdWakesUnderPause.
//
// It answers, for each role whose wake it dropped, the wake kind, the detail and the task of the
// start it deleted, the newest of each, which the cancel hands its builder (DroppedWake). The kind
// is the start's own (record.SuperviseRequest.WakeKind), the kind its task was rendered with: the
// builder renders that task again (workflow.CancelledWakeTask), and the deleted resume can be
// another wake's, of another kind. A role it dropped nothing of is not in the answer, and reads as
// the zero DroppedWake. So does a role whose resume it deleted with no start, and a role whose
// deleted start an earlier daemon queued, which carries a task and no detail (retellStarts says
// why): its resume then carries no task of the wake. The transaction logs why once it commits, and
// the line carries the deleted task, since the row is gone (logStartWithoutDetail).
func dropWakesUnderRelease(ctx context.Context, tx pgx.Tx, project, issue string, roles []string, wake, last int64) (map[claim.Role]DroppedWake, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `with woken as (
			delete from pod_resumes where project = $1 and issue = $2 and id > $3 and role = any($4) returning id, role),
		started as (
			delete from outbox where issue = $2 and id > $5 and `+wakeStartRow+`
				and outbox.payload->>'role' in (select role from woken)
			returning outbox.id, outbox.payload->>'role' as role, outbox.payload->>'wakeKind' as kind,
				coalesce(outbox.payload->>'task', '') as task, outbox.payload->>'wakeDetail' as detail)
		select w.role, coalesce(s.kind, ''), coalesce(s.id, 0), coalesce(s.task, ''), s.detail from woken w left join started s on s.role = w.role
		order by w.id desc, s.id desc`,
		project, issue, wake, roles, last)
	if err != nil {
		return nil, fmt.Errorf("delete the wakes taken while the release of %s stood: %w", issue, err)
	}
	defer rows.Close()
	dropped := map[claim.Role]DroppedWake{}
	for rows.Next() {
		var role string
		var start int64
		var detail *string
		var got DroppedWake
		if err := rows.Scan(&role, &got.Kind, &start, &got.Task, &detail); err != nil {
			return nil, fmt.Errorf("delete the wakes taken while the release of %s stood: %w", issue, err)
		}
		if _, seen := dropped[claim.Role(role)]; seen {
			continue
		}
		switch {
		case got.Task != "" && detail == nil:
			logStartWithoutDetail(ctx, issue, start, role, got.Kind, got.Task, "the cancel's resume carries no task of that wake")
			got = DroppedWake{}
		case detail != nil:
			got.Detail = *detail
		}
		dropped[claim.Role(role)] = got
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delete the wakes taken while the release of %s stood: %w", issue, err)
	}
	return dropped, nil
}
