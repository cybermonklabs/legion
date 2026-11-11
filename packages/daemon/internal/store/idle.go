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
// task it carries, a stop), an architect's notice, and the message a phase's end sends the role
// whose phase ended. The issue's board writes — its Dispatch status, its merge-queue packet —
// reach no session and hold no pod. The same set is what the release's boundary is read over
// (IssueRelease), so the rule that holds a pod and the rule that cancels its release never
// disagree about which row speaks to a role.
var idleWaitingKinds = []string{"supervise", "notice", "phase_ended"}

// IdleReleaseRow is one outbox row a release writes, encoded by its caller (record.NewOutboxRow),
// in the order the issue's lifecycle lane runs them: the save of the workspace's unpushed work,
// then one role stop per live claim, then the teardown. The store writes them rather than
// record's Enqueue because the boundary row's id must be read back under the same transaction.
//
// Boundary marks the one row whose id is the release's boundary: the stop of the first role
// process of the pod, which is what every later decision about this release reads
// (IssueRelease). The save before it is not the boundary — it stops nothing, so a delivery
// queued while it runs still cancels the whole release.
type IdleReleaseRow struct {
	Kind     string
	Payload  []byte
	Boundary bool
}

// IdleReleaseMark is what a decision to release writes: the rows above and the role whose shim
// the bundle is asked of. Rows empty means the pod is not released, and nothing is written.
//
// SuspendsNoRole is the release of a pod no role can use (workflow.UnownedPodRelease), whose
// every claim is already failed, retired or suspended: it suspends no role and saves no bundle,
// so it names no role and queues the teardown alone. The mark records which of the two rules
// wrote it (issue_releases.kind), because what a release did is what its teardown and its
// cancel must read: a release that suspended nothing has nothing to resume and no roles to
// record. A mark that suspends a role names it, and one that suspends none leaves Role empty.
type IdleReleaseMark struct {
	Generation     uint64
	Role           claim.Role
	SuspendsNoRole bool
	Rows           []IdleReleaseRow
}

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
// all stopped has no role to ask for a bundle and no claim to suspend, so there is nothing for a
// release to do with it.
func (s *Store) IdlePods(ctx context.Context, project string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select distinct issue from claims
		where project = $1 and state not in ('failed', 'retired', 'suspended') and issue <> '' order by issue`, project)
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", project, err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, fmt.Errorf("list the pods of %s: %w", project, err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", project, err)
	}
	return issues, nil
}

// UnownedPods is every issue of project whose pod no role can use, in key order: one that holds
// at least one claim, holds no claim whose state is outside ('failed', 'retired', 'suspended')
// — no live claim, that is — and whose own row and whose tree root's row both still stand in
// the workflow. It is the candidate list of the second release rule
// (workflow.UnownedPodRelease), beside IdlePods, which answers the other half: the pods a live
// claim still holds. Such a pod stands with 3 CPU and 19 GiB reserved and no role that can take
// a turn in it, which is what the rule releases.
//
// project is the issue record's own project (issues.project, the Dispatch project the records
// carry), not the claim token's spelling IdlePods reads from claims. outOfWorkflow is the
// workflow's list of statuses that take an issue out of it (record.OutOfWorkflowStatuses),
// which the store does not restate: the gate every other path reads is a Go predicate
// (pods.InWorkflowNotLingering) and a select cannot call one, so the rule arrives as a list and
// the filter is this statement's three conditions per row — the status outside that list, the
// phase short of done, and no linger.
//
// Those three conditions are what the filter is for, since the claim states alone would answer
// every issue of the deployment whose roles ever stopped. Every candidate costs one transaction
// of ReleaseIdleIssue, which opens by taking the daemon's global fact serializer
// (treelifecycle.Serialize, pg_advisory_xact_lock(hashtext('legion.apply_fact'))) — the same
// lock the intake takes for every fact, so a tick of candidates is a tick of facts not applied.
// The reading behind that: on the INFRA deployment at 2026-11-10 19:39Z, 1,136 issues held
// claims while 5 stood in the workflow under a live tree.
//
// It adds no index of its own. The answer is read once a minute, the partial index issues_live
// (migrations/0062_live_issue_reads.up.sql) already serves the project's live rows, and claims
// holds at most six rows per issue, so the two subqueries read a handful of rows each.
func (s *Store) UnownedPods(ctx context.Context, project string, outOfWorkflow []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select i.key from issues i join issues root on root.key = i.tree
		where i.project = $1
		and i.status <> all($2) and i.phase <> 'done' and i.linger_until is null
		and root.status <> all($2) and root.phase <> 'done' and root.linger_until is null
		and exists (select 1 from claims c where c.issue = i.key)
		and not exists (select 1 from claims c where c.issue = i.key
			and c.state not in ('failed', 'retired', 'suspended'))
		order by i.key`, project, outOfWorkflow)
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s no claim owns: %w", project, err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, fmt.Errorf("list the pods of %s no claim owns: %w", project, err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pods of %s no claim owns: %w", project, err)
	}
	return issues, nil
}

