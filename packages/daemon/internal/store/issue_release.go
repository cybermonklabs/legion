package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// IssueReleased is one idle release of an issue's pod as its issue_release outbox row names it:
// the run it released (the issue's generation and its tree root's) and the row, written after the
// release's per-role stops. It is the release's counterpart to IssueClose, and differs in what it
// authorizes: a close acts on an issue the workflow has taken out, a release on an issue that is
// still in it and comes back at the next wake for one of its roles.
type IssueReleased struct {
	Issue, Tree                     string
	IssueGeneration, TreeGeneration uint64
	Row                             int64
}

// ReleaseAct is what one row of an idle release may do, which the store decides and the row's
// executor carries out.
type ReleaseAct int

const (
	// ReleaseOver: the row's own release is over. Either no mark stands — it finished, or a row
	// of it cancelled it and undid what it had done — or the mark that stands names another
	// release's rows and not this one. Such a row does nothing and undoes nothing: the pod it
	// would act on is not the pod it was queued for.
	ReleaseOver ReleaseAct = iota
	// ReleaseCancel: the release is cancelled. The caller carries the cancel out
	// (CancelIssueRelease): the claims this release suspended are resumed and the mark goes, in
	// one transaction, and the bundle it stored is deleted, so the pod is left as a wake would
	// have left it. The mark stands until that call, so a cancel that does not finish is read
	// again by the row's retry rather than lost.
	ReleaseCancel
	// ReleaseActs: the row acts.
	ReleaseActs
)

// String is the act's name in a log line.
func (a ReleaseAct) String() string {
	switch a {
	case ReleaseOver:
		return "over"
	case ReleaseCancel:
		return "cancel"
	case ReleaseActs:
		return "acts"
	}
	return "unknown"
}

// ReleaseCause is why a release was cancelled, as the store read it. It is the store's answer
// and not the row's, because one cause reaches the daemon through two rows — a role stop and the
// teardown both read releaseOvertaken — and the records a deployment groups by reason
// (workflow.IdleHold) must name what happened rather than which row happened to read it.
type ReleaseCause int

const (
	// ReleaseCauseNone: the decision is not a cancel.
	ReleaseCauseNone ReleaseCause = iota
	// ReleaseCauseOvertaken: the pod was given work after its release was marked — a claim of it
	// started, resumed or holds a process, or, at the boundary, a task, notice, message or start
	// reached a role of the pod.
	ReleaseCauseOvertaken
	// ReleaseCauseUnauthorized: the run the release was taken against has moved on, the issue has
	// left the workflow, its tree lingers, or its cleanup is reserved. None of those is a
	// release's business, and each of them is a close's.
	ReleaseCauseUnauthorized
	// ReleaseCausePaused: the project is paused at the release's save. The pause saves no
	// workspace and deletes no pod, so the release is cancelled before it stores a bundle
	// (IssueReleaseSave, decision 12 and departure 4 of dispatch://INFRA-3759).
	ReleaseCausePaused
)

// String is the cause's name in a log line or a test's message. The hold the daemon records for
// a cause is the daemon's own mapping (releaseHold), not this name.
func (c ReleaseCause) String() string {
	switch c {
	case ReleaseCauseOvertaken:
		return "overtaken"
	case ReleaseCauseUnauthorized:
		return "unauthorized"
	case ReleaseCausePaused:
		return "paused"
	}
	return "none"
}

// ReleaseDecision is what the store decided about one row of a release: the act the row may
// carry out, the cause of a cancel, the release's own id, the roles whose claims it has
// suspended so far and the rule that decided the release. Release is the id the mark carries,
// which the save puts on the bundle it stores, and Suspended is the set a cancel resumes and a
// finished release records.
//
// SuspendsNoRole is the mark's kind read back (issue_releases.kind, IdleReleaseMark): the
// release of a pod no role can use suspends none and saves no bundle, so its rows must be able
// to tell what release they belong to. A decision that holds no mark — a row of a release that
// is over — carries the zero of it, as it carries the zero of every other field.
//
// PodTaken is whether the release has taken its pod: its teardown recorded the release
// (issue_releases.released_id, RecordPodRelease), which it does once the runtime has answered the
// teardown without an error, and before the teardown's own later steps: the end of the pause's
// hold on the pod, the backstop and the clear of the mark. Only the teardown answers it
// (IssueRelease), always with ReleaseActs: the teardown that runs again then finishes the steps
// that are left and does not ask the runtime again. A step of the runtime that fails after the pod
// is gone leaves no record; the commit below is what keeps such a release from being cancelled.
//
// Committed is whether the release's teardown was authorized to take the pod and has not recorded
// it yet (issue_releases.committed, CommitIssueRelease): the runtime's own read answered
// ReleaseActs under the pod's launch turn, and the runtime may have taken the pod before a later
// step of it failed. Only the teardown answers it, always with ReleaseActs: the teardown that runs
// again asks the runtime again, whose steps are each done at most once, and then records the
// release.
type ReleaseDecision struct {
	Act            ReleaseAct
	Cause          ReleaseCause
	Release        int64
	Suspended      []claim.Role
	SuspendsNoRole bool
	PodTaken       bool
	Committed      bool
}

