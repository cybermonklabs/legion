package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// idleWaitingKinds are the outbox kinds that reach a role's own session, which is what "a task, a
// notice, a message or a start waits for a role of the pod" means: a supervise row (a start, the
// task it carries, a stop), an architect's notice, the words a hook fired on a move back puts in
// front of a role (record.HookPrompt), the ask an update round puts to a role of the pod
// (record.UpdateAsk), the message that tells the merger its pull request left the merge queue
// (record.MergeQueueRemoval), and each message of a merge forward, the ask to the implementer or,
// in merging, the merger, and the word to the worker of the phase the issue stays in
// (record.MergeForwardMessage). Two reads of GitHub reach no session themselves, but their
// answers are what write a message, so each holds the pod as its message does: the read that
// comes before a merge-queue removal's message (record.MergeQueueRead), and the second read of a
// head a first read found conflicting, whose confirmation writes a merge forward's messages
// (record.ConflictConfirm). The issue's board writes — its Dispatch status, its messages on the
// Dispatch issue — reach no session and hold no pod. The same set is what the release's boundary
// is read over (IssueRelease), so the rule that holds a pod and the rule that cancels its release
// never disagree about which row speaks to a role. Every kind of the role-message lane belongs
// here: one left out lets the daemon release the
// issue's pod out from under the row that was just written for one of its roles, and that role
// never reads it. An update ask holds the pod only while its row is pending, which is one attempt
// where the publish goes through or finds no holder.
var idleWaitingKinds = []string{"supervise", "notice", "hook_prompt", "update_ask", "merge_queue_read", "merge_queue_removal",
	"conflict_confirm", "merge_forward"}

// idleWaitingRow is the predicate, over the outbox with idleWaitingKinds as $2, of a row that
// waits for a role of the pod, and so holds the pod and cancels its release at the boundary
// (readIdleView, IssueReleaseStop), and, where it is a start, at the teardown (IssueRelease): a
// row of one of those kinds, but a start queued for news of the approved design alone
// (record.SuperviseRequest.News, which its payload writes as `"news": true`). Such a start
// reaches only a claim already given its phase's entry, and the claim an idle release suspended
// is one: the start resumes it as any start does, so a release under the start loses nothing, and
// the role comes back with its news. And such a start can wait a long time. A merger's waits for
// its tree's design gate for the whole time the gate stays closed (the daemon's
// mergerWaitsForTheDesignGate), and any one waits behind a task its claim has not finished
// (supervise's queue). A pod held for it would stand idle for all that time. The teardown passes
// over one start of this predicate: a wake's own start of a role the release recorded, with a
// wake since the mark (wakeStartRow, IssueRelease). That start waits for the teardown and starts
// the role in the pod that comes back.
const idleWaitingRow = `kind = any($2) and not (kind = 'supervise' and coalesce(payload->>'news', '') = 'true')`

// IdleReleaseRow is one outbox row a release writes, encoded by its caller (record.NewOutboxRow),
// in the order the issue's lifecycle lane runs them: the check of the workspace for unpushed
// work, then one role stop per live claim, then the teardown. The store writes them rather than
// record's Enqueue because the boundary row's id must be read back under the same transaction.
//
// Boundary marks the one row whose id is the release's boundary: the stop of the first role
// process of the pod, which is what every later decision about this release reads
// (IssueRelease). The check before it is not the boundary — it stops nothing, so a delivery
// queued while it runs still cancels the whole release.
type IdleReleaseRow struct {
	Kind     string
	Payload  []byte
	Boundary bool
}

// IdleReleaseMark is what a decision to release writes: the rows above and the role whose shim
// the workspace is read through, which is also the role told when the read finds unpushed work.
// Rows empty means the pod is not released, and nothing is written.
//
// SuspendsNoRole is the release of a pod no role can use (workflow.UnownedPodRelease), whose
// every claim is already failed, retired or suspended: it suspends no role and reads no
// workspace, so it names no role and queues the teardown alone. The mark records which of the two rules
// wrote it (issue_releases.kind), because what a release did is what its teardown and its
// cancel must read: a release that suspended nothing has nothing to resume and no roles to
// record. A mark that suspends a role names it, and one that suspends none leaves Role empty.
type IdleReleaseMark struct {
	Generation     uint64
	Role           claim.Role
	SuspendsNoRole bool
	// Close is no release: it is the close of an issue the workflow has taken out
	// (workflow.LeftPodClose), whose rows are the stop of each live role and the issue's Sandbox
	// suspension. They are written with no mark, because no release is under way and every row of
	// a close is fenced on the issue's own record (the outbox's supervise executor,
	// IssueSuspension) rather than on a mark. Role and SuspendsNoRole say nothing of it.
	Close bool
	Rows  []IdleReleaseRow
}