// ReleaseIdleIssue is the release's one transaction: it reads issue's idle view and hands it to
// decide, and when decide answers rows it writes them and the release's mark together, so the
// view the decision rests on and the mark that records it are one atomic act.
//
// The mark records the ids the rows were given: first_row and last_row are the release's own
// first and last, which is how a row of this release is told from a row of one that is over
// (IssueReleaseStop, IssueRelease), and boundary_row is the row the mark names its boundary —
// the stop of the first role process of the pod — which is where a delivery that reached the
// pod after the mark cancels the release.
//
// It answers whether a release was marked. decide answering no rows writes nothing, which is the
// ordinary case: nearly every tick finds a pod that is working, or one whose idle period has not
// passed.
//
// A mark that names no role and does not say it suspends none is refused before anything is
// written, as a fault of the rule that decided it rather than of the database: the save of the
// workspace is asked of the role the mark names, so a release without one and without the
// second rule's kind is a release nobody can carry out.
func (s *Store) ReleaseIdleIssue(ctx context.Context, project, issue string, decide func(pods.IdleView) (IdleReleaseMark, error)) (bool, error) {
	if project == "" || issue == "" {
		return false, errors.New("an idle release needs its project and its issue")
	}
	var marked bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
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
		if mark.Role == "" && !mark.SuspendsNoRole {
			return fmt.Errorf("the release of %s names no role and is not a release that suspends none", issue)
		}
		if view.Releasing {
			return nil
		}
		var first, boundary, last int64
		for _, row := range mark.Rows {
			id, err := insertOutboxRow(ctx, tx, issue, row)
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
		var started int64
		for _, c := range view.Claims {
			started = max(started, c.LastStartRow)
		}
		// The newest wake this pod has had: a wake recorded above it is one this release never
		// saw, and cancels it (releaseOvertaken).
		var wake int64
		if err := tx.QueryRow(ctx, `select coalesce(max(id), 0) from pod_resumes where project = $1 and issue = $2`,
			project, issue).Scan(&wake); err != nil {
			return fmt.Errorf("read the newest wake of %s: %w", issue, err)
		}
		// The kind is the rule that decided this release, and the role is the one whose shim the
		// save is asked of: a release that suspends no role has none, and leaves the column
		// empty, which is the one spelling the table's check takes beside that kind.
		kind, role := releaseKindIdle, string(mark.Role)
		if mark.SuspendsNoRole {
			kind, role = releaseKindUnowned, ""
		}
		if _, err := tx.Exec(ctx, `insert into issue_releases
			(project, issue, generation, first_row, boundary_row, last_row, started_row, wake_row, role, kind, marked_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())`,
			project, issue, int64(mark.Generation), first, boundary, last, started, wake, role, kind); err != nil {
			return fmt.Errorf("mark the release of %s: %w", issue, err)
		}
		marked = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("release the idle pod of %s: %w", issue, err)
	}
	return marked, nil
}