// releaseMark is the mark one release in flight left, as every row of that release reads it.
type releaseMark struct {
	id                             int64
	first, boundary, last, started int64
	// wake is the newest wake this pod had when the mark was written (max pod_resumes.id), so a
	// resume row above it is a wake taken since. Such a wake cancels the release unless its role
	// is in suspended: a wake takes only a released claim, so a wake of such a role came after
	// this release recorded it, and the release runs to its end (releaseOvertaken).
	wake int64
	// suspended are the roles whose claims this release's own role stops have suspended as
	// `released`, each appended once its stop has suspended it (ReleaseSuspendedClaim).
	suspended []string
	// kind is the release rule that wrote this mark, releaseKindIdle or releaseKindUnowned: the
	// release of a quiet pod, or the release of a pod no role can use, which suspends no role.
	kind string
	// released is the pod_releases row the teardown recorded for this release once the runtime
	// took the pod (RecordPodRelease), 0 while the pod stands.
	released int64
	// committed is whether the runtime's own read authorized the teardown to take the pod
	// (CommitIssueRelease).
	committed bool
}

// holds is whether row is one of the rows this mark's release queued. Every row of a release was
// written in the mark's own transaction, so its id lies between the release's first row and its
// last; a row outside that range belongs to a release this mark is not.
func (m releaseMark) holds(row int64) bool { return row >= m.first && row <= m.last }

// readReleaseMark reads the mark of issue's release and places row in it: held is false for a
// row no mark holds, which is a row of a release that is over, and otherwise the mark is the one
// the row belongs to.
func readReleaseMark(ctx context.Context, tx pgx.Tx, project, issue string, row int64) (mark releaseMark, held bool, err error) {
	err = tx.QueryRow(ctx, `select id, first_row, boundary_row, last_row, started_row, wake_row, suspended, kind, released_id, committed
		from issue_releases where project = $1 and issue = $2`, project, issue).Scan(&mark.id, &mark.first, &mark.boundary,
		&mark.last, &mark.started, &mark.wake, &mark.suspended, &mark.kind, &mark.released, &mark.committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return releaseMark{}, false, nil
	}
	if err != nil {
		return releaseMark{}, false, err
	}
	if !mark.holds(row) {
		return releaseMark{}, false, nil
	}
	return mark, true, nil
}

// roles is the mark's suspended set as the roles it names, in the order the stops recorded them.
func (m releaseMark) roles() []claim.Role {
	if len(m.suspended) == 0 {
		return nil
	}
	roles := make([]claim.Role, 0, len(m.suspended))
	for _, role := range m.suspended {
		roles = append(roles, claim.Role(role))
	}
	return roles
}

// suspendsNoRole is whether this mark's release is the one that suspends no role: the release
// of a pod no role can use, which saves no bundle and queues the teardown alone
// (IdleReleaseMark.SuspendsNoRole). Its rows read the kind rather than the empty role, because
// the kind is what the rule wrote and the empty role is only how the table spells it.
func (m releaseMark) suspendsNoRole() bool { return m.kind == releaseKindUnowned }

