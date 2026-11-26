package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// This file is the take of a wake under the issue's release mark: the take a workflow wake and a
// test make (Store.TakePodWake, PodRecords.TakePodWake), the outbox's delivery wake
// (Store.TakeDeliveryWake), the reads of a queued start that each makes (StartQueued,
// wakeStartQueued), the mark's lock that orders them against a release's record and cancel
// (lockReleaseMark), the rule of which pause holds a wake (PodRecords.PauseOfWake), and the close of
// a resume whose pod went away (PodRecords.ClosePodGoneResume). The rest of a wake's records — the
// released claims, the backstops, the resumes and the listener's report — are in wake.go.

// TakePodWake takes one wake in a transaction of its own: PodRecords.TakePodWake, for a caller
// outside any transaction, with the pause that holds the wake read by PodRecords.PauseOfWake.
// Like a workflow wake, it takes the wake while a release of the pod still stands. The outbox's
// delivery wake must take nothing then, so it takes its wake through TakeDeliveryWake. The daemon
// itself does not call this; tests use it to take a wake as a workflow wake leaves one, or as a
// delivery wake leaves one once no release mark stands.
//
// token is the claim the wake brings back, and the role it names is the role woken: a token that
// names no role is an error. mark is the pause that held the outbox row the wake is taken for
// (record.OutboxRow.HeldByPause), 0 for a row no pause held, as TakeDeliveryWake reads it.
//
// project is the project token the claim-keyed tables hold, as it is for every other
// project-keyed method of this file and of wake.go: the daemon derives it once at boot
// (claim.ProjectToken, internal/daemon/bootreads.go) and its outbox carries that, so nothing is
// derived again here.
func (s *Store) TakePodWake(ctx context.Context, project, issue string, token claim.Token, kind string, at time.Time, mark int64) (bool, error) {
	role, ok := token.Role()
	if !ok {
		return false, fmt.Errorf("take a wake of %s: claim token %q names no role", issue, token)
	}
	records := &PodRecords{store: s, project: project}
	var taken bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		pause, _, err := records.PauseOfWake(ctx, tx, issue, token, mark)
		if err != nil {
			return err
		}
		taken, err = records.TakePodWake(ctx, tx, issue, role, kind, at, pause)
		return err
	})
	return taken, err
}

// DeliveryWake is what one delivery wake did (Store.TakeDeliveryWake): whether it took the wake,
// and, where it took none, why the row waits.
type DeliveryWake struct {
	// Taken is whether this call took the wake.
	Taken bool
	// InRelease is a release mark of the issue that stands: the call takes nothing until the
	// release ends.
	InRelease bool
	// StartQueued is a start that brings the role back, queued in the outbox (StartQueued): the
	// call takes nothing, and that start resumes the role.
	StartQueued bool
	// NotReleased is a claim whose row the record does not hold as suspended by a release: the
	// call took nothing, since something already brought the role back or took the claim over.
	NotReleased bool
	// OpenResumePause and WakeStartQueued say, where the role's open resume refused the take, what
	// that resume is, as the take read it in its own transaction, after the refusal and under the
	// lock it holds on the claim's row (openPodResume). The caller logs the refusal from them and
	// reads nothing itself (internal/daemon's logRefusedByOpenResume).
	//
	// OpenResumePause is the project pause that held that resume (pod_resumes.pause_id): the
	// resume is then that pause's held wake (decision 15 of dispatch://INFRA-3759). It is 0 for a
	// resume that no pause held.
	//
	// WakeStartQueued is that a wake's own start of the role is queued (wakeStartRow): the start
	// of the wake that took that resume, which has not run yet. The open resume is then a wake on
	// its way, not a fault: the start of a workflow wake taken while a release stood runs only
	// once the release's teardown row leaves the outbox (record's outboxInOrder), and a pause's
	// held wake runs its start at the pause's resume. The caller logs such a refusal at debug.
	//
	// Both are zero only where the take was not refused by an open resume: it took the wake, or it
	// answered InRelease, StartQueued or NotReleased before it tried. A resume another wake of the
	// role took while this take waited for the claim's row is in them, since the read comes after
	// the refusal that resume made.
	OpenResumePause int64
	WakeStartQueued bool
}