// IdleReleaseDecide is what ReleaseIdleIssue hands the view it reads to: a rule's decision as the
// rows it writes (IdleReleaseMark), none where the rule writes nothing. It is an alias, so a caller
// that names it and one that spells the func type out name one type.
type IdleReleaseDecide = func(pods.IdleView) (IdleReleaseMark, error)

// releaseKindIdle and releaseKindUnowned are the two release rules as issue_releases.kind spells
// them: the release of a pod whose roles are all quiet, and the release of a pod no role can use
// any more. Every reader of a mark reads the kind rather than guessing the rule from the role,
// which is empty for the second and for nothing else.
const (
	releaseKindIdle    = "idle"
	releaseKindUnowned = "unowned"
)

// IdlePods is every issue of project whose pod has a live claim — one neither failed, retired nor
// already suspended — in key order: the issues the idle tick decides. An issue whose claims are
// all stopped has no role to read the workspace through and no claim to suspend, so there is nothing for a
// release to do with it.
func (s *Store) IdlePods(ctx context.Context, project string) ([]string, error) {
	what := "list the pods of " + project
	var issues []string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select distinct issue from claims
			where project = $1 and state not in ('failed', 'retired', 'suspended') and issue <> '' order by issue`, project)
		if err != nil {
			return err
		}
		issues, err = issueKeys(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return issues, nil
}

// issueKeys reads a one-column answer of issue keys into a slice: the candidate lists of the two
// release rules are two statements and one loop (IdlePods, UnownedPods), and each caller names
// whatever goes wrong once. An answer of no rows is a nil slice, which is what a caller reads as
// no candidate, and the rows are closed whichever way the read ends.
func issueKeys(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return issues, nil
}

// UnownedPods is every issue of project whose pod no role can use, in key order: one that holds
// at least one claim, holds no claim whose state is outside ('failed', 'retired', 'suspended')
// — no live claim, that is — and whose own row and whose tree root's row both still stand in
// the workflow. It is the candidate list of the second release rule
// (workflow.UnownedPodRelease), beside IdlePods, which answers the other half: the pods a live
// claim still holds. Such a pod holds its reservation (2 CPU and 8 GiB at the defaults) with no
// role that can take a turn in it, which is what the rule releases.
//
// The project is spelled two ways, and the two are not the same string, so this read takes
// both. project is the issue record's own project (issues.project, the Dispatch project key
// the records carry, "LEGION"), and claimProject is the claim token's spelling the claims
// table holds (claims.project, "legion"), which is what IdlePods and ReleaseIdleIssue read.
// The caller has both already (daemon.releaseUnownedPods: w.dispatchProject and w.projectID).
// The claims this read asks about are this project's, as they are in every other claim read of
// this file and of the cancel's (IdlePods, readIdleView, release_cancel.go's stillReleasedRoles),
// and the predicate costs nothing: the subqueries scan the same rows either way.
//
// outOfWorkflow is the workflow's list of statuses that take an issue out of it
// (record.OutOfWorkflowStatuses), which the store does not restate: the gate every other path
// reads is a Go predicate (pods.InWorkflowNotLingering) and a select cannot call one, so the
// rule arrives as a list and the filter is this statement's three conditions per row — the
// status outside that list, the phase short of done, and no linger.
//
// Those three conditions are what the filter is for, since the claim states alone would answer
// every issue of the deployment whose roles ever stopped. Every candidate costs one transaction
// of ReleaseIdleIssue, which opens by taking the daemon's global fact serializer
// (treelifecycle.Serialize, pg_advisory_xact_lock(hashtext('legion.apply_fact'))) — the same
// lock the intake takes for every fact, so a tick of candidates is a tick of facts not applied.
// The reading behind that: on the INFRA deployment at 2026-11-10 19:39Z, 1,136 issues held
// claims while 5 stood in the workflow under a live tree.
//
// It adds no index of its own, and the answer is read once a minute. The partial index
// issues_live (migrations/0062_live_issue_reads.up.sql) serves the issue rows: the query's
// i.phase <> 'done' implies that index's predicate. Nothing serves the two subqueries over
// claims, which carries the token primary key and claims_boot_token_hash and no index on issue
// or project at all (migrations/0002_claims.up.sql), so each of them reads the whole of claims,
// once a minute — the same relation, at the same rate, that IdlePods already scans whole for
// its own answer.
func (s *Store) UnownedPods(ctx context.Context, project, claimProject string, outOfWorkflow []string) ([]string, error) {
	what := "list the pods of " + project + " no claim owns"
	var issues []string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select i.key from issues i join issues root on root.key = i.tree
			where i.project = $1
			and i.status <> all($2) and i.phase <> 'done' and i.linger_until is null
			and root.status <> all($2) and root.phase <> 'done' and root.linger_until is null
			and exists (select 1 from claims c where c.issue = i.key and c.project = $3)
			and not exists (select 1 from claims c where c.issue = i.key and c.project = $3
				and c.state not in ('failed', 'retired', 'suspended'))
			order by i.key`, project, outOfWorkflow, claimProject)
		if err != nil {
			return err
		}
		issues, err = issueKeys(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return issues, nil
}