// releaseOvertaken is whether the pod was woken or went on working after its release was marked,
// which is what cancels the release. Four things say it was:
//
//   - a wake of the pod taken since the mark for a role outside the mark's suspended set: a
//     pod_resumes row above the mark's own (TakePodWake writes one in the wake's own transaction)
//     whose role this release has not recorded as `released`. The wake is read rather than what
//     it did to a claim, because what it did to a claim says too little. The outbox's wake of a
//     delivery it holds resumes its role by rule (RequestRuleResume), which queues no start row
//     and moves no last_start_row; and a wake for a claim an earlier release left `released` —
//     a role message to an architect this release never stopped — touches no claim of the mark's
//     own set. Either way that role comes back in the pod that still stands, so the release goes
//     rather than the pod, whatever that claim looks like by the time this reads it;
//   - a claim of the issue started since the newest start the mark was taken at;
//   - a live claim in a turn, starting, or whose launch is not yet known — the states the idle
//     rule refuses to release a pod in (workflow's idleStateReleasable);
//   - a claim this release suspended that is no longer suspended as `released`: something
//     resumed it or took its suspension over — an operator's resume, a close, a linger — which
//     leaves no wake of its own to read.
//
// A wake of a role in the mark's suspended set is none of those. This release's record of a
// claim as `released` (ReleaseSuspendedClaim) is the release's point of no return for that role:
// a wake takes only a released claim, so such a wake came after the record. The release then
// runs to its end. The wake's `start` waits for the teardown (record's outboxInOrder) and starts
// the role once, in the pod that comes back, and the release's record points the wake's resume
// at this release (RecordPodRelease). The outbox's delivery wake takes no wake at all while the
// mark stands, of a role in the set or of any other (TakeDeliveryWake). The three claims clauses
// still read every role of the set, because a claim that started, holds a process or is no
// longer suspended as `released` is a process that really runs — after an operator's resume,
// for example — and the pod must not go from under it.
//
// A claim the release itself suspended and still holds is none of those, so the release's own
// stops never read as work.
func releaseOvertaken(ctx context.Context, tx pgx.Tx, project, issue string, mark releaseMark) (bool, error) {
	var overtaken bool
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from pod_resumes where project = $1 and issue = $2 and id > $3
		and not (role = any(coalesce($4::text[], '{}'))))`,
		project, issue, mark.wake, mark.suspended).Scan(&overtaken); err != nil || overtaken {
		return overtaken, err
	}
	err := tx.QueryRow(ctx, `select exists (
		select 1 from claims where project = $1 and issue = $2
		and (last_start_row > $3 or state not in ('ready', 'idle', 'suspended', 'failed', 'retired')
			or (role = any($4) and not (state = 'suspended' and suspended_by = 'released'))))`,
		project, issue, mark.started, mark.suspended).Scan(&overtaken)
	return overtaken, err
}

// IssueReleaseStop decides what one role stop of a release may do, and is the one place the
// release's boundary is read. The boundary is the row that stops the first role process of the
// pod — the release's first `release` supervise row, recorded as issue_releases.boundary_row
// when the release was marked.
//
// Two things cancel the release here, and both leave the pod exactly as it stands with its roles
// going on in it:
//
//   - the pod went on working after the mark (releaseOvertaken), read at every role stop of the
//     release and not at the boundary alone. The release's rows run on the issue's lifecycle
//     lane, which runs them by next_at and then by id, so a start of one role can run ahead of
//     another role's stop; a stop that suspended its claim after such a start would strand that
//     role's task and the teardown would take its pod;
//
//   - at the boundary, where no role process of the pod has been stopped yet, a task, notice,
//     message or start of the issue addressed to a role of the pod that the release did not
//     queue itself. One below the release's own rows was queued by a transaction already in
//     flight when the mark was written; one above them was queued once the mark stood, while
//     every role still ran. Either way the pod is the one that must answer it, so the release
//     goes rather than the pod.
//
// The mark stands through a cancel decided here: the caller carries the cancel out whole
// (CancelIssueRelease), and a cancel that did not finish — a resume refused, the daemon stopped
// mid-row — is read again by the row's retry, where a mark deleted ahead of the resumes would
// have left the roles this release suspended `released` in a pod that still stands.
//
// A row no mark holds — a release already cancelled or finished, or a row of an earlier release
// still on the lane while a newer one runs — is ReleaseOver: it does nothing and cancels
// nothing, so a stale row can neither suspend a claim of the live release nor undo it.
func (s *Store) IssueReleaseStop(ctx context.Context, project, issue string, row int64) (ReleaseDecision, error) {
	if project == "" || issue == "" || row <= 0 {
		return ReleaseDecision{}, errors.New("a release's role stop requires its project, its issue and its outbox row")
	}
	var decision ReleaseDecision
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		decision = ReleaseDecision{}
		mark, held, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || !held {
			return err
		}
		decision.Release, decision.Suspended, decision.SuspendsNoRole = mark.id, mark.roles(), mark.suspendsNoRole()
		overtaken, err := releaseOvertaken(ctx, tx, project, issue, mark)
		if err != nil {
			return err
		}
		if !overtaken && row == mark.boundary {
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and kind = any($2) and (id < $3 or id > $4))`,
				issue, idleWaitingKinds, mark.first, mark.last).Scan(&overtaken); err != nil {
				return err
			}
		}
		if overtaken {
			decision.Act, decision.Cause = ReleaseCancel, ReleaseCauseOvertaken
			return nil
		}
		decision.Act = ReleaseActs
		return nil
	})
	if err != nil {
		return ReleaseDecision{}, fmt.Errorf("authorize the role stop of %s's release: %w", issue, err)
	}
	return decision, nil
}

