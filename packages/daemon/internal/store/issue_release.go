package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// one transaction, so the pod is left as a wake would have left it. The mark stands until
	// that call, so a cancel that does not finish is read again by the row's retry rather than
	// lost.
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
	// ReleaseCausePaused: the project is paused at the release's workspace check. The pause deletes
	// no pod, so the release is cancelled before any role stops (IssueReleaseCheck).
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
// and Suspended is the set a cancel resumes and a finished release records.
//
// SuspendsNoRole is the mark's kind read back (issue_releases.kind, IdleReleaseMark): the
// release of a pod no role can use suspends none, and its check, where it has one, is read by the
// pod's launcher, so its rows must be able to tell what release they belong to. A decision that
// holds no mark — a row of a release that is over — carries the zero of it, as it carries the zero
// of every other field.
//
// Teardown is what the mark notes of the release's teardown (issue_releases.teardown,
// ReleaseTeardown). Every decision that holds a mark carries all of the mark's half of it
// (releaseMark.decision), so no reader gets the zero note, which is a real state, by accident.
type ReleaseDecision struct {
	Act            ReleaseAct
	Cause          ReleaseCause
	Release        int64
	Suspended      []claim.Role
	SuspendsNoRole bool
	Teardown       ReleaseTeardown
}

// ReleaseTeardown is whether the teardown of the release of a pod no role can use may have taken
// the pod, as its mark notes it (issue_releases.teardown, NoteReleaseTeardown).
//
// The rule: that release records a pod release when the runtime took a Sandbox (its own answer,
// runtime.IssueReleaser's true) or the mark says TeardownReached, and otherwise records none — the
// Sandbox was deleted by another hand, or by an earlier teardown of this daemon's, and nothing of
// this release took it. The idle release reads no note: it records its release whatever the
// runtime answered, since its stops suspended the roles a wake pairs with its record.
//
// The protocol: this release's mark starts at TeardownNotReached (ReleaseIdleIssue writes it), and
// the teardown writes TeardownReached from the runtime's callback, under the pod lock, just before
// the runtime's first write to the issue's Sandbox, so an attempt that took the pod and then
// failed, or whose daemon died, before its record leaves its retry the note, and the
// retry, which finds no Sandbox, still records the release. An attempt that fails before that
// first write, or finds no Sandbox at all, leaves the note unset, and its retry decides afresh. A
// teardown that records its release whatever the runtime answers, the idle release's or one whose
// mark already says TeardownReached, gives the runtime no callback, since it has nothing to note.
// The outbox's count of a row's attempts cannot stand in for the note: a run the daemon died in is
// counted as no attempt (record.QueuedSupervise.Attempts), and a retry of a release that took
// nothing and then failed is counted as one. A mark this release did not write — one standing when
// the column was added, or one an earlier release writes, which names no teardown — reads the
// column's default, TeardownReached: the daemon that wrote it records a release whatever its
// teardown found, and so does this release's teardown of it.
//
// The window it leaves: a note written just before a first write that then failed or never ran,
// its daemon dead or its write refused, over a Sandbox another hand deleted before the retry —
// that retry finds no Sandbox, reads the note and records a release that no attempt made.
type ReleaseTeardown string

const (
	// TeardownNotReached: no attempt of the teardown has come to the runtime's first write.
	TeardownNotReached ReleaseTeardown = ""
	// TeardownReached: an attempt came to the runtime's first write; the pod may have been taken.
	TeardownReached ReleaseTeardown = "reached"
)

// releaseMark is the mark one release in flight left, as every row of that release reads it.
type releaseMark struct {
	id                             int64
	first, boundary, last, started int64
	// wake is the newest wake this pod had when the mark was written (max pod_resumes.id), so a
	// resume row above it is a wake taken since (releaseOvertaken).
	wake int64
	// turn is the newest turn of the pod's claims when the mark was written
	// (issue_releases.turn_start, the greatest claims.last_turn_start), nil where none had one,
	// so a claim whose last turn started after it ran a turn since (releaseOvertaken).
	turn *time.Time
	// suspended are the roles whose claims this release's own role stops have suspended as
	// `released`, each appended once its stop has suspended it (ReleaseSuspendedClaim).
	suspended []string
	// kind is the release rule that wrote this mark, releaseKindIdle or releaseKindUnowned: the
	// release of a quiet pod, or the release of a pod no role can use, which suspends no role.
	kind string
	// teardown is the mark's note of whether an attempt of the release's teardown came to the
	// runtime's first write (ReleaseTeardown).
	teardown ReleaseTeardown
}