// LeftPods is every child issue of project that has left the workflow while its tree runs on —
// its phase done and its status one outOfWorkflow names, under a tree root that is in the
// workflow and does not linger — and whose claims have all stopped, in key order: the candidates
// of the close of such a pod (workflow.LeftPodClose), beside IdlePods and UnownedPods, which
// answer the pods with a live claim and the pods of issues still in the workflow. Its arguments
// are UnownedPods's, spelled as that method says.
//
// Such an issue's own departure was its one close, and nothing of its running tree closes it
// again, so a pod of it that still stands — one an operator's resume, task or retry, or the
// controller's fresh session, built again after that close and whose roles have stopped since —
// is a pod no other rule takes. Nearly every issue this answers has no pod at all, because its
// close deleted it; the caller asks the runtime before it opens a transaction for one.
func (s *Store) LeftPods(ctx context.Context, project, claimProject string, outOfWorkflow []string) ([]string, error) {
	what := "list the pods of " + project + " whose issue left the workflow under a running tree"
	var issues []string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select i.key from issues i join issues root on root.key = i.tree
			where i.project = $1 and i.key <> i.tree
			and i.phase = 'done' and i.status = any($2) and i.linger_until is null
			and root.status <> all($2) and root.phase <> 'done' and root.linger_until is null
			and exists (select 1 from claims c where c.issue = i.key and c.project = $3)
			and not exists (select 1 from claims c where c.issue = i.key and c.project = $3
				and c.state not in ('failed', 'retired', 'suspended'))
			order by i.key`, project, outOfWorkflow, claimProject)
		if err != nil {
			return err
		}
		issues, err = issueKeys(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return issues, nil
}

// ReleaseIdleIssue is the release's one transaction: it reads issue's idle view and hands it to
// decide, and when decide answers rows it writes them and the release's mark together, so the
// view the decision rests on and the mark that records it are one atomic act. It is the close's
// one transaction too, for the pod of an issue the workflow has taken out
// (IdleReleaseMark.Close): the rows that close writes rest on the same view, and nothing else.
//
// The mark records the ids the rows were given: first_row and last_row are the release's own
// first and last, which is how a row of this release is told from a row of one that is over
// (IssueReleaseStop, IssueRelease), and boundary_row is the row the mark names its boundary —
// the stop of the first role process of the pod — which is where a delivery that reached the
// pod after the mark, while every role still ran, cancels the release. Once a stop has recorded
// its role's claim as `released`, a wake of that role no longer cancels the release: the release
// runs to its end, and the role comes back in the new pod (releaseOvertaken).
//
// It answers whether it wrote the decision's rows: a release's with its mark, or a close's
// (IdleReleaseMark.Close), which carries none. decide answering no rows writes nothing, which is
// the ordinary case: nearly every tick finds a pod that is working, or one whose idle period has
// not passed.
//
// A project that is paused is marked no release and written no close, whatever decide answers:
// the pause starts no release (#4115's project pause). decide is still handed the
// view, which carries the pause (pods.IdleView.Paused), so its caller can record what kept the
// pod. The view is read under treelifecycle.Serialize, which the pause's transaction takes too,
// so a pause and a mark commit one after the other.
//
// A mark that names no role and does not say it suspends none is refused before anything is
// written, as a fault of the rule that decided it rather than of the database: the check of the
// workspace is read through the role the mark names, so a release without one and without the
// second rule's kind is a release nobody can carry out.
func (s *Store) ReleaseIdleIssue(ctx context.Context, project, issue string, decide func(pods.IdleView) (IdleReleaseMark, error)) (bool, error) {
	if project == "" || issue == "" {
		return false, errors.New("an idle release needs its project and its issue")
	}
	var marked bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		view, err := readIdleView(ctx, tx, project, issue)
		if err != nil {
			return err
		}
		mark, err := decide(view)
		if err != nil || len(mark.Rows) == 0 {
			return err
		}
		if mark.Close {
			if view.Releasing || view.Paused {
				return nil
			}
			for _, row := range mark.Rows {
				if _, err := insertOutboxRow(ctx, tx, project, issue, row); err != nil {
					return err
				}
			}
			marked = true
			return nil
		}
		if mark.Role == "" && !mark.SuspendsNoRole {
			return fmt.Errorf("the release of %s names no role and is not a release that suspends none", issue)
		}
		if view.Releasing || view.Paused {
			return nil
		}
		var first, boundary, last int64
		for _, row := range mark.Rows {
			id, err := insertOutboxRow(ctx, tx, project, issue, row)
			if err != nil {
				return err
			}
			if first == 0 {
				first = id
			}
			last = id
			if row.Boundary {
				boundary = id
			}
		}
		if boundary == 0 {
			return fmt.Errorf("the release of %s named no boundary row", issue)
		}
		// The newest start row and the newest turn of the pod's claims (pods.IdleView.NewestTurnStart):
		// a start row above the one or a turn after the other is the pod given work since this mark
		// (releaseOvertaken). The turn is the supervisor's own stamp (claims.last_turn_start), so the
		// two compare on one clock; null where no claim has had a turn.
		var started int64
		for _, c := range view.Claims {
			started = max(started, c.LastStartRow)
		}
		var turned *time.Time
		if newest := view.NewestTurnStart(); !newest.IsZero() {
			turned = &newest
		}
		// The newest wake this pod has had: a wake recorded above it is one this release never
		// saw. It cancels the release, unless it is a wake of a role this release has since
		// recorded as `released` (releaseOvertaken).
		var wake int64
		if err := tx.QueryRow(ctx, `select coalesce(max(id), 0) from pod_resumes where project = $1 and issue = $2`,
			project, issue).Scan(&wake); err != nil {
			return fmt.Errorf("read the newest wake of %s: %w", issue, err)
		}
		// The kind is the rule that decided this release, and the role is the one whose shim the
		// check reads the workspace through: a release that suspends no role has none, and leaves the column
		// empty, which is the one spelling the table's check takes beside that kind.
		kind, role := releaseKindIdle, string(mark.Role)
		if mark.SuspendsNoRole {
			kind, role = releaseKindUnowned, ""
		}
		// The mark starts with no teardown noted (TeardownNotReached), written here because the
		// column's default is TeardownReached, the note of every mark an earlier release writes.
		if _, err := tx.Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, wake_row, role, kind, marked_at, turn_start, teardown)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), $11, $12)`,
			project, issue, int64(mark.Generation), first, boundary, last, started, wake, role, kind, turned,
			string(TeardownNotReached)); err != nil {
			return fmt.Errorf("mark the release of %s: %w", issue, err)
		}
		// The pod this release stops is gone, so a resume of it still open — a wake whose pod
		// came back and whose role took no turn the daemon saw — belongs to that pod and not to
		// the next one. Closing them here, in the transaction that marks the release, is what
		// lets the next wake of each role take its own (store's TakePodWake, one open resume per
		// role).
		//
		// A release that suspends no role is no wake's to end, so it closes none. It stops a pod
		// whose every claim is failed, retired or suspended, which is a pod no wake of this issue
		// is waiting on: a claim an earlier release left `released` is waiting for the pod that
		// release took, not for this one, and its resume is the record of that wait. Closing it
		// here would stamp that release woken and never turned up, and charge it from its own
		// moment to every later read — the distortion the records exist to remove, moved onto the
		// row beside it. internal/daemon's
		// TestAnIssueWithNoLiveClaimHasItsSandboxReleased/"the pod holding a claim an earlier
		// release left released" is what holds this, and the teardown's own reading in that
		// package's outbox arms no backstop here for the same reason. A resume this release
		// leaves open is closed by the next wake of its role, which reads that a release was
		// recorded after that wake was taken (store's ClosePodGoneResume, the workflow's
		// wakeReleased).
		if !mark.SuspendsNoRole {
			if err := CloseOpenResumes(ctx, tx, project, issue); err != nil {
				return err
			}
		}
		marked = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("release the idle pod of %s: %w", issue, err)
	}
	return marked, nil
}