// ReleaseSuspendedClaim records on the mark that one role stop of the release has suspended its
// claim as `released`. The stop calls it after the suspension is made, so the set the mark
// carries is what the release has actually done and not what it set out to do: a stop that could
// not suspend its claim records nothing, and its row's retry suspends and records together.
//
// A row no mark holds records nothing, as it acts on nothing. A role already in the set is left
// as it is, so a row that ran twice counts once.
func (s *Store) ReleaseSuspendedClaim(ctx context.Context, project, issue string, role claim.Role, row int64) error {
	if project == "" || issue == "" || role == "" || row <= 0 {
		return errors.New("recording a claim a release suspended requires its project, its issue, the role and its outbox row")
	}
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		_, held, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || !held {
			return err
		}
		_, err = tx.Exec(ctx, `update issue_releases set suspended = suspended || $3::text
			where project = $1 and issue = $2 and not ($3 = any(suspended))`, project, issue, string(role))
		return err
	})
	if err != nil {
		return fmt.Errorf("record the %s claim %s's release suspended: %w", role, issue, err)
	}
	return nil
}

// IssueReleaseSave decides what the release's save of the workspace may do: it acts for a row the
// release's mark holds, and is ReleaseOver for every other, which is a save of a release that is
// over. The save reads no boundary — it stops nothing, so there is nothing yet to cancel — but it
// does write the issue's one stored bundle, and a save of a finished release must not replace the
// bundle the live one stored.
//
// A save that finds the project paused cancels the release (ReleaseCausePaused): the pause saves
// no workspace and deletes no pod, so a release whose save had not run when the pause began is
// cancelled and keeps its pod, whatever reached its roles first (decision 12 and departure 4 of
// dispatch://INFRA-3759). The pause is project's, read by its token (readOpenPauseByToken,
// which says why). No production row reaches a save for another project's issue: the save runs
// from the release's row, which is keyed by its issue, and the daemon's outbox runs only the rows
// of its own project's keys (record's DueRows). A release that saved before the pause finishes
// as it does while the project runs: its role stops and its teardown read no pause. The open
// pause is the one fact this reads of it, in this transaction.
//
// The decision carries the release's id, which the save puts on the bundle it stores
// (PutWorkspaceBundle): the restore of the pod that replaces the released one names the release
// whose work it put back, so a late report deletes no newer release's bundle.
func (s *Store) IssueReleaseSave(ctx context.Context, project, issue string, row int64) (ReleaseDecision, error) {
	if project == "" || issue == "" || row <= 0 {
		return ReleaseDecision{}, errors.New("a release's save requires its project, its issue and its outbox row")
	}
	var decision ReleaseDecision
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		decision = ReleaseDecision{}
		mark, held, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || !held {
			return err
		}
		decision.Act, decision.Release = ReleaseActs, mark.id
		decision.Suspended, decision.SuspendsNoRole = mark.roles(), mark.suspendsNoRole()
		_, paused, err := readOpenPauseByToken(ctx, tx, project, "")
		if err != nil {
			return err
		}
		if paused {
			decision.Act, decision.Cause = ReleaseCancel, ReleaseCausePaused
		}
		return nil
	})
	if err != nil {
		return ReleaseDecision{}, fmt.Errorf("authorize the save of %s's release: %w", issue, err)
	}
	return decision, nil
}