// TakeDeliveryWake takes the wake of the outbox's delivery wake (internal/daemon's
// wakeForDelivery) in a transaction of its own: the wake for a notice, a hook prompt, a
// merge-queue removal or a merge forward's message that found the role it is for released. While
// a release mark of issue stands, it writes nothing and answers InRelease. While the role has no
// open resume and a start that brings it back at generation is queued, it writes nothing and
// answers StartQueued. Otherwise it takes the wake as PodRecords.TakePodWake does, held by the
// pause PodRecords.PauseOfWake reads, and answers whether this call took it.
//
// The delivery wake waits for any release of the issue that still stands, not only for one that
// recorded the role. Its resume runs in process (supervise.RequestRuleResume), so no `start` row
// waits behind the teardown row (record's outboxInOrder), as a workflow wake's `start` does. A
// resume while the release stands would launch the role in the pod the teardown is about to take.
// The role on the mark does not show that in time. A release's stop suspends the claim as
// `released` first, and records the role on the mark after it, in a transaction of its own
// (ReleaseSuspendedClaim). A wake in that gap reads the role as not recorded and resumes it. The
// record then names the role, and the release, which no wake of a recorded role cancels
// (releaseOvertaken), tears the pod down under the resumed role.
//
// The mark is read with the take's own lock (lockReleaseMark), in the take's own transaction, so
// nothing can come between the read and the take. A release's record or cancel that has not
// committed makes the read wait until it commits. Once the teardown clears the mark, the next call
// takes the wake against the newest release recorded (LatestPodRelease): the release the teardown
// recorded, or the release before it where the teardown gave up on its record (internal/daemon's
// recordGivenUp, SettleUnrecordedRelease).
//
// A cancel clears the mark too, and in the same transaction it queues a start for each claim it
// resumes (CancelIssueRelease). The claim reads `released` until that start runs, but its pod
// never went away. A wake taken then would record a resume, paired with the newest release
// recorded before it, for a pod that no release took, and the records would count that pod's
// return. So the call reads, after the mark, whether a start that brings the role back is queued,
// and takes nothing while one is. The cancel committed before this read began, so the read sees
// the cancel's start. The row waits, the start resumes the role in the pod that stayed, and the
// row's next attempt finds the claim live and hands its delivery over. The read is made only for a
// role with no open resume, which is the cancel's case: the cancel deleted the wakes taken under
// its mark. A role with an open resume refuses the take whatever is queued, so the take records
// nothing, and the caller resumes the role in process as it always did (internal/daemon's
// wakeForDelivery): the pause's held wake, whose start is queued for the pause's resume, and a
// workflow wake whose start has not run. Both are normal, and the call answers what that open
// resume is for them: the pause that held it (OpenResumePause), and whether a wake's own start of
// the role is queued (wakeStartQueued, WakeStartQueued). It reads both after the refusal, under
// the lock it holds on the claim's row, so a resume another wake of the role took while this take
// waited for that lock is what they name. The caller can then tell such a wake from a resume with
// no start behind it, with no read of its own.
//
// The caller read the claim as released from its machine, outside this transaction, and a start
// can run and finish between that read and the read of the queued starts: the cancel's start
// resumes the role and is deleted. So the call reads the claim's row too, after the starts, and
// takes nothing unless the record holds the claim suspended by `released`. A start that finished
// before the starts' read has written the claim's resume before it was deleted, so the two reads
// cannot both miss it. A claim the record holds no row of takes nothing either: every claim the
// caller's supervisor holds has a row. The supervisor writes a new claim's row before it builds
// the claim's machine (internal/daemon's supervisor.Create, AdmitClaim), and the boot builds each
// machine from a row (supervisor.restore). Every later write of the claim, the release's
// suspension of it as `released` among them, is an upsert of that row (upsertClaim), and no path
// deletes a claim's row. The row is read `for update`, so the take and a write of the claim that
// takes it over (an operator's suspension or resume, a close's or a linger's suspension, each one
// write of the claim's row) run one after the other: a write that commits first is what this read
// finds, and one that comes later waits until the take commits.
//
// The claim's lock comes after the mark's and the pause row's (PauseOfWake), the order TakePodWake
// gives for the mark, the pause's row and the writes after them. The stop transaction of a
// project's pause, which writes a claim's row, takes the mark and the pause's row first too
// (internal/daemon's projectPause.stopTx). A project's resume (Resume) takes the pause's row `for
// update` as its first statement and locks neither the mark nor a claim's row; a take that holds
// the pause's row makes it wait, and a take that waits for the pause's row holds only the mark,
// which the resume never asks for. No transaction that writes a claim's row locks the mark or the
// pause's row after it, so this lock adds no cycle.
//
// How long a delivery waits for a queued start is that start's own fate. The start runs on its
// lane like any row. Its failures, and those of an older row of the issue that it waits behind
// (record's outboxInOrder), are logged by the failing row itself: an error at each attempt, and a
// wait at WARN once its attempts pass the outbox's count (internal/daemon's waitWarnAttempts). A
// start that finishes without acting, such as one that serves an earlier generation, is deleted,
// and the delivery's next attempt takes the wake itself. So the delivery logs its own wait at
// debug and adds no warning of its own.
//
// token is the claim the wake brings back, and the role it names is the role woken: a token that
// names no role is an error. generation is the issue's generation, which a start that brings the
// role back names. mark is the pause that held the outbox row the wake is taken for
// (record.OutboxRow.HeldByPause, which the resume writes on every row its pause holds), 0 for a
// row no pause held. PodRecords.PauseOfWake reads which pause holds the wake from the token and
// the mark. A project's pause holds a row, whatever its due time, and the row runs, and wakes its
// role, only after the resume: its wake is a held wake of that pause, marked with it and paired
// with no release (PodRecords.TakePodWake, decision 15 of dispatch://INFRA-3759), as the wakes the
// workflow takes while the project is paused are, and so is the wake of a row written after the
// resume for a role that pause stopped. So the update round over the role's pod counts that
// pause's hold on to the role's let-out (PausePodHoldsSince), and the release records leave the
// wait out.
//
// project is the project token the claim-keyed tables hold, as it is for TakePodWake.
func (s *Store) TakeDeliveryWake(ctx context.Context, project, issue string, token claim.Token, generation uint64, at time.Time, mark int64) (DeliveryWake, error) {
	role, ok := token.Role()
	if !ok {
		return DeliveryWake{}, fmt.Errorf("take a delivery wake of %s: claim token %q names no role", issue, token)
	}
	records := &PodRecords{store: s, project: project}
	var wake DeliveryWake
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		wake = DeliveryWake{}
		var err error
		if wake.InRelease, err = lockReleaseMark(ctx, tx, project, issue); err != nil || wake.InRelease {
			return err
		}
		_, _, open, err := openPodResume(ctx, tx, project, issue, role)
		if err != nil {
			return err
		}
		if !open {
			if wake.StartQueued, err = startQueued(ctx, tx, issue, role, generation); err != nil || wake.StartQueued {
				return err
			}
		}
		held, _, err := records.PauseOfWake(ctx, tx, issue, token, mark)
		if err != nil {
			return err
		}
		var released bool
		err = tx.QueryRow(ctx, `select state = 'suspended' and suspended_by = 'released' from claims
			where project = $1 and token = $2 for update`, project, string(token)).Scan(&released)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read whether %s is still released: %w", token, err)
		}
		if wake.NotReleased = !released; wake.NotReleased {
			return nil
		}
		if wake.Taken, err = records.TakePodWake(ctx, tx, issue, role, pods.WakeDelivery, at, held); err != nil || wake.Taken {
			return err
		}
		// The take was refused, and the role's open resume is what refuses it. The read above was
		// made before this transaction locked the claim's row, so a resume another wake of the
		// role took while this one waited for that lock is not in it. Read it again here, under
		// the lock and after the refusal, so the caller names the wake that refused this one
		// rather than a resume with nothing behind it (logRefusedByOpenResume).
		_, pause, open, err := openPodResume(ctx, tx, project, issue, role)
		if err != nil || !open {
			return err
		}
		wake.OpenResumePause = pause
		wake.WakeStartQueued, err = wakeStartQueued(ctx, tx, issue, role, generation)
		return err
	})
	return wake, err
}