// holds is whether row is one of the rows this mark's release queued. Every row of a release was
// written in the mark's own transaction, so its id lies between the release's first row and its
// last; a row outside that range belongs to a release this mark is not.
func (m releaseMark) holds(row int64) bool { return row >= m.first && row <= m.last }

// decision is the mark's half of a ReleaseDecision — the release's id, the roles its stops have
// suspended, its rule and its teardown's note — which every row of the release reads alike.
func (m releaseMark) decision() ReleaseDecision {
	return ReleaseDecision{Release: m.id, Suspended: m.roles(), SuspendsNoRole: m.suspendsNoRole(), Teardown: m.teardown}
}

// readReleaseMark reads the mark of issue's release and places row in it: held is false for a
// row no mark holds, which is a row of a release that is over, and otherwise the mark is the one
// the row belongs to.
func readReleaseMark(ctx context.Context, tx pgx.Tx, project, issue string, row int64) (mark releaseMark, held bool, err error) {
	err = tx.QueryRow(ctx, `select id, first_row, boundary_row, last_row, started_row, wake_row, suspended, kind, turn_start, teardown
		from issue_releases where project = $1 and issue = $2`, project, issue).Scan(&mark.id, &mark.first, &mark.boundary,
		&mark.last, &mark.started, &mark.wake, &mark.suspended, &mark.kind, &mark.turn, &mark.teardown)
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
// of a pod no role can use, which queues the teardown behind its launcher's read, or alone where
// it deletes no disk (IdleReleaseMark.SuspendsNoRole). Its rows read the kind rather than the empty role, because
// the kind is what the rule wrote and the empty role is only how the table spells it.
func (m releaseMark) suspendsNoRole() bool { return m.kind == releaseKindUnowned }

// releaseOvertaken is whether the pod was woken or went on working after its release was marked,
// which is what cancels the release. Four things say it was:
//
//   - a wake of the pod taken since the mark: one pod_resumes row above the mark's own
//     (TakePodWake writes one in the wake's own transaction). The wake is read rather than what
//     it did to a claim, because what it did to a claim says too little. The outbox's wake of a
//     delivery it holds resumes its role by rule (RequestRuleResume), which queues no start row
//     and moves no last_start_row; and a wake for a claim an earlier release left `released` —
//     a notice for an architect this release never stopped — touches no claim of the mark's own
//     set. Either way the pod is coming back, so the release goes rather than the pod, whichever
//     claim the wake was for and whatever that claim looks like by the time this reads it;
//   - a claim of the issue started since the newest start the mark was taken at;
//   - a claim of the issue went on working since the newest turn the mark was taken at
//     (releaseMark.turn, claimWorkedSince): it started a turn since, a whole turn that began after
//     the workspace check's read and ended before that role's stop being a turn the next state
//     check would not see, which can have left work the read did not find; or it is live in a
//     turn, starting, or of a launch not yet known — the states the idle rule refuses to release a
//     pod in;
//   - a claim this release suspended that is no longer suspended as `released`: something
//     resumed it or took its suspension over — an operator's resume, a close, a linger — which
//     leaves no wake of its own to read.
//
// A claim the release itself suspended and still holds is none of those, so the release's own
// stops never read as work.
func releaseOvertaken(ctx context.Context, tx pgx.Tx, project, issue string, mark releaseMark) (bool, error) {
	var overtaken bool
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from pod_resumes where project = $1 and issue = $2 and id > $3)`,
		project, issue, mark.wake).Scan(&overtaken); err != nil || overtaken {
		return overtaken, err
	}
	err := tx.QueryRow(ctx, overtakenClaimsQuery, project, issue, mark.started, mark.suspended, mark.turn).Scan(&overtaken)
	return overtaken, err
}

// overtakenClaimsQuery is releaseOvertaken's read of the issue's claims, built once: $3 is the
// mark's newest start row, $4 the roles it suspended, and $5 its newest turn start, null where no
// claim had had a turn.
var overtakenClaimsQuery = `select exists (
	select 1 from claims where project = $1 and issue = $2
	and (last_start_row > $3 or ` + claimWorkedSince(`coalesce($5::timestamptz, '-infinity')`) + `
		or (role = any($4) and not (state = 'suspended' and suspended_by = 'released'))))`

// claimWorkedSince is the one SQL condition, over a claims row, that its claim went on working
// after the moment the SQL expression since names: it started a turn after that moment
// (claims.last_turn_start), or it is live in a turn now, starting, or of a launch not yet known —
// a state the idle rule releases no pod in (workflow's idleStateReleasable). A release's mark is
// overtaken on it (releaseOvertaken), and a left pod's close records no decline from a read a claim
// went on working beside (Store.LeftPodTurned).
func claimWorkedSince(since string) string {
	return `(last_turn_start > ` + since + ` or state not in ('ready', 'idle', 'suspended', 'failed', 'retired'))`
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
//     goes rather than the pod. A start queued for news alone, and a worker start that waits for
//     an admission slot its issue does not hold, are not such rows (idleWaitingRow), as they hold
//     no pod: each wakes the claim the release suspends when it runs, and the role comes back
//     with its news or its task.
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
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		decision = ReleaseDecision{}
		mark, held, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || !held {
			return err
		}
		decision = mark.decision()
		overtaken, err := releaseOvertaken(ctx, tx, project, issue, mark)
		if err != nil {
			return err
		}
		if !overtaken && row == mark.boundary {
			// A start that waits because the pod lacks the slot of its claim or the role of its
			// step is no work the pod was given (startWaitsForPodRow): the idle rule's Waiting
			// leaves it out too.
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and `+idleWaitingRow+` and (id < $3 or id > $4)
					and not `+fmt.Sprintf(startWaitsForPodRow, "$5")+`)`,
				issue, idleWaitingKinds, mark.first, mark.last, pods.StartWaitsForPodPrefix).Scan(&overtaken); err != nil {
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
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
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

// IssueReleaseCheck decides what the release's check of the workspace may do: it acts for a row
// the release's mark holds, and is ReleaseOver for every other, which is a check of a release that
// is over. The check reads no boundary, since it stops nothing, but a check of a finished release
// must not decline, record a decline or tell a role anything about a release that is not the live
// one. It cancels the release for two causes only: the project's pause (ReleaseCausePaused) and
// the pod given work since the mark (ReleaseCauseOvertaken), each named below.
//
// A check that finds the project paused cancels the release (ReleaseCausePaused): the pause
// deletes no pod, so a release whose check had not run when the pause began is cancelled and keeps
// its pod, whatever reached its roles first.
// The pause is project's, read by its token (readOpenPauseByToken, which
// says why). No production row reaches a check for another project's issue: the check runs from
// the release's row, which is keyed by its issue, and the daemon's outbox runs only the rows of its
// own project's keys (record's DueRows). A release whose check ran before the pause finishes as it
// does while the project runs: its role stops and its teardown read no pause. The open pause is the
// one fact this reads of it, in this transaction. Where the project runs, a check that finds the pod
// given work since the mark cancels the release (ReleaseCauseOvertaken, releaseOvertaken).
func (s *Store) IssueReleaseCheck(ctx context.Context, project, issue string, row int64) (ReleaseDecision, error) {
	if project == "" || issue == "" || row <= 0 {
		return ReleaseDecision{}, errors.New("a release's workspace check requires its project, its issue and its outbox row")
	}
	var decision ReleaseDecision
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		decision = ReleaseDecision{}
		mark, held, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || !held {
			return err
		}
		decision = mark.decision()
		decision.Act = ReleaseActs
		_, paused, err := readOpenPauseByToken(ctx, tx, project, "")
		if err != nil {
			return err
		}
		if paused {
			decision.Act, decision.Cause = ReleaseCancel, ReleaseCausePaused
			return nil
		}
		// The pod given work since the mark — a wake, a start, a claim in a turn or a turn that
		// started after the mark's newest — cancels the release here, before any read, as it does
		// at a role stop and at the teardown: a read made beside a turn could miss what that turn
		// makes, and a decline taken then would hold the pod for work the turn may have pushed.
		overtaken, err := releaseOvertaken(ctx, tx, project, issue, mark)
		if err != nil {
			return err
		}
		if overtaken {
			decision.Act, decision.Cause = ReleaseCancel, ReleaseCauseOvertaken
		}
		return nil
	})
	if err != nil {
		return ReleaseDecision{}, fmt.Errorf("authorize the workspace check of %s's release: %w", issue, err)
	}
	return decision, nil
}