// IssueRelease decides whether an idle release may now stop its issue's Sandbox, in one short
// transaction under the shared serializer. It is ReleaseOver for a row the release's mark does
// not hold, which is a teardown of a release that is over: one a role stop cancelled, having
// already resumed what it suspended, and one that finished, whose pod is gone and whose saved
// bundle the issue's next pod is owed.
//
// It cancels the release — the pod stays, the claims it suspended are resumed and the bundle it
// stored goes — when the run it released has moved on, the issue has left the workflow or its
// tree lingers (each of which is a close's business, not a release's), when the tree's cleanup is
// reserved, which deletes the Sandbox this release would, or when the pod went on working after
// the mark: a claim started, resumed or holding a process now. A wake of a role this release has
// recorded as `released`, and the start that wake queued, are not such work: the teardown runs to
// its end, and the start waits for it (releaseOvertaken). It waits while the release's own
// workspace save or one of its role stops is unfinished, or a stored claim of the issue may still
// hold a process. outOfWorkflow is the workflow's rule for a status that takes an issue out of it
// (record.OutOfWorkflow), which the store does not restate; the gate itself is the one every
// other path that acts on a released pod reads (pods.InWorkflowNotLingering), down to the
// issue's own linger, which this read missed.
//
// The decision carries the cause of a cancel, because a pod taken up again reads the same here
// as it does at a role stop and a deployment's records must say so once, and the roles whose
// claims the release suspended, which are the roles a finished release records and the claims a
// cancel resumes.
//
// A release whose teardown recorded it has taken its pod (ReleaseDecision.PodTaken), and the
// teardown's later steps failed: this row is that teardown run again. Nothing undoes a pod that is
// gone, so the answer is ReleaseActs whatever the claims and the queue say now, and the teardown
// finishes the steps that are left. A cancel then would resume the roles in a pod that is gone,
// delete the bundle of unpushed work that the next pod restores, and drop the wakes the record
// already counted. A release whose teardown the runtime was authorized to carry out
// (ReleaseDecision.Committed, CommitIssueRelease) is answered ReleaseActs for the same reason: the
// runtime may have taken the pod before a later step of it failed.
//
// This is IssueSuspension's shape with its authorization inverted: a close acts only once its
// issue has left the workflow or its tree lingers, and a release acts only while the issue is
// still in the workflow and its tree is live. The two order against each other because each
// waits on the other's role stops: both read the same `suspend`, `issue_close` and `release` ops.
func (s *Store) IssueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool) (ReleaseDecision, error) {
	return s.issueRelease(ctx, project, release, outOfWorkflow, false)
}

// CommitIssueRelease is IssueRelease as the runtime reads it under the pod's launch turn, just
// before it takes the pod (the teardown's authorize, runtime.IssueReleaser). The runtime reads the
// issue's Sandbox under that turn first, and it reads this only for a Sandbox of the issue and
// tree, or for one already gone: it refuses a Sandbox labelled for another project, issue or tree
// before this read, so that refusal commits nothing. Where it answers ReleaseActs, the same
// transaction marks the release committed (issue_releases.committed). That read is the
// teardown's point of no return: the runtime acts on it, and each of the runtime's steps after it
// can fail once the pod is already gone. So from that read on, the release is never cancelled:
// IssueRelease answers ReleaseActs (ReleaseDecision.Committed), and CancelIssueRelease refuses
// the mark. The teardown that runs again asks the runtime again, and then records the release
// (RecordPodRelease), after which the runtime is not asked again (ReleaseDecision.PodTaken).
func (s *Store) CommitIssueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool) (ReleaseDecision, error) {
	return s.issueRelease(ctx, project, release, outOfWorkflow, true)
}