// resumeStartRow is the shape of an outbox `start` that names no phase and waits for no admission
// slot, over the outer table `outbox`, unaliased. Five rows have that shape: the row a wake queues
// (workflow's wakeRow), the row a cancelled release queues for each claim it suspended
// (CancelIssueRelease), a root architect's entry start, a production check's look, and an
// architect's start queued for news of the approved design alone (record.SuperviseRequest.News),
// which names no phase for an architect (the daemon's settleNews). StartQueued reads each of them
// but the look and the start of news alone.
const resumeStartRow = `outbox.kind = 'supervise' and outbox.payload->>'op' = 'start'
	and coalesce(outbox.payload->>'phase', '') = '' and coalesce(outbox.payload->>'needsSlot', '') <> 'true'`

// wakeStartRow is the shape of a wake's own start: a resumeStartRow that carries the wake's kind
// (record.SuperviseRequest.WakeKind). Only a wake's start carries one (workflow's wakeRow and
// PauseWakeRow). A root architect's entry start and a production check's look have the shape of
// resumeStartRow and carry no kind: they are work the pod is given, not a wake. Four reads of the
// wakes taken while a release stands read this shape. The teardown passes over a wake's own start
// of a role the release recorded (IssueRelease). A cancel deletes that start
// (dropWakesUnderRelease). The release's record and a cancel rewrite its task
// (retellWakesUnderRelease, retellWakesOf). One more read is the delivery wake's, which tells an
// open resume whose start has not run from one with no start behind it (wakeStartQueued).
const wakeStartRow = resumeStartRow + ` and coalesce(outbox.payload->>'wakeKind', '') <> ''`