// IssueRelease decides whether an idle release may now stop its issue's Sandbox, in one short
// transaction under the shared serializer. It is ReleaseOver for a row the release's mark does
// not hold, which is a teardown of a release that is over: one a role stop cancelled, having
// already resumed what it suspended, and one that finished, whose pod is gone.
//
// It cancels the release — the pod stays and the claims it suspended are resumed — when the run
// it released has moved on, the issue has left the workflow or its
// tree lingers (each of which is a close's business, not a release's), when the tree's cleanup is
// reserved, which deletes the Sandbox this release would, or when the pod went on working after
// the mark: a claim started, resumed or holding a process now. It waits while the release's own
// workspace check or one of its role stops is unfinished, or a stored claim of the issue may still
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
// This is IssueSuspension's shape with its authorization inverted: a close acts only once its
// issue has left the workflow or its tree lingers, and a release acts only while the issue is
// still in the workflow and its tree is live. The two order against each other because each
// waits on the other's role stops: both read the same `suspend`, `issue_close` and `release` ops.
func (s *Store) IssueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool) (ReleaseDecision, error) {
	if release.Issue == "" || release.Tree == "" || release.IssueGeneration == 0 || release.TreeGeneration == 0 || release.Row <= 0 {
		return ReleaseDecision{}, errors.New("issue release requires issue, tree, both generations and its outbox row")
	}
	var decision ReleaseDecision
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		decision = ReleaseDecision{}
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		mark, held, err := readReleaseMark(ctx, tx, project, release.Issue, release.Row)
		if err != nil || !held {
			return err
		}
		decision = mark.decision()
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
		// A role started or resumed since the mark, or a start of a role of the pod queued
		// outside the release's own rows, is the pod being used again: the teardown would take a
		// pod with a live role in it, and the stop that would have caught it has already run. A
		// start above the rows was queued once the mark stood. One below them is a start that
		// has come due since an earlier read: for an idle release, a worker start that waited for
		// an admission slot its issue did not hold when the boundary read it, whose slot has come
		// since, or one a transaction in flight at the mark committed after that read. The
		// release of a pod no claim owns makes no boundary read (the daemon's releaseUnownedPods),
		// so for it this is the one read of the starts on both sides of its rows. The start rows
		// read here are those that wait for a role of the pod, the one rule the idle hold and the
		// boundary read too (idleWaitingRow), so neither a start queued for news alone nor a worker
		// start that still waits for an admission slot its issue does not hold cancels the
		// teardown: each wakes the claim this release suspended when it runs, as any start wakes
		// one, and the role comes back with its news or its task in the pod that replaces this
		// one. The release reads
		// every other queued start row where IssueSuspension reads only a production check's
		// look, and needs no other narrowing: the read above has already returned a cancel for
		// an issue that is not pods.InWorkflowNotLingering, so this predicate runs only for an
		// issue still in the workflow under a live tree, where the outbox's start fence declines
		// nothing and a queued start row really is a start. The cause is the one a role stop
		// reads for it.
		overtaken, err := releaseOvertaken(ctx, tx, project, release.Issue, mark)
		if err != nil {
			return err
		}
		if !overtaken {
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and `+idleWaitingRow+` and (id < $4 or id > $3)
					and kind = 'supervise' and payload->>'op' = 'start')`,
				release.Issue, idleWaitingKinds, mark.last, mark.first).Scan(&overtaken); err != nil {
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
		// The release's own check is its workspace-check row inside the mark's range, the range
		// holds reads; a check below it is an earlier release's and holds no pod of this one. The
		// row's kind keeps the name of the save it replaced (record.OutboxKindIssueBundle
		// says why). The teardown never runs ahead of the check: a pod whose workspace nobody read
		// could hold work that no branch on origin has, and the teardown deletes its disk.
		var checking bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and kind = 'issue_bundle' and id >= $2 and id <= $3)`,
			release.Issue, mark.first, mark.last).Scan(&checking); err != nil {
			return err
		}
		if checking {
			return wait.Errorf("issue %s release waits for its workspace check", release.Issue)
		}
		decision.Act, decision.Cause = ReleaseActs, ReleaseCauseNone
		return nil
	})
	if err != nil {
		return ReleaseDecision{}, fmt.Errorf("authorize issue release: %w", err)
	}
	return decision, nil
}