// insertOutboxRow queues one outbox row of project, encoded by the caller, and answers the id the
// outbox gave it: the ids of a release's own rows are the range its mark is read against
// (IssueReleaseStop), and a resume a cancel queues takes its id from the same sequence, above every
// row of the release it ends. A pause's stop rows and its notices are queued through it too
// (queuePauseStopRow, queuePauseNotice).
//
// project is the caller's project, its Dispatch key or its claim token's spelling, and a row keyed
// by an issue of another project is refused (claim.KeyOfProject): only that project's daemon runs
// it, against that daemon's own claims. The refusal is an error (claim.ErrKeyOfAnotherProject),
// which rolls the caller's transaction back, since every caller writes the row together with what
// names it — a release's mark, a cancel's end of one, a pause and its stops — and neither stands
// without the other.
func insertOutboxRow(ctx context.Context, tx pgx.Tx, project, issue string, row IdleReleaseRow) (int64, error) {
	if !claim.KeyOfProject(project, issue) {
		return 0, fmt.Errorf("queue the %s row of %s for project %s: %w", row.Kind, issue, project, claim.ErrKeyOfAnotherProject)
	}
	var id int64
	if err := tx.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '') returning id`, row.Kind, issue, string(row.Payload)).Scan(&id); err != nil {
		return 0, fmt.Errorf("queue the %s row of %s: %w", row.Kind, issue, err)
	}
	return id, nil
}

// IdleReleaseDecline is what the workspace check found when it declines a release, or the close of
// a left pod (DeclineIssueRelease, DeclineLeftPodClose). At is the moment of the check, by the
// caller's clock, which the rules compare a later turn's start with (pods.IdleDecline.Stands).
// Role is the role the workspace was read through, and Reason is one of pods.DeclineUnpushedWork
// and pods.DeclineWorkspaceUnread. Head, Commits and Changed are what the read found: the
// working-copy commit, how many commits on a working copy or a local bookmark no branch or tag on
// origin holds, and whether the working copy holds a change; a read that failed found none of
// them. PodUID is the pod the read was made in, "" where the runtime could not say. Message is the
// one row that tells the role the release waits for its push; nil sends none.
type IdleReleaseDecline struct {
	At      time.Time
	Role    claim.Role
	Reason  string
	Head    string
	Commits int
	Changed bool
	PodUID  string
	Message *IdleReleaseRow
}

// check refuses a decline the rules could not read.
func (d IdleReleaseDecline) check(issue string) error {
	if d.At.IsZero() || d.Role == "" ||
		(d.Reason != pods.DeclineUnpushedWork && d.Reason != pods.DeclineWorkspaceUnread) || d.Commits < 0 {
		return fmt.Errorf("the decline of %s's release needs its moment, its role and a reason the idle rule reads (%q)", issue, d.Reason)
	}
	return nil
}

// DeclineLeftPodClose records the decline of a left pod's close, whose workspace check found work
// that origin does not hold or could not read the workspace (internal/daemon's
// checkLeftPodWorkspace), under the shared serializer as every writer of the issue's lifecycle
// takes it. That close marked no release, so there is nothing to cancel: the check queued no stop,
// and the pod stays with its roles as they are. It answers whether the decline's message was
// queued (recordDecline).
func (s *Store) DeclineLeftPodClose(ctx context.Context, project, issue string, decline IdleReleaseDecline) (bool, error) {
	if project == "" || issue == "" {
		return false, errors.New("the decline of a left pod's close needs its project and its issue")
	}
	if err := decline.check(issue); err != nil {
		return false, err
	}
	var told bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		told, err = recordDecline(ctx, tx, project, issue, decline)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("decline the close of %s's pod: %w", issue, err)
	}
	return told, nil
}

// recordDecline writes the decline of issue's release (idle_release_declines, one row per issue)
// and queues its message, through the caller's transaction, and answers whether it queued the
// message. The row's started_row is the newest start row of the issue's claims, read in this
// transaction, so a role process started after it ends the decline (pods.IdleDecline.Stands).
//
// A decline that finds the same work as the decline on record, read through the same role — the
// same working-copy commit, the same count of commits, the same change and the same role — is a
// read that role was already told of: it moves checked_at, started_row and pod_uid, sets the
// reason, keeps declined_at, and queues no message. That is what ends the loop a message would
// otherwise start. The message starts a turn in the role's session, the turn ends the decline (it
// can change the workspace), the next read finds the same work, and a second message would start
// the next turn. The role is part of the key because the row names the role that was told: a role
// that takes over, the next phase's worker, is told of the work once, as its own.
//
// A decline whose read failed (pods.DeclineWorkspaceUnread) says nothing of the work, so it keeps
// what the last read that could read found, and the role that read told: on a row that stands it
// moves the reason, checked_at, started_row and pod_uid alone. The next read that finds the same
// work through the same role then tells nobody, and the pod's disk is still read as holding that
// work where no role runs to read it again (pods.IdleDecline.Work). A failed read made in another
// pod than the row's keeps no work, since the work was found on a disk that pod replaced. On no
// row, it writes a row that holds no work. Any other decline replaces the row whole, at its own
// moment, and queues its message.
func recordDecline(ctx context.Context, tx pgx.Tx, project, issue string, decline IdleReleaseDecline) (bool, error) {
	const startedRow = `(select coalesce(max(last_start_row), 0) from claims where project = $1 and issue = $2)`
	if decline.Reason == pods.DeclineWorkspaceUnread {
		// The row's pod and the failed read's are both named and differ: the pod was made again.
		const otherPod = `idle_release_declines.pod_uid <> '' and excluded.pod_uid <> ''
			and idle_release_declines.pod_uid <> excluded.pod_uid`
		if _, err := tx.Exec(ctx, `insert into idle_release_declines
			(project, issue, declined_at, checked_at, role, reason, started_row, pod_uid)
			values ($1, $2, $3, $3, $4, $5, `+startedRow+`, $6)
			on conflict (project, issue) do update set reason = excluded.reason,
				checked_at = greatest(idle_release_declines.declined_at, excluded.checked_at),
				head = case when `+otherPod+` then '' else idle_release_declines.head end,
				commits = case when `+otherPod+` then 0 else idle_release_declines.commits end,
				changed = case when `+otherPod+` then false else idle_release_declines.changed end,
				started_row = excluded.started_row, pod_uid = excluded.pod_uid`,
			project, issue, decline.At, string(decline.Role), decline.Reason, decline.PodUID); err != nil {
			return false, fmt.Errorf("record the decline of %s's release: %w", issue, err)
		}
		return false, nil
	}
	var same bool
	err := tx.QueryRow(ctx, `select head = $3 and commits = $4 and changed = $5 and role = $6
		from idle_release_declines where project = $1 and issue = $2 for update`,
		project, issue, decline.Head, decline.Commits, decline.Changed, string(decline.Role)).Scan(&same)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read the decline of %s's release: %w", issue, err)
	}
	if same {
		if _, err := tx.Exec(ctx, `update idle_release_declines set checked_at = greatest(declined_at, $3),
			reason = $4, started_row = `+startedRow+`, pod_uid = $5 where project = $1 and issue = $2`,
			project, issue, decline.At, decline.Reason, decline.PodUID); err != nil {
			return false, fmt.Errorf("record the decline of %s's release: %w", issue, err)
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `insert into idle_release_declines
		(project, issue, declined_at, checked_at, role, reason, head, commits, changed, started_row, pod_uid)
		values ($1, $2, $3, $3, $4, $5, $6, $7, $8, `+startedRow+`, $9)
		on conflict (project, issue) do update set declined_at = excluded.declined_at,
			checked_at = excluded.checked_at, role = excluded.role, reason = excluded.reason,
			head = excluded.head, commits = excluded.commits, changed = excluded.changed,
			started_row = excluded.started_row, pod_uid = excluded.pod_uid`,
		project, issue, decline.At, string(decline.Role), decline.Reason, decline.Head, decline.Commits, decline.Changed,
		decline.PodUID); err != nil {
		return false, fmt.Errorf("record the decline of %s's release: %w", issue, err)
	}
	if decline.Message == nil {
		return false, nil
	}
	if _, err := insertOutboxRow(ctx, tx, project, issue, *decline.Message); err != nil {
		return false, err
	}
	return true, nil
}

// ForgetIdleDecline deletes the decline of issue's release, if one stands: the issue is done or
// its tree is closing, so no idle release of its pod is coming, or a workspace check has just read
// the workspace and found nothing that origin does not hold, so the work the decline named is gone
// and no rule may keep the pod for it. A decline the table does not hold is success, so a retried
// delete finishes.
//
// It takes no serializer of its own. A caller that joins it to a transaction that also writes the
// issue's lifecycle takes the lifecycle serializer (treelifecycle.Serialize) first, before this
// delete, as every writer of the decline row does (recordDecline's callers): a delete of the row
// ahead of the serializer would hold the row while waiting for the serializer, and a writer that
// holds the serializer and writes the row would wait for it, a deadlock
// (checkLeftPodWorkspace in internal/daemon).
func (s *Store) ForgetIdleDecline(ctx context.Context, project, issue string) error {
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `delete from idle_release_declines where project = $1 and issue = $2`, project, issue)
		return err
	}); err != nil {
		return fmt.Errorf("forget the declined release of %s: %w", issue, err)
	}
	return nil
}

// ClearIssueRelease forgets the mark of issue's release once the release finished. A cancel
// forgets its mark in the transaction that queues its resumes (CancelIssueRelease). A mark
// already gone is success, so a retried finish finishes.
func (s *Store) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return deleteIssueRelease(ctx, tx, project, issue)
	}); err != nil {
		return fmt.Errorf("clear the release mark of %s: %w", issue, err)
	}
	return nil
}

// NoteReleaseTeardown notes on the mark of release, issue's release, whether an attempt of its
// teardown came to the runtime's first write (ReleaseTeardown). A mark that is gone, or that
// belongs to another release, takes nothing: that release is over, and its teardown reads it so.
func (s *Store) NoteReleaseTeardown(ctx context.Context, project, issue string, release int64, teardown ReleaseTeardown) error {
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update issue_releases set teardown = $4 where project = $1 and issue = $2 and id = $3`,
			project, issue, release, string(teardown))
		return err
	}); err != nil {
		return fmt.Errorf("note the teardown of the release of %s: %w", issue, err)
	}
	return nil
}