// StartQueued is whether the outbox holds a `start` that brings issue's role back at generation:
// a start of that role and generation that names no phase, waits for no admission slot
// (resumeStartRow), looks at no production check and is not queued for news alone. Rows of that
// shape come from a cancelled release (its resume of each claim it suspended: CancelIssueRelease,
// the daemon outbox's releaseResume), a wake (workflow's wakeRow) and an architect start
// (admission, and the resume of an awaited root). Such a row meets no phase fence and waits for no
// slot. When it runs, the outbox's start executor resumes a suspended claim of that role
// (supervise.RequestRuleResume), so a role with one queued is coming back. A production check's
// look has the same shape, but it is fenced on its check and look, and it finishes without acting
// when its look ended before it ran. So a look is not counted: a wake that read it as the role
// coming back would take nothing, and the role would stay released when the look starts nothing.
// An architect's start queued for news of the approved design alone (record.SuperviseRequest.News)
// has the same shape too, and it is not counted for the same reason: it finishes without acting at
// a claim it may not reach, and wherever the record owes nothing when it runs (the daemon's
// startNewsOf). workflow's startsWork reads such a start as no start for the same reason. A row the
// outbox has finished is deleted, so every row this reads is still to run.
func (p *PodRecords) StartQueued(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, generation uint64) (bool, error) {
	return startQueued(ctx, tx, issue, role, generation)
}