// issueRelease is IssueRelease, and CommitIssueRelease where commit is true.
func (s *Store) issueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool, commit bool) (ReleaseDecision, error) {
	if release.Issue == "" || release.Tree == "" || release.IssueGeneration == 0 || release.TreeGeneration == 0 || release.Row <= 0 {
		return ReleaseDecision{}, errors.New("issue release requires issue, tree, both generations and its outbox row")
	}
	var decision ReleaseDecision
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		decision = ReleaseDecision{}
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		mark, held, err := readReleaseMark(ctx, tx, project, release.Issue, release.Row)
		if err != nil || !held {
			return err
		}
		decision.Release, decision.Suspended, decision.SuspendsNoRole = mark.id, mark.roles(), mark.suspendsNoRole()
		if mark.released != 0 {
			decision.Act, decision.PodTaken = ReleaseActs, true
			return nil
		}
		if mark.committed {
			decision.Act, decision.Committed = ReleaseActs, true
			return nil
		}
		decision.Act, decision.Cause = ReleaseCancel, ReleaseCauseUnauthorized
		var issueGeneration, rootGeneration int64
		var issuePhase, status, rootPhase, rootStatus string
		var lingering, rootLingering bool
		err = tx.QueryRow(ctx, `select i.generation, i.phase, i.status, i.linger_until is not null,
			root.generation, root.phase, root.status, root.linger_until is not null
			from issues i join issues root on root.key = i.tree where i.key = $1 and i.tree = $2`,
			release.Issue, release.Tree).Scan(&issueGeneration, &issuePhase, &status, &lingering,
			&rootGeneration, &rootPhase, &rootStatus, &rootLingering)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		live := pods.InWorkflowNotLingering(
			pods.Standing{Phase: phase.Phase(issuePhase), Status: status, Lingers: lingering},
			pods.Standing{Phase: phase.Phase(rootPhase), Status: rootStatus, Lingers: rootLingering},
			outOfWorkflow)
		if uint64(issueGeneration) != release.IssueGeneration || uint64(rootGeneration) != release.TreeGeneration || !live {
			return nil
		}
		// A role started or resumed since the mark, a wake of a role this release did not record,
		// or a start queued above the release's own rows, is the pod being used again: the
		// teardown would take a pod with a live role in it, and the stop that would have caught
		// it has already run. The release reads every queued start row where IssueSuspension
		// reads only a production check's look, and needs no narrowing of its own: the read
		// above has already returned a cancel for an issue that is not
		// pods.InWorkflowNotLingering, so this predicate runs only for an issue still in the
		// workflow under a live tree, where the outbox's start fence declines nothing and a
		// queued start row really is a start. The cause is the one a role stop reads for it.
		//
		// The one start passed over is a wake's own: a start that names no phase, waits for no
		// slot and carries the wake's kind (wakeStartRow), of a role in the mark's suspended set
		// that has a wake recorded since the mark (workflow's wakeRow). That wake came after this
		// release recorded the role (releaseOvertaken), and its start waits for this row (record's
		// outboxInOrder), so it starts the role in the pod that comes back. A phase's start of the
		// same role names its phase, and it still cancels the release. So do a root architect's
		// entry start and a production check's look of that role: they name no phase either, but
		// they are work the pod was given, not a wake's start, and they carry no wake's kind.
		overtaken, err := releaseOvertaken(ctx, tx, project, release.Issue, mark)
		if err != nil {
			return err
		}
		if !overtaken {
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and id > $2 and kind = 'supervise' and payload->>'op' = 'start'
				and not (`+wakeStartRow+` and payload->>'role' = any(coalesce($3::text[], '{}'))
					and exists (select 1 from pod_resumes woken where woken.project = $4 and woken.issue = $1
						and woken.id > $5 and woken.role = outbox.payload->>'role')))`,
				release.Issue, mark.last, mark.suspended, project, mark.wake).Scan(&overtaken); err != nil {
				return err
			}
		}
		if overtaken {
			decision.Cause = ReleaseCauseOvertaken
			return nil
		}
		may, err := issueStopFences(ctx, tx, project, release.Issue, release.Tree, "release", release.Row, release.IssueGeneration)
		if err != nil {
			return err
		}
		if !may {
			return nil
		}
		// The release's own save is its issue_bundle row inside the mark's range, the range holds
		// reads; a save below it is an earlier release's and holds no pod of this one.
		var saving bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and kind = 'issue_bundle' and id >= $2 and id <= $3)`,
			release.Issue, mark.first, mark.last).Scan(&saving); err != nil {
			return err
		}
		if saving {
			return wait.Errorf("issue %s release waits for its workspace save", release.Issue)
		}
		decision.Act, decision.Cause = ReleaseActs, ReleaseCauseNone
		if !commit {
			return nil
		}
		if _, err := tx.Exec(ctx, `update issue_releases set committed = true where project = $1 and issue = $2`,
			project, release.Issue); err != nil {
			return fmt.Errorf("mark the release of %s committed: %w", release.Issue, err)
		}
		decision.Committed = true
		return nil
	})
	if err != nil {
		return ReleaseDecision{}, fmt.Errorf("authorize issue release: %w", err)
	}
	return decision, nil
}