// insertOutboxRow queues one row of a release, encoded by the caller, and answers the id the
// outbox gave it: the ids of the release's own rows are the range its mark is read against
// (IssueReleaseStop), and a resume a cancel queues takes its id from the same sequence, above
// every row of the release it ends.
func insertOutboxRow(ctx context.Context, tx pgx.Tx, issue string, row IdleReleaseRow) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '') returning id`, row.Kind, issue, string(row.Payload)).Scan(&id); err != nil {
		return 0, fmt.Errorf("queue the %s of %s's release: %w", row.Kind, issue, err)
	}
	return id, nil
}

// ReleaseResumeRow builds the outbox row that brings one claim a cancelled release suspended
// back, at the generation the cancel read of the issue. It is the caller's because the payload
// is record's to encode, as every row of a release is (IdleReleaseRow).
type ReleaseResumeRow func(role claim.Role, generation uint64) (IdleReleaseRow, error)

// CancelIssueRelease ends a release that did not finish, in one transaction: the bundle that
// release saved is deleted, the resume of each claim this release suspended and still holds is
// queued, and the mark goes with them. The three are one act because any of them alone is a
// wrong state of the pod: a mark deleted before the resumes are durable leaves those roles
// `released` in a pod that still stands, with no backstop — a release arms one only once its
// pod is gone — and nothing left to read that says they are owed a process, since the next row
// of the release reads the release as over and the next idle tick sees a pod whose claims are
// suspended; and a bundle left behind is a copy of work the pod the roles resume in holds on
// its own disk, waiting for some later pod of the issue to restore it over whatever those roles
// push in the meantime. Queueing the resumes rather than relaunching here is what makes them
// durable: the row is the daemon's own record of what the pod is owed, and its lane retries it
// until the role has it back.
//
// It answers the roles it queued a resume for: the claims this release suspended that are still
// suspended as `released`. A claim another path took over — a close, a linger, an operator —
// is left to that path, which is what the source on the suspension says; one an earlier release
// left `released` is no business of this release, which is why the mark's own set is read and
// never every released claim of the issue.
//
// Beside them it answers whether the release it ended was one that suspends no role
// (IdleReleaseMark.SuspendsNoRole, issue_releases.kind): the mark's kind, read in the same
// transaction that reads its id and its suspended set, since the cancel reads the mark itself
// and its caller holds none. The caller needs it to say what it cancelled — a release that
// suspended no role resumes none, so an empty answer there is the whole story rather than a
// release whose claims another path took over.
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
func (s *Store) CancelIssueRelease(ctx context.Context, project, issue string, resume ReleaseResumeRow,
	outOfWorkflow func(status string) bool) ([]claim.Role, bool, error) {
	if project == "" || issue == "" || resume == nil || outOfWorkflow == nil {
		return nil, false, errors.New("cancelling a release needs its project, its issue, the row that resumes a claim and the workflow's own rule for a status")
	}
	var resumed []claim.Role
	var suspendsNoRole bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		resumed, suspendsNoRole = nil, false
		var release int64
		var suspended []string
		var kind string
		err := tx.QueryRow(ctx, `select id, suspended, kind from issue_releases where project = $1 and issue = $2`,
			project, issue).Scan(&release, &suspended, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		suspendsNoRole = kind == releaseKindUnowned
		// The bundle this release saved goes with the mark, in this transaction. The pod it was
		// taken off is the pod the roles resume in, with that work in its own workspace, so the
		// row holds a copy of work a live disk has; left behind it would hold the issue's pod
		// (workflow.IdleHoldBundleUnrestored) and wait for a pod created after the save — an
		// eviction's, an update round's, a park and unpark's — which would restore it over
		// however much its roles had pushed since. A delete that fails fails the cancel, which
		// its row's lane runs again; a cancel that left the bundle behind and went on was that
		// sequence's first step.
		if _, err := tx.Exec(ctx, `delete from workspace_bundles where issue = $1 and release_id = $2`, issue, release); err != nil {
			return err
		}
		roles, err := stillReleasedRoles(ctx, tx, project, issue, suspended)
		if err != nil {
			return err
		}
		var generation uint64
		if len(roles) > 0 {
			var live bool
			generation, live, err = resumeStanding(ctx, tx, issue, outOfWorkflow)
			if err != nil {
				return err
			}
			if !live {
				roles = nil
			}
		}
		for _, role := range roles {
			row, err := resume(role, generation)
			if err != nil {
				return err
			}
			if _, err := insertOutboxRow(ctx, tx, issue, row); err != nil {
				return err
			}
			resumed = append(resumed, role)
		}
		return deleteIssueRelease(ctx, tx, project, issue)
	})
	if err != nil {
		return nil, false, fmt.Errorf("cancel the release of %s: %w", issue, err)
	}
	return resumed, suspendsNoRole, nil
}

// resumeStanding is what a cancel reads of issue before it queues a resume: the generation the
// resume names, and whether the issue and its tree root are both still the workflow's to run
// (pods.InWorkflowNotLingering). An issue or a root the record no longer holds is neither.
func resumeStanding(ctx context.Context, tx pgx.Tx, issue string, outOfWorkflow func(status string) bool) (uint64, bool, error) {
	var generation int64
	var issuePhase, status, rootPhase, rootStatus string
	var lingering, rootLingering bool
	err := tx.QueryRow(ctx, `select i.generation, i.phase, i.status, i.linger_until is not null,
		root.phase, root.status, root.linger_until is not null
		from issues i join issues root on root.key = i.tree where i.key = $1`, issue).Scan(
		&generation, &issuePhase, &status, &lingering, &rootPhase, &rootStatus, &rootLingering)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s for the resumes of its cancelled release: %w", issue, err)
	}
	live := pods.InWorkflowNotLingering(
		pods.Standing{Phase: phase.Phase(issuePhase), Status: status, Lingers: lingering},
		pods.Standing{Phase: phase.Phase(rootPhase), Status: rootStatus, Lingers: rootLingering},
		outOfWorkflow)
	return uint64(generation), live, nil
}

// stillReleasedRoles are the roles among suspended whose claims are still suspended as
// `released`, in role order.
func stillReleasedRoles(ctx context.Context, tx pgx.Tx, project, issue string, suspended []string) ([]claim.Role, error) {
	if len(suspended) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select role from claims where project = $1 and issue = $2
		and role = any($3) and state = 'suspended' and suspended_by = 'released' order by role`,
		project, issue, suspended)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []claim.Role
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, claim.Role(role))
	}
	return roles, rows.Err()
}