// startQueued is StartQueued through tx, which the delivery wake reads in its own transaction too
// (Store.TakeDeliveryWake).
func startQueued(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, generation uint64) (bool, error) {
	var queued bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where issue = $1 and `+resumeStartRow+`
		and coalesce(outbox.payload->>'checkLook', '') = '' and coalesce(outbox.payload->>'news', '') <> 'true'
		and payload @> jsonb_build_object('role', $2::text, 'generation', $3::bigint))`,
		issue, string(role), int64(generation)).Scan(&queued); err != nil {
		return false, fmt.Errorf("read whether a start of %s's %s is queued: %w", issue, role, err)
	}
	return queued, nil
}

// wakeStartQueued is whether the outbox holds a wake's own start (wakeStartRow) that brings issue's
// role back at generation, through tx: the delivery wake reads it for a role whose open resume
// refused its take (Store.TakeDeliveryWake, DeliveryWake.WakeStartQueued).
func wakeStartQueued(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, generation uint64) (bool, error) {
	var queued bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where issue = $1 and `+wakeStartRow+`
		and payload @> jsonb_build_object('role', $2::text, 'generation', $3::bigint))`,
		issue, string(role), int64(generation)).Scan(&queued); err != nil {
		return false, fmt.Errorf("read whether a wake's start of %s's %s is queued: %w", issue, role, err)
	}
	return queued, nil
}

// lockReleaseMark locks issue's release mark, its issue_releases row, `for share`, and answers
// whether the mark stands. It is a wake's half of the lock order PodRecords.TakePodWake gives: a
// release's record and its cancel lock the same row `for update`, so each side waits for the
// other to commit. With no mark there is no row, and nothing is locked.
func lockReleaseMark(ctx context.Context, tx pgx.Tx, project, issue string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `select 1 from issue_releases where project = $1 and issue = $2 for share`,
		project, issue).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock the release mark of %s: %w", issue, err)
	}
	return true, nil
}