// deleteIssueRelease forgets the mark of issue's release in the caller's transaction. One mark,
// one delete, whichever runs it.
func deleteIssueRelease(ctx context.Context, tx pgx.Tx, project, issue string) error {
	_, err := tx.Exec(ctx, `delete from issue_releases where project = $1 and issue = $2`, project, issue)
	return err
}

// readIdleView is issue's pod as the idle rules read it (pods.IdleView), through tx; project is
// the project token, as the claims hold it.
//
// The issue's record counts only where its project is project's (issues.project holds the
// Dispatch key, which the token lowercases), and any other record is read as none: the view
// answers Found false, as for an issue no record holds, and both rules then keep the pod
// (workflow.IdleHoldNoPod, IdleHoldUnownedNoClaim). The issues table holds the issues of every
// project on the database, and the operator's spawn makes a claim of another project's issue
// under this project's token, so its pod can find the record that project's daemon keeps of the
// issue. A release decided on that record would queue rows keyed by the other project's issue,
// which this project's outbox never runs (record's DueRows), under a mark that would never
// clear. The root is read through the issue's own tree and needs no check of its own: a record
// takes its root's project when the workflow writes it, so the root of a record of this project
// is this project's too.
func readIdleView(ctx context.Context, tx pgx.Tx, project, issue string) (pods.IdleView, error) {
	view := pods.IdleView{Issue: issue}
	var issueGeneration, rootGeneration int64
	var issuePhase, rootPhase string
	err := tx.QueryRow(ctx, `select i.tree, i.phase, i.status, i.generation, i.linger_until,
		root.phase, root.status, root.generation, root.linger_until,
		root.phase = 'awaiting_person', root.awaiting_answered_at is not null
		from issues i join issues root on root.key = i.tree where i.key = $1 and lower(i.project) = $2`, issue, project).Scan(
		&view.Tree, &issuePhase, &view.Status, &issueGeneration, &view.LingerUntil,
		&rootPhase, &view.Root.Status, &rootGeneration, &view.Root.LingerUntil,
		&view.Root.AwaitingPerson, &view.Root.Answered)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return view, nil
	case err != nil:
		return pods.IdleView{}, fmt.Errorf("read %s for its idle view: %w", issue, err)
	}
	view.Found = true
	view.Phase, view.Root.Phase = phase.Phase(issuePhase), phase.Phase(rootPhase)
	view.Generation, view.Root.Generation = uint64(issueGeneration), uint64(rootGeneration)

	claims, err := tx.Query(ctx, `select c.role, c.state, c.plugin_contract, c.serving_generation,
		c.last_start_row, c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at,
		c.update_round, c.updated_at, c.locator is not null, c.suspended_by,
		greatest(d.delivered_at, d.confirmed_at), c.last_turn_start
		from claims c left join pending_task_deliveries d on d.claim_token = c.token
		where c.project = $1 and c.issue = $2 order by c.token`, project, issue)
	if err != nil {
		return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}
	defer claims.Close()
	for claims.Next() {
		var c pods.IdleClaim
		var role, state, kinds, suspendedBy string
		var servingGeneration int64
		var activityAt, reminder, lastTurn, lastTurnStart *time.Time
		if err := claims.Scan(&role, &state, &c.PluginContract, &servingGeneration, &c.LastStartRow,
			&activityAt, &c.ActivityBackground, &kinds, &reminder, &c.UpdateRound, &c.UpdatedAt,
			&c.HasProcess, &suspendedBy, &lastTurn, &lastTurnStart); err != nil {
			return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
		}
		c.Role, c.State = claim.Role(role), supervise.ClaimState(state)
		c.SuspendedBy = supervise.SuspendSource(suspendedBy)
		c.ServingGeneration = uint64(servingGeneration)
		c.ActivityAt, c.ActivityNextReminderAt, c.LastTurnAt = orZero(activityAt), orZero(reminder), orZero(lastTurn)
		c.LastTurnStart = orZero(lastTurnStart)
		if kinds != "" {
			c.ActivityKinds = strings.Split(kinds, ",")
		}
		view.Claims = append(view.Claims, c)
	}
	if err := claims.Err(); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}

	// The rows of a close are read with the rows that wait for a role, in the one statement:
	// a role stop of the issue (each supervise op a close, a linger's close or an idle release
	// stops a role with), the issue's Sandbox suspension, the bound row of an open look of its
	// production check, whose end is a close of its own, and a workspace check, which a left
	// pod's close queues before any stop (the outbox kind keeps the name of the release's save,
	// record.OutboxKindIssueBundle).
	if err := tx.QueryRow(ctx, `select
		exists (select 1 from outbox where issue = $1 and `+idleWaitingRow+`),
		exists (select 1 from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' in ('suspend', 'issue_close', 'tree_close', 'release')),
		exists (select 1 from outbox where issue = $1 and kind = 'issue_suspend'),
		exists (select 1 from outbox where issue = $1 and kind = 'check_look'),
		exists (select 1 from outbox where issue = $1 and kind = 'issue_bundle')`,
		issue, idleWaitingKinds).Scan(&view.Waiting, &view.RoleStopQueued, &view.IssueSuspendQueued,
		&view.CheckLookOpen, &view.WorkspaceCheckQueued); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
	}
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_releases where project = $1 and issue = $2)`,
		project, issue).Scan(&view.Releasing); err != nil {
		return pods.IdleView{}, fmt.Errorf("read whether %s is being released: %w", issue, err)
	}
	// The release the daemon last declined for the issue, which holds the pod while it stands
	// (pods.IdleDecline). A view with no such row reads the zero decline. The pod the runtime holds
	// now is no fact of this store's: the daemon's pass sets it (pods.IdleView.PodUID).
	err = tx.QueryRow(ctx, `select declined_at, checked_at, reason, started_row, pod_uid, commits > 0 or changed
		from idle_release_declines where project = $1 and issue = $2`, project, issue).Scan(&view.Declined.At,
		&view.Declined.CheckedAt, &view.Declined.Reason, &view.Declined.StartedRow, &view.Declined.PodUID, &view.Declined.Work)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pods.IdleView{}, fmt.Errorf("read the declined release of %s: %w", issue, err)
	}
	// The pause's two facts, read by the view's project token as readOpenPauseByToken reads a
	// pause (which says why): project_pauses and the pod holds carry its Dispatch key, which
	// project, the token (claim.ProjectToken), lowercases for every key project_pauses admits.
	// The view is read under treelifecycle.Serialize, which the pause's own transaction takes
	// too, so a release is marked either before a pause or not at all (ReleaseIdleIssue,
	// decision 12 of dispatch://INFRA-3759).
	if err := tx.QueryRow(ctx, `select
		exists (select 1 from project_pauses where lower(project) = $1 and ended_at is null),
		exists (select 1 from project_pause_pod_holds where lower(project) = $1 and issue = $2 and ended_at is null)`,
		project, issue).Scan(&view.Paused, &view.PauseHeld); err != nil {
		return pods.IdleView{}, fmt.Errorf("read whether a pause holds %s: %w", issue, err)
	}
	holding, err := openPodHold(ctx, tx, project, issue)
	if err != nil {
		return pods.IdleView{}, err
	}
	view.Holding = holding
	return view, nil
}