// ClearIssueRelease forgets the mark of issue's release once the release finished, on the pool:
// the teardown's own call, outside any transaction. A cancel forgets its mark itself, in the
// transaction that queues its resumes (CancelIssueRelease). A mark already gone is success, so a
// retried finish finishes.
func (s *Store) ClearIssueRelease(ctx context.Context, project, issue string) error {
	if err := deleteIssueRelease(ctx, s.pool, project, issue); err != nil {
		return fmt.Errorf("clear the release mark of %s: %w", issue, err)
	}
	return nil
}

// deleteIssueRelease forgets the mark of issue's release through db, which is the pool for the
// teardown's own call and the caller's transaction for the cancel that queues the resumes of
// the claims the release suspended (CancelIssueRelease). One mark, one delete, whichever runs it.
func deleteIssueRelease(ctx context.Context, db execer, project, issue string) error {
	_, err := db.Exec(ctx, `delete from issue_releases where project = $1 and issue = $2`, project, issue)
	return err
}

func readIdleView(ctx context.Context, tx pgx.Tx, project, issue string) (pods.IdleView, error) {
	view := pods.IdleView{Issue: issue}
	var issueGeneration, rootGeneration int64
	var issuePhase, rootPhase string
	err := tx.QueryRow(ctx, `select i.tree, i.phase, i.status, i.generation, i.linger_until,
		root.phase, root.status, root.generation, root.linger_until,
		root.phase = 'awaiting_person', root.awaiting_answered_at is not null
		from issues i join issues root on root.key = i.tree where i.key = $1`, issue).Scan(
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
		greatest(d.delivered_at, d.confirmed_at)
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
		var activityAt, reminder, lastTurn *time.Time
		if err := claims.Scan(&role, &state, &c.PluginContract, &servingGeneration, &c.LastStartRow,
			&activityAt, &c.ActivityBackground, &kinds, &reminder, &c.UpdateRound, &c.UpdatedAt,
			&c.HasProcess, &suspendedBy, &lastTurn); err != nil {
			return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
		}
		c.Role, c.State = claim.Role(role), supervise.ClaimState(state)
		c.SuspendedBy = supervise.SuspendSource(suspendedBy)
		c.ServingGeneration = uint64(servingGeneration)
		c.ActivityAt, c.ActivityNextReminderAt, c.LastTurnAt = orZero(activityAt), orZero(reminder), orZero(lastTurn)
		if kinds != "" {
			c.ActivityKinds = strings.Split(kinds, ",")
		}
		view.Claims = append(view.Claims, c)
	}
	if err := claims.Err(); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the claims of %s for its idle view: %w", issue, err)
	}

	if err := tx.QueryRow(ctx, `select exists (
		select 1 from outbox where issue = $1 and kind = any($2))`, issue, idleWaitingKinds).Scan(&view.Waiting); err != nil {
		return pods.IdleView{}, fmt.Errorf("read the queued rows of %s for its idle view: %w", issue, err)
	}
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_releases where project = $1 and issue = $2)`,
		project, issue).Scan(&view.Releasing); err != nil {
		return pods.IdleView{}, fmt.Errorf("read whether %s is being released: %w", issue, err)
	}
	unrestored, err := workspaceBundleUnrestored(ctx, tx, issue)
	if err != nil {
		return pods.IdleView{}, err
	}
	view.BundleUnrestored = unrestored
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from pod_holds where project = $1 and issue = $2 and not ended)`,
		project, issue).Scan(&view.Holding); err != nil {
		return pods.IdleView{}, fmt.Errorf("read whether a hold streak of %s is running: %w", issue, err)
	}
	return view, nil
}