// TakePodWake takes one wake of issue's role inside the caller's transaction, and is the whole
// of what taking a wake is: the mark under the wake's own key, the resume recorded against the
// release that wake ends, and the role's own backstop cancelled. It answers whether this call
// took the wake; false is a wake another pass already took, and then the caller resumes nothing,
// because the first wake is already bringing the pod back.
//
// A role is woken once while its pod is coming back, whatever else reaches it: at most one
// resume of a role has no first turn stamped on it (pod_resumes_one_open_per_role), so the
// insert that records this wake is also what takes it. A CI settlement wakes the pod and, in the
// same transaction, sends that worker a role message, whose listener report then arrives while
// the claim is still released; both are the same pod coming back for the same role, and the
// second writes nothing rather than leaving two resumes and two start rows — one describing the
// daemon's own settlement to the worker as a message somebody published — against R6's one task
// per wake. Another role of the same pod is not held by it: a wake resumes one claim, and a role
// whose wake was dropped would have no process in a pod that is running.
//
// The index decides it inside the caller's transaction, so the workflow's four wakes and the
// outbox's delivery wake, which runs in a transaction of its own, are held to it alike; a read
// could be overtaken between its answer and the insert, and this cannot. A wake refused here
// writes nothing of its own, and the cause is not lost, since the message or settlement that
// wakes a role still reaches it once the pod is back — but the resume that refused it stands,
// so a caller whose claim is still not running resumes it all the same rather than reading the
// refusal as a pod already on its way: the outbox's wakeForDelivery resumes the claim in process,
// and the workflow's wakeReleased closes a resume whose pod has gone away before it takes its
// wake (ClosePodGoneResume). The transaction of a release that suspends a role closes the resumes
// left open (CloseOpenResumes), so a pod that came back and whose role took no turn the daemon
// saw holds nothing back at the next release; a release that suspends none closes no resume,
// because it ends no wake, and the resumes its pod leaves behind are the heal's.
//
// A wake can be taken while a release of the pod still runs: after a stop of that release
// recorded the role's claim as `released`, and before its teardown recorded the release. That
// release has no pod_releases row yet, so LatestPodRelease answers the release before it, and the
// resume's seen_release is the one before it too. The teardown's record of the release points the
// resume at the release it ends (RecordPodRelease). A held wake keeps release 0, and the record
// moves only its seen_release and its times. Such a wake does not cancel the release
// (releaseOvertaken), and its `start` waits for the teardown (record's outboxInOrder), so the role
// starts once, in the pod that comes back. A cancel of the release for another cause deletes the
// resume instead (CancelIssueRelease, dropWakesUnderRelease), unless the project is paused: then
// the wake is the launch the pause owes the role, and the cancel makes it a held wake of that
// pause (holdWakesUnderPause). Only a workflow wake, or the owed launch of a pause's stop
// (pods.WakePause), is taken so. The outbox's delivery wake resumes its claim in process, with no
// `start` to wait for the teardown, so it takes nothing while any release mark of the issue
// stands, whether or not that release recorded its role (Store.TakeDeliveryWake).
//
// The take, the record and the cancel run in different transactions, so the record or the cancel
// can run while a take has not committed. Under READ COMMITTED a statement reads only what
// committed before it started. So the three order against each other on the issue's release
// mark, its issue_releases row. The take locks the mark `for share` (lockReleaseMark) before it
// reads LatestPodRelease. The record and the cancel lock it `for update`. When the take locks
// first, the record or the cancel waits until the take commits. Its next statement then reads the
// take's resume: the record points that resume at its release, and the cancel deletes it. When
// the record or the cancel locks first, the take waits. The take then reads the release the
// record wrote, or finds the mark gone. With no mark there is no row, and the take locks nothing.
// Without the lock, a take still open when the record commits keeps the release before, and the
// record never reads its resume; a take still open when the cancel deletes the wakes leaves its
// resume behind.
//
// Three more reads of a wake order on the same lock, each in the take's own transaction. The
// workflow's wake closes a resume whose pod went away before it takes its own
// (ClosePodGoneResume), which reads the newest release. The delivery wake reads whether a mark
// stands (Store.TakeDeliveryWake). Every wake but the stop's owed launch reads which pause holds
// it (PauseOfWake), which reads the newest release too (StoppingPauseTx). Each of the three locks
// the mark `for share` before it reads, so the take's own lock is held already when the take runs.
//
// This lock order cannot deadlock. Each transaction that takes both the mark and the project's
// pause row takes the mark first. A wake locks the mark before the project's pause row
// (PauseOfWake's LockOpenPauseByTokenTx) and before it writes a resume. The stop transaction of a
// project's pause (internal/daemon's projectPause.stopTx), whose owed launch is a take, locks the
// mark first (LockReleaseMark), before the issue's pause lock (LockIssueForPause), the pause's row
// and its writes. The workflow's take and the cancel both take the global fact serializer first
// (treelifecycle.Serialize), then the mark. The cancel takes the issue's pause lock and the pause's
// row after the mark, and before it writes or deletes a start or a resume (CancelIssueRelease).
// The outbox's delivery take holds no lock when it locks the mark. The record holds no lock when
// it locks the mark. It locks the pause's row after the mark, and before it writes a resume or a
// start (Store.RecordPodRelease). A project's resume (Resume) locks the pause's row `for update`
// and never the mark. It then writes the starts the pause holds and the project's open resumes. A
// take, a cancel or a record that waits for that row has written no resume and no start yet. The
// teardown's delete of the mark (ClearIssueRelease) waits for a take's share lock, as every delete
// of a locked row does.
//
// Every wake runs through here — the four the workflow applies inside a fact's transaction, the
// delivery wake the outbox takes in one of its own (Store.TakeDeliveryWake) and the stop
// transaction's owed launch (pods.WakePause) — so there is one account of what a wake writes and
// one order it writes it in.
//
// pause is the pause that holds the wake (PauseOfWake decides it for every wake but the stop's
// owed launch, which names its own pause), and 0 is a wake no pause holds. A held wake records its
// pause on the resume (pod_resumes.pause_id) and pairs with release 0 rather than the issue's
// latest release (decision 15 of dispatch://INFRA-3759): the pause, not a release, is what kept
// the role down, so the release measurements leave its wait out.
func (p *PodRecords) TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind string, at time.Time, pause int64) (bool, error) {
	if _, err := lockReleaseMark(ctx, tx, p.project, issue); err != nil {
		return false, err
	}
	var release int64
	if pause == 0 {
		var err error
		if release, err = LatestPodRelease(ctx, tx, p.project, issue); err != nil {
			return false, err
		}
	}
	_, taken, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: p.project, Issue: issue,
		Role: string(role), Kind: kind, At: at, Pause: pause})
	if err != nil || !taken {
		return false, err
	}
	return true, CancelPodBackstops(ctx, tx, p.project, issue, role)
}

// PauseOfWake is the one rule of which pause holds a wake of claim token, issue's role (decision
// 15 of dispatch://INFRA-3759), and every wake but the stop's owed launch reads it: the outbox's
// delivery wake (Store.TakeDeliveryWake) and the workflow's wakes (its wakeReleased). The stop
// transaction's owed launch (pods.WakePause) names its own pause. held is the first of these that
// names a pause, and 0 where none does:
//
//   - the project's open pause, read on tx with its row locked for share
//     (LockOpenPauseByTokenTx), so the resume's end of it waits for the wake to commit: a wake
//     taken while the project is paused is that pause's, the wake of a row a lane leased before
//     the pause and ran while it stood among them. It is read by the token of the project whose
//     pod tables these are (readOpenPauseByToken says why);
//   - mark, the pause that held the outbox row the wake is taken for (record.OutboxRow.HeldByPause),
//     0 for a wake taken for no row, as the workflow's are;
//   - stopping, the pause that stopped the claim (StoppingPauseTx).
//
// stopping is answered whatever held is, because it is also what the woken role's task names as
// what stopped its session (workflow.WakeTask, decision 16).
//
// StoppingPauseTx reads the issue's newest release, so this locks the issue's release mark `for
// share` first (lockReleaseMark), before the pause's row, as the take does (TakePodWake gives the
// lock order). The lock sets the order of the wake and a release's record: the record comes
// before this read, or after the wake commits. It does not choose the words. A wake of a role
// that a pause stopped, taken while a release that recorded the role on its mark still stands,
// reads the pause as what stopped the role, and its task says that its pod was kept. The
// release's record then takes that pod, and it retells that wake's start in the release's words
// (Store.RecordPodRelease, PodRelease.WakeTask). The record reads that start because it waits for
// the wake to commit.
func (p *PodRecords) PauseOfWake(ctx context.Context, tx pgx.Tx, issue string, token claim.Token, mark int64) (held, stopping int64, err error) {
	if _, err := lockReleaseMark(ctx, tx, p.project, issue); err != nil {
		return 0, 0, err
	}
	// open is the zero Pause, whose id is 0, while the project runs (scanOpenPause).
	open, _, err := LockOpenPauseByTokenTx(ctx, tx, p.project)
	if err != nil {
		return 0, 0, err
	}
	if stopping, err = StoppingPauseTx(ctx, tx, p.project, issue, token); err != nil {
		return 0, 0, err
	}
	return cmp.Or(open.ID, mark, stopping), stopping, nil
}

// LockReleaseMark locks issue's release mark `for share` in tx (lockReleaseMark). The stop
// transaction of a project's pause (internal/daemon's projectPause.stopTx) takes it before
// anything else, because its owed launch is a take, and the take's lock order puts the mark
// before the issue's pause lock, the pause's row and every write of a resume (TakePodWake).
func (p *PodRecords) LockReleaseMark(ctx context.Context, tx pgx.Tx, issue string) error {
	_, err := lockReleaseMark(ctx, tx, p.project, issue)
	return err
}

// ClosePodGoneResume closes the open resume of issue's role when the pod that resume was waiting
// for has gone away since the wake was taken: a release of the issue, of whatever kind, recorded
// since (newestPodRelease, seen_release), or a release whose teardown gave up on its record after
// the resume's time (pod_release_give_ups.at after pod_resumes.at). A release that gave up took
// the pod as a recorded release does, and it writes no pod_releases row (SettleUnrecordedRelease),
// so a read of pod_releases alone would leave such a resume open. Without the close that row would
// refuse every later wake of the role for as long as the issue lives, because a release that
// suspends no role closes no resume and a pod fully released is never released again.
//
// The comparison with a give-up's time is strict. The give-up's settle moves the time of each wake
// taken under that release's mark to the give-up's time where it was earlier, as a record moves it
// to the release's time, so such a resume is not closed by the give-up of the release it was
// taken under: its start is the role's one start, in the pod that comes back.
//
// It is one update that matches nothing in the ordinary case, so a caller runs it before the take
// rather than after a refusal: a resume of a pod still coming back was written after the newest
// release there is, or dated to the give-up of the release it was taken under, so no release
// stands above its own, and the row stands.
//
// It locks the issue's release mark `for share` before it reads the newest release, as the take
// does (lockReleaseMark; PodRecords.TakePodWake gives the lock order). The workflow runs the
// close and the take in one transaction, so the take's own lock is held already when the take
// runs. Without the lock here, a release recorded after this read and before the take's lock is
// missed by the close and read by the take. The take then finds the old resume still open and
// takes nothing, so the wake is lost while the role's pod is gone: a fired backstop is spent, and
// a listener's report is dropped. A give-up is written under the mark's lock too, in the
// transaction that deletes the mark (SettleUnrecordedRelease), so the lock orders it the same way.
//
// It answers whether it closed a row, so the caller can say in the log that a pod went away under
// an open resume, and, where it closed one, the issue's newest recorded release (newest is 0 where
// it closed none). Where a recorded release closed the row, that release is the one standing
// above the resume's own, which with two releases since the wake is the later of them rather than
// the one that took the pod. Where a give-up alone closed it, newest is the resume's own seen
// release, which is 0 for an issue with no recorded release. Nothing else records the close: the
// row it closes leaves the stuck warning and the stranded report with it (release_records.go's
// podResumesWithoutAFirstTurn), so without the line a pod vanishing under a waiting role is
// written down nowhere.
//
// The other way a resume sticks — a daemon that stopped between the take and the resume it leads
// to, with no release since — is the outbox's to heal, in process, because the claim's own state
// is what tells it (the delivery wake, internal/daemon's wakeForDelivery).
func (p *PodRecords) ClosePodGoneResume(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (newest int64, closed bool, err error) {
	if _, err := lockReleaseMark(ctx, tx, p.project, issue); err != nil {
		return 0, false, err
	}
	if newest, err = newestPodRelease(ctx, tx, p.project, issue); err != nil {
		return 0, false, err
	}
	tag, err := tx.Exec(ctx, `update pod_resumes r set closed_at = now()
		where r.project = $1 and r.issue = $2 and r.role = $3 and r.first_turn_at is null and r.closed_at is null
		and (r.seen_release < $4 or exists (select 1 from pod_release_give_ups g
			where g.project = $1 and g.issue = $2 and g.at > r.at))`, p.project, issue, string(role), newest)
	if err != nil {
		return 0, false, fmt.Errorf("close the stale resume of %s's %s: %w", issue, role, err)
	}
	if tag.RowsAffected() == 0 {
		return 0, false, nil
	}
	return newest, true, nil
}
